package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Reading recorded content without touching the working tree.
//
// A caller that wants to show an asset as it was committed cannot get it from
// Git — Git holds the pointer — and must not get it by hydrating, because
// hydrating overwrites the working file, which in the case that matters is the
// modified file being compared. These functions read the content behind a
// pointer straight out of the local object cache and give back a stream.
//
// They never contact a remote. "Not on this machine" is a different answer from
// "damaged" or "no such thing", and it is the answer an interface can act on by
// offering a download; a read that quietly fetched hundreds of megabytes
// because a preview panel opened would be the wrong behaviour with the right
// bytes.

// ContentLocal resolves the storage plan for a pointer using only what is
// already cached.
//
// It differs from Content in one respect that matters here: a chunked pointer
// names its chunks in a manifest object, and Content fetches that manifest from
// a remote when it is missing. This reports ErrObjectNotCached instead, so a
// caller asking about content it may not have does not trigger a download by
// asking.
func (r *Repo) ContentLocal(ctx context.Context, ptr pointer.Pointer) (Content, error) {
	if err := r.checkPointer(ptr); err != nil {
		return Content{}, err
	}
	if ptr.Encoding != pointer.EncodingWhole && !r.Cache.Has(r.Domain, ptr.ManifestDigest) {
		return Content{}, notCached(ptr.ManifestDigest, "object manifest")
	}
	return r.Content(ctx, ptr)
}

// checkPointer rejects a pointer this repository must not read: malformed, or
// belonging to another storage domain. The domain check is not a formality —
// content is never deduplicated across access boundaries, so a pointer from
// another domain naming a digest this cache happens to hold must not resolve.
func (r *Repo) checkPointer(ptr pointer.Pointer) error {
	if err := ptr.Validate(); err != nil {
		return err
	}
	if ptr.StorageDomainID != r.Domain {
		return fmt.Errorf("%w: pointer storage domain %s does not match repository domain %s", ErrIntegrity, ptr.StorageDomainID, r.Domain)
	}
	return nil
}

func notCached(digest, what string) error {
	return fmt.Errorf("%w: %s %s is not in the local object cache", ErrObjectNotCached, what, digest)
}

// ObjectInfo is what is known locally about the content behind a pointer.
type ObjectInfo struct {
	Digest string
	Size   int64
	// Cached is true when every object behind the pointer is present locally
	// at the size the manifest records.
	//
	// Present, not proven: it is the condition under which a read can proceed
	// without the network, not a guarantee that the read will succeed. The
	// bytes are verified as they stream — a damaged object fails the read
	// rather than being handed over — so establishing this cheaply is right,
	// and a caller must still handle a read failing.
	Cached bool
	// Objects counts the stored objects the content is made of: one for a
	// whole/1 asset, a manifest plus a chunk per piece for a chunked one.
	// Zero when the manifest itself is not cached and the count is unknown.
	Objects int
	// Encoding is the pointer encoding (whole/1, cdc-fastcdc/1).
	Encoding string
}

// StatContent reports what is known locally about a pointer's content without
// reading or fetching it. It answers the question an interface has to settle
// before it offers a comparison at all: can this be read right now.
func (r *Repo) StatContent(ctx context.Context, ptr pointer.Pointer) (ObjectInfo, error) {
	if err := r.checkPointer(ptr); err != nil {
		return ObjectInfo{}, err
	}
	info := ObjectInfo{Digest: ptr.OID, Size: ptr.Size, Encoding: ptr.Encoding}
	c, err := r.ContentLocal(ctx, ptr)
	if errors.Is(err, ErrObjectNotCached) {
		return info, nil // resolvable only once the manifest is here
	}
	if err != nil {
		return info, err
	}
	info.Objects = len(c.Objects)
	info.Cached = true
	for _, o := range c.Objects {
		if !r.Cache.Has(r.Domain, o.Digest) {
			info.Cached = false
			break
		}
	}
	return info, nil
}

// OpenContentAt returns a reader over the content behind a pointer, from the
// local cache only, optionally restricted to a byte range. offset 0 with
// length 0 (or length covering the rest) is the whole content.
//
// The returned reader outlives the Repo: it holds open cache files, not
// repository state.
func (r *Repo) OpenContentAt(ctx context.Context, ptr pointer.Pointer, offset, length int64) (io.ReadCloser, error) {
	c, err := r.ContentLocal(ctx, ptr)
	if err != nil {
		return nil, err
	}
	for _, o := range c.Objects {
		if !r.Cache.Has(r.Domain, o.Digest) {
			return nil, notCached(o.Digest, "object")
		}
	}
	start, end, err := clampRange(offset, length, ptr.Size)
	if err != nil {
		return nil, err
	}
	if start == 0 && end == ptr.Size {
		// The whole content: the existing reader also checks the
		// reconstruction against the pointer oid at EOF, which a partial read
		// cannot do. Prefer it whenever it applies.
		return r.OpenContent(ctx, ptr)
	}
	return &rangeReader{repo: r, items: planRange(c.Manifest.Chunks, start, end)}, nil
}

