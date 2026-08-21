package reconcile

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
)

func digest(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }

func planInput() PlanInput {
	maxPlayers := 4
	return PlanInput{
		AgentConfig: domain.AgentConfig{Spec: domain.AgentConfigSpec{
			AgentID:      "agent-a",
			SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 3, MaxAutomaticRollbacks: 1},
		}},
		Target: domain.Target{
			Metadata: domain.NamedMetadata{Name: "dev"},
			Spec: domain.TargetSpec{
				Release: "v1.0.0", Environment: "dev", AgentID: "agent-a",
				Retention: domain.RetentionPolicy{Keep: 1},
				Health:    &domain.HealthPolicy{MaxAutomaticRollbacks: 2, MaxPlayersForRestart: &maxPlayers},
				Verifiers: map[string]domain.VerifierBinding{"resource-present": {Implementation: "portable-contract", ImplementationVersion: 1}},
			},
		},
		Environment: domain.Environment{Metadata: domain.NamedMetadata{Name: "dev"}, Spec: domain.EnvironmentSpec{Revision: 2}},
		Release:     domain.Release{Metadata: domain.ReleaseMetadata{Name: "v1.0.0", CreatedAt: "2026-08-20T20:00:00Z", CreatedBy: "test"}, Spec: domain.ReleaseSpec{SourceSetDigest: digest("a")}},
		Binding: domain.AgentTargetBinding{Target: "dev", Verifiers: map[string]domain.VerifierBinding{
			"resource-present": {Implementation: "rcon-resource-state", ImplementationVersion: 1, EndpointRef: "local-rcon"},
		}},
		HookDigests:        map[string]string{"render": digest("b")},
		SecretVersions:     map[string]string{"fivem/rcon-password": "hmac-sha256:" + strings.Repeat("c", 64)},
		DiskAvailableBytes: 1000,
		ActivationAllowed:  true,
		Supervisor:         SupervisorCapabilities{Observe: true, Restart: true, PlayerCount: true, ProcessIdentity: true},
	}
}

func TestPlanIsDeterministicAndAppliesSafetyFloors(t *testing.T) {
	in := planInput()
	a := Plan(in)
	b := Plan(in)
	if !a.Applicable() {
		t.Fatalf("unexpected refusals: %#v", a.Refusals)
	}
	if a.RealizationID == "" || !strings.HasPrefix(a.RealizationID, "rz_") {
		t.Fatalf("bad realization ID %q", a.RealizationID)
	}
	if a.EffectiveRetention != 3 || a.AutomaticRollbackLimit != 1 {
		t.Fatalf("safety floors not applied: %#v", a)
	}
	ja, _ := canonical.Marshal(a)
	jb, _ := canonical.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("plan is not deterministic:\n%s\n%s", ja, jb)
	}
}

func TestPlanSortsMissingContentAndRefusesCapacity(t *testing.T) {
	in := planInput()
	in.DiskAvailableBytes = 5
	in.Content = []ContentRequirement{
		{Digest: digest("f"), Bytes: 4, Available: false},
		{Digest: digest("d"), Bytes: 3, Available: true},
		{Digest: digest("e"), Bytes: 100, Present: true, Available: true},
	}
	out := Plan(in)
	if out.RequiredBytes != 7 || len(out.MissingContent) != 2 || out.MissingContent[0].Digest != digest("d") {
		t.Fatalf("unexpected missing content: %#v", out)
	}
	if !hasRefusal(out, RefusalUnavailableContent) || !hasRefusal(out, RefusalInsufficientSpace) {
		t.Fatalf("missing expected refusals: %#v", out.Refusals)
	}
}

func TestPlanLifecycleGatesFailClosed(t *testing.T) {
	in := planInput()
	in.RequiresRestart = true
	in.ActivationAllowed = false
	players := 5
	in.PlayerCount = &players
	out := Plan(in)
	if !hasRefusal(out, RefusalActivationWindow) || !hasRefusal(out, RefusalPlayerGate) {
		t.Fatalf("missing gates: %#v", out.Refusals)
	}
	in.Supervisor.ProcessIdentity = false
	out = Plan(in)
	if !hasRefusal(out, RefusalUnsupportedLifecycle) {
		t.Fatalf("unsafe supervisor accepted: %#v", out.Refusals)
	}
}

