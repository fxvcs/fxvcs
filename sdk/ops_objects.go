package sdk

import (
	"context"
	"errors"
	"io"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// ---- reading recorded content ----------------------------------------------

// Reading an asset as it was recorded, without touching the working tree.
//
// Git cannot supply this: for a managed path Git holds the pointer, not the
// content. Hydrate cannot either — it materializes into the working tree, which
// in the case these operations exist for is the modified file being compared.
//
// The content is named by its pointer, not by a digest. That is not a
// preference: a digest is a complete name only for a whole/1 asset. A chunked
// asset's content is stored as a manifest plus its chunks, and the asset's own
// oid is not one of the stored objects — nothing on disk or in the ledger is
// keyed by it. A digest-taking API would therefore work for small assets and
// fail for exactly the large ones worth streaming, which is a worse outcome
// than not offering it. The pointer, which callers already have from
// `git show <rev>:<path>`, names the content completely: domain, encoding,
// manifest, oid, and size.

type ObjectReadRequest struct {
	Path string `json:"path"`
	// Pointer is the pointer text as Git stores it for the path — the bytes of
	// `git show HEAD:<path>`, trailing newline included or not.
	Pointer string `json:"pointer"`
	// Offset and Length restrict the read to a byte range of the content.
	// Zero and zero is the whole thing. An Offset at or past the end yields an
	// empty reader rather than an error, which is what a range request that
	// has run off the end of a file expects.
	Offset int64 `json:"offset,omitempty"`
	Length int64 `json:"length,omitempty"`
}

type ObjectReadResult struct {
	Digest string `json:"digest"`
	// Size is the whole content's size, whatever range was asked for.
	Size int64 `json:"size"`
	// Offset and Length describe the bytes Content will produce.
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
	// Content streams the bytes. The caller owns it and must Close it; it
	// stays valid after this client is done with the repository.
	Content io.ReadCloser `json:"-"`
}

// ObjectRead streams recorded content from the local object cache.
//
// It never writes to the working tree and never contacts a remote. Content
// that is not on this machine is refused with CodeNotCached — a distinct
// answer from CodeNotFound, and the one an interface can act on by offering
// ObjectFetch instead of reporting a fault.
//
// Chunked content is reassembled here, in the order its manifest records, and
// verified on the way past: the manifest is checked against the pointer before
// any digest inside it is trusted, and every chunk against its manifest entry.
// A whole read additionally checks the reconstruction against the pointer's
// oid. A ranged read cannot make that last check — it is defined over bytes
// the range deliberately does not read — but every chunk it touches is still
// verified in full.
func (c *Client) ObjectRead(ctx context.Context, req ObjectReadRequest) (*ObjectReadResult, error) {
	ptr, err := decodePointer(req.Pointer)
	if err != nil {
		return nil, err
	}
	r, err := c.openRepo(ctx, req.Path, "object read")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rc, err := r.OpenContentAt(ctx, ptr, req.Offset, req.Length)
	if err != nil {
		return nil, translateObjectError(r.Root, err)
	}
	offset, length := readSpan(req.Offset, req.Length, ptr.Size)
	return &ObjectReadResult{Digest: ptr.OID, Size: ptr.Size, Offset: offset, Length: length, Content: rc}, nil
}

// readSpan reports the range the reader will actually produce, clamped to the
// content, so a caller can set Content-Length without reading first.
func readSpan(offset, length, size int64) (int64, int64) {
	if offset > size {
		offset = size
	}
	end := size
	if length > 0 && offset+length < size {
		end = offset + length
	}
	return offset, end - offset
}

type ObjectStatRequest struct {
	Path    string `json:"path"`
	Pointer string `json:"pointer"`
}

type ObjectInfo struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	// Cached reports that every object behind the content is on this machine,
	// so ObjectRead needs no network. It is not a promise that the read
	// succeeds: content is verified while it streams, and a damaged local
	// object fails the read rather than being returned.
	Cached bool `json:"cached"`
	// Objects counts the stored objects behind the content: one for a whole/1
	// asset, a manifest plus a chunk per piece for a chunked one. Zero when
	// the manifest is not cached and the count cannot be known locally.
	Objects int `json:"objects,omitempty"`
	// Encoding is the pointer encoding: whole/1, or a chunked encoding.
	Encoding string `json:"encoding,omitempty"`
}

