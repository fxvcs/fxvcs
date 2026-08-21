package journal

import (
	"errors"
	"testing"
)

func TestOrderIsHierarchy(t *testing.T) {
	o := Order()
	want := []string{"target", "checkout-worktree", "path", "digest", "cache-prune", "publication"}
	if len(o) != len(want) {
		t.Fatalf("order = %v", o)
	}
	for i, l := range o {
		if l.String() != want[i] || !l.Valid() {
			t.Fatalf("order[%d] = %s, want %s", i, l, want[i])
		}
		if i > 0 && o[i-1] >= l {
			t.Fatalf("levels not strictly ascending at %d", i)
		}
	}
	if Level(0).Valid() || Level(99).Valid() {
		t.Fatal("invalid levels reported valid")
	}
}

func TestSequenceEnforcesOrder(t *testing.T) {
	var s Sequence
	if err := s.Acquire(LockCheckoutWorktree, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(LockPath, "assets/a.ytd"); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(LockPath, "assets/b.ytd"); err != nil {
		t.Fatalf("ascending same-level: %v", err)
	}
	if err := s.Acquire(LockPath, "assets/0.ytd"); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("descending same-level should fail: %v", err)
	}
	if err := s.Acquire(LockPath, "assets/a.ytd"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("double acquire: %v", err)
	}
	if err := s.Acquire(LockPublication, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire(LockDigest, "sha256:ab"); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("lower level after higher must fail: %v", err)
	}
	if err := s.Acquire(Level(42), ""); !errors.Is(err, ErrLockUnknown) {
		t.Fatalf("unknown level: %v", err)
	}
	s.Release(LockPublication, "")
	if err := s.Acquire(LockDigest, "sha256:ab"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if !s.Holds(LockDigest, "sha256:ab") || s.Holds(LockPublication, "") {
		t.Fatalf("held = %v", s.Held())
	}
	if got := s.Held(); len(got) != 4 || got[0].Level != LockCheckoutWorktree {
		t.Fatalf("held = %v", got)
	}
	// Fresh sequence: taking a high level first then anything lower is refused.
	var s2 Sequence
	_ = s2.Acquire(LockCachePrune, "")
	if err := s2.Acquire(LockCheckoutWorktree, ""); !errors.Is(err, ErrLockOrder) {
		t.Fatalf("worktree after prune: %v", err)
	}
}
