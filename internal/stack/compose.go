// Package stack resolves a coordination composition: which component
// repositories participate, where each one mounts in the realization, which
// display name each one resolves to, and — with the runtime catalog and sparse
// overrides — the exact activation plan a Release snapshots.
//
// Everything here is pure: callers supply pinned component configuration and
// catalogs (read from Git in internal/controlplane/gitops), and get back either
// a resolved composition or the complete list of problems. Nothing performs
// I/O, and nothing fails on the first problem — an operator fixing a
// composition wants to see all of it at once.
package stack

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// Problem is one composition finding.
type Problem struct {
	// Component is the repositoryID the problem belongs to ("" for
	// composition-wide problems).
	Component string `json:"component,omitempty"`
	Field     string `json:"field"`
	Message   string `json:"message"`
}

func (p Problem) String() string {
	if p.Component == "" {
		return p.Field + ": " + p.Message
	}
	return p.Component + " " + p.Field + ": " + p.Message
}

// ErrInvalidComposition lets callers errors.Is a *CompositionError.
var ErrInvalidComposition = errors.New("stack: composition is not valid")

// CompositionError aggregates every problem found in one pass.
type CompositionError struct{ Problems []Problem }

func (e *CompositionError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return "stack: composition is not valid: " + strings.Join(parts, "; ")
}

func (e *CompositionError) Is(target error) bool { return target == ErrInvalidComposition }

// Component is one pinned component of a composition.
type Component struct {
	// Spec is the coordination record (remote, mount, alias, override).
	Spec domain.StackComponent
	// Commit is the resolved full commit id.
	Commit string
	// Config is the component's committed .fxvcs/repository.yaml at Commit.
	Config *domain.Repository
	// Catalog is the component's committed .fxvcs/resources.yaml at Commit
	// (nil for a non-FiveM component).
	Catalog *domain.ResourceCatalog
	// AssetRoot is the digest of the canonical asset index of the pinned tree.
	AssetRoot string
}

// Descriptor returns the accepted layout descriptor and its provenance. The
// component owns its source root and layout; coordination may record an
// explicit reviewed override, and the Release snapshots which one was used.
func (c Component) Descriptor() (domain.ComponentDescriptor, string) {
	if c.Spec.DescriptorOverride != nil {
		return *c.Spec.DescriptorOverride, domain.DescriptorFromStack
	}
	return domain.ComponentDescriptor{
		SourceRoot:   c.Config.Spec.SourceRoot,
		Layout:       c.Config.Spec.Layout,
		ResourceName: c.Config.Spec.ResourceName,
	}, domain.DescriptorFromComponent
}

// DisplayName is the coordination alias when one is recorded, otherwise the
// component's own metadata.name. Display names never enter sourceSetDigest.
func (c Component) DisplayName() string {
	if c.Spec.Alias != "" {
		return c.Spec.Alias
	}
	return c.Config.Metadata.Name
}

// OverridesComponent reports that coordination recorded a descriptor that
// disagrees with the component's own. It is legal — that is what an override
// is for — but it is always surfaced, because it changes the realization shape
// without any change in the component repository.
func (c Component) OverridesComponent() bool {
	if c.Spec.DescriptorOverride == nil {
		return false
	}
	own, _ := Component{Spec: domain.StackComponent{}, Config: c.Config}.Descriptor()
	return !sameDescriptor(own, *c.Spec.DescriptorOverride)
}

func sameDescriptor(a, b domain.ComponentDescriptor) bool {
	if a.SourceRoot != b.SourceRoot || a.Layout != b.Layout {
		return false
	}
	switch {
	case a.ResourceName == nil && b.ResourceName == nil:
		return true
	case a.ResourceName == nil || b.ResourceName == nil:
		return false
	default:
		return *a.ResourceName == *b.ResourceName
	}
}

// ValidateComposition checks everything that must hold across components
// before a Release may be built: identity uniqueness, descriptor/mount
// agreement, mount containment and overlap, category semantics, and resolved
// display-name uniqueness. It returns every problem it finds.
func ValidateComposition(comps []Component) []Problem {
	var probs []Problem
	add := func(id, field, msg string) { probs = append(probs, Problem{Component: id, Field: field, Message: msg}) }

	seenID := map[string]bool{}
	seenName := map[string]string{}
	type mountRec struct{ id, mount string }
	var mounts []mountRec

	for _, c := range comps {
		id := c.Spec.RepositoryID
		if id == "" {
			add("", "repositoryID", "component has no repositoryID")
			continue
		}
		if seenID[id] {
			add(id, "repositoryID", "registered more than once in this stack")
			continue
		}
		seenID[id] = true

		if c.Config == nil {
			add(id, "repository", "pinned commit has no .fxvcs/repository.yaml")
			continue
		}
		if got := c.Config.Metadata.RepositoryID; got != id {
			add(id, "repositoryID", fmt.Sprintf("pinned commit declares repositoryID %s; the stack registered %s (wrong remote, or a fork that needs its own identity)", got, id))
		}

		name := c.DisplayName()
		if other, dup := seenName[name]; dup {
			add(id, "name", fmt.Sprintf("resolved display name %q is already used by %s; add an alias to one of them", name, other))
		} else {
			seenName[name] = id
		}

		desc, from := c.Descriptor()
		mount, err := validateMount(c.Spec.Mount, desc)
		if err != nil {
			add(id, "mount", err.Error())
			continue
		}
		if from == domain.DescriptorFromStack && c.Config.Spec.Profile == domain.ProfileFiveM {
			if err := validateDescriptor(desc); err != nil {
				add(id, "descriptorOverride", err.Error())
				continue
			}
		}
		mounts = append(mounts, mountRec{id, mount})
	}

	for i := range mounts {
		for j := i + 1; j < len(mounts); j++ {
			a, b := mounts[i], mounts[j]
			switch {
			case a.mount == b.mount:
				add(b.id, "mount", fmt.Sprintf("mount %s is already used by %s", b.mount, a.id))
			case pathsafe.Overlaps(a.mount, b.mount):
				add(b.id, "mount", fmt.Sprintf("mount %s overlaps %s (%s); one realization path cannot be owned by two repositories", b.mount, a.mount, a.id))
			}
		}
	}
	return probs
}

