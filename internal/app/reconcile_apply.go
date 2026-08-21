package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/deployment"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/health"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/rcon"
	"fxvcs.dev/fxvcs/internal/readiness"
	"fxvcs.dev/fxvcs/internal/reconcile"
	"fxvcs.dev/fxvcs/internal/rollback"
	"fxvcs.dev/fxvcs/internal/runtimecheck"
	"fxvcs.dev/fxvcs/internal/secrets"
	"fxvcs.dev/fxvcs/internal/state"
	"fxvcs.dev/fxvcs/internal/supervisor"
	"fxvcs.dev/fxvcs/internal/templatehook"
	"fxvcs.dev/fxvcs/internal/verifier"
)

// ApplyDependencies are the typed host boundaries used by reconciliation.
// The coordinator owns ordering, durability and state transitions; process and
// readiness I/O remain injectable so the exact same engine is crash-tested.
type ApplyDependencies struct {
	Supervisor               supervisor.Adapter
	PlayerCountObserver      supervisor.PlayerCountObserver
	RCONTransport            rcon.Transport
	Readiness                readiness.Runner
	Verifiers                verifier.Registry
	Now                      func() time.Time
	Sleep                    func(context.Context, time.Duration) error
	EvaluateActivationWindow func(context.Context, string, time.Time) (bool, error)
	Failpoint                func(string) error
	Progress                 func(string)
	LockTimeout              time.Duration
}

type ApplyResult struct {
	Plan       reconcile.PlanResult
	Deployment domain.Deployment
	Applied    bool
}

func (d ApplyDependencies) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func (d ApplyDependencies) sleep(ctx context.Context, duration time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d ApplyDependencies) fail(point string) error {
	if d.Failpoint == nil {
		return nil
	}
	return d.Failpoint(point)
}

func (d ApplyDependencies) stage(name string) {
	if d.Progress != nil {
		d.Progress(name)
	}
}

