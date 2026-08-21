// Package state owns the clone-local and target-local SQLite databases
// (docs/spec/local-state.md and docs/stage-4-plan.md):
//
//   - $GIT_DIR/fxvcs/state.db — per-worktree hydration state: path rows,
//     stat-cache fingerprints and the compare-and-swap operation journal. It is
//     derived from Git and the working tree and is always rebuildable.
//   - $GIT_COMMON_DIR/fxvcs/publication.db — the ledger shared by linked
//     worktrees: locally ingested objects, their referrers, and per-remote
//     publication state. Pending pins are authoritative until published.
//   - <deployRoot>/state/agent.db — per-target reconciliation history,
//     readiness evidence and crash-recovery journals. The filesystem current
//     link remains activation authority; this database is its durable index.
//
// Both use modernc.org/sqlite through database/sql, WAL journaling, a busy
// timeout, foreign keys and a single connection whose writes start with
// BEGIN IMMEDIATE. Both are versioned with PRAGMA user_version and migrated
// through internal/migrate; a database written by a newer binary is refused
// before any write, and a corrupt file is reported with typed guidance
// (rebuild for state.db, restore-from-backup for publication.db).
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/migrate"
)

// Paths locates every clone-local FXVCS file. Callers resolve gitDir and
// gitCommonDir through Git (rev-parse --git-dir / --git-common-dir); this
// package never assumes ".git" is a directory.
type Paths struct {
	StateDB       string // $GIT_DIR/fxvcs/state.db
	PublicationDB string // $GIT_COMMON_DIR/fxvcs/publication.db
	TmpDir        string // $GIT_DIR/fxvcs/tmp/  (same filesystem as the worktree's git dir)
	ConfigYAML    string // $GIT_COMMON_DIR/fxvcs/config.yaml
}

// Locate computes the clone-local paths from the resolved Git directories.
func Locate(gitDir, gitCommonDir string) Paths {
	if gitCommonDir == "" {
		gitCommonDir = gitDir
	}
	return Paths{
		StateDB:       filepath.Join(gitDir, "fxvcs", "state.db"),
		PublicationDB: filepath.Join(gitCommonDir, "fxvcs", "publication.db"),
		TmpDir:        filepath.Join(gitDir, "fxvcs", "tmp"),
		ConfigYAML:    filepath.Join(gitCommonDir, "fxvcs", "config.yaml"),
	}
}

// Errors.
var (
	// ErrNotFound: the database does not exist and Create was false.
	ErrNotFound = errors.New("state: database does not exist")
	// ErrCorrupt: the file is not a readable SQLite database (truncated,
	// garbage, or failing integrity_check). Nothing was written.
	ErrCorrupt = errors.New("state: database is corrupt or not an fxvcs database")
	// ErrBusy: another process holds the database and did not let go within
	// the busy timeout. The file is fine; the caller should retry.
	//
	// It is a separate error because the recovery advice is opposite. A
	// database that cannot be read is not the same as a database someone else
	// is writing, and telling an operator to delete state.db — or to restore
	// publication.db from backup, which is what the authoritative one advises
	// — because a colleague's push held a lock for a moment would destroy
	// state to fix nothing.
	ErrBusy = errors.New("state: database is in use by another process")
	// ErrWrongDatabase: the file is a valid FXVCS database of the other kind
	// (state.db vs publication.db swapped).
	ErrWrongDatabase = errors.New("state: database kind mismatch")
	// ErrWrongTarget: an agent database belongs to a different target binding.
	ErrWrongTarget = errors.New("state: agent database target mismatch")
	// ErrNeedsMigration: the schema is older than this binary and Options.
	// Migrate was false.
	ErrNeedsMigration = errors.New("state: database schema requires migration")
	// ErrIntentInvalid: hydration intent outside the closed set.
	ErrIntentInvalid = errors.New("state: invalid hydration intent")
	// ErrOperationClosed: Commit/Abort on a finished operation handle.
	ErrOperationClosed = errors.New("state: operation already finished")
)

// Kind names of the two databases, stored under meta key "database".
const (
	KindHydrationState    = "hydration-state"
	KindPublicationLedger = "publication-ledger"
	KindAgentState        = "agent-state"
)

// Meta keys.
const (
	MetaDatabase         = "database"
	MetaSchema           = "schema"
	MetaRepositoryID     = "repository_id"
	MetaStorageDomainID  = "storage_domain_id"
	MetaTarget           = "target"
	MetaCreatedByVersion = "created_by_version"
	MetaCreatedAt        = "created_at"
)

// Options control Open / OpenLedger.
type Options struct {
	// Create makes a missing database (schema at the current version). When
	// false a missing file is ErrNotFound.
	Create bool
	// Migrate allows an older on-disk schema to be migrated in place on open.
	// When false an older schema is ErrNeedsMigration (plan-first callers such
	// as `fxvcs upgrade` use migrate.MigrateFile instead).
	Migrate bool
	// InstalledVersion is recorded in meta on create (created_by_version).
	InstalledVersion string
	// RepositoryID / StorageDomainID are recorded in meta on create.
	RepositoryID    string
	StorageDomainID string
	// Target is recorded by OpenAgent and prevents one target from reusing
	// another target's state database after a configuration mistake.
	Target string
	// BusyTimeout for SQLite locks; zero means 5s.
	BusyTimeout time.Duration
	// Now supplies timestamps (journals, meta); nil means time.Now.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) busy() time.Duration {
	if o.BusyTimeout <= 0 {
		return 5 * time.Second
	}
	return o.BusyTimeout
}

