package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// errWorkingFileDiverged: the file on disk is not what its index pointer says.
// It is internal — the caller re-derives the content to report the real digest
// in the conflict, because "it differs" is not an actionable thing to tell
// someone about a file they are about to lose.
var errWorkingFileDiverged = errors.New("app: working file does not match its index pointer")

// ensureRecoverable guarantees the bytes about to leave the working tree stay
// recoverable, by proving the working file itself against the manifest that
// describes it and repairing the cache from the bytes it is reading.
//
// Unload is the one operation that destroys the user's copy of the content, so
// its precondition is the only place in the system where a wrong answer is
// unrecoverable rather than an error. Everywhere else a bad object is caught
// by the digest check that every read performs before the bytes reach anything
// observable, and the operation simply fails; here there is no downstream read
// to catch it.
//
// So the proof runs over the file being destroyed rather than over the cache
// copy standing in for it. Reading the working file is not the expensive half
// of either approach — both read the content once — but it answers a stronger
// question: the old check could confirm that a cached object matched its
// digest and still leave the working file destroyed against a cache it had no
// way to correct, because storing an object whose key already exists is a
// no-op no matter what the stored bytes have become. Proving the file in hand
// means a divergent cache entry can be replaced, not merely detected.
//
// It costs one sequential pass. The manifest already records where the chunk
// boundaries are, so there is no content-defined chunking to redo, and a slice
// that hashes to the digest the manifest names for it establishes the whole
// file transitively: the manifest is itself verified when it is loaded, and it
// declares the oid of the concatenation.
func (r *Repo) ensureRecoverable(ctx context.Context, ptr pointer.Pointer, dest, path string, onProgress func(int64)) error {
	var reported int64
	report := func(n int64) {
		if onProgress != nil && n > reported {
			reported = n
			onProgress(n)
		}
	}
	err := r.proveRecoverable(ctx, ptr, dest, path, report)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, errNoManifest) && !errors.Is(err, errWorkingFileDiverged):
		return err
	}
	// Either there was no manifest to check against, or the file did not match
	// the one there was. Re-deriving from the working file settles both: it
	// rebuilds a missing manifest, and it names the digest actually found when
	// the file has genuinely changed.
	if rerr := r.rederive(ctx, ptr, dest, path); rerr != nil {
		return rerr
	}
	// Re-derivation is not the proof, on either route. It establishes the
	// whole-file digest, but it stores a chunk it believes is already cached
	// without reading it — right for content that will be verified when it is
	// next read, wrong for content about to become the only copy. So prove
	// against the rebuilt manifest, once.
	switch err := r.proveRecoverable(ctx, ptr, dest, path, report); {
	case err == nil:
		return nil
	case errors.Is(err, errNoManifest):
		return fmt.Errorf("%w: object manifest %s is still unavailable after re-deriving %s",
			ErrObjectUnavailable, ptr.ManifestDigest, path)
	case errors.Is(err, errWorkingFileDiverged):
		return fmt.Errorf("%w: %s still does not match index pointer %s after re-deriving it",
			ErrConflict, path, ptr.OID)
	default:
		return err
	}
}

// errNoManifest: the content plan could not be resolved, so there is nothing
// to prove the working file against.
var errNoManifest = errors.New("app: content manifest unavailable")

