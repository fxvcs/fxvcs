package state

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"fxvcs.dev/fxvcs/internal/migrate"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Intent is the per-path hydration choice recorded by the user.
type Intent string

const (
	IntentUnhydrated Intent = "unhydrated"
	IntentHydrated   Intent = "hydrated"
)

func (i Intent) valid() bool { return i == IntentUnhydrated || i == IntentHydrated }

// Condition is the checkout consistency state derived for a path
// (docs/spec/checkout-consistency.md §1). Values match sdk.AssetState.
type Condition string

const (
	UnhydratedClean  Condition = "unhydrated-clean"
	HydratedClean    Condition = "hydrated-clean"
	HydratedModified Condition = "hydrated-modified"
	Conflict         Condition = "conflict"
	Missing          Condition = "missing"
)

// PathRow is one row of the paths table.
type PathRow struct {
	Path               string
	Intent             Intent
	IndexPointerDigest string // OID from the index pointer (sha256:...)
	WorkingDigest      string // digest of the working file's clean result ('' unknown)
	Fingerprint        Fingerprint
	LastVerifiedNS     int64
	Generation         int64
}

// Condition derives the consistency state of the row from what was recorded.
func (r PathRow) Condition() Condition {
	switch {
	case r.IndexPointerDigest == "":
		return Conflict
	case r.Fingerprint.IsZero() && r.WorkingDigest == "":
		return Missing
	case r.Intent == IntentUnhydrated && r.WorkingDigest == r.IndexPointerDigest:
		return UnhydratedClean
	case r.Intent == IntentHydrated && r.WorkingDigest == r.IndexPointerDigest:
		return HydratedClean
	case r.Intent == IntentHydrated && r.WorkingDigest != "":
		return HydratedModified
	}
	return Conflict
}

// DB is an open $GIT_DIR/fxvcs/state.db.
type DB struct{ *handle }

// Open opens (and with Options.Create creates) the per-worktree hydration
// state database. Version negotiation happens before any write; see package
// documentation for the failure classes.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	h, err := open(ctx, path, KindHydrationState, HydrationMigrations(), opts)
	if err != nil {
		return nil, err
	}
	return &DB{h}, nil
}

const pathCols = "path, intent, index_pointer_digest, working_digest, size, mtime_ns, ctime_ns, file_id, last_verified_ns, generation"

func scanPath(sc interface{ Scan(...any) error }) (PathRow, error) {
	var r PathRow
	err := sc.Scan(&r.Path, &r.Intent, &r.IndexPointerDigest, &r.WorkingDigest, &r.Fingerprint.Size, &r.Fingerprint.MtimeNS, &r.Fingerprint.CtimeNS, &r.Fingerprint.FileID, &r.LastVerifiedNS, &r.Generation)
	return r, err
}

// Get returns the row for a repository-relative path.
func (d *DB) Get(ctx context.Context, path string) (PathRow, bool, error) {
	p, err := pathsafe.Normalize(path)
	if err != nil {
		return PathRow{}, false, err
	}
	r, err := scanPath(d.db.QueryRowContext(ctx, "SELECT "+pathCols+" FROM paths WHERE path = ?", p))
	if errors.Is(err, sql.ErrNoRows) {
		return PathRow{}, false, nil
	}
	return r, err == nil, err
}

// Upsert inserts or replaces a row. The path is normalized; the intent must be
// one of the closed set.
func (d *DB) Upsert(ctx context.Context, row PathRow) error {
	p, err := pathsafe.Normalize(row.Path)
	if err != nil {
		return err
	}
	if !row.Intent.valid() {
		return fmt.Errorf("%w: %q", ErrIntentInvalid, row.Intent)
	}
	return d.tx(ctx, func(tx migrate.Tx) error { return upsertPath(tx, p, row) })
}

