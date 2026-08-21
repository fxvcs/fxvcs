package release

import (
	"fmt"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
)

// A change set is the machine-readable answer to "what changed between these
// two releases". It is computed from the two immutable records alone, so it is
// reproducible by anyone holding them and needs no Git access. Prose written
// from it (release notes) is a separate artifact: regenerating the wording must
// never be able to change what the release is.

// ComponentChange is one component's difference between two releases.
type ComponentChange struct {
	RepositoryID   string `json:"repositoryID"`
	RepositoryName string `json:"repositoryName"`
	// Change is added | removed | updated | unchanged.
	Change     string `json:"change"`
	FromCommit string `json:"fromCommit,omitempty"`
	ToCommit   string `json:"toCommit,omitempty"`
	// AssetsChanged reports that the component's asset root moved, so its
	// tracked content is different (not only its source).
	AssetsChanged bool `json:"assetsChanged,omitempty"`
	// MountChanged reports a realization-shape change.
	MountChanged bool   `json:"mountChanged,omitempty"`
	FromMount    string `json:"fromMount,omitempty"`
	ToMount      string `json:"toMount,omitempty"`
}

// ResourceChange is one resource's difference between two releases.
type ResourceChange struct {
	Name string `json:"name"`
	// Change is added | removed | activation | order | provenance.
	Change         string `json:"change"`
	Source         string `json:"source"`
	RepositoryName string `json:"repositoryName,omitempty"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
}

// ChangeSet compares a base release with a new one.
type ChangeSet struct {
	From string `json:"from"`
	To   string `json:"to"`
	// SameSource reports equal sourceSetDigest: identical component commits,
	// assets, mounts, and portable non-component requirements. Two releases
	// that differ only in activation are same-source.
	SameSource bool              `json:"sameSource"`
	Components []ComponentChange `json:"components"`
	Resources  []ResourceChange  `json:"resources"`
	// Enabled/Disabled are the resource counts of the new release.
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
}

// Empty reports that nothing changed at all.
func (c ChangeSet) Empty() bool {
	for _, comp := range c.Components {
		if comp.Change != "unchanged" {
			return false
		}
	}
	return len(c.Resources) == 0
}

// Diff computes the change set from base to next. A zero base (no previous
// release) reports every component and resource as added.
func Diff(base, next domain.Release) ChangeSet {
	cs := ChangeSet{From: base.Metadata.Name, To: next.Metadata.Name,
		Components: []ComponentChange{}, Resources: []ResourceChange{}}
	cs.SameSource = base.Spec.SourceSetDigest != "" && base.Spec.SourceSetDigest == next.Spec.SourceSetDigest

	from := map[string]domain.ReleaseComponent{}
	for _, c := range base.Spec.Components {
		from[c.RepositoryID] = c
	}
	seen := map[string]bool{}
	for _, c := range next.Spec.Components {
		seen[c.RepositoryID] = true
		prev, existed := from[c.RepositoryID]
		ch := ComponentChange{RepositoryID: c.RepositoryID, RepositoryName: c.RepositoryName, ToCommit: c.Commit, ToMount: c.Mount}
		if !existed {
			ch.Change = "added"
			cs.Components = append(cs.Components, ch)
			continue
		}
		ch.FromCommit, ch.FromMount = prev.Commit, prev.Mount
		ch.AssetsChanged = prev.AssetRoot != c.AssetRoot
		ch.MountChanged = prev.Mount != c.Mount
		if prev.Commit == c.Commit && !ch.MountChanged {
			ch.Change = "unchanged"
		} else {
			ch.Change = "updated"
		}
		cs.Components = append(cs.Components, ch)
	}
	for _, c := range base.Spec.Components {
		if !seen[c.RepositoryID] {
			cs.Components = append(cs.Components, ComponentChange{
				RepositoryID: c.RepositoryID, RepositoryName: c.RepositoryName,
				Change: "removed", FromCommit: c.Commit, FromMount: c.Mount,
			})
		}
	}
	sort.Slice(cs.Components, func(i, j int) bool { return cs.Components[i].RepositoryID < cs.Components[j].RepositoryID })

	prevRes := map[string]domain.ReleaseResource{}
	for _, r := range base.Spec.Resources {
		prevRes[resourceKey(r)] = r
	}
	nextSeen := map[string]bool{}
	for _, r := range next.Spec.Resources {
		key := resourceKey(r)
		nextSeen[key] = true
		if r.Activation == domain.ActivationEnabled {
			cs.Enabled++
		} else {
			cs.Disabled++
		}
		prev, existed := prevRes[key]
		if !existed {
			cs.Resources = append(cs.Resources, ResourceChange{Name: r.Name, Change: "added", Source: r.Source,
				RepositoryName: r.RepositoryName, To: string(r.Activation)})
			continue
		}
		switch {
		case prev.Activation != r.Activation:
			cs.Resources = append(cs.Resources, ResourceChange{Name: r.Name, Change: "activation", Source: r.Source,
				RepositoryName: r.RepositoryName, From: string(prev.Activation), To: string(r.Activation)})
		case prev.ActivationSource != r.ActivationSource:
			cs.Resources = append(cs.Resources, ResourceChange{Name: r.Name, Change: "provenance", Source: r.Source,
				RepositoryName: r.RepositoryName, From: prev.ActivationSource, To: r.ActivationSource})
		case prev.EnsureOrder != r.EnsureOrder:
			cs.Resources = append(cs.Resources, ResourceChange{Name: r.Name, Change: "order", Source: r.Source,
				RepositoryName: r.RepositoryName, From: fmt.Sprint(prev.EnsureOrder), To: fmt.Sprint(r.EnsureOrder)})
		}
	}
	for _, r := range base.Spec.Resources {
		if !nextSeen[resourceKey(r)] {
			cs.Resources = append(cs.Resources, ResourceChange{Name: r.Name, Change: "removed", Source: r.Source,
				RepositoryName: r.RepositoryName, From: string(r.Activation)})
		}
	}
	sort.SliceStable(cs.Resources, func(i, j int) bool {
		if cs.Resources[i].Change != cs.Resources[j].Change {
			return changeRank(cs.Resources[i].Change) < changeRank(cs.Resources[j].Change)
		}
		return cs.Resources[i].Name < cs.Resources[j].Name
	})
	return cs
}

func changeRank(c string) int {
	switch c {
	case "added":
		return 0
	case "removed":
		return 1
	case "activation":
		return 2
	case "provenance":
		return 3
	default:
		return 4
	}
}

func resourceKey(r domain.ReleaseResource) string {
	if r.Source == domain.ResourceSourceComponent {
		return "component\x00" + r.RepositoryID + "\x00" + r.Path
	}
	return "id\x00" + r.ResourceID
}

// Notes renders a reviewable Markdown draft from a change set. The draft is a
// starting point for a human, not a published artifact, and it is written
// separately from the release so editing it cannot alter release identity.
func Notes(cs ChangeSet, next domain.Release) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", next.Metadata.Name)
	if next.Metadata.CreatedAt != "" {
		fmt.Fprintf(&b, "Released %s", next.Metadata.CreatedAt)
		if next.Metadata.CreatedBy != "" {
			fmt.Fprintf(&b, " by %s", next.Metadata.CreatedBy)
		}
		b.WriteString(".\n\n")
	}
	if cs.From != "" {
		fmt.Fprintf(&b, "Changes since %s.\n\n", cs.From)
		if cs.SameSource {
			b.WriteString("> Same source as the previous release: identical component commits, assets, and mounts. Only which resources start has changed.\n\n")
		}
	}

	var added, removed, updated []ComponentChange
	for _, c := range cs.Components {
		switch c.Change {
		case "added":
			added = append(added, c)
		case "removed":
			removed = append(removed, c)
		case "updated":
			updated = append(updated, c)
		}
	}
	if len(added)+len(removed)+len(updated) > 0 {
		b.WriteString("## Repositories\n\n")
		for _, c := range updated {
			fmt.Fprintf(&b, "- **%s** updated to `%s`", c.RepositoryName, shortCommit(c.ToCommit))
			if c.AssetsChanged {
				b.WriteString(" (assets changed)")
			}
			if c.MountChanged {
				fmt.Fprintf(&b, " (now installed at `%s`)", c.ToMount)
			}
			b.WriteString("\n")
		}
		for _, c := range added {
			fmt.Fprintf(&b, "- **%s** added at `%s` (`%s`)\n", c.RepositoryName, c.ToMount, shortCommit(c.ToCommit))
		}
		for _, c := range removed {
			fmt.Fprintf(&b, "- **%s** removed\n", c.RepositoryName)
		}
		b.WriteString("\n")
	}

	sections := []struct {
		change, title string
	}{
		{"added", "Resources added"},
		{"removed", "Resources removed"},
		{"activation", "Resources turned on or off"},
	}
	for _, s := range sections {
		var lines []string
		for _, r := range cs.Resources {
			if r.Change != s.change {
				continue
			}
			switch s.change {
			case "activation":
				lines = append(lines, fmt.Sprintf("- `%s` is now **%s** (was %s)", r.Name, r.To, r.From))
			case "added":
				lines = append(lines, fmt.Sprintf("- `%s` (%s)", r.Name, r.To))
			default:
				lines = append(lines, fmt.Sprintf("- `%s`", r.Name))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.title, strings.Join(lines, "\n"))
		}
	}

	fmt.Fprintf(&b, "## What this release starts\n\n%d resources enabled, %d disabled.\n", cs.Enabled, cs.Disabled)
	return b.String()
}
