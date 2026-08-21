package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/health"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/materialize"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/readiness"
	"fxvcs.dev/fxvcs/internal/reconcile"
	"fxvcs.dev/fxvcs/internal/runtimecheck"
	stagesecrets "fxvcs.dev/fxvcs/internal/secrets"
	"fxvcs.dev/fxvcs/internal/state"
	"fxvcs.dev/fxvcs/internal/supervisor"
)

type applySupervisor struct {
	generation                    string
	restarts                      int
	observations                  int
	changeGenerationOnObservation int
}

func (*applySupervisor) Name() string { return "systemd" }
func (*applySupervisor) Capabilities() supervisor.Capabilities {
	return supervisor.Capabilities{Observe: true, Restart: true, ProcessIdentity: true}
}

func (s *applySupervisor) Observe(context.Context) (supervisor.Observation, error) {
	s.observations++
	if s.changeGenerationOnObservation > 0 && s.observations == s.changeGenerationOnObservation {
		s.generation += "-operator-restart"
	}
	return supervisor.Observation{Running: true, ProcessGeneration: s.generation, EvidenceSource: "test"}, nil
}
func (s *applySupervisor) Restart(context.Context) error {
	s.restarts++
	s.generation = fmt.Sprintf("generation-%d", s.restarts)
	return nil
}
func (*applySupervisor) ReloadResources(context.Context, []string) error {
	return supervisor.ErrUnsupported
}

type applyChecker struct{}

func (applyChecker) Check(context.Context, domain.HealthCheck) readiness.Evidence {
	return readiness.Evidence{Interface: "test:ready", Success: true}
}

type applySequenceChecker struct {
	results []bool
	calls   int
}

type applyPlayerObserver struct {
	players int
	err     error
	calls   int
}

type applyRCONTransport struct {
	requests [][]byte
}

func (t *applyRCONTransport) RoundTrip(_ context.Context, _ string, request []byte, _ time.Duration) ([]byte, error) {
	t.requests = append(t.requests, append([]byte(nil), request...))
	return []byte("\xff\xff\xff\xffprint authenticated status\n"), nil
}

func (o *applyPlayerObserver) ObservePlayerCount(context.Context) (int, error) {
	o.calls++
	return o.players, o.err
}

func TestObservePlayerGateOnlyCallsSourceForConfiguredMissingEvidence(t *testing.T) {
	source := &applyPlayerObserver{players: 3, err: errors.New("must not be called")}
	players, caps, err := observePlayerGate(context.Background(), &domain.HealthPolicy{}, supervisor.Observation{}, supervisor.Capabilities{Restart: true}, source)
	if err != nil || players != nil || caps.PlayerCount || source.calls != 0 {
		t.Fatalf("unconfigured gate observed players: players=%v caps=%#v calls=%d err=%v", players, caps, source.calls, err)
	}
	maximum := 5
	source.err = nil
	players, caps, err = observePlayerGate(context.Background(), &domain.HealthPolicy{MaxPlayersForRestart: &maximum}, supervisor.Observation{}, caps, source)
	if err != nil || players == nil || *players != 3 || !caps.PlayerCount || source.calls != 1 {
		t.Fatalf("configured gate evidence: players=%v caps=%#v calls=%d err=%v", players, caps, source.calls, err)
	}
}

func TestApplyTargetRunsCredentialedRCONStatusWithoutPersistingOutput(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Secrets.Namespaces = map[string]domain.NamespaceProvider{"fivem": {Provider: "rcon-env"}}
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "rcon-status"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	secret := "rcon-secret-value"
	t.Setenv("FXVCS_SECRET_DEV__FIVEM__RCON_PASSWORD", secret)
	binding := domain.AgentTargetBinding{
		Target: "dev", DeployRoot: deployRoot, ServerData: serverData,
		Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service", RCON: &domain.RCONEndpointRef{Host: "127.0.0.1", Port: 30120, PasswordSecret: domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}}},
		Secrets: map[string]domain.SecretProvider{"rcon-env": {Type: "env"}}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{},
	}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 17, 0, 0, 0, time.UTC)
	transport := &applyRCONTransport{}
	result, err := co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: &applySupervisor{generation: "generation-0"}, RCONTransport: transport,
		Readiness: readiness.Runner{Checkers: map[string]readiness.Checker{}, Now: func() time.Time { return now }},
		Now:       func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil || !result.Applied || len(transport.requests) != 1 || string(transport.requests[0]) != "\xff\xff\xff\xffrcon "+secret+" status" {
		t.Fatalf("credentialed apply=%#v requests=%q err=%v", result, transport.requests, err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	samples, err := db.HealthSamples(ctx, result.Plan.RealizationID)
	if err != nil || len(samples) != 1 || !samples[0].Success || strings.Contains(string(samples[0].Detail), "authenticated status") {
		t.Fatalf("RCON health evidence=%#v err=%v", samples, err)
	}
}

func TestApplyTargetGeneratesAndChecksAuthenticatedResourceState(t *testing.T) {
	ctx := context.Background()
	token := "resource-state-secret-token"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/fxvcs-health/state" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"resources":[{"name":"city-map","state":"started"}]}`))
	}))
	defer server.Close()
	address := server.Listener.Addr().(*net.TCPAddr)

	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "fxvcs-resource-state"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("FXVCS_SECRET_DEV__FIVEM__HEALTH_TOKEN", token)
	binding := domain.AgentTargetBinding{
		Target: "dev", DeployRoot: deployRoot, ServerData: serverData,
		Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service", ResourceState: &domain.ResourceStateEndpointRef{Host: "127.0.0.1", Port: address.Port, TokenSecret: domain.SecretRef{Namespace: "fivem", Key: "health-token"}}},
		Secrets: map[string]domain.SecretProvider{"env": {Type: "env"}}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{},
	}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC)
	result, err := co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: &applySupervisor{generation: "generation-0"},
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil || !result.Applied || requests != 1 || result.Plan.Inputs.SecretVersions["fivem/health-token"] == "" {
		t.Fatalf("resource-state apply=%#v requests=%d err=%v", result, requests, err)
	}
	root := filepath.Join(deployRoot, layout.ReleasesDir, result.Plan.RealizationID)
	metadata, _ := os.ReadFile(filepath.Join(root, layout.RuntimeDir, "realization.json"))
	if bytes.Contains(metadata, []byte(token)) {
		t.Fatal("resource-state token leaked into realization metadata")
	}
	t.Setenv("FXVCS_SECRET_DEV__FIVEM__HEALTH_TOKEN", "rotated-resource-state-token")
	rotated, err := co.ResolveTargetPlan(ctx, "dev", config, binding)
	if err != nil || rotated.RealizationID == result.Plan.RealizationID || rotated.Inputs.SecretVersions["fivem/health-token"] == result.Plan.Inputs.SecretVersions["fivem/health-token"] || rotated.LifecycleMode != reconcile.LifecycleFullRestart {
		t.Fatalf("token rotation did not create a full-restart realization: %#v err=%v", rotated, err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	samples, err := db.HealthSamples(ctx, result.Plan.RealizationID)
	if err != nil || len(samples) != 1 || !samples[0].Success || strings.Contains(string(samples[0].Detail), "city-map") {
		t.Fatalf("resource-state evidence=%#v err=%v", samples, err)
	}
}

func TestApplyTargetUsesTypedRCONHotLifecycleForComponentOnlyChange(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("FXVCS_SECRET_DEV__FIVEM__RCON_PASSWORD", "rcon-secret")
	binding := domain.AgentTargetBinding{
		Target: "dev", DeployRoot: deployRoot, ServerData: serverData,
		Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service", RCON: &domain.RCONEndpointRef{Host: "127.0.0.1", Port: 30120, PasswordSecret: domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}}},
		Secrets: map[string]domain.SecretProvider{"env": {Type: "env"}}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{},
	}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 18, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-0"}
	transport := &applyRCONTransport{}
	deps := ApplyDependencies{
		Supervisor: process, RCONTransport: transport,
		Readiness: readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:       func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	}
	first, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !first.Applied || process.restarts != 1 {
		t.Fatalf("initial full restart: %#v restarts=%d err=%v", first, process.restarts, err)
	}
	component.work.WriteFile("city-map/update.lua", []byte("-- changed\n"))
	component.push("change map")
	if _, err := co.StackSync(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, co, createInput("v2"))
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v2"}); err != nil {
		t.Fatal(err)
	}
	transport.requests = nil
	second, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !second.Applied || second.Plan.LifecycleMode != reconcile.LifecycleHotReload || process.restarts != 1 {
		t.Fatalf("hot apply: %#v restarts=%d err=%v", second, process.restarts, err)
	}
	want := []string{"\xff\xff\xff\xffrcon rcon-secret refresh", "\xff\xff\xff\xffrcon rcon-secret restart city-map"}
	got := make([]string, len(transport.requests))
	for i := range transport.requests {
		got[i] = string(transport.requests[i])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("RCON lifecycle=%q want=%q", got, want)
	}
}

