package fivem

import (
	"path"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// ChangeKind is the conservative v1 classification of one changed path.
type ChangeKind string

const (
	// ChangeHotReloadSafe: the change is confined to one resource that no
	// other catalog resource depends on; a resource restart is sufficient.
	ChangeHotReloadSafe ChangeKind = "hot-reload-safe"
	// ChangeRestartRequired: the resource's manifest changed, or other
	// resources depend on it, so dependents must be restarted too; v1 treats
	// this as a full-restart request.
	ChangeRestartRequired ChangeKind = "restart-required"
	// ChangeGlobal: the path is outside every catalog resource (server.cfg,
	// a category rename, files at the source root); a full restart is needed
	// and the catalog must be re-synced.
	ChangeGlobal ChangeKind = "global"
)

// ResourceChange is the classification of one changed path.
type ResourceChange struct {
	Path     string     `json:"path"`
	Resource string     `json:"resource,omitempty"` // catalog path of the owning resource
	Kind     ChangeKind `json:"kind"`
	Reason   string     `json:"reason"`
}

// Classify maps changed paths (relative to the component source root, "/"
// separated) onto catalog resources using the v1 rules:
//
//   - a manifest change (fxmanifest.lua / __resource.lua) is restart-required;
//   - a change inside a resource that another catalog resource depends on
//     (by name or by a provided name) is restart-required;
//   - any other change inside a resource is hot-reload-safe;
//   - a change outside every resource is global.
//
// For a single-resource descriptor every path belongs to that resource. The
// result is sorted by path with duplicates removed.
func Classify(changedPaths []string, cat *domain.ResourceCatalog, desc domain.ComponentDescriptor) []ResourceChange {
	dependedOn := map[string]bool{}
	var resources []domain.CatalogResource
	if cat != nil {
		resources = cat.Spec.Resources
		provider := map[string]string{}
		for _, r := range resources {
			provider[r.Name] = r.Path
			for _, p := range r.Provides {
				if _, taken := provider[p]; !taken {
					provider[p] = r.Path
				}
			}
		}
		for _, r := range resources {
			for _, d := range r.Dependencies {
				if owner, ok := provider[d]; ok && owner != r.Path {
					dependedOn[owner] = true
				}
			}
		}
	}
	single := desc.Layout == discovery.LayoutSingle && desc.ResourceName != nil

	seen := map[string]bool{}
	out := make([]ResourceChange, 0, len(changedPaths))
	for _, p := range changedPaths {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		owner := ""
		if single {
			owner = *desc.ResourceName
		} else {
			for _, r := range resources {
				if strings.HasPrefix(p, r.Path+"/") {
					// longest match wins (nested categories cannot nest
					// resources, so at most one resource owns a path)
					if len(r.Path) > len(owner) {
						owner = r.Path
					}
				}
			}
		}
		switch {
		case owner == "":
			out = append(out, ResourceChange{Path: p, Kind: ChangeGlobal, Reason: "outside every catalog resource"})
		case isManifest(path.Base(p)):
			out = append(out, ResourceChange{Path: p, Resource: owner, Kind: ChangeRestartRequired, Reason: "manifest changed"})
		case dependedOn[owner]:
			out = append(out, ResourceChange{Path: p, Resource: owner, Kind: ChangeRestartRequired, Reason: "other resources depend on this resource"})
		default:
			out = append(out, ResourceChange{Path: p, Resource: owner, Kind: ChangeHotReloadSafe, Reason: "confined to one independent resource"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func isManifest(base string) bool {
	return base == discovery.FxManifest || base == discovery.LegacyManifest
}
