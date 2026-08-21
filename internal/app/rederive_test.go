package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

// TestUnloadProvesSoleCopiesEvenWhenTheManifestMustBeRebuilt is the regression
// for a hole in unload's precondition that the fast path had closed and the
// fallback had not.
//
// When the manifest cannot be resolved there is nothing to check the working
// file against, so the file is re-derived to rebuild it. Re-derivation alone
// is not a proof: ingestion stores a chunk it believes is already cached
// without reading it, which is right for content that will be verified when it
// is next read and wrong for content that is about to become the only copy.
// A chunk that rotted in place at its recorded size therefore passed, and the
// working file was destroyed against it.
func TestUnloadProvesSoleCopiesEvenWhenTheManifestMustBeRebuilt(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	if ptr.ManifestDigest == "" {
		t.Skip("not a chunked asset")
	}
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
	// The cache is the only copy: nothing published, and no remote holds it.
	forgetPublication(t, repo, victim.Digest)
	forgetPublication(t, repo, ptr.ManifestDigest)
	emptyRemote(t, repo)
	// Lose the manifest, so unload has to rebuild it before it can check
	// anything — the fallback path.
	if err := repo.Cache.Remove(repo.Domain, ptr.ManifestDigest, true); err != nil {
		t.Fatal(err)
	}
	// Rot a chunk in place, same length.
	writeRawCacheObject(t, repo, victim.Digest, bytes.Repeat([]byte{0xEE}, int(victim.Size)))

	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 1 {
		t.Fatalf("expected one outcome: %+v", outs)
	}
	// Unload may refuse, or it may repair from the working file — but it must
	// not destroy the file and leave the damaged copy standing.
	if !outs[0].Skipped {
		if err := repo.Cache.Verify(repo.Domain, victim.Digest); err != nil {
			t.Fatalf("unload replaced the working file while its only copy was damaged: %v", err)
		}
		if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(repo.Root + string(os.PathSeparator) + path) //nolint:gosec // test fixture
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatal("content did not survive the round trip")
		}
	}
}