func (c *applySequenceChecker) Check(context.Context, domain.HealthCheck) readiness.Evidence {
	index := c.calls
	c.calls++
	if index >= len(c.results) {
		index = len(c.results) - 1
	}
	return readiness.Evidence{Interface: "test:ready", Success: c.results[index]}
}

func TestApplyTargetRecoversCrashAfterActivationAndPromotesHealthy(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, err := co.GetTarget(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	target.Spec.Health = &domain.HealthPolicy{
		StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1,
		SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}},
	}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-old"}
	crash := errors.New("simulated crash after activation")
	result, err := co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now },
		Sleep:      func(context.Context, time.Duration) error { return nil },
		Failpoint: func(point string) error {
			if point == "after-activation" {
				return crash
			}
			return nil
		},
	})
	if !errors.Is(err, crash) || result.Deployment.Phase != "active-grace" || process.restarts != 0 {
		t.Fatalf("activation crash was not durably exposed: %#v restarts=%d err=%v", result, process.restarts, err)
	}
	result, err = co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now },
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil || !result.Applied || result.Deployment.Phase != "active-healthy" || process.restarts != 1 {
		t.Fatalf("unexpected recovered apply result: %#v restarts=%d err=%v", result, process.restarts, err)
	}
	current, err := activation.ObserveCurrent(deployRoot)
	if err != nil || current != result.Plan.RealizationID {
		t.Fatalf("current=%q want=%q err=%v", current, result.Plan.RealizationID, err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	targetState, err := db.TargetState(ctx)
	if err != nil || targetState.LastKnownGoodRealizationID != result.Plan.RealizationID {
		t.Fatalf("target state=%#v err=%v", targetState, err)
	}
	samples, err := db.HealthSamples(ctx, result.Plan.RealizationID)
	if err != nil || len(samples) != 1 || !samples[0].Success {
		t.Fatalf("health samples=%#v err=%v", samples, err)
	}
	second, err := co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now },
	})
	if err != nil || !second.Applied || second.Deployment.Phase != "active-healthy" || process.restarts != 1 {
		t.Fatalf("healthy current was not idempotent: %#v restarts=%d err=%v", second, process.restarts, err)
	}
}

func TestApplyTargetInterruptedActivationRecoveryRechecksWindow(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, err := co.GetTarget(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	window := "window.v1;tz=UTC;days=*;start=00:00;end=23:59"
	target.Spec.Health = &domain.HealthPolicy{
		StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1,
		SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", ActivationWindow: &window,
		Checks: []domain.HealthCheck{{Type: "tcp"}},
	}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-old"}
	crash := errors.New("simulated crash after activation")
	open := true
	deps := ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
		EvaluateActivationWindow: func(context.Context, string, time.Time) (bool, error) { return open, nil },
		Failpoint: func(point string) error {
			if point == "after-activation" {
				return crash
			}
			return nil
		},
	}
	result, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if !errors.Is(err, crash) || result.Deployment.Phase != "active-grace" || process.restarts != 0 {
		t.Fatalf("activation crash was not exposed: %#v restarts=%d err=%v", result, process.restarts, err)
	}
	open = false
	deps.Failpoint = nil
	result, err = co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err == nil || !strings.Contains(err.Error(), "outside the activation window") || result.Applied || process.restarts != 0 {
		t.Fatalf("closed-window recovery was not refused: %#v restarts=%d err=%v", result, process.restarts, err)
	}
}

func TestApplyTargetKeepAllowsLateRecoveryWithoutAnotherRestart(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-old"}
	checker := &applySequenceChecker{results: []bool{false, true}}
	deps := ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": checker}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	}
	failed, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err == nil || failed.Applied || failed.Deployment.Phase != "active-unhealthy" || !failed.Deployment.RetryHeld || process.restarts != 1 {
		t.Fatalf("keep failure: %#v restarts=%d err=%v", failed, process.restarts, err)
	}
	recovered, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !recovered.Applied || recovered.Deployment.Phase != "active-healthy" || recovered.Deployment.RetryHeld || process.restarts != 1 {
		t.Fatalf("late recovery: %#v restarts=%d err=%v", recovered, process.restarts, err)
	}
}

