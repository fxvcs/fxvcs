// Package objectstore defines the immutable, content-addressed object store
// contract (build plan §6). Objects are keyed by storage domain and sha256
// digest; content is never overwritten or mutated. Backends: filesystem (this
// stage), S3-compatible (later).
package objectstore

import (
	"context"
	"errors"
	"io"
)

// Errors.
var (
	ErrNotFound      = errors.New("objectstore: object not found")
	ErrInvalidDigest = errors.New("objectstore: invalid digest")
	ErrInvalidDomain = errors.New("objectstore: invalid storage domain")
	ErrUnavailable   = errors.New("objectstore: remote unavailable")
)

// Store is the backend-neutral contract. Implementations must be safe for
// concurrent use across processes; Put must be atomic (no partially written
// object is ever visible) and idempotent for identical content.
type Store interface {
	Has(ctx context.Context, storageDomainID, digest string) (bool, error)
	// Put stores exactly the bytes read from r. size is the expected length
	// (-1 if unknown); the implementation must fail if the stream length or
	// digest disagrees with the declared identity.
	Put(ctx context.Context, storageDomainID, digest string, size int64, r io.Reader) error
	// Get returns a reader over the object bytes. Callers verify the digest
	// while reading (see internal/cache.Reader) — the store itself makes no
	// integrity promise beyond returning what was stored.
	Get(ctx context.Context, storageDomainID, digest string) (io.ReadCloser, error)
}

// BatchHaser is optionally implemented by backends that can answer many
// existence questions in fewer round trips than one Has each. Publication
// planning for a chunked asset asks about hundreds of digests at once, and on
// a network backend the per-object round trips dominate the wall clock.
//
// The returned map has an entry for every requested digest.
type BatchHaser interface {
	BatchHas(ctx context.Context, storageDomainID string, digests []string) (map[string]bool, error)
}

// HasMany answers existence for many digests, using the backend's bulk query
// when it has one and falling back to individual lookups when it does not.
// The returned map has an entry for every requested digest.
func HasMany(ctx context.Context, s Store, storageDomainID string, digests []string) (map[string]bool, error) {
	if b, ok := s.(BatchHaser); ok {
		return b.BatchHas(ctx, storageDomainID, digests)
	}
	out := make(map[string]bool, len(digests))
	for _, d := range digests {
		if _, done := out[d]; done {
			continue
		}
		has, err := s.Has(ctx, storageDomainID, d)
		if err != nil {
			return nil, err
		}
		out[d] = has
	}
	return out, nil
}

// Capability is one storage test result.
type Capability struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Detail    string `json:"detail,omitempty"`
}

// Tester is optionally implemented by backends for `storage test`.
type Tester interface {
	Test(ctx context.Context, storageDomainID string) ([]Capability, error)
}
