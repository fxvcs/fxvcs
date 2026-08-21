package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/format"
)

// TestLoadHealsACorruptCachedManifest: the manifest is an object like any
// other, and a damaged one must be re-fetched rather than bricking the asset.
func TestLoadHealsACorruptCachedManifest(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	if ptr.ManifestDigest == "" {
		t.Skip("not a chunked asset")
	}
	if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	// Same key, same length, different bytes — what bit rot looks like.
	corruptCacheObject(t, repo, ptr.ManifestDigest)
	outs, err := repo.Hydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Skipped {
		t.Fatalf("load did not heal a corrupt cached manifest: %+v", outs)
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, path)) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("healed load produced different content")
	}
}

// TestManifestHealKeepsThePin: evicting an object to replace it also removes
// its pending-publication marker, and an unpinned pending object is one the
// cache may prune while the ledger still says it is unpublished.
func TestManifestHealKeepsThePin(t *testing.T) {
	repo, _, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	if ptr.ManifestDigest == "" {
		t.Skip("not a chunked asset")
	}
	if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	// Model a manifest still pending publication.
	if err := repo.Cache.Pin(repo.Domain, ptr.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	corruptCacheObject(t, repo, ptr.ManifestDigest)
	if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	if pinned, err := repo.Cache.IsPinned(repo.Domain, ptr.ManifestDigest); err != nil || !pinned {
		t.Fatalf("healing the manifest dropped its pin (pinned=%v err=%v)", pinned, err)
	}
}

// TestVerifyReportsCorruptionInsteadOfRepairingIt: a check that quietly fixes
// what it finds and then reports the repository healthy has destroyed the
// evidence and told the operator nothing happened.
func TestVerifyReportsCorruptionInsteadOfRepairingIt(t *testing.T) {
	repo, _, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	if ptr.ManifestDigest == "" {
		t.Skip("not a chunked asset")
	}
	if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
		t.Fatal(err)
	}
	corruptCacheObject(t, repo, ptr.ManifestDigest)

	diag := openDiagnostic(t, repo)
	_, issues, err := diag.Verify(ctx, Selection{}, VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var integrity int
	for _, i := range issues {
		if i.Kind == IssueIntegrity {
			integrity++
		}
	}
	if integrity == 0 {
		t.Fatalf("verify reported no integrity issue for a corrupt manifest: %+v", issues)
	}
	if err := repo.Cache.Verify(repo.Domain, ptr.ManifestDigest); err == nil {
		t.Fatal("verify repaired the object it was asked to report on")
	}
}

// corruptCacheObject replaces an object's bytes in place, keeping its length
// and its stored read-only mode.
func corruptCacheObject(t *testing.T, repo *Repo, digest string) {
	t.Helper()
	p := cacheObjectPath(repo, digest)
	orig, err := os.ReadFile(p) //nolint:gosec // test fixture inside t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte{'x'}, len(orig)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
}

// openDiagnostic reopens the same checkout the way verify and doctor do.
func openDiagnostic(t *testing.T, repo *Repo) *Repo {
	t.Helper()
	d, err := Open(context.Background(), repo.Root, Options{
		Installed: format.MustParseVersion("0.0.0-dev"), Operation: "verify",
		CacheDir: repo.Cache.Root(), Diagnostic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Git = repo.Git
	t.Cleanup(func() { _ = d.Close() })
	return d
}