func TestApplyTargetBuildsAndHealthChecksAutomaticRollback(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	mustCreate(t, co, createInput("v2"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "rollback", MaxAutomaticRollbacks: 1, RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-0"}
	checker := &applySequenceChecker{results: []bool{true, false, true}}
	var stages []string
	deps := ApplyDependencies{Supervisor: process, Readiness: readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": checker}, Now: func() time.Time { return now }}, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil }, Progress: func(stage string) { stages = append(stages, stage) }}
	first, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !first.Applied {
		t.Fatalf("initial deployment: %#v %v", first, err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v2"}); err != nil {
		t.Fatal(err)
	}
	failed, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err == nil || failed.Applied {
		t.Fatalf("failed desired deployment reported success: %#v %v", failed, err)
	}
	current, err := activation.ObserveCurrent(deployRoot)
	if err != nil || current == first.Plan.RealizationID || current == failed.Plan.RealizationID {
		t.Fatalf("rollback should be a fresh realization: current=%q first=%q failed=%q err=%v", current, first.Plan.RealizationID, failed.Plan.RealizationID, err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	failedDep, ok, err := db.Deployment(ctx, failed.Plan.RealizationID)
	if err != nil || !ok || failedDep.Phase != "rolled-back" || !failedDep.RetryHeld {
		t.Fatalf("failed deployment evidence: %#v ok=%v err=%v", failedDep, ok, err)
	}
	rollbackDep, ok, err := db.Deployment(ctx, current)
	if err != nil || !ok || rollbackDep.Phase != "active-healthy" || rollbackDep.Inputs.ReleaseName != "v1" || rollbackDep.Inputs.TargetContentDigest != failed.Plan.Inputs.TargetContentDigest {
		t.Fatalf("rollback deployment evidence: %#v ok=%v err=%v", rollbackDep, ok, err)
	}
	if !slices.Contains(stages, "materialization") || !slices.Contains(stages, "activation") || !slices.Contains(stages, "readiness") || !slices.Contains(stages, "rollback") {
		t.Fatalf("missing reconciliation progress stages: %#v", stages)
	}
}

func TestApplyTargetInvalidatesCompetingRestartDuringReadiness(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 14, 0, 0, 0, time.UTC)
	// Observe order is final preflight, pre-activation, post-restart, then the
	// first readiness batch. The fourth observation simulates txAdmin/operator
	// replacing the process after FXVCS proved its own restart.
	process := &applySupervisor{generation: "generation-0", changeGenerationOnObservation: 4}
	result, err := co.ApplyTarget(ctx, "dev", config, binding, ApplyDependencies{
		Supervisor: process,
		Readiness:  readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }},
		Now:        func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err == nil || result.Applied || !strings.Contains(err.Error(), "process generation changed") {
		t.Fatalf("competing restart was not refused: %#v err=%v", result, err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dep, ok, err := db.Deployment(ctx, result.Plan.RealizationID)
	if err != nil || !ok || dep.Phase != "active-grace" || !dep.AttemptInvalidated {
		t.Fatalf("restart race evidence: %#v ok=%v err=%v", dep, ok, err)
	}
	stateRow, err := db.TargetState(ctx)
	if err != nil || stateRow.LastKnownGoodRealizationID != "" {
		t.Fatalf("restart race must not promote LKG: %#v err=%v", stateRow, err)
	}
}

func TestApplyTargetPreActivationFailurePreservesCurrentAndHoldsTuple(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	mustCreate(t, co, createInput("v2"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "keep", RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-0"}
	deps := ApplyDependencies{Supervisor: process, Readiness: readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": applyChecker{}}, Now: func() time.Time { return now }}, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil }}
	first, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !first.Applied {
		t.Fatalf("initial deployment: %#v err=%v", first, err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v2"}); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated failure after immutable publication")
	deps.Failpoint = func(point string) error {
		if point == "after-publication" {
			return crash
		}
		return nil
	}
	failed, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if !errors.Is(err, crash) || failed.Applied || failed.Deployment.Phase != "activation-failed" || !failed.Deployment.RetryHeld {
		t.Fatalf("pre-activation failure evidence: %#v err=%v", failed, err)
	}
	current, err := activation.ObserveCurrent(deployRoot)
	if err != nil || current != first.Plan.RealizationID || process.restarts != 1 {
		t.Fatalf("pre-activation failure changed live runtime: current=%q restarts=%d err=%v", current, process.restarts, err)
	}
	deps.Failpoint = nil
	_, err = co.ApplyTarget(ctx, "dev", config, binding, deps)
	if !errors.Is(err, health.ErrRetryHeld) {
		t.Fatalf("identical failed tuple was not held: %v", err)
	}
}

func TestApplyTargetHaltsAfterFailedAutomaticRollback(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	mustCreate(t, co, createInput("v2"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "dev", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1", Environment: "dev", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "env"}); err != nil {
		t.Fatal(err)
	}
	target, _ := co.GetTarget(ctx, "dev")
	target.Spec.Health = &domain.HealthPolicy{StartupGrace: "0s", Interval: "1s", Timeout: "1s", SuccessThreshold: 1, FailureThreshold: 1, SoakDuration: "0s", OnFailure: "rollback", MaxAutomaticRollbacks: 1, RollbackWindow: "1h", Checks: []domain.HealthCheck{{Type: "tcp"}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot, _ := filepath.EvalSymlinks(t.TempDir())
	serverData, _ := filepath.EvalSymlinks(t.TempDir())
	binding := domain.AgentTargetBinding{Target: "dev", DeployRoot: deployRoot, ServerData: serverData, Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{}, Hooks: map[string]domain.HookDefinition{}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	now := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	process := &applySupervisor{generation: "generation-0"}
	checker := &applySequenceChecker{results: []bool{true, false, false}}
	deps := ApplyDependencies{Supervisor: process, Readiness: readiness.Runner{Checkers: map[string]readiness.Checker{"tcp": checker}, Now: func() time.Time { return now }}, Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil }}
	first, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err != nil || !first.Applied {
		t.Fatalf("initial deployment: %#v err=%v", first, err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v2"}); err != nil {
		t.Fatal(err)
	}
	failed, err := co.ApplyTarget(ctx, "dev", config, binding, deps)
	if err == nil || failed.Applied {
		t.Fatalf("double failure reported success: %#v err=%v", failed, err)
	}
	current, observeErr := activation.ObserveCurrent(deployRoot)
	if observeErr != nil || current == first.Plan.RealizationID || current == failed.Plan.RealizationID {
		t.Fatalf("failed rollback current evidence: current=%q err=%v", current, observeErr)
	}
	db, openErr := state.OpenAgent(ctx, filepath.Join(deployRoot, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer db.Close()
	failedDep, ok, queryErr := db.Deployment(ctx, failed.Plan.RealizationID)
	if queryErr != nil || !ok || failedDep.Phase != "halted" || !failedDep.RetryHeld {
		t.Fatalf("failed desired halt evidence: %#v ok=%v err=%v", failedDep, ok, queryErr)
	}
	rollbackDep, ok, queryErr := db.Deployment(ctx, current)
	if queryErr != nil || !ok || rollbackDep.Phase != "active-unhealthy" || !rollbackDep.RetryHeld {
		t.Fatalf("failed rollback realization evidence: %#v ok=%v err=%v", rollbackDep, ok, queryErr)
	}
	restarts := process.restarts
	_, err = co.ApplyTarget(ctx, "dev", config, binding, deps)
	if !errors.Is(err, health.ErrRetryHeld) || process.restarts != restarts {
		t.Fatalf("halted tuple oscillated: restarts=%d want=%d err=%v", process.restarts, restarts, err)
	}
}

type hookSecretResolver struct{ value stagesecrets.Value }

func (r hookSecretResolver) Resolve(context.Context, string, domain.SecretRef) (stagesecrets.Value, error) {
	return r.value, nil
}

func testContentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestMaterializePointerFromWholeAndChunkedRemoteObjects(t *testing.T) {
	ctx := context.Background()
	domainID := "domain_materialize_test"
	source, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := materialize.Store{DeployRoot: t.TempDir()}

	whole := []byte("whole content")
	wholeOID := testContentDigest(whole)
	if err := source.Put(ctx, domainID, wholeOID, int64(len(whole)), bytes.NewReader(whole)); err != nil {
		t.Fatal(err)
	}
	wholePointer := pointer.Pointer{StorageDomainID: domainID, Encoding: pointer.EncodingWhole, ManifestDigest: wholeOID, OID: wholeOID, Size: int64(len(whole))}
	if _, err := materializePointerFrom(ctx, source, domainID, wholePointer, target); err != nil {
		t.Fatal(err)
	}
	if err := target.Verify(wholeOID, int64(len(whole))); err != nil {
		t.Fatal(err)
	}

	parts := [][]byte{[]byte("chunk-one"), []byte("chunk-two")}
	content := append(append([]byte(nil), parts[0]...), parts[1]...)
	chunks := make([]domain.ManifestChunk, 0, len(parts))
	for _, part := range parts {
		digest := testContentDigest(part)
		if err := source.Put(ctx, domainID, digest, int64(len(part)), bytes.NewReader(part)); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, domain.ManifestChunk{Digest: digest, RawSize: int64(len(part)), EncodedSize: int64(len(part))})
	}
	manifest := domain.ObjectManifest{FormatVersion: format.ObjectManifestVersion, StorageDomainID: domainID, Encoding: "cdc-fastcdc/1", Compression: "none", OID: testContentDigest(content), Size: int64(len(content)), Chunks: chunks}
	manifestDigest, err := integrity.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := canonical.Marshal(manifest)
	if err := source.Put(ctx, domainID, manifestDigest, int64(len(manifestBytes)), bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	chunkedPointer := pointer.Pointer{StorageDomainID: domainID, Encoding: manifest.Encoding, ManifestDigest: manifestDigest, OID: manifest.OID, Size: manifest.Size}
	if _, err := materializePointerFrom(ctx, source, domainID, chunkedPointer, target); err != nil {
		t.Fatal(err)
	}
	chunkedPath, _ := target.Path(chunkedPointer.OID)
	got, err := os.ReadFile(chunkedPath)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("chunked materialization differs: %q %v", got, err)
	}
}

func TestMaterializeReleaseContentFromCoordinationBinding(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	remoteRoot := t.TempDir()
	component := newComponentWithStorage(t, "maps", remoteRoot)
	addComponent(t, co, component, StackAddInput{})
	if _, err := co.StorageBind(StorageAddInput{Name: "primary", Type: "filesystem", Path: remoteRoot}); err != nil {
		t.Fatal(err)
	}
	writeObject(t, remoteRoot, component)
	in := createInput("v1")
	in.SkipObjectCheck = false
	if _, err := co.ReleaseCreate(ctx, in); err != nil {
		t.Fatal(err)
	}
	rel, err := co.GetRelease(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	deployRoot := t.TempDir()
	items, err := co.MaterializeReleaseContent(ctx, rel, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	body := assetBody(component.name)
	oid := testContentDigest(body)
	if len(items) != 1 || items[0].Digest != oid || items[0].Remote != "primary" || items[0].Existed {
		t.Fatalf("unexpected materialization evidence: %#v", items)
	}
	if err := (materialize.Store{DeployRoot: deployRoot}).Verify(oid, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	second, err := co.MaterializeReleaseContent(ctx, rel, deployRoot)
	if err != nil || len(second) != 1 || !second[0].Existed || second[0].Remote != "" {
		t.Fatalf("idempotent materialization: %#v %v", second, err)
	}
}

type lyingObjectStore struct{ data map[string][]byte }

func (s lyingObjectStore) Has(context.Context, string, string) (bool, error) { return true, nil }
func (s lyingObjectStore) Put(context.Context, string, string, int64, io.Reader) error {
	return nil
}
func (s lyingObjectStore) Get(_ context.Context, _, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data[digest])), nil
}

func TestMaterializePointerFromRejectsLyingChunkStore(t *testing.T) {
	ctx := context.Background()
	domainID := "domain_lying_test"
	good := []byte("expected")
	chunkDigest := testContentDigest(good)
	manifest := domain.ObjectManifest{FormatVersion: format.ObjectManifestVersion, StorageDomainID: domainID, Encoding: "cdc-fastcdc/1", Compression: "none", OID: chunkDigest, Size: int64(len(good)), Chunks: []domain.ManifestChunk{{Digest: chunkDigest, RawSize: int64(len(good)), EncodedSize: int64(len(good))}}}
	manifestDigest, err := integrity.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := canonical.Marshal(manifest)
	source := lyingObjectStore{data: map[string][]byte{manifestDigest: manifestBytes, chunkDigest: []byte("tampered")}}
	ptr := pointer.Pointer{StorageDomainID: domainID, Encoding: manifest.Encoding, ManifestDigest: manifestDigest, OID: manifest.OID, Size: manifest.Size}
	target := materialize.Store{DeployRoot: t.TempDir()}
	if _, err := materializePointerFrom(ctx, source, domainID, ptr, target); err == nil || !strings.Contains(err.Error(), "chunk") {
		t.Fatalf("lying chunk store was accepted: %v", err)
	}
	failedPath, _ := target.Path(ptr.OID)
	if _, err := os.Stat(failedPath); err == nil {
		t.Fatal("failed content was published")
	}
}

func TestReadManifestFromBindsEveryPointerIdentityField(t *testing.T) {
	ctx := context.Background()
	domainID := "domain_manifest_test"
	oid := "sha256:" + strings.Repeat("a", 64)
	chunkDigest := "sha256:" + strings.Repeat("b", 64)
	manifest := domain.ObjectManifest{
		FormatVersion: format.ObjectManifestVersion, StorageDomainID: domainID,
		Encoding: "cdc-fastcdc/1", Compression: "none", OID: oid, Size: 4,
		Chunks: []domain.ManifestChunk{{Digest: chunkDigest, RawSize: 4, EncodedSize: 4}},
	}
	manifestDigest, err := integrity.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := canonical.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, domainID, manifestDigest, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	ptr := pointer.Pointer{StorageDomainID: domainID, Encoding: "cdc-fastcdc/1", ManifestDigest: manifestDigest, OID: "sha256:" + strings.Repeat("c", 64), Size: 4}
	if _, err := readManifestFrom(ctx, store, domainID, ptr); err == nil || !strings.Contains(err.Error(), "pointer oid") {
		t.Fatalf("manifest was accepted for a different pointer identity: %v", err)
	}
}

func TestResolveTargetPlanFromPinnedCoordinationState(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "envfile"}); err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	binding := domain.AgentTargetBinding{Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 2, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applicable() || plan.RealizationID == "" || plan.Target != "prd" || plan.Inputs.ReleaseName != "v1" || plan.Inputs.EnvironmentRevision != 1 {
		t.Fatalf("unexpected resolved plan: %#v", plan)
	}
	if len(plan.Actions) < 4 || plan.Actions[0].Action != "materialize-content" || plan.Actions[len(plan.Actions)-1].Action != "evaluate-health" {
		t.Fatalf("unexpected actions: %#v", plan.Actions)
	}
}

func TestResolveTargetPlanIncludesVerifiedSecretHookVersions(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "core", resources: map[string]string{"chat": ""}})
	templateSource := []byte("rcon_password {{ cfgQuote .Inputs.password }}\n")
	component.work.WriteFile("deploy/server.cfg.tmpl", templateSource)
	component.push("add secret template")
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "process"}); err != nil {
		t.Fatal(err)
	}
	target, err := co.GetTarget(ctx, "prd")
	if err != nil {
		t.Fatal(err)
	}
	target.Spec.Secrets.DefaultProvider = "process"
	ref := domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}
	target.Spec.Hooks = &domain.HookRequests{PostMaterialize: []domain.HookRequest{{
		ID: "render-server", Type: "template.v1",
		Template:    &domain.TemplateRef{Repository: "core", Path: "deploy/server.cfg.tmpl", Digest: testContentDigest(templateSource)},
		Destination: "runtime/config/server.cfg", Mode: "0600",
		Inputs: map[string]domain.InputSource{"password": {Secret: &ref}},
	}}}
	if _, err := co.SaveTarget(target); err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	if _, err := stagesecrets.LoadOrCreateHMACKey(filepath.Join(deployRoot, layout.StateDir, "secret-hmac.key")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FXVCS_SECRET_PRD__FIVEM__RCON_PASSWORD", "safe-secret")
	binding := domain.AgentTargetBinding{
		Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"},
		Secrets: map[string]domain.SecretProvider{"process": {Type: "env"}},
		Hooks:   map[string]domain.HookDefinition{"render-server": {Type: "template.v1", AllowedDestRoot: "runtime/config", MaxOutputBytes: 4096, AllowedTemplates: []string{"core:deploy/server.cfg.tmpl"}}},
	}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 2, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applicable() || plan.Inputs.SecretVersions["fivem/rcon-password"] == "" || plan.Inputs.HookDigests["render-server"] == "" {
		t.Fatalf("secret hook preflight missing from plan: %#v", plan)
	}
}

func TestAssembleReleaseStagingBuildsUnselectableImmutableView(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "envfile"}); err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	binding := domain.AgentTargetBinding{Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 2, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil || !plan.Applicable() {
		t.Fatalf("resolve: %#v %v", plan, err)
	}
	rel, err := co.GetRelease(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := co.AssembleReleaseStaging(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(result.StagingPath) })
	if !strings.HasPrefix(result.StagingPath, filepath.Join(deployRoot, layout.StateDir, "staging")+string(filepath.Separator)) {
		t.Fatalf("staging escaped deploy state: %s", result.StagingPath)
	}
	if _, err := os.Lstat(filepath.Join(deployRoot, layout.CurrentLink)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("assembly selected current: %v", err)
	}
	manifestPath := filepath.Join(result.StagingPath, "resources", "[maps]", "city-map", "fxmanifest.lua")
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := testContentDigest([]byte(manifestBody))
	storedPath, _ := (materialize.Store{DeployRoot: deployRoot}).Path(manifestDigest)
	storedInfo, err := os.Stat(storedPath)
	if err != nil || !os.SameFile(manifestInfo, storedInfo) {
		t.Fatalf("immutable file is not a materialized hardlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.StagingPath, "resources", "[maps]", ".fxvcs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repository metadata leaked into runtime view: %v", err)
	}
	metadataBytes, err := os.ReadFile(result.MetadataPath)
	if err != nil || len(metadataBytes) == 0 || len(result.Metadata.Files) != 2 || result.Metadata.Files[0].Placement != "hardlink" || result.Metadata.Files[1].Path != "runtime/fxvcs.cfg" || result.Metadata.Files[1].Placement != "generated" {
		t.Fatalf("assembly metadata incomplete: %#v bytes=%d err=%v", result.Metadata, len(metadataBytes), err)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(result.StagingPath, "runtime", "fxvcs.cfg"))
	if err != nil || string(runtimeConfig) != "# Generated by fxvcs; do not edit.\nensure city-map\n" {
		t.Fatalf("runtime config: %q %v", runtimeConfig, err)
	}
	published, err := PublishReleaseStaging(result, deployRoot)
	if err != nil || published.Existed {
		t.Fatalf("publish staging: %#v %v", published, err)
	}
	if _, err := os.Stat(result.StagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published staging still exists: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(deployRoot, layout.CurrentLink)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication selected current: %v", err)
	}
	second, err := co.AssembleReleaseStaging(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := PublishReleaseStaging(second, deployRoot)
	if err != nil || !reused.Existed || reused.Path != published.Path {
		t.Fatalf("idempotent publish: %#v %v", reused, err)
	}
	publishedManifest := filepath.Join(published.Path, "resources", "[maps]", "city-map", "fxmanifest.lua")
	if err := os.Remove(publishedManifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publishedManifest, []byte("tampered"), 0o444); err != nil {
		t.Fatal(err)
	}
	third, err := co.AssembleReleaseStaging(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(third.StagingPath) })
	if _, err := PublishReleaseStaging(third, deployRoot); err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("corrupt existing realization was reused: %v", err)
	}
}

func TestResolveAndAssembleSecretHookWithoutPersistingPlainDigest(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	templateSource := []byte("sv_hostname {{ cfgQuote .Inputs.hostname }}\nrcon_password {{ cfgQuote .Inputs.password }}\n")
	component.work.WriteFile("deploy/server.cfg.tmpl", templateSource)
	component.push("add deployment template")
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	rel, err := co.GetRelease(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}
	target := domain.Target{Metadata: domain.NamedMetadata{Name: "prd"}, Spec: domain.TargetSpec{
		Secrets: domain.SecretPolicy{DefaultProvider: "local"},
		Hooks: &domain.HookRequests{PostMaterialize: []domain.HookRequest{{
			ID: "render-server", Type: "template.v1",
			Template:    &domain.TemplateRef{Repository: component.name, Path: "deploy/server.cfg.tmpl", Digest: testContentDigest(templateSource)},
			Destination: "runtime/config/server.cfg", Mode: "0600",
			Inputs: map[string]domain.InputSource{
				"hostname": {Target: stringPointer("server.hostname")},
				"password": {Secret: &ref},
			},
		}}},
	}}
	environment := domain.Environment{Metadata: domain.NamedMetadata{Name: "prd"}, Spec: domain.EnvironmentSpec{Revision: 1, Inputs: map[string]string{"hostname": "Example"}}}
	binding := domain.AgentTargetBinding{Hooks: map[string]domain.HookDefinition{
		"render-server": {Type: "template.v1", AllowedDestRoot: "runtime/config", MaxOutputBytes: 4096, AllowedTemplates: []string{component.name + ":deploy/server.cfg.tmpl"}},
	}}
	options, err := co.ResolveAssemblyHooks(ctx, target, environment, rel, binding, hookSecretResolver{value: stagesecrets.Value{Plaintext: []byte("do-not-persist"), Version: "hmac-sha256:opaque"}})
	if err != nil {
		t.Fatal(err)
	}
	releaseDigest, _ := canonical.Digest(rel)
	targetDigest, _ := canonical.Digest(target)
	inputs := domain.RealizationInputs{
		ReleaseName: rel.Metadata.Name, ReleaseDigest: releaseDigest, SourceSetDigest: rel.Spec.SourceSetDigest,
		EnvironmentName: environment.Metadata.Name, EnvironmentRevision: environment.Spec.Revision, TargetContentDigest: targetDigest,
		HookDigests: options.HookDigests, VerifierBindings: map[string]string{}, SecretVersions: options.SecretVersions,
	}
	realizationID, err := integrity.RealizationID(inputs)
	if err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	result, err := co.AssembleReleaseStagingWithOptions(ctx, rel, inputs, realizationID, deployRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	metadataBytes, err := os.ReadFile(result.MetadataPath)
	if err != nil {
		t.Fatal(err)
	}
	secretByteDigest := testContentDigest([]byte("sv_hostname \"Example\"\nrcon_password \"do-not-persist\"\n"))
	if bytes.Contains(metadataBytes, []byte("do-not-persist")) || bytes.Contains(metadataBytes, []byte(secretByteDigest)) {
		t.Fatalf("secret plaintext or offline digest persisted in metadata: %s", metadataBytes)
	}
	generatedPath := filepath.Join(result.StagingPath, "runtime", "config", "server.cfg")
	generated, err := os.ReadFile(generatedPath)
	if err != nil || string(generated) != "sv_hostname \"Example\"\nrcon_password \"do-not-persist\"\n" {
		t.Fatalf("generated hook output: %q %v", generated, err)
	}
	rootCfg, err := os.ReadFile(filepath.Join(result.StagingPath, "runtime", "fxvcs.cfg"))
	if err != nil || !strings.Contains(string(rootCfg), "exec config/server.cfg\n") {
		t.Fatalf("root cfg did not include hook fragment: %q %v", rootCfg, err)
	}
	published, err := PublishReleaseStaging(result, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(published.Path, "runtime", "config", "server.cfg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(published.Path, "runtime", "config", "server.cfg"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := co.AssembleReleaseStagingWithOptions(ctx, rel, inputs, realizationID, deployRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishReleaseStaging(second, deployRoot); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("tampered secret output was reused: %v", err)
	}
}

func TestAssembleBindsExplicitPersistentFileAndDirectory(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "tools", resources: map[string]string{"admin-tools": ""}, persistent: map[string]map[string]string{
		"admin-tools": {"data/logs": "directory", "data/bans.json": "file"},
	}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "unused"}); err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	binding := domain.AgentTargetBinding{Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: %#v %v", plan, err)
	}
	rel, _ := co.GetRelease(ctx, "v1")
	result, err := co.AssembleReleaseStaging(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Metadata.PersistentPaths) != 2 {
		t.Fatalf("persistent metadata: %#v", result.Metadata.PersistentPaths)
	}
	published, err := PublishReleaseStaging(result, deployRoot)
	if err != nil {
		t.Fatal(err)
	}
	for declared, kind := range map[string]string{"data/logs": "directory", "data/bans.json": "file"} {
		link := filepath.Join(published.Path, "resources", "[tools]", "admin-tools", filepath.FromSlash(declared))
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a persistent symlink: %v", declared, err)
		}
		target, _ := os.Readlink(link)
		resolved := filepath.Clean(filepath.Join(filepath.Dir(link), target))
		want, _ := layout.PersistentPath(deployRoot, component.id, "admin-tools", declared)
		if resolved != want {
			t.Fatalf("%s resolves to %s, want %s", declared, resolved, want)
		}
		if kind == "directory" {
			if stat, err := os.Stat(want); err != nil || !stat.IsDir() {
				t.Fatalf("persistent directory not created: %v", err)
			}
		} else if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("persistent file should be runtime-created, got %v", err)
		}
	}
}

func TestAssembleRefusesSourceControlledPersistentSeed(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "tools", resources: map[string]string{"admin-tools": ""}, persistent: map[string]map[string]string{
		"admin-tools": {"data/bans.json": "file"},
	}})
	component.work.WriteFile("admin-tools/data/bans.json", []byte("seed"))
	component.push("add unsafe persistent seed")
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	rel, _ := co.GetRelease(ctx, "v1")
	releaseDigest, _ := canonical.Digest(rel)
	inputs := domain.RealizationInputs{ReleaseName: "v1", ReleaseDigest: releaseDigest, SourceSetDigest: rel.Spec.SourceSetDigest, EnvironmentName: "prd", EnvironmentRevision: 1, TargetContentDigest: testContentDigest([]byte("target")), HookDigests: map[string]string{}, VerifierBindings: map[string]string{}, SecretVersions: map[string]string{}}
	id, _ := integrity.RealizationID(inputs)
	_, err := co.AssembleReleaseStaging(ctx, rel, inputs, id, filepath.Join(t.TempDir(), "deploy"))
	if err == nil || !strings.Contains(err.Error(), "source-controlled seed") {
		t.Fatalf("persistent seed accepted: %v", err)
	}
}

func TestAssembleGeneratesAuthenticatedResourceStateHelperLastWithoutPersistingToken(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "core", resources: map[string]string{"chat": ""}})
	addComponent(t, co, component, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	rel, _ := co.GetRelease(ctx, "v1")
	releaseDigest, _ := canonical.Digest(rel)
	version := "hmac-sha256:" + strings.Repeat("a", 64)
	inputs := domain.RealizationInputs{ReleaseName: "v1", ReleaseDigest: releaseDigest, SourceSetDigest: rel.Spec.SourceSetDigest, EnvironmentName: "prd", EnvironmentRevision: 1, TargetContentDigest: testContentDigest([]byte("target")), HookDigests: map[string]string{}, VerifierBindings: map[string]string{}, SecretVersions: map[string]string{"fivem/health-token": version}}
	id, _ := integrity.RealizationID(inputs)
	token := []byte("resource-state-secret-token")
	result, err := co.AssembleReleaseStagingWithOptions(ctx, rel, inputs, id, filepath.Join(t.TempDir(), "deploy"), AssemblyOptions{SecretVersions: map[string]string{"fivem/health-token": version}, ResourceStateToken: token})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(result.MetadataPath)
	if err != nil || bytes.Contains(metadata, token) {
		t.Fatalf("metadata leaked token: %q err=%v", metadata, err)
	}
	configPath := filepath.Join(result.StagingPath, "runtime", "config", "fxvcs-health.cfg")
	config, err := os.ReadFile(configPath)
	if err != nil || !bytes.Contains(config, token) {
		t.Fatalf("secret config=%q err=%v", config, err)
	}
	if info, err := os.Stat(configPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o400) {
		t.Fatalf("secret config mode=%v err=%v", info.Mode().Perm(), err)
	}
	server, err := os.ReadFile(filepath.Join(result.StagingPath, "resources", "[fxvcs]", "fxvcs-health", "server.lua"))
	if err != nil || bytes.Contains(server, token) || !bytes.Contains(server, []byte(`GetResourceState(name)`)) {
		t.Fatalf("helper source invalid: token=%v err=%v", bytes.Contains(server, token), err)
	}
	cfg, err := os.ReadFile(filepath.Join(result.StagingPath, "runtime", "fxvcs.cfg"))
	if err != nil || !strings.HasSuffix(string(cfg), "ensure chat\nensure fxvcs-health\n") {
		t.Fatalf("runtime cfg=%q err=%v", cfg, err)
	}
}

func TestRuntimeResourceIsValidatedButNotMaterializedOrEnsured(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "core", resources: map[string]string{"chat": ""}})
	addComponent(t, co, component, StackAddInput{})
	runtimeCatalog := &domain.RuntimeResourceCatalog{Envelope: domain.Envelope{APIVersion: "fxvcs.dev/v1alpha1", Kind: "RuntimeResourceCatalog"}, Spec: domain.RuntimeResourceCatalogSpec{Resources: []domain.RuntimeResource{{
		ID: "runtime:system/monitor", Name: "monitor", Source: domain.ResourceSourceRuntime, Activation: domain.ActivationEnabled,
	}}}}
	if err := co.SaveRuntimeResources(runtimeCatalog); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "unused"}); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "system-resources")
	if err := os.MkdirAll(filepath.Join(runtimeRoot, "monitor"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeRoot, _ = filepath.EvalSymlinks(runtimeRoot)
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	binding := domain.AgentTargetBinding{Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service", SystemResourcesRoot: runtimeRoot}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: %#v %v", plan, err)
	}
	rel, _ := co.GetRelease(ctx, "v1")
	evidence, err := runtimecheck.ValidateSystemResources(runtimeRoot, rel.Spec.RuntimeResources)
	if err != nil {
		t.Fatal(err)
	}
	result, err := co.AssembleReleaseStagingWithOptions(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot, AssemblyOptions{RuntimeEvidence: &evidence})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(result.StagingPath, "runtime", "fxvcs.cfg"))
	if err != nil || strings.Contains(string(cfg), "monitor") || !strings.Contains(string(cfg), "ensure chat") {
		t.Fatalf("runtime cfg: %q %v", cfg, err)
	}
}

func TestExternalResourceRequiresVerifierEvidenceAndIsNeverEnsured(t *testing.T) {
	ctx := context.Background()
	co := coordFixture(t)
	component := newComponent(t, componentSpec{name: "core", resources: map[string]string{"chat": ""}})
	addComponent(t, co, component, StackAddInput{})
	catalog := &domain.RuntimeResourceCatalog{Envelope: domain.Envelope{APIVersion: "fxvcs.dev/v1alpha1", Kind: "RuntimeResourceCatalog"}, Spec: domain.RuntimeResourceCatalogSpec{Resources: []domain.RuntimeResource{{
		ID: "external:host/custom-anticheat", Name: "custom-anticheat", Source: domain.ResourceSourceExternal, Activation: domain.ActivationEnabled,
		Verifier: &domain.VerifierContract{Contract: "resource-present", Version: 1}, Lifecycle: &domain.LifecycleContract{Ownership: "external", Actions: "observe-only"},
	}}}}
	if err := co.SaveRuntimeResources(catalog); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, co, createInput("v1"))
	if _, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd", AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "unused"}); err != nil {
		t.Fatal(err)
	}
	deployRoot := filepath.Join(t.TempDir(), "deploy")
	binding := domain.AgentTargetBinding{Target: "prd", DeployRoot: deployRoot, ServerData: filepath.Join(t.TempDir(), "server"), Runtime: domain.RuntimeBinding{Supervisor: "systemd", SystemdUnit: "fxserver.service"}, Verifiers: map[string]domain.VerifierBinding{"resource-present": {Implementation: "rcon-resource-state", ImplementationVersion: 1, EndpointRef: "local-rcon"}}}
	config := domain.AgentConfig{Spec: domain.AgentConfigSpec{AgentID: "agent-a", Targets: []domain.AgentTargetBinding{binding}, SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1, MaxAutomaticRollbacks: 1}}}
	plan, err := co.ResolveTargetPlan(ctx, "prd", config, binding)
	if err != nil || !plan.Applicable() || plan.Inputs.VerifierBindings["resource-present"] == "" {
		t.Fatalf("plan: %#v %v", plan, err)
	}
	rel, _ := co.GetRelease(ctx, "v1")
	if _, err := co.AssembleReleaseStagingWithOptions(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot, AssemblyOptions{}); err == nil || !strings.Contains(err.Error(), "external verifier evidence") {
		t.Fatalf("external assembly accepted without evidence: %v", err)
	}
	evidenceDigest, _ := canonical.Digest(map[string]any{"verified": true})
	result, err := co.AssembleReleaseStagingWithOptions(ctx, rel, plan.Inputs, plan.RealizationID, deployRoot, AssemblyOptions{ExternalVerifierEvidence: map[string]string{"external:host/custom-anticheat": evidenceDigest}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(result.StagingPath, "runtime", "fxvcs.cfg"))
	if err != nil || strings.Contains(string(cfg), "custom-anticheat") || !strings.Contains(string(cfg), "ensure chat") {
		t.Fatalf("runtime cfg: %q %v", cfg, err)
	}
}

