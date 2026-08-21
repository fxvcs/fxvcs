package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JournalTable is the SQLite migration journal maintained by Run.
const JournalTable = "fxvcs_schema_migrations"

const createJournalSQL = `CREATE TABLE IF NOT EXISTS ` + JournalTable + ` (
  version INTEGER PRIMARY KEY,
  description TEXT NOT NULL,
  checksum TEXT NOT NULL,
  started_at TEXT NOT NULL,
  applied_at TEXT,
  in_progress INTEGER NOT NULL DEFAULT 1 CHECK (in_progress IN (0,1))
)`

// JournalEntry is one row of the migration journal.
type JournalEntry struct {
	Version     int
	Description string
	Checksum    string
	StartedAt   string
	AppliedAt   string // empty while in progress
	InProgress  bool
}

// connTx adapts a *sql.Conn running an explicit BEGIN IMMEDIATE to Tx.
type connTx struct {
	ctx  context.Context
	conn *sql.Conn
}

func (t connTx) Exec(q string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(t.ctx, q, args...)
}
func (t connTx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(t.ctx, q, args...)
}
func (t connTx) QueryRow(q string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(t.ctx, q, args...)
}

// Immediate runs fn inside BEGIN IMMEDIATE ... COMMIT on one connection, so the
// write lock is taken up front (single-writer discipline) and readers of the
// same process cannot interleave. A non-nil error rolls back.
func Immediate(ctx context.Context, db *sql.DB, fn func(tx Tx) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("migrate: begin immediate: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err = fn(connTx{ctx, conn}); err != nil { //nolint:gocritic // assigns the named result so the deferred ROLLBACK sees it
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("migrate: commit: %w", err)
	}
	return nil
}

// UserVersion reads PRAGMA user_version without writing.
func UserVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("migrate: read user_version: %w", err)
	}
	return v, nil
}

// Run executes plan against db. Each step:
//
//  1. asserts PRAGMA user_version == step.From (ErrVersionMismatch otherwise);
//  2. durably writes an in-progress journal row for step.To (own transaction);
//  3. in one IMMEDIATE transaction runs Apply, marks the row applied and sets
//     PRAGMA user_version = step.To.
//
// If Apply fails, transaction 3 rolls back and the in-progress row remains, so
// Verify reports ErrInterruptedMigration until Run is re-invoked with a plan
// starting at the same version; a stale in-progress row for exactly step.To
// with user_version == step.From is resumed, which makes re-running a
// crashed plan idempotent.
func Run(ctx context.Context, db *sql.DB, plan Plan, opts RunOptions) error {
	if plan.Empty() {
		return nil
	}
	if _, err := db.ExecContext(ctx, createJournalSQL); err != nil {
		return fmt.Errorf("migrate: create journal: %w", err)
	}
	for _, step := range plan.Steps {
		if err := runStep(ctx, db, step, opts); err != nil {
			return err
		}
	}
	return nil
}

func runStep(ctx context.Context, db *sql.DB, step Migration, opts RunOptions) error {
	stamp := opts.now().UTC().Format(time.RFC3339Nano)
	// 1+2: version check and in-progress marker, durably committed on their own.
	err := Immediate(ctx, db, func(tx Tx) error {
		var uv int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
			return err
		}
		if uv != step.From {
			return fmt.Errorf("%w: user_version %d, step expects %d", ErrVersionMismatch, uv, step.From)
		}
		var existingInProgress sql.NullInt64
		err := tx.QueryRow("SELECT in_progress FROM "+JournalTable+" WHERE version = ?", step.To).Scan(&existingInProgress)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.Exec("INSERT INTO "+JournalTable+" (version, description, checksum, started_at, in_progress) VALUES (?,?,?,?,1)",
				step.To, step.Description, step.Checksum(), stamp)
			return err
		case err != nil:
			return err
		case existingInProgress.Int64 == 1:
			// Resume: previous attempt rolled back before applying.
			_, err = tx.Exec("UPDATE "+JournalTable+" SET description=?, checksum=?, started_at=? WHERE version=?",
				step.Description, step.Checksum(), stamp, step.To)
			return err
		default:
			return fmt.Errorf("%w: journal says version %d applied but user_version is %d", ErrInterruptedMigration, step.To, uv)
		}
	})
	if err != nil {
		return err
	}
	// 3: apply + mark + bump, atomically.
	return Immediate(ctx, db, func(tx Tx) error {
		if err := step.Apply(tx); err != nil {
			return fmt.Errorf("migrate: step %d->%d (%s): %w", step.From, step.To, step.Description, err)
		}
		if _, err := tx.Exec("UPDATE "+JournalTable+" SET applied_at=?, in_progress=0 WHERE version=?", stamp, step.To); err != nil {
			return err
		}
		// PRAGMA does not accept bind parameters; the value is a trusted int.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", step.To)); err != nil {
			return err
		}
		return nil
	})
}

