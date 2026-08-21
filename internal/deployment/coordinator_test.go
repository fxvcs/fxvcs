package deployment

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/health"
	"fxvcs.dev/fxvcs/internal/state"
)

func testDB(t *testing.T) *state.AgentDB {
	db, err := state.OpenAgent(context.Background(), filepath.Join(t.TempDir(), "agent.db"), state.Options{Create: true, Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testDep() domain.Deployment {
	return domain.Deployment{
		RealizationID: "rz_health", Target: "dev", Phase: string(health.PhaseActiveGrace), StartedAt: "2026-08-20T20:00:00Z",
		Inputs: domain.RealizationInputs{
			ReleaseDigest:   "sha256:" + strings.Repeat("a", 64),
			SourceSetDigest: "sha256:" + strings.Repeat("b", 64), TargetContentDigest: "sha256:" + strings.Repeat("c", 64),
			HookDigests: map[string]string{}, VerifierBindings: map[string]string{}, SecretVersions: map[string]string{},
		},
	}
}

func TestHealthyPersistsLastKnownGood(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	now := time.Date(2026, 8, 20, 20, 5, 0, 0, time.UTC)
	coordinator := Coordinator{DB: db, Now: func() time.Time { return now }}
	dep := testDep()
	if err := db.PutDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	updated, err := coordinator.ApplyEvent(ctx, dep, health.Policy{OnFailure: health.OnFailureKeep}, health.EventHealthy, dep.RealizationID)
	if err != nil || updated.Phase != string(health.PhaseActiveHealthy) {
		t.Fatalf("transition: %#v %v", updated, err)
	}
	target, err := db.TargetState(ctx)
	if err != nil || target.LastKnownGoodRealizationID != dep.RealizationID {
		t.Fatalf("target state: %#v %v", target, err)
	}
}

func TestHealthyRefusesMismatchedFilesystemCurrent(t *testing.T) {
	db := testDB(t)
	dep := testDep()
	if err := db.PutDeployment(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
	_, err := (Coordinator{DB: db}).ApplyEvent(context.Background(), dep, health.Policy{OnFailure: health.OnFailureKeep}, health.EventHealthy, "rz_other")
	if !errors.Is(err, ErrCurrentMismatch) {
		t.Fatalf("want current mismatch, got %v", err)
	}
}

func TestRollbackTransitionRecordsCapEvidence(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	dep := testDep()
	if err := db.PutDeployment(ctx, dep); err != nil {
		t.Fatal(err)
	}
	policy := health.Policy{OnFailure: health.OnFailureRollback, MaxAutomaticRollbacks: 1, LocalMaxAutomaticRollbacks: 1, RollbackWindow: time.Hour}
	updated, err := (Coordinator{DB: db}).ApplyEvent(ctx, dep, policy, health.EventUnhealthy, dep.RealizationID)
	if err != nil || updated.Phase != string(health.PhaseRollingBack) || !updated.RetryHeld {
		t.Fatalf("rollback transition: %#v %v", updated, err)
	}
	times, err := db.RollbackTimes(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(times) != 1 {
		t.Fatalf("rollback evidence: %#v %v", times, err)
	}
}
