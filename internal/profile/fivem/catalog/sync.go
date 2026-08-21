// Package catalog maintains the FiveM component resource catalog
// (.fxvcs/resources.yaml): synchronizing it with discovered manifests while
// preserving operator-owned fields, validating it for release, and diffing a
// pinned catalog against a rescan so release creation fails closed.
//
// Nothing here performs I/O; callers load and store the catalog document.
package catalog

import (
	"errors"
	"fmt"
	"maps"
	"sort"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// Status values for domain.CatalogResource.Status.
const (
	StatusPresent = "present"
	StatusMissing = "missing"
)

// MutableMode values.
const (
	MutableImmutable = "immutable"
	MutableCopy      = "copy"
)

// Report summarizes what Sync changed. Every list is sorted by path.
type Report struct {
	Added     []string `json:"added"`     // new entries (activation = newActivation)
	Refreshed []string `json:"refreshed"` // existing entries whose manifest-derived fields changed
	Unchanged []string `json:"unchanged"` // existing entries with no change
	Missing   []string `json:"missing"`   // entries without a directory (status = missing)
	Revived   []string `json:"revived"`   // previously missing entries that reappeared
}

// Changed reports whether Sync produced a catalog different from its input.
func (r Report) Changed() bool {
	return len(r.Added)+len(r.Refreshed)+len(r.Revived) > 0 || len(r.Missing) > 0
}

// ErrSync classifies Sync input failures.
var ErrSync = errors.New("catalog: sync")

// Sync merges discovered resources into existing (which may be nil) and
// returns a new catalog. It never mutates its inputs.
//
// Semantics:
//   - identity is Path (within one repository);
//   - matching entries keep operator-owned activation, after, before,
//     persistentPaths, and mutableMode; provides, dependencies, games,
//     serverOnly, and name are refreshed from the manifest;
//   - new entries get newActivation (normally pending) and empty operator
//     fields with mutableMode immutable;
//   - entries without a discovered directory become status missing and are
//     never deleted (a review tombstone);
//   - the result is sorted by path and Sync is idempotent.
func Sync(existing *domain.ResourceCatalog, discovered []discovery.DiscoveredResource, newActivation domain.Activation) (*domain.ResourceCatalog, Report, error) {
	rep := Report{Added: []string{}, Refreshed: []string{}, Unchanged: []string{}, Missing: []string{}, Revived: []string{}}
	switch newActivation {
	case domain.ActivationEnabled, domain.ActivationDisabled, domain.ActivationPending:
	default:
		return nil, rep, fmt.Errorf("%w: invalid activation %q for new entries", ErrSync, newActivation)
	}

	out := &domain.ResourceCatalog{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindResourceCatalog},
		Spec:     domain.ResourceCatalogSpec{Resources: []domain.CatalogResource{}},
	}
	prev := map[string]domain.CatalogResource{}
	if existing != nil {
		out.Metadata = existing.Metadata
		if existing.APIVersion != "" {
			out.Envelope = existing.Envelope
		}
		for _, r := range existing.Spec.Resources {
			if _, dup := prev[r.Path]; dup {
				return nil, rep, fmt.Errorf("%w: existing catalog has duplicate path %q", ErrSync, r.Path)
			}
			prev[r.Path] = r
		}
	}

	seen := map[string]bool{}
	for _, d := range discovered {
		if seen[d.Path] {
			return nil, rep, fmt.Errorf("%w: discovered resources contain duplicate path %q", ErrSync, d.Path)
		}
		seen[d.Path] = true
		if d.Manifest == nil {
			return nil, rep, fmt.Errorf("%w: discovered resource %q has no manifest", ErrSync, d.Path)
		}
		fresh := domain.CatalogResource{
			Path:                d.Path,
			Name:                d.Name,
			Activation:          newActivation,
			After:               []string{},
			Before:              []string{},
			Provides:            cloneOrEmpty(d.Manifest.Provides),
			Dependencies:        cloneOrEmpty(d.Manifest.Dependencies),
			RuntimeRequirements: cloneNilIfEmpty(d.Manifest.RuntimeRequirements),
			Games:               cloneOrEmpty(d.Manifest.Games),
			ServerOnly:          d.Manifest.ServerOnly,
			PersistentPaths:     []string{},
			MutableMode:         MutableImmutable,
			Status:              StatusPresent,
		}
		old, had := prev[d.Path]
		if !had {
			rep.Added = append(rep.Added, d.Path)
			out.Spec.Resources = append(out.Spec.Resources, fresh)
			continue
		}
		merged := fresh
		merged.Activation = old.Activation
		merged.After = cloneOrEmpty(old.After)
		merged.Before = cloneOrEmpty(old.Before)
		merged.PersistentPaths = cloneOrEmpty(old.PersistentPaths)
		merged.PersistentPathKinds = maps.Clone(old.PersistentPathKinds)
		if old.MutableMode != "" {
			merged.MutableMode = old.MutableMode
		}
		if merged.Activation == "" {
			merged.Activation = newActivation
		}
		switch {
		case old.Status == StatusMissing:
			rep.Revived = append(rep.Revived, d.Path)
		case !equalResource(normalizeResource(old), merged):
			rep.Refreshed = append(rep.Refreshed, d.Path)
		default:
			rep.Unchanged = append(rep.Unchanged, d.Path)
		}
		out.Spec.Resources = append(out.Spec.Resources, merged)
	}
	for p, old := range prev {
		if seen[p] {
			continue
		}
		tomb := normalizeResource(old)
		tomb.Status = StatusMissing
		rep.Missing = append(rep.Missing, p)
		out.Spec.Resources = append(out.Spec.Resources, tomb)
	}
	sort.Slice(out.Spec.Resources, func(i, j int) bool { return out.Spec.Resources[i].Path < out.Spec.Resources[j].Path })
	for _, l := range []*[]string{&rep.Added, &rep.Refreshed, &rep.Unchanged, &rep.Missing, &rep.Revived} {
		sort.Strings(*l)
	}
	return out, rep, nil
}