// ApplyTarget executes one complete target attempt under the cross-process
// target lock. It never reports success before immutable publication,
// journaled activation, a proved new process generation, and readiness soak.
func (c *Coord) ApplyTarget(ctx context.Context, targetName string, config domain.AgentConfig, binding domain.AgentTargetBinding, deps ApplyDependencies) (ApplyResult, error) {
	deps.stage("preflight")
	if deps.Supervisor == nil {
		return ApplyResult{}, errors.New("reconcile: supervisor adapter required")
	}
	lockHandle, err := activation.AcquireTargetLock(ctx, binding.DeployRoot, "reconcile target "+targetName, deps.LockTimeout)
	if err != nil {
		return ApplyResult{}, err
	}
	defer func() { _ = lockHandle.Release() }()

	if err := os.MkdirAll(filepath.Join(binding.DeployRoot, layout.StateDir), 0o700); err != nil {
		return ApplyResult{}, err
	}
	db, err := state.OpenAgent(ctx, filepath.Join(binding.DeployRoot, layout.StateDir, "agent.db"), state.Options{
		Create: true, Migrate: true, Target: targetName, Now: deps.Now,
	})
	if err != nil {
		return ApplyResult{}, err
	}
	defer db.Close()
	if err := recoverTargetOperations(ctx, db, binding.DeployRoot); err != nil {
		return ApplyResult{}, err
	}

	target, err := c.GetTarget(ctx, targetName)
	if err != nil {
		return ApplyResult{}, err
	}
	usesIdentitySecrets := targetHasIdentitySecrets(target)
	usesProtectedSecrets := usesIdentitySecrets || binding.Runtime.RCON != nil
	if usesProtectedSecrets {
		keyPath := filepath.Join(binding.DeployRoot, layout.StateDir, "secret-hmac.key")
		if _, keyErr := secrets.LoadHMACKey(keyPath); errors.Is(keyErr, os.ErrNotExist) {
			deployments, listErr := db.Deployments(ctx)
			if listErr != nil {
				return ApplyResult{}, listErr
			}
			if usesIdentitySecrets && len(deployments) != 0 {
				return ApplyResult{}, errors.New("reconcile: secret fingerprint key is missing for an established target; restore it instead of changing realization identity")
			}
			if _, keyErr = secrets.LoadOrCreateHMACKey(keyPath); keyErr != nil {
				return ApplyResult{}, keyErr
			}
		} else if keyErr != nil {
			return ApplyResult{}, keyErr
		}
	}
	var rconClient *rcon.Client
	var rconPlaintext []byte
	if targetNeedsRCONStatus(target) && binding.Runtime.RCON != nil {
		var rconErr error
		rconClient, rconPlaintext, rconErr = resolveTargetRCON(ctx, target, binding, deps.RCONTransport)
		if rconErr != nil {
			return ApplyResult{}, rconErr
		}
		defer clear(rconPlaintext)
		checkers := make(map[string]readiness.Checker, len(deps.Readiness.Checkers)+1)
		for name, checker := range deps.Readiness.Checkers {
			checkers[name] = checker
		}
		checkers["rcon-status"] = readiness.RCONStatusChecker{Client: rconClient}
		deps.Readiness.Checkers = checkers
	}

	preflightObservation, err := deps.Supervisor.Observe(ctx)
	if err != nil && deps.Supervisor.Capabilities().Observe {
		return ApplyResult{}, fmt.Errorf("reconcile: observe supervisor during final preflight: %w", err)
	}
	activationAllowed := true
	if target.Spec.Health != nil && target.Spec.Health.ActivationWindow != nil && *target.Spec.Health.ActivationWindow != "" {
		activationAllowed = false
		if deps.EvaluateActivationWindow != nil {
			activationAllowed, err = deps.EvaluateActivationWindow(ctx, *target.Spec.Health.ActivationWindow, deps.now())
			if err != nil {
				return ApplyResult{}, fmt.Errorf("reconcile: evaluate activation window: %w", err)
			}
		}
	}
	caps := deps.Supervisor.Capabilities()
	playerCount, caps, err := observePlayerGate(ctx, target.Spec.Health, preflightObservation, caps, deps.PlayerCountObserver)
	if err != nil {
		return ApplyResult{}, err
	}
	plan, err := c.ResolveTargetPlanWithObservations(ctx, targetName, config, binding, TargetPlanObservations{
		Supervisor:  reconcile.SupervisorCapabilities{Observe: caps.Observe, Restart: caps.Restart, HotReload: caps.HotReload, PlayerCount: caps.PlayerCount, ProcessIdentity: caps.ProcessIdentity},
		PlayerCount: playerCount, ActivationAllowed: activationAllowed,
	})
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{Plan: plan}
	if !plan.Applicable() {
		return result, nil
	}
	if plan.LifecycleMode == reconcile.LifecycleHotReload && rconClient == nil {
		var rconErr error
		rconClient, rconPlaintext, rconErr = resolveTargetRCON(ctx, target, binding, deps.RCONTransport)
		if rconErr != nil {
			return result, rconErr
		}
		defer clear(rconPlaintext)
	}
	if targetNeedsResourceState(target) {
		key, keyErr := secrets.LoadHMACKey(filepath.Join(binding.DeployRoot, layout.StateDir, "secret-hmac.key"))
		if keyErr != nil {
			return result, keyErr
		}
		resolver, resolverErr := secrets.NewTargetResolver(target.Metadata.Name, binding.Secrets, key, os.LookupEnv)
		clear(key)
		if resolverErr != nil {
			return result, resolverErr
		}
		token, _, tokenErr := resolveResourceStateSecret(ctx, target, binding, resolver)
		resolver.Close()
		if tokenErr != nil {
			return result, tokenErr
		}
		defer clear(token)
		endpoint := binding.Runtime.ResourceState
		checkers := make(map[string]readiness.Checker, len(deps.Readiness.Checkers)+1)
		for name, checker := range deps.Readiness.Checkers {
			checkers[name] = checker
		}
		checkers["fxvcs-resource-state"] = readiness.ResourceStateChecker{BaseURL: "http://" + net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port)), Token: token}
		deps.Readiness.Checkers = checkers
	}
	if err := deps.fail("after-final-plan"); err != nil {
		return result, err
	}
	if target.Spec.Health == nil {
		return result, errors.New("reconcile: apply requires an explicit health policy")
	}
	if err := validateReadinessBindings(*target.Spec.Health, deps.Readiness); err != nil {
		return result, err
	}

	existing, exists, err := db.Deployment(ctx, plan.RealizationID)
	if err != nil {
		return result, err
	}
	if plan.RealizationID == plan.CurrentRealizationID {
		if !exists {
			return result, errors.New("reconcile: current realization has no durable deployment evidence; restore or explicitly recover agent.db")
		}
		if (existing.Phase == string(health.PhaseActiveGrace) || existing.Phase == string(health.PhaseActiveUnhealthy)) && existing.ProcessGeneration == "" {
			if !caps.Restart || !caps.Observe || !caps.ProcessIdentity {
				return result, errors.New("reconcile: interrupted activation recovery requires observe, restart, and process identity capabilities")
			}
			if !activationAllowed {
				return result, errors.New("reconcile: interrupted activation recovery is outside the activation window")
			}
			if target.Spec.Health != nil && target.Spec.Health.MaxPlayersForRestart != nil {
				if playerCount == nil || !caps.PlayerCount {
					return result, errors.New("reconcile: interrupted activation recovery requires a live player-count observation")
				}
				if *playerCount > *target.Spec.Health.MaxPlayersForRestart {
					return result, fmt.Errorf("reconcile: interrupted activation recovery player count %d exceeds limit %d", *playerCount, *target.Spec.Health.MaxPlayersForRestart)
				}
			}
		}
		environment, envErr := c.GetEnvironment(target.Spec.Environment)
		if envErr != nil {
			return result, envErr
		}
		resumed, resumeErr := c.resumeCurrentAttempt(ctx, db, deps, target, config, binding, environment, existing)
		result.Deployment = resumed
		result.Applied = resumeErr == nil && resumed.Phase == string(health.PhaseActiveHealthy)
		return result, resumeErr
	}
	if exists {
		if err := health.CanStart(health.State{Phase: health.Phase(existing.Phase), RetryHeld: existing.RetryHeld, Tuple: existing.RealizationID}, plan.RealizationID); err != nil {
			return result, err
		}
	}

	environment, err := c.GetEnvironment(target.Spec.Environment)
	if err != nil {
		return result, err
	}
	rel, err := c.GetRelease(ctx, target.Spec.Release)
	if err != nil {
		return result, err
	}
	assemblyOptions, runtimeEvidence, err := c.resolveApplyOptions(ctx, target, environment, rel, binding)
	if err != nil {
		return result, err
	}
	defer clear(assemblyOptions.ResourceStateToken)
	assemblyOptions.RuntimeEvidence = &runtimeEvidence
	externalEvidence, err := verifier.VerifyExternal(ctx, rel.Spec.RuntimeResources, binding.Verifiers, deps.Verifiers)
	if err != nil {
		return result, err
	}
	assemblyOptions.ExternalVerifierEvidence = externalEvidence

	dep := domain.Deployment{RealizationID: plan.RealizationID, Target: targetName, Inputs: plan.Inputs, Phase: string(health.PhaseIdle), StartedAt: deps.now().Format(time.RFC3339Nano), ObservedRuntime: &domain.ObservedRuntime{SystemResourcesDigest: runtimeEvidence.SystemResourcesDigest, ExternalVerifierEvidence: externalEvidence}}
	if err := db.PutDeployment(ctx, dep); err != nil {
		return result, err
	}
	coordinator := deployment.Coordinator{DB: db, Now: deps.Now}
	policy, err := deploymentPolicy(*target.Spec.Health, config.Spec.SafetyFloors)
	if err != nil {
		return result, err
	}
	dep, err = coordinator.ApplyEvent(ctx, dep, policy, health.EventStagingStarted, plan.CurrentRealizationID)
	if err != nil {
		return result, err
	}
	result.Deployment = dep

	failBeforeActivation := func(cause error) (ApplyResult, error) {
		updated, transitionErr := coordinator.ApplyEvent(ctx, dep, policy, health.EventPreActivateFailed, plan.CurrentRealizationID)
		if transitionErr == nil {
			updated.Outcome = cause.Error()
			_ = db.PutDeployment(ctx, updated)
			result.Deployment = updated
		}
		return result, errors.Join(cause, transitionErr)
	}
	if err := deps.fail("after-staging-record"); err != nil {
		return failBeforeActivation(err)
	}
	deps.stage("materialization")
	if _, err := c.MaterializeReleaseContent(ctx, rel, binding.DeployRoot); err != nil {
		return failBeforeActivation(err)
	}
	if err := deps.fail("after-materialization"); err != nil {
		return failBeforeActivation(err)
	}
	deps.stage("rendering")
	assembly, err := c.AssembleReleaseStagingWithOptions(ctx, rel, plan.Inputs, plan.RealizationID, binding.DeployRoot, assemblyOptions)
	if err != nil {
		return failBeforeActivation(err)
	}
	if _, err := PublishReleaseStaging(assembly, binding.DeployRoot); err != nil {
		return failBeforeActivation(err)
	}
	if err := deps.fail("after-publication"); err != nil {
		return failBeforeActivation(err)
	}
	if err := activation.EnsureServerDataLinks(layout.Roots{DeployRoot: binding.DeployRoot, ServerData: binding.ServerData, TxData: binding.Runtime.TxData}); err != nil {
		return failBeforeActivation(err)
	}
	if err := deps.fail("after-serverdata-links"); err != nil {
		return failBeforeActivation(err)
	}
	before, err := deps.Supervisor.Observe(ctx)
	if err != nil {
		return failBeforeActivation(fmt.Errorf("reconcile: observe supervisor before activation: %w", err))
	}
	deps.stage("activation")
	operationID := fmt.Sprintf("activate-%s-%d", plan.RealizationID, deps.now().UnixNano())
	if err := activation.Activate(ctx, db, operationID, binding.DeployRoot, plan.CurrentRealizationID, plan.RealizationID); err != nil {
		return failBeforeActivation(err)
	}
	dep, err = coordinator.ApplyEvent(ctx, dep, policy, health.EventActivated, plan.RealizationID)
	if err != nil {
		return result, err
	}
	result.Deployment = dep
	if err := deps.fail("after-activation"); err != nil {
		return result, err
	}
	deps.stage("lifecycle")
	switch plan.LifecycleMode {
	case reconcile.LifecycleFullRestart:
		if err := deps.Supervisor.Restart(ctx); err != nil {
			result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "runtime restart failed: "+err.Error())
			return result, err
		}
	case reconcile.LifecycleHotReload:
		if rconClient == nil {
			result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "credentialed RCON resource lifecycle is unavailable")
			return result, err
		}
		if err := rconClient.ApplyResources(ctx, rconOperations(plan.ResourceOperations)); err != nil {
			result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "resource lifecycle failed: "+err.Error())
			return result, err
		}
	case reconcile.LifecycleNone:
		// Pointer activation alone; process identity below must remain stable.
	default:
		result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "unknown lifecycle mode")
		return result, err
	}
	after, err := deps.Supervisor.Observe(ctx)
	generationInvalid := err != nil || !after.Running || after.ProcessGeneration == ""
	if plan.LifecycleMode == reconcile.LifecycleFullRestart {
		generationInvalid = generationInvalid || (before.Running && after.ProcessGeneration == before.ProcessGeneration)
	} else {
		generationInvalid = generationInvalid || after.ProcessGeneration != before.ProcessGeneration
	}
	if generationInvalid {
		cause := fmt.Sprintf("runtime lifecycle did not prove the expected running process generation for %s (before=%q after=%q error=%v)", plan.LifecycleMode, before.ProcessGeneration, after.ProcessGeneration, err)
		result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, cause)
		return result, err
	}
	dep.ProcessGeneration = after.ProcessGeneration
	if err := db.PutDeployment(ctx, dep); err != nil {
		return result, err
	}
	if err := deps.fail("after-process-generation"); err != nil {
		return result, err
	}
	deps.stage("readiness")
	decision, err := runReadiness(ctx, db, deps, deps.Supervisor, target.Spec.Health.Checks, *target.Spec.Health, dep.RealizationID, after.ProcessGeneration)
	if err != nil {
		if errors.Is(err, errProcessGenerationChanged) {
			dep, transitionErr := coordinator.ApplyEvent(ctx, dep, policy, health.EventSupervisorRestart, dep.RealizationID)
			if transitionErr == nil {
				dep.Outcome = err.Error()
				_ = db.PutDeployment(ctx, dep)
				result.Deployment = dep
			}
			return result, errors.Join(err, transitionErr)
		}
		return result, err
	}
	if decision == readiness.Healthy {
		if err := deps.fail("after-readiness"); err != nil {
			return result, err
		}
		observed, observeErr := activation.ObserveCurrent(binding.DeployRoot)
		if observeErr != nil {
			return result, observeErr
		}
		dep, err = coordinator.ApplyEvent(ctx, dep, policy, health.EventHealthy, observed)
		if err != nil {
			return result, err
		}
		dep.Outcome = "healthy"
		if err := db.PutDeployment(ctx, dep); err != nil {
			return result, err
		}
		result.Deployment, result.Applied = dep, true
		return result, nil
	}
	result.Deployment, err = c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "readiness failure threshold reached")
	return result, err
}