func stringPointer(value string) *string { return &value }

func TestValidateReleaseEntriesRejectsSymlinksAndGitlinks(t *testing.T) {
	component := domain.ReleaseComponent{RepositoryID: "repo_one", Mount: "resources/one", Descriptor: domain.ComponentDescriptor{SourceRoot: "."}}
	for _, mode := range []string{"120000", "160000"} {
		err := validateReleaseEntries(component, []gitops.TreeEntry{{Path: "unsafe", Mode: mode}}, map[string]string{})
		if err == nil || !strings.Contains(err.Error(), mode) {
			t.Fatalf("mode %s was not refused: %v", mode, err)
		}
	}
}

func TestValidateReleaseEntriesRejectsCaseFoldedMountCollision(t *testing.T) {
	destinations := map[string]string{}
	first := domain.ReleaseComponent{RepositoryID: "repo_one", Mount: "resources/shared", Descriptor: domain.ComponentDescriptor{SourceRoot: "src"}}
	second := domain.ReleaseComponent{RepositoryID: "repo_two", Mount: "resources/shared", Descriptor: domain.ComponentDescriptor{SourceRoot: "."}}
	if err := validateReleaseEntries(first, []gitops.TreeEntry{{Path: "src/Thing.lua", Mode: "100644"}}, destinations); err != nil {
		t.Fatal(err)
	}
	err := validateReleaseEntries(second, []gitops.TreeEntry{{Path: "thing.lua", Mode: "100644"}}, destinations)
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("case-folded collision was not refused: %v", err)
	}
}

