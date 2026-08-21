// Package discovery finds FiveM resources inside a component tree using the
// FXServer scanning rules and the component's accepted descriptor, and
// proposes a descriptor for an unconfigured tree. All paths are "/"-separated
// and relative to the tree root; brackets are literal directory names.
package discovery

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/pathsafe"
)

// Manifest file names, in precedence order for diagnostics.
const (
	FxManifest     = "fxmanifest.lua"
	LegacyManifest = "__resource.lua"
)

// ConventionalWrapper is the only non-bracket wrapper directory Detect
// proposes as an alternative source root.
const ConventionalWrapper = "resources"

// IgnoredDirectories are never scanned for resources. A manifest found only
// beneath one of them is a vendor/build artefact, not a component resource.
// The list is deliberately short and explicit; dot-directories are also
// skipped (see IsIgnoredDir).
var IgnoredDirectories = []string{"node_modules"}

// IsIgnoredDir reports whether a directory name is skipped during discovery.
func IsIgnoredDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, n := range IgnoredDirectories {
		if name == n {
			return true
		}
	}
	return false
}

// resourceNameRE is the FiveM resource-name rule enforced by FXVCS: letters,
// digits, underscore, hyphen, and dot, not starting with a dot. Brackets,
// spaces, and slashes are never allowed in a resource name.
var resourceNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// ValidateResourceName checks a resource directory/runtime name.
func ValidateResourceName(name string) error {
	if _, err := pathsafe.Normalize(name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("resource name %q must be a single path segment", name)
	}
	if pathsafe.IsBracketCategory(name) || strings.ContainsAny(name, "[]") {
		return fmt.Errorf("resource name %q must not contain brackets", name)
	}
	if !resourceNameRE.MatchString(name) {
		return fmt.Errorf("resource name %q must match %s", name, resourceNameRE)
	}
	return nil
}

// Reason classifies a discovery failure.
type Reason string

const (
	ReasonNoManifests         Reason = "no-manifests"
	ReasonVendorOnly          Reason = "vendor-only"
	ReasonMixedShape          Reason = "mixed-shape"
	ReasonAmbiguousSourceRoot Reason = "ambiguous-source-root"
	ReasonUnreachable         Reason = "unreachable-resources"
	ReasonCategoryManifest    Reason = "category-manifest"
	ReasonDuplicateManifests  Reason = "duplicate-manifests"
	ReasonUnsafeName          Reason = "unsafe-name"
	ReasonDuplicateName       Reason = "duplicate-resource-name"
	ReasonBadDescriptor       Reason = "bad-descriptor"
	ReasonManifest            Reason = "manifest"
	ReasonIO                  Reason = "io"
)

