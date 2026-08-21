// Package reconcile contains the pure Stage 4 target reconciliation planner.
// Planning accepts already-loaded, authorized documents and observations and
// performs no filesystem, database, Git, network, secret, or runtime I/O.
package reconcile

import (
	"fmt"
	"sort"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/integrity"
)

// RefusalCode is a stable machine-readable reason why a plan cannot apply.
type RefusalCode string

const (
	RefusalInvalidBinding       RefusalCode = "invalid-binding"
	RefusalDesiredStateMismatch RefusalCode = "desired-state-mismatch"
	RefusalUnavailableContent   RefusalCode = "unavailable-content"
	RefusalInsufficientSpace    RefusalCode = "insufficient-space"
	RefusalUnsupportedLifecycle RefusalCode = "unsupported-lifecycle"
	RefusalActivationWindow     RefusalCode = "activation-window"
	RefusalPlayerGate           RefusalCode = "player-gate"
	RefusalInvalidIdentity      RefusalCode = "invalid-realization-identity"
)

type Refusal struct {
	Code    RefusalCode `json:"code"`
	Path    string      `json:"path,omitempty"`
	Message string      `json:"message"`
}

type ContentRequirement struct {
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Present   bool   `json:"present"`
	Available bool   `json:"available"`
}

type SupervisorCapabilities struct {
	Observe         bool `json:"observe"`
	Restart         bool `json:"restart"`
	HotReload       bool `json:"hotReload"`
	PlayerCount     bool `json:"playerCount"`
	ProcessIdentity bool `json:"processIdentity"`
}

type Action struct {
	Phase  string `json:"phase"`
	Action string `json:"action"`
}

type LifecycleMode string

const (
	LifecycleNone        LifecycleMode = "none"
	LifecycleHotReload   LifecycleMode = "hot-resource"
	LifecycleFullRestart LifecycleMode = "full-restart"
)

type ResourceOperation struct {
	Action string `json:"action"` // refresh | ensure | start | stop | restart
	Name   string `json:"name,omitempty"`
}

// PlanInput is the complete immutable/observed input to Plan. Values such as
// content availability, disk capacity and activation gates are observations
// made by the caller before invoking this pure function.
type PlanInput struct {
	AgentConfig          domain.AgentConfig
	Target               domain.Target
	Environment          domain.Environment
	Release              domain.Release
	Binding              domain.AgentTargetBinding
	TargetContentDigest  string
	HookDigests          map[string]string
	SecretVersions       map[string]string
	Content              []ContentRequirement
	DiskAvailableBytes   int64
	CurrentRealizationID string
	RequiresRestart      bool
	LifecycleMode        LifecycleMode
	ResourceOperations   []ResourceOperation
	ActivationAllowed    bool
	PlayerCount          *int
	Supervisor           SupervisorCapabilities
}

type PlanResult struct {
	Target                 string                   `json:"target"`
	RealizationID          string                   `json:"realizationID,omitempty"`
	Inputs                 domain.RealizationInputs `json:"inputs"`
	CurrentRealizationID   string                   `json:"currentRealizationID,omitempty"`
	MissingContent         []ContentRequirement     `json:"missingContent,omitempty"`
	RequiredBytes          int64                    `json:"requiredBytes"`
	EffectiveRetention     int                      `json:"effectiveRetention"`
	AutomaticRollbackLimit int                      `json:"automaticRollbackLimit"`
	Actions                []Action                 `json:"actions,omitempty"`
	LifecycleMode          LifecycleMode            `json:"lifecycleMode,omitempty"`
	ResourceOperations     []ResourceOperation      `json:"resourceOperations,omitempty"`
	Refusals               []Refusal                `json:"refusals,omitempty"`
}

func (p PlanResult) Applicable() bool { return len(p.Refusals) == 0 }

func refuse(out *PlanResult, code RefusalCode, path, message string) {
	out.Refusals = append(out.Refusals, Refusal{Code: code, Path: path, Message: message})
}

