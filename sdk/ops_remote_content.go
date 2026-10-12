package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/repository"
)

// ---- reading content without a checkout -------------------------------------

// Everything else in this package operates on a working tree. A deploy tool
// staging a release from a Git archive has none: it holds the committed
// repository configuration and the pointer bytes, and wants the verified
// content behind each pointer written where it chooses. A checkout, the
// hydration state database and the host object cache would all be dead weight
// there — and the cache a second copy of every asset the tool already keeps.
//
// OpenRemoteContent serves that case. It reads straight from the repository's
// storage remotes and writes nothing anywhere.

// Pointer is a decoded pointer: the complete name of one asset's content.
type Pointer struct {
	StorageDomainID string `json:"storageDomainID"`
	Encoding        string `json:"encoding"`
	ManifestDigest  string `json:"manifestDigest"`
	// OID is the sha256 digest of the whole content, "sha256:<hex>".
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// PointerMaxSize is the largest a pointer file can be. A file larger than
// this is content, never a pointer, so a scan can skip it unread.
const PointerMaxSize = pointer.MaxSize

// IsPointer reports whether data looks like a pointer of any version. It does
// not validate: a file that passes and then fails ParsePointer is a damaged
// pointer, which callers should report rather than deliver as content.
func IsPointer(data []byte) bool {
	return len(data) <= pointer.MaxSize && pointer.Sniff(data)
}

// ParsePointer strictly decodes pointer bytes. A future pointer version is
// CodeUnsupportedFormat; anything else that fails is CodeInvalidArgument.
func ParsePointer(data []byte) (Pointer, error) {
	p, err := pointer.Decode(data)
	if err != nil {
		if errors.Is(err, pointer.ErrUnsupportedVersion) {
			return Pointer{}, WrapError(CodeUnsupportedFormat, err.Error(), err).
				WithHint("the pointer was written by a newer fxvcs; upgrade the reader")
		}
		return Pointer{}, WrapError(CodeInvalidArgument, err.Error(), err)
	}
	return publicPointer(p), nil
}

func publicPointer(p pointer.Pointer) Pointer {
	return Pointer{StorageDomainID: p.StorageDomainID, Encoding: p.Encoding,
		ManifestDigest: p.ManifestDigest, OID: p.OID, Size: p.Size}
}

// StorageCredentials is an S3 identity. Secrets never serialize.
type StorageCredentials struct {
	AccessKeyID     string `json:"accessKeyID,omitempty"`
	SecretAccessKey string `json:"-"`
	SessionToken    string `json:"-"`
}

type RemoteContentRequest struct {
	// Repository is the committed repository configuration: the bytes of
	// .fxvcs/repository.yaml at the revision whose pointers will be read.
	Repository []byte `json:"-"`
	// Credentials by remote name. A remote without an entry resolves its
	// identity as everywhere else: FXVCS_S3_<REMOTE>_* then FXVCS_S3_*
	// environment variables, the credential store, the AWS default chain.
	Credentials map[string]StorageCredentials `json:"-"`
	// Paths binds filesystem remotes to a directory on this machine.
	// Filesystem remotes without one are skipped.
	Paths map[string]string `json:"paths,omitempty"`
}

// RemoteContent reads verified content from a repository's storage remotes.
// It is safe for concurrent use and holds no open resources.
type RemoteContent struct {
	src *app.RemoteSource
	// RepositoryID and StorageDomainID identify the repository the
	// configuration describes.
	RepositoryID    string `json:"repositoryID"`
	StorageDomainID string `json:"storageDomainID"`
	// Remotes lists the remotes reads are tried against, in order:
	// publication remotes first.
	Remotes []string `json:"remotes"`
}

// OpenRemoteContent prepares to read content for one repository without a
// checkout or cache. It contacts nothing: an unreachable remote or a refused
// credential surfaces on the first Read.
func (c *Client) OpenRemoteContent(ctx context.Context, req RemoteContentRequest) (*RemoteContent, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	if len(req.Repository) == 0 {
		return nil, NewError(CodeInvalidArgument, "no repository configuration given").
			WithHint("pass the bytes of " + repository.ConfigPath + " at the revision being read")
	}
	cfg, err := repository.LoadConfigBytes(req.Repository)
	if err != nil {
		return nil, translateError("", err)
	}
	if err := repository.Preflight(repository.ConfigPath, cfg, c.installed); err != nil {
		return nil, translateError("", err)
	}
	creds := make(map[string]s3.Credentials, len(req.Credentials))
	for name, cr := range req.Credentials {
		creds[name] = s3.Credentials{AccessKeyID: cr.AccessKeyID, SecretAccessKey: cr.SecretAccessKey, SessionToken: cr.SessionToken}
	}
	src, err := app.OpenRemoteSource(app.RemoteSourceInput{Config: cfg, Credentials: creds,
		Paths: req.Paths, ConfigDir: c.opts.ConfigDir})
	if err != nil {
		return nil, translateObjectError("", err)
	}
	return &RemoteContent{src: src, RepositoryID: cfg.Metadata.RepositoryID,
		StorageDomainID: cfg.Metadata.StorageDomainID, Remotes: src.Remotes()}, nil
}

// Read streams the whole content behind a pointer, reassembling chunks and
// verifying as it goes. The reader reports io.EOF only after the content has
// matched the pointer's oid and size; any other error means the bytes already
// read must be discarded. A pointer from another storage domain is
// CodeIntegrity, an object on none of the remotes CodeNotFound.
func (rc *RemoteContent) Read(ctx context.Context, p Pointer) (*ObjectReadResult, error) {
	ptr := pointer.Pointer{StorageDomainID: p.StorageDomainID, Encoding: p.Encoding,
		ManifestDigest: p.ManifestDigest, OID: p.OID, Size: p.Size}
	if err := ptr.Validate(); err != nil {
		return nil, WrapError(CodeInvalidArgument, fmt.Sprintf("invalid pointer: %v", err), err)
	}
	body, err := rc.src.Open(ctx, ptr)
	if err != nil {
		return nil, translateObjectError("", err)
	}
	return &ObjectReadResult{Digest: ptr.OID, Size: ptr.Size, Length: ptr.Size, Content: codedReader{body}}, nil
}

// codedReader gives failures discovered while streaming — a chunk that does
// not verify, a remote that drops mid-object — the same codes as failures
// discovered before the first byte.
type codedReader struct{ io.ReadCloser }

func (r codedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = translateObjectError("", err)
	}
	return n, err
}