// clampRange turns a caller's offset/length into an absolute [start, end).
// A range starting at or past the end of the content is empty rather than an
// error: asking for byte 5000 of a 4096-byte file is how HTTP range requests
// discover they have reached the end.
func clampRange(offset, length, size int64) (start, end int64, err error) {
	if offset < 0 || length < 0 {
		return 0, 0, fmt.Errorf("%w: offset %d length %d: neither may be negative", ErrInvalidArgument, offset, length)
	}
	start = min(offset, size)
	end = size
	if length > 0 {
		end = min(start+length, size)
	}
	return start, end, nil
}

// rangeItem is one chunk's part in a ranged read: skip the first Skip bytes,
// emit the next Take, and read whatever is left so the chunk's digest check
// still runs over its whole content.
type rangeItem struct {
	Digest string
	Skip   int64
	Take   int64
}

// planRange decides which chunks a byte range touches. Chunks outside the
// range are never opened at all — which is the point of a range: seeking into
// a large asset must not cost a read of everything before it.
func planRange(chunks []domain.ManifestChunk, start, end int64) []rangeItem {
	var items []rangeItem
	var pos int64
	for _, ch := range chunks {
		chunkStart, chunkEnd := pos, pos+ch.RawSize
		pos = chunkEnd
		if chunkEnd <= start || chunkStart >= end {
			continue
		}
		skip := max(0, start-chunkStart)
		take := min(chunkEnd, end) - (chunkStart + skip)
		if take <= 0 {
			continue // a zero-width overlap: opening the chunk would buy nothing
		}
		items = append(items, rangeItem{Digest: ch.Digest, Skip: skip, Take: take})
	}
	return items
}

// rangeReader emits a byte range of a pointer's content from the cache.
//
// Every chunk it opens is read to the end before it moves on, even when the
// range needs only part of it: the cache verifies an object's digest at EOF,
// and stopping early would mean serving bytes that were never checked. The
// waste is bounded by the chunk size at each end of the range (a whole/1 asset
// is under the chunking floor, and a chunked one is split into pieces of about
// that size), while every chunk the range does not touch costs nothing at all.
//
// The whole-content digest is the one check a ranged read cannot make, since
// it is defined over bytes it deliberately does not read. What remains is not
// weak: the manifest is validated against the pointer before any chunk digest
// in it is trusted (see decodeManifest), and each chunk is verified against
// its manifest entry, so the bytes are pinned to the pointer by construction.
type rangeReader struct {
	repo  *Repo
	items []rangeItem
	idx   int
	cur   *cache.Reader
	take  int64
	err   error
}