func targetNeedsRCONStatus(target domain.Target) bool {
	if target.Spec.Health == nil {
		return false
	}
	for _, check := range target.Spec.Health.Checks {
		if check.Type == "rcon-status" {
			return true
		}
	}
	return false
}

func targetNeedsResourceState(target domain.Target) bool {
	if target.Spec.Health == nil {
		return false
	}
	for _, check := range target.Spec.Health.Checks {
		if check.Type == "fxvcs-resource-state" {
			return true
		}
	}
	return false
}

func resolveTargetRCON(ctx context.Context, target domain.Target, binding domain.AgentTargetBinding, transport rcon.Transport) (*rcon.Client, []byte, error) {
	endpoint := binding.Runtime.RCON
	if endpoint == nil {
		return nil, nil, errors.New("reconcile: credentialed RCON check requires an agent-local endpoint binding")
	}
	key, err := secrets.LoadHMACKey(filepath.Join(binding.DeployRoot, layout.StateDir, "secret-hmac.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("reconcile: load protected secret state for RCON: %w", err)
	}
	resolver, err := secrets.NewTargetResolver(target.Metadata.Name, binding.Secrets, key, nil)
	clear(key)
	if err != nil {
		return nil, nil, err
	}
	defer resolver.Close()
	provider, err := providerForSecret(target, endpoint.PasswordSecret)
	if err != nil {
		return nil, nil, err
	}
	value, err := resolver.Resolve(ctx, provider, endpoint.PasswordSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("reconcile: resolve RCON credential: %w", err)
	}
	plaintext := append([]byte(nil), value.Plaintext...)
	clear(value.Plaintext)
	return &rcon.Client{Host: endpoint.Host, Port: endpoint.Port, Password: plaintext, Transport: transport}, plaintext, nil
}

func rconOperations(operations []reconcile.ResourceOperation) []rcon.ResourceOperation {
	out := make([]rcon.ResourceOperation, len(operations))
	for i, operation := range operations {
		out[i] = rcon.ResourceOperation{Action: rcon.ResourceAction(operation.Action), Name: operation.Name}
	}
	return out
}

func observePlayerGate(ctx context.Context, policy *domain.HealthPolicy, observation supervisor.Observation, caps supervisor.Capabilities, source supervisor.PlayerCountObserver) (*int, supervisor.Capabilities, error) {
	playerCount := observation.PlayerCount
	if policy == nil || policy.MaxPlayersForRestart == nil || playerCount != nil || source == nil {
		return playerCount, caps, nil
	}
	players, err := source.ObservePlayerCount(ctx)
	if err != nil {
		return nil, caps, fmt.Errorf("reconcile: observe player count during final preflight: %w", err)
	}
	caps.PlayerCount = true
	return &players, caps, nil
}

func recoverTargetOperations(ctx context.Context, db *state.AgentDB, deployRoot string) error {
	open, err := db.OpenOperations(ctx)
	if err != nil {
		return err
	}
	for _, rec := range open {
		if rec.Operation != "activate" {
			return fmt.Errorf("reconcile: unresolved %s operation %s requires recovery before apply", rec.Operation, rec.OperationID)
		}
		if err := activation.RecoverActivation(ctx, db, rec, deployRoot); err != nil {
			return err
		}
	}
	return nil
}

func validateReadinessBindings(policy domain.HealthPolicy, runner readiness.Runner) error {
	if len(policy.Checks) == 0 {
		return errors.New("reconcile: health policy has no readiness checks")
	}
	for _, check := range policy.Checks {
		checker := runner.Checkers[check.Type]
		if checker == nil {
			return fmt.Errorf("reconcile: no typed readiness adapter for %s", check.Type)
		}
		if validator, ok := checker.(interface {
			Validate(domain.HealthCheck) error
		}); ok {
			if err := validator.Validate(check); err != nil {
				return fmt.Errorf("reconcile: readiness adapter %s is not configured: %w", check.Type, err)
			}
		}
	}
	return nil
}

func deploymentPolicy(policy domain.HealthPolicy, floors domain.AgentSafetyFloors) (health.Policy, error) {
	window, err := time.ParseDuration(policy.RollbackWindow)
	if err != nil || window < 0 {
		return health.Policy{}, fmt.Errorf("reconcile: invalid rollbackWindow %q", policy.RollbackWindow)
	}
	out := health.Policy{OnFailure: policy.OnFailure, MaxAutomaticRollbacks: policy.MaxAutomaticRollbacks, RollbackWindow: window, LocalMaxAutomaticRollbacks: floors.MaxAutomaticRollbacks, LocalRollbackDisabled: floors.DisableAutomaticRollback}
	return out, health.ValidatePolicy(out)
}

func (c *Coord) resolveApplyOptions(ctx context.Context, target domain.Target, environment domain.Environment, rel domain.Release, binding domain.AgentTargetBinding) (AssemblyOptions, runtimecheck.Evidence, error) {
	var resolver templatehook.SecretResolver
	if targetHasIdentitySecrets(target) {
		key, err := secrets.LoadHMACKey(filepath.Join(binding.DeployRoot, layout.StateDir, "secret-hmac.key"))
		if err != nil {
			return AssemblyOptions{}, runtimecheck.Evidence{}, err
		}
		resolved, err := secrets.NewTargetResolver(target.Metadata.Name, binding.Secrets, key, os.LookupEnv)
		clear(key)
		if err != nil {
			return AssemblyOptions{}, runtimecheck.Evidence{}, err
		}
		defer resolved.Close()
		resolver = resolved
	}
	options, err := c.ResolveAssemblyHooks(ctx, target, environment, rel, binding, resolver)
	if err != nil {
		return AssemblyOptions{}, runtimecheck.Evidence{}, err
	}
	if targetNeedsResourceState(target) {
		token, version, err := resolveResourceStateSecret(ctx, target, binding, resolver)
		if err != nil {
			return AssemblyOptions{}, runtimecheck.Evidence{}, err
		}
		ref := binding.Runtime.ResourceState.TokenSecret.Namespace + "/" + binding.Runtime.ResourceState.TokenSecret.Key
		if previous, exists := options.SecretVersions[ref]; exists && previous != version {
			clear(token)
			return AssemblyOptions{}, runtimecheck.Evidence{}, fmt.Errorf("reconcile: secret %s changed during preflight", ref)
		}
		options.SecretVersions[ref] = version
		options.ResourceStateToken = token
	}
	evidence, err := runtimecheck.ValidateSystemResources(binding.Runtime.SystemResourcesRoot, rel.Spec.RuntimeResources)
	return options, evidence, err
}

var errProcessGenerationChanged = errors.New("reconcile: process generation changed during readiness")

func runReadiness(ctx context.Context, db *state.AgentDB, deps ApplyDependencies, adapter supervisor.Adapter, checks []domain.HealthCheck, domainPolicy domain.HealthPolicy, realizationID, generation string) (readiness.Decision, error) {
	policy, err := readiness.PolicyFromDomain(domainPolicy)
	if err != nil {
		return readiness.Unhealthy, err
	}
	interval, err := time.ParseDuration(domainPolicy.Interval)
	if err != nil || interval <= 0 {
		return readiness.Unhealthy, fmt.Errorf("readiness: invalid interval %q", domainPolicy.Interval)
	}
	tracker := readiness.Tracker{ActivatedAt: deps.now(), Decision: readiness.Pending}
	maxBatches := int((policy.StartupGrace+policy.SoakDuration)/interval) + policy.SuccessThreshold + policy.FailureThreshold + 4
	for batch := 0; batch < maxBatches && tracker.Decision == readiness.Pending; batch++ {
		observation, observeErr := adapter.Observe(ctx)
		if observeErr != nil || !observation.Running || observation.ProcessGeneration != generation {
			return readiness.Unhealthy, fmt.Errorf("%w: want %q got %q: %v", errProcessGenerationChanged, generation, observation.ProcessGeneration, observeErr)
		}
		evidence := deps.Readiness.Run(ctx, checks, policy.Timeout)
		observedAt := deps.now()
		for _, item := range evidence {
			detail, _ := json.Marshal(struct {
				Detail string `json:"detail"`
			}{item.Detail})
			if err := db.RecordHealthSample(ctx, state.HealthSample{RealizationID: realizationID, CheckType: item.Type, Interface: item.Interface, Success: item.Success, ObservedAt: observedAt.Format(time.RFC3339Nano), LatencyNS: item.Latency.Nanoseconds(), Detail: detail}); err != nil {
				return readiness.Unhealthy, err
			}
		}
		tracker, err = readiness.Observe(tracker, policy, observedAt, readiness.AllPassed(evidence))
		if err != nil {
			return readiness.Unhealthy, err
		}
		if tracker.Decision == readiness.Pending {
			if err := deps.sleep(ctx, interval); err != nil {
				return readiness.Unhealthy, err
			}
		}
	}
	if tracker.Decision == readiness.Pending {
		return readiness.Unhealthy, errors.New("readiness: bounded evaluation ended without a decision")
	}
	return tracker.Decision, nil
}

// resumeCurrentAttempt handles a crash after current was switched but before
// health promotion. It never infers healthy from the pointer alone. Readiness
// thresholds restart conservatively from zero; this may delay promotion but
// cannot create a false success.
func (c *Coord) resumeCurrentAttempt(ctx context.Context, db *state.AgentDB, deps ApplyDependencies, target domain.Target, config domain.AgentConfig, binding domain.AgentTargetBinding, environment domain.Environment, dep domain.Deployment) (domain.Deployment, error) {
	if dep.Phase == string(health.PhaseActiveHealthy) {
		return dep, nil
	}
	if dep.AttemptInvalidated {
		return dep, errors.New("reconcile: current deployment attempt was invalidated by an unowned process-generation change; operator retry is required")
	}
	policy, err := deploymentPolicy(*target.Spec.Health, config.Spec.SafetyFloors)
	if err != nil {
		return dep, err
	}
	coordinator := deployment.Coordinator{DB: db, Now: deps.Now}
	if dep.Phase == string(health.PhaseStaging) {
		dep, err = coordinator.ApplyEvent(ctx, dep, policy, health.EventActivated, dep.RealizationID)
		if err != nil {
			return dep, err
		}
	} else if dep.Phase != string(health.PhaseActiveGrace) && dep.Phase != string(health.PhaseActiveUnhealthy) {
		return dep, fmt.Errorf("reconcile: current realization has non-resumable deployment phase %s", dep.Phase)
	}

	generation := dep.ProcessGeneration
	if generation == "" {
		before, observeErr := deps.Supervisor.Observe(ctx)
		if observeErr != nil {
			return dep, observeErr
		}
		if err := deps.Supervisor.Restart(ctx); err != nil {
			return c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "runtime restart failed while recovering: "+err.Error())
		}
		after, observeErr := deps.Supervisor.Observe(ctx)
		if observeErr != nil || !after.Running || after.ProcessGeneration == "" || (before.Running && after.ProcessGeneration == before.ProcessGeneration) {
			cause := fmt.Sprintf("recovered restart did not prove a new running process generation (before=%q after=%q error=%v)", before.ProcessGeneration, after.ProcessGeneration, observeErr)
			return c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, cause)
		}
		generation = after.ProcessGeneration
		dep.ProcessGeneration = generation
		if err := db.PutDeployment(ctx, dep); err != nil {
			return dep, err
		}
	} else {
		observation, observeErr := deps.Supervisor.Observe(ctx)
		if observeErr != nil || !observation.Running || observation.ProcessGeneration != generation {
			dep, transitionErr := coordinator.ApplyEvent(ctx, dep, policy, health.EventSupervisorRestart, dep.RealizationID)
			if transitionErr == nil {
				dep.Outcome = fmt.Sprintf("process generation changed during recovery: want %q got %q", generation, observation.ProcessGeneration)
				_ = db.PutDeployment(ctx, dep)
			}
			return dep, errors.Join(errProcessGenerationChanged, observeErr, transitionErr)
		}
	}

	decision, err := runReadiness(ctx, db, deps, deps.Supervisor, target.Spec.Health.Checks, *target.Spec.Health, dep.RealizationID, generation)
	if err != nil {
		if errors.Is(err, errProcessGenerationChanged) {
			dep, transitionErr := coordinator.ApplyEvent(ctx, dep, policy, health.EventSupervisorRestart, dep.RealizationID)
			if transitionErr == nil {
				dep.Outcome = err.Error()
				_ = db.PutDeployment(ctx, dep)
			}
			return dep, errors.Join(err, transitionErr)
		}
		return dep, err
	}
	if decision != readiness.Healthy {
		return c.finishActivatedFailure(ctx, db, coordinator, deps, target, binding, environment, dep, policy, "readiness failure threshold reached during recovery")
	}
	observed, err := activation.ObserveCurrent(binding.DeployRoot)
	if err != nil {
		return dep, err
	}
	dep, err = coordinator.ApplyEvent(ctx, dep, policy, health.EventHealthy, observed)
	if err != nil {
		return dep, err
	}
	dep.Outcome = "healthy"
	return dep, db.PutDeployment(ctx, dep)
}