// Guidance is the typed recovery advice attached to open failures.
type Guidance = migrate.Guidance

// OpenError carries typed guidance next to the cause. errors.Is still matches
// the sentinel (ErrCorrupt, migrate.ErrNewerThanSupported, ...).
type OpenError struct {
	Path     string
	Kind     string // KindHydrationState | KindPublicationLedger
	Guidance Guidance
	Err      error
}

func (e *OpenError) Error() string {
	return fmt.Sprintf("%s (%s): %v; %s", filepath.Base(e.Path), e.Kind, e.Err, e.Guidance.Message)
}

// Unwrap exposes the cause.
func (e *OpenError) Unwrap() error { return e.Err }

// handle is the shared open/negotiate machinery.
type handle struct {
	db   *sql.DB
	path string
	kind string
	opts Options
}

func kindPolicy(kind string) migrate.DatabaseKind {
	if kind == KindHydrationState {
		return migrate.Derived
	}
	return migrate.Authoritative
}

// isBusy reports that an error is SQLite refusing to proceed because someone
// else holds the database, rather than anything wrong with it.
//
// The codes are matched by value rather than by message: 5 is SQLITE_BUSY and
// the extended codes above it (261 SQLITE_BUSY_RECOVERY, 517
// SQLITE_BUSY_SNAPSHOT, 773 SQLITE_BUSY_TIMEOUT) share its low byte. 6 is
// SQLITE_LOCKED, which is the same answer from a different layer.
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
		return true
	}
	return false
}

func busyGuidance() Guidance {
	return Guidance{Action: migrate.ActionNone, Message: "another fxvcs or git process is using it; wait for it to finish and try again"}
}

func recoverGuidance(kind string) Guidance {
	if kind == KindPublicationLedger {
		return Guidance{Action: migrate.ActionRestore, Message: "restore " + filepath.Base(kindFile(kind)) + " from its .bak-<version> copy or reconcile pending publication against every required remote; do not delete it"}
	}
	if kind == KindAgentState {
		return Guidance{Action: migrate.ActionRestore, Message: "restore agent.db from backup or reconcile it conservatively from current and realization metadata; do not guess an activation outcome"}
	}
	return Guidance{Action: migrate.ActionRebuild, Message: "delete " + filepath.Base(kindFile(kind)) + " and rebuild it from the Git index and working tree (fxvcs repair); no working file is touched"}
}

func kindFile(kind string) string {
	if kind == KindPublicationLedger {
		return "publication.db"
	}
	if kind == KindAgentState {
		return "agent.db"
	}
	return "state.db"
}

