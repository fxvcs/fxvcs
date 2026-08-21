package activation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/state"
)

func makeRealization(t *testing.T, root, id string) {
	t.Helper()
	path, err := layout.Realization(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func openAgentDB(t *testing.T, root string) *state.AgentDB {
	t.Helper()
	db, err := state.OpenAgent(context.Background(), filepath.Join(root, "state", "agent.db"), state.Options{Create: true, Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestActivateCompareAndSwitch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := openAgentDB(t, root)
	makeRealization(t, root, "rz_one")
	makeRealization(t, root, "rz_two")
	if err := Activate(ctx, db, "op-one", root, "", "rz_one"); err != nil {
		t.Fatal(err)
	}
	if current, err := ObserveCurrent(root); err != nil || current != "rz_one" {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if err := Activate(ctx, db, "op-two", root, "rz_one", "rz_two"); err != nil {
		t.Fatal(err)
	}
	if current, err := ObserveCurrent(root); err != nil || current != "rz_two" {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if err := Switch(root, "rz_one", "rz_one"); !errors.Is(err, ErrCompareFailed) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	if open, err := db.OpenOperations(ctx); err != nil || len(open) != 0 {
		t.Fatalf("operations left open: %#v %v", open, err)
	}
}

func TestRecoverActivationFromFilesystemFact(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := openAgentDB(t, root)
	makeRealization(t, root, "rz_old")
	makeRealization(t, root, "rz_new")
	if err := Switch(root, "", "rz_old"); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(journalPayload{Expected: "rz_old", Next: "rz_new"})
	rec := state.OperationRecord{OperationID: "recover", Operation: "activate", RealizationID: "rz_new", Phase: "prepared", Payload: payload}
	if err := db.BeginOperation(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// Crash before the switch: current still names expected, so recovery aborts.
	open, _ := db.OpenOperations(ctx)
	if err := RecoverActivation(ctx, db, open[0], root); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenOperations(ctx); len(open) != 0 {
		t.Fatalf("aborted recovery remains open: %#v", open)
	}

	if err := db.BeginOperation(ctx, state.OperationRecord{OperationID: "recover-switched", Operation: "activate", RealizationID: "rz_new", Phase: "prepared", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := Switch(root, "rz_old", "rz_new"); err != nil {
		t.Fatal(err)
	}
	open, _ = db.OpenOperations(ctx)
	if err := RecoverActivation(ctx, db, open[0], root); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.OpenOperations(ctx); len(open) != 0 {
		t.Fatalf("committed recovery remains open: %#v", open)
	}
}

func TestObserveCurrentRejectsAmbiguousForms(t *testing.T) {
	root := t.TempDir()
	makeRealization(t, root, "rz_one")
	if err := os.Symlink(filepath.Join(root, "releases", "rz_one"), layout.Current(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveCurrent(root); !errors.Is(err, ErrAmbiguousCurrent) {
		t.Fatalf("absolute current accepted: %v", err)
	}
}
