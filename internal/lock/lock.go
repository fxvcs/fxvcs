// Package lock provides the durable cross-process locks that serialize CLI,
// SDK consumers, the Git filter, and (later) the agent. Correctness never
// depends on a daemon owning the operation: locks are OS advisory file locks
// (flock on Unix, LockFileEx on Windows) that the kernel releases when the
// holder dies, plus a small owner record for typed busy diagnostics.
//
// Lock order is enforced in-process by internal/journal.Sequence; this package
// only implements acquisition. Never hold a lock across an unbounded network
// request or a Git invocation that may itself start fxvcs (checkout, add).
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fxvcs.dev/fxvcs/internal/journal"
)

// Owner is the safe metadata written into the lock file for diagnostics.
type Owner struct {
	Operation string `json:"operation"`
	PID       int    `json:"pid"`
	Host      string `json:"host,omitempty"`
	StartedAt string `json:"startedAt"`
}

// ErrBusy is returned (wrapped in *BusyError) when a lock is held elsewhere.
var ErrBusy = errors.New("lock: busy")

// BusyError carries the current holder's metadata when it could be read.
type BusyError struct {
	Path  string
	Owner *Owner
	Level journal.Level
	Key   string
}

func (e *BusyError) Error() string {
	if e.Owner != nil {
		return fmt.Sprintf("lock: %s %q is held by %s (pid %d since %s)", e.Level, e.Key, e.Owner.Operation, e.Owner.PID, e.Owner.StartedAt)
	}
	return fmt.Sprintf("lock: %s %q is held by another process", e.Level, e.Key)
}

func (e *BusyError) Unwrap() error { return ErrBusy }

// Handle is a held lock.
type Handle struct {
	path  string
	f     *os.File
	level journal.Level
	key   string
	set   *Set
}

// Path returns the lock file path.
func (h *Handle) Path() string { return h.path }

// Release unlocks. It is safe to call twice.
func (h *Handle) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	err := unlock(h.f)
	cerr := h.f.Close()
	h.f = nil
	if h.set != nil {
		h.set.seq.Release(h.level, h.key)
	}
	if err != nil {
		return err
	}
	return cerr
}

// Set is a per-process group of locks that enforces the documented order.
type Set struct {
	// Dir holds lock files: <Dir>/<level>/<key>.lock
	Dir       string
	Operation string
	seq       journal.Sequence
	held      []*Handle
}

// NewSet creates a set rooted at dir for the named operation.
func NewSet(dir, operation string) *Set {
	return &Set{Dir: dir, Operation: operation}
}

// Acquire takes the lock at level/key, waiting up to timeout (0 = try once).
// It returns *BusyError when another process holds it and the timeout
// expires, and journal.ErrLockOrder when the caller violates lock order.
func (s *Set) Acquire(ctx context.Context, level journal.Level, key string, timeout time.Duration) (*Handle, error) {
	if err := s.seq.Acquire(level, key); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Dir, level.String())     // s.Dir is $GIT_DIR/fxvcs/locks; key is sanitized below
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G703: lock root is agent-controlled, key sanitized
		s.seq.Release(level, key)
		return nil, err
	}
	path := filepath.Join(dir, sanitize(key)+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // lock dir is under $GIT_DIR/fxvcs
	if err != nil {
		s.seq.Release(level, key)
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := tryLock(f)
		if err == nil {
			break
		}
		if !errors.Is(err, errWouldBlock) {
			_ = f.Close()
			s.seq.Release(level, key)
			return nil, err
		}
		if timeout == 0 || time.Now().After(deadline) || ctx.Err() != nil {
			owner := readOwner(path)
			_ = f.Close()
			s.seq.Release(level, key)
			return nil, &BusyError{Path: path, Owner: owner, Level: level, Key: key}
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	host, _ := os.Hostname()
	rec, _ := json.Marshal(Owner{Operation: s.Operation, PID: os.Getpid(), Host: host, StartedAt: time.Now().UTC().Format(time.RFC3339)})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(rec, 0)
	h := &Handle{path: path, f: f, level: level, key: key, set: s}
	s.held = append(s.held, h)
	return h, nil
}

// ReleaseAll releases every held lock in reverse acquisition order.
func (s *Set) ReleaseAll() {
	for i := len(s.held) - 1; i >= 0; i-- {
		_ = s.held[i].Release()
	}
	s.held = nil
}

func readOwner(path string) *Owner {
	b, err := os.ReadFile(path) //nolint:gosec // our own lock file
	if err != nil || len(b) == 0 {
		return nil
	}
	var o Owner
	if json.Unmarshal(b, &o) != nil {
		return nil
	}
	return &o
}

// sanitize maps a lock key (path, digest) to a safe filename.
func sanitize(key string) string {
	if key == "" {
		return "_"
	}
	out := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			out = append(out, c)
		default:
			out = append(out, '%')
			out = append(out, "0123456789abcdef"[c>>4], "0123456789abcdef"[c&15])
		}
	}
	if len(out) > 200 {
		// keep tail (most specific) plus a hash-ish prefix to stay unique enough
		out = out[len(out)-200:]
	}
	return string(out)
}

var errWouldBlock = errors.New("lock: would block")