func TestValidateReleaseEntriesRejectsFileDirectoryCollision(t *testing.T) {
	destinations := map[string]string{}
	first := domain.ReleaseComponent{RepositoryID: "repo_one", Mount: "resources/shared", Descriptor: domain.ComponentDescriptor{SourceRoot: "."}}
	second := domain.ReleaseComponent{RepositoryID: "repo_two", Mount: "resources/shared/file", Descriptor: domain.ComponentDescriptor{SourceRoot: "."}}
	if err := validateReleaseEntries(first, []gitops.TreeEntry{{Path: "file", Mode: "100644"}}, destinations); err != nil {
		t.Fatal(err)
	}
	if err := validateReleaseEntries(second, []gitops.TreeEntry{{Path: "child", Mode: "100644"}}, destinations); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("file/directory collision was not refused: %v", err)
	}
}

func TestResolveHookDigestsRequiresAllowlistAndRetainsTypedSecretRequest(t *testing.T) {
	target := domain.Target{Spec: domain.TargetSpec{Hooks: &domain.HookRequests{PreActivate: []domain.HookRequest{{ID: "cfg", Type: "template.v1"}}}}}
	if _, err := resolveHookDigests(target, domain.AgentTargetBinding{}); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("missing hook allowlist was not refused: %v", err)
	}
	binding := domain.AgentTargetBinding{Hooks: map[string]domain.HookDefinition{"cfg": {Type: "template.v1"}}}
	digests, err := resolveHookDigests(target, binding)
	if err != nil || digests["cfg"] == "" {
		t.Fatalf("allowlisted hook did not resolve: %#v, %v", digests, err)
	}
	ref := domain.SecretRef{Namespace: "server", Key: "token"}
	target.Spec.Hooks.PreActivate[0].Inputs = map[string]domain.InputSource{"token": {Secret: &ref}}
	withSecret, err := resolveHookDigests(target, binding)
	if err != nil || withSecret["cfg"] == "" || withSecret["cfg"] == digests["cfg"] {
		t.Fatalf("typed secret request did not affect hook identity: %#v %v", withSecret, err)
	}
}
