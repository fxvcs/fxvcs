package catalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// StaleEntry is a catalog entry whose manifest-derived field disagrees with
// the rescanned manifest.
type StaleEntry struct {
	Path     string `json:"path"`
	Field    string `json:"field"`
	Catalog  string `json:"catalog"`
	Manifest string `json:"manifest"`
}

// DiffReport compares a pinned catalog against a rescan of the same tree.
type DiffReport struct {
	// Undeclared are discovered resources absent from the catalog (would be
	// pending after a sync).
	Undeclared []string `json:"undeclared"`
	// Missing are catalog entries whose directory no longer exists.
	Missing []string `json:"missing"`
	// Stale are entries whose provides/dependencies/games/serverOnly/name no
	// longer match the manifest.
	Stale []StaleEntry `json:"stale"`
}

// Clean reports whether the catalog matches the rescan exactly.
func (d DiffReport) Clean() bool {
	return len(d.Undeclared) == 0 && len(d.Missing) == 0 && len(d.Stale) == 0
}

// ErrCatalogDrift is returned by DiffReport.Err when the report is not clean.
var ErrCatalogDrift = errors.New("catalog: pinned catalog disagrees with rescan")

// Err returns nil for a clean report, otherwise ErrCatalogDrift with details.
func (d DiffReport) Err() error {
	if d.Clean() {
		return nil
	}
	var parts []string
	for _, p := range d.Undeclared {
		parts = append(parts, "undeclared "+p)
	}
	for _, p := range d.Missing {
		parts = append(parts, "missing "+p)
	}
	for _, s := range d.Stale {
		parts = append(parts, fmt.Sprintf("stale %s %s (catalog %s, manifest %s)", s.Path, s.Field, s.Catalog, s.Manifest))
	}
	return fmt.Errorf("%w: %s", ErrCatalogDrift, strings.Join(parts, "; "))
}

// Diff compares pinned against rescanned. Entries already tombstoned as
// missing in the pinned catalog are reported as missing too: nothing missing
// may enter a Release.
func Diff(pinned *domain.ResourceCatalog, rescanned []discovery.DiscoveredResource) DiffReport {
	rep := DiffReport{Undeclared: []string{}, Missing: []string{}, Stale: []StaleEntry{}}
	byPath := map[string]discovery.DiscoveredResource{}
	for _, d := range rescanned {
		byPath[d.Path] = d
	}
	declared := map[string]bool{}
	if pinned != nil {
		for _, r := range pinned.Spec.Resources {
			declared[r.Path] = true
			d, ok := byPath[r.Path]
			if !ok {
				rep.Missing = append(rep.Missing, r.Path)
				continue
			}
			m := d.Manifest
			check := func(field, a, b string) {
				if a != b {
					rep.Stale = append(rep.Stale, StaleEntry{Path: r.Path, Field: field, Catalog: a, Manifest: b})
				}
			}
			check("name", r.Name, d.Name)
			check("provides", joinList(r.Provides), joinList(m.Provides))
			check("dependencies", joinList(r.Dependencies), joinList(m.Dependencies))
			check("games", joinList(r.Games), joinList(m.Games))
			check("serverOnly", fmt.Sprint(r.ServerOnly), fmt.Sprint(m.ServerOnly))
			if r.Status == StatusMissing {
				rep.Stale = append(rep.Stale, StaleEntry{Path: r.Path, Field: "status", Catalog: r.Status, Manifest: StatusPresent})
			}
		}
	}
	for p := range byPath {
		if !declared[p] {
			rep.Undeclared = append(rep.Undeclared, p)
		}
	}
	sort.Strings(rep.Undeclared)
	sort.Strings(rep.Missing)
	sort.SliceStable(rep.Stale, func(i, j int) bool {
		if rep.Stale[i].Path != rep.Stale[j].Path {
			return rep.Stale[i].Path < rep.Stale[j].Path
		}
		return rep.Stale[i].Field < rep.Stale[j].Field
	})
	return rep
}

func joinList(l []string) string {
	c := append([]string{}, l...)
	sort.Strings(c)
	return "[" + strings.Join(c, ",") + "]"
}
