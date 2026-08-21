package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"fxvcs.dev/fxvcs/internal/chunk"
)

func TestCacheLimitTrimsAfterIngestButNeverPinnedObjects(t *testing.T) {
	t.Setenv(chunk.EnvPolicy, "off") // one object per file keeps the arithmetic plain
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	const size = 256 << 10
	if err := repo.SetCacheLimit(ctx, 2*size); err != nil {
		t.Fatal(err)
	}

	// Three assets, all published (so nothing is pinned): the cache must trim
	// back to the limit, evicting the least recently used.
	for i := 0; i < 3; i++ {
		if _, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(100+i), size)), "assets/a.bin"); err != nil {
			t.Fatal(err)
		}
		if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
			t.Fatalf("publish: %v %v", failed, err)
		}
	}
	// The last ingest happens after the previous ones were published, so the
	// trim can act; run one more to make the budget bite.
	if _, err := repo.Ingest(ctx, bytes.NewReader(asset(200, size)), "assets/a.bin"); err != nil {
		t.Fatal(err)
	}
	total, err := repo.Cache.TotalBytes()
	if err != nil {
		t.Fatal(err)
	}
	// The freshly ingested object is pinned (not yet published) and survives
	// regardless; everything else must fit the budget.
	if total > 3*size {
		t.Fatalf("cache holds %d bytes with a %d-byte limit and one pinned object", total, 2*size)
	}

	// With unpublished (pinned) objects only, the limit cannot be honoured and
	// must not be enforced by deleting them.
	fresh, freshCache := testRepo(t)
	repo2 := openTest(t, fresh, freshCache)
	if err := repo2.SetCacheLimit(ctx, size); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := repo2.Ingest(ctx, bytes.NewReader(asset(uint64(300+i), size)), "assets/a.bin"); err != nil {
			t.Fatal(err)
		}
	}
	total2, err := repo2.Cache.TotalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if total2 < 4*size {
		t.Fatalf("pinned objects were evicted to honour the limit: %d bytes remain of %d", total2, 4*size)
	}
}

func TestCacheLimitIsClonseLocalAndRoundTrips(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if got := repo.CacheLimit(); got != 0 {
		t.Fatalf("default limit %d, want none", got)
	}
	if err := repo.SetCacheLimit(ctx, 50<<30); err != nil {
		t.Fatal(err)
	}
	if got := repo.CacheLimit(); got != 50<<30 {
		t.Fatalf("limit %d", got)
	}
	// It lives in clone-local configuration, never in the committed file.
	cfg, err := os.ReadFile(repo.Paths.ConfigYAML) //nolint:gosec // test-owned temp tree
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cfg, []byte("limitBytes")) {
		t.Fatalf("limit not recorded locally:\n%s", cfg)
	}
	committed, err := os.ReadFile(repo.Root + "/.fxvcs/repository.yaml") //nolint:gosec // test-owned temp tree
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(committed, []byte("limitBytes")) {
		t.Fatal("a machine's disk budget leaked into committed configuration")
	}
	if err := repo.SetCacheLimit(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := repo.CacheLimit(); got != 0 {
		t.Fatalf("limit %d after clearing", got)
	}
}

func TestCacheVerifyFindsSilentCorruption(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	published, err := repo.Ingest(ctx, bytes.NewReader(asset(1, 3<<20)), "assets/published.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	pending, err := repo.Ingest(ctx, bytes.NewReader(asset(2, 3<<20)), "assets/pending.bin")
	if err != nil {
		t.Fatal(err)
	}

	// A clean cache verifies.
	var verifyProgress []CacheVerifyProgress
	rep, err := repo.CacheVerify(ctx, CacheVerifyOptions{}, func(p CacheVerifyProgress) {
		verifyProgress = append(verifyProgress, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked == 0 || len(rep.Corrupt) != 0 {
		t.Fatalf("clean cache reported %d corrupt of %d checked", len(rep.Corrupt), rep.Checked)
	}
	if len(verifyProgress) != 1+2*rep.Checked || verifyProgress[0].Digest != "" || verifyProgress[0].Total != rep.Checked {
		t.Fatalf("verify progress framing = %+v, checked %d", verifyProgress, rep.Checked)
	}
	for i := 0; i < rep.Checked; i++ {
		started, finished := verifyProgress[1+2*i], verifyProgress[2+2*i]
		if !started.Started || started.Digest == "" || finished.Started || finished.Digest != started.Digest || finished.Done != i+1 {
			t.Fatalf("verify progress pair %d = start %+v finish %+v", i, started, finished)
		}
	}

	// Rot one chunk of each asset behind the cache's back.
	rotten := published.Manifest.Chunks[0]
	rottenPending := pending.Manifest.Chunks[0]
	for _, c := range []struct {
		digest string
		size   int64
	}{{rotten.Digest, rotten.RawSize}, {rottenPending.Digest, rottenPending.RawSize}} {
		writeRawCacheObject(t, repo, c.digest, bytes.Repeat([]byte{0x00}, int(c.size)))
	}

	rep, err = repo.CacheVerify(ctx, CacheVerifyOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Corrupt) != 2 {
		t.Fatalf("found %d corrupt objects, want 2: %+v", len(rep.Corrupt), rep.Corrupt)
	}

	// Repair drops the published copy (it can be fetched again) and keeps the
	// pending one (its bytes are the only copy of unpublished work).
	rep, err = repo.CacheVerify(ctx, CacheVerifyOptions{Repair: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Removed != 1 || len(rep.Kept) != 1 || !rep.Kept[0].Pinned {
		t.Fatalf("repair removed %d and kept %+v", rep.Removed, rep.Kept)
	}
	if repo.Cache.Has(repo.Domain, rotten.Digest) {
		t.Fatal("the corrupt published object was not removed")
	}
	if !repo.Cache.Has(repo.Domain, rottenPending.Digest) {
		t.Fatal("the corrupt pending object was removed; its bytes were the only copy")
	}
	// The published asset heals: its chunk comes back from the remote.
	if _, err := repo.EnsureContent(ctx, published.Pointer, nil); err != nil {
		t.Fatalf("published asset did not heal after repair: %v", err)
	}
	rc, err := repo.OpenContent(ctx, published.Pointer)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("healed asset does not reconstruct: %v", err)
	}
}