func TestPlanHotResourceLifecycleCarriesTypedOperations(t *testing.T) {
	in := planInput()
	in.LifecycleMode = LifecycleHotReload
	in.ResourceOperations = []ResourceOperation{{Action: "refresh"}, {Action: "restart", Name: "city-map"}}
	in.Supervisor.HotReload = true
	out := Plan(in)
	if !out.Applicable() {
		t.Fatalf("unexpected refusals: %#v", out.Refusals)
	}
	if out.LifecycleMode != LifecycleHotReload || len(out.ResourceOperations) != 2 {
		t.Fatalf("typed lifecycle omitted: %#v", out)
	}
	if got := out.Actions[len(out.Actions)-2].Action; got != "reload-resources" {
		t.Fatalf("unexpected lifecycle action %q", got)
	}
}

func TestPlanHotResourceLifecycleFailsClosed(t *testing.T) {
	in := planInput()
	in.LifecycleMode = LifecycleHotReload
	in.ResourceOperations = []ResourceOperation{{Action: "refresh"}}
	out := Plan(in)
	if !hasRefusal(out, RefusalUnsupportedLifecycle) {
		t.Fatalf("missing hot reload capability accepted: %#v", out)
	}

	in.Supervisor.HotReload = true
	in.ResourceOperations = nil
	out = Plan(in)
	if !hasRefusal(out, RefusalUnsupportedLifecycle) {
		t.Fatalf("empty typed hot operation set accepted: %#v", out)
	}
}

func TestPlanNoProcessActionStillRequiresObservationAndActivationGate(t *testing.T) {
	in := planInput()
	in.LifecycleMode = LifecycleNone
	out := Plan(in)
	if !out.Applicable() || out.Actions[len(out.Actions)-2].Action != "observe-process-generation" {
		t.Fatalf("unexpected no-action lifecycle plan: %#v", out)
	}

	in.ActivationAllowed = false
	out = Plan(in)
	if !hasRefusal(out, RefusalActivationWindow) {
		t.Fatalf("closed activation window accepted: %#v", out)
	}

	in.ActivationAllowed = true
	in.Supervisor.ProcessIdentity = false
	out = Plan(in)
	if !hasRefusal(out, RefusalUnsupportedLifecycle) {
		t.Fatalf("missing process identity accepted: %#v", out)
	}
}

func TestPlanCurrentIdentityIsObserveOnly(t *testing.T) {
	in := planInput()
	first := Plan(in)
	in.CurrentRealizationID = first.RealizationID
	out := Plan(in)
	if len(out.Actions) != 1 || out.Actions[0].Action != "observe-current" {
		t.Fatalf("unexpected no-op actions: %#v", out.Actions)
	}
}

func TestPlanIncludesEnabledExternalVerifierBindingInIdentity(t *testing.T) {
	in := planInput()
	in.Target.Spec.Verifiers = nil
	in.Release.Spec.RuntimeResources = []domain.RuntimeResource{{ID: "external:host/custom", Source: domain.ResourceSourceExternal, Activation: domain.ActivationEnabled, Verifier: &domain.VerifierContract{Contract: "resource-present", Version: 1}, Lifecycle: &domain.LifecycleContract{Ownership: "external", Actions: "observe-only"}}}
	out := Plan(in)
	if !out.Applicable() || out.Inputs.VerifierBindings["resource-present"] == "" {
		t.Fatalf("external verifier binding omitted: %#v", out)
	}
	delete(in.Binding.Verifiers, "resource-present")
	out = Plan(in)
	if !hasRefusal(out, RefusalInvalidBinding) {
		t.Fatalf("missing external binding accepted: %#v", out)
	}
}

func hasRefusal(plan PlanResult, code RefusalCode) bool {
	for _, refusal := range plan.Refusals {
		if refusal.Code == code {
			return true
		}
	}
	return false
}
