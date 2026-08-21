package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// asset builds deterministic, mildly compressible content of n bytes.
func asset(seed uint64, n int) []byte {
	src := rand.New(rand.NewPCG(seed, 0xC0FFEE))
	b := make([]byte, n)
	motif := make([]byte, 64)
	fill := func() {
		for i := 0; i < len(motif); i += 8 {
			binary.LittleEndian.PutUint64(motif[i:], src.Uint64())
		}
	}
	fill()
	for off := 0; off < n; off += len(motif) {
		copy(b[off:], motif)
		if src.IntN(5) == 0 {
			fill()
		}
	}
	return b
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return cache.Digest(sum[:])
}

// readAll reconstructs a pointer's content through the content layer.
func readAll(t *testing.T, repo *Repo, ptr pointer.Pointer) []byte {
	t.Helper()
	rc, err := repo.OpenContent(context.Background(), ptr)
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

// TestIngestChunksLargeAssetsAndReconstructsExactly covers the shape of a
// chunked object: the pointer names a stored manifest, the manifest names the
// chunks, every object is cached and pinned, and reading it back reproduces
// the input byte for byte.
func TestIngestChunksLargeAssetsAndReconstructsExactly(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()

	content := asset(1, 6<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ing.Pointer.Encoding != chunk.EncodingFastCDCv1 {
		t.Fatalf("encoding %q, want %q", ing.Pointer.Encoding, chunk.EncodingFastCDCv1)
	}
	if ing.Digest != digestOf(content) || ing.Size != int64(len(content)) {
		t.Fatalf("pointer identity %s/%d does not describe the input", ing.Digest, ing.Size)
	}
	if len(ing.Manifest.Chunks) < 4 {
		t.Fatalf("a %d-byte asset produced %d chunk(s)", len(content), len(ing.Manifest.Chunks))
	}
	// The whole-file digest is *not* a stored object under a chunked encoding.
	if repo.Cache.Has(repo.Domain, ing.Digest) {
		t.Fatal("the whole file was stored as well as its chunks")
	}
	var total int64
	for _, o := range ing.Objects {
		if !repo.Cache.Has(repo.Domain, o.Digest) {
			t.Fatalf("object %s (%s) not cached", o.Digest, o.Kind)
		}
		if pinned, _ := repo.Cache.IsPinned(repo.Domain, o.Digest); !pinned {
			t.Fatalf("object %s (%s) not pinned pending publication", o.Digest, o.Kind)
		}
		if o.Kind == ObjectKindChunk {
			total += o.Size
		}
	}
	if total != int64(len(content)) {
		t.Fatalf("chunks hold %d bytes, asset is %d", total, len(content))
	}
	if ing.Objects[0].Kind != ObjectKindManifest || ing.Objects[0].Digest != ing.Pointer.ManifestDigest {
		t.Fatalf("first object should be the manifest named by the pointer: %+v", ing.Objects[0])
	}
	if got := readAll(t, repo, ing.Pointer); !bytes.Equal(got, content) {
		t.Fatalf("reconstruction differs from the input (%d vs %d bytes)", len(got), len(content))
	}
	// Every object is recorded in the shared ledger and attributed to the path.
	l, err := repo.Ledger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ing.Objects {
		rec, ok, err := l.Object(ctx, o.Digest)
		if err != nil || !ok || !rec.Pending || rec.Encoding != o.Kind {
			t.Fatalf("ledger row for %s: %+v %v %v", o.Digest, rec, ok, err)
		}
		refs, err := l.Referrers(ctx, o.Digest)
		if err != nil || len(refs) != 1 || refs[0][0] != "assets/big.bin" {
			t.Fatalf("referrers for %s: %v %v", o.Digest, refs, err)
		}
	}
}

// TestIngestIsIdempotentAndSmallAssetsStayWhole pins the encoding decision:
// below the policy floor nothing changes from Stage 1.
func TestIngestIsIdempotentAndSmallAssetsStayWhole(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	policy, err := repo.ChunkPolicy()
	if err != nil {
		t.Fatal(err)
	}

	small := asset(2, int(policy.MinChunkedSize)-1)
	ing, err := repo.Ingest(ctx, bytes.NewReader(small), "assets/small.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ing.Pointer.Encoding != pointer.EncodingWhole {
		t.Fatalf("a %d-byte asset was encoded as %q", len(small), ing.Pointer.Encoding)
	}
	if len(ing.Objects) != 1 || ing.Objects[0].Digest != ing.Pointer.OID {
		t.Fatalf("whole/1 should store exactly the file: %+v", ing.Objects)
	}
	// Exactly at the floor the asset is chunked.
	atFloor := asset(3, int(policy.MinChunkedSize))
	ing2, err := repo.Ingest(ctx, bytes.NewReader(atFloor), "assets/floor.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ing2.Pointer.Encoding != chunk.EncodingFastCDCv1 {
		t.Fatalf("an asset exactly at the floor was encoded as %q", ing2.Pointer.Encoding)
	}
	// Re-ingesting identical bytes yields an identical pointer and writes
	// nothing new.
	again, err := repo.Ingest(ctx, bytes.NewReader(atFloor), "assets/floor.bin")
	if err != nil {
		t.Fatal(err)
	}
	if again.Pointer != ing2.Pointer {
		t.Fatalf("re-ingest produced a different pointer:\n%+v\n%+v", again.Pointer, ing2.Pointer)
	}
	if !again.Existing {
		t.Fatal("re-ingesting identical bytes reported new objects")
	}
}

// TestChunkingUploadsOnlyChangedRegions is the Stage 2 exit criterion:
// changing a small region uploads only new chunks plus a new manifest.
func TestChunkingUploadsOnlyChangedRegions(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	v1 := asset(4, 8<<20)
	if _, err := repo.Ingest(ctx, bytes.NewReader(v1), "assets/big.bin"); err != nil {
		t.Fatal(err)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish v1: %v %v", failed, err)
	}
	// Edit 1 KiB in the middle; everything else is unchanged.
	v2 := bytes.Clone(v1)
	for i := 4 << 20; i < 4<<20+1024; i++ {
		v2[i] ^= 0x5A
	}
	ing2, err := repo.Ingest(ctx, bytes.NewReader(v2), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	published, failed, err := repo.ObjectsPublish(ctx, nil, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("publish v2: %v %v", failed, err)
	}
	var uploaded int64
	for _, p := range published {
		uploaded += p.Bytes
	}
	// A new manifest is unavoidable; the chunk bytes must be a small
	// fraction of the file.
	if uploaded > int64(len(v2))/4 {
		t.Fatalf("a 1 KiB edit uploaded %d bytes of an %d-byte asset (%d objects)", uploaded, len(v2), len(published))
	}
	if uploaded == 0 {
		t.Fatal("the edited asset uploaded nothing")
	}
	// The second version still reconstructs exactly.
	if got := readAll(t, repo, ing2.Pointer); !bytes.Equal(got, v2) {
		t.Fatal("second version did not reconstruct")
	}
}

// TestChunkedContentFetchesFromRemoteWithEmptyCache proves a fresh machine can
// rebuild a chunked asset from published objects alone.
func TestChunkedContentFetchesFromRemoteWithEmptyCache(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	content := asset(5, 5<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	stored, err := repo.Content(ctx, ing.Pointer)
	if err != nil {
		t.Fatal(err)
	}

	// A second clone-like context with an empty cache and the same remote.
	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	if fresh.HasContent(ctx, ing.Pointer) {
		t.Fatal("a fresh cache should not report the content as present")
	}
	var progress []ItemProgress
	if _, err := fresh.FetchContent(ctx, ing.Pointer, func(p ItemProgress) { progress = append(progress, p) }); err != nil {
		t.Fatal(err)
	}
	planAt := -1
	var plan ItemProgress
	for i, p := range progress {
		if p.IsPlan() {
			planAt, plan = i, p
			break
		}
	}
	if planAt < 0 || plan.PlannedBytes != stored.StoredBytes() || plan.ReusedBytes != 0 {
		t.Fatalf("transfer plan = %+v, want %d bytes with no reuse", plan, stored.StoredBytes())
	}
	manifestReportedWhileResolving := false
	for _, p := range progress[:planAt] {
		if p.Fetched && p.PartID == ing.Pointer.ManifestDigest && p.Bytes > 0 {
			manifestReportedWhileResolving = true
		}
	}
	if !manifestReportedWhileResolving {
		t.Fatal("manifest transfer was silent while resolving")
	}
	final := progress[len(progress)-1]
	if !final.Done || final.Phase != ProgressPhaseTransfer || final.Bytes != plan.PlannedBytes ||
		final.WireBytes < final.Bytes || final.Chunks != len(stored.Objects) || final.ChunksDone != final.Chunks {
		t.Fatalf("final transfer progress = %+v", final)
	}
	if !fresh.HasContent(ctx, ing.Pointer) {
		t.Fatal("content still missing after EnsureContent")
	}
	if got := readAll(t, fresh, ing.Pointer); !bytes.Equal(got, content) {
		t.Fatal("fetched objects did not reconstruct the asset")
	}
}

// TestChunkedContentFailsClosed covers every way a chunked object can be
// wrong: a missing chunk, a corrupt chunk, a manifest that does not match its
// pointer, and a manifest that is not canonical JSON.
func TestChunkedContentFailsClosed(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	content := asset(6, 4<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	lastChunk := ing.Manifest.Chunks[len(ing.Manifest.Chunks)-1].Digest

	t.Run("missing chunk", func(t *testing.T) {
		if err := repo.Cache.Remove(repo.Domain, lastChunk, true); err != nil {
			t.Fatal(err)
		}
		rc, err := repo.OpenContent(ctx, ing.Pointer)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		if _, err := io.Copy(io.Discard, rc); !errors.Is(err, ErrObjectUnavailable) {
			t.Fatalf("got %v, want ErrObjectUnavailable", err)
		}
		if repo.HasContent(ctx, ing.Pointer) {
			t.Fatal("HasContent is true with a chunk missing")
		}
	})

	t.Run("corrupt chunk", func(t *testing.T) {
		// Put the chunk back with the right length but wrong bytes.
		var size int64
		for _, c := range ing.Manifest.Chunks {
			if c.Digest == lastChunk {
				size = c.RawSize
			}
		}
		writeRawCacheObject(t, repo, lastChunk, bytes.Repeat([]byte{0xFF}, int(size)))
		rc, err := repo.OpenContent(ctx, ing.Pointer)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		if _, err := io.Copy(io.Discard, rc); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v, want ErrIntegrity", err)
		}
	})

	t.Run("manifest does not describe the pointer", func(t *testing.T) {
		wrong := ing.Pointer
		wrong.Size++ // the manifest says otherwise
		if _, err := repo.Content(ctx, wrong); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v, want ErrIntegrity", err)
		}
		wrong = ing.Pointer
		wrong.OID = digestOf([]byte("something else"))
		if _, err := repo.Content(ctx, wrong); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v, want ErrIntegrity", err)
		}
	})

	t.Run("non-canonical manifest bytes", func(t *testing.T) {
		// Bytes that parse to the same manifest but are not the canonical
		// encoding must be rejected even though a naive parser would accept
		// them: identity is defined over the canonical form.
		canonicalBytes, err := canonical.Marshal(ing.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(canonicalBytes, &generic); err != nil {
			t.Fatal(err)
		}
		pretty, err := json.MarshalIndent(generic, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeManifest(pretty, ing.Pointer); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v, want ErrIntegrity for non-canonical manifest bytes", err)
		}
		// And the canonical bytes are accepted.
		if _, err := decodeManifest(canonicalBytes, ing.Pointer); err != nil {
			t.Fatalf("canonical manifest rejected: %v", err)
		}
	})

	t.Run("unknown encoding", func(t *testing.T) {
		future := ing.Pointer
		future.Encoding = "cdc-fastcdc/2"
		if _, err := repo.Content(ctx, future); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("got %v, want ErrIntegrity", err)
		}
	})
}

// writeRawCacheObject plants bytes under a digest they do not hash to, which
// only a test can do; the cache never produces such a file itself.
func writeRawCacheObject(t *testing.T, repo *Repo, digest string, data []byte) {
	t.Helper()
	pinned, _ := repo.Cache.IsPinned(repo.Domain, digest)
	if err := repo.Cache.Remove(repo.Domain, digest, true); err != nil {
		t.Fatal(err)
	}
	if pinned {
		// Removing an object drops its pin; corruption on disk would not, so
		// restore it or the test would model the wrong situation.
		if err := repo.Cache.Pin(repo.Domain, digest); err != nil {
			t.Fatal(err)
		}
	}
	res, err := repo.Cache.Put(repo.Domain, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	from := cacheObjectPath(repo, res.Digest)
	to := cacheObjectPath(repo, digest)
	b, err := os.ReadFile(from) //nolint:gosec // test fixture inside t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Stored objects are read-only; damage does not make them writable, and
	// on Windows that attribute is what a replacing rename trips over.
	if err := os.Chmod(to, 0o444); err != nil {
		t.Fatal(err)
	}
}

func cacheObjectPath(repo *Repo, digest string) string {
	h := digest[len("sha256:"):]
	return repo.Cache.Root() + string(os.PathSeparator) + "objects" + string(os.PathSeparator) + repo.Domain +
		string(os.PathSeparator) + h[:2] + string(os.PathSeparator) + h[2:4] + string(os.PathSeparator) + h
}

// TestChunkingCanBeDisabledForDiagnostics documents the escape hatch.
func TestChunkingCanBeDisabledForDiagnostics(t *testing.T) {
	t.Setenv(chunk.EnvPolicy, "off")
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ing, err := repo.Ingest(context.Background(), bytes.NewReader(asset(7, 4<<20)), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ing.Pointer.Encoding != pointer.EncodingWhole {
		t.Fatalf("encoding %q with %s=off", ing.Pointer.Encoding, chunk.EnvPolicy)
	}
}

// TestManifestObjectIsCanonicalAndSelfDescribing checks the stored manifest
// bytes, which travel to other machines and must be reproducible there.
func TestManifestObjectIsCanonicalAndSelfDescribing(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	ing, err := repo.Ingest(ctx, bytes.NewReader(asset(8, 3<<20)), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	rd, err := repo.Cache.Open(repo.Domain, ing.Pointer.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	data, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	var stored domain.ObjectManifest
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.OID != ing.Pointer.OID || stored.Encoding != ing.Pointer.Encoding || stored.StorageDomainID != repo.Domain {
		t.Fatalf("stored manifest does not describe its pointer: %+v", stored)
	}
	want, err := canonical.Marshal(ing.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatal("stored manifest bytes are not the canonical encoding")
	}
	if digestOf(data) != ing.Pointer.ManifestDigest {
		t.Fatal("stored manifest bytes do not hash to the pointer's manifest digest")
	}
}

// TestUnloadReportsReclaimedSpaceNotPointerSize guards a units bug: unload
// used to report the couple of hundred bytes of placeholder it writes against
// the asset's own size as the total, so a display counted kibibytes towards a
// mebibyte total and looked stalled. The two sides of that ratio have to be
// the same quantity, and the useful one is the space reclaimed.
func TestUnloadReportsReclaimedSpaceNotPointerSize(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	const size = 3 << 20
	content := asset(77, size)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	// Stage the pointer the way the clean filter would, then put the real
	// bytes back so the path is hydrated and unload has something to reclaim.
	r.WriteFile("big.bin", ing.Pointer.MustEncode())
	r.Git("add", ".")
	r.Commit("asset")
	r.WriteFile("big.bin", content)
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}

	var finished []ItemProgress
	outs, err := repo.Dehydrate(ctx, Selection{}, func(p ItemProgress) {
		if p.Done {
			finished = append(finished, p)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 || outs[0].Skipped {
		t.Fatalf("unload did not run: %+v", outs)
	}
	if outs[0].Bytes != size {
		t.Fatalf("outcome reports %d bytes for a %d-byte asset; it must report the space reclaimed, not the placeholder", outs[0].Bytes, size)
	}
	if len(finished) != 1 {
		t.Fatalf("expected one completion event, got %d", len(finished))
	}
	ev := finished[0]
	if ev.Bytes != ev.Total {
		t.Fatalf("completion reports %d of %d: the two sides of the ratio are different quantities", ev.Bytes, ev.Total)
	}
	if ev.Total != ing.Pointer.Size {
		t.Fatalf("completion total %d does not match the asset size %d", ev.Total, ing.Pointer.Size)
	}
	// The placeholder really is tiny; the point is that it is not what is
	// reported.
	if fi, err := os.Stat(filepath.Join(repo.Root, "big.bin")); err != nil || fi.Size() > 1024 {
		t.Fatalf("working file is %v after unload (%v)", fi, err)
	}
}

// TestDuplicateContentIsReportedAsReused covers the bug behind rows that never
// finished, and the value proposition behind it.
//
// A repository vendors the same font or icon into many resources, so one
// pointer appears under many paths. Counting a duplicated asset's objects once
// per copy left its row needing N times what it actually downloads: pinned at
// exactly 1/N, never complete, never yielding its place on screen. Each object
// must be counted once — and the paths that got their content for free should
// say so, because storing it once is the point.
func TestDuplicateContentIsReportedAsReused(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	// The same content under four paths, plus one unique asset.
	shared := asset(91, 3<<20)
	unique := asset(92, 2<<20)
	files := map[string][]byte{
		"a/font.bin": shared, "b/font.bin": shared, "c/font.bin": shared, "d/font.bin": shared,
		"solo.bin": unique,
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		ing, err := repo.Ingest(ctx, bytes.NewReader(files[p]), p)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}

	// A fresh machine loads them all.
	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	var plan ItemProgress
	perPath := map[string]ItemProgress{}
	outs, err := fresh.Hydrate(ctx, Selection{}, func(p ItemProgress) {
		switch {
		case p.IsPlan():
			plan = p
		case p.Fetched:
			perPath[p.Path] = p
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != len(files) {
		t.Fatalf("loaded %d of %d paths", len(outs), len(files))
	}

	// Three of the four copies need no transfer at all.
	shares := 0
	for _, o := range outs {
		if o.Skipped {
			t.Fatalf("%s was skipped: %s", o.Path, o.Reason)
		}
		if o.Reuse == ReuseShared {
			shares++
		}
	}
	if shares != 3 {
		t.Fatalf("%d paths reported shared content, want 3: %+v", shares, outs)
	}
	if plan.ReusedItems != 3 || plan.ReusedBytes != 3*int64(len(shared)) {
		t.Fatalf("plan reports %d reused items / %d bytes, want 3 / %d", plan.ReusedItems, plan.ReusedBytes, 3*len(shared))
	}

	// The one path that does the downloading asks for exactly what it needs,
	// and gets there. This is the assertion the old code failed: its total was
	// four times too big, so Bytes could never reach Total.
	var downloader ItemProgress
	for _, p := range perPath {
		if p.Total > downloader.Total {
			downloader = p
		}
	}
	if downloader.Bytes != downloader.Total {
		t.Fatalf("%s finished at %d of %d bytes; the total counts work that is not done",
			downloader.Path, downloader.Bytes, downloader.Total)
	}
	if downloader.ChunksDone != downloader.Chunks {
		t.Fatalf("%s finished at %d of %d chunks", downloader.Path, downloader.ChunksDone, downloader.Chunks)
	}
	// Every file still lands byte-for-byte.
	for _, p := range paths {
		got, err := os.ReadFile(filepath.Join(fresh.Root, p)) //nolint:gosec // test tree
		if err != nil || !bytes.Equal(got, files[p]) {
			t.Fatalf("%s did not materialize correctly (%v)", p, err)
		}
	}
}

// TestLoadAnnouncesItsStagesAndResolvesConcurrently covers both halves of the
// same problem: a load is three different kinds of work, and the first of them
// used to be invisible *and* serial — one round trip per asset to read its
// manifest before a single chunk was fetched.
func TestLoadAnnouncesItsStagesAndResolvesConcurrently(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	const files = 8
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("a%02d.bin", i)
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(300+i), 3<<20)), path)
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

	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	// Every manifest fetch is slow, so a serial resolve would dominate.
	slow := &slowStore{inner: mustStore(t, fresh, "primary"), delay: 30 * time.Millisecond}
	fresh.setStore("primary", slow)

	var stages []string
	if _, err := fresh.Hydrate(ctx, Selection{}, func(p ItemProgress) {
		if p.IsStage() && (len(stages) == 0 || stages[len(stages)-1] != p.Stage) {
			stages = append(stages, p.Stage)
		}
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{StageResolving, StageTransferring, StageWriting}
	if len(stages) != len(want) {
		t.Fatalf("stages %v, want %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("stages %v, want %v", stages, want)
		}
	}
	if slow.inFlmax < 2 {
		t.Fatalf("manifests were resolved one at a time (max %d in flight); a load pays one round trip per asset before any chunk moves", slow.inFlmax)
	}
}
