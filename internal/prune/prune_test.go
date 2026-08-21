package prune

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/materialize"
	"fxvcs.dev/fxvcs/internal/state"
)

func fixture(t *testing.T) (string, *state.AgentDB, []Realization) {
	t.Helper()
	root := t.TempDir()
	db, err := state.OpenAgent(context.Background(), filepath.Join(root, "state", "agent.db"), state.Options{Create: true, Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t0 := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	var all []Realization
	for i, id := range []string{"rz_one", "rz_two", "rz_three", "rz_four"} {
		path, _ := layout.Realization(root, id)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "data"), make([]byte, i+1), 0o444); err != nil {
			t.Fatal(err)
		}
		all = append(all, Realization{RealizationID: id, Path: path, CreatedAt: t0.Add(time.Duration(i) * time.Minute), Bytes: int64(i + 1)})
	}
	if err := activation.Switch(root, "", "rz_four"); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveTargetState(context.Background(), state.TargetState{CurrentRealizationID: "rz_four", LastKnownGoodRealizationID: "rz_three", ObservedAt: t0.Add(5 * time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	return root, db, all
}

func pruneDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestBuildProtectsCurrentLKGRetentionAndRetained(t *testing.T) {
	root, _, all := fixture(t)
	plan, err := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Retained: map[string]string{"rz_one": "operator"}, Keep: 1, MinimumRetention: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].RealizationID != "rz_two" || plan.EstimatedBytes != 2 || plan.PlanID == "" {
		t.Fatalf("unexpected plan: %#v", plan)
	}
}

func TestApplyRevalidatesAndDeletesExactCandidates(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	plan, err := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 2, MinimumRetention: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10}); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		if _, err := os.Lstat(candidate.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate remains: %s %v", candidate.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "releases", "rz_four")); err != nil {
		t.Fatalf("current deleted: %v", err)
	}
}

func TestApplyRefusesChangedProtectionsAndRate(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	plan, _ := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 2, MinimumRetention: 2})
	if err := db.RetainRealization(ctx, plan.Candidates[0].RealizationID, "late retain"); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10}); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("late retention ignored: %v", err)
	}
	root2, db2, all2 := fixture(t)
	plan2, _ := Build(PlanInput{DeployRoot: root2, Realizations: all2, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 2, MinimumRetention: 2})
	if err := Apply(ctx, plan2, ApplyOptions{DB: db2, MaxDeletionsPerHour: 0}); !errors.Is(err, ErrRateLimit) {
		t.Fatalf("rate limit ignored: %v", err)
	}
}

func TestRecoverAfterRenamedCandidate(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	plan, _ := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 3, MinimumRetention: 3})
	fail := errors.New("crash")
	err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10, Failpoint: func(point string) error {
		if point == "after-rename" {
			return fail
		}
		return nil
	}})
	if !errors.Is(err, fail) {
		t.Fatalf("failpoint did not fire: %v", err)
	}
	if err := Recover(ctx, root, plan.PlanID, ApplyOptions{DB: db}); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenOperations(ctx); len(open) != 0 {
		t.Fatalf("recovery left operation open: %#v", open)
	}
	if err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10}); err != nil {
		t.Fatalf("resuming recovered plan: %v", err)
	}
}

func TestRecoverBeforeRenameReopensExactCandidate(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	plan, _ := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 3, MinimumRetention: 3})
	crash := errors.New("crash")
	err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10, Failpoint: func(point string) error {
		if point == "before-rename" {
			return crash
		}
		return nil
	}})
	if !errors.Is(err, crash) {
		t.Fatalf("failpoint did not fire: %v", err)
	}
	if err := Recover(ctx, root, plan.PlanID, ApplyOptions{DB: db}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10}); err != nil {
		t.Fatalf("reopened exact candidate: %v", err)
	}
	if _, err := os.Lstat(plan.Candidates[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate remains: %v", err)
	}
}

func TestRecoverAfterRemovedCandidateBeforeEvidence(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	plan, _ := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 3, MinimumRetention: 3})
	fail := errors.New("crash")
	err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10, Failpoint: func(point string) error {
		if point == "after-remove" {
			return fail
		}
		return nil
	}})
	if !errors.Is(err, fail) {
		t.Fatalf("failpoint did not fire: %v", err)
	}
	if err := Recover(ctx, root, plan.PlanID, ApplyOptions{DB: db}); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenOperations(ctx); len(open) != 0 {
		t.Fatalf("recovery left operation open: %#v", open)
	}
	if times, err := db.PruneDeletionTimes(ctx, time.Time{}); err != nil || len(times) != 1 {
		t.Fatalf("expected one recovered deletion record: times=%v err=%v", times, err)
	}
	if err := Recover(ctx, root, plan.PlanID, ApplyOptions{DB: db}); err != nil {
		t.Fatalf("second recovery should be idempotent: %v", err)
	}
}

func TestPruneGarbageCollectsOnlyObjectsWhoseReferencesArePruned(t *testing.T) {
	ctx := context.Background()
	root, db, all := fixture(t)
	store := materialize.Store{DeployRoot: root}
	orphaned, retained := []byte("orphaned after view prune"), []byte("still referenced")
	orphanedDigest, retainedDigest := pruneDigest(orphaned), pruneDigest(retained)
	if _, err := store.Put(ctx, orphanedDigest, int64(len(orphaned)), bytes.NewReader(orphaned)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, retainedDigest, int64(len(retained)), bytes.NewReader(retained)); err != nil {
		t.Fatal(err)
	}
	for _, realization := range all {
		runtimeDir := filepath.Join(realization.Path, layout.RuntimeDir)
		if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		digest := ""
		switch realization.RealizationID {
		case "rz_one":
			digest = orphanedDigest
		case "rz_four":
			digest = retainedDigest
		}
		metadata := []byte(`{"files":[]}`)
		if digest != "" {
			metadata = []byte(`{"files":[{"digest":"` + digest + `"}]}`)
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, "realization.json"), metadata, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := ObserveMaterialized(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Build(PlanInput{DeployRoot: root, Realizations: all, CurrentID: "rz_four", LastKnownGoodID: "rz_three", Keep: 2, MinimumRetention: 2, Materialized: objects})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.MaterializedCandidates) != 1 || plan.MaterializedCandidates[0].Digest != orphanedDigest {
		t.Fatalf("unexpected materialized GC plan: %#v", plan.MaterializedCandidates)
	}
	if err := Apply(ctx, plan, ApplyOptions{DB: db, MaxDeletionsPerHour: 10}); err != nil {
		t.Fatal(err)
	}
	orphanedPath, _ := store.Path(orphanedDigest)
	if _, err := os.Lstat(orphanedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned object remains: %v", err)
	}
	if err := store.Verify(retainedDigest, int64(len(retained))); err != nil {
		t.Fatalf("retained object was removed: %v", err)
	}
}