// emptyRemote removes every object from the repository's filesystem remote, so
// nothing can be re-fetched.
func emptyRemote(t *testing.T, repo *Repo) {
	t.Helper()
	for _, v := range repo.Remotes() {
		if v.Type != "filesystem" || v.Path == "" {
			continue
		}
		if err := os.RemoveAll(v.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(v.Path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestUnloadProvesSoleCopiesWhenTheFileFailsTheFirstCheck is the sibling of
// the test above, for the other route into re-derivation.
//
// The proof can fail for a reason other than a missing manifest — a region
// that does not hash to what the manifest says. That is normally a modified
// working file, so the file is re-derived to name the digest actually found.
// But re-derivation is not itself a proof, on this route either: it stores a
// chunk it believes is already cached without reading it, so a sole copy that
// rotted in place at its recorded size passed, and the file was destroyed
// against it.
func TestUnloadProvesSoleCopiesWhenTheFileFailsTheFirstCheck(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []ContentObject
	for _, o := range c.Objects {
		if o.Kind != ObjectKindManifest {
			chunks = append(chunks, o)
		}
	}
	if len(chunks) < 2 {
		t.Skip("need at least two chunks")
	}
	// The cache is the only copy of everything.
	for _, o := range c.Objects {
		forgetPublication(t, repo, o.Digest)
	}
	emptyRemote(t, repo)
	// Rot the last chunk in place, same length: the working file still matches
	// the manifest, so the first check passes on it and fails nowhere — so
	// force the first check down the diverged route by damaging the manifest's
	// view of an earlier chunk instead.
	victim := chunks[len(chunks)-1]
	writeRawCacheObject(t, repo, victim.Digest, bytes.Repeat([]byte{0xEE}, int(victim.Size)))
	// Truncate the working file by one byte and restore it, so the proof runs
	// against a file that has genuinely moved and then moved back — the shape
	// that sends ensureRecoverable through re-derivation.
	full, err := os.ReadFile(repo.Root + string(os.PathSeparator) + path) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo.Root+string(os.PathSeparator)+path, full[:len(full)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	if outs, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil || !outs[0].Skipped {
		t.Fatalf("unload accepted a short working file: %+v %v", outs, err)
	}
	if err := os.WriteFile(repo.Root+string(os.PathSeparator)+path, full, 0o600); err != nil {
		t.Fatal(err)
	}

	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !outs[0].Skipped {
		if err := repo.Cache.Verify(repo.Domain, victim.Digest); err != nil {
			t.Fatalf("unload replaced the working file while its only copy was damaged: %v", err)
		}
		if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(repo.Root + string(os.PathSeparator) + path) //nolint:gosec // test fixture
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatal("content did not survive the round trip")
		}
	}
}

// failingReader yields n good bytes and then an I/O error, standing in for a
// disk that fails mid-read.
type failingReader struct {
	data []byte
	n    int
	pos  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= f.n {
		return 0, errors.New("input/output error")
	}
	n := copy(p, f.data[f.pos:f.n])
	f.pos += n
	return n, nil
}

// TestProveAgainstDoesNotMistakeAReadFailureForADivergentFile is the
// regression for the reachable route into the gap above.
//
// A file that cannot be read tells you nothing about its content. Reporting
// that as "the file does not match" sent the caller down the re-derivation
// path — which rebuilds from the same file and stores chunks it believes are
// already cached without reading them — so a sole copy that had rotted in
// place could be accepted on the strength of an I/O error that said nothing
// about it.
func TestProveAgainstDoesNotMistakeAReadFailureForADivergentFile(t *testing.T) {
	repo, content, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	sole := map[string]bool{}
	for _, o := range c.Objects {
		sole[o.Digest] = false // published; the cheap check applies
	}
	// Fails partway through the first chunk.
	rd := &failingReader{data: content, n: 4096}
	err = repo.proveAgainst(c, ptr, rd, sole)
	if err == nil {
		t.Fatal("a file that could not be read was accepted as proven")
	}
	if errors.Is(err, errWorkingFileDiverged) {
		t.Fatalf("a read failure was reported as divergence: %v", err)
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unexpected error kind: %v", err)
	}
	_ = path
}

// TestProveAgainstStillReportsAShortFileAsDivergent keeps the distinction
// honest in the other direction: a file that ends early really has changed.
func TestProveAgainstStillReportsAShortFileAsDivergent(t *testing.T) {
	repo, content, _ := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, "big.bin")
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	sole := map[string]bool{}
	if err := repo.proveAgainst(c, ptr, bytes.NewReader(content[:len(content)/2]), sole); !errors.Is(err, errWorkingFileDiverged) {
		t.Fatalf("a short file must read as divergent, got %v", err)
	}
}

// erroringReader yields the whole content and then fails instead of reporting
// the end of the file — a disk that goes away at exactly the wrong moment.
type erroringReader struct {
	data []byte
	pos  int
}

func (e *erroringReader) Read(p []byte) (int, error) {
	if e.pos >= len(e.data) {
		return 0, errors.New("input/output error")
	}
	n := copy(p, e.data[e.pos:])
	e.pos += n
	return n, nil
}

// TestProveAgainstDoesNotTakeAFailedReadAsProofOfLength covers the last step of
// the proof, which had the same flaw as the first.
//
// After every declared chunk matches, one more read decides whether anything
// follows. Reading bytes means the file is longer than the manifest. An error
// that is not EOF means the question was never answered — and treating that as
// "nothing follows" let unload replace a file whose length it had not
// established, destroying whatever came after the part it did check.
func TestProveAgainstDoesNotTakeAFailedReadAsProofOfLength(t *testing.T) {
	repo, content, _ := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, "big.bin")
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	sole := map[string]bool{}
	err = repo.proveAgainst(c, ptr, &erroringReader{data: content}, sole)
	if err == nil {
		t.Fatal("a file whose end could not be read was accepted as proven")
	}
	if errors.Is(err, errWorkingFileDiverged) {
		t.Fatalf("a read failure was reported as divergence: %v", err)
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

// stallingReader returns the whole content, then one zero-length read with no
// error, then a trailing byte. A Read that returns (0, nil) means no progress
// was made — the io.Reader contract is explicit that it does not mean the end
// of the input — and a later read may still produce content.
type stallingReader struct {
	data    []byte
	pos     int
	stalled bool
	tail    []byte
	tailPos int
}

func (s *stallingReader) Read(p []byte) (int, error) {
	if s.pos < len(s.data) {
		n := copy(p, s.data[s.pos:])
		s.pos += n
		return n, nil
	}
	if !s.stalled {
		s.stalled = true
		return 0, nil // no progress, not the end
	}
	if s.tailPos < len(s.tail) {
		n := copy(p, s.tail[s.tailPos:])
		s.tailPos += n
		return n, nil
	}
	return 0, io.EOF
}

// TestProveAgainstDoesNotTakeANoProgressReadAsTheEndOfTheFile: a file with
// trailing content must be refused even when the read that would have found it
// first returns nothing without failing.
func TestProveAgainstDoesNotTakeANoProgressReadAsTheEndOfTheFile(t *testing.T) {
	repo, content, _ := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, "big.bin")
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	rd := &stallingReader{data: content, tail: []byte("extra")}
	err = repo.proveAgainst(c, ptr, rd, map[string]bool{})
	if err == nil {
		t.Fatal("a file with trailing content was accepted as proven")
	}
	if !errors.Is(err, errWorkingFileDiverged) {
		t.Fatalf("trailing content must read as divergence, got %v", err)
	}
}
