package filter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Errors surfaced by the engine.
var (
	// ErrUnsupportedPointer wraps pointer.ErrUnsupportedVersion with the
	// installed/required detail required at the byte boundary.
	ErrUnsupportedPointer = errors.New("filter: unsupported pointer format version")
	// ErrMalformedPointer is a blob that starts like a pointer but is invalid.
	ErrMalformedPointer = errors.New("filter: malformed pointer")
	// ErrObjectMismatch is a cached object whose bytes/size disagree with the
	// pointer; the working file is never written from it.
	ErrObjectMismatch = errors.New("filter: cached object does not match pointer")
)

// Engine performs clean and smudge. With a Backend it ingests through the
// engine's single ingestion path (cache pin + publication ledger row),
// consults the recorded hydration intent, and may fetch from configured
// remotes; without one (unit tests, degraded clone-in-progress mode) it works
// over the local object cache alone and never records anything.
type Engine struct {
	Cache *cache.Cache
	// Domain is the storageDomainID new objects are ingested under (clean).
	Domain string
	// Installed is the running binary version, for diagnostics.
	Installed format.Version
	// Backend is optional (see type comment).
	Backend Backend
}

// ErrObjectUnavailable is returned by smudge for a path recorded as hydrated
// whose object is neither cached nor obtainable from a configured remote.
var ErrObjectUnavailable = errors.New("filter: object not available locally")

// Compile-time check.
var _ Handler = (*Engine)(nil)

// readHead reads up to pointer.MaxSize+1 bytes so the caller can decide
// whether the whole content is a pointer without buffering large files.
func readHead(r io.Reader) ([]byte, error) {
	head := make([]byte, pointer.MaxSize+1)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return head[:n], nil
}

// classify decides what a content prefix is.
type kind int

const (
	kindRaw     kind = iota // not a pointer: real asset bytes or historical blob
	kindPointer             // a valid pointer
)

// classifyHead returns the kind and, for pointers, the decoded pointer.
// Sniffed-but-invalid content is an error (fail closed).
func (e *Engine) classifyHead(head []byte, whole bool) (kind, pointer.Pointer, error) {
	if !whole || !pointer.Sniff(head) {
		return kindRaw, pointer.Pointer{}, nil
	}
	p, err := pointer.Decode(head)
	if err == nil {
		return kindPointer, p, nil
	}
	if errors.Is(err, pointer.ErrUnsupportedVersion) {
		return kindRaw, pointer.Pointer{}, fmt.Errorf("%w: %v; installed fxvcs %s supports pointer format v%d; run: fxvcs upgrade --check",
			ErrUnsupportedPointer, err, e.Installed, pointer.FormatVersion)
	}
	return kindRaw, pointer.Pointer{}, fmt.Errorf("%w: %v", ErrMalformedPointer, err)
}

// Clean converts working-tree bytes into a pointer, ingesting the object into
// the cache and pinning it as pending publication. Content that already is a
// valid pointer passes through unchanged (idempotent). It streams: memory is
// bounded by pointer.MaxSize plus copy buffers regardless of file size.
func (e *Engine) Clean(ctx context.Context, req Request, content io.Reader, out io.Writer) error {
	head, err := readHead(content)
	if err != nil {
		return err
	}
	if len(head) == 0 {
		// Zero-length content is stored inline as the empty blob, never as a
		// pointer. This is required for correctness, not an optimisation.
		//
		// Git records the working file's size in the index and, in
		// ce_match_stat_basic(), treats a recorded size of 0 as "racily
		// smudged, size unknown" and forces DATA_CHANGED -- unless the index
		// blob is the empty blob. A pointer for an empty file therefore makes
		// the entry permanently stat-dirty: `git diff --quiet` exits 1 while
		// `git status`, which goes on to compare content, reports clean. That
		// disagreement makes `git stash create` exit 1 and every `git merge`
		// die with "fatal: stash failed".
		//
		// An empty file also has no bytes to store, so there is nothing to
		// ingest, pin, publish or hydrate: the empty blob round-trips through
		// smudge unchanged (it does not sniff as a pointer, so it passes
		// through as raw content).
		return nil
	}
	whole := len(head) <= pointer.MaxSize
	k, _, err := e.classifyHead(head, whole)
	if err != nil {
		return err
	}
	if k == kindPointer {
		_, err := out.Write(head)
		return err
	}
	body := io.MultiReader(bytes.NewReader(head), content)
	var ptr pointer.Pointer
	if e.Backend != nil {
		p, err := e.Backend.Ingest(ctx, body, req.Pathname)
		if err != nil {
			return err
		}
		ptr = p
		// The working file now holds real bytes: remember the hydrated intent
		// so a later checkout of this path fails loudly instead of quietly
		// substituting a pointer when the object is unavailable.
		if req.Pathname != "" {
			_ = e.Backend.RecordHydrated(ctx, req.Pathname, ptr.OID, ptr.Size)
		}
	} else {
		if e.Cache == nil {
			return errors.New("filter: no object cache configured")
		}
		if e.Domain == "" {
			return errors.New("filter: repository has no storageDomainID")
		}
		res, err := e.Cache.Put(e.Domain, body)
		if err != nil {
			return err
		}
		p, err := WholePointer(e.Domain, res.Digest, res.Size)
		if err != nil {
			return err
		}
		if err := e.Cache.Pin(e.Domain, res.Digest); err != nil {
			return fmt.Errorf("filter: pin pending object: %w", err)
		}
		ptr = p
	}
	enc, err := ptr.Encode()
	if err != nil {
		return err
	}
	_, err = out.Write(enc)
	return err
}

