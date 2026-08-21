package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/migrate"
	"fxvcs.dev/fxvcs/internal/pointer"
)

var update = flag.Bool("update", false, "rewrite golden fixtures under spec/fixtures")

const (
	oidA     = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oidB     = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	oidC     = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	manifest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func fixedClock() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func testOpts(create bool) Options {
	return Options{Create: create, Migrate: create, InstalledVersion: "0.1.0-test", RepositoryID: "repo_fixture", StorageDomainID: "domain_fixture", Now: fixedClock}
}

func fixtureDir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "spec", "fixtures"}, parts...)...)
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// loadFixtureDB reconstructs a database file from a SQL fixture in a temp dir.
func loadFixtureDB(t *testing.T, sqlPath, name string) string {
	t.Helper()
	script, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	db := rawOpen(t, p)
	if err := migrate.LoadSQL(context.Background(), db, string(script)); err != nil {
		t.Fatalf("load %s: %v", sqlPath, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocate(t *testing.T) {
	t.Parallel()
	p := Locate(filepath.Join("repo", ".git", "worktrees", "wt"), filepath.Join("repo", ".git"))
	if p.StateDB != filepath.Join("repo", ".git", "worktrees", "wt", "fxvcs", "state.db") ||
		p.PublicationDB != filepath.Join("repo", ".git", "fxvcs", "publication.db") ||
		p.TmpDir != filepath.Join("repo", ".git", "worktrees", "wt", "fxvcs", "tmp") ||
		p.ConfigYAML != filepath.Join("repo", ".git", "fxvcs", "config.yaml") {
		t.Fatalf("paths = %+v", p)
	}
	q := Locate(".git", "")
	if q.PublicationDB != filepath.Join(".git", "fxvcs", "publication.db") {
		t.Fatalf("common dir default: %+v", q)
	}
}

func TestSchemaVersionsAgreeWithFormat(t *testing.T) {
	t.Parallel()
	if HydrationMigrations().Latest() != format.HydrationStateSchema {
		t.Fatalf("hydration chain reaches %d, format declares %d", HydrationMigrations().Latest(), format.HydrationStateSchema)
	}
	if PublicationMigrations().Latest() != format.PublicationLedgerSchema {
		t.Fatalf("publication chain reaches %d, format declares %d", PublicationMigrations().Latest(), format.PublicationLedgerSchema)
	}
}

func TestOpenCreateRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := Locate(t.TempDir(), "").StateDB
	db, err := Open(ctx, p, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := db.SchemaVersion(); v != format.HydrationStateSchema {
		t.Fatalf("schema = %d", v)
	}
	meta, err := db.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[MetaDatabase] != KindHydrationState || meta[MetaSchema] != fmt.Sprint(format.HydrationStateSchema) || meta[MetaRepositoryID] != "repo_fixture" || meta[MetaCreatedByVersion] != "0.1.0-test" {
		t.Fatalf("meta = %v", meta)
	}
	row := PathRow{Path: "assets/a.ytd", Intent: IntentHydrated, IndexPointerDigest: oidA, WorkingDigest: oidA, Fingerprint: Fingerprint{Size: 10, MtimeNS: 20, FileID: "1:2"}, LastVerifiedNS: 30, Generation: 1}
	if err := db.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := db.Upsert(ctx, PathRow{Path: "x", Intent: "weird", IndexPointerDigest: oidA}); !errors.Is(err, ErrIntentInvalid) {
		t.Fatalf("bad intent: %v", err)
	}
	if err := db.Upsert(ctx, PathRow{Path: "../escape", Intent: IntentHydrated}); err == nil {
		t.Fatal("path escape accepted")
	}
	got, ok, err := db.Get(ctx, "assets/a.ytd")
	if err != nil || !ok || got != row {
		t.Fatalf("get = %+v %v %v", got, ok, err)
	}
	if got.Condition() != HydratedClean {
		t.Fatalf("condition = %s", got.Condition())
	}
	rows, _ := db.List(ctx)
	if len(rows) != 1 {
		t.Fatalf("list = %d", len(rows))
	}
	if err := db.Delete(ctx, "assets/a.ytd"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.Get(ctx, "assets/a.ytd"); ok {
		t.Fatal("still present after delete")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen without Create: works, schema verified.
	db2, err := Open(ctx, p, Options{Now: fixedClock})
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()
	// Missing without Create: ErrNotFound and no file created.
	missing := filepath.Join(t.TempDir(), "none", "state.db")
	if _, err := Open(ctx, missing, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("file created despite missing Create")
	}
}

func TestPathRowConditions(t *testing.T) {
	t.Parallel()
	fp := Fingerprint{Size: 1, MtimeNS: 1}
	cases := []struct {
		row  PathRow
		want Condition
	}{
		{PathRow{Intent: IntentUnhydrated, IndexPointerDigest: oidA, WorkingDigest: oidA, Fingerprint: fp}, UnhydratedClean},
		{PathRow{Intent: IntentHydrated, IndexPointerDigest: oidA, WorkingDigest: oidA, Fingerprint: fp}, HydratedClean},
		{PathRow{Intent: IntentHydrated, IndexPointerDigest: oidA, WorkingDigest: oidB, Fingerprint: fp}, HydratedModified},
		{PathRow{Intent: IntentUnhydrated, IndexPointerDigest: oidA, WorkingDigest: oidB, Fingerprint: fp}, Conflict},
		{PathRow{Intent: IntentUnhydrated, IndexPointerDigest: oidA}, Missing},
		{PathRow{Intent: IntentHydrated, WorkingDigest: oidA, Fingerprint: fp}, Conflict},
	}
	for i, c := range cases {
		if got := c.row.Condition(); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

func TestOpenRefusesNewerSchemaBeforeWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, kind := range []string{"state-db", "publication-db"} {
		t.Run(kind, func(t *testing.T) {
			p := loadFixtureDB(t, filepath.Join(fixtureDir(t, "upgrade", kind), "future.sql"), "future.db")
			before, listing := hashFile(t, p), dirListing(t, filepath.Dir(p))
			var err error
			if kind == "state-db" {
				_, err = Open(ctx, p, testOpts(true))
			} else {
				_, err = OpenLedger(ctx, p, testOpts(true))
			}
			if !errors.Is(err, migrate.ErrNewerThanSupported) || !errors.Is(err, format.ErrUnsupportedSchemaDB) {
				t.Fatalf("err = %v", err)
			}
			var oe *OpenError
			if !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionNone {
				t.Fatalf("guidance: %v", err)
			}
			if hashFile(t, p) != before {
				t.Fatal("database modified despite refusal")
			}
			if got := dirListing(t, filepath.Dir(p)); strings.Join(got, ",") != strings.Join(listing, ",") {
				t.Fatalf("side files created: %v -> %v", listing, got)
			}
		})
	}
}

func TestOpenCorruptDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	garbage := fixtureDir(t, "crash", "not-a-database.txt")
	cases := []struct {
		name string
		make func(t *testing.T) string
	}{
		{"garbage", func(t *testing.T) string {
			b, err := os.ReadFile(garbage)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "state.db")
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"truncated", func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "state.db")
			db, err := Open(ctx, p, testOpts(true))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Upsert(ctx, PathRow{Path: "a", Intent: IntentHydrated, IndexPointerDigest: oidA}); err != nil {
				t.Fatal(err)
			}
			db.Close()
			b, _ := os.ReadFile(p)
			if err := os.WriteFile(p, b[:len(b)/2], 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.make(t)
			before := hashFile(t, p)
			_, err := Open(ctx, p, testOpts(true))
			var oe *OpenError
			if !errors.Is(err, ErrCorrupt) || !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionRebuild {
				t.Fatalf("state.db corrupt: %v", err)
			}
			if hashFile(t, p) != before {
				t.Fatal("corrupt file was modified")
			}
			_, err = OpenLedger(ctx, p, testOpts(true))
			if !errors.Is(err, ErrCorrupt) || !errors.As(err, &oe) || oe.Guidance.Action != migrate.ActionRestore {
				t.Fatalf("publication.db corrupt: %v", err)
			}
			if hashFile(t, p) != before {
				t.Fatal("corrupt file was modified")
			}
		})
	}
}

func TestOpenWrongKindAndOlderSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, p, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := OpenLedger(ctx, p, Options{}); !errors.Is(err, ErrWrongDatabase) {
		t.Fatalf("wrong kind: %v", err)
	}
	// Existing v0 (empty) database, Migrate=false: refused with guidance, untouched.
	v0 := loadFixtureDB(t, filepath.Join(fixtureDir(t, "upgrade", "state-db"), "v0.sql"), "state.db")
	before := hashFile(t, v0)
	if _, err := Open(ctx, v0, Options{Create: true}); !errors.Is(err, ErrNeedsMigration) {
		t.Fatalf("older without Migrate: %v", err)
	}
	if hashFile(t, v0) != before {
		t.Fatal("older database modified without Migrate")
	}
}

// TestGoldenUpgrade proves every supported prior schema reaches the current
// schema deterministically. v<current>.sql is (re)generated from code with
// -update; older vN.sql fixtures are frozen history.
func TestGoldenUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kinds := []struct {
		dir     string
		current int
		build   func(t *testing.T, path string)
	}{
		{"state-db", format.HydrationStateSchema, buildStateSample},
		{"publication-db", format.PublicationLedgerSchema, buildLedgerSample},
	}
	for _, k := range kinds {
		t.Run(k.dir, func(t *testing.T) {
			dir := fixtureDir(t, "upgrade", k.dir)
			currentFixture := filepath.Join(dir, "v"+strconv.Itoa(k.current)+".sql")
			if *update {
				p := filepath.Join(t.TempDir(), "current.db")
				k.build(t, p)
				dump := dumpPath(t, p)
				if err := os.WriteFile(currentFixture, []byte(dump), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			matches, _ := filepath.Glob(filepath.Join(dir, "v*.sql"))
			if len(matches) == 0 {
				t.Fatal("no vN.sql fixtures")
			}
			for _, fx := range matches {
				n := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(fx), "v"), ".sql")
				expected := filepath.Join(dir, "expected-v"+n+"-to-current.sql")
				p := loadFixtureDB(t, fx, "fixture.db")
				var err error
				if k.dir == "state-db" {
					var d *DB
					d, err = Open(ctx, p, testOpts(true))
					if err == nil {
						d.Close()
					}
				} else {
					var l *Ledger
					l, err = OpenLedger(ctx, p, testOpts(true))
					if err == nil {
						l.Close()
					}
				}
				if err != nil {
					t.Fatalf("open %s: %v", fx, err)
				}
				got := dumpPath(t, p)
				if !strings.HasPrefix(got, "PRAGMA user_version = "+strconv.Itoa(k.current)+";") {
					t.Fatalf("%s did not reach current schema:\n%s", fx, got)
				}
				if *update {
					if err := os.WriteFile(expected, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(expected)
				if err != nil {
					t.Fatalf("missing golden %s (run with -update): %v", expected, err)
				}
				if string(want) != got {
					t.Errorf("%s: migrated dump differs from %s\n--- got ---\n%s\n--- want ---\n%s", fx, filepath.Base(expected), got, want)
				}
				// Determinism: a second run yields the identical dump.
				p2 := loadFixtureDB(t, fx, "fixture2.db")
				if k.dir == "state-db" {
					d, err := Open(ctx, p2, testOpts(true))
					if err != nil {
						t.Fatal(err)
					}
					d.Close()
				} else {
					l, err := OpenLedger(ctx, p2, testOpts(true))
					if err != nil {
						t.Fatal(err)
					}
					l.Close()
				}
				if dumpPath(t, p2) != got {
					t.Errorf("%s: migration is not deterministic", fx)
				}
			}
		})
	}
}

func buildStateSample(t *testing.T, path string) {
	ctx := context.Background()
	db, err := Open(ctx, path, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := []PathRow{
		{Path: "assets/hydrated-clean.ytd", Intent: IntentHydrated, IndexPointerDigest: oidA, WorkingDigest: oidA, Fingerprint: Fingerprint{Size: 4096, MtimeNS: 1700000000000000000, FileID: "1:100"}, LastVerifiedNS: 1700000001000000000},
		{Path: "assets/unhydrated.ytd", Intent: IntentUnhydrated, IndexPointerDigest: oidB, WorkingDigest: oidB, Fingerprint: Fingerprint{Size: 220, MtimeNS: 1700000000000000000, FileID: "1:101"}},
	}
	for _, r := range rows {
		if err := db.Upsert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// One committed and one aborted operation, deterministic ids.
	op, err := db.BeginOperation(ctx, OpHydrate, "assets/hydrated-clean.ytd", Expectation{IndexOID: oidA, Fingerprint: Fingerprint{Size: 220, MtimeNS: 1700000000000000000, FileID: "1:100"}})
	if err != nil {
		t.Fatal(err)
	}
	op.row.OpID = "op-fixture-1"
	if _, err := db.SQL().Exec("UPDATE journal SET op_id = 'op-fixture-1' WHERE generation = ?", op.row.Generation); err != nil {
		t.Fatal(err)
	}
	if err := op.Advance(ctx, PhaseFetched); err != nil {
		t.Fatal(err)
	}
	if err := op.Commit(ctx, rows[0]); err != nil {
		t.Fatal(err)
	}
	op2, err := db.BeginOperation(ctx, OpDehydrate, "assets/unhydrated.ytd", Expectation{IndexOID: oidB})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec("UPDATE journal SET op_id = 'op-fixture-2' WHERE generation = ?", op2.row.Generation); err != nil {
		t.Fatal(err)
	}
	op2.row.OpID = "op-fixture-2"
	if err := op2.Abort(ctx, "index changed"); err != nil {
		t.Fatal(err)
	}
}

func buildLedgerSample(t *testing.T, path string) {
	ctx := context.Background()
	l, err := OpenLedger(ctx, path, testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, d := range []string{oidA, oidB} {
		if err := l.RecordIngest(ctx, ObjectRecord{Digest: d, StorageDomainID: "domain_fixture", Size: 4096, Encoding: pointer.EncodingWhole, ManifestDigest: manifest}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.AddReferrer(ctx, oidA, "assets/hydrated-clean.ytd", "index"); err != nil {
		t.Fatal(err)
	}
	if err := l.MarkPublished(ctx, oidA, "origin-s3"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Settle(ctx, []string{"origin-s3"}); err != nil {
		t.Fatal(err)
	}
}

func dumpPath(t *testing.T, p string) string {
	t.Helper()
	db := rawOpen(t, p)
	defer db.Close()
	s, err := migrate.Dump(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := StatFingerprint(p)
	if err != nil || fp.Size != 5 || fp.MtimeNS == 0 {
		t.Fatalf("fp = %+v %v", fp, err)
	}
	if runtime.GOOS != "windows" && fp.FileID == "" {
		t.Fatal("expected dev:ino file id on unix")
	}
	back, err := ParseFingerprint(fp.String())
	if err != nil || back != fp {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if _, err := ParseFingerprint("nope"); err == nil {
		t.Fatal("malformed accepted")
	}
	if z, err := StatFingerprint(filepath.Join(t.TempDir(), "absent")); err != nil || !z.IsZero() {
		t.Fatalf("absent: %+v %v", z, err)
	}
}

// TestListPathsMatchesList: the batched, path-scoped read must return exactly
// what a full read would have said about those paths, including across the
// query batch boundary, and must not invent rows for paths it has none for.
func TestListPathsMatchesList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"), testOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var recorded []string
	for i := 0; i < 1100; i++ {
		p := "assets/d" + strconv.Itoa(i%10) + "/f" + strconv.Itoa(i) + ".bin"
		row := PathRow{Path: p, Intent: IntentUnhydrated, IndexPointerDigest: oidA, WorkingDigest: oidA,
			Fingerprint: Fingerprint{Size: int64(i), MtimeNS: int64(i) * 10, FileID: "1:1"}}
		if err := db.Upsert(ctx, row); err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, p)
	}
	all, err := db.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]PathRow{}
	for _, r := range all {
		want[r.Path] = r
	}

	for _, n := range []int{1, 499, 500, 501, 1000, 1100} {
		got, err := db.ListPaths(ctx, recorded[:n])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != n {
			t.Fatalf("ListPaths(%d paths) returned %d rows", n, len(got))
		}
		for _, r := range got {
			if r != want[r.Path] {
				t.Fatalf("%s: %+v, List says %+v", r.Path, r, want[r.Path])
			}
		}
	}
	// Paths with no row are skipped, not faked.
	got, err := db.ListPaths(ctx, []string{recorded[0], "assets/never-recorded.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != recorded[0] {
		t.Fatalf("unknown path produced %+v", got)
	}
}