func (r *Repo) proveRecoverable(ctx context.Context, ptr pointer.Pointer, dest, path string, onProgress func(int64)) error {
	c, err := r.Content(ctx, ptr)
	if err != nil {
		return fmt.Errorf("%w: %w", errNoManifest, err)
	}
	f, err := os.Open(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	// Objects the required remotes already hold have a second copy, so a
	// damaged local one is a refetch rather than a loss. Objects that are
	// still pending do not: the cache is the only place those bytes exist
	// anywhere, so they are read in full. In a repository that has published,
	// that is almost nothing; in one that has not, it is everything — which is
	// exactly the right way round.
	sole, err := r.soleCopies(ctx, c)
	if err != nil {
		return err
	}
	var source io.Reader = f
	if onProgress != nil {
		source = &progressReader{r: f, step: progressStep, next: progressStep, fn: onProgress}
	}
	if err := r.proveAgainst(c, ptr, source, sole); err != nil {
		return err // including errWorkingFileDiverged, which the caller settles
	}
	if onProgress != nil {
		onProgress(ptr.Size)
	}
	// The objects exist and are proven; make sure the ledger and the pins
	// agree, so pruning cannot take them away afterwards.
	_, err = r.recordObjects(ctx, c.Objects, ptr, path)
	return err
}

// proveAgainst reads the working file once, checking each region against the
// digest the manifest records for it and restoring any cached object that is
// missing or stored at the wrong size from the bytes just read.
func (r *Repo) proveAgainst(c Content, ptr pointer.Pointer, f io.Reader, sole map[string]bool) error {
	chunks := make([]ContentObject, 0, len(c.Objects))
	for _, o := range c.Objects {
		if o.Kind == ObjectKindManifest {
			continue // not part of the content; verified when it was loaded
		}
		chunks = append(chunks, o)
	}
	if len(chunks) == 1 && chunks[0].Kind == ObjectKindWhole {
		o := chunks[0]
		if sole[o.Digest] && r.Cache.HasSized(r.Domain, o.Digest, o.Size) {
			if err := r.Cache.Verify(r.Domain, o.Digest); err != nil {
				// The only copy, and damaged: clear it so the stream below
				// rewrites it rather than recognising it as present.
				//
				// Removing an object drops its pin, and an unpinned pending
				// object may be pruned. Put it back: the window is short but
				// it is the window in which these bytes exist nowhere else.
				pinned, _ := r.Cache.IsPinned(r.Domain, o.Digest)
				if rerr := r.Cache.Remove(r.Domain, o.Digest, true); rerr != nil {
					return rerr
				}
				if pinned {
					if perr := r.Cache.Pin(r.Domain, o.Digest); perr != nil {
						return perr
					}
				}
			}
		}
		// One object as large as the file: stream it rather than hold it.
		// PutExpecting hashes what it reads and writes only if the cache is
		// missing the object or holds it at the wrong size.
		res, err := r.Cache.PutExpecting(r.Domain, chunks[0].Digest, chunks[0].Size, f)
		if err != nil {
			if errors.Is(err, cache.ErrCorrupt) {
				return errWorkingFileDiverged
			}
			return err
		}
		if res.Digest != ptr.OID {
			return errWorkingFileDiverged
		}
		return nil
	}
	var buf []byte
	for _, o := range chunks {
		if o.Size < 0 || o.Size > maxChunkBuffer {
			return fmt.Errorf("%w: manifest chunk %s declares %d bytes", ErrIntegrity, o.Digest, o.Size)
		}
		if int64(cap(buf)) < o.Size {
			buf = make([]byte, o.Size)
		}
		region := buf[:o.Size]
		if _, err := io.ReadFull(f, region); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errWorkingFileDiverged // the file is shorter than the manifest
			}
			// A failure to read is not evidence about the content. Treating it
			// as divergence would send the caller down the re-derivation path
			// and let a damaged sole copy through on the strength of an I/O
			// error that says nothing about it.
			return fmt.Errorf("%w: reading %s: %w", ErrIntegrity, o.Digest, err)
		}
		sum := sha256.Sum256(region)
		if cache.Digest(sum[:]) != o.Digest {
			return errWorkingFileDiverged
		}
		if r.Cache.HasSized(r.Domain, o.Digest, o.Size) {
			if !sole[o.Digest] {
				continue // a required remote holds it too
			}
			// No other copy exists. Identity and size do not detect bytes that
			// rotted in place, and there would be nowhere to refetch from, so
			// this one is read.
			if err := r.Cache.Verify(r.Domain, o.Digest); err == nil {
				continue
			}
		}
		// Missing, stored at a size the manifest contradicts, or the only copy
		// and damaged. The bytes in hand are proven, so put them right rather
		// than fail the unload.
		if err := r.Cache.PutVerified(r.Domain, o.Digest, region); err != nil {
			return err
		}
	}
	// Nothing may follow the last chunk: a longer file is a different file.
	//
	// Both halves of the answer matter. Bytes read mean the file is longer
	// than the manifest, which is divergence; an error that is not EOF means
	// the question was not answered at all, and accepting that as "nothing
	// follows" would let unload destroy a file whose length it never
	// established.
	// io.ReadFull rather than a bare Read, because a Read may legitimately
	// return no bytes and no error — the contract says that means no progress
	// was made, not that the file ended. Treating it as the end would let
	// unload replace a file whose length it never established. ReadFull
	// retries until it has a byte, a real EOF, or an error.
	var probe [1]byte
	switch _, err := io.ReadFull(f, probe[:]); {
	case err == nil:
		return errWorkingFileDiverged // a byte followed the last chunk
	case errors.Is(err, io.EOF):
		return nil
	default:
		return fmt.Errorf("%w: reading past the last chunk: %w", ErrIntegrity, err)
	}
}

// soleCopies names the objects behind the content that no required remote is
// recorded as holding, so the local cache is the only copy of them.
//
// It is one batched ledger query, and it is deliberately pessimistic in both
// directions that matter: an object the ledger has never seen counts as sole,
// and a repository with no required remotes has every object sole. Being wrong
// the other way would mean skipping the full read of bytes that exist nowhere
// else.
func (r *Repo) soleCopies(ctx context.Context, c Content) (map[string]bool, error) {
	sole := map[string]bool{}
	digests := make([]string, 0, len(c.Objects))
	for _, o := range c.Objects {
		digests = append(digests, o.Digest)
		sole[o.Digest] = true
	}
	required := r.RequiredRemotes()
	if len(required) == 0 {
		return sole, nil
	}
	l, err := r.LedgerIfExists(ctx)
	if err != nil || l == nil {
		return sole, nil //nolint:nilerr // no ledger means nothing is known to be published
	}
	_, remotes, err := l.PublicationIndex(ctx, digests)
	if err != nil {
		return nil, err
	}
	for _, d := range digests {
		have := remotes[d]
		all := true
		for _, name := range required {
			if !have[name] {
				all = false
				break
			}
		}
		if all {
			sole[d] = false
		}
	}
	return sole, nil
}

// maxChunkBuffer bounds what one manifest entry may make this allocate. The
// chunk policy's maximum is far below it; the check is against a manifest that
// is malformed or hostile rather than against ordinary content.
const maxChunkBuffer = 64 << 20

// rederive re-ingests the working file from scratch, which rebuilds a missing
// manifest and reports the real digest when the file has genuinely changed.
func (r *Repo) rederive(ctx context.Context, ptr pointer.Pointer, dest, path string) error {
	f, err := os.Open(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	ing, err := r.IngestExpecting(ctx, f, path, ptr)
	if err != nil {
		if errors.Is(err, cache.ErrCorrupt) {
			return fmt.Errorf("%w: working file does not match index pointer %s: %v", ErrConflict, ptr.OID, err)
		}
		return err
	}
	if ing.Digest != ptr.OID {
		return fmt.Errorf("%w: working file digest %s differs from index pointer %s", ErrConflict, ing.Digest, ptr.OID)
	}
	return nil
}