func (rr *rangeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if rr.err != nil {
			return 0, rr.err
		}
		if rr.cur == nil {
			if rr.idx >= len(rr.items) {
				return 0, io.EOF
			}
			if err := rr.open(rr.items[rr.idx]); err != nil {
				return 0, err
			}
		}
		if rr.take == 0 {
			// Finish the chunk (draining whatever the range did not want) so
			// its digest is checked, then move to the next one.
			if err := rr.closeChunk(); err != nil {
				return 0, err
			}
			continue
		}
		n, err := rr.cur.Read(p[:min(int64(len(p)), rr.take)])
		rr.take -= int64(n)
		if err != nil && !errors.Is(err, io.EOF) {
			return n, rr.fail(err)
		}
		if errors.Is(err, io.EOF) && rr.take > 0 {
			return n, rr.fail(fmt.Errorf("%w: chunk %s ended early", ErrIntegrity, rr.items[rr.idx].Digest))
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (rr *rangeReader) open(it rangeItem) error {
	rd, err := rr.repo.Cache.Open(rr.repo.Domain, it.Digest)
	if err != nil {
		return rr.fail(fmt.Errorf("%w: chunk %s: %v", ErrObjectUnavailable, it.Digest, err))
	}
	if it.Skip > 0 {
		if _, err := io.CopyN(io.Discard, rd, it.Skip); err != nil {
			_ = rd.Close()
			return rr.fail(corrupt(err))
		}
	}
	rr.cur, rr.take = rd, it.Take
	return nil
}

// closeChunk reads the remainder of the current chunk so the cache's
// verify-at-EOF fires, then releases it.
func (rr *rangeReader) closeChunk() error {
	_, err := io.Copy(io.Discard, rr.cur)
	cerr := rr.cur.Close()
	rr.cur = nil
	rr.idx++
	if err != nil {
		return rr.fail(corrupt(err))
	}
	if cerr != nil {
		return rr.fail(cerr)
	}
	return nil
}

func (rr *rangeReader) fail(err error) error {
	rr.err = err
	return err
}

// Close releases the chunk currently open. Chunks not yet reached were never
// opened.
func (rr *rangeReader) Close() error {
	if rr.cur == nil {
		return nil
	}
	err := rr.cur.Close()
	rr.cur = nil
	return err
}

func corrupt(err error) error {
	if errors.Is(err, cache.ErrCorrupt) {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	return err
}

// FetchContent downloads every object behind a pointer into the local cache
// and reports what it took. It writes nothing to the working tree, which is
// what separates it from hydrate: it makes content readable, it does not
// decide what a file on disk should be.
func (r *Repo) FetchContent(ctx context.Context, ptr pointer.Pointer, progress ProgressFunc) (ObjectInfo, error) {
	if err := r.checkPointer(ptr); err != nil {
		return ObjectInfo{}, err
	}
	progress.emit(ItemProgress{Stage: StageResolving, Count: 1})
	progress.emit(ItemProgress{Digest: ptr.OID, Phase: ProgressPhaseTransfer})

	var mu sync.Mutex
	useful := map[string]int64{}
	wire := map[string]int64{}
	sizes := map[string]int64{}
	partIndex := map[string]int{}
	doneParts := map[string]bool{}
	var total, totalUseful, totalWire int64
	partCount, partsDone := 0, 0

	reportLocked := func(part string) ItemProgress {
		return ItemProgress{
			Digest: ptr.OID, Bytes: totalUseful, Total: total, Fetched: true,
			Chunks: partCount, ChunksDone: partsDone, Phase: ProgressPhaseTransfer,
			WireBytes: totalWire, PartID: part, PartBytes: useful[part], PartTotal: sizes[part],
			PartWireBytes: wire[part], PartIndex: partIndex[part], PartCount: partCount,
		}
	}
	onBytes := func(digest string, delta int64) {
		mu.Lock()
		defer mu.Unlock()
		useful[digest] += delta
		totalUseful += delta
		if delta > 0 {
			wire[digest] += delta
			totalWire += delta
		}
		progress.emit(reportLocked(digest))
	}
	markFetched := func(digest string) {
		mu.Lock()
		defer mu.Unlock()
		if !doneParts[digest] {
			doneParts[digest] = true
			partsDone++
		}
		progress.emit(reportLocked(digest))
	}

	c, err := r.ensureContentProgress(ctx, ptr, ensureContentCallbacks{
		manifestBytes: onBytes,
		planned: func(content Content, missing []ContentObject) {
			mu.Lock()
			defer mu.Unlock()
			transfer := make([]ContentObject, 0, len(missing)+1)
			if wire[ptr.ManifestDigest] > 0 {
				for _, o := range content.Objects {
					if o.Kind == ObjectKindManifest {
						transfer = append(transfer, o)
						break
					}
				}
			}
			transfer = append(transfer, missing...)
			for i, o := range transfer {
				sizes[o.Digest] = o.Size
				partIndex[o.Digest] = i + 1
				total += o.Size
			}
			partCount = len(transfer)
			reused := content.StoredBytes() - total
			if reused < 0 {
				reused = 0
			}
			reusedItems := 0
			if total == 0 {
				reusedItems = 1
			}
			progress.emit(ItemProgress{Planned: 1, PlannedBytes: total, PlanPhase: ProgressPhaseTransfer,
				ReusedItems: reusedItems, ReusedBytes: reused})
			if wire[ptr.ManifestDigest] > 0 {
				doneParts[ptr.ManifestDigest] = true
				partsDone++
				progress.emit(reportLocked(ptr.ManifestDigest))
			}
			progress.emit(ItemProgress{Stage: StageTransferring, Count: 1})
		},
		objectBytes: onBytes,
		fetched:     markFetched,
	})
	if err != nil {
		return ObjectInfo{}, err
	}
	mu.Lock()
	progress.emit(ItemProgress{Digest: ptr.OID, Bytes: totalUseful, Total: total, Done: true,
		Chunks: partCount, ChunksDone: partsDone, Phase: ProgressPhaseTransfer, WireBytes: totalWire})
	mu.Unlock()
	return ObjectInfo{Digest: ptr.OID, Size: ptr.Size, Cached: true, Objects: len(c.Objects), Encoding: ptr.Encoding}, nil
}
