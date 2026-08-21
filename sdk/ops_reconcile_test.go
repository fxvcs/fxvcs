package sdk

import (
	"context"
	"testing"

	"fxvcs.dev/fxvcs/internal/reconcile"
)

func TestReconcileTargetApplyRequiresAgentBinding(t *testing.T) {
	client, err := New(Options{InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReconcileTarget(context.Background(), ReconcileTargetRequest{Path: t.TempDir(), Target: "dev", Apply: true})
	if result != nil || CodeOf(err) != CodeNotFound {
		t.Fatalf("apply should require its host contract: result=%#v err=%v code=%s", result, err, CodeOf(err))
	}
}

func TestPublicReconcilePlanPreservesTypedEvidence(t *testing.T) {
	plan := reconcile.PlanResult{
		Target: "dev", RealizationID: "rz_next", CurrentRealizationID: "rz_old", RequiredBytes: 12,
		Actions:        []reconcile.Action{{Phase: "materialization", Action: "materialize-content"}},
		Refusals:       []reconcile.Refusal{{Code: reconcile.RefusalUnavailableContent, Path: "/content", Message: "missing"}},
		MissingContent: []reconcile.ContentRequirement{{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Bytes: 12, Available: false}},
	}
	got := publicReconcilePlan(plan)
	if got.Applicable || got.Applied || got.Target != "dev" || got.RealizationID != "rz_next" || len(got.Refusals) != 1 || got.Refusals[0].Code != "unavailable-content" || len(got.MissingContent) != 1 {
		t.Fatalf("public plan lost evidence: %#v", got)
	}
}
