package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/migrate"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// rawOpen opens a SQLite file without any FXVCS negotiation (fixture plumbing).
func rawOpen(t *testing.T, p string) *sql.DB {
	t.Helper()
	db, err := sql.Open(migrate.DriverName, p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func samplePointer(oid string) []byte {
	return pointer.Pointer{StorageDomainID: "domain_fixture", Encoding: pointer.EncodingWhole, ManifestDigest: manifest, OID: oid, Size: 4096}.MustEncode()
}

// TestRebuildFromFactsNeverWritesDirtyFile: state.db is deleted entirely and
// rebuilt from read-only facts; a working file whose bytes differ from the
// index pointer is recorded as hydrated-modified and left byte-for-byte and
// mtime-identical.
func TestRebuildFromFactsNeverWritesDirtyFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	git := t.TempDir()
	work := t.TempDir()
	statePath := Locate(git, "").StateDB

	// Prior database with rows that will be lost.
	db, err := Open(ctx, statePath, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(ctx, PathRow{Path: "assets/dirty.ytd", Intent: IntentHydrated, IndexPointerDigest: oidA, WorkingDigest: oidA}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}

	// Working tree: dirty hydrated file, clean pointer, conflicting pointer, missing.
	dirty := filepath.Join(work, "assets", "dirty.ytd")
	if err := os.MkdirAll(filepath.Dir(dirty), 0o755); err != nil {
		t.Fatal(err)
	}
	dirtyBytes := []byte("edited locally, not what the index pointer says")
	if err := os.WriteFile(dirty, dirtyBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	cleanPtr := filepath.Join(work, "assets", "clean.ytd")
	if err := os.WriteFile(cleanPtr, samplePointer(oidB), 0o644); err != nil {
		t.Fatal(err)
	}
	stalePtr := filepath.Join(work, "assets", "stale.ytd")
	if err := os.WriteFile(stalePtr, samplePointer(oidC), 0o644); err != nil {
		t.Fatal(err)
	}
	fiBefore, _ := os.Stat(dirty)

	scan := func(p string) WorkingFact {
		wf, err := ScanFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return wf
	}
	facts := []Fact{
		{Path: "assets/dirty.ytd", IndexPointerOID: oidA, Working: scan(dirty)},
		{Path: "assets/clean.ytd", IndexPointerOID: oidB, Working: scan(cleanPtr)},
		{Path: "assets/stale.ytd", IndexPointerOID: oidA, Working: scan(stalePtr)},
		{Path: "assets/missing.ytd", IndexPointerOID: oidA, Working: scan(filepath.Join(work, "assets", "missing.ytd"))},
	}

	db, err = Open(ctx, statePath, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rep, err := db.Rebuild(ctx, func(yield func(Fact) bool) {
		for _, f := range facts {
			if !yield(f) {
				return
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 4 || rep.ByState[HydratedModified] != 1 || rep.ByState[UnhydratedClean] != 1 || rep.ByState[Conflict] != 1 || rep.ByState[Missing] != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Modified) != 1 || rep.Modified[0] != "assets/dirty.ytd" {
		t.Fatalf("modified = %v", rep.Modified)
	}
	row, ok, _ := db.Get(ctx, "assets/dirty.ytd")
	if !ok || row.Condition() != HydratedModified || row.Intent != IntentHydrated || row.WorkingDigest == oidA {
		t.Fatalf("dirty row = %+v", row)
	}
	if row.Fingerprint != FingerprintOf(fiBefore) {
		t.Fatalf("fingerprint %+v != stat %+v", row.Fingerprint, FingerprintOf(fiBefore))
	}
	// The working file was never written.
	after, _ := os.ReadFile(dirty)
	fiAfter, _ := os.Stat(dirty)
	if string(after) != string(dirtyBytes) || !fiAfter.ModTime().Equal(fiBefore.ModTime()) || fiAfter.Size() != fiBefore.Size() {
		t.Fatal("rebuild modified the dirty working file")
	}
	// Second rebuild is idempotent.
	rep2, err := db.Rebuild(ctx, func(yield func(Fact) bool) {
		for _, f := range facts {
			if !yield(f) {
				return
			}
		}
	})
	if err != nil || rep2.Rows != 4 {
		t.Fatalf("second rebuild: %+v %v", rep2, err)
	}
}

func TestOperationJournalInterruptedAndReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, p, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint{Size: 10, MtimeNS: 20, FileID: "1:2"}
	op, err := db.BeginOperation(ctx, OpHydrate, "assets/a.ytd", Expectation{IndexOID: oidA, Fingerprint: fp})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Advance(ctx, PhaseFetched); err != nil {
		t.Fatal(err)
	}
	if err := op.Advance(ctx, PhaseCommitted); err == nil {
		t.Fatal("Advance to terminal phase accepted")
	}
	// Crash: close without Commit/Abort.
	db.Close()

	db, err = Open(ctx, p, Options{Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	open, err := db.ListInterrupted(ctx)
	if err != nil || len(open) != 1 || open[0].Phase != PhaseFetched || open[0].Expected != (Expectation{IndexOID: oidA, Fingerprint: fp}) || open[0].Generation != 1 {
		t.Fatalf("interrupted = %+v %v", open, err)
	}
	row := open[0]
	if d := Reconcile(row, oidA, fp); d != SafeRebuild {
		t.Fatalf("unchanged: %s", d)
	}
	if d := Reconcile(row, oidA, Fingerprint{Size: 11, MtimeNS: 20, FileID: "1:2"}); d != KeepDirty {
		t.Fatalf("file changed: %s", d)
	}
	if d := Reconcile(row, oidB, fp); d != IndexMoved {
		t.Fatalf("index moved: %s", d)
	}
	if d := Reconcile(row, oidB, Fingerprint{Size: 11}); d != KeepDirty {
		t.Fatalf("both changed must keep dirty: %s", d)
	}
	if err := db.ResolveInterrupted(ctx, row.OpID, KeepDirty); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.ListInterrupted(ctx); len(open) != 0 {
		t.Fatalf("still interrupted: %+v", open)
	}
	// Commit path: row written with the operation generation.
	op2, err := db.BeginOperation(ctx, OpHydrate, "assets/b.ytd", Expectation{IndexOID: oidB})
	if err != nil {
		t.Fatal(err)
	}
	if err := op2.Commit(ctx, PathRow{Path: "assets/other.ytd", Intent: IntentHydrated, IndexPointerDigest: oidB}); err == nil {
		t.Fatal("commit for another path accepted")
	}
	if err := op2.Commit(ctx, PathRow{Path: "assets/b.ytd", Intent: IntentHydrated, IndexPointerDigest: oidB, WorkingDigest: oidB, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	if err := op2.Commit(ctx, PathRow{}); !errors.Is(err, ErrOperationClosed) {
		t.Fatalf("double commit: %v", err)
	}
	got, ok, _ := db.Get(ctx, "assets/b.ytd")
	if !ok || got.Generation != 2 || got.Condition() != HydratedClean {
		t.Fatalf("committed row = %+v", got)
	}
}

func TestInterruptedOperationFixture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := loadFixtureDB(t, filepath.Join(fixtureDir(t, "crash", "state-db"), "interrupted-operation.sql"), "state.db")
	// The fixture predates the current schema, so opening it migrates.
	db, err := Open(ctx, p, Options{Migrate: true, Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	open, err := db.ListInterrupted(ctx)
	if err != nil || len(open) != 1 || open[0].Phase != PhaseFetched || open[0].Path != "assets/crashed.ytd" {
		t.Fatalf("interrupted = %+v %v", open, err)
	}
	if d := Reconcile(open[0], open[0].Expected.IndexOID, Fingerprint{Size: 1, MtimeNS: 1}); d != KeepDirty {
		t.Fatalf("decision = %s", d)
	}
}

func TestInterruptedMigrationFixture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := filepath.Join(fixtureDir(t, "crash", "state-db"), "interrupted-migration.sql")
	// Without Migrate: refused, guidance resume, file untouched.
	p := loadFixtureDB(t, fx, "state.db")
	before := hashFile(t, p)
	_, err := Open(ctx, p, Options{})
	var oe *OpenError
	if !errors.Is(err, migrate.ErrInterruptedMigration) || !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionResume {
		t.Fatalf("err = %v", err)
	}
	if hashFile(t, p) != before {
		t.Fatal("refused open modified the file")
	}
	// With Migrate: resumes and completes.
	db, err := Open(ctx, p, Options{Migrate: true, Now: fixedClock})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if v, _ := db.SchemaVersion(); v != format.HydrationStateSchema {
		t.Fatalf("schema = %d", v)
	}
	if err := db.Upsert(ctx, PathRow{Path: "a", Intent: IntentHydrated, IndexPointerDigest: oidA}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Non-resumable inconsistency: guidance differs by database kind.
	bad := filepath.Join(fixtureDir(t, "crash", "state-db"), "inconsistent-journal.sql")
	sp := loadFixtureDB(t, bad, "state.db")
	_, err = Open(ctx, sp, Options{Migrate: true})
	if !errors.Is(err, migrate.ErrInterruptedMigration) || !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionRebuild {
		t.Fatalf("state guidance: %v", err)
	}
	lp := loadFixtureDB(t, bad, "publication.db")
	_, err = OpenLedger(ctx, lp, Options{Migrate: true})
	if !errors.Is(err, migrate.ErrInterruptedMigration) || !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionRestore {
		t.Fatalf("ledger guidance: %v", err)
	}
}

func TestLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := Locate(t.TempDir(), "").PublicationDB
	l, err := OpenLedger(ctx, p, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.RecordIngest(ctx, ObjectRecord{Digest: "bad", StorageDomainID: "d", Encoding: "whole/1"}); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("bad digest: %v", err)
	}
	rec := ObjectRecord{Digest: oidA, StorageDomainID: "domain_fixture", Size: 5, Encoding: pointer.EncodingWhole, ManifestDigest: manifest}
	if err := l.RecordIngest(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordIngest(ctx, rec); err != nil {
		t.Fatal("re-record must be idempotent")
	}
	if err := l.RecordIngest(ctx, ObjectRecord{Digest: oidB, StorageDomainID: "domain_fixture", Size: 5, Encoding: pointer.EncodingWhole}); err != nil {
		t.Fatal(err)
	}
	if p, _ := l.IsPending(ctx, oidA); !p {
		t.Fatal("ingested object must be pending")
	}
	if p, _ := l.IsPending(ctx, oidC); p {
		t.Fatal("unknown digest pending")
	}
	if err := l.AddReferrer(ctx, oidA, "assets/a.ytd", "index"); err != nil {
		t.Fatal(err)
	}
	if err := l.AddReferrer(ctx, oidA, "assets/a.ytd", "index"); err != nil {
		t.Fatal("duplicate referrer must be idempotent")
	}
	if err := l.MarkPublished(ctx, oidC, "s3"); err == nil {
		t.Fatal("publish of unknown object accepted")
	}
	if err := l.MarkPublished(ctx, oidA, "s3"); err != nil {
		t.Fatal(err)
	}
	pend, err := l.Pending(ctx, []string{"s3", "backup"})
	if err != nil || len(pend) != 2 {
		t.Fatalf("pending two remotes = %v %v", pend, err)
	}
	pend, _ = l.Pending(ctx, []string{"s3"})
	if len(pend) != 1 || pend[0] != oidB {
		t.Fatalf("pending s3 = %v", pend)
	}
	rel, err := l.Settle(ctx, []string{"s3"})
	if err != nil || len(rel) != 1 || rel[0] != oidA {
		t.Fatalf("settle = %v %v", rel, err)
	}
	if p, _ := l.IsPending(ctx, oidA); p {
		t.Fatal("settled object still pending")
	}
	if pend, _ := l.Pending(ctx, nil); len(pend) != 1 || pend[0] != oidB {
		t.Fatalf("pending after settle = %v", pend)
	}
	obj, ok, _ := l.Object(ctx, oidA)
	if !ok || obj.Pending || obj.Size != 5 || obj.IngestedNS != fixedClock().UnixNano() {
		t.Fatalf("object = %+v", obj)
	}
	refs, _ := l.Referrers(ctx, oidA)
	if len(refs) != 1 || refs[0] != [2]string{"assets/a.ytd", "index"} {
		t.Fatalf("refs = %v", refs)
	}
}

// TestMigrateFileOnRealSchemas exercises the transactional swap with the
// production registries: a v0 (empty) file reaches the current schema and the
// pre-migration backup is kept for the authoritative ledger.
func TestMigrateFileOnRealSchemas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := loadFixtureDB(t, filepath.Join(fixtureDir(t, "upgrade", "publication-db"), "v0.sql"), "publication.db")
	rep, err := migrate.MigrateFile(ctx, p, PublicationMigrations(), migrate.FileOptions{KeepBackup: true, Run: migrate.RunOptions{Now: fixedClock}})
	if err != nil || !rep.Applied || rep.BackupPath == "" {
		t.Fatalf("migrate file: %+v %v", rep, err)
	}
	if _, err := os.Stat(rep.BackupPath); err != nil {
		t.Fatal("backup missing")
	}
	l, err := OpenLedger(ctx, p, Options{Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}