func cloneOrEmpty(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	return append([]string{}, in...)
}

// normalizeResource fills nil lists and defaults so comparisons are stable.
func normalizeResource(r domain.CatalogResource) domain.CatalogResource {
	r.After = cloneOrEmpty(r.After)
	r.Before = cloneOrEmpty(r.Before)
	r.Provides = cloneOrEmpty(r.Provides)
	r.Dependencies = cloneOrEmpty(r.Dependencies)
	r.RuntimeRequirements = cloneNilIfEmpty(r.RuntimeRequirements)
	r.Games = cloneOrEmpty(r.Games)
	r.PersistentPaths = cloneOrEmpty(r.PersistentPaths)
	if len(r.PersistentPathKinds) == 0 {
		r.PersistentPathKinds = nil
	} else {
		r.PersistentPathKinds = maps.Clone(r.PersistentPathKinds)
	}
	if r.MutableMode == "" {
		r.MutableMode = MutableImmutable
	}
	if r.Status == "" {
		r.Status = StatusPresent
	}
	return r
}

func equalResource(a, b domain.CatalogResource) bool {
	return a.Path == b.Path && a.Name == b.Name && a.Activation == b.Activation &&
		eqList(a.After, b.After) && eqList(a.Before, b.Before) &&
		eqList(a.Provides, b.Provides) && eqList(a.Dependencies, b.Dependencies) &&
		eqList(a.RuntimeRequirements, b.RuntimeRequirements) &&
		eqList(a.Games, b.Games) && a.ServerOnly == b.ServerOnly &&
		eqList(a.PersistentPaths, b.PersistentPaths) && maps.Equal(a.PersistentPathKinds, b.PersistentPathKinds) && a.MutableMode == b.MutableMode &&
		a.Status == b.Status
}

func eqList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// cloneNilIfEmpty copies a list but keeps empty as nil so optional YAML fields
// are omitted rather than serialized as [].
func cloneNilIfEmpty(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