// finishActivatedFailure applies the policy transition. Automatic rollback is
// completed in a separate helper so a failed rollback can halt without
// oscillating. The helper is intentionally conservative when no proved LKG is
// available.
func (c *Coord) finishActivatedFailure(ctx context.Context, db *state.AgentDB, coordinator deployment.Coordinator, deps ApplyDependencies, target domain.Target, binding domain.AgentTargetBinding, environment domain.Environment, dep domain.Deployment, policy health.Policy, cause string) (domain.Deployment, error) {
	stateBefore, err := db.TargetState(ctx)
	if err != nil {
		return dep, err
	}
	if stateBefore.LastKnownGoodRealizationID == "" {
		policy.LocalRollbackDisabled = true
	}
	updated, err := coordinator.ApplyEvent(ctx, dep, policy, health.EventUnhealthy, dep.RealizationID)
	if err != nil {
		return dep, err
	}
	updated.Outcome = cause
	if err := db.PutDeployment(ctx, updated); err != nil {
		return updated, err
	}
	if updated.Phase != string(health.PhaseRollingBack) {
		return updated, errors.New("reconcile: " + cause)
	}
	rollbackErr := c.applyRollback(ctx, db, coordinator, deps, target, binding, environment, updated, stateBefore.LastKnownGoodRealizationID, policy, cause)
	latest, ok, readErr := db.Deployment(ctx, updated.RealizationID)
	if readErr != nil {
		return updated, errors.Join(rollbackErr, readErr)
	}
	if ok {
		updated = latest
	}
	return updated, rollbackErr
}