// open negotiates the version and opens the connection. Order matters:
// nothing is written before the file proves to be an FXVCS database at a
// supported version.
func open(ctx context.Context, path, kind string, reg *migrate.Registry, opts Options) (*handle, error) {
	fail := func(g Guidance, err error) (*handle, error) {
		return nil, &OpenError{Path: path, Kind: kind, Guidance: g, Err: err}
	}
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if !exists {
		if !opts.Create {
			return fail(Guidance{Action: migrate.ActionRebuild, Message: "create it with Options.Create or run fxvcs setup"}, fmt.Errorf("%w: %s", ErrNotFound, path))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open(migrate.DriverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	h := &handle{db: db, path: path, kind: kind, opts: opts}

	if exists {
		// The busy timeout has to be set before the probes, not with the rest
		// of the pragmas after them.
		//
		// The probes deliberately run before anything is written, so the
		// pragmas that do write (journal_mode) come later — but that left the
		// probing connection with SQLite's default timeout of zero. A
		// concurrent opener rebuilding the WAL index, or a writer mid-commit,
		// then failed the very first read instantly, and every probe failure
		// was classified as corruption. Setting a timeout writes nothing.
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", opts.busy().Milliseconds())); err != nil {
			db.Close()
			return nil, err
		}
		// Read-only probes first: a garbage or truncated file fails here with
		// no write and no side file.
		uv, err := migrate.UserVersion(db)
		if err != nil {
			db.Close()
			if isBusy(err) {
				return fail(busyGuidance(), fmt.Errorf("%w: read user_version: %v", ErrBusy, err))
			}
			return fail(recoverGuidance(kind), fmt.Errorf("%w: %v", ErrCorrupt, err))
		}
		var quick string
		if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quick); err != nil || quick != "ok" {
			db.Close()
			if isBusy(err) {
				return fail(busyGuidance(), fmt.Errorf("%w: quick_check: %v", ErrBusy, err))
			}
			return fail(recoverGuidance(kind), fmt.Errorf("%w: quick_check %q %v", ErrCorrupt, quick, err))
		}
		// Which database this is, before how new it is. The two schemas
		// version independently, so a file of the wrong kind can also read as
		// "newer than supported" — and telling someone to upgrade fxvcs when
		// the real answer is that they pointed at the other database sends
		// them somewhere there is nothing to find.
		if got, err := h.metaValue(ctx, MetaDatabase); err == nil && got != "" && got != kind {
			db.Close()
			return fail(Guidance{Action: migrate.ActionNone, Message: "the file at this location is a " + got + " database"}, fmt.Errorf("%w: found %s, want %s", ErrWrongDatabase, got, kind))
		}
		if kind == KindAgentState && opts.Target != "" {
			if got, err := h.metaValue(ctx, MetaTarget); err == nil && got != "" && got != opts.Target {
				db.Close()
				return fail(Guidance{Action: migrate.ActionNone, Message: "use the deployRoot assigned to target " + got}, fmt.Errorf("%w: found %s, want %s", ErrWrongTarget, got, opts.Target))
			}
		}
		if uv > reg.Latest() {
			db.Close()
			return fail(Guidance{Action: migrate.ActionNone, Message: "upgrade fxvcs; a newer binary wrote this database"},
				fmt.Errorf("%w: %w: user_version %d, supported %d", format.ErrUnsupportedSchemaDB, migrate.ErrNewerThanSupported, uv, reg.Latest()))
		}
		st, verr := migrate.Verify(ctx, db)
		if verr != nil {
			g := migrate.Recover(kindPolicy(kind), st, verr)
			if g.Action != migrate.ActionResume || !opts.Migrate {
				db.Close()
				return fail(g, verr)
			}
		}
		if uv < reg.Latest() && !opts.Migrate {
			db.Close()
			return fail(Guidance{Action: migrate.ActionNone, Message: "run fxvcs upgrade to migrate local state"}, fmt.Errorf("%w: user_version %d, current %d", ErrNeedsMigration, uv, reg.Latest()))
		}
	}
	if err := h.pragmas(ctx); err != nil {
		db.Close()
		return nil, err
	}
	uv, err := migrate.UserVersion(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	plan, err := reg.Plan(uv, reg.Latest())
	if err != nil {
		db.Close()
		return fail(migrate.Recover(kindPolicy(kind), migrate.Status{UserVersion: uv}, err), err)
	}
	if !plan.Empty() {
		if err := migrate.Run(ctx, db, plan, migrate.RunOptions{Now: opts.Now}); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := h.ensureMeta(ctx, reg.Latest()); err != nil {
		db.Close()
		return nil, err
	}
	return h, nil
}

func (h *handle) pragmas(ctx context.Context) error {
	for _, p := range []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", h.opts.busy().Milliseconds()),
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := h.db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("state: %s: %w", p, err)
		}
	}
	return nil
}

func (h *handle) metaValue(ctx context.Context, key string) (string, error) {
	var v string
	err := h.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil && strings.Contains(err.Error(), "no such table") {
		return "", nil
	}
	return v, err
}

func (h *handle) ensureMeta(ctx context.Context, schema int) error {
	return migrate.Immediate(ctx, h.db, func(tx migrate.Tx) error {
		set := func(k, v string) error {
			_, err := tx.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING", k, v)
			return err
		}
		if err := set(MetaDatabase, h.kind); err != nil {
			return err
		}
		if err := set(MetaSchema, fmt.Sprint(schema)); err != nil {
			return err
		}
		if err := set(MetaCreatedByVersion, h.opts.InstalledVersion); err != nil {
			return err
		}
		if err := set(MetaCreatedAt, h.opts.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if h.opts.RepositoryID != "" {
			if err := set(MetaRepositoryID, h.opts.RepositoryID); err != nil {
				return err
			}
		}
		if h.opts.StorageDomainID != "" {
			if err := set(MetaStorageDomainID, h.opts.StorageDomainID); err != nil {
				return err
			}
		}
		if h.opts.Target != "" {
			if err := set(MetaTarget, h.opts.Target); err != nil {
				return err
			}
		}
		// Keep the human-readable mirror in step with user_version.
		_, err := tx.Exec("UPDATE meta SET value = ? WHERE key = ?", fmt.Sprint(schema), MetaSchema)
		return err
	})
}

// Meta returns every meta key/value.
func (h *handle) Meta(ctx context.Context) (map[string]string, error) {
	rows, err := h.db.QueryContext(ctx, "SELECT key, value FROM meta ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SchemaVersion returns PRAGMA user_version.
func (h *handle) SchemaVersion() (int, error) { return migrate.UserVersion(h.db) }

// SQL exposes the underlying connection for fixture dumps and diagnostics.
func (h *handle) SQL() *sql.DB { return h.db }

// Close checkpoints and closes the database.
func (h *handle) Close() error {
	_, _ = h.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return h.db.Close()
}

func (h *handle) tx(ctx context.Context, fn func(tx migrate.Tx) error) error {
	return migrate.Immediate(ctx, h.db, fn)
}

func (h *handle) nowNS() int64 { return h.opts.now().UnixNano() }