// Error is the typed discovery failure. Discovery fails closed: the first
// reason encountered aborts the operation.
type Error struct {
	Reason     Reason
	Path       string // tree-relative path the reason refers to ("" for the root)
	Detail     string
	Candidates []Candidate // populated for ambiguity reasons
	Err        error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("discovery: ")
	b.WriteString(string(e.Reason))
	if e.Path != "" {
		b.WriteString(" at ")
		b.WriteString(e.Path)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// ErrDiscovery lets callers errors.Is any *Error.
var ErrDiscovery = errors.New("discovery failed")

func (e *Error) Is(target error) bool { return target == ErrDiscovery }

// Candidate is one plausible descriptor found by Detect.
type Candidate struct {
	SourceRoot   string   `json:"sourceRoot"`
	Layout       string   `json:"layout"`
	ResourceName string   `json:"resourceName,omitempty"`
	Resources    []string `json:"resources"` // resource paths relative to SourceRoot
}

// manifestDir is a directory that directly contains a manifest.
type manifestDir struct {
	Path     string // tree-relative, "." for the root
	File     string // FxManifest or LegacyManifest
	Ignored  bool   // beneath an ignored directory
	Category bool   // the directory itself is a [bracket] category
}

// manifestIn reports which manifest file dir contains, or "" if none.
func manifestIn(fsys fs.FS, dir string, entries []fs.DirEntry) (string, error) {
	var fx, legacy bool
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch e.Name() {
		case FxManifest:
			fx = true
		case LegacyManifest:
			legacy = true
		}
	}
	if fx && legacy {
		return "", &Error{Reason: ReasonDuplicateManifests, Path: dir, Detail: "both fxmanifest.lua and __resource.lua present"}
	}
	if fx {
		return FxManifest, nil
	}
	if legacy {
		return LegacyManifest, nil
	}
	return "", nil
}

// walkAll lists every manifest directory below root without descending into
// resource directories (a resource never contains another resource) and
// marking manifests beneath ignored directories. Results are sorted by path.
func walkAll(fsys fs.FS, root string) ([]manifestDir, error) {
	var out []manifestDir
	var visit func(dir string, ignored bool) error
	visit = func(dir string, ignored bool) error {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return &Error{Reason: ReasonIO, Path: dir, Err: err}
		}
		file, err := manifestIn(fsys, dir, entries)
		if err != nil {
			return err
		}
		if file != "" {
			category := dir != root && pathsafe.IsBracketCategory(path.Base(dir))
			out = append(out, manifestDir{Path: dir, File: file, Ignored: ignored, Category: category})
			// A resource never contains another resource, so do not descend —
			// except below the root itself, so a root manifest plus nested
			// resources (mixed shape) is still detected, and except into a
			// bracket category, which FXServer keeps scanning even though it
			// warns about the stray manifest.
			if dir != root && !category {
				return nil
			}
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			child := path.Join(dir, n)
			if err := visit(child, ignored || IsIgnoredDir(n)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(root, false); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// reachableFrom reports whether FXServer scanning from sourceRoot would load
// the resource at dir: every intermediate directory must be a bracket
// category and the resource directory itself must not be one.
func reachableFrom(sourceRoot, dir string) bool {
	if dir == sourceRoot {
		return false // the source root itself is never a collection member
	}
	rel := relPath(sourceRoot, dir)
	if rel == "" {
		return false
	}
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		last := i == len(segs)-1
		if last {
			if pathsafe.IsBracketCategory(s) {
				return false
			}
		} else if !pathsafe.IsBracketCategory(s) {
			return false
		}
	}
	return true
}

// Skipped is a manifest directory that FXServer's scanner would not load and
// that discovery therefore ignores instead of failing: an unreachable manifest
// is dead weight in the tree, not a broken component. It is always reported so
// the operator can see what will never be deployed.
type Skipped struct {
	Path   string `json:"path"`   // tree-relative manifest directory
	File   string `json:"file"`   // FxManifest or LegacyManifest
	Reason Reason `json:"reason"` // ReasonUnreachable
	Detail string `json:"detail"`
}

// partitionCategories splits out manifests that sit directly inside a
// [bracket] directory. FXServer treats the directory as a category and never
// loads the manifest (it warns "is a category, but has a resource manifest"),
// so discovery reports it and moves on.
func partitionCategories(dirs []manifestDir) (rest []manifestDir, skipped []Skipped) {
	for _, m := range dirs {
		if !m.Category {
			rest = append(rest, m)
			continue
		}
		skipped = append(skipped, Skipped{
			Path:   m.Path,
			File:   m.File,
			Reason: ReasonCategoryManifest,
			Detail: "a [bracket] directory is a category, not a resource; FXServer would not load this manifest",
		})
	}
	return rest, skipped
}

// partitionReachable splits manifest directories into the ones FXServer would
// load from sourceRoot and the ones it would walk past. Both slices keep the
// input order (sorted by path).
func partitionReachable(sourceRoot string, dirs []manifestDir) (members []manifestDir, skipped []Skipped) {
	for _, m := range dirs {
		if reachableFrom(sourceRoot, m.Path) {
			members = append(members, m)
			continue
		}
		skipped = append(skipped, Skipped{
			Path:   m.Path,
			File:   m.File,
			Reason: ReasonUnreachable,
			Detail: fmt.Sprintf("not reachable from sourceRoot %q (only bracket categories are traversed); FXServer would not load it", sourceRoot),
		})
	}
	return members, skipped
}

// relPath returns dir relative to sourceRoot ("." means the tree root), or ""
// when dir is not strictly beneath sourceRoot.
func relPath(sourceRoot, dir string) string {
	if sourceRoot == "." {
		if dir == "." {
			return ""
		}
		return dir
	}
	if strings.HasPrefix(dir, sourceRoot+"/") {
		return dir[len(sourceRoot)+1:]
	}
	return ""
}

// plausibleRoot returns the source root from which dir would be reachable:
// its parent, then upward while parents are bracket categories.
func plausibleRoot(dir string) string {
	p := path.Dir(dir)
	for p != "." && pathsafe.IsBracketCategory(path.Base(p)) {
		p = path.Dir(p)
	}
	return p
}

// sortSkipped orders skipped entries by path for stable output.
func sortSkipped(s []Skipped) []Skipped {
	sort.Slice(s, func(i, j int) bool { return s[i].Path < s[j].Path })
	return s
}