func (c *Coord) applyRollback(ctx context.Context, db *state.AgentDB, coordinator deployment.Coordinator, deps ApplyDependencies, target domain.Target, binding domain.AgentTargetBinding, environment domain.Environment, failed domain.Deployment, lkgID string, policy health.Policy, cause string) error {
	deps.stage("rollback")
	previous, ok, err := db.Deployment(ctx, lkgID)
	if err != nil || !ok {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return fmt.Errorf("reconcile: cannot load last-known-good deployment %s after %s: %w", lkgID, cause, err)
	}
	rollbackPlan, err := rollback.Build(previous, failed)
	if err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	rel, err := c.GetRelease(ctx, rollbackPlan.PriorRelease)
	if err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	options, runtimeEvidence, err := c.resolveApplyOptions(ctx, target, environment, rel, binding)
	if err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	defer clear(options.ResourceStateToken)
	options.RuntimeEvidence = &runtimeEvidence
	externalEvidence, err := verifier.VerifyExternal(ctx, rel.Spec.RuntimeResources, binding.Verifiers, deps.Verifiers)
	if err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	options.ExternalVerifierEvidence = externalEvidence
	if _, err = c.MaterializeReleaseContent(ctx, rel, binding.DeployRoot); err == nil {
		var assembled AssemblyResult
		assembled, err = c.AssembleReleaseStagingWithOptions(ctx, rel, rollbackPlan.Inputs, rollbackPlan.RealizationID, binding.DeployRoot, options)
		if err == nil {
			_, err = PublishReleaseStaging(assembled, binding.DeployRoot)
		}
	}
	if err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	rollbackDep := domain.Deployment{RealizationID: rollbackPlan.RealizationID, Target: target.Metadata.Name, Inputs: rollbackPlan.Inputs, Phase: string(health.PhaseStaging), StartedAt: deps.now().Format(time.RFC3339Nano), ObservedRuntime: &domain.ObservedRuntime{SystemResourcesDigest: runtimeEvidence.SystemResourcesDigest, ExternalVerifierEvidence: externalEvidence}}
	if err := db.PutDeployment(ctx, rollbackDep); err != nil {
		return err
	}
	if err := activation.Activate(ctx, db, fmt.Sprintf("rollback-%s-%d", rollbackPlan.RealizationID, deps.now().UnixNano()), binding.DeployRoot, failed.RealizationID, rollbackPlan.RealizationID); err != nil {
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, failed.RealizationID)
		return err
	}
	rollbackDep, err = coordinator.ApplyEvent(ctx, rollbackDep, health.Policy{OnFailure: health.OnFailureNone}, health.EventActivated, rollbackPlan.RealizationID)
	if err != nil {
		return err
	}
	markRollbackAttemptUnhealthy := func(reason string) {
		updated, transitionErr := coordinator.ApplyEvent(ctx, rollbackDep, health.Policy{OnFailure: health.OnFailureNone}, health.EventUnhealthy, rollbackPlan.RealizationID)
		if transitionErr == nil {
			updated.Outcome = reason
			_ = db.PutDeployment(ctx, updated)
			rollbackDep = updated
		}
	}
	if err := deps.Supervisor.Restart(ctx); err != nil {
		markRollbackAttemptUnhealthy("automatic rollback restart failed: " + err.Error())
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, rollbackPlan.RealizationID)
		return err
	}
	observation, err := deps.Supervisor.Observe(ctx)
	if err != nil || !observation.Running || observation.ProcessGeneration == "" {
		markRollbackAttemptUnhealthy("automatic rollback process generation unavailable")
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, rollbackPlan.RealizationID)
		return fmt.Errorf("reconcile: rollback process generation unavailable: %w", err)
	}
	decision, err := runReadiness(ctx, db, deps, deps.Supervisor, target.Spec.Health.Checks, *target.Spec.Health, rollbackDep.RealizationID, observation.ProcessGeneration)
	if err != nil || decision != readiness.Healthy {
		markRollbackAttemptUnhealthy("automatic rollback did not become healthy")
		_, _ = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackFailed, rollbackPlan.RealizationID)
		return fmt.Errorf("reconcile: rollback did not become healthy: %w", err)
	}
	rollbackDep.ProcessGeneration = observation.ProcessGeneration
	rollbackDep, err = coordinator.ApplyEvent(ctx, rollbackDep, health.Policy{OnFailure: health.OnFailureNone}, health.EventHealthy, rollbackPlan.RealizationID)
	if err != nil {
		return err
	}
	rollbackDep.Outcome = "healthy automatic rollback"
	if err := db.PutDeployment(ctx, rollbackDep); err != nil {
		return err
	}
	failed, err = coordinator.ApplyEvent(ctx, failed, policy, health.EventRollbackActivated, rollbackPlan.RealizationID)
	if err != nil {
		return err
	}
	failed.Outcome = cause + "; rolled back to " + rollbackPlan.RealizationID
	if err := db.PutDeployment(ctx, failed); err != nil {
		return err
	}
	return errors.New("reconcile: " + failed.Outcome)
}