func upsertPath(tx migrate.Tx, p string, row PathRow) error {
	_, err := tx.Exec(`INSERT INTO paths (`+pathCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET intent=excluded.intent, index_pointer_digest=excluded.index_pointer_digest,
		working_digest=excluded.working_digest, size=excluded.size, mtime_ns=excluded.mtime_ns, ctime_ns=excluded.ctime_ns, file_id=excluded.file_id,
		last_verified_ns=excluded.last_verified_ns, generation=excluded.generation`,
		p, string(row.Intent), row.IndexPointerDigest, row.WorkingDigest, row.Fingerprint.Size, row.Fingerprint.MtimeNS, row.Fingerprint.CtimeNS, row.Fingerprint.FileID, row.LastVerifiedNS, row.Generation)
	return err
}

// Delete removes a row (no error when absent).
func (d *DB) Delete(ctx context.Context, path string) error {
	p, err := pathsafe.Normalize(path)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec("DELETE FROM paths WHERE path = ?", p)
		return err
	})
}

// List returns every row ordered by path.
func (d *DB) List(ctx context.Context) ([]PathRow, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+pathCols+" FROM paths ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PathRow
	for rows.Next() {
		r, err := scanPath(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListPaths returns the rows for the named paths, skipping those with no row.
//
// It exists so that a listing scoped to part of a repository costs what it
// selected rather than what the repository contains: List reads every recorded
// path, which on a repository with a hundred thousand assets is work done to
// answer a question about one directory. Paths are queried in batches because
// SQLite bounds how many parameters one statement may carry.
func (d *DB) ListPaths(ctx context.Context, paths []string) ([]PathRow, error) {
	// Comfortably under SQLITE_MAX_VARIABLE_NUMBER, which is 999 on older
	// builds and 32766 on current ones.
	const batch = 500
	out := make([]PathRow, 0, len(paths))
	norm := make([]string, 0, len(paths))
	for _, p := range paths {
		n, err := pathsafe.Normalize(p)
		if err != nil {
			return nil, err
		}
		norm = append(norm, n)
	}
	for start := 0; start < len(norm); start += batch {
		end := min(start+batch, len(norm))
		chunk := norm[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, p := range chunk {
			args[i] = p
			holders[i] = "?"
		}
		// The only thing concatenated into the statement is a run of "?"
		// separated by commas, generated here from the batch length. Every
		// path is a bound parameter.
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every path is a bound parameter
		query := "SELECT " + pathCols + " FROM paths WHERE path IN (" + strings.Join(holders, ",") + ") ORDER BY path"
		rows, err := d.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				r, err := scanPath(rows)
				if err != nil {
					return err
				}
				out = append(out, r)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ---- operation journal -----------------------------------------------------

// Operation names journaled by compare-and-swap mutations.
type Operation string

const (
	OpHydrate   Operation = "hydrate"
	OpDehydrate Operation = "dehydrate"
	OpMigrate   Operation = "migrate" // asset blob -> pointer conversion
	OpRepair    Operation = "repair"
	OpDiscard   Operation = "discard" // working changes thrown away deliberately
)

// Phase is the journal phase of an operation (docs/spec/checkout-consistency.md §4).
type Phase string

const (
	PhaseStarted   Phase = "started"   // expectations recorded, lock held
	PhaseFetched   Phase = "fetched"   // bytes in a temporary object under tmp/
	PhaseVerified  Phase = "verified"  // digests verified
	PhasePinned    Phase = "pinned"    // pending publication durably recorded
	PhaseCommitted Phase = "committed" // destination replaced, row updated
	PhaseAborted   Phase = "aborted"   // nothing replaced
)

// Terminal reports whether the phase closes the operation.
func (p Phase) Terminal() bool { return p == PhaseCommitted || p == PhaseAborted }

// Expectation is what step 1 of the CAS sequence recorded.
type Expectation struct {
	IndexOID    string
	Fingerprint Fingerprint
}

// JournalRow is one operation journal entry.
type JournalRow struct {
	OpID       string
	Generation int64
	Operation  Operation
	Path       string
	Phase      Phase
	Expected   Expectation
	StartedNS  int64
	FinishedNS int64
	Outcome    string
}

// Op is a live operation handle. Exactly one of Commit or Abort must be
// called; a process crash leaves the last recorded phase for reconciliation.
type Op struct {
	db   *DB
	row  JournalRow
	done bool
}

// Row returns the current journal row.
func (o *Op) Row() JournalRow { return o.row }

func newOpID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// BeginOperation records phase 'started' with the expectations from CAS step 1
// and bumps the per-worktree operation generation.
func (d *DB) BeginOperation(ctx context.Context, op Operation, path string, exp Expectation) (*Op, error) {
	p, err := pathsafe.Normalize(path)
	if err != nil {
		return nil, err
	}
	row := JournalRow{OpID: newOpID(), Operation: op, Path: p, Phase: PhaseStarted, Expected: exp, StartedNS: d.nowNS()}
	err = d.tx(ctx, func(tx migrate.Tx) error {
		if err := tx.QueryRow("SELECT COALESCE(MAX(generation),0)+1 FROM journal").Scan(&row.Generation); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO journal (op_id, generation, operation, path, phase, expected_index_oid, expected_fingerprint, started_ns)
			VALUES (?,?,?,?,?,?,?,?)`, row.OpID, row.Generation, string(op), p, string(PhaseStarted), exp.IndexOID, exp.Fingerprint.String(), row.StartedNS)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Op{db: d, row: row}, nil
}

