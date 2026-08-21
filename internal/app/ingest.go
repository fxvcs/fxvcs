package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// Ingested describes the objects a single ingestion durably recorded.
type Ingested struct {
	Pointer  pointer.Pointer
	Digest   string // whole-file sha256 (== Pointer.OID)
	Size     int64
	Manifest domain.ObjectManifest
	// Objects lists every stored object the pointer depends on: for whole/1
	// the file itself, for a chunked encoding the manifest and its chunks.
	Objects []ContentObject
	// Existing is true when nothing new had to be written: every object was
	// already cached and recorded.
	Existing bool
}

// Ingest streams content into the local immutable cache under this
// repository's storage domain, pins the objects it creates (pending
// publication), records them in the shared publication ledger, and returns the
// deterministic pointer. It is the single ingestion path used by the Git clean
// filter and by `migrate`; it needs no network and is idempotent for identical
// bytes.
//
// The encoding is chosen by chunk.DefaultPolicy, which is a property of the
// format generation rather than of this machine: identical bytes must produce
// an identical pointer everywhere, or Git would report a modification whenever
// a differently configured client restaged the file. Small objects stay
// whole/1 exactly as in Stage 1.
//
// Ordering matters for crash safety: object bytes first (atomic rename), then
// pins, then ledger rows, and for chunked encodings the manifest object is
// written only after every chunk it names. A crash between steps leaves at
// worst an unpinned or unrecorded cached object, which repair/rebuild can
// reconcile; it never leaves a pointer whose objects are missing.
func (r *Repo) Ingest(ctx context.Context, content io.Reader, referrerPath string) (Ingested, error) {
	return r.ingest(ctx, content, referrerPath, ingestHint{})
}

// IngestExpecting is Ingest for a caller that already knows what the content
// must hash to — re-deriving a working file from its own index pointer.
//
// The hint never changes what is stored or what pointer comes back; it only
// lets the whole-blob path recognise that the object it is about to write is
// already cached, and skip writing a second copy of a file that can be several
// hundred megabytes. The stream is still read and hashed in full, so the
// content is proven, and a mismatch is reported rather than stored.
func (r *Repo) IngestExpecting(ctx context.Context, content io.Reader, referrerPath string, expect pointer.Pointer) (Ingested, error) {
	return r.ingest(ctx, content, referrerPath, ingestHint{oid: expect.OID, size: expect.Size})
}

// ingestHint carries a caller's advance knowledge of the content identity.
// The zero value means "not known", which is the Git clean filter's case.
type ingestHint struct {
	oid  string
	size int64
}

func (h ingestHint) known() bool { return h.oid != "" && h.size >= 0 }

func (r *Repo) ingest(ctx context.Context, content io.Reader, referrerPath string, hint ingestHint) (Ingested, error) {
	if err := ctx.Err(); err != nil {
		return Ingested{}, err
	}
	policy, err := r.ChunkPolicy()
	if err != nil {
		return Ingested{}, err
	}
	// Decide the encoding from the real size, never from "the size is not
	// known yet": the same bytes must not get two different pointers because
	// one arrived as a stream and the other as a file.
	floor := policy.AtLeastMinChunked()
	encoding := policy.Encoding(referrerPath, floor)
	if encoding == chunk.EncodingWhole {
		// This path is never chunked (policy off, or a measured-no-benefit
		// extension): no lookahead, no buffering.
		return r.ingestWhole(ctx, content, referrerPath, hint)
	}
	head, short, err := lookahead(content, floor)
	if err != nil {
		return Ingested{}, err
	}
	body := io.MultiReader(bytes.NewReader(head), content)
	if short && policy.Encoding(referrerPath, int64(len(head))) == chunk.EncodingWhole {
		return r.ingestWhole(ctx, body, referrerPath, hint)
	}
	return r.ingestChunked(ctx, body, referrerPath, encoding)
}

