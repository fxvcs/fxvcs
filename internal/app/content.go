package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"

	"crypto/sha256"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Object kinds recorded in the publication ledger's encoding column. They
// describe what a *stored object* is, which is not the same as the pointer
// encoding: one cdc-fastcdc/1 pointer is backed by one manifest object and
// many chunk objects.
const (
	ObjectKindWhole    = pointer.EncodingWhole // the whole file, one object
	ObjectKindChunk    = "chunk/1"             // one content-defined chunk
	ObjectKindManifest = "manifest/1"          // the canonical object manifest
)

// ContentObject is one stored object behind a pointer.
type ContentObject struct {
	Digest string
	Size   int64
	Kind   string
}

// Content is the resolved storage plan for one pointer: the manifest plus
// every object that must exist before the file can be materialized.
type Content struct {
	Pointer  pointer.Pointer
	Manifest domain.ObjectManifest
	// Objects lists the manifest object (chunked encodings only, because a
	// whole/1 manifest is derived from the pointer and never stored) followed
	// by the chunks in file order.
	Objects []ContentObject
}

// Chunked reports whether the pointer is backed by more than the file itself.
func (c Content) Chunked() bool { return c.Pointer.Encoding != pointer.EncodingWhole }

// Digests returns every object digest the pointer depends on.
func (c Content) Digests() []string {
	out := make([]string, 0, len(c.Objects))
	for _, o := range c.Objects {
		out = append(out, o.Digest)
	}
	return out
}

// StoredBytes is the total size of the objects behind the pointer.
func (c Content) StoredBytes() int64 {
	var n int64
	for _, o := range c.Objects {
		n += o.Size
	}
	return n
}

// Content resolves the storage plan for a pointer. For whole/1 this is pure
// local arithmetic. For a chunked encoding it needs the manifest object, which
// it takes from the cache or fetches from a configured remote, and which it
// validates against the pointer before trusting a single chunk digest.
func (r *Repo) Content(ctx context.Context, ptr pointer.Pointer) (Content, error) {
	return r.contentProgress(ctx, ptr, nil)
}

// contentProgress is Content with byte-level reporting for a manifest that
// must cross the storage boundary before its chunk plan can be decoded.
func (r *Repo) contentProgress(ctx context.Context, ptr pointer.Pointer, onManifestBytes func(digest string, delta int64)) (Content, error) {
	if err := ptr.Validate(); err != nil {
		return Content{}, err
	}
	if ptr.StorageDomainID != r.Domain {
		return Content{}, fmt.Errorf("%w: pointer storage domain %s does not match repository domain %s", ErrIntegrity, ptr.StorageDomainID, r.Domain)
	}
	if ptr.Encoding == pointer.EncodingWhole {
		want, err := filter.WholePointer(ptr.StorageDomainID, ptr.OID, ptr.Size)
		if err != nil {
			return Content{}, err
		}
		if want.ManifestDigest != ptr.ManifestDigest {
			return Content{}, fmt.Errorf("%w: pointer manifest %s does not match the whole/1 derivation %s", ErrIntegrity, ptr.ManifestDigest, want.ManifestDigest)
		}
		return Content{
			Pointer:  ptr,
			Manifest: filter.WholeManifest(ptr.StorageDomainID, ptr.OID, ptr.Size),
			Objects:  []ContentObject{{Digest: ptr.OID, Size: ptr.Size, Kind: ObjectKindWhole}},
		}, nil
	}
	if _, err := chunk.ParamsFor(ptr.Encoding); err != nil {
		return Content{}, fmt.Errorf("%w: %v; run: fxvcs upgrade --check", ErrIntegrity, err)
	}
	man, err := r.loadManifestProgress(ctx, ptr, onManifestBytes)
	if err != nil {
		return Content{}, err
	}
	objs := make([]ContentObject, 0, len(man.Chunks)+1)
	objs = append(objs, ContentObject{Digest: ptr.ManifestDigest, Size: manifestSize(man), Kind: ObjectKindManifest})
	for _, c := range man.Chunks {
		objs = append(objs, ContentObject{Digest: c.Digest, Size: c.RawSize, Kind: ObjectKindChunk})
	}
	return Content{Pointer: ptr, Manifest: man, Objects: objs}, nil
}

