package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// ckFixture is a repository whose *.bin paths are managed, with the engine
// opened over it. Pointers are staged directly (no filter process): the
// engine's own ingestion path produces the same pointer bytes the clean
// filter would, so the checkout engine can be exercised in-process while the
// filter integration lives in internal/filter.
type ckFixture struct {
	t        *testing.T
	git      *gittest.Repo
	repo     *Repo
	cacheDir string
	storeDir string
}

func ckSetup(t *testing.T) *ckFixture {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	r.WriteFile(".fxvcs/repository.yaml", []byte(repoYAML))
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	r.Commit("init")
	f := &ckFixture{t: t, git: r, cacheDir: t.TempDir()}
	f.repo = f.open("test")
	return f
}

func (f *ckFixture) open(operation string) *Repo {
	f.t.Helper()
	repo, err := Open(context.Background(), f.git.Root, Options{Installed: format.MustParseVersion("0.0.0-dev"), Operation: operation, CacheDir: f.cacheDir})
	if err != nil {
		f.t.Fatal(err)
	}
	repo.Git = repo.Git.WithEnv(f.git.Env)
	f.t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// stage ingests content through the engine and stages its pointer at path.
func (f *ckFixture) stage(path string, content []byte) pointer.Pointer {
	f.t.Helper()
	ing, err := f.repo.Ingest(context.Background(), bytes.NewReader(content), path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.git.WriteFile(path, ing.Pointer.MustEncode())
	f.git.Git("add", "--", path)
	return ing.Pointer
}

// bindRemote binds the committed "primary" remote to a temp directory.
func (f *ckFixture) bindRemote() {
	f.t.Helper()
	if f.storeDir != "" {
		return
	}
	f.storeDir = f.t.TempDir()
	if _, err := f.repo.StorageAdd(context.Background(), StorageAddInput{Name: "primary", Type: "filesystem", Path: f.storeDir}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ckFixture) read(path string) []byte { return f.git.ReadFile(path) }

func (f *ckFixture) statusOf(path string) PathStatus {
	f.t.Helper()
	st, _, err := f.repo.Status(context.Background(), Selection{Paths: []string{path}})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(st) != 1 {
		f.t.Fatalf("status for %s: %+v", path, st)
	}
	return st[0]
}

func (f *ckFixture) tmpLeftovers() []string {
	ents, _ := os.ReadDir(f.repo.Paths.TmpDir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func ckRandom(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func ckDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return cache.Digest(sum[:])
}

func cachedObjectPath(c *cache.Cache, domainID, digest string) string {
	hexPart := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(c.Root(), "objects", domainID, hexPart[:2], hexPart[2:4], hexPart)
}

func overwriteCached(t *testing.T, c *cache.Cache, domainID, digest string, data []byte) {
	t.Helper()
	p := cachedObjectPath(c, domainID, digest)
	_ = os.Chmod(p, 0o644)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHydrateDehydrateStatusRoundTrip(t *testing.T) {
	t.Parallel()
	f := ckSetup(t)
	ctx := context.Background()
	asset := ckRandom(3<<20+123, 1)
	ptr := f.stage("assets/a.bin", asset)
	f.stage("b.bin", []byte("small"))
	f.git.WriteFile("plain.txt", []byte("not managed"))
	f.git.Git("add", "plain.txt")
	f.git.Commit("assets")

	// Fresh state: pointers, cached, pending publication.
	st := f.statusOf("assets/a.bin")
	if st.State != state.UnhydratedClean || !st.Cached || !st.Pending || st.Published || st.Size != int64(len(asset)) || st.IndexDigest != ptr.OID {
		t.Fatalf("initial status %+v", st)
	}
	// Prune cannot evict the pending object.
	if plan, _, err := f.repo.CachePrune(ctx, 0, true); err != nil || len(plan) != 0 {
		t.Fatalf("pending object in prune plan: %+v %v", plan, err)
	}

	var events []ItemProgress
	outs, err := f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/a.bin", "nope.bin", "plain.txt"}}, func(p ItemProgress) { events = append(events, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 {
		t.Fatalf("outcomes %+v", outs)
	}
	byPath := map[string]PathOutcome{}
	for _, o := range outs {
		byPath[o.Path] = o
	}
	if o := byPath["assets/a.bin"]; o.Skipped || o.Bytes != int64(len(asset)) || o.Digest != ptr.OID {
		t.Fatalf("hydrate a.bin: %+v", o)
	}
	if o := byPath["nope.bin"]; !o.Skipped || !errors.Is(o.Err, ErrNotManaged) {
		t.Fatalf("unknown path: %+v", o)
	}
	if o := byPath["plain.txt"]; !o.Skipped || !errors.Is(o.Err, ErrNotManaged) {
		t.Fatalf("unmanaged path: %+v", o)
	}
	if !bytes.Equal(f.read("assets/a.bin"), asset) {
		t.Fatal("hydrated bytes differ")
	}
	if len(events) < 2 || !events[len(events)-1].Done || events[len(events)-1].Bytes != int64(len(asset)) {
		t.Fatalf("progress events %+v", events)
	}
	var writePlan, transferPlan ItemProgress
	for _, event := range events {
		if !event.IsPlan() {
			continue
		}
		switch event.PlanPhase {
		case ProgressPhaseWrite:
			writePlan = event
		case ProgressPhaseTransfer:
			transferPlan = event
		}
	}
	if writePlan.Planned != 3 || transferPlan.Planned != 3 {
		t.Fatalf("plans did not include every selected outcome: write=%+v transfer=%+v", writePlan, transferPlan)
	}
	if got := f.tmpLeftovers(); len(got) != 0 {
		t.Fatalf("tmp leftovers %v", got)
	}
	if st := f.statusOf("assets/a.bin"); st.State != state.HydratedClean {
		t.Fatalf("after hydrate %+v", st)
	}
	// git sees a clean tree only through the filter; here the raw bytes are
	// staged as pointer, so no assertion on git status. Idempotent hydrate:
	outs, _ = f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil)
	if !outs[0].Skipped || outs[0].Reason != "already hydrated" {
		t.Fatalf("second hydrate %+v", outs[0])
	}
	// The stat cache answers without re-hashing when the fingerprint matches.
	db, _ := f.repo.State(ctx)
	row, ok, _ := db.Get(ctx, "assets/a.bin")
	if !ok || row.Intent != state.IntentHydrated || row.WorkingDigest != ptr.OID {
		t.Fatalf("row %+v", row)
	}
	if obs, err := f.repo.observePath("assets/a.bin", ptr.OID, &row); err != nil || obs.Rehashed {
		t.Fatalf("stat cache not used: %+v %v", obs, err)
	}

	// Modify the hydrated file: hydrated-modified; hydrate and dehydrate refuse.
	dirty := append(append([]byte(nil), asset...), 'x')
	f.git.WriteFile("assets/a.bin", dirty)
	if st := f.statusOf("assets/a.bin"); st.State != state.HydratedModified {
		t.Fatalf("modified status %+v", st)
	}
	outs, _ = f.repo.Dehydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil)
	if !outs[0].Skipped || !errors.Is(outs[0].Err, ErrConflict) {
		t.Fatalf("dehydrate dirty: %+v", outs[0])
	}
	outs, _ = f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil)
	if !outs[0].Skipped || !errors.Is(outs[0].Err, ErrConflict) {
		t.Fatalf("hydrate dirty: %+v", outs[0])
	}
	if !bytes.Equal(f.read("assets/a.bin"), dirty) {
		t.Fatal("dirty file was touched")
	}
	// Restore the bytes: hydrated-clean again; dehydrate writes the pointer.
	f.git.WriteFile("assets/a.bin", asset)
	var verifyProgress []ItemProgress
	outs, err = f.repo.Dehydrate(ctx, Selection{}, func(p ItemProgress) {
		if p.Path == "assets/a.bin" && p.Phase == ProgressPhaseVerify {
			verifyProgress = append(verifyProgress, p)
		}
	}) // empty selector = every managed path
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 2 {
		t.Fatalf("dehydrate all: %+v", outs)
	}
	for _, o := range outs {
		switch o.Path {
		case "assets/a.bin":
			if o.Skipped {
				t.Fatalf("dehydrate a.bin %+v", o)
			}
		case "b.bin":
			if !o.Skipped || o.Reason != "already a pointer" {
				t.Fatalf("dehydrate b.bin %+v", o)
			}
		}
	}
	if !bytes.Equal(f.read("assets/a.bin"), ptr.MustEncode()) {
		t.Fatal("dehydrate did not write the exact pointer bytes")
	}
	if len(verifyProgress) < 2 || verifyProgress[0].Bytes != 0 || verifyProgress[len(verifyProgress)-1].Bytes != int64(len(asset)) {
		t.Fatalf("dehydrate verification progress = %+v", verifyProgress)
	}
	if st := f.statusOf("assets/a.bin"); st.State != state.UnhydratedClean {
		t.Fatalf("after dehydrate %+v", st)
	}
	// Glob selection.
	outs, err = f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/*.bin"}, Glob: true}, nil)
	if err != nil || len(outs) != 1 || outs[0].Path != "assets/a.bin" || outs[0].Skipped {
		t.Fatalf("glob hydrate %+v %v", outs, err)
	}
	// Missing working file.
	if err := os.Remove(filepath.Join(f.git.Root, "assets", "a.bin")); err != nil {
		t.Fatal(err)
	}
	if st := f.statusOf("assets/a.bin"); st.State != state.Missing {
		t.Fatalf("missing status %+v", st)
	}
	outs, _ = f.repo.Dehydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil)
	if !outs[0].Skipped {
		t.Fatalf("dehydrate missing %+v", outs[0])
	}
	// Hydrate re-materializes a missing file.
	outs, _ = f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil)
	if outs[0].Skipped || !bytes.Equal(f.read("assets/a.bin"), asset) {
		t.Fatalf("hydrate missing %+v", outs[0])
	}
	// A pointer to another object at a managed path is a conflict: reported,
	// never touched by hydrate or dehydrate.
	otherPtr, _ := filter.WholePointer(f.repo.Domain, ckDigest([]byte("elsewhere")), 9)
	f.git.WriteFile("b.bin", otherPtr.MustEncode())
	if st := f.statusOf("b.bin"); st.State != state.Conflict || !strings.Contains(st.Detail, otherPtr.OID) {
		t.Fatalf("conflict status %+v", st)
	}
	for _, op := range []func(context.Context, Selection, ProgressFunc) ([]PathOutcome, error){f.repo.Hydrate, f.repo.Dehydrate} {
		outs, _ := op(ctx, Selection{Paths: []string{"b.bin"}}, nil)
		if !outs[0].Skipped || !errors.Is(outs[0].Err, ErrConflict) || !bytes.Equal(f.read("b.bin"), otherPtr.MustEncode()) {
			t.Fatalf("conflict path touched: %+v", outs[0])
		}
	}
	// Publication flags after publishing.
	f.bindRemote()
	if _, failed, err := f.repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish %v %+v", err, failed)
	}
	if st := f.statusOf("assets/a.bin"); !st.Published || st.Pending {
		t.Fatalf("published status %+v", st)
	}
	// Once published the object is evictable; hydrate re-fetches from the
	// remote after eviction.
	if _, _, err := f.repo.CachePrune(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	if f.repo.Cache.Has(f.repo.Domain, ptr.OID) {
		t.Fatal("published object still cached after prune")
	}
	if outs, _ = f.repo.Dehydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, nil); outs[0].Skipped {
		t.Fatalf("dehydrate published %+v", outs[0])
	}
	// Dehydrate proves recoverability and may refill the cache while doing so;
	// evict again to exercise the single-item transfer path itself.
	if _, _, err := f.repo.CachePrune(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	var singleFetch []ItemProgress
	if outs, _ = f.repo.Hydrate(ctx, Selection{Paths: []string{"assets/a.bin"}}, func(p ItemProgress) {
		if p.Fetched {
			singleFetch = append(singleFetch, p)
		}
	}); outs[0].Skipped || !bytes.Equal(f.read("assets/a.bin"), asset) {
		t.Fatalf("hydrate from remote %+v", outs[0])
	}
	if len(singleFetch) == 0 || singleFetch[len(singleFetch)-1].Bytes != singleFetch[len(singleFetch)-1].Total {
		t.Fatalf("single-item hydrate did not report completed transfer progress: %+v", singleFetch)
	}
}

func TestHydrateFailsClosed(t *testing.T) {
	f := ckSetup(t)
	ctx := context.Background()
	asset := ckRandom(200000, 2)
	ptr := f.stage("a.bin", asset)
	f.git.Commit("a")
	ptrBytes := ptr.MustEncode()
	assertUntouched := func(step string) {
		t.Helper()
		if !bytes.Equal(f.read("a.bin"), ptrBytes) {
			t.Fatalf("%s: working file was modified", step)
		}
		if got := f.tmpLeftovers(); len(got) != 0 {
			t.Fatalf("%s: tmp leftovers %v", step, got)
		}
	}
	hydrate := func() PathOutcome {
		outs, err := f.repo.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return outs[0]
	}
	// Corrupt (bit flip, same size).
	corrupt := append([]byte(nil), asset...)
	corrupt[100] ^= 0xff
	overwriteCached(t, f.repo.Cache, f.repo.Domain, ptr.OID, corrupt)
	if o := hydrate(); !o.Skipped || !errors.Is(o.Err, ErrObjectUnavailable) && !errors.Is(o.Err, ErrIntegrity) {
		t.Fatalf("corrupt: %+v", o)
	}
	assertUntouched("corrupt")
	// The damaged object may still be on disk: it is only replaced once a
	// good copy has actually arrived, so a repository with nowhere to fetch
	// from keeps the bytes it has rather than being left with none. What must
	// never happen is that it is treated as valid.
	if err := f.repo.Cache.Verify(f.repo.Domain, ptr.OID); err == nil {
		t.Fatal("corrupt cached object was kept as valid")
	}
	// Truncated.
	_, _ = f.repo.Cache.Put(f.repo.Domain, bytes.NewReader(asset))
	overwriteCached(t, f.repo.Cache, f.repo.Domain, ptr.OID, asset[:len(asset)/2])
	if o := hydrate(); !o.Skipped {
		t.Fatalf("truncated: %+v", o)
	}
	assertUntouched("truncated")
	// Oversized.
	_, _ = f.repo.Cache.Put(f.repo.Domain, bytes.NewReader(asset))
	overwriteCached(t, f.repo.Cache, f.repo.Domain, ptr.OID, append(append([]byte(nil), asset...), asset...))
	if o := hydrate(); !o.Skipped {
		t.Fatalf("oversized: %+v", o)
	}
	assertUntouched("oversized")
	// Missing everywhere (no remote bound).
	_ = f.repo.Cache.Remove(f.repo.Domain, ptr.OID, true)
	if o := hydrate(); !o.Skipped || !errors.Is(o.Err, ErrObjectUnavailable) {
		t.Fatalf("missing: %+v", o)
	}
	assertUntouched("missing")
	// Interrupted write: the copy into the temporary object fails midway.
	_, _ = f.repo.Cache.Put(f.repo.Domain, bytes.NewReader(asset))
	_ = f.repo.Cache.Pin(f.repo.Domain, ptr.OID)
	hydrateWriteHook = func(_ string, w io.Writer) io.Writer { return &failingWriter{w: w, after: 4096} }
	t.Cleanup(func() { hydrateWriteHook = nil })
	if o := hydrate(); !o.Skipped || o.Err == nil {
		t.Fatalf("interrupted: %+v", o)
	}
	assertUntouched("interrupted")
	hydrateWriteHook = nil
	// Journal shows aborted operations, none interrupted.
	db, _ := f.repo.State(ctx)
	if open, _ := db.ListInterrupted(ctx); len(open) != 0 {
		t.Fatalf("interrupted ops left: %+v", open)
	}
	// Foreign-domain and malformed pointers in the index fail closed too.
	foreign, _ := filter.WholePointer("domain_other_01", ptr.OID, ptr.Size)
	f.git.WriteFile("f.bin", foreign.MustEncode())
	f.git.WriteFile("m.bin", []byte(pointer.VersionLine+"\ndomain domain_test_01\n"))
	f.git.Git("add", "f.bin", "m.bin")
	outs, _ := f.repo.Hydrate(ctx, Selection{Paths: []string{"f.bin", "m.bin"}}, nil)
	for _, o := range outs {
		if !o.Skipped || !errors.Is(o.Err, ErrIntegrity) {
			t.Fatalf("%s: %+v", o.Path, o)
		}
	}
	// A good hydrate still works afterwards.
	if o := hydrate(); o.Skipped || !bytes.Equal(f.read("a.bin"), asset) {
		t.Fatalf("recovery: %+v", o)
	}
}

type failingWriter struct {
	w     io.Writer
	after int
	n     int
}

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.n+len(b) > f.after {
		return 0, errors.New("injected write failure")
	}
	f.n += len(b)
	return f.w.Write(b)
}

func TestHydrateAbortsWhenPremiseMoves(t *testing.T) {
	f := ckSetup(t)
	ctx := context.Background()
	asset := ckRandom(50000, 3)
	ptr := f.stage("a.bin", asset)
	f.git.Commit("a")
	// Index moves between fingerprinting and rename.
	otherPtr, _ := filter.WholePointer(f.repo.Domain, ckDigest([]byte("other")), 5)
	hydrateBeforeRecheck = func(path string) {
		oid, err := f.repo.Git.HashObject(ctx, bytes.NewReader(otherPtr.MustEncode()), true)
		if err != nil {
			t.Fatal(err)
		}
		f.git.Git("update-index", "--cacheinfo", "100644,"+oid+","+path)
	}
	t.Cleanup(func() { hydrateBeforeRecheck = nil })
	outs, _ := f.repo.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
	if !outs[0].Skipped || !errors.Is(outs[0].Err, ErrConflict) || !strings.Contains(outs[0].Reason, "index entry changed") {
		t.Fatalf("index moved: %+v", outs[0])
	}
	if !bytes.Equal(f.read("a.bin"), ptr.MustEncode()) || len(f.tmpLeftovers()) != 0 {
		t.Fatal("working file or tmp touched after index-moved abort")
	}
	// Restore the index and make the working file move instead.
	f.git.Git("checkout", "HEAD", "--", "a.bin")
	f.git.WriteFile("a.bin", ptr.MustEncode())
	hydrateBeforeRecheck = func(path string) {
		p := filepath.Join(f.git.Root, path)
		if err := os.WriteFile(p, []byte("user wrote here"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outs, _ = f.repo.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
	if !outs[0].Skipped || !errors.Is(outs[0].Err, ErrConflict) || !strings.Contains(outs[0].Reason, "working file changed") {
		t.Fatalf("file moved: %+v", outs[0])
	}
	if string(f.read("a.bin")) != "user wrote here" {
		t.Fatal("concurrent user write was overwritten")
	}
	hydrateBeforeRecheck = nil
	db, _ := f.repo.State(ctx)
	if open, _ := db.ListInterrupted(ctx); len(open) != 0 {
		t.Fatalf("interrupted ops left: %+v", open)
	}
}

func TestVerifyReportsIntegrityAndAvailability(t *testing.T) {
	t.Parallel()
	f := ckSetup(t)
	ctx := context.Background()
	asset := ckRandom(70000, 4)
	ptr := f.stage("a.bin", asset)
	f.stage("b.bin", []byte("bee"))
	f.git.Commit("a")
	checked, issues, err := f.repo.Verify(ctx, Selection{}, VerifyOptions{})
	if err != nil || checked != 2 || len(issues) != 0 {
		t.Fatalf("clean verify: %d %+v %v", checked, issues, err)
	}
	// Remote check without a bound remote: storage issue.
	_, issues, _ = f.repo.Verify(ctx, Selection{}, VerifyOptions{Remote: true})
	if len(issues) != 2 || issues[0].Kind != IssueStorage {
		t.Fatalf("unbound remote: %+v", issues)
	}
	f.bindRemote()
	_, issues, _ = f.repo.Verify(ctx, Selection{}, VerifyOptions{Remote: true})
	if len(issues) != 2 || issues[0].Kind != IssueNotFound {
		t.Fatalf("unpublished: %+v", issues)
	}
	if _, _, err := f.repo.ObjectsPublish(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, issues, _ = f.repo.Verify(ctx, Selection{}, VerifyOptions{Remote: true})
	if len(issues) != 0 {
		t.Fatalf("published: %+v", issues)
	}
	// Corrupt cache object.
	bad := append([]byte(nil), asset...)
	bad[0] ^= 1
	overwriteCached(t, f.repo.Cache, f.repo.Domain, ptr.OID, bad)
	_, issues, _ = f.repo.Verify(ctx, Selection{Paths: []string{"a.bin"}}, VerifyOptions{})
	if len(issues) != 1 || issues[0].Kind != IssueIntegrity {
		t.Fatalf("corrupt: %+v", issues)
	}
	// Truncated cache object.
	overwriteCached(t, f.repo.Cache, f.repo.Domain, ptr.OID, asset[:10])
	_, issues, _ = f.repo.Verify(ctx, Selection{Paths: []string{"a.bin"}}, VerifyOptions{})
	if len(issues) != 1 || issues[0].Kind != IssueIntegrity || !strings.Contains(issues[0].Message, "size") {
		t.Fatalf("truncated: %+v", issues)
	}
	// Malformed pointer in the index.
	f.git.WriteFile("m.bin", []byte(pointer.VersionLine+"\ndomain nope\n"))
	f.git.Git("add", "m.bin")
	_, issues, _ = f.repo.Verify(ctx, Selection{Paths: []string{"m.bin"}}, VerifyOptions{})
	if len(issues) != 1 || issues[0].Kind != IssueIntegrity {
		t.Fatalf("malformed: %+v", issues)
	}
	// Evicted and unpublished elsewhere: not found.
	f.git.WriteFile("m.bin", ptr.MustEncode())
	f.git.Git("add", "m.bin")
	_ = f.repo.Cache.Remove(f.repo.Domain, ptr.OID, true)
	_ = os.RemoveAll(f.storeDir)
	_ = os.MkdirAll(f.storeDir, 0o755)
	_, issues, _ = f.repo.Verify(ctx, Selection{Paths: []string{"a.bin"}}, VerifyOptions{Remote: true})
	found := false
	for _, is := range issues {
		if is.Kind == IssueNotFound {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing on remote not reported: %+v", issues)
	}
}

func TestDoctorDetectsAndRepairFixesOnlySafeState(t *testing.T) {
	t.Parallel()
	f := ckSetup(t)
	ctx := context.Background()
	launcher := filepath.Join(t.TempDir(), "fxvcs")
	opts := DoctorOptions{Launcher: launcher}
	dirtyAsset := ckRandom(3000, 5)
	f.stage("dirty.bin", dirtyAsset)
	stalePtr := f.stage("stale.bin", []byte("stale"))
	missingPtr := f.stage("missing.bin", []byte("missing"))
	unrecPtr := f.stage("unrecorded.bin", []byte("unrecorded"))
	f.stage("clean.bin", []byte("clean"))
	f.git.Commit("all")

	// Baseline: git config and hook are absent -> git-config findings only
	// (plus nothing else because every path is an unhydrated pointer).
	findings, err := f.repo.Doctor(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range findings {
		if fd.Kind != FindingGitConfig {
			t.Fatalf("unexpected baseline finding %+v", fd)
		}
	}
	if len(findings) != len(filter.GitConfigEntries(launcher))+1 {
		t.Fatalf("baseline findings %+v", findings)
	}
	// Doctor is read-only: no state.db was created.
	if _, err := os.Stat(f.repo.Paths.StateDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created state.db: %v", err)
	}

	// Build the disagreement classes.
	if outs, _ := f.repo.Hydrate(ctx, Selection{Paths: []string{"dirty.bin", "stale.bin"}}, nil); outs[0].Skipped || outs[1].Skipped {
		t.Fatalf("hydrate %+v", outs)
	}
	f.git.WriteFile("dirty.bin", append(dirtyAsset, 'X')) // hydrated-modified
	db, _ := f.repo.State(ctx)
	row, _, _ := db.Get(ctx, "stale.bin")
	row.IndexPointerDigest = "sha256:" + strings.Repeat("0", 64) // stale row
	if err := db.Upsert(ctx, row); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(f.git.Root, "missing.bin")) // missing
	// Interrupted operations: one safe (nothing moved), one keep-dirty.
	safeFP, _ := state.StatFingerprint(filepath.Join(f.git.Root, "clean.bin"))
	cleanOID, _ := f.repo.indexBlobOID(ctx, "clean.bin")
	if _, err := db.BeginOperation(ctx, state.OpHydrate, "clean.bin", state.Expectation{IndexOID: cleanOID, Fingerprint: safeFP}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginOperation(ctx, state.OpHydrate, "dirty.bin", state.Expectation{IndexOID: "deadbeef", Fingerprint: state.Fingerprint{Size: 1}}); err != nil {
		t.Fatal(err)
	}
	// Cached + pinned object referenced by the index but absent from the ledger.
	l, _ := f.repo.Ledger(ctx)
	if _, err := l.SQL().ExecContext(ctx, "DELETE FROM objects WHERE digest = ?", unrecPtr.OID); err != nil {
		t.Fatal(err)
	}
	// Ledger pending row without cached bytes.
	if err := l.RecordIngest(ctx, state.ObjectRecord{Digest: "sha256:" + strings.Repeat("ab", 32), StorageDomainID: f.repo.Domain, Size: 1, Encoding: "whole/1"}); err != nil {
		t.Fatal(err)
	}
	// Stale row for a path no longer in the index.
	if err := db.Upsert(ctx, state.PathRow{Path: "gone.bin", Intent: state.IntentUnhydrated, IndexPointerDigest: stalePtr.OID}); err != nil {
		t.Fatal(err)
	}
	// Foreign pre-push hook.
	hooks, _ := f.repo.Git.GitPath(ctx, "hooks")
	_ = os.MkdirAll(hooks, 0o755)
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	findings, err = f.repo.Doctor(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	has := func(kind, path, substr string, repairable bool) bool {
		for _, fd := range findings {
			if fd.Kind == kind && fd.Path == path && strings.Contains(fd.Message, substr) && fd.Repairable == repairable {
				return true
			}
		}
		return false
	}
	checks := []struct {
		kind, path, substr string
		repairable         bool
	}{
		{FindingIndexWorktree, "dirty.bin", "hydrated-modified", false},
		{FindingStateDerived, "stale.bin", "records index pointer", true},
		{FindingIndexWorktree, "missing.bin", "missing", true},
		{FindingStateDerived, "clean.bin", "interrupted hydrate", true},
		{FindingStateDerived, "dirty.bin", "keep-dirty", false},
		{FindingPublication, "unrecorded.bin", "no publication ledger row", true},
		{FindingPublication, "", "pending publication but its bytes are not", false},
		{FindingStateDerived, "gone.bin", "no longer a managed index entry", true},
		{FindingGitConfig, "", "filter.fxvcs.process", true},
		{FindingGitConfig, "", "not managed by fxvcs", false},
	}
	for _, c := range checks {
		if !has(c.kind, c.path, c.substr, c.repairable) {
			t.Errorf("missing finding %+v in:\n%s", c, dumpFindings(findings))
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	rep, err := f.repo.Repair(ctx, opts)
	if err != nil {
		t.Fatalf("repair: %v %+v", err, rep)
	}
	refusedPaths := map[string]bool{}
	for _, fd := range rep.Refused {
		refusedPaths[fd.Path] = true
	}
	if !refusedPaths["dirty.bin"] {
		t.Fatalf("dirty file not refused: %+v", rep.Refused)
	}
	if !bytes.Equal(f.read("dirty.bin"), append(dirtyAsset, 'X')) {
		t.Fatal("repair overwrote the dirty file")
	}
	joined := strings.Join(rep.Rebuilt, "\n")
	for _, want := range []string{"row(s) rebuilt", "journal: hydrate clean.bin resolved", "pointer rewritten for missing file missing.bin", unrecPtr.OID + " recorded as pending", "git config:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rebuilt lacks %q:\n%s", want, joined)
		}
	}
	// Foreign hook is refused, not overwritten.
	if b, _ := os.ReadFile(filepath.Join(hooks, "pre-push")); strings.Contains(string(b), filter.HookMarker) {
		t.Fatal("foreign hook was overwritten")
	}
	// After repair only manual findings remain: the dirty file, its
	// keep-dirty journal row, the orphan ledger row, and the foreign hook.
	findings, _ = f.repo.Doctor(ctx, opts)
	for _, fd := range findings {
		if fd.Repairable {
			t.Errorf("repairable finding survived repair: %+v", fd)
		}
	}
	// keep-dirty journal row is still open; the safe one is closed.
	open, _ := db.ListInterrupted(ctx)
	if len(open) != 1 || open[0].Path != "dirty.bin" {
		t.Fatalf("interrupted after repair: %+v", open)
	}
	// Missing pointer materialized, ledger row present, config installed.
	if !bytes.Equal(f.read("missing.bin"), missingPtr.MustEncode()) {
		t.Fatal("missing pointer not rewritten")
	}
	if _, ok, _ := l.Object(ctx, unrecPtr.OID); !ok {
		t.Fatal("ledger row not recorded")
	}
	if v, ok, _ := f.repo.Git.ConfigGet(ctx, "filter.fxvcs.process"); !ok || !strings.Contains(v, "filter-process") {
		t.Fatalf("git config not repaired: %q %v", v, ok)
	}
	// Replace the foreign hook by nothing: repair installs the managed hook.
	_ = os.Remove(filepath.Join(hooks, "pre-push"))
	rep, err = f.repo.Repair(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(hooks, "pre-push")); err != nil || !strings.Contains(string(b), filter.HookMarker) {
		t.Fatalf("hook not installed: %v %+v", err, rep)
	}
	// Corrupt state.db: doctor reports rebuild guidance, repair rebuilds.
	_ = f.repo.Close()
	f.repo = f.open("test")
	if err := os.WriteFile(f.repo.Paths.StateDB, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(f.repo.Paths.StateDB + sfx)
	}
	findings, err = f.repo.Doctor(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	corruptSeen := false
	for _, fd := range findings {
		if fd.Kind == FindingStateDerived && strings.Contains(fd.Message, "state.db") && fd.Repairable {
			corruptSeen = true
		}
	}
	if !corruptSeen {
		t.Fatalf("corrupt state.db not reported:\n%s", dumpFindings(findings))
	}
	if rep, err := f.repo.Repair(ctx, opts); err != nil || !strings.Contains(strings.Join(rep.Rebuilt, "\n"), "corrupt database removed") {
		t.Fatalf("repair corrupt: %v %+v", err, rep)
	}
	if !bytes.Equal(f.read("dirty.bin"), append(dirtyAsset, 'X')) {
		t.Fatal("repair of corrupt state.db touched the dirty file")
	}
}

func dumpFindings(fs []Finding) string {
	var b strings.Builder
	for _, fd := range fs {
		b.WriteString(fd.Kind + " " + fd.Path + " repairable=" + map[bool]string{true: "y", false: "n"}[fd.Repairable] + ": " + fd.Message + "\n")
	}
	return b.String()
}

// TestLockHolderHelper is not a test: re-executed as a helper process it
// takes the path lock for a.bin in the given repository and blocks until it is
// killed, standing in for a crashed operation.
func TestLockHolderHelper(t *testing.T) {
	gitDir := os.Getenv("FXVCS_TEST_LOCK_GITDIR")
	if gitDir == "" {
		t.Skip("helper only")
	}
	set := lock.NewSet(filepath.Join(gitDir, "fxvcs", "locks"), "crashed-hydrate")
	h, err := set.Acquire(context.Background(), journal.LockPath, "a.bin", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	_, _ = os.Stdout.WriteString("locked\n")
	time.Sleep(time.Hour)
}

func TestConcurrentHydrateSerializesOnPathLock(t *testing.T) {
	t.Parallel()
	f := ckSetup(t)
	ctx := context.Background()
	asset := ckRandom(20000, 6)
	ptr := f.stage("a.bin", asset)
	f.git.Commit("a")

	// In-process second engine holds the path lock: the first gets busy with
	// owner metadata (LockTimeout 0).
	other := f.open("holder")
	h, err := other.Lock(ctx, journal.LockPath, "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	outs, err := f.repo.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var be *lock.BusyError
	if !outs[0].Skipped || !errors.As(outs[0].Err, &be) || be.Owner == nil || be.Owner.Operation != "holder" {
		t.Fatalf("busy expected: %+v", outs[0])
	}
	if !bytes.Equal(f.read("a.bin"), ptr.MustEncode()) {
		t.Fatal("file touched while busy")
	}
	// With a timeout the operation waits for the holder to release.
	waiter, err := Open(ctx, f.git.Root, Options{Installed: format.MustParseVersion("0.0.0-dev"), Operation: "waiter", CacheDir: f.cacheDir, LockTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Git = waiter.Git.WithEnv(f.git.Env)
	defer waiter.Close()
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = h.Release()
	}()
	outs, err = waiter.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
	if err != nil || outs[0].Skipped {
		t.Fatalf("waiter: %v %+v", err, outs)
	}
	if !bytes.Equal(f.read("a.bin"), asset) {
		t.Fatal("waiter did not hydrate")
	}
	// Two goroutines racing on the same path: exactly one hydrates, the
	// other is busy or finds it already hydrated; the file is never partial.
	if outs, _ := waiter.Dehydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil); outs[0].Skipped {
		t.Fatalf("dehydrate %+v", outs[0])
	}
	a := f.open("racer-a")
	b := f.open("racer-b")
	results := make(chan PathOutcome, 2)
	for _, r := range []*Repo{a, b} {
		go func(r *Repo) {
			outs, _ := r.Hydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil)
			results <- outs[0]
		}(r)
	}
	ra, rb := <-results, <-results
	if ra.Skipped && rb.Skipped {
		if !errors.As(ra.Err, &be) && !errors.As(rb.Err, &be) && ra.Reason != "already hydrated" && rb.Reason != "already hydrated" {
			t.Fatalf("both racers refused: %+v %+v", ra, rb)
		}
	}
	if !bytes.Equal(f.read("a.bin"), asset) {
		t.Fatal("racers left a non-hydrated file")
	}

	// A crashed holder (killed helper process) does not wedge the next run:
	// the OS releases the lock with the process.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLockHolderHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "FXVCS_TEST_LOCK_GITDIR="+f.repo.GitDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	deadline := time.Now().Add(20 * time.Second)
	got := ""
	for !strings.Contains(got, "locked") && time.Now().Before(deadline) {
		n, rerr := stdout.Read(buf)
		got += string(buf[:n])
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(got, "locked") {
		_ = cmd.Process.Kill()
		t.Fatalf("helper did not lock: %q", got)
	}
	if outs, _ := waiter.Dehydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil); !outs[0].Skipped || !errors.As(outs[0].Err, &be) || be.Owner == nil || be.Owner.Operation != "crashed-hydrate" {
		t.Fatalf("expected busy from helper: %+v", outs[0])
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if outs, _ := waiter.Dehydrate(ctx, Selection{Paths: []string{"a.bin"}}, nil); outs[0].Skipped {
		t.Fatalf("lock wedged after crash: %+v", outs[0])
	}
}
