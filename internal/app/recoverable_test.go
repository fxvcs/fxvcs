package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// setupUnloadable builds a repository with one hydrated, published chunked
// asset and returns the repo, the content, and the asset's path.
func setupUnloadable(t *testing.T, size int) (*Repo, []byte, string) {
	t.Helper()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	content := asset(91, size)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	r.WriteFile("big.bin", ing.Pointer.MustEncode())
	r.Git("add", ".")
	r.Commit("asset")
	r.WriteFile("big.bin", content)
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	return repo, content, "big.bin"
}

// TestUnloadRepairsADamagedCacheEntryFromTheWorkingFile is the regression for
// a silent way to lose data.
//
// Unload replaces the working file with a pointer, so its precondition is the
// last chance to notice that the copy standing in for those bytes is not
// them. Checking the cached object was not enough on its own: storing an
// object whose key already exists is a no-op, because objects are immutable —
// so a damaged entry could be detected and then left exactly as it was, and
// the working file destroyed against it anyway. Proving the working file
// means the damaged entry gets replaced with the bytes that are still there.
func TestUnloadRepairsADamagedCacheEntryFromTheWorkingFile(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()

	ptr := indexPointerFor(t, repo, path)
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	var victim ContentObject
	for _, o := range c.Objects {
		if o.Kind != ObjectKindManifest {
			victim = o
			break
		}
	}
	if victim.Digest == "" {
		t.Fatal("no chunk to damage")
	}
	// Damage it the way a torn write or a rolled-back snapshot would: same
	// key, same length, different bytes. Drop the publication record too, so
	// the cache really is the only copy and there is nothing to refetch — the
	// case where a wrong answer costs the content.
	forgetPublication(t, repo, victim.Digest)
	writeRawCacheObject(t, repo, victim.Digest, bytes.Repeat([]byte{0xEE}, int(victim.Size)))
	if err := repo.Cache.Verify(repo.Domain, victim.Digest); err == nil {
		t.Fatal("test setup did not actually damage the object")
	}

	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Skipped {
		t.Fatalf("unload refused a file it could have made recoverable: %+v", outs)
	}
	// The whole point: the cache now holds the real bytes, so the content the
	// working file no longer has can still be produced.
	if err := repo.Cache.Verify(repo.Domain, victim.Digest); err != nil {
		t.Fatalf("unload destroyed the working file without repairing the cache: %v", err)
	}
	if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, path)) //nolint:gosec // inside t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("reloaded content differs: %d bytes vs %d", len(got), len(content))
	}
}

// TestUnloadRefusesAWorkingFileThatDoesNotMatchItsPointer keeps the other half
// of the promise: proving the file is not the same as trusting it.
func TestUnloadRefusesAWorkingFileThatDoesNotMatchItsPointer(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	modified := append([]byte(nil), content...)
	modified[len(modified)/2] ^= 0xFF
	if err := os.WriteFile(filepath.Join(repo.Root, path), modified, 0o600); err != nil {
		t.Fatal(err)
	}
	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || !outs[0].Skipped {
		t.Fatalf("unload replaced a modified working file: %+v", outs)
	}
	// The bytes are still there.
	got, err := os.ReadFile(filepath.Join(repo.Root, path)) //nolint:gosec // inside t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, modified) {
		t.Fatal("the modified working file was not left alone")
	}
}

// TestUnloadRestoresAnObjectMissingFromTheCache covers the other repair: the
// cached copy is gone entirely rather than damaged.
func TestUnloadRestoresAnObjectMissingFromTheCache(t *testing.T) {
	repo, _, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	var removed int
	for _, o := range c.Objects {
		if o.Kind == ObjectKindManifest {
			continue
		}
		if err := repo.Cache.Remove(repo.Domain, o.Digest, true); err != nil {
			t.Fatal(err)
		}
		removed++
		break
	}
	if removed == 0 {
		t.Fatal("nothing removed")
	}
	if outs, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil || len(outs) != 1 || outs[0].Skipped {
		t.Fatalf("unload refused: %+v %v", outs, err)
	}
	for _, o := range c.Objects {
		if !repo.Cache.Has(repo.Domain, o.Digest) {
			t.Fatalf("object %s is still missing after unload claimed the content was recoverable", o.Digest)
		}
	}
}

// TestCachePutVerifiedReplacesADamagedEntry is the unit-level regression for
// the same bug: Put treats an existing key as success without looking at what
// is stored there, which is correct for immutable content and wrong for
// repair.
func TestCachePutVerifiedReplacesADamagedEntry(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	const domain = "domain_test_01"
	good := []byte("the real content")
	res, err := c.Put(domain, bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	// Damage it in place, keeping the same length so nothing but a read can
	// tell, and put the stored mode back afterwards.
	//
	// The mode matters: objects are stored 0444, and on Windows that is the
	// read-only attribute, which makes a replacing rename fail instead of
	// overwrite. A repair that reported success while leaving the damaged
	// bytes in place would be worse than one that admitted it could not.
	victim := cachePathFor(t, c, domain, res.Digest)
	if err := os.Chmod(victim, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("the fake content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(domain, res.Digest); err == nil {
		t.Fatal("setup did not damage the object")
	}
	// Put alone must not be expected to fix it — that is what PutVerified is
	// for, and the distinction is the point.
	if err := c.PutVerified(domain, res.Digest, good); err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(domain, res.Digest); err != nil {
		t.Fatalf("PutVerified left the damaged bytes in place: %v", err)
	}
	// It refuses content that is not what it claims.
	if err := c.PutVerified(domain, res.Digest, []byte("something else")); !errors.Is(err, cache.ErrCorrupt) {
		t.Fatalf("PutVerified stored mislabelled content: %v", err)
	}
}

func cachePathFor(t *testing.T, c *cache.Cache, domain, digest string) string {
	t.Helper()
	h := digest[len("sha256:"):]
	return filepath.Join(c.Root(), "objects", domain, h[:2], h[2:4], h)
}

// indexPointerFor returns the pointer the index records for a managed path.
func indexPointerFor(t *testing.T, repo *Repo, path string) pointer.Pointer {
	t.Helper()
	entries, _, err := repo.ManagedEntries(context.Background(), Selection{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].HasPointer {
		t.Fatalf("no pointer for %s: %+v", path, entries)
	}
	return entries[0].Pointer
}

// forgetPublication drops the ledger's record that a remote holds the object,
// modelling a remote that lost it or was never really written to.
func forgetPublication(t *testing.T, repo *Repo, digest string) {
	t.Helper()
	l, err := repo.Ledger(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.SQL().Exec("DELETE FROM remotes WHERE digest = ?", digest); err != nil {
		t.Fatal(err)
	}
}