// lookahead reads up to n bytes. short reports that the stream ended within
// those n bytes, so len(head) is the whole content length.
func lookahead(r io.Reader, n int64) (head []byte, short bool, err error) {
	if n <= 0 {
		return nil, false, nil
	}
	buf := make([]byte, n)
	read, err := io.ReadFull(r, buf)
	switch {
	case err == nil:
		return buf, false, nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:read], true, nil
	default:
		return nil, false, err
	}
}

// ingestWhole stores the content as one object (the Stage 1 encoding).
func (r *Repo) ingestWhole(ctx context.Context, content io.Reader, referrerPath string, hint ingestHint) (Ingested, error) {
	put := r.Cache.Put
	if hint.known() {
		put = func(domain string, rd io.Reader) (cache.PutResult, error) {
			return r.Cache.PutExpecting(domain, hint.oid, hint.size, rd)
		}
	}
	res, err := put(r.Domain, content)
	if err != nil {
		return Ingested{}, err
	}
	man := filter.WholeManifest(r.Domain, res.Digest, res.Size)
	ptr, err := filter.WholePointer(r.Domain, res.Digest, res.Size)
	if err != nil {
		return Ingested{}, err
	}
	objs := []ContentObject{{Digest: res.Digest, Size: res.Size, Kind: ObjectKindWhole}}
	existing, err := r.recordObjects(ctx, objs, ptr, referrerPath)
	if err != nil {
		return Ingested{}, err
	}
	r.enforceCacheLimit(ctx)
	return Ingested{Pointer: ptr, Digest: res.Digest, Size: res.Size, Manifest: man, Objects: objs, Existing: existing && res.Existed}, nil
}

// ingestChunked splits the content, stores each chunk, then stores the
// canonical manifest that names them.
func (r *Repo) ingestChunked(ctx context.Context, content io.Reader, referrerPath, encoding string) (Ingested, error) {
	params, err := chunk.ParamsFor(encoding)
	if err != nil {
		return Ingested{}, err
	}
	whole := sha256.New()
	var chunks []domain.ManifestChunk
	var objs []ContentObject
	var total int64
	allExisted := true
	err = chunk.Split(io.TeeReader(content, whole), params, func(c chunk.Chunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := r.Cache.PutBytes(r.Domain, c.Data)
		if err != nil {
			return err
		}
		if !res.Existed {
			allExisted = false
		}
		chunks = append(chunks, domain.ManifestChunk{Digest: res.Digest, RawSize: res.Size, EncodedSize: res.Size})
		objs = append(objs, ContentObject{Digest: res.Digest, Size: res.Size, Kind: ObjectKindChunk})
		total += res.Size
		return nil
	})
	if err != nil {
		return Ingested{}, err
	}
	oid := cache.Digest(whole.Sum(nil))
	man := domain.ObjectManifest{
		FormatVersion:   format.ObjectManifestVersion,
		StorageDomainID: r.Domain,
		Encoding:        params.Encoding,
		Compression:     "none",
		OID:             oid,
		Size:            total,
		Chunks:          chunks,
	}
	manifestDigest, err := integrity.ManifestDigest(man)
	if err != nil {
		return Ingested{}, err
	}
	manifestBytes, err := canonical.Marshal(man)
	if err != nil {
		return Ingested{}, err
	}
	// The manifest is itself an immutable object: it must travel with the
	// chunks, or a fresh clone could not learn which chunks to fetch. It is
	// stored through the checked put, so re-deriving a file also repairs a
	// damaged manifest instead of accepting the one that is already there.
	res, err := r.Cache.PutBytesChecked(r.Domain, manifestBytes)
	if err != nil {
		return Ingested{}, err
	}
	if res.Digest != manifestDigest {
		return Ingested{}, fmt.Errorf("%w: stored manifest hashes to %s, canonical digest is %s", ErrIntegrity, res.Digest, manifestDigest)
	}
	if !res.Existed {
		allExisted = false
	}
	ptr := pointer.Pointer{StorageDomainID: r.Domain, Encoding: params.Encoding, ManifestDigest: manifestDigest, OID: oid, Size: total}
	if err := ptr.Validate(); err != nil {
		return Ingested{}, err
	}
	// Manifest first in the object list: a reader needs it before the chunks.
	objs = append([]ContentObject{{Digest: manifestDigest, Size: res.Size, Kind: ObjectKindManifest}}, objs...)
	recorded, err := r.recordObjects(ctx, objs, ptr, referrerPath)
	if err != nil {
		return Ingested{}, err
	}
	r.enforceCacheLimit(ctx)
	return Ingested{Pointer: ptr, Digest: oid, Size: total, Manifest: man, Objects: objs, Existing: allExisted && recorded}, nil
}