// validateMount normalizes a mount and checks it against the accepted
// descriptor.
//
// The FiveM rule the mount has to respect: a plain folder under `resources/`
// is a resource, a bracketed folder is a category that holds resources. So a
// collection mounts *as* a category, and a single resource mounts as its own
// directory inside one.
func validateMount(mount string, desc domain.ComponentDescriptor) (string, error) {
	norm, err := pathsafe.Normalize(mount)
	if err != nil {
		return "", fmt.Errorf("%q is not a safe relative path: %v", mount, err)
	}
	if norm == "resources" || !strings.HasPrefix(norm, "resources/") {
		return "", fmt.Errorf("%q must be under resources/ (for example resources/[maps])", mount)
	}
	segs := strings.Split(norm, "/")[1:]
	for _, s := range segs {
		if s == "" {
			return "", fmt.Errorf("%q has an empty path segment", mount)
		}
	}
	last := segs[len(segs)-1]
	switch desc.Layout {
	case domain.LayoutCollection:
		if !pathsafe.IsBracketCategory(last) {
			return "", fmt.Errorf("%q holds several resources, so it must mount as a bracketed category such as resources/[%s]", mount, strings.Trim(last, "[]"))
		}
	case domain.LayoutSingle:
		if desc.ResourceName == nil || *desc.ResourceName == "" {
			return "", errors.New("single-resource layout has no resourceName")
		}
		if last != *desc.ResourceName {
			return "", fmt.Errorf("%q must end with the resource directory name %q", mount, *desc.ResourceName)
		}
		if pathsafe.IsBracketCategory(last) {
			return "", fmt.Errorf("%q ends in a bracketed category; a single resource mounts inside one, for example resources/[%s]/%s", mount, last, last)
		}
		if len(segs) < 2 || !pathsafe.IsBracketCategory(segs[len(segs)-2]) {
			return "", fmt.Errorf("%q must sit inside a bracketed category, for example resources/[%s]/%s", mount, *desc.ResourceName, *desc.ResourceName)
		}
	default:
		return "", fmt.Errorf("unknown layout %q", desc.Layout)
	}
	return norm, nil
}

// validateDescriptor checks an overridden descriptor on its own terms.
func validateDescriptor(desc domain.ComponentDescriptor) error {
	if desc.SourceRoot != "." {
		if _, err := pathsafe.Normalize(desc.SourceRoot); err != nil {
			return fmt.Errorf("sourceRoot %q is not a safe relative path: %v", desc.SourceRoot, err)
		}
	}
	switch desc.Layout {
	case domain.LayoutCollection:
		if desc.ResourceName != nil {
			return errors.New("collection layout must not set resourceName")
		}
	case domain.LayoutSingle:
		if desc.ResourceName == nil || *desc.ResourceName == "" {
			return errors.New("single layout requires resourceName")
		}
		if err := discovery.ValidateResourceName(*desc.ResourceName); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown layout %q", desc.Layout)
	}
	return nil
}

// MountFor returns the realization-relative directory of one resource: the
// component mount for a single-resource component, or the mount joined with
// the resource's catalog path for a collection.
func MountFor(mount string, desc domain.ComponentDescriptor, resourcePath string) (string, error) {
	if desc.Layout == domain.LayoutSingle {
		return mount, nil
	}
	rel, err := pathsafe.Normalize(resourcePath)
	if err != nil {
		return "", err
	}
	return path.Join(mount, rel), nil
}

// ProposeMount suggests the mount for a newly detected component: a bracketed
// category named after the repository for a collection, and the resource
// directory inside a category of its own name for a single resource.
func ProposeMount(displayName string, desc domain.ComponentDescriptor) (string, error) {
	name := strings.TrimSpace(displayName)
	if err := discovery.ValidateResourceName(strings.Trim(name, "[]")); err != nil {
		return "", fmt.Errorf("cannot propose a mount for %q: %w", displayName, err)
	}
	switch desc.Layout {
	case domain.LayoutCollection:
		return "resources/[" + name + "]", nil
	case domain.LayoutSingle:
		if desc.ResourceName == nil || *desc.ResourceName == "" {
			return "", errors.New("single layout requires resourceName")
		}
		return "resources/[" + *desc.ResourceName + "]/" + *desc.ResourceName, nil
	}
	return "", fmt.Errorf("unknown layout %q", desc.Layout)
}

// SortComponents orders components by repository ID so every derived document
// is deterministic regardless of stack file order.
func SortComponents(comps []Component) {
	sort.SliceStable(comps, func(i, j int) bool {
		return comps[i].Spec.RepositoryID < comps[j].Spec.RepositoryID
	})
}