// manifestSize is the canonical byte length of a manifest, used only for
// progress and publication accounting.
func manifestSize(m domain.ObjectManifest) int64 {
	b, err := canonical.Marshal(m)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// noHealKey marks a context under which a damaged object must be reported
// rather than replaced.
//
// Replacing means fetching, and fetching is an unbounded network request.
// docs/spec/checkout-consistency.md forbids holding a path lock across one —
// that is why the compare-and-swap sequence does its I/O outside the lock and
// re-validates afterwards. So the read that happens under the lock reports,
// and the caller heals once it has let go.
type noHealKey struct{}

// withoutHealing marks ctx as a context that may not repair what it finds.
func withoutHealing(ctx context.Context) context.Context {
	return context.WithValue(ctx, noHealKey{}, true)
}

func healingAllowed(ctx context.Context) bool {
	v, _ := ctx.Value(noHealKey{}).(bool)
	return !v
}

// loadManifestProgress reads and validates the manifest object named by the pointer,
// re-fetching it once if the cached copy turns out to be damaged (except under
// Options.Diagnostic, where finding damage is the result).
//
// The retry is here rather than at the presence check because presence is
// cheap and reading is where damage is detectable. A manifest is also the one
// object whose loss takes its whole asset with it — every chunk digest lives
// in it — so it is worth one round trip to replace rather than reporting an
// asset unreadable.
func (r *Repo) loadManifestProgress(ctx context.Context, ptr pointer.Pointer, onBytes func(digest string, delta int64)) (domain.ObjectManifest, error) {
	m, err := r.readManifestProgress(ctx, ptr, onBytes)
	if err == nil || !errors.Is(err, cache.ErrCorrupt) {
		return m, err
	}
	if r.opts.Diagnostic || !healingAllowed(ctx) {
		return m, err // reporting, not repairing
	}
	// repairCorrupt, not an open-coded evict, so the manifest keeps the pin it
	// may be holding: an object that loses its pin becomes evictable while the
	// ledger still says it is unpublished.
	if healed, _ := r.repairCorruptProgress(ctx, err, func(delta int64) {
		if onBytes != nil {
			onBytes(ptr.ManifestDigest, delta)
		}
	}); !healed {
		return m, err // nowhere to fetch it from; the integrity error stands
	}
	return r.readManifestProgress(ctx, ptr, onBytes)
}

func (r *Repo) readManifestProgress(ctx context.Context, ptr pointer.Pointer, onBytes func(digest string, delta int64)) (domain.ObjectManifest, error) {
	var m domain.ObjectManifest
	if err := r.ensureCachedProgress(ctx, ptr.ManifestDigest, func(delta int64) {
		if onBytes != nil {
			onBytes(ptr.ManifestDigest, delta)
		}
	}); err != nil {
		return m, err
	}
	rd, err := r.Cache.Open(r.Domain, ptr.ManifestDigest)
	if err != nil {
		return m, fmt.Errorf("%w: object manifest %s: %v", ErrObjectUnavailable, ptr.ManifestDigest, err)
	}
	defer rd.Close()
	// A manifest is small by construction; a huge object under a manifest
	// digest is a tampering attempt, not a manifest.
	const maxManifest = 64 << 20
	if rd.Size() > maxManifest {
		return m, fmt.Errorf("%w: object manifest %s is %d bytes", ErrIntegrity, ptr.ManifestDigest, rd.Size())
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		if errors.Is(err, cache.ErrCorrupt) {
			return m, fmt.Errorf("%w: object manifest %s: %w", ErrIntegrity, ptr.ManifestDigest, err)
		}
		return m, err
	}
	return decodeManifest(data, ptr)
}

// decodeManifest parses manifest bytes and checks every claim it makes against
// the pointer. The cache already proved the bytes hash to the manifest digest;
// this proves the manifest describes *this* pointer and nothing else.
func decodeManifest(data []byte, ptr pointer.Pointer) (domain.ObjectManifest, error) {
	var m domain.ObjectManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%w: object manifest %s: %v", ErrIntegrity, ptr.ManifestDigest, err)
	}
	if dec.More() {
		return m, fmt.Errorf("%w: object manifest %s has trailing data", ErrIntegrity, ptr.ManifestDigest)
	}
	// Reject a manifest that is not in canonical form even if it parses: the
	// digest is defined over the canonical encoding, so a non-canonical byte
	// sequence with a colliding meaning must never be accepted.
	canonicalBytes, err := canonical.Marshal(m)
	if err != nil {
		return m, fmt.Errorf("%w: object manifest %s: %v", ErrIntegrity, ptr.ManifestDigest, err)
	}
	if !bytes.Equal(canonicalBytes, data) {
		return m, fmt.Errorf("%w: object manifest %s is not canonical JSON", ErrIntegrity, ptr.ManifestDigest)
	}
	digest, err := integrity.ManifestDigest(m)
	if err != nil {
		return m, fmt.Errorf("%w: object manifest %s: %v", ErrIntegrity, ptr.ManifestDigest, err)
	}
	switch {
	case digest != ptr.ManifestDigest:
		return m, fmt.Errorf("%w: object manifest hashes to %s, pointer says %s", ErrIntegrity, digest, ptr.ManifestDigest)
	case m.StorageDomainID != ptr.StorageDomainID:
		return m, fmt.Errorf("%w: object manifest domain %s, pointer domain %s", ErrIntegrity, m.StorageDomainID, ptr.StorageDomainID)
	case m.Encoding != ptr.Encoding:
		return m, fmt.Errorf("%w: object manifest encoding %s, pointer encoding %s", ErrIntegrity, m.Encoding, ptr.Encoding)
	case m.OID != ptr.OID:
		return m, fmt.Errorf("%w: object manifest oid %s, pointer oid %s", ErrIntegrity, m.OID, ptr.OID)
	case m.Size != ptr.Size:
		return m, fmt.Errorf("%w: object manifest size %d, pointer size %d", ErrIntegrity, m.Size, ptr.Size)
	}
	return m, nil
}