// recordObjects pins every object, records it in the shared ledger, and
// attaches the referring path. It reports whether all of them were already
// recorded. An object that is already published everywhere required is
// unpinned again so cache pruning stays possible.
func (r *Repo) recordObjects(ctx context.Context, objs []ContentObject, ptr pointer.Pointer, referrerPath string) (allExisted bool, err error) {
	l, err := r.Ledger(ctx)
	if err != nil {
		return false, err
	}
	// Read what is already recorded in one query, then write in one
	// transaction. Both halves used to be per object, and the ledger is
	// opened synchronous=FULL, so a chunked asset spent an fsync per chunk
	// twice over to record objects whose bytes were already durable.
	digests := make([]string, len(objs))
	for i, o := range objs {
		digests[i] = o.Digest
	}
	known, err := l.ObjectsMany(ctx, digests)
	if err != nil {
		return false, err
	}
	// Pin before recording: an object visible to the ledger must already be
	// protected from pruning. Pinning what is already pinned is a wasted file
	// creation, and re-derivation asks about objects that mostly are.
	// Every object is offered to the ledger, new or not: the insert is
	// ON CONFLICT DO NOTHING, and the referrer row must be attached even when
	// the content itself is already recorded — the same bytes can arrive under
	// a new path.
	allExisted = true
	rows := make([]state.ObjectRecord, 0, len(objs))
	var unpin []string
	for _, o := range objs {
		rec, existed := known[o.Digest]
		switch {
		case existed && !rec.Pending:
			// Settled: it stays evictable, and must not be re-pinned.
			unpin = append(unpin, o.Digest)
		default:
			// Pinning what is already pinned is a wasted file creation, and
			// re-deriving a file asks about objects that mostly are.
			if pinned, perr := r.Cache.IsPinned(r.Domain, o.Digest); perr != nil || !pinned {
				if err := r.Cache.Pin(r.Domain, o.Digest); err != nil {
					return false, fmt.Errorf("pin pending object: %w", err)
				}
			}
		}
		if !existed {
			allExisted = false
		}
		row := state.ObjectRecord{Digest: o.Digest, StorageDomainID: r.Domain, Size: o.Size,
			Encoding: o.Kind, IngestedNS: rec.IngestedNS}
		if o.Kind == ObjectKindWhole {
			row.ManifestDigest = ptr.ManifestDigest
		}
		rows = append(rows, row)
	}
	if err := l.RecordIngestMany(ctx, rows, referrerPath, "index"); err != nil {
		return false, err
	}
	for _, d := range unpin {
		_ = r.Cache.Unpin(r.Domain, d)
	}
	return allExisted, nil
}

// ChunkPolicy returns the encoding policy for this repository.
func (r *Repo) ChunkPolicy() (chunk.Policy, error) {
	// Guarded because unload re-derives content, and a re-derivation runs
	// inside a path worker: on a freshly opened repository two of them can
	// reach this memo before either has filled it.
	r.handlesMu.Lock()
	defer r.handlesMu.Unlock()
	if r.policy != nil {
		return *r.policy, nil
	}
	p, err := chunk.PolicyFromEnv()
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	r.policy = &p
	return p, nil
}
