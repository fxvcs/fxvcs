package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// countingStore wraps a store and counts the requests a remote would serve.
// It deliberately does *not* implement BatchHaser, so the count reflects the
// worst case the engine can face.
type countingStore struct {
	inner objectstore.Store
	has   atomic.Int64
	gets  atomic.Int64
}

func (c *countingStore) Has(ctx context.Context, domain, digest string) (bool, error) {
	c.has.Add(1)
	return c.inner.Has(ctx, domain, digest)
}

func (c *countingStore) Put(ctx context.Context, domain, digest string, size int64, r io.Reader) error {
	return c.inner.Put(ctx, domain, digest, size, r)
}

func (c *countingStore) Get(ctx context.Context, domain, digest string) (io.ReadCloser, error) {
	c.gets.Add(1)
	return c.inner.Get(ctx, domain, digest)
}

// batchingStore is countingStore plus bulk existence, like the S3 backend.
type batchingStore struct {
	*countingStore
	batches atomic.Int64
}

func (b *batchingStore) BatchHas(ctx context.Context, domain string, digests []string) (map[string]bool, error) {
	b.batches.Add(1)
	out := make(map[string]bool, len(digests))
	for _, d := range digests {
		ok, err := b.inner.Has(ctx, domain, d)
		if err != nil {
			return nil, err
		}
		out[d] = ok
	}
	return out, nil
}

// TestPublicationCheckDoesNotScaleRoundTripsWithAssets is the regression for a
// push that spent a minute deciding what to upload before uploading anything.
//
// The pre-push barrier asks whether each asset is published. Asked one asset
// at a time against a remote with a 50 ms round trip, 200 assets cost ten
// seconds of nothing happening. The batched form must ask a number of times
// that does not grow with the number of assets.
func TestPublicationCheckDoesNotScaleRoundTripsWithAssets(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	const assets = 200
	ptrs := make([]pointer.Pointer, 0, assets)
	for i := 0; i < assets; i++ {
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(i), 8<<10)), "assets/a.bin")
		if err != nil {
			t.Fatal(err)
		}
		ptrs = append(ptrs, ing.Pointer)
	}

	// A backend without bulk existence: one query per object is the floor, but
	// the engine must not multiply that by asking again per asset.
	plain := &countingStore{inner: mustStore(t, repo, "primary")}
	repo.setStore("primary", plain)
	missing, err := repo.PublicationMissingMany(ctx, ptrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != assets {
		t.Fatalf("%d of %d assets reported unpublished", len(missing), assets)
	}
	if got := plain.has.Load(); got > assets {
		t.Fatalf("asked the remote %d times for %d assets; the check must not scale with assets", got, assets)
	}

	// A backend with bulk existence — what the S3 store provides — collapses
	// the whole question into one call per remote.
	batching := &batchingStore{countingStore: &countingStore{inner: mustStore(t, repo, "primary")}}
	repo.setStore("primary", batching)
	if _, err := repo.PublicationMissingMany(ctx, ptrs); err != nil {
		t.Fatal(err)
	}
	if got := batching.batches.Load(); got != 1 {
		t.Fatalf("%d bulk queries for %d assets, want exactly 1", got, assets)
	}
}