// ObjectStat reports whether recorded content can be read right now, without
// reading or fetching it. It exists so an interface can decide whether to
// offer a comparison before performing one, instead of learning by failure or
// by eagerly reading objects nobody asked for.
//
// It is not ObjectsStatus, which lists locally ingested objects and their
// per-remote publication state from the ledger. This answers "can I read this
// content", which is asked about revisions that may not be in the current
// index at all.
func (c *Client) ObjectStat(ctx context.Context, req ObjectStatRequest) (*ObjectInfo, error) {
	ptr, err := decodePointer(req.Pointer)
	if err != nil {
		return nil, err
	}
	r, err := c.openRepo(ctx, req.Path, "object stat")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.StatContent(ctx, ptr)
	if err != nil {
		return nil, translateObjectError(r.Root, err)
	}
	return &ObjectInfo{Digest: info.Digest, Size: info.Size, Cached: info.Cached,
		Objects: info.Objects, Encoding: info.Encoding}, nil
}

type ObjectFetchRequest = ObjectStatRequest

// ObjectFetch downloads the content behind a pointer into the local cache from
// the configured remotes, and writes nothing to the working tree. It is what
// turns "this comparison is unavailable" into something a user can act on.
//
// Progress arrives on the usual callback under the operation name
// "object fetch". Content already cached is not re-downloaded, so calling it
// when everything is present costs nothing.
func (c *Client) ObjectFetch(ctx context.Context, req ObjectFetchRequest) (*ObjectInfo, error) {
	ptr, err := decodePointer(req.Pointer)
	if err != nil {
		return nil, err
	}
	r, err := c.openRepo(ctx, req.Path, "object fetch")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	const op = "object fetch"
	c.progress(Event{Operation: op, Kind: EventOperationStarted})
	info, err := r.FetchContent(ctx, ptr, c.itemProgress(op))
	if err != nil {
		kind := EventItemFailed
		if errors.Is(err, context.Canceled) {
			kind = EventItemCancelled
		}
		c.progress(Event{Operation: op, Kind: kind, Phase: PhaseTransfer, Digest: ptr.OID, Message: err.Error()})
	}
	c.progress(Event{Operation: op, Kind: EventOperationFinished})
	if err != nil {
		return nil, translateObjectError(r.Root, err)
	}
	return &ObjectInfo{Digest: info.Digest, Size: info.Size, Cached: info.Cached,
		Objects: info.Objects, Encoding: info.Encoding}, nil
}

// decodePointer parses the pointer text a caller supplied. A damaged or
// future-version pointer is reported as what it is — the blob is wrong, not
// the call — so an interface can tell the difference between its own mistake
// and a repository that needs attention.
func decodePointer(text string) (pointer.Pointer, error) {
	if text == "" {
		return pointer.Pointer{}, NewError(CodeInvalidArgument, "no pointer given").
			WithHint("pass the pointer text Git holds for the path, e.g. the output of: git show HEAD:<path>")
	}
	b := []byte(text)
	if b[len(b)-1] != '\n' {
		// Command output is routinely captured without its trailing newline,
		// and refusing that would be a decoding rule masquerading as an API.
		b = append(b, '\n')
	}
	ptr, err := pointer.Decode(b)
	if err != nil {
		switch {
		case errors.Is(err, pointer.ErrUnsupportedVersion):
			return ptr, WrapError(CodeUnsupportedFormat, err.Error(), err).
				WithHint("fxvcs upgrade --check")
		case errors.Is(err, pointer.ErrMalformed):
			return ptr, WrapError(CodeIntegrity, err.Error(), err)
		}
		return ptr, WrapError(CodeIntegrity, err.Error(), err)
	}
	return ptr, nil
}

// translateObjectError keeps the not-cached / not-found distinction the object
// operations promise, then falls back to the shared translation.
func translateObjectError(root string, err error) error {
	if code, ok := checkoutCode(err); ok {
		e := WrapError(code, err.Error(), err)
		if code == CodeNotCached {
			e.Hint = "the content is not on this computer; fetch it with ObjectFetch (fxvcs load downloads into the working tree instead)"
		}
		return e
	}
	if errors.Is(err, app.ErrInvalidArgument) {
		return WrapError(CodeInvalidArgument, err.Error(), err)
	}
	return translateError(root, err)
}
