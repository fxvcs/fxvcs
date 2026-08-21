package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/diskspace"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/materialize"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/reconcile"
	"fxvcs.dev/fxvcs/internal/release"
	"fxvcs.dev/fxvcs/internal/runtimecheck"
	"fxvcs.dev/fxvcs/internal/secrets"
)

// ResolveTargetPlan loads and verifies the immutable desired-state inputs for
// one target before handing them to the pure Stage 4 planner. It performs no
// activation, runtime control, or agent database mutation.
func (c *Coord) ResolveTargetPlan(ctx context.Context, targetName string, config domain.AgentConfig, binding domain.AgentTargetBinding) (reconcile.PlanResult, error) {
	return c.resolveTargetPlan(ctx, targetName, config, binding, nil)
}

// TargetPlanObservations are host facts that cannot be inferred from
// AgentConfig. Apply supplies them from the selected typed supervisor and an
// injected activation-window evaluator; dry-run callers omit them and every
// observation-dependent gate stays closed.
type TargetPlanObservations struct {
	Supervisor        reconcile.SupervisorCapabilities
	PlayerCount       *int
	ActivationAllowed bool
}

func (c *Coord) ResolveTargetPlanWithObservations(ctx context.Context, targetName string, config domain.AgentConfig, binding domain.AgentTargetBinding, observations TargetPlanObservations) (reconcile.PlanResult, error) {
	return c.resolveTargetPlan(ctx, targetName, config, binding, &observations)
}

func (c *Coord) resolveTargetPlan(ctx context.Context, targetName string, config domain.AgentConfig, binding domain.AgentTargetBinding, observations *TargetPlanObservations) (reconcile.PlanResult, error) {
	target, err := c.GetTarget(ctx, targetName)
	if err != nil {
		return reconcile.PlanResult{}, err
	}
	environment, err := c.GetEnvironment(target.Spec.Environment)
	if err != nil {
		return reconcile.PlanResult{}, err
	}
	rel, err := c.GetRelease(ctx, target.Spec.Release)
	if err != nil {
		return reconcile.PlanResult{}, err
	}

	content, err := c.resolveReleaseContent(ctx, rel, binding.DeployRoot)
	if err != nil {
		return reconcile.PlanResult{}, err
	}
	hookDigests, err := resolveHookDigests(target, binding)
	if err != nil {
		return reconcile.PlanResult{}, err
	}
	secretVersions := map[string]string{}
	if targetHasIdentitySecrets(target) {
		forbidden := []string{c.Root, c.GitDir, binding.DeployRoot, binding.ServerData, binding.Runtime.TxData}
		for alias, provider := range binding.Secrets {
			if provider.Type == "envfile" {
				if err := secrets.ValidateEnvfile(provider.Path, forbidden); err != nil {
					return reconcile.PlanResult{}, fmt.Errorf("reconcile: secret provider %s: %w", alias, err)
				}
			}
		}
		key, err := secrets.LoadHMACKey(filepath.Join(binding.DeployRoot, layout.StateDir, "secret-hmac.key"))
		if err != nil {
			return reconcile.PlanResult{}, fmt.Errorf("reconcile: secret-fingerprint-key-missing: %w", err)
		}
		resolver, err := secrets.NewTargetResolver(target.Metadata.Name, binding.Secrets, key, os.LookupEnv)
		clear(key)
		if err != nil {
			return reconcile.PlanResult{}, err
		}
		defer resolver.Close()
		options, err := c.ResolveAssemblyHooks(ctx, target, environment, rel, binding, resolver)
		if err != nil {
			return reconcile.PlanResult{}, err
		}
		hookDigests = options.HookDigests
		secretVersions = options.SecretVersions
		if targetNeedsResourceState(target) {
			token, version, err := resolveResourceStateSecret(ctx, target, binding, resolver)
			if err != nil {
				return reconcile.PlanResult{}, err
			}
			clear(token)
			ref := binding.Runtime.ResourceState.TokenSecret.Namespace + "/" + binding.Runtime.ResourceState.TokenSecret.Key
			secretVersions[ref] = version
		}
	} else if target.Spec.Hooks != nil {
		options, err := c.ResolveAssemblyHooks(ctx, target, environment, rel, binding, nil)
		if err != nil {
			return reconcile.PlanResult{}, err
		}
		hookDigests = options.HookDigests
	}
	if _, err := runtimecheck.ValidateSystemResources(binding.Runtime.SystemResourcesRoot, rel.Spec.RuntimeResources); err != nil {
		return reconcile.PlanResult{}, fmt.Errorf("reconcile: validate target runtime resources: %w", err)
	}
	current, err := activation.ObserveCurrent(binding.DeployRoot)
	if errors.Is(err, activation.ErrNoCurrent) {
		current = ""
	} else if err != nil {
		return reconcile.PlanResult{}, err
	}
	availableBytes, err := diskspace.Available(binding.DeployRoot)
	if err != nil {
		return reconcile.PlanResult{}, fmt.Errorf("reconcile: observe deploy filesystem capacity: %w", err)
	}

	caps := reconcile.SupervisorCapabilities{}
	switch binding.Runtime.Supervisor {
	case "systemd":
		caps.Observe, caps.Restart, caps.ProcessIdentity = true, true, true
	case "manual":
		// A concrete manual observer is host-injected. Configuration alone
		// never proves that one exists, so planning stays fail-closed.
	case "txadmin":
		// Stage 4 deliberately withholds txAdmin capabilities until the live
		// lab contract is proved and a transport is supplied.
	}
	activationAllowed := target.Spec.Health == nil || target.Spec.Health.ActivationWindow == nil || *target.Spec.Health.ActivationWindow == ""
	if !activationAllowed {
		// A declared schedule is a safety gate. Until a clock/schedule
		// evaluator supplies a proved observation, planning stays closed.
		activationAllowed = false
	}
	playerCount := (*int)(nil)
	if observations != nil {
		caps = observations.Supervisor
		activationAllowed = observations.ActivationAllowed
		playerCount = observations.PlayerCount
	}
	if binding.Runtime.RCON != nil {
		caps.HotReload = true
	}
	input := reconcile.PlanInput{
		AgentConfig:          config,
		Target:               target,
		Environment:          environment,
		Release:              rel,
		Binding:              binding,
		HookDigests:          hookDigests,
		SecretVersions:       secretVersions,
		Content:              content,
		DiskAvailableBytes:   availableBytes,
		CurrentRealizationID: current,
		ActivationAllowed:    activationAllowed,
		PlayerCount:          playerCount,
		Supervisor:           caps,
	}
	preliminary := reconcile.Plan(input)
	if preliminary.RealizationID == "" || preliminary.RealizationID == current {
		return preliminary, nil
	}
	mode, operations, err := c.lifecyclePlan(ctx, binding.DeployRoot, current, preliminary.Inputs, target, rel)
	if err != nil {
		return reconcile.PlanResult{}, err
	}
	input.LifecycleMode = mode
	input.ResourceOperations = operations
	return reconcile.Plan(input), nil
}

