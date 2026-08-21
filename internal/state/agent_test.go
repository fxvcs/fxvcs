package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

func testDeployment() domain.Deployment {
	return domain.Deployment{
		RealizationID: "rz_abc123",
		Target:        "dev",
		Inputs: domain.RealizationInputs{
			ReleaseName:         "v1.0.0",
			ReleaseDigest:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourceSetDigest:     "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			EnvironmentName:     "dev",
			EnvironmentRevision: 1,
			TargetContentDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			HookDigests:         map[string]string{"render": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
			VerifierBindings:    map[string]string{},
			SecretVersions:      map[string]string{"fivem/rcon-password": "hmac-sha256:opaque"},
		},
		Phase:              "staging",
		RetryHeld:          true,
		AttemptInvalidated: true,
		StartedAt:          "2026-08-20T20:00:00Z",
		FinishedAt:         "",
		ObservedRuntime: &domain.ObservedRuntime{
			FXServerBuild:         12345,
			FXServerVersionSource: "version-file",
		},
		ProcessGeneration: "pid:42:start:99",
	}
}

func TestAgentStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "agent.db")
	db, err := OpenAgent(ctx, path, Options{Create: true, Target: "dev", InstalledVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if version, err := db.SchemaVersion(); err != nil || version != format.AgentStateSchema {
		t.Fatalf("schema version = %d, %v; want %d", version, err, format.AgentStateSchema)
	}
	meta, err := db.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[MetaDatabase] != KindAgentState || meta[MetaTarget] != "dev" {
		t.Fatalf("unexpected meta: %#v", meta)
	}

	dep := testDeployment()
	if err := db.PutDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.Deployment(ctx, dep.RealizationID)
	if err != nil || !ok {
		t.Fatalf("deployment: ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(dep, got); diff != "" {
		t.Fatalf("deployment mismatch (-want +got):\n%s", diff)
	}

	state := TargetState{CurrentRealizationID: dep.RealizationID, LastKnownGoodRealizationID: dep.RealizationID, ObservedAt: "2026-08-20T20:01:00Z"}
	if err := db.ObserveTargetState(ctx, state); err != nil {
		t.Fatal(err)
	}
	gotState, err := db.TargetState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(state, gotState); diff != "" {
		t.Fatalf("target state mismatch (-want +got):\n%s", diff)
	}

	sample := HealthSample{RealizationID: dep.RealizationID, CheckType: "tcp", Interface: "tcp:30120", Success: true, ObservedAt: "2026-08-20T20:01:01Z", LatencyNS: 500}
	if err := db.RecordHealthSample(ctx, sample); err != nil {
		t.Fatal(err)
	}
	samples, err := db.HealthSamples(ctx, dep.RealizationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].ID == 0 {
		t.Fatalf("unexpected health samples: %#v", samples)
	}
	samples[0].ID = 0
	if diff := cmp.Diff(sample, samples[0]); diff != "" {
		t.Fatalf("health sample mismatch (-want +got):\n%s", diff)
	}
	rollbackAt := time.Date(2026, 8, 20, 20, 1, 30, 0, time.UTC)
	if err := db.RecordRollback(ctx, dep.RealizationID, rollbackAt); err != nil {
		t.Fatal(err)
	}
	rollbacks, err := db.RollbackTimes(ctx, rollbackAt.Add(-time.Second))
	if err != nil || len(rollbacks) != 1 || !rollbacks[0].Equal(rollbackAt) {
		t.Fatalf("rollback events: %#v %v", rollbacks, err)
	}

	op := OperationRecord{OperationID: "op-1", Operation: "activate", RealizationID: dep.RealizationID, Phase: "prepared", Payload: json.RawMessage(`{"current":"rz_old"}`)}
	if err := db.BeginOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	open, err := db.OpenOperations(ctx)
	if err != nil || len(open) != 1 || open[0].Phase != "prepared" {
		t.Fatalf("unexpected open operations: %#v, %v", open, err)
	}
	if err := db.AdvanceOperation(ctx, op.OperationID, "activated", json.RawMessage(`{"current":"rz_abc123"}`), "committed"); err != nil {
		t.Fatal(err)
	}
	if open, err := db.OpenOperations(ctx); err != nil || len(open) != 0 {
		t.Fatalf("closed operation remains open: %#v, %v", open, err)
	}

	plan := PrunePlanRecord{PlanID: "prune-1", CreatedAt: "2026-08-20T20:02:00Z", Status: "planned", Candidates: json.RawMessage(`[{"realizationID":"rz_old"}]`)}
	if err := db.PutPrunePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	gotPlan, ok, err := db.PrunePlan(ctx, plan.PlanID)
	if err != nil || !ok {
		t.Fatalf("prune plan: ok=%v err=%v", ok, err)
	}
	if diff := cmp.Diff(plan, gotPlan); diff != "" {
		t.Fatalf("prune plan mismatch (-want +got):\n%s", diff)
	}
	if err := db.RetainRealization(ctx, dep.RealizationID, "operator"); err != nil {
		t.Fatal(err)
	}
	retained, err := db.RetainedRealizations(ctx)
	if err != nil || retained[dep.RealizationID] != "operator" {
		t.Fatalf("unexpected retained realizations: %#v, %v", retained, err)
	}
}

func TestAgentStateRejectsWrongTarget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, err := OpenAgent(ctx, path, Options{Create: true, Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenAgent(ctx, path, Options{Target: "prd"})
	if !errors.Is(err, ErrWrongTarget) {
		t.Fatalf("want ErrWrongTarget, got %v", err)
	}
}

func TestAgentStateRejectsInvalidDeploymentBeforeWrite(t *testing.T) {
	ctx := context.Background()
	db, err := OpenAgent(ctx, filepath.Join(t.TempDir(), "agent.db"), Options{Create: true, Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dep := testDeployment()
	dep.Phase = "invented"
	if err := db.PutDeployment(ctx, dep); err == nil {
		t.Fatal("expected invalid phase error")
	}
	all, err := db.Deployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("invalid deployment was persisted: %#v", all)
	}
}
