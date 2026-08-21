package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

const repoYAML = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_test_01
  storageDomainID: domain_test_01
  name: test
spec:
  profile: generic
  minimumFXVCSVersion: 0.0.0-dev
  gitIntegrationVersion: 1
  storage:
    remotes:
      - name: primary
        type: filesystem
        requiredForPublication: true
`

func testRepo(t testing.TB) (*gittest.Repo, string) {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	r.WriteFile(".fxvcs/repository.yaml", []byte(repoYAML))
	r.Commit("init")
	cacheDir := t.TempDir()
	return r, cacheDir
}

func openTest(t testing.TB, r *gittest.Repo, cacheDir string) *Repo {
	t.Helper()
	repo, err := Open(context.Background(), r.Root, Options{Installed: format.MustParseVersion("0.0.0-dev"), Operation: "test", CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	repo.Git = repo.Git.WithEnv(r.Env)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestOpenPreflightAndPaths(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	if repo.Domain != "domain_test_01" || repo.Root == "" || repo.GitDir == "" {
		t.Fatalf("%+v", repo)
	}
	if filepath.Dir(repo.Paths.StateDB) != filepath.Join(repo.GitDir, "fxvcs") {
		t.Fatalf("state db path %s", repo.Paths.StateDB)
	}
	// Non-git directory with a config: typed error, but the version gate wins first.
	plain := t.TempDir()
	_ = os.MkdirAll(filepath.Join(plain, ".fxvcs"), 0o755)
	_ = os.WriteFile(filepath.Join(plain, ".fxvcs", "repository.yaml"), []byte(repoYAML), 0o644)
	if _, err := Open(context.Background(), plain, Options{Installed: format.MustParseVersion("0.0.0-dev")}); !errors.Is(err, ErrNotGitWorktree) {
		t.Fatalf("got %v", err)
	}
	var ur *format.UpgradeRequiredError
	if _, err := Open(context.Background(), plain, Options{Installed: format.MustParseVersion("0.0.0-a")}); !errors.As(err, &ur) {
		t.Fatalf("got %v", err)
	}
	if _, err := Open(context.Background(), t.TempDir(), Options{Installed: format.MustParseVersion("0.0.0-dev")}); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("got %v", err)
	}
}

func TestStorageBindingPublishAndPruneProtection(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()

	// Committed remote without a local binding is listed but unbound.
	rem := repo.Remotes()
	if len(rem) != 1 || rem[0].Name != "primary" || !rem[0].Committed || rem[0].Bound {
		t.Fatalf("%+v", rem)
	}
	if _, _, err := repo.Store("primary"); !errors.Is(err, ErrRemoteUnbound) {
		t.Fatalf("got %v", err)
	}
	// Adding a new non-local remote declares it in the committed config
	// (working tree only, uncommitted) and binds it locally.
	sharedDir := t.TempDir()
	sv, err := repo.StorageAdd(ctx, StorageAddInput{Name: "shared", Type: "filesystem", Path: sharedDir, RequiredForPublication: true})
	if err != nil || !sv.Committed || !sv.Bound || !sv.RequiredForPublication {
		t.Fatalf("%+v %v", sv, err)
	}
	cfgBytes, _ := os.ReadFile(filepath.Join(repo.Root, ".fxvcs", "repository.yaml"))
	if !bytes.Contains(cfgBytes, []byte("name: shared")) || !bytes.Contains(cfgBytes, []byte("apiVersion: fxvcs.dev/v1alpha1")) {
		t.Fatalf("committed config not updated:\n%s", cfgBytes)
	}
	if out := r.Git("status", "--porcelain"); !bytes.Contains([]byte(out), []byte("repository.yaml")) {
		t.Fatalf("expected an uncommitted working-tree change, got %q", out)
	}
	if out := r.Git("log", "--oneline"); len(bytes.Split(bytes.TrimSpace([]byte(out)), []byte("\n"))) != 1 {
		t.Fatalf("storage add must not commit: %s", out)
	}
	if got := repo.RequiredRemotes(); len(got) != 2 {
		t.Fatalf("required %v", got)
	}
	storeDir := t.TempDir()
	v, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: storeDir})
	if err != nil || !v.Bound || v.Local {
		t.Fatalf("%+v %v", v, err)
	}
	// Local-only mirror.
	mirror := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "mirror", Type: "filesystem", Path: mirror, Local: true}); err != nil {
		t.Fatal(err)
	}
	if got := repo.RequiredRemotes(); len(got) != 2 || got[0] != "primary" || got[1] != "shared" {
		t.Fatalf("required %v", got)
	}
	// Local config round-trips through a fresh Open.
	repo2 := openTest(t, r, cacheDir)
	if len(repo2.Remotes()) != 3 {
		t.Fatalf("local config not persisted: %+v", repo2.Remotes())
	}
	caps, err := repo2.StorageTest(ctx, "primary")
	if err != nil || len(caps) == 0 {
		t.Fatalf("%v %v", caps, err)
	}
	for _, c := range caps {
		if !c.Supported {
			t.Fatalf("capability %s unsupported: %s", c.Name, c.Detail)
		}
	}

	// Ingest an object into the cache + ledger as the filter would, pinned.
	payload := bytes.Repeat([]byte("Z"), 4096)
	res, err := repo2.Cache.Put(repo2.Domain, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo2.Cache.Pin(repo2.Domain, res.Digest); err != nil {
		t.Fatal(err)
	}
	l, err := repo2.Ledger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.RecordIngest(ctx, state.ObjectRecord{Digest: res.Digest, StorageDomainID: repo2.Domain, Size: res.Size, Encoding: "whole/1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.AddReferrer(ctx, res.Digest, "a.bin", "index"); err != nil {
		t.Fatal(err)
	}
	st, err := repo2.ObjectsStatus(ctx)
	if err != nil || len(st) != 1 || !st[0].Pending || st[0].Remotes["primary"] {
		t.Fatalf("%+v %v", st, err)
	}
	// Prune cannot evict the pinned object even with target 0.
	plan, _, err := repo2.CachePrune(ctx, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("pinned object in prune plan: %+v", plan)
	}
	// Publish to required remotes; object lands in the store, pin settles, ledger updated.
	pub, failed, err := repo2.ObjectsPublish(ctx, nil, nil)
	if err != nil || len(failed) != 0 || len(pub) != 2 || pub[0].Remote != "primary" || pub[1].Remote != "shared" || pub[0].Bytes != int64(len(payload)) {
		t.Fatalf("pub=%+v failed=%+v err=%v", pub, failed, err)
	}
	s, _, _ := repo2.Store("primary")
	if ok, _ := s.Has(ctx, repo2.Domain, res.Digest); !ok {
		t.Fatal("object not in remote store")
	}
	st, _ = repo2.ObjectsStatus(ctx)
	if st[0].Pending || !st[0].Remotes["primary"] {
		t.Fatalf("ledger not updated: %+v", st)
	}
	if pinned, _ := repo2.Cache.IsPinned(repo2.Domain, res.Digest); pinned {
		t.Fatal("pin not settled after publication")
	}
	// Now the object is evictable; prune it and refetch it from the remote.
	evicted, _, err := repo2.CachePrune(ctx, 0, false)
	if err != nil || len(evicted) != 1 {
		t.Fatalf("%+v %v", evicted, err)
	}
	if repo2.Cache.Has(repo2.Domain, res.Digest) {
		t.Fatal("still cached")
	}
	if err := repo2.FetchObject(ctx, res.Digest); err != nil {
		t.Fatal(err)
	}
	if err := repo2.Cache.Verify(repo2.Domain, res.Digest); err != nil {
		t.Fatal(err)
	}
	// Corrupt the remote copy: fetch must fail closed and leave nothing cached.
	_ = repo2.Cache.Remove(repo2.Domain, res.Digest, true)
	for _, root := range []string{storeDir, sharedDir} {
		objPath := filepath.Join(root, repo2.Domain, "objects", res.Digest[7:9], res.Digest[7:])
		_ = os.WriteFile(objPath, []byte("corrupt"), 0o644)
	}
	if err := repo2.FetchObject(ctx, res.Digest); err == nil {
		t.Fatal("corrupt remote object accepted")
	}
	if repo2.Cache.Has(repo2.Domain, res.Digest) {
		t.Fatal("corrupt object left in cache")
	}
	if err := repo2.FetchObject(ctx, "sha256:"+string(bytes.Repeat([]byte("0"), 64))); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	// Publishing again with nothing pending is a no-op.
	pub, failed, err = repo2.ObjectsPublish(ctx, nil, nil)
	if err != nil || len(pub) != 0 || len(failed) != 0 {
		t.Fatalf("%+v %+v %v", pub, failed, err)
	}
	// Publishing with an unbound remote name reports a failure, not a crash.
	_ = repo2.Cache.Put
	res2, _ := repo2.Cache.Put(repo2.Domain, bytes.NewReader([]byte("second")))
	_ = repo2.Cache.Pin(repo2.Domain, res2.Digest)
	_ = l.RecordIngest(ctx, state.ObjectRecord{Digest: res2.Digest, StorageDomainID: repo2.Domain, Size: res2.Size, Encoding: "whole/1"})
	// Every recorded object lacks a record on "ghost", so each reports a
	// typed failure rather than the operation aborting.
	_, failed, err = repo2.ObjectsPublish(ctx, []string{"ghost"}, nil)
	if err != nil || len(failed) != 2 {
		t.Fatalf("%+v %v", failed, err)
	}
	for _, f := range failed {
		if f.Remote != "ghost" || !errors.Is(f.Err, ErrNoSuchRemote) {
			t.Fatalf("unexpected failure %+v", f)
		}
	}
}

func TestIngestIsDeterministicAndIdempotent(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("Q"), 1<<16)
	a, err := repo.Ingest(ctx, bytes.NewReader(payload), "x.bin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := repo.Ingest(ctx, bytes.NewReader(payload), "y.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Pointer.MustEncode(), b.Pointer.MustEncode()) || !b.Existing || a.Existing {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if pinned, _ := repo.Cache.IsPinned(repo.Domain, a.Digest); !pinned {
		t.Fatal("not pinned")
	}
	if _, err := pointer.Decode(a.Pointer.MustEncode()); err != nil {
		t.Fatal(err)
	}
	l, _ := repo.Ledger(ctx)
	refs, _ := l.Referrers(ctx, a.Digest)
	if len(refs) != 2 {
		t.Fatalf("referrers %v", refs)
	}
}

// A remote added after objects were published (and settled, and evicted from
// the cache) still receives them on the next publish.
func TestPublishToRemoteAddedLater(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	primary := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: primary}); err != nil {
		t.Fatal(err)
	}
	emptySummary, err := repo.PublicationSummary(ctx)
	if err != nil || emptySummary.Objects != 0 || len(emptySummary.Remotes) != 1 || emptySummary.Remotes[0].Name != "primary" || emptySummary.Remotes[0].Bytes != 0 {
		t.Fatalf("summary without ledger: %+v %v", emptySummary, err)
	}
	ing, err := repo.Ingest(ctx, bytes.NewReader(bytes.Repeat([]byte("L"), 3000)), "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	summary, err := repo.PublicationSummary(ctx)
	if err != nil || summary.Objects != 1 || summary.Bytes != 3000 || summary.PendingObjects != 1 || summary.PendingBytes != 3000 {
		t.Fatalf("summary before publish: %+v %v", summary, err)
	}
	remoteUsage := func(summary PublicationSummary, name string) PublicationRemoteUsage {
		for _, usage := range summary.Remotes {
			if usage.Name == name {
				return usage
			}
		}
		return PublicationRemoteUsage{}
	}
	if usage := remoteUsage(summary, "primary"); usage.Name != "primary" || usage.Objects != 0 || usage.Bytes != 0 {
		t.Fatalf("primary before publish: %+v", summary.Remotes)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("%v %+v", err, failed)
	}
	summary, err = repo.PublicationSummary(ctx)
	if err != nil || summary.PendingObjects != 0 || summary.PendingBytes != 0 {
		t.Fatalf("summary after primary publish: %+v %v", summary, err)
	}
	if usage := remoteUsage(summary, "primary"); usage.Objects != 1 || usage.Bytes != 3000 {
		t.Fatalf("primary after publish: %+v", summary.Remotes)
	}
	// Settled: unpinned and evictable; evict it.
	if pinned, _ := repo.Cache.IsPinned(repo.Domain, ing.Digest); pinned {
		t.Fatal("still pinned after publish")
	}
	if _, _, err := repo.CachePrune(ctx, 0, false); err != nil {
		t.Fatal(err)
	}
	if repo.Cache.Has(repo.Domain, ing.Digest) {
		t.Fatal("not evicted")
	}
	// Add a second required remote (as `storage add` would declare + bind).
	backup := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "backup", Type: "filesystem", Path: backup, RequiredForPublication: true}); err != nil {
		t.Fatal(err)
	}
	summary, err = repo.PublicationSummary(ctx)
	if err != nil || summary.Objects != 1 || summary.Bytes != 3000 || summary.PendingObjects != 1 || summary.PendingBytes != 3000 {
		t.Fatalf("summary after adding required remote: %+v %v", summary, err)
	}
	if primary, backup := remoteUsage(summary, "primary"), remoteUsage(summary, "backup"); primary.Objects != 1 || primary.Bytes != 3000 || backup.Name != "backup" || backup.Objects != 0 || backup.Bytes != 0 {
		t.Fatalf("remote usage after adding backup: %+v", summary.Remotes)
	}
	st, _ := repo.ObjectsStatus(ctx)
	if len(st) != 1 || st[0].Remotes["backup"] {
		t.Fatalf("status before publish: %+v", st)
	}
	pub, failed, err := repo.ObjectsPublish(ctx, nil, nil)
	if err != nil || len(failed) != 0 || len(pub) != 1 || pub[0].Remote != "backup" {
		t.Fatalf("pub=%+v failed=%+v err=%v", pub, failed, err)
	}
	s, _, _ := repo.Store("backup")
	if ok, _ := s.Has(ctx, repo.Domain, ing.Digest); !ok {
		t.Fatal("object not uploaded to the remote added later")
	}
	st, _ = repo.ObjectsStatus(ctx)
	if !st[0].Remotes["backup"] || !st[0].Remotes["primary"] || st[0].Pending {
		t.Fatalf("status after publish: %+v", st)
	}
	summary, err = repo.PublicationSummary(ctx)
	if err != nil || summary.PendingObjects != 0 || summary.PendingBytes != 0 {
		t.Fatalf("summary after backup publish: %+v %v", summary, err)
	}
	if primary, backup := remoteUsage(summary, "primary"), remoteUsage(summary, "backup"); primary.Objects != 1 || primary.Bytes != 3000 || backup.Objects != 1 || backup.Bytes != 3000 {
		t.Fatalf("remote usage after backup publish: %+v", summary.Remotes)
	}
	// Idempotent.
	if pub, _, _ := repo.ObjectsPublish(ctx, nil, nil); len(pub) != 0 {
		t.Fatalf("re-published: %+v", pub)
	}
}