// TestPublicationCheckAgreesWithThePerPointerForm keeps the fast path honest:
// batching may not change any answer.
func TestPublicationCheckAgreesWithThePerPointerForm(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	var ptrs []pointer.Pointer
	for i := 0; i < 6; i++ {
		size := 8 << 10
		if i%2 == 0 {
			size = 3 << 20 // chunked: manifest plus chunks
		}
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(100+i), size)), "assets/a.bin")
		if err != nil {
			t.Fatal(err)
		}
		ptrs = append(ptrs, ing.Pointer)
	}
	compare := func(stage string) {
		many, err := repo.PublicationMissingMany(ctx, ptrs)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ptrs {
			one, err := repo.PublicationMissing(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			got := many[p.OID]
			if len(one) != len(got) {
				t.Fatalf("%s: %s: batched says %v, per-pointer says %v", stage, p.OID, got, one)
			}
			for i := range one {
				if one[i] != got[i] {
					t.Fatalf("%s: %s: batched says %v, per-pointer says %v", stage, p.OID, got, one)
				}
			}
		}
	}
	compare("before publishing")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	compare("after publishing")

	// A remote that already holds the objects but has no ledger record — a
	// colleague published them — is discovered and recorded, not re-uploaded.
	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	for _, p := range ptrs {
		if _, err := fresh.Ingest(ctx, bytes.NewReader(mustRead(t, repo, p)), "assets/a.bin"); err != nil {
			t.Fatal(err)
		}
	}
	missing, err := fresh.PublicationMissingMany(ctx, ptrs)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("objects already on the remote were reported as unpublished: %v", missing)
	}
}

func mustStore(t *testing.T, repo *Repo, name string) objectstore.Store {
	t.Helper()
	s, _, err := repo.Store(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustRead(t *testing.T, repo *Repo, p pointer.Pointer) []byte {
	t.Helper()
	rc, err := repo.OpenContent(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// slowStore adds latency to every Get and records how many were in flight at
// once, which is the difference between "downloads one file at a time" and
// "uses the link".
type slowStore struct {
	inner   objectstore.Store
	delay   time.Duration
	mu      sync.Mutex
	inFlmax int
	inFl    int
	gets    int
}

func (s *slowStore) Has(ctx context.Context, domain, digest string) (bool, error) {
	return s.inner.Has(ctx, domain, digest)
}

func (s *slowStore) Put(ctx context.Context, domain, digest string, size int64, r io.Reader) error {
	return s.inner.Put(ctx, domain, digest, size, r)
}

func (s *slowStore) Get(ctx context.Context, domain, digest string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.inFl++
	s.gets++
	if s.inFl > s.inFlmax {
		s.inFlmax = s.inFl
	}
	s.mu.Unlock()
	time.Sleep(s.delay)
	s.mu.Lock()
	s.inFl--
	s.mu.Unlock()
	return s.inner.Get(ctx, domain, digest)
}

// TestHydrateFetchesAcrossFilesConcurrently is the regression for a load that
// downloaded one file at a time.
//
// Materializing a file is network work (independent per file) followed by a
// compare-and-swap write (serialized per path). When both ran in one loop, a
// selection of N assets cost N round trips in series. The fetches must now
// overlap, while the writes stay one at a time.
func TestHydrateFetchesAcrossFilesConcurrently(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	const files = 12
	for i := 0; i < files; i++ {
		r.WriteFile(fmt.Sprintf("a%02d.bin", i), asset(uint64(500+i), 64<<10))
	}
	// Stage through the engine so the index holds pointers.
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("a%02d.bin", i)
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(500+i), 64<<10)), path)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(path, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}

	// A fresh cache, and a remote where every object costs 40 ms.
	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	slow := &slowStore{inner: mustStore(t, fresh, "primary"), delay: 40 * time.Millisecond}
	fresh.setStore("primary", slow)

	start := time.Now()
	outs, err := fresh.Hydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	loaded := 0
	for _, o := range outs {
		if !o.Skipped {
			loaded++
		}
	}
	if loaded != files {
		t.Fatalf("loaded %d of %d assets: %+v", loaded, files, outs)
	}
	t.Logf("elapsed=%s gets=%d maxInFlight=%d", elapsed.Round(time.Millisecond), slow.gets, slow.inFlmax)
	// The assertion is about overlap, not wall clock: the remaining time is
	// the per-path write, which is compare-and-swap against the index and is
	// meant to be serialized.
	if slow.inFlmax < 3 {
		t.Fatalf("downloads never overlapped (max %d in flight for %d files); hydrate is fetching one file at a time", slow.inFlmax, files)
	}
	if slow.gets != files {
		t.Fatalf("%d downloads for %d files; each object must be fetched once", slow.gets, files)
	}
}