func targetHasSecretHook(target domain.Target) bool {
	if target.Spec.Hooks == nil {
		return false
	}
	phases := [][]domain.HookRequest{target.Spec.Hooks.PostMaterialize, target.Spec.Hooks.PreActivate, target.Spec.Hooks.PostActivate, target.Spec.Hooks.OnFailure}
	for _, requests := range phases {
		for _, request := range requests {
			for _, input := range request.Inputs {
				if input.Secret != nil {
					return true
				}
			}
		}
	}
	return false
}

func targetHasIdentitySecrets(target domain.Target) bool {
	return targetHasSecretHook(target) || targetNeedsResourceState(target)
}

func (c *Coord) resolveReleaseContent(ctx context.Context, rel domain.Release, deployRoot string) ([]reconcile.ContentRequirement, error) {
	checker, err := c.ObjectChecker(ctx)
	if err != nil {
		return nil, err
	}
	store := materialize.Store{DeployRoot: deployRoot}
	requirements := map[string]reconcile.ContentRequirement{}
	destinations := map[string]string{}

	for _, component := range rel.Spec.Components {
		tree, err := c.Tree(ctx, component.RepositoryID, component.Commit)
		if err != nil {
			return nil, err
		}
		entries, err := tree.Entries(ctx)
		if err != nil {
			return nil, err
		}
		if err := validateReleaseEntries(component, entries, destinations); err != nil {
			return nil, err
		}
		root, _, err := release.AssetRoot(ctx, tree, component.StorageDomainID)
		if err != nil {
			return nil, err
		}
		if root != component.AssetRoot {
			return nil, fmt.Errorf("reconcile: component %s asset root is %s, release pins %s", component.RepositoryID, root, component.AssetRoot)
		}
		pointers, err := release.Pointers(ctx, tree, component.StorageDomainID)
		if err != nil {
			return nil, err
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			return nil, err
		}
		report, err := checker.CheckComponent(ctx, cfg, pointers)
		if err != nil {
			return nil, err
		}
		available := len(report.Remotes) > 0 && len(report.Missing) == 0 && len(report.Unverifiable) == 0
		for _, p := range pointers {
			req, exists := requirements[p.OID]
			if exists && req.Bytes != p.Size {
				return nil, fmt.Errorf("reconcile: digest %s has conflicting sizes %d and %d", p.OID, req.Bytes, p.Size)
			}
			present := store.Verify(p.OID, p.Size) == nil
			if !present {
				materializedPath, pathErr := store.Path(p.OID)
				if pathErr != nil {
					return nil, pathErr
				}
				if _, statErr := os.Lstat(materializedPath); statErr == nil {
					return nil, fmt.Errorf("reconcile: materialized content %s failed verification", p.OID)
				} else if !errors.Is(statErr, os.ErrNotExist) {
					return nil, statErr
				}
			}
			itemAvailable := available
			if exists {
				present = present || req.Present
				itemAvailable = itemAvailable || req.Available
			}
			requirements[p.OID] = reconcile.ContentRequirement{Digest: p.OID, Bytes: p.Size, Present: present, Available: itemAvailable || present}
		}
	}
	out := make([]reconcile.ContentRequirement, 0, len(requirements))
	for _, req := range requirements {
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

func validateReleaseEntries(component domain.ReleaseComponent, entries []gitops.TreeEntry, destinations map[string]string) error {
	root := component.Descriptor.SourceRoot
	if root == "" {
		return fmt.Errorf("reconcile: component %s has an empty source root", component.RepositoryID)
	}
	if root != "." {
		if _, err := pathsafe.Normalize(root); err != nil {
			return fmt.Errorf("reconcile: component %s source root: %w", component.RepositoryID, err)
		}
	}
	if _, err := pathsafe.Normalize(component.Mount); err != nil {
		return fmt.Errorf("reconcile: component %s mount: %w", component.RepositoryID, err)
	}
	if !strings.HasPrefix(component.Mount, "resources/") {
		return fmt.Errorf("reconcile: component %s mount %s is outside resources/", component.RepositoryID, component.Mount)
	}
	for _, entry := range entries {
		if !entry.IsRegular() {
			return fmt.Errorf("reconcile: component %s contains unsupported Git mode %s at %s", component.RepositoryID, entry.Mode, entry.Path)
		}
		rel, included := sourceRelative(root, entry.Path)
		if !included {
			continue
		}
		destination := path.Join(component.Mount, rel)
		key := strings.ToLower(destination)
		existingPaths := make([]string, 0, len(destinations))
		for existing := range destinations {
			existingPaths = append(existingPaths, existing)
		}
		sort.Strings(existingPaths)
		for _, existing := range existingPaths {
			if key == existing || strings.HasPrefix(key, existing+"/") || strings.HasPrefix(existing, key+"/") {
				return fmt.Errorf("reconcile: deployment path collision between %s and %s at %s", destinations[existing], component.RepositoryID, destination)
			}
		}
		destinations[key] = component.RepositoryID
	}
	return nil
}

func sourceRelative(root, entry string) (string, bool) {
	if root == "." {
		return entry, true
	}
	if entry == root {
		return "", false
	}
	prefix := root + "/"
	if !strings.HasPrefix(entry, prefix) {
		return "", false
	}
	return strings.TrimPrefix(entry, prefix), true
}

func resolveHookDigests(target domain.Target, binding domain.AgentTargetBinding) (map[string]string, error) {
	out := map[string]string{}
	if target.Spec.Hooks == nil {
		return out, nil
	}
	phases := [][]domain.HookRequest{target.Spec.Hooks.PostMaterialize, target.Spec.Hooks.PreActivate, target.Spec.Hooks.PostActivate, target.Spec.Hooks.OnFailure}
	for _, requests := range phases {
		for _, request := range requests {
			definition, ok := binding.Hooks[request.ID]
			if !ok {
				return nil, fmt.Errorf("reconcile: hook %s has no host allowlist binding", request.ID)
			}
			if request.Type != definition.Type {
				return nil, fmt.Errorf("reconcile: hook %s requests %s but host allows %s", request.ID, request.Type, definition.Type)
			}
			if _, exists := out[request.ID]; exists {
				return nil, fmt.Errorf("reconcile: hook ID %s is requested more than once", request.ID)
			}
			digest, err := canonical.Digest(struct {
				Request    domain.HookRequest
				Definition domain.HookDefinition
			}{request, definition})
			if err != nil {
				return nil, err
			}
			out[request.ID] = digest
		}
	}
	return out, nil
}