// Advance records a non-terminal phase (fetched, verified, pinned).
func (o *Op) Advance(ctx context.Context, phase Phase) error {
	if o.done {
		return ErrOperationClosed
	}
	if phase.Terminal() || phase == PhaseStarted {
		return fmt.Errorf("state: Advance to %q is not allowed; use Commit/Abort", phase)
	}
	return o.setPhase(ctx, phase, "", 0)
}

// Commit records 'committed' together with the final row for the path, in one
// transaction, after the caller has atomically replaced the destination.
func (o *Op) Commit(ctx context.Context, final PathRow) error {
	if o.done {
		return ErrOperationClosed
	}
	p, err := pathsafe.Normalize(final.Path)
	if err != nil {
		return err
	}
	if p != o.row.Path {
		return fmt.Errorf("state: commit path %q differs from operation path %q", p, o.row.Path)
	}
	if !final.Intent.valid() {
		return fmt.Errorf("%w: %q", ErrIntentInvalid, final.Intent)
	}
	final.Generation = o.row.Generation
	now := o.db.nowNS()
	err = o.db.tx(ctx, func(tx migrate.Tx) error {
		if err := upsertPath(tx, p, final); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE journal SET phase=?, finished_ns=?, outcome=? WHERE op_id=?", string(PhaseCommitted), now, "ok", o.row.OpID)
		return err
	})
	if err != nil {
		return err
	}
	o.row.Phase, o.row.FinishedNS, o.row.Outcome, o.done = PhaseCommitted, now, "ok", true
	return nil
}

// Abort records 'aborted' with a reason. The path row is untouched.
func (o *Op) Abort(ctx context.Context, reason string) error {
	if o.done {
		return ErrOperationClosed
	}
	if err := o.setPhase(ctx, PhaseAborted, reason, o.db.nowNS()); err != nil {
		return err
	}
	o.done = true
	return nil
}

func (o *Op) setPhase(ctx context.Context, phase Phase, outcome string, finished int64) error {
	err := o.db.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec("UPDATE journal SET phase=?, outcome=?, finished_ns=? WHERE op_id=?", string(phase), outcome, finished, o.row.OpID)
		return err
	})
	if err != nil {
		return err
	}
	o.row.Phase, o.row.Outcome, o.row.FinishedNS = phase, outcome, finished
	return nil
}

const journalCols = "op_id, generation, operation, path, phase, expected_index_oid, expected_fingerprint, started_ns, finished_ns, outcome"

func scanJournal(sc interface{ Scan(...any) error }) (JournalRow, error) {
	var r JournalRow
	var fp string
	if err := sc.Scan(&r.OpID, &r.Generation, &r.Operation, &r.Path, &r.Phase, &r.Expected.IndexOID, &fp, &r.StartedNS, &r.FinishedNS, &r.Outcome); err != nil {
		return r, err
	}
	if fp != "" {
		f, err := ParseFingerprint(fp)
		if err != nil {
			return r, err
		}
		r.Expected.Fingerprint = f
	}
	return r, nil
}

// ListInterrupted returns operations whose last phase is not terminal — the
// input to startup reconciliation.
func (d *DB) ListInterrupted(ctx context.Context) ([]JournalRow, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+journalCols+" FROM journal WHERE phase NOT IN ('committed','aborted') ORDER BY generation")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalRow
	for rows.Next() {
		r, err := scanJournal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveInterrupted marks an interrupted operation aborted with the given
// reconciliation decision as outcome. It never touches the working file.
func (d *DB) ResolveInterrupted(ctx context.Context, opID string, decision Decision) error {
	return d.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec("UPDATE journal SET phase=?, finished_ns=?, outcome=? WHERE op_id=? AND phase NOT IN ('committed','aborted')",
			string(PhaseAborted), d.nowNS(), "reconciled:"+string(decision), opID)
		return err
	})
}

// ---- reconciliation --------------------------------------------------------

// Decision is the outcome of reconciling an interrupted operation.
type Decision string

const (
	// SafeRebuild: index and working file match the recorded expectations;
	// the operation left no visible change (temporary objects can be dropped)
	// or its derived state can be rebuilt from facts.
	SafeRebuild Decision = "safe-rebuild"
	// KeepDirty: the working file changed since the operation started (or
	// exists where none was expected). It is never overwritten; report it.
	KeepDirty Decision = "keep-dirty"
	// IndexMoved: the index pointer changed under the operation. Its premise
	// is gone; abort it and re-derive from the current index. The working file
	// is left alone.
	IndexMoved Decision = "index-moved"
)

