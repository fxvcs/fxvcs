package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/format"
)

func fixedClock() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// syntheticChain is a test-only v0->v1->v2->v3 chain; failAt makes that
// step's Apply return an error after a partial write to prove rollback.
func syntheticChain(failAt int) *Registry {
	mk := func(from int, ddl string) Migration {
		return Migration{From: from, To: from + 1, Description: "synthetic " + ddl, Apply: func(tx Tx) error {
			if _, err := tx.Exec(ddl); err != nil {
				return err
			}
			if from+1 == failAt {
				return errors.New("simulated crash")
			}
			return nil
		}}
	}
	return MustRegistry(
		mk(0, "CREATE TABLE t1 (id INTEGER PRIMARY KEY, v TEXT)"),
		mk(1, "CREATE TABLE t2 (id INTEGER PRIMARY KEY)"),
		mk(2, "CREATE TABLE t3 (id INTEGER PRIMARY KEY)"),
	)
}

func openTemp(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	db, err := sql.Open(DriverName, p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, p
}

func reopen(t *testing.T, db *sql.DB, p string) *sql.DB {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	nd, err := sql.Open(DriverName, p)
	if err != nil {
		t.Fatal(err)
	}
	nd.SetMaxOpenConns(1)
	t.Cleanup(func() { nd.Close() })
	return nd
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestRegistryValidation(t *testing.T) {
	step := func(f int) Migration { return Migration{From: f, To: f + 1, Apply: func(Tx) error { return nil }} }
	if _, err := NewRegistry(); !errors.Is(err, ErrGap) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := NewRegistry(step(0), step(2)); !errors.Is(err, ErrGap) {
		t.Fatalf("gap: %v", err)
	}
	if _, err := NewRegistry(Migration{From: 0, To: 2, Apply: func(Tx) error { return nil }}); !errors.Is(err, ErrGap) {
		t.Fatalf("skip: %v", err)
	}
	if _, err := NewRegistry(Migration{From: 0, To: 1}); err == nil {
		t.Fatal("nil Apply accepted")
	}
	r, err := NewRegistry(step(0), step(1), step(2))
	if err != nil {
		t.Fatal(err)
	}
	if r.Latest() != 3 || r.Oldest() != 0 {
		t.Fatalf("latest/oldest = %d/%d", r.Latest(), r.Oldest())
	}
}

func TestPlan(t *testing.T) {
	r := syntheticChain(0)
	cases := []struct {
		cur, tgt int
		steps    int
		err      error
	}{
		{0, 3, 3, nil},
		{1, 3, 2, nil},
		{2, 2, 0, nil},
		{3, 3, 0, nil},
		{4, 3, 0, ErrNewerThanSupported},
		{999, 3, 0, ErrNewerThanSupported},
		{-1, 3, 0, ErrUnknownVersion},
		{0, 4, 0, ErrGap},
	}
	for _, c := range cases {
		p, err := r.Plan(c.cur, c.tgt)
		if !errors.Is(err, c.err) {
			t.Errorf("Plan(%d,%d) err = %v, want %v", c.cur, c.tgt, err, c.err)
			continue
		}
		if err == nil && len(p.Steps) != c.steps {
			t.Errorf("Plan(%d,%d) steps = %d, want %d", c.cur, c.tgt, len(p.Steps), c.steps)
		}
		for i := 1; i < len(p.Steps); i++ {
			if p.Steps[i].From != p.Steps[i-1].To {
				t.Errorf("Plan(%d,%d) not ordered", c.cur, c.tgt)
			}
		}
	}
}

func TestRunOrderingAndJournal(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, "chain.db")
	r := syntheticChain(0)
	plan, err := r.Plan(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, plan, RunOptions{Now: fixedClock}); err != nil {
		t.Fatal(err)
	}
	st, err := Verify(ctx, db)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if st.UserVersion != 3 || st.JournalApplied != 3 || len(st.Journal) != 3 {
		t.Fatalf("status = %+v", st)
	}
	for i, e := range st.Journal {
		if e.Version != i+1 || e.InProgress || e.AppliedAt == "" || e.Checksum != r.steps[i].Checksum() {
			t.Fatalf("journal[%d] = %+v", i, e)
		}
	}
	for _, tb := range []string{"t1", "t2", "t3"} {
		if !tableExists(t, db, tb) {
			t.Fatalf("missing %s", tb)
		}
	}
	// Idempotent re-run: plan from 3 is empty and nothing changes.
	p2, err := r.Plan(3, 3)
	if err != nil || !p2.Empty() {
		t.Fatalf("replan: %v %+v", err, p2)
	}
	if err := Run(ctx, db, p2, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, plan, RunOptions{}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("stale plan should be refused: %v", err)
	}
}

func TestRunInterruptionDetectedAndResumable(t *testing.T) {
	ctx := context.Background()
	db, p := openTemp(t, "crash.db")
	crashy := syntheticChain(3)
	plan, _ := crashy.Plan(0, 3)
	err := Run(ctx, db, plan, RunOptions{Now: fixedClock})
	if err == nil {
		t.Fatal("expected simulated crash")
	}
	// Reopen: steps 1 and 2 committed, step 3 rolled back but marked in progress.
	db = reopen(t, db, p)
	uv, _ := UserVersion(db)
	if uv != 2 {
		t.Fatalf("user_version = %d, want 2", uv)
	}
	if tableExists(t, db, "t3") {
		t.Fatal("step 3 DDL survived rollback")
	}
	st, err := Verify(ctx, db)
	if !errors.Is(err, ErrInterruptedMigration) {
		t.Fatalf("verify: %v", err)
	}
	if !st.Resumable() {
		t.Fatalf("expected resumable: %+v", st)
	}
	if g := Recover(Derived, st, err); g.Action != ActionResume {
		t.Fatalf("guidance = %+v", g)
	}
	// Re-run with a working chain from the recorded version: idempotent, completes.
	good := syntheticChain(0)
	plan2, err := good.Plan(uv, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, db, plan2, RunOptions{Now: fixedClock}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if st, err := Verify(ctx, db); err != nil || st.UserVersion != 3 || len(st.InProgress) != 0 {
		t.Fatalf("after resume: %+v %v", st, err)
	}
	if !tableExists(t, db, "t3") {
		t.Fatal("t3 missing after resume")
	}
}

func TestVerifyDisagreementGuidance(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, "bad.db")
	plan, _ := syntheticChain(0).Plan(0, 2)
	if err := Run(ctx, db, plan, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// Tamper: journal says 2 applied, user_version rolled back to 1.
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	st, err := Verify(ctx, db)
	if !errors.Is(err, ErrInterruptedMigration) || st.Resumable() {
		t.Fatalf("verify: %v %+v", err, st)
	}
	if g := Recover(Derived, st, err); g.Action != ActionRebuild {
		t.Fatalf("derived guidance = %+v", g)
	}
	if g := Recover(Authoritative, st, err); g.Action != ActionRestore {
		t.Fatalf("authoritative guidance = %+v", g)
	}
	// user_version without a journal is also inconsistent.
	db2, _ := openTemp(t, "nojournal.db")
	if _, err := db2.Exec("PRAGMA user_version = 7"); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, db2); !errors.Is(err, ErrInterruptedMigration) {
		t.Fatalf("no journal: %v", err)
	}
	fresh, _ := openTemp(t, "fresh.db")
	if st, err := Verify(ctx, fresh); err != nil || st.UserVersion != 0 || st.HasJournal {
		t.Fatalf("fresh: %+v %v", st, err)
	}
}

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestMigrateFileSwapAndBackup(t *testing.T) {
	ctx := context.Background()
	db, p := openTemp(t, "swap.db")
	plan, _ := syntheticChain(0).Plan(0, 1)
	if err := Run(ctx, db, plan, RunOptions{Now: fixedClock}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t1 (id, v) VALUES (1, 'keep')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rep, err := MigrateFile(ctx, p, syntheticChain(0), FileOptions{KeepBackup: true, Run: RunOptions{Now: fixedClock}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Applied || rep.From != 1 || rep.To != 3 || rep.BackupPath != BackupPath(p, 1) {
		t.Fatalf("report = %+v", rep)
	}
	if _, err := os.Stat(StagingPath(p)); !os.IsNotExist(err) {
		t.Fatal("staging file left behind")
	}
	// Backup is the pre-migration database.
	bk, err := sql.Open(DriverName, rep.BackupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer bk.Close()
	if uv, _ := UserVersion(bk); uv != 1 {
		t.Fatalf("backup user_version = %d", uv)
	}
	// Migrated database has data and new schema.
	nd, err := sql.Open(DriverName, p)
	if err != nil {
		t.Fatal(err)
	}
	defer nd.Close()
	if st, err := Verify(ctx, nd); err != nil || st.UserVersion != 3 {
		t.Fatalf("migrated: %+v %v", st, err)
	}
	var v string
	if err := nd.QueryRow("SELECT v FROM t1 WHERE id=1").Scan(&v); err != nil || v != "keep" {
		t.Fatalf("data lost: %v %q", err, v)
	}
	// Second call is a no-op.
	rep2, err := MigrateFile(ctx, p, syntheticChain(0), FileOptions{})
	if err != nil || rep2.Applied {
		t.Fatalf("noop: %+v %v", rep2, err)
	}
}

func TestMigrateFileRefusesNewerBeforeWrite(t *testing.T) {
	ctx := context.Background()
	db, p := openTemp(t, "future.db")
	if _, err := db.Exec("PRAGMA user_version = 999; CREATE TABLE future (x)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, p)
	entries, _ := os.ReadDir(filepath.Dir(p))
	_, err := MigrateFile(ctx, p, syntheticChain(0), FileOptions{})
	if !errors.Is(err, ErrNewerThanSupported) {
		t.Fatalf("err = %v", err)
	}
	if hashFile(t, p) != before {
		t.Fatal("file modified despite refusal")
	}
	after, _ := os.ReadDir(filepath.Dir(p))
	if len(after) != len(entries) {
		t.Fatalf("files created despite refusal: %d -> %d", len(entries), len(after))
	}
}

func TestMigrateFileCrashLeavesOriginalIntact(t *testing.T) {
	ctx := context.Background()
	db, p := openTemp(t, "crash.db")
	plan, _ := syntheticChain(0).Plan(0, 1)
	if err := Run(ctx, db, plan, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, p)
	_, err := MigrateFile(ctx, p, syntheticChain(3), FileOptions{})
	if err == nil {
		t.Fatal("expected crash")
	}
	if hashFile(t, p) != before {
		t.Fatal("original modified by failed swap migration")
	}
	if _, err := os.Stat(StagingPath(p)); !os.IsNotExist(err) {
		t.Fatal("staging not cleaned")
	}
}

func TestDumpLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t, "dump.db")
	plan, _ := syntheticChain(0).Plan(0, 3)
	if err := Run(ctx, db, plan, RunOptions{Now: fixedClock}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t1 VALUES (1, 'it''s'), (2, NULL); INSERT INTO t2 VALUES (5)"); err != nil {
		t.Fatal(err)
	}
	d1, err := Dump(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	db2, _ := openTemp(t, "dump2.db")
	if err := LoadSQL(ctx, db2, d1); err != nil {
		t.Fatalf("load: %v\n%s", err, d1)
	}
	d2, err := Dump(ctx, db2)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("round trip differs:\n%s\n---\n%s", d1, d2)
	}
	if st, err := Verify(ctx, db2); err != nil || st.UserVersion != 3 {
		t.Fatalf("loaded verify: %+v %v", st, err)
	}
}

func TestDocMigrationChain(t *testing.T) {
	const v0 = format.Group + "/v1alpha0"
	reg, err := NewDocRegistry(DocMigration{
		FromAPIVersion: v0, ToAPIVersion: format.CurrentAPIVersion, Description: "rename storage.domain to storageDomainID",
		Apply: func(doc map[string]any) (map[string]any, error) {
			if v, ok := doc["storageDomain"]; ok {
				doc["storageDomainID"] = v
				delete(doc, "storageDomain")
			}
			return doc, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"apiVersion": v0, "kind": "Repository", "storageDomain": "domain_a"}
	plan, err := reg.PlanDoc(v0, format.CurrentAPIVersion)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan: %v %d", err, len(plan))
	}
	out, err := ApplyDoc(in, plan)
	if err != nil {
		t.Fatal(err)
	}
	if out["apiVersion"] != format.CurrentAPIVersion || out["storageDomainID"] != "domain_a" || out["storageDomain"] != nil {
		t.Fatalf("out = %v", out)
	}
	if in["storageDomain"] != "domain_a" || in["apiVersion"] != v0 {
		t.Fatalf("input mutated: %v", in)
	}
	// Already current: empty plan.
	if p, err := reg.PlanDoc(format.CurrentAPIVersion, format.CurrentAPIVersion); err != nil || len(p) != 0 {
		t.Fatalf("noop plan: %v %d", err, len(p))
	}
	// Newer than the chain: refused, typed.
	if _, err := reg.PlanDoc(format.Group+"/v2alpha1", format.CurrentAPIVersion); !errors.Is(err, ErrNewerThanSupported) {
		t.Fatalf("newer: %v", err)
	}
	if _, err := reg.PlanDoc(format.CurrentAPIVersion, v0); !errors.Is(err, ErrNewerThanSupported) {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err := reg.PlanDoc("other.dev/v1", format.CurrentAPIVersion); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := reg.PlanDoc(v0, "fxvcs.dev/v9"); !errors.Is(err, ErrGap) {
		t.Fatalf("bad target: %v", err)
	}
	// Mismatched document is refused before applying.
	if _, err := ApplyDoc(map[string]any{"apiVersion": "x"}, plan); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	// Chain validation.
	if _, err := NewDocRegistry(DocMigration{FromAPIVersion: "a", ToAPIVersion: "b", Apply: func(d map[string]any) (map[string]any, error) { return d, nil }},
		DocMigration{FromAPIVersion: "c", ToAPIVersion: "d", Apply: func(d map[string]any) (map[string]any, error) { return d, nil }}); !errors.Is(err, ErrGap) {
		t.Fatalf("doc gap: %v", err)
	}
}