// Status is the reconciliation of PRAGMA user_version and the journal.
type Status struct {
	UserVersion    int
	JournalApplied int   // highest version with in_progress=0 (0 if none)
	InProgress     []int // versions still marked in progress
	Journal        []JournalEntry
	HasJournal     bool
}

// Resumable reports that the only discrepancy is a single in-progress marker
// for UserVersion+1, i.e. the crashed step rolled back cleanly and Run may
// simply be invoked again from UserVersion.
func (s Status) Resumable() bool {
	return len(s.InProgress) == 1 && s.InProgress[0] == s.UserVersion+1 && s.JournalApplied == s.UserVersion
}

// Verify reconciles user_version against the journal. It never writes.
// It returns ErrInterruptedMigration when a step is in progress or the highest
// applied journal version differs from user_version. A database whose journal
// table does not exist yet (fresh, user_version 0) verifies clean.
func Verify(ctx context.Context, db *sql.DB) (Status, error) {
	var st Status
	uv, err := UserVersion(db)
	if err != nil {
		return st, err
	}
	st.UserVersion = uv
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", JournalTable).Scan(&n); err != nil {
		return st, fmt.Errorf("migrate: inspect schema: %w", err)
	}
	st.HasJournal = n == 1
	if !st.HasJournal {
		if uv != 0 {
			return st, fmt.Errorf("%w: user_version %d but no migration journal", ErrInterruptedMigration, uv)
		}
		return st, nil
	}
	rows, err := db.QueryContext(ctx, "SELECT version, description, checksum, started_at, COALESCE(applied_at,''), in_progress FROM "+JournalTable+" ORDER BY version")
	if err != nil {
		return st, fmt.Errorf("migrate: read journal: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e JournalEntry
		var ip int
		if err := rows.Scan(&e.Version, &e.Description, &e.Checksum, &e.StartedAt, &e.AppliedAt, &ip); err != nil {
			return st, err
		}
		e.InProgress = ip == 1
		st.Journal = append(st.Journal, e)
		if e.InProgress {
			st.InProgress = append(st.InProgress, e.Version)
		} else if e.Version > st.JournalApplied {
			st.JournalApplied = e.Version
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if len(st.InProgress) > 0 {
		return st, fmt.Errorf("%w: steps in progress %v (user_version %d)", ErrInterruptedMigration, st.InProgress, uv)
	}
	if st.JournalApplied != uv {
		return st, fmt.Errorf("%w: journal applied %d, user_version %d", ErrInterruptedMigration, st.JournalApplied, uv)
	}
	return st, nil
}

// DatabaseKind selects the recovery policy: derived state is rebuilt from
// facts, authoritative state is restored from the pre-migration backup.
type DatabaseKind int

const (
	// Derived databases (per-worktree hydration state) are rebuilt from Git and
	// the working tree; deleting them loses no authority.
	Derived DatabaseKind = iota + 1
	// Authoritative databases (publication ledger with pending pins) must be
	// restored from <db>.bak-<from> or reconciled against remotes; deleting
	// them can lose pending-publication knowledge.
	Authoritative
)

// Action is what recovery should do.
type Action string

const (
	ActionNone    Action = "none"    // verified clean
	ActionResume  Action = "resume"  // re-run the plan from user_version
	ActionRebuild Action = "rebuild" // delete and rebuild from facts (Derived)
	ActionRestore Action = "restore" // restore <db>.bak-<from> (Authoritative)
)

// Guidance is the typed recovery recommendation surfaced by doctor/repair.
type Guidance struct {
	Action  Action
	Message string
}

// Recover maps a Verify outcome to guidance. It performs no I/O.
func Recover(kind DatabaseKind, st Status, verifyErr error) Guidance {
	switch {
	case verifyErr == nil:
		return Guidance{ActionNone, "database schema verified"}
	case st.Resumable():
		return Guidance{ActionResume, fmt.Sprintf("migration to version %d was interrupted before commit; re-run the migration plan from version %d", st.InProgress[0], st.UserVersion)}
	case kind == Derived:
		return Guidance{ActionRebuild, "derived state is inconsistent; delete the database and rebuild it from the Git index and working tree (fxvcs repair)"}
	default:
		return Guidance{ActionRestore, "authoritative state is inconsistent; restore the pre-migration backup (<db>.bak-<version>) or reconcile pending publication against every required remote before continuing"}
	}
}

// BackupPath is where MigrateFile keeps the pre-migration copy.
func BackupPath(dbPath string, fromVersion int) string {
	return fmt.Sprintf("%s.bak-%d", dbPath, fromVersion)
}

// StagingPath is the temporary copy that MigrateFile migrates and renames over
// the original.
func StagingPath(dbPath string) string { return dbPath + ".migrating" }
