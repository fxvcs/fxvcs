// Package migrate is the ordered migration model shared by every versioned
// FXVCS format that is upgraded in place (docs/spec/version-negotiation.md).
//
// Two families exist:
//
//   - SQLite databases (Migration / Registry / Plan / Run). Every step runs in
//     its own IMMEDIATE transaction, is journaled in fxvcs_schema_migrations
//     with an in-progress marker, and advances PRAGMA user_version in the same
//     transaction as its schema change, so an interruption at any boundary is
//     detectable and the database is always at a well-defined version.
//   - Committed YAML/JSON documents (DocMigration / DocRegistry / PlanDoc /
//     ApplyDoc). Document migrations are pure functions over the generic
//     document map; the caller writes the result as an ordinary reviewable
//     Git change.
//
// In both families the plan is computed and validated before anything is
// written: a version newer than the registry's target is refused with
// ErrNewerThanSupported without touching the file.
package migrate

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Errors. Callers must inspect with errors.Is.
var (
	// ErrNewerThanSupported is returned before any write when the current
	// version is later than the target this binary knows.
	ErrNewerThanSupported = errors.New("migrate: format version is newer than this binary supports")
	// ErrUnknownVersion is returned when the current version is not part of the
	// migration chain (negative, or below the chain start).
	ErrUnknownVersion = errors.New("migrate: unknown format version")
	// ErrGap is returned by NewRegistry when the migrations do not form one
	// contiguous chain, and by Plan when no step leads from current toward target.
	ErrGap = errors.New("migrate: gap in migration chain")
	// ErrInterruptedMigration is returned by Verify when the journal and
	// PRAGMA user_version disagree, or a step is still marked in progress.
	ErrInterruptedMigration = errors.New("migrate: interrupted migration detected")
	// ErrVersionMismatch is returned by Run when the database version moved
	// under a plan (another process migrated concurrently).
	ErrVersionMismatch = errors.New("migrate: database version does not match plan")
	// ErrBusy is returned by MigrateFile when the database has other live
	// connections (a write-ahead log is present) and cannot be swapped safely.
	ErrBusy = errors.New("migrate: database is in use by another connection")
)

// Tx is the subset of a transaction a Migration may use. *sql.Tx satisfies it,
// as does the connection-scoped transaction Run creates.
type Tx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Migration is one ordered SQLite schema step From -> To (To == From+1).
type Migration struct {
	From, To    int
	Description string
	Apply       func(tx Tx) error
}

// Checksum identifies a migration definition in the journal so a database can
// prove which definition produced its schema.
func (m Migration) Checksum() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d->%d:%s", m.From, m.To, m.Description))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Registry is a validated contiguous chain of migrations.
type Registry struct {
	steps []Migration // steps[i].From == start+i
	start int
}

// NewRegistry validates that ms form a contiguous chain (each To == From+1 and
// each next From == previous To) with non-nil Apply. Order in ms is
// significant and must be ascending.
func NewRegistry(ms ...Migration) (*Registry, error) {
	if len(ms) == 0 {
		return nil, fmt.Errorf("%w: empty registry", ErrGap)
	}
	for i, m := range ms {
		if m.To != m.From+1 {
			return nil, fmt.Errorf("%w: migration %d->%d must advance exactly one version", ErrGap, m.From, m.To)
		}
		if m.Apply == nil {
			return nil, fmt.Errorf("migrate: migration %d->%d has nil Apply", m.From, m.To)
		}
		if i > 0 && m.From != ms[i-1].To {
			return nil, fmt.Errorf("%w: expected migration from %d, got %d->%d", ErrGap, ms[i-1].To, m.From, m.To)
		}
	}
	return &Registry{steps: append([]Migration(nil), ms...), start: ms[0].From}, nil
}

// MustRegistry panics on an invalid chain; for package-level registries.
func MustRegistry(ms ...Migration) *Registry {
	r, err := NewRegistry(ms...)
	if err != nil {
		panic(err)
	}
	return r
}

// Latest is the version the whole chain reaches.
func (r *Registry) Latest() int { return r.steps[len(r.steps)-1].To }

// Oldest is the version the chain starts from (0 for "empty database").
func (r *Registry) Oldest() int { return r.start }

// Steps returns a copy of the chain.
func (r *Registry) Steps() []Migration { return append([]Migration(nil), r.steps...) }

// Plan is the ordered list of steps that carries a database From -> To.
type Plan struct {
	From, To int
	Steps    []Migration
}

// Empty reports whether nothing needs to run.
func (p Plan) Empty() bool { return len(p.Steps) == 0 }

// Plan computes the chain from current to target. It performs no I/O.
//
//   - current == target: empty plan
//   - current  > target: ErrNewerThanSupported (a newer binary wrote it)
//   - current  < Oldest(): ErrUnknownVersion
//   - target beyond Latest() or missing step: ErrGap
func (r *Registry) Plan(current, target int) (Plan, error) {
	p := Plan{From: current, To: target}
	switch {
	case current == target:
		return p, nil
	case current > target:
		return p, fmt.Errorf("%w: have %d, this binary supports up to %d", ErrNewerThanSupported, current, target)
	case current < r.start:
		return p, fmt.Errorf("%w: %d (chain starts at %d)", ErrUnknownVersion, current, r.start)
	case target > r.Latest():
		return p, fmt.Errorf("%w: target %d beyond latest %d", ErrGap, target, r.Latest())
	}
	for v := current; v < target; v++ {
		p.Steps = append(p.Steps, r.steps[v-r.start])
	}
	return p, nil
}

// RunOptions tune Run.
type RunOptions struct {
	// Now supplies journal timestamps; nil means time.Now. Fixtures pass a fixed
	// clock so golden dumps are deterministic.
	Now func() time.Time
}

func (o RunOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}