// EnsureContent makes every object behind the pointer available in the local
// cache, fetching what is missing from configured remotes with bounded
// concurrency. No lock is held while it runs.
func (r *Repo) EnsureContent(ctx context.Context, ptr pointer.Pointer, progress func(fetched, total int)) (Content, error) {
	total := 0
	done := 0
	return r.ensureContentProgress(ctx, ptr, ensureContentCallbacks{
		planned: func(_ Content, missing []ContentObject) { total = len(missing) },
		fetched: func(string) {
			done++
			if progress != nil {
				progress(done, total)
			}
		},
	})
}

type ensureContentCallbacks struct {
	manifestBytes func(digest string, delta int64)
	planned       func(content Content, missing []ContentObject)
	objectBytes   func(digest string, delta int64)
	fetched       func(digest string)
}

// ensureContentProgress centralizes content resolution and fetching so callers
// can choose count-only or byte/part telemetry without duplicating cache rules.
func (r *Repo) ensureContentProgress(ctx context.Context, ptr pointer.Pointer, cb ensureContentCallbacks) (Content, error) {
	c, err := r.contentProgress(ctx, ptr, cb.manifestBytes)
	if err != nil {
		return c, err
	}
	// The manifest is already cached by Content; only chunks can be missing.
	var missing []ContentObject
	seen := map[string]bool{}
	for _, o := range c.Objects {
		if o.Kind == ObjectKindManifest {
			continue
		}
		// A manifest may reference the same content-addressed chunk more than
		// once. It is one stored object and must be fetched and counted once.
		if seen[o.Digest] {
			continue
		}
		seen[o.Digest] = true
		// Presence, not verification. Every one of these objects is hashed
		// again on the way out — contentReader checks each chunk against its
		// manifest digest and the whole reconstruction against the pointer
		// oid, and materialize aborts before the atomic rename on mismatch —
		// so re-hashing the entire cached byte set here only bought the same
		// answer twice, once per load. The size check comes from the manifest
		// the caller has already validated, which catches a truncated entry.
		if r.Cache.HasSized(r.Domain, o.Digest, o.Size) {
			continue
		}
		missing = append(missing, o)
	}
	if cb.planned != nil {
		cb.planned(c, missing)
	}
	if len(missing) == 0 {
		return c, nil
	}
	digests := make([]string, len(missing))
	for i, o := range missing {
		digests[i] = o.Digest
	}
	if err := r.fetchManyProgress(ctx, digests, true, cb.fetched, cb.objectBytes); err != nil {
		return c, err
	}
	// Trimming happens after the whole asset is present, never between its
	// chunks, and never below what this asset needs: evicting a chunk we are
	// about to read would be self-defeating.
	r.enforceCacheLimitReserving(ctx, c.StoredBytes())
	return c, nil
}

// FetchConcurrency bounds simultaneous object downloads.
//
// Downloads are bound by round trips for the same reason uploads are: a
// chunked asset is many small objects, and a handful at a time leaves the link
// idle. It matches the publisher's default and the backend's idle-connection
// pool, so the workers reuse connections rather than renegotiating TLS.
const FetchConcurrency = 24

