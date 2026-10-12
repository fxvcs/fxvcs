package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// RemoteSource reads recorded content straight from a repository's storage
// remotes. It needs no checkout and keeps no cache: it is the read path for a
// machine that has only the committed repository configuration and pointer
// bytes — a deploy agent staging a release from a Git archive, say — and that
// manages whatever it writes itself.
//
// Content is verified exactly as a checkout verifies it: a manifest is accepted
// only when it canonicalizes to the digest the pointer names and describes that
// pointer, every chunk is checked against its manifest entry, and the whole
// stream against the pointer's oid and size before EOF is reported.
type RemoteSource struct {
	repo   *domain.Repository
	stores []namedStore
}

type namedStore struct {
	name  string
	store objectstore.Store
}

// RemoteSourceInput describes how this machine reaches a repository's
// storage. Only Config is required.
type RemoteSourceInput struct {
	// Config is the committed repository configuration.
	Config *domain.Repository
	// Credentials by remote name. A remote without an entry resolves its
	// identity the usual way: FXVCS_S3_* environment variables, the
	// credential store under ConfigDir, then the AWS default chain.
	Credentials map[string]s3.Credentials
	// Paths binds filesystem remotes to a directory on this machine. A
	// filesystem remote without a binding is skipped.
	Paths map[string]string
	// ConfigDir locates the credential store; empty means the default.
	ConfigDir string
}

// OpenRemoteSource prepares stores for the configured remotes, publication
// remotes first (they are the ones guaranteed to hold every published object),
// then any others, each in committed order.
func OpenRemoteSource(in RemoteSourceInput) (*RemoteSource, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("%w: no repository configuration", ErrInvalidArgument)
	}
	domainID := in.Config.Metadata.StorageDomainID
	var required, others []namedStore
	var skipped []string
	for _, remote := range in.Config.Spec.Storage.Remotes {
		var store objectstore.Store
		var err error
		switch remote.Type {
		case "s3":
			cfg := s3.Config{Endpoint: remote.Endpoint, Bucket: remote.Bucket, Prefix: remote.Prefix,
				Region: remote.Region, Addressing: remote.Addressing}
			if strings.TrimSpace(cfg.Bucket) == "" {
				return nil, fmt.Errorf("%w: s3 storage %q has no bucket", ErrInvalidArgument, remote.Name)
			}
			creds, ok := in.Credentials[remote.Name]
			if !ok {
				if creds, err = coordCredentials(domainID, remote.Name, nil, in.ConfigDir); err != nil {
					return nil, err
				}
			}
			cfg.Credentials = creds
			store, err = s3.New(cfg)
		case "filesystem":
			path := in.Paths[remote.Name]
			if path == "" {
				skipped = append(skipped, remote.Name)
				continue
			}
			store, err = filesystem.New(path)
		default:
			skipped = append(skipped, remote.Name)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("remote %s: %w", remote.Name, err)
		}
		ns := namedStore{name: remote.Name, store: store}
		if remote.RequiredForPublication {
			required = append(required, ns)
		} else {
			others = append(others, ns)
		}
	}
	stores := make([]namedStore, 0, len(required)+len(others))
	stores = append(stores, required...)
	stores = append(stores, others...)
	if len(stores) == 0 {
		if len(skipped) > 0 {
			return nil, fmt.Errorf("%w: none of the storage remotes (%s) is reachable from here; bind a filesystem remote's directory or add an s3 remote",
				ErrRemoteUnbound, strings.Join(skipped, ", "))
		}
		return nil, fmt.Errorf("%w: the repository configures no storage remotes", ErrInvalidArgument)
	}
	return &RemoteSource{repo: in.Config, stores: stores}, nil
}

// Repository is the configuration the source was opened with.
func (s *RemoteSource) Repository() *domain.Repository { return s.repo }

// Remotes names the remotes the source reads from, in the order it tries them.
func (s *RemoteSource) Remotes() []string {
	out := make([]string, len(s.stores))
	for i, ns := range s.stores {
		out[i] = ns.name
	}
	return out
}