// Plan resolves identity, capacity, supervisor actions, gates, and effective
// host safety floors. It never mutates its input or consults external state.
func Plan(in PlanInput) PlanResult {
	out := PlanResult{Target: in.Target.Metadata.Name, CurrentRealizationID: in.CurrentRealizationID}

	if in.AgentConfig.Spec.AgentID == "" || in.Target.Spec.AgentID != in.AgentConfig.Spec.AgentID {
		refuse(&out, RefusalInvalidBinding, "/spec/agentID", "target agentID does not match the host agent")
	}
	if in.Binding.Target != in.Target.Metadata.Name {
		refuse(&out, RefusalInvalidBinding, "/agentConfig/target", "agent binding does not name the desired target")
	}
	if in.Target.Spec.Release != in.Release.Metadata.Name {
		refuse(&out, RefusalDesiredStateMismatch, "/spec/release", "loaded Release does not match Target.spec.release")
	}
	if in.Target.Spec.Environment != in.Environment.Metadata.Name {
		refuse(&out, RefusalDesiredStateMismatch, "/spec/environment", "loaded Environment does not match Target.spec.environment")
	}

	releaseDigest, err := canonical.Digest(in.Release)
	if err != nil {
		refuse(&out, RefusalInvalidIdentity, "/release", err.Error())
	}
	targetDigest := in.TargetContentDigest
	if targetDigest == "" {
		targetDigest, err = canonical.Digest(in.Target)
		if err != nil {
			refuse(&out, RefusalInvalidIdentity, "/target", err.Error())
		}
	}

	verifierDigests := map[string]string{}
	contractSet := make(map[string]bool, len(in.Target.Spec.Verifiers))
	for contract := range in.Target.Spec.Verifiers {
		contractSet[contract] = true
	}
	for _, resource := range in.Release.Spec.RuntimeResources {
		if resource.Source == domain.ResourceSourceExternal && resource.Activation == domain.ActivationEnabled && resource.Verifier != nil {
			contractSet[resource.Verifier.Contract] = true
		}
	}
	contracts := make([]string, 0, len(contractSet))
	for contract := range contractSet {
		contracts = append(contracts, contract)
	}
	sort.Strings(contracts)
	for _, contract := range contracts {
		binding, ok := in.Binding.Verifiers[contract]
		if !ok {
			refuse(&out, RefusalInvalidBinding, "/agentConfig/verifiers/"+contract, "required verifier has no host binding")
			continue
		}
		digest, digestErr := integrity.VerifierBindingDigest(contract, binding)
		if digestErr != nil {
			refuse(&out, RefusalInvalidBinding, "/agentConfig/verifiers/"+contract, digestErr.Error())
			continue
		}
		verifierDigests[contract] = digest
	}
	out.Inputs = domain.RealizationInputs{
		ReleaseName:         in.Release.Metadata.Name,
		ReleaseDigest:       releaseDigest,
		SourceSetDigest:     in.Release.Spec.SourceSetDigest,
		EnvironmentName:     in.Environment.Metadata.Name,
		EnvironmentRevision: in.Environment.Spec.Revision,
		TargetContentDigest: targetDigest,
		HookDigests:         cloneMap(in.HookDigests),
		VerifierBindings:    verifierDigests,
		SecretVersions:      cloneMap(in.SecretVersions),
	}
	if id, identityErr := integrity.RealizationID(out.Inputs); identityErr != nil {
		refuse(&out, RefusalInvalidIdentity, "/realization", identityErr.Error())
	} else {
		out.RealizationID = id
	}

	out.EffectiveRetention = max(in.Target.Spec.Retention.Keep, in.AgentConfig.Spec.SafetyFloors.MinimumRetention)
	if in.Target.Spec.Health != nil {
		out.AutomaticRollbackLimit = min(in.Target.Spec.Health.MaxAutomaticRollbacks, in.AgentConfig.Spec.SafetyFloors.MaxAutomaticRollbacks)
	}
	if in.AgentConfig.Spec.SafetyFloors.DisableAutomaticRollback {
		out.AutomaticRollbackLimit = 0
	}

	content := append([]ContentRequirement(nil), in.Content...)
	sort.Slice(content, func(i, j int) bool { return content[i].Digest < content[j].Digest })
	unavailable := 0
	for _, item := range content {
		if !item.Present {
			out.MissingContent = append(out.MissingContent, item)
			out.RequiredBytes += item.Bytes
			if !item.Available {
				unavailable++
			}
		}
	}
	if unavailable > 0 {
		refuse(&out, RefusalUnavailableContent, "/content", fmt.Sprintf("%d required objects are unavailable", unavailable))
	}
	if in.DiskAvailableBytes >= 0 && out.RequiredBytes > in.DiskAvailableBytes {
		refuse(&out, RefusalInsufficientSpace, "/disk", fmt.Sprintf("need %d bytes, have %d", out.RequiredBytes, in.DiskAvailableBytes))
	}

	if out.RealizationID != "" && out.RealizationID == in.CurrentRealizationID {
		out.Actions = []Action{{Phase: "complete", Action: "observe-current"}}
		return out
	}
	out.Actions = []Action{
		{Phase: "materialization", Action: "materialize-content"},
		{Phase: "rendering", Action: "render-hooks"},
		{Phase: "activation", Action: "switch-current"},
	}
	mode := in.LifecycleMode
	if mode == "" && in.RequiresRestart {
		mode = LifecycleFullRestart
	}
	out.LifecycleMode = mode
	out.ResourceOperations = append([]ResourceOperation(nil), in.ResourceOperations...)
	if mode != "" && mode != LifecycleNone && mode != LifecycleHotReload && mode != LifecycleFullRestart {
		refuse(&out, RefusalUnsupportedLifecycle, "/lifecycle", "planner received an unknown lifecycle mode")
	}
	if mode != "" && (!in.Supervisor.Observe || !in.Supervisor.ProcessIdentity) {
		refuse(&out, RefusalUnsupportedLifecycle, "/supervisor", "safe activation requires observe and process identity capabilities")
	}
	if mode != "" && !in.ActivationAllowed {
		refuse(&out, RefusalActivationWindow, "/activationWindow", "activation window is closed")
	}
	switch mode {
	case LifecycleFullRestart:
		if !in.Supervisor.Restart || !in.Supervisor.Observe || !in.Supervisor.ProcessIdentity {
			refuse(&out, RefusalUnsupportedLifecycle, "/supervisor", "safe restart requires observe, restart, and process identity capabilities")
		}
		if in.Target.Spec.Health != nil && in.Target.Spec.Health.MaxPlayersForRestart != nil {
			if in.PlayerCount == nil || !in.Supervisor.PlayerCount {
				refuse(&out, RefusalUnsupportedLifecycle, "/playerCount", "player gate requires a supported player-count observation")
			} else if *in.PlayerCount > *in.Target.Spec.Health.MaxPlayersForRestart {
				refuse(&out, RefusalPlayerGate, "/playerCount", fmt.Sprintf("player count %d exceeds limit %d", *in.PlayerCount, *in.Target.Spec.Health.MaxPlayersForRestart))
			}
		}
		out.Actions = append(out.Actions, Action{Phase: "lifecycle", Action: "restart-runtime"})
	case LifecycleHotReload:
		if !in.Supervisor.HotReload {
			refuse(&out, RefusalUnsupportedLifecycle, "/supervisor", "hot-resource lifecycle requires a credentialed resource adapter")
		}
		if len(in.ResourceOperations) == 0 {
			refuse(&out, RefusalUnsupportedLifecycle, "/lifecycle", "hot-resource lifecycle requires typed resource operations")
		}
		out.Actions = append(out.Actions, Action{Phase: "lifecycle", Action: "reload-resources"})
	case LifecycleNone:
		out.Actions = append(out.Actions, Action{Phase: "lifecycle", Action: "observe-process-generation"})
	}
	out.Actions = append(out.Actions, Action{Phase: "readiness", Action: "evaluate-health"})
	return out
}

func cloneMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
