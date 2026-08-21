// Package activation owns observation and atomic replacement of a target's
// current realization indirection plus conservative journal recovery.
package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/state"
)

var (
	ErrNoCurrent        = errors.New("activation: no current realization")
	ErrAmbiguousCurrent = errors.New("activation: current realization is ambiguous")
	ErrCompareFailed    = errors.New("activation: current realization changed")
)

// AcquireTargetLock takes the cross-process lock covering recovery, final
// preflight, activation, health state, rollback, and prune application.
func AcquireTargetLock(ctx context.Context, deployRoot, operation string, timeout time.Duration) (*lock.Handle, error) {
	set := lock.NewSet(filepath.Join(deployRoot, layout.StateDir, "locks"), operation)
	return set.Acquire(ctx, journal.LockTarget, "current", timeout)
}

// ObserveCurrent validates that current is exactly a relative symlink to one
// releases/<realizationID> entry. Absolute, escaping, nested, and non-symlink
// forms are held as ambiguous rather than normalized or guessed.
func ObserveCurrent(deployRoot string) (string, error) {
	current := layout.Current(deployRoot)
	info, err := os.Lstat(current)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCurrent
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%w: current is not a symlink", ErrAmbiguousCurrent)
	}
	target, err := os.Readlink(current)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) || filepath.Clean(target) != target {
		return "", fmt.Errorf("%w: invalid target %q", ErrAmbiguousCurrent, target)
	}
	parts := strings.Split(filepath.ToSlash(target), "/")
	if len(parts) != 2 || parts[0] != layout.ReleasesDir {
		return "", fmt.Errorf("%w: invalid target %q", ErrAmbiguousCurrent, target)
	}
	if want, err := layout.RealizationTarget(parts[1]); err != nil || filepath.Clean(want) != filepath.Clean(target) {
		return "", fmt.Errorf("%w: invalid realization target %q", ErrAmbiguousCurrent, target)
	}
	return parts[1], nil
}

// Switch atomically compare-and-swaps current. The target lock must be held by
// the caller; expected may be empty only when current is absent.
func Switch(deployRoot, expected, next string) error {
	nextDir, err := layout.Realization(deployRoot, next)
	if err != nil {
		return err
	}
	info, err := os.Stat(nextDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("activation: realization %s is not a directory: %w", next, err)
	}
	observed, observeErr := ObserveCurrent(deployRoot)
	if expected == "" {
		if !errors.Is(observeErr, ErrNoCurrent) {
			if observeErr != nil {
				return observeErr
			}
			return fmt.Errorf("%w: found %s, expected none", ErrCompareFailed, observed)
		}
	} else if observeErr != nil || observed != expected {
		if observeErr != nil {
			return observeErr
		}
		return fmt.Errorf("%w: found %s, expected %s", ErrCompareFailed, observed, expected)
	}
	target, _ := layout.RealizationTarget(next)
	tmp, err := os.CreateTemp(deployRoot, ".current-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	defer os.Remove(tmpPath)
	if err := os.Symlink(target, tmpPath); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, layout.Current(deployRoot)); err != nil {
		return err
	}
	dir, err := os.Open(deployRoot)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type journalPayload struct {
	Expected string `json:"expected"`
	Next     string `json:"next"`
}

// Activate journals the intended CAS before switching and closes the record
// only after the filesystem proves next is selected.
func Activate(ctx context.Context, db *state.AgentDB, operationID, deployRoot, expected, next string) error {
	payload, _ := json.Marshal(journalPayload{Expected: expected, Next: next})
	if err := db.BeginOperation(ctx, state.OperationRecord{OperationID: operationID, Operation: "activate", RealizationID: next, Phase: "prepared", Payload: payload}); err != nil {
		return err
	}
	if err := Switch(deployRoot, expected, next); err != nil {
		return err
	}
	if err := db.AdvanceOperation(ctx, operationID, "switched", payload, ""); err != nil {
		return err
	}
	observed, err := ObserveCurrent(deployRoot)
	if err != nil || observed != next {
		return fmt.Errorf("%w: after switch found %q: %v", ErrAmbiguousCurrent, observed, err)
	}
	return db.AdvanceOperation(ctx, operationID, "committed", payload, "committed")
}

// RecoverActivation resolves an open activation journal solely from the exact
// expected/next tuple and current filesystem fact. Any third state is left open
// and returned as ambiguity for operator diagnosis.
func RecoverActivation(ctx context.Context, db *state.AgentDB, rec state.OperationRecord, deployRoot string) error {
	if rec.Operation != "activate" || rec.Outcome != "" {
		return errors.New("activation: record is not an open activation")
	}
	var payload journalPayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil || payload.Next == "" {
		return fmt.Errorf("%w: invalid journal payload", ErrAmbiguousCurrent)
	}
	observed, err := ObserveCurrent(deployRoot)
	if err != nil {
		if errors.Is(err, ErrNoCurrent) && payload.Expected == "" {
			return db.AdvanceOperation(ctx, rec.OperationID, "not-switched", rec.Payload, "aborted")
		}
		return err
	}
	switch observed {
	case payload.Next:
		return db.AdvanceOperation(ctx, rec.OperationID, "committed", rec.Payload, "recovered-committed")
	case payload.Expected:
		return db.AdvanceOperation(ctx, rec.OperationID, "not-switched", rec.Payload, "aborted")
	default:
		return fmt.Errorf("%w: current=%s expected=%s next=%s", ErrAmbiguousCurrent, observed, payload.Expected, payload.Next)
	}
}