// WholeManifest builds the whole/1, uncompressed manifest for an object.
func WholeManifest(domainID, oid string, size int64) domain.ObjectManifest {
	return domain.ObjectManifest{
		FormatVersion:   format.ObjectManifestVersion,
		StorageDomainID: domainID,
		Encoding:        pointer.EncodingWhole,
		Compression:     "none",
		OID:             oid,
		Size:            size,
		Chunks:          []domain.ManifestChunk{{Digest: oid, RawSize: size, EncodedSize: size}},
	}
}

// WholePointer builds the deterministic pointer for a whole/1 object.
func WholePointer(domainID, oid string, size int64) (pointer.Pointer, error) {
	md, err := integrity.ManifestDigest(WholeManifest(domainID, oid, size))
	if err != nil {
		return pointer.Pointer{}, err
	}
	p := pointer.Pointer{
		StorageDomainID: domainID,
		Encoding:        pointer.EncodingWhole,
		ManifestDigest:  md,
		OID:             oid,
		Size:            size,
	}
	if err := p.Validate(); err != nil {
		return pointer.Pointer{}, err
	}
	return p, nil
}

// Smudge converts a pointer into working-tree bytes according to the
// recorded hydration intent of the path:
//
//   - no Backend, no state row, or intent unhydrated: emit the pointer bytes
//     unchanged (the unhydrated default; a fresh clone holds pointers even
//     when the host cache happens to contain the object);
//   - intent hydrated: reconstruct from the cache, else fetch from a
//     configured remote into the cache, else fail the Git operation
//     (ErrObjectUnavailable) — never silently substitute a pointer for a
//     path the user hydrated.
//
// Non-pointer input passes through unchanged. It never emits partial or
// unverified content as success.
func (e *Engine) Smudge(ctx context.Context, req Request, content io.Reader, out io.Writer) error {
	head, err := readHead(content)
	if err != nil {
		return err
	}
	whole := len(head) <= pointer.MaxSize
	k, ptr, err := e.classifyHead(head, whole)
	if err != nil {
		return err
	}
	if k == kindRaw {
		if _, err := out.Write(head); err != nil {
			return err
		}
		_, err := io.Copy(out, content)
		return err
	}
	if ptr.Encoding == pointer.EncodingWhole {
		// The manifest digest is derived; a pointer whose manifest does not
		// match its own oid/size is inconsistent and must not hydrate. This
		// check is free, so it runs before the intent lookup.
		want, err := WholePointer(ptr.StorageDomainID, ptr.OID, ptr.Size)
		if err != nil {
			return err
		}
		if want.ManifestDigest != ptr.ManifestDigest {
			return fmt.Errorf("%w: manifest %s does not match whole/1 derivation %s", ErrMalformedPointer, ptr.ManifestDigest, want.ManifestDigest)
		}
	}
	if e.Domain != "" && ptr.StorageDomainID != e.Domain {
		return fmt.Errorf("%w: pointer storage domain %s does not match repository domain %s", ErrMalformedPointer, ptr.StorageDomainID, e.Domain)
	}
	hydrated := false
	if e.Backend != nil {
		h, known, err := e.Backend.HydrationIntent(ctx, req.Pathname)
		if err != nil {
			return err
		}
		hydrated = known && h
	}
	if !hydrated {
		_, err := out.Write(head)
		return err
	}
	// With an engine behind it, reconstruction goes through the same content
	// layer as `fxvcs load`: it resolves the object manifest, fetches what
	// is missing, and verifies every chunk and the whole file.
	if e.Backend != nil {
		rc, err := e.Backend.OpenContent(ctx, ptr)
		if err != nil {
			return fmt.Errorf("%w: object %s for loaded path %s could not be reconstructed (%v); run fxvcs load %s",
				ErrObjectUnavailable, ptr.OID, req.Pathname, err, req.Pathname)
		}
		defer rc.Close()
		_, err = io.Copy(out, rc)
		return err
	}
	// Without one (unit tests, degraded clone-in-progress mode) only the
	// cache is available, and only whole/1 can be rebuilt from it alone.
	if ptr.Encoding != pointer.EncodingWhole || e.Cache == nil {
		_, err := out.Write(head)
		return err
	}
	r, err := e.Cache.Open(ptr.StorageDomainID, ptr.OID)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			return fmt.Errorf("%w: object %s for loaded path %s; run fxvcs load %s", ErrObjectUnavailable, ptr.OID, req.Pathname, req.Pathname)
		}
		return err
	}
	defer r.Close()
	if r.Size() != ptr.Size {
		return fmt.Errorf("%w: cached size %d, pointer size %d", ErrObjectMismatch, r.Size(), ptr.Size)
	}
	if _, err := io.Copy(out, r); err != nil {
		if errors.Is(err, cache.ErrCorrupt) {
			return fmt.Errorf("%w: %v", ErrObjectMismatch, err)
		}
		return err
	}
	return nil
}