// fetchMany downloads objects concurrently and returns the first failure.
//
// With stopOnError the first failure cancels the rest, which is what a caller
// materializing one file wants: the file cannot be produced anyway. A
// warm-the-cache caller passes false, because one unavailable object must not
// stop the other files from being prepared — their own materialization will
// report their own errors.
func (r *Repo) fetchMany(ctx context.Context, digests []string, stopOnError bool, fetched func(digest string)) error {
	return r.fetchManyProgress(ctx, digests, stopOnError, fetched, nil)
}

// fetchManyProgress is fetchMany with byte-level reporting per object.
func (r *Repo) fetchManyProgress(ctx context.Context, digests []string, stopOnError bool, fetched func(digest string), onBytes func(digest string, delta int64)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, FetchConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, d := range digests {
		wg.Add(1)
		sem <- struct{}{}
		go func(digest string) {
			defer wg.Done()
			defer func() { <-sem }()
			var err error
			if onBytes == nil {
				err = r.ensureCached(ctx, digest)
			} else {
				err = r.ensureCachedProgress(ctx, digest, func(delta int64) { onBytes(digest, delta) })
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					if stopOnError {
						cancel()
					}
				}
				return
			}
			if fetched != nil {
				fetched(digest)
			}
		}(d)
	}
	wg.Wait()
	if stopOnError {
		return firstErr
	}
	return nil
}

// PrefetchProgress is Prefetch with byte-level reporting.
func (r *Repo) PrefetchProgress(ctx context.Context, ptrs []pointer.Pointer, fetched func(digest string), onBytes func(digest string, delta int64)) {
	r.prefetch(ctx, ptrs, fetched, onBytes)
}

// Prefetch warms the cache for many pointers at once.
//
// Materializing a file is two different kinds of work: fetching its objects,
// which is network-bound and independent per file, and writing it into the
// working tree, which is a compare-and-swap against the index and must stay
// serialized per path. Doing both in one loop makes the network work
// sequential too, so loading 600 assets from a remote store costs 600 round
// trips in series — minutes of waiting for a link that could have been
// saturated.
//
// Prefetch does the network half for the whole selection first, with bounded
// concurrency and shared deduplication (two assets that share a chunk fetch it
// once). It is best effort: an object that cannot be fetched is left alone, and
// the per-path operation reports it with the right error and outcome.
func (r *Repo) Prefetch(ctx context.Context, ptrs []pointer.Pointer, fetched func(digest string)) {
	r.prefetch(ctx, ptrs, fetched, nil)
}

func (r *Repo) prefetch(ctx context.Context, ptrs []pointer.Pointer, fetched func(digest string), onBytes func(digest string, delta int64)) {
	if len(ptrs) == 0 {
		return
	}
	// A chunked pointer names its chunks in a manifest object, so the
	// manifests have to arrive before the chunk list is even known. Fetch
	// them as their own concurrent wave rather than one at a time.
	var manifests []string
	seen := map[string]bool{}
	for _, p := range ptrs {
		if p.Encoding == pointer.EncodingWhole || seen[p.ManifestDigest] {
			continue
		}
		if !r.Cache.Has(r.Domain, p.ManifestDigest) {
			seen[p.ManifestDigest] = true
			manifests = append(manifests, p.ManifestDigest)
		}
	}
	if len(manifests) > 0 {
		_ = r.fetchManyProgress(ctx, manifests, false, fetched, onBytes)
	}
	// Now every resolvable manifest is local, so the chunk lists cost nothing.
	var missing []string
	want := map[string]bool{}
	for _, p := range ptrs {
		if err := ctx.Err(); err != nil {
			return
		}
		c, err := r.Content(ctx, p)
		if err != nil {
			continue // reported per path when the file is materialized
		}
		for _, o := range c.Objects {
			if want[o.Digest] || r.Cache.Has(r.Domain, o.Digest) {
				continue
			}
			want[o.Digest] = true
			missing = append(missing, o.Digest)
		}
	}
	if len(missing) > 0 {
		_ = r.fetchManyProgress(ctx, missing, false, fetched, onBytes)
	}
}

