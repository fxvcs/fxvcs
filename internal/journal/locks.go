// Package journal defines the lock hierarchy every FXVCS mutation follows
// (docs/spec/checkout-consistency.md §3) and an in-process ordering guard.
//
// The real cross-process locks (lock files under $GIT_DIR/fxvcs and the cache
// directory) wrap a Sequence so that any attempt to acquire out of order fails
// fast with ErrLockOrder instead of deadlocking. Locks are never held across an
// unbounded network request or a Git invocation.
package journal

import (
	"errors"
	"fmt"
	"strings"
)

// Level is a position in the lock order. Lower levels are acquired first.
type Level int

// The lock hierarchy, in acquisition order.
const (
	LockTarget           Level = iota + 1 // one per deployment target: Stage 4 recovery, activation, rollback, prune
	LockCheckoutWorktree                  // one per worktree: reconciliation, rebuild, upgrade of state.db
	LockPath                              // one per repository-relative path: hydrate/dehydrate/migrate CAS
	LockDigest                            // one per object digest: ingest, fetch into cache, verify
	LockCachePrune                        // whole cache: prune/eviction (excludes pinned digests)
	LockPublication                       // publication ledger: pending pins, publish, ledger migration
)

var names = map[Level]string{
	LockTarget:           "target",
	LockCheckoutWorktree: "checkout-worktree",
	LockPath:             "path",
	LockDigest:           "digest",
	LockCachePrune:       "cache-prune",
	LockPublication:      "publication",
}

// String returns the documented lock name.
func (l Level) String() string {
	if n, ok := names[l]; ok {
		return n
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Valid reports whether the level is part of the hierarchy.
func (l Level) Valid() bool { _, ok := names[l]; return ok }

// Order returns the hierarchy from first-acquired to last-acquired.
func Order() []Level {
	return []Level{LockTarget, LockCheckoutWorktree, LockPath, LockDigest, LockCachePrune, LockPublication}
}

// ErrLockOrder is returned when a Sequence would acquire a lock at a level
// lower than one it already holds, or a same-level lock whose key sorts
// before one already held (same-level locks are taken in ascending key order).
var ErrLockOrder = errors.New("journal: lock acquisition violates lock order")

// ErrLockUnknown is returned for a level outside the hierarchy.
var ErrLockUnknown = errors.New("journal: unknown lock level")

// ErrLockHeld is returned when the same lock is acquired twice.
var ErrLockHeld = errors.New("journal: lock already held")

// Held is one acquired lock.
type Held struct {
	Level Level
	Key   string // path, digest, or "" for singleton levels
}

func (h Held) String() string {
	if h.Key == "" {
		return h.Level.String()
	}
	return h.Level.String() + ":" + h.Key
}

// Sequence tracks the locks one operation holds and enforces the order. It is
// not safe for concurrent use; one Sequence per operation.
type Sequence struct {
	held []Held
}

// Acquire records taking the lock, or refuses with ErrLockOrder / ErrLockHeld.
func (s *Sequence) Acquire(level Level, key string) error {
	if !level.Valid() {
		return fmt.Errorf("%w: %d", ErrLockUnknown, level)
	}
	for _, h := range s.held {
		switch {
		case h.Level == level && h.Key == key:
			return fmt.Errorf("%w: %s", ErrLockHeld, h)
		case h.Level > level:
			return fmt.Errorf("%w: cannot take %s while holding %s", ErrLockOrder, Held{level, key}, h)
		case h.Level == level && strings.Compare(key, h.Key) < 0:
			return fmt.Errorf("%w: same-level %s locks must be taken in ascending key order (%q after %q)", ErrLockOrder, level, key, h.Key)
		}
	}
	s.held = append(s.held, Held{level, key})
	return nil
}

// Release drops the lock; releasing something not held is a no-op.
func (s *Sequence) Release(level Level, key string) {
	for i, h := range s.held {
		if h.Level == level && h.Key == key {
			s.held = append(s.held[:i], s.held[i+1:]...)
			return
		}
	}
}

// Held returns the locks currently held, in acquisition order.
func (s *Sequence) Held() []Held { return append([]Held(nil), s.held...) }

// Holds reports whether the lock is held.
func (s *Sequence) Holds(level Level, key string) bool {
	for _, h := range s.held {
		if h.Level == level && h.Key == key {
			return true
		}
	}
	return false
}
