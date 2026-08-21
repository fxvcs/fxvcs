// Package release builds immutable Release records from a pinned composition,
// compares two releases, and moves one release between coordination
// repositories as a portable document.
//
// The invariant the whole package exists to protect: a Release must be
// reproducible. Given the same component commits it must produce byte-identical
// content, and it must be impossible for a later change in a component
// repository or a coordination override to alter a Release that already exists.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// ErrAssetIndex marks a tree whose FXVCS content cannot be described.
var ErrAssetIndex = errors.New("release: cannot build the asset index")

// AssetIndex derives the canonical asset index of a pinned component tree.
//
// An entry is any regular blob whose content is an FXVCS pointer. Deriving
// membership from the bytes rather than from `.gitattributes` is deliberate:
// the attributes file says what *should* be filtered from now on, while the
// asset root has to describe what the pinned commit actually contains. A path
// that someone committed as a real file before it was tracked is honestly not
// an asset, and a path whose tracking rule was removed after the fact still is
// one while its blob is a pointer.
//
// Blobs larger than a pointer can be are never read.
func AssetIndex(ctx context.Context, tree *gitops.Tree, storageDomainID string) (domain.AssetIndex, []domain.AssetIndexEntry, error) {
	entries, err := tree.Entries(ctx)
	if err != nil {
		return domain.AssetIndex{}, nil, err
	}
	idx := domain.AssetIndex{
		FormatVersion:   format.AssetIndexVersion,
		StorageDomainID: storageDomainID,
		Entries:         []domain.AssetIndexEntry{},
	}
	for _, e := range entries {
		if !e.IsRegular() || e.Size < 0 || e.Size > pointer.MaxSize {
			continue
		}
		data, err := tree.ReadBlob(ctx, e.OID)
		if err != nil {
			return domain.AssetIndex{}, nil, err
		}
		if !pointer.Sniff(data) {
			continue
		}
		p, err := pointer.Decode(data)
		if err != nil {
			return domain.AssetIndex{}, nil, fmt.Errorf("%w: %s holds a damaged placeholder: %w", ErrAssetIndex, e.Path, err)
		}
		if p.StorageDomainID != storageDomainID {
			return domain.AssetIndex{}, nil, fmt.Errorf("%w: %s belongs to storage domain %s but the repository declares %s; assets cannot be shared across access boundaries",
				ErrAssetIndex, e.Path, p.StorageDomainID, storageDomainID)
		}
		sum := sha256.Sum256(data)
		idx.Entries = append(idx.Entries, domain.AssetIndexEntry{
			Path:          e.Path,
			Mode:          e.Mode,
			PointerDigest: "sha256:" + hex.EncodeToString(sum[:]),
		})
	}
	integrity.SortAssetIndex(&idx)
	return idx, idx.Entries, nil
}

// AssetRoot computes the asset-root digest of a pinned component tree.
func AssetRoot(ctx context.Context, tree *gitops.Tree, storageDomainID string) (string, domain.AssetIndex, error) {
	idx, _, err := AssetIndex(ctx, tree, storageDomainID)
	if err != nil {
		return "", domain.AssetIndex{}, err
	}
	root, err := integrity.AssetRoot(idx)
	if err != nil {
		return "", domain.AssetIndex{}, err
	}
	return root, idx, nil
}

// Pointers reads the decoded pointers of a pinned tree, keyed by path. Release
// creation uses them to check that every referenced object is already durable
// in storage.
func Pointers(ctx context.Context, tree *gitops.Tree, storageDomainID string) (map[string]pointer.Pointer, error) {
	entries, err := tree.Entries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]pointer.Pointer{}
	for _, e := range entries {
		if !e.IsRegular() || e.Size < 0 || e.Size > pointer.MaxSize {
			continue
		}
		data, err := tree.ReadBlob(ctx, e.OID)
		if err != nil {
			return nil, err
		}
		if !pointer.Sniff(data) {
			continue
		}
		p, err := pointer.Decode(data)
		if err != nil {
			return nil, fmt.Errorf("%w: %s holds a damaged placeholder: %w", ErrAssetIndex, e.Path, err)
		}
		if p.StorageDomainID != storageDomainID {
			return nil, fmt.Errorf("%w: %s belongs to storage domain %s but the repository declares %s",
				ErrAssetIndex, e.Path, p.StorageDomainID, storageDomainID)
		}
		out[e.Path] = p
	}
	return out, nil
}
