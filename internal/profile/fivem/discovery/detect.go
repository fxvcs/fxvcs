package discovery

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

// Layout values for domain.ComponentDescriptor.Layout.
const (
	LayoutSingle     = "single"
	LayoutCollection = "collection"
)

// Detection is the proposed descriptor for an unconfigured tree.
type Detection struct {
	Descriptor domain.ComponentDescriptor `json:"descriptor"`
	// ResourceNameProposed is true when Descriptor.ResourceName was derived
	// from the tree's directory basename rather than observed on a server.
	ResourceNameProposed bool `json:"resourceNameProposed"`
	// Resources are the resource paths relative to Descriptor.SourceRoot.
	Resources []string `json:"resources"`
	// Skipped lists manifests FXServer would not load from the proposed
	// source root. They are ignored rather than fatal, and reported so the
	// operator can see what will never deploy.
	Skipped []Skipped `json:"skipped,omitempty"`
}

// Detect proposes a component descriptor for the tree rooted at fsys.
// rootName is the directory/repository basename used as the proposed
// resource name for a single-resource tree. It fails closed with a typed
// *Error for every ambiguous shape.
func Detect(fsys fs.FS, rootName string) (Detection, error) {
	all, err := walkAll(fsys, ".")
	if err != nil {
		return Detection{}, err
	}
	var live, ignored []manifestDir
	for _, m := range all {
		if m.Ignored {
			ignored = append(ignored, m)
		} else {
			live = append(live, m)
		}
	}
	if len(live) == 0 {
		if len(ignored) > 0 {
			return Detection{}, &Error{Reason: ReasonVendorOnly, Path: ignored[0].Path, Detail: "manifests exist only beneath ignored vendor/build directories"}
		}
		return Detection{}, &Error{Reason: ReasonNoManifests, Detail: "no fxmanifest.lua or __resource.lua found"}
	}

	// Root manifest: single-resource repository.
	if live[0].Path == "." {
		if len(live) > 1 {
			return Detection{}, &Error{Reason: ReasonMixedShape, Path: live[1].Path, Detail: "root manifest plus nested resource directories"}
		}
		if err := ValidateResourceName(rootName); err != nil {
			return Detection{}, &Error{Reason: ReasonUnsafeName, Detail: "proposed single-resource name", Err: err}
		}
		name := rootName
		return Detection{
			Descriptor:           domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutSingle, ResourceName: &name},
			ResourceNameProposed: true,
			Resources:            []string{name},
		}, nil
	}

	// A manifest inside a [bracket] directory is never a resource to
	// FXServer; drop it before choosing a source root.
	live, skipped := partitionCategories(live)
	if len(live) == 0 {
		return Detection{}, &Error{Reason: ReasonCategoryManifest, Path: skipped[0].Path, Detail: "every manifest sits inside a [bracket] category directory"}
	}
	// Collection: reachable from "." or from the conventional wrapper.
	fromRoot := reachable(".", live)
	fromWrapper := reachable(ConventionalWrapper, live)
	var sourceRoot string
	var members []manifestDir
	switch {
	case len(fromRoot) > 0 && len(fromWrapper) > 0:
		return Detection{}, &Error{
			Reason:     ReasonAmbiguousSourceRoot,
			Detail:     "resources are reachable from both the tree root and resources/",
			Candidates: []Candidate{candidate(".", fromRoot), candidate(ConventionalWrapper, fromWrapper)},
		}
	case len(fromRoot) > 0:
		sourceRoot, members = ".", fromRoot
	case len(fromWrapper) > 0:
		sourceRoot, members = ConventionalWrapper, fromWrapper
	default:
		roots := map[string][]manifestDir{}
		for _, m := range live {
			r := plausibleRoot(m.Path)
			roots[r] = append(roots[r], m)
		}
		keys := make([]string, 0, len(roots))
		for k := range roots {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		cands := make([]Candidate, 0, len(keys))
		for _, k := range keys {
			cands = append(cands, candidate(k, roots[k]))
		}
		return Detection{}, &Error{
			Reason:     ReasonAmbiguousSourceRoot,
			Detail:     "resources are only reachable from a non-conventional source root; set sourceRoot explicitly after review",
			Candidates: cands,
		}
	}
	// Manifests that are not reachable from the chosen root are ignored, the
	// same way FXServer walks past them, and reported to the operator.
	if len(members) != len(live) {
		_, unreachable := partitionReachable(sourceRoot, live)
		skipped = append(skipped, unreachable...)
	}
	paths, err := memberPaths(sourceRoot, members)
	if err != nil {
		return Detection{}, err
	}
	return Detection{
		Descriptor: domain.ComponentDescriptor{SourceRoot: sourceRoot, Layout: LayoutCollection},
		Resources:  paths,
		Skipped:    sortSkipped(skipped),
	}, nil
}

// DetectDir is Detect over an OS directory; the directory basename is the
// proposed single-resource name.
func DetectDir(dir string) (Detection, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Detection{}, &Error{Reason: ReasonIO, Err: err}
	}
	return Detect(os.DirFS(abs), filepath.Base(abs))
}

func reachable(sourceRoot string, dirs []manifestDir) []manifestDir {
	var out []manifestDir
	for _, m := range dirs {
		if reachableFrom(sourceRoot, m.Path) {
			out = append(out, m)
		}
	}
	return out
}

func candidate(sourceRoot string, dirs []manifestDir) Candidate {
	c := Candidate{SourceRoot: sourceRoot, Layout: LayoutCollection, Resources: []string{}}
	for _, m := range dirs {
		c.Resources = append(c.Resources, relPath(sourceRoot, m.Path))
	}
	sort.Strings(c.Resources)
	return c
}

// memberPaths validates category and resource names and duplicate resource
// names, returning sorted source-root-relative paths.
func memberPaths(sourceRoot string, members []manifestDir) ([]string, error) {
	seen := map[string]string{}
	out := make([]string, 0, len(members))
	for _, m := range members {
		rel := relPath(sourceRoot, m.Path)
		if _, err := pathsafe.Normalize(rel); err != nil {
			return nil, &Error{Reason: ReasonUnsafeName, Path: m.Path, Err: err}
		}
		name := path.Base(rel)
		if err := ValidateResourceName(name); err != nil {
			return nil, &Error{Reason: ReasonUnsafeName, Path: m.Path, Err: err}
		}
		if prev, dup := seen[name]; dup {
			return nil, &Error{Reason: ReasonDuplicateName, Path: m.Path, Detail: fmt.Sprintf("resource name %q already used by %s", name, prev)}
		}
		seen[name] = m.Path
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}