// OpenContent returns a reader over the whole file behind a pointer,
// reconstructed from the local cache and verified as it streams: every chunk
// is checked against its own digest, the total length against the pointer
// size, and — for chunked encodings — the concatenation against the pointer
// oid at EOF. Callers must treat any error as "no valid content".
//
// It does not fetch; call EnsureContent first.
func (r *Repo) OpenContent(ctx context.Context, ptr pointer.Pointer) (io.ReadCloser, error) {
	c, err := r.Content(ctx, ptr)
	if err != nil {
		return nil, err
	}
	cr := &contentReader{repo: r, ptr: ptr, chunks: c.Manifest.Chunks}
	if c.Chunked() {
		// For whole/1 the single chunk digest *is* the oid, so the cache
		// reader's own check already proves the whole-file digest.
		cr.whole = sha256.New()
	}
	return cr, nil
}

// contentReader concatenates the cached chunks of one object.
type contentReader struct {
	repo   *Repo
	ptr    pointer.Pointer
	chunks []domain.ManifestChunk
	idx    int
	cur    *cache.Reader
	whole  hash.Hash
	n      int64
	failed error
	closed bool
}

func (c *contentReader) Read(p []byte) (int, error) {
	if c.failed != nil {
		return 0, c.failed
	}
	for {
		if c.cur == nil {
			if c.idx >= len(c.chunks) {
				return 0, c.finish()
			}
			ch := c.chunks[c.idx]
			rd, err := c.repo.Cache.Open(c.repo.Domain, ch.Digest)
			if err != nil {
				return 0, c.fail(fmt.Errorf("%w: chunk %d/%d (%s): %v", ErrObjectUnavailable, c.idx+1, len(c.chunks), ch.Digest, err))
			}
			if rd.Size() != ch.RawSize {
				_ = rd.Close()
				return 0, c.fail(fmt.Errorf("%w: %w", ErrIntegrity,
					&cache.CorruptError{Digest: ch.Digest, Size: rd.Size(), Want: ch.RawSize}))
			}
			c.cur = rd
		}
		n, err := c.cur.Read(p)
		if n > 0 {
			c.n += int64(n)
			if c.whole != nil {
				c.whole.Write(p[:n])
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			_ = c.cur.Close()
			c.cur = nil
			c.idx++
			if n > 0 {
				return n, nil
			}
		case err != nil:
			if errors.Is(err, cache.ErrCorrupt) {
				// Wrap, but keep the cache error reachable by errors.As: the
				// digest it names is what a repairing caller has to evict.
				err = fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			return n, c.fail(err)
		default:
			return n, nil
		}
	}
}

// finish runs the end-of-stream checks exactly once.
func (c *contentReader) finish() error {
	if c.n != c.ptr.Size {
		return c.fail(fmt.Errorf("%w: reconstructed %d bytes, pointer size %d", ErrIntegrity, c.n, c.ptr.Size))
	}
	if c.whole != nil {
		if got := cache.Digest(c.whole.Sum(nil)); got != c.ptr.OID {
			return c.fail(fmt.Errorf("%w: reconstructed content hashes to %s, pointer oid %s", ErrIntegrity, got, c.ptr.OID))
		}
	}
	return io.EOF
}

func (c *contentReader) fail(err error) error {
	c.failed = err
	return err
}

// Close releases the chunk currently open.
func (c *contentReader) Close() error {
	c.closed = true
	if c.cur != nil {
		err := c.cur.Close()
		c.cur = nil
		return err
	}
	return nil
}

// HasContent reports whether every object behind the pointer is present
// locally. It does not read object bytes and never contacts a remote, so it is
// cheap enough for status listings; a chunked pointer whose manifest is not
// cached answers false.
func (r *Repo) HasContent(ctx context.Context, ptr pointer.Pointer) bool {
	if ptr.Encoding == pointer.EncodingWhole {
		return r.Cache.Has(r.Domain, ptr.OID)
	}
	if !r.Cache.Has(r.Domain, ptr.ManifestDigest) {
		return false
	}
	c, err := r.Content(ctx, ptr)
	if err != nil {
		return false
	}
	for _, o := range c.Objects {
		if !r.Cache.Has(r.Domain, o.Digest) {
			return false
		}
	}
	return true
}

// ContentDigests returns every object digest behind a pointer, or an error
// when the manifest cannot be resolved.
func (r *Repo) ContentDigests(ctx context.Context, ptr pointer.Pointer) ([]string, error) {
	c, err := r.Content(ctx, ptr)
	if err != nil {
		return nil, err
	}
	return c.Digests(), nil
}