// Open streams the whole content behind a pointer. The reader fails rather
// than return bytes that do not verify; a caller that writes them somewhere
// must treat any read error as "discard what was written".
func (s *RemoteSource) Open(ctx context.Context, ptr pointer.Pointer) (io.ReadCloser, error) {
	domainID := s.repo.Metadata.StorageDomainID
	if ptr.StorageDomainID != domainID {
		return nil, fmt.Errorf("%w: pointer belongs to storage domain %s, this repository stores %s",
			ErrIntegrity, ptr.StorageDomainID, domainID)
	}
	source := fallbackStore(s.stores)
	var body io.ReadCloser
	if ptr.Encoding == pointer.EncodingWhole {
		rc, err := source.Get(ctx, domainID, ptr.OID)
		if err != nil {
			return nil, err
		}
		body = rc
	} else {
		manifest, err := readManifestFrom(ctx, source, domainID, ptr)
		if err != nil {
			return nil, err
		}
		body = &verifiedChunkReader{ctx: ctx, source: source, domainID: domainID, chunks: manifest.Chunks}
	}
	return &oidReader{body: body, h: sha256.New(), oid: ptr.OID, size: ptr.Size}, nil
}

// oidReader checks a reconstruction against the pointer: it never yields more
// than size bytes, and reports EOF only once the bytes hash to oid.
type oidReader struct {
	body   io.ReadCloser
	h      hash.Hash
	oid    string
	size   int64
	read   int64
	failed error
}

func (r *oidReader) Read(p []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	n, err := r.body.Read(p)
	r.read += int64(n)
	if r.read > r.size {
		return 0, r.fail(fmt.Errorf("%w: content %s is longer than its recorded %d bytes", ErrIntegrity, r.oid, r.size))
	}
	_, _ = r.h.Write(p[:n])
	if errors.Is(err, io.EOF) {
		got := "sha256:" + hex.EncodeToString(r.h.Sum(nil))
		if r.read != r.size || got != r.oid {
			return 0, r.fail(fmt.Errorf("%w: content read %d bytes hashing to %s, pointer says %d bytes %s",
				ErrIntegrity, r.read, got, r.size, r.oid))
		}
		return n, io.EOF
	}
	if err != nil {
		return n, r.fail(err)
	}
	return n, nil
}

func (r *oidReader) fail(err error) error {
	r.failed = err
	return err
}

func (r *oidReader) Close() error {
	err := r.body.Close()
	if r.failed != nil && errors.Is(err, r.failed) {
		return nil
	}
	return err
}

// fallbackStore reads each object from the first remote that has it. Only a
// missing or unreachable object moves on to the next remote; anything else —
// a corrupt object, a refused credential — is the answer.
type fallbackStore []namedStore

func (f fallbackStore) Get(ctx context.Context, domainID, digest string) (io.ReadCloser, error) {
	var failures []error
	notFound := true
	for _, ns := range f {
		rc, err := ns.store.Get(ctx, domainID, digest)
		if err == nil {
			return rc, nil
		}
		if !errors.Is(err, objectstore.ErrNotFound) && !errors.Is(err, objectstore.ErrUnavailable) {
			return nil, fmt.Errorf("remote %s: %w", ns.name, err)
		}
		if !errors.Is(err, objectstore.ErrNotFound) {
			notFound = false
		}
		failures = append(failures, fmt.Errorf("remote %s: %w", ns.name, err))
	}
	err := errors.Join(failures...)
	if notFound {
		return nil, fmt.Errorf("%w: object %s is on none of the remotes: %w", ErrObjectUnavailable, digest, err)
	}
	return nil, err
}

func (f fallbackStore) Has(ctx context.Context, domainID, digest string) (bool, error) {
	for _, ns := range f {
		if ok, err := ns.store.Has(ctx, domainID, digest); err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}

func (f fallbackStore) Put(context.Context, string, string, int64, io.Reader) error {
	return fmt.Errorf("%w: a remote source is read-only", ErrInvalidArgument)
}
