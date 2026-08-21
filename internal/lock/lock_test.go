package lock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/journal"
)

func TestAcquireReleaseAndOrder(t *testing.T) {
	dir := t.TempDir()
	s := NewSet(dir, "test")
	ctx := context.Background()
	h1, err := s.Acquire(ctx, journal.LockCheckoutWorktree, "wt", 0)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.Acquire(ctx, journal.LockPath, "a/b.ytd", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Out-of-order acquisition is refused in-process.
	if _, err := s.Acquire(ctx, journal.LockCheckoutWorktree, "other", 0); !errors.Is(err, journal.ErrLockOrder) {
		t.Fatalf("expected lock order error, got %v", err)
	}
	if err := h2.Release(); err != nil {
		t.Fatal(err)
	}
	if err := h1.Release(); err != nil {
		t.Fatal(err)
	}
	if err := h1.Release(); err != nil {
		t.Fatal("double release should be safe")
	}
	if _, err := os.Stat(filepath.Join(dir, "checkout-worktree", "wt.lock")); err != nil {
		t.Fatal(err)
	}
}

// TestCrossProcessBusy proves another process holding the lock yields a typed
// BusyError with the owner record, and that the kernel releases it on exit.
func TestCrossProcessBusy(t *testing.T) {
	if os.Getenv("FXVCS_LOCK_HELPER") == "1" {
		s := NewSet(os.Getenv("FXVCS_LOCK_DIR"), "helper-op")
		h, err := s.Acquire(context.Background(), journal.LockPublication, "ledger", 0)
		if err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(filepath.Join(os.Getenv("FXVCS_LOCK_DIR"), "ready"), nil, 0o644)
		time.Sleep(1500 * time.Millisecond)
		_ = h.Release()
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrossProcessBusy")
	cmd.Env = append(os.Environ(), "FXVCS_LOCK_HELPER=1", "FXVCS_LOCK_DIR="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never signalled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := NewSet(dir, "main-op")
	_, err := s.Acquire(context.Background(), journal.LockPublication, "ledger", 0)
	var be *BusyError
	if !errors.As(err, &be) || !errors.Is(err, ErrBusy) {
		t.Fatalf("expected BusyError, got %v", err)
	}
	if be.Owner == nil || be.Owner.Operation != "helper-op" || be.Owner.PID == 0 {
		t.Fatalf("owner metadata missing: %+v", be.Owner)
	}
	// Waiting acquisition succeeds once the helper releases/exits.
	h, err := s.Acquire(context.Background(), journal.LockPublication, "ledger", 5*time.Second)
	if err != nil {
		t.Fatalf("waiting acquire failed: %v", err)
	}
	_ = h.Release()
}

func TestSanitize(t *testing.T) {
	if sanitize("resources/[maps]/x.ytd") != "resources%2f%5bmaps%5d%2fx.ytd" {
		t.Fatal(sanitize("resources/[maps]/x.ytd"))
	}
	if sanitize("") != "_" {
		t.Fatal("empty key")
	}
}
