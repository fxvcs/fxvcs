// Package integrity computes FXVCS content identities from domain documents:
// object-manifest digests, asset roots, the semantic release digest, and
// sourceSetDigest. Every function is pure and deterministic; each has golden
// fixtures under spec/fixtures/golden.
package integrity

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ErrInvalid marks structurally invalid identity input.
var ErrInvalid = errors.New("integrity: invalid input")

// IsDigest reports whether s is a "sha256:<64 hex>" string.
func IsDigest(s string) bool { return digestRE.MatchString(s) }

// ManifestDigest canonicalizes and hashes an object manifest.
func ManifestDigest(m domain.ObjectManifest) (string, error) {
	if m.FormatVersion != format.ObjectManifestVersion {
		return "", fmt.Errorf("%w: manifest formatVersion %d", ErrInvalid, m.FormatVersion)
	}
	if !IsDigest(m.OID) {
		return "", fmt.Errorf("%w: manifest oid", ErrInvalid)
	}
	if len(m.Chunks) == 0 {
		return "", fmt.Errorf("%w: manifest has no chunks", ErrInvalid)
	}
	var total int64
	for i, c := range m.Chunks {
		if !IsDigest(c.Digest) || c.RawSize < 0 || c.EncodedSize < 0 {
			return "", fmt.Errorf("%w: chunk %d", ErrInvalid, i)
		}
		total += c.RawSize
	}
	if total != m.Size {
		return "", fmt.Errorf("%w: chunk raw sizes sum to %d, manifest size %d", ErrInvalid, total, m.Size)
	}
	return canonical.Digest(m)
}

// AssetRoot validates ordering/uniqueness and hashes an asset index.
func AssetRoot(idx domain.AssetIndex) (string, error) {
	if idx.FormatVersion != format.AssetIndexVersion {
		return "", fmt.Errorf("%w: assetIndex formatVersion %d", ErrInvalid, idx.FormatVersion)
	}
	if idx.Entries == nil {
		// An empty tree has exactly one identity: "entries":[] not null.
		idx.Entries = []domain.AssetIndexEntry{}
	}
	for i, e := range idx.Entries {
		if _, err := pathsafe.Normalize(e.Path); err != nil {
			return "", fmt.Errorf("%w: entry %d: %v", ErrInvalid, i, err)
		}
		if !IsDigest(e.PointerDigest) {
			return "", fmt.Errorf("%w: entry %d pointerDigest", ErrInvalid, i)
		}
		if e.Mode != "100644" && e.Mode != "100755" {
			return "", fmt.Errorf("%w: entry %d mode %q", ErrInvalid, i, e.Mode)
		}
		if i > 0 && idx.Entries[i-1].Path >= e.Path {
			return "", fmt.Errorf("%w: entries not strictly sorted at %d (%q >= %q)", ErrInvalid, i, idx.Entries[i-1].Path, e.Path)
		}
	}
	return canonical.Digest(idx)
}

// SortAssetIndex sorts entries by path in byte order (the required order).
func SortAssetIndex(idx *domain.AssetIndex) {
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Path < idx.Entries[j].Path })
}

// SourceSetDigest computes the mandatory release source identity. It covers
// component IDs, commits, asset roots, storage domains, descriptors and mounts,
// plus non-component resource IDs with their portable verifier/lifecycle
// requirements. It excludes display names, aliases, activation, ordering,
// activationSource, environment state, target bindings, and runtime evidence.
func SourceSetDigest(rel domain.ReleaseSpec) (string, error) {
	comps := make([]map[string]any, 0, len(rel.Components))
	for _, c := range rel.Components {
		if !IsDigest(c.AssetRoot) {
			return "", fmt.Errorf("%w: component %s assetRoot", ErrInvalid, c.RepositoryID)
		}
		d := map[string]any{
			"sourceRoot": c.Descriptor.SourceRoot,
			"layout":     c.Descriptor.Layout,
		}
		if c.Descriptor.ResourceName != nil {
			d["resourceName"] = *c.Descriptor.ResourceName
		}
		comps = append(comps, map[string]any{
			"repositoryID":    c.RepositoryID,
			"commit":          c.Commit,
			"assetRoot":       c.AssetRoot,
			"storageDomainID": c.StorageDomainID,
			"descriptor":      d,
			"mount":           c.Mount,
		})
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i]["repositoryID"].(string) < comps[j]["repositoryID"].(string) })

	nonComp := make([]map[string]any, 0, len(rel.RuntimeResources))
	for _, r := range rel.RuntimeResources {
		e := map[string]any{"id": r.ID, "source": r.Source}
		if r.Verifier != nil {
			e["verifier"] = map[string]any{"contract": r.Verifier.Contract, "version": r.Verifier.Version}
		}
		if r.Lifecycle != nil {
			e["lifecycle"] = map[string]any{"ownership": r.Lifecycle.Ownership, "actions": r.Lifecycle.Actions}
		}
		nonComp = append(nonComp, e)
	}
	sort.Slice(nonComp, func(i, j int) bool { return nonComp[i]["id"].(string) < nonComp[j]["id"].(string) })

	return canonical.Digest(map[string]any{
		"sourceSetVersion":      1,
		"components":            comps,
		"nonComponentResources": nonComp,
	})
}

// ReleaseDigest is the immutable content identity of a Release: the canonical
// semantic encoding of the whole spec (not the YAML bytes). Signatures are
// excluded because they sign this digest.
func ReleaseDigest(rel domain.Release) (string, error) {
	if err := format.ValidateEnvelope(rel.Envelope, format.KindRelease); err != nil {
		return "", err
	}
	spec := rel.Spec
	spec.Signature = nil
	return canonical.Digest(map[string]any{
		"apiVersion": rel.APIVersion,
		"kind":       rel.Kind,
		"metadata":   rel.Metadata,
		"spec":       spec,
	})
}