// Reconcile decides what recovery may do with an interrupted operation given
// the current index OID and working-file fingerprint. It is a pure function:
// the fingerprint check wins, because a dirty working file is never
// overwritten regardless of what the index did.
func Reconcile(row JournalRow, currentIndexOID string, currentFingerprint Fingerprint) Decision {
	if row.Phase == PhaseCommitted {
		// A committed row is only interrupted if the path row update was lost;
		// treat like a fingerprint check.
		if currentFingerprint != row.Expected.Fingerprint {
			return KeepDirty
		}
		return SafeRebuild
	}
	if currentFingerprint != row.Expected.Fingerprint {
		return KeepDirty
	}
	if currentIndexOID != row.Expected.IndexOID {
		return IndexMoved
	}
	return SafeRebuild
}

// ---- rebuild ---------------------------------------------------------------

// Fact is one observation about a path, gathered read-only from Git and the
// working tree by the caller. Rebuild derives rows from facts only; it never
// opens a working file for writing.
type Fact struct {
	Path            string
	IndexPointerOID string // OID recorded in the index pointer ('' = not attributed / no pointer)
	// Working describes the working file; Present=false means absent.
	Working WorkingFact
}

// WorkingFact is the read-only observation of a working file.
type WorkingFact struct {
	Present     bool
	IsPointer   bool   // bytes decode as a pointer
	Digest      string // pointer OID when IsPointer, otherwise sha256 of the bytes
	Fingerprint Fingerprint
}

// RebuildReport summarizes a Rebuild.
type RebuildReport struct {
	Rows      int
	ByState   map[Condition]int
	Modified  []string // hydrated-modified paths (dirty; reported, untouched)
	Conflicts []string // conflict/missing paths (reported, untouched)
}

// Rebuild replaces every derived row from facts in one transaction. Existing
// hydration intent is preserved for paths whose intent cannot be inferred
// (absent working file); otherwise intent follows the observed working file.
// Interrupted journal rows are left for Reconcile.
func (d *DB) Rebuild(ctx context.Context, facts func(yield func(Fact) bool)) (RebuildReport, error) {
	rep := RebuildReport{ByState: map[Condition]int{}}
	now := d.nowNS()
	err := d.tx(ctx, func(tx migrate.Tx) error {
		prev := map[string]Intent{}
		rows, err := tx.Query("SELECT path, intent FROM paths")
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			var in Intent
			if err := rows.Scan(&p, &in); err != nil {
				rows.Close()
				return err
			}
			prev[p] = in
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM paths"); err != nil {
			return err
		}
		var gen int64
		if err := tx.QueryRow("SELECT COALESCE(MAX(generation),0) FROM journal").Scan(&gen); err != nil {
			return err
		}
		var ferr error
		facts(func(f Fact) bool {
			p, err := pathsafe.Normalize(f.Path)
			if err != nil {
				ferr = err
				return false
			}
			row := deriveRow(p, f, prev[p], gen, now)
			if err := upsertPath(tx, p, row); err != nil {
				ferr = err
				return false
			}
			rep.Rows++
			c := row.Condition()
			rep.ByState[c]++
			switch c {
			case HydratedModified:
				rep.Modified = append(rep.Modified, p)
			case Conflict, Missing:
				rep.Conflicts = append(rep.Conflicts, p)
			}
			return true
		})
		return ferr
	})
	if err != nil {
		return RebuildReport{}, err
	}
	return rep, nil
}

func deriveRow(p string, f Fact, prevIntent Intent, gen, now int64) PathRow {
	row := PathRow{Path: p, IndexPointerDigest: f.IndexPointerOID, Generation: gen, LastVerifiedNS: now}
	switch {
	case !f.Working.Present:
		row.Intent = prevIntent
		if row.Intent == "" {
			row.Intent = IntentUnhydrated
		}
	case f.Working.IsPointer:
		row.Intent = IntentUnhydrated
		row.WorkingDigest = f.Working.Digest
		row.Fingerprint = f.Working.Fingerprint
	default:
		row.Intent = IntentHydrated
		row.WorkingDigest = f.Working.Digest
		row.Fingerprint = f.Working.Fingerprint
	}
	return row
}

// ScanFile observes a working file read-only: it stats and hashes the bytes,
// or decodes them as a pointer. It never opens the file for writing.
func ScanFile(path string) (WorkingFact, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return WorkingFact{}, nil
	}
	if err != nil {
		return WorkingFact{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return WorkingFact{}, err
	}
	defer f.Close()
	wf := WorkingFact{Present: true, Fingerprint: FingerprintOf(fi)}
	if fi.Size() <= pointer.MaxSize {
		b, err := io.ReadAll(f)
		if err != nil {
			return WorkingFact{}, err
		}
		if ptr, err := pointer.Decode(b); err == nil {
			wf.IsPointer, wf.Digest = true, ptr.OID
			return wf, nil
		}
		sum := sha256.Sum256(b)
		wf.Digest = "sha256:" + hex.EncodeToString(sum[:])
		return wf, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return WorkingFact{}, err
	}
	wf.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return wf, nil
}
