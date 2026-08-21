package discovery

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/profile/fivem/manifest"
)

// DiscoveredResource is one resource found beneath the accepted source root.
type DiscoveredResource struct {
	// Path is the resource path relative to the source root, normalized. For
	// a single-resource component it equals the descriptor's resourceName
	// (the source root itself is the resource directory).
	Path string `json:"path"`
	// Name is the FiveM runtime name: the resource directory basename, or the
	// descriptor's resourceName for a single-resource component. It is not the
	// manifest's cosmetic `name` directive.
	Name string `json:"name"`
	// ManifestFile is FxManifest or LegacyManifest.
	ManifestFile string `json:"manifestFile"`
	// Dir is the tree-relative directory ("." for a single-resource root).
	Dir      string             `json:"dir"`
	Manifest *manifest.Manifest `json:"manifest"`
}

// ScanOptions tunes Scan. The zero value uses the manifest package defaults.
type ScanOptions struct {
	Manifest manifest.Options
}

// ScanResult is the full outcome of a scan: the resources FXServer would load
// from the source root, and the manifests it would walk past.
type ScanResult struct {
	Resources []DiscoveredResource `json:"resources"`
	// Skipped lists manifests FXServer would not load (unreachable behind a
	// plain directory, or sitting inside a [bracket] category). They are
	// ignored, exactly as the server ignores them, and reported so the
	// operator can see what will never deploy.
	Skipped []Skipped `json:"skipped,omitempty"`
}

// Scan discovers resources beneath desc.SourceRoot using the accepted
// descriptor and evaluates every manifest in the sandbox. Results are sorted
// by Path. It fails closed with a typed *Error. Manifests FXServer itself
// would ignore are skipped, not fatal; use ScanTree to see them.
func Scan(ctx context.Context, fsys fs.FS, desc domain.ComponentDescriptor) ([]DiscoveredResource, error) {
	return ScanWithOptions(ctx, fsys, desc, ScanOptions{})
}

// ScanWithOptions is Scan with explicit sandbox limits.
func ScanWithOptions(ctx context.Context, fsys fs.FS, desc domain.ComponentDescriptor, opts ScanOptions) ([]DiscoveredResource, error) {
	res, err := ScanTree(ctx, fsys, desc, opts)
	if err != nil {
		return nil, err
	}
	return res.Resources, nil
}

// ScanTree is ScanWithOptions returning the skipped manifests as well.
func ScanTree(ctx context.Context, fsys fs.FS, desc domain.ComponentDescriptor, opts ScanOptions) (ScanResult, error) {
	sourceRoot, err := normalizeSourceRoot(desc.SourceRoot)
	if err != nil {
		return ScanResult{}, err
	}
	if sourceRoot != "." {
		info, statErr := fs.Stat(fsys, sourceRoot)
		if statErr != nil || !info.IsDir() {
			return ScanResult{}, &Error{Reason: ReasonBadDescriptor, Path: sourceRoot, Detail: "sourceRoot is not a directory"}
		}
	}
	all, err := walkAll(fsys, sourceRoot)
	if err != nil {
		return ScanResult{}, err
	}
	var live []manifestDir
	for _, m := range all {
		if !m.Ignored {
			live = append(live, m)
		}
	}

	switch desc.Layout {
	case LayoutSingle:
		if desc.ResourceName == nil || *desc.ResourceName == "" {
			return ScanResult{}, &Error{Reason: ReasonBadDescriptor, Detail: "layout single requires resourceName"}
		}
		name := *desc.ResourceName
		if err := ValidateResourceName(name); err != nil {
			return ScanResult{}, &Error{Reason: ReasonUnsafeName, Detail: "resourceName", Err: err}
		}
		if len(live) == 0 || live[0].Path != sourceRoot {
			return ScanResult{}, &Error{Reason: ReasonNoManifests, Path: sourceRoot, Detail: "layout single requires a manifest directly in the source root"}
		}
		if len(live) > 1 {
			return ScanResult{}, &Error{Reason: ReasonMixedShape, Path: live[1].Path, Detail: "single-resource component contains nested resource directories"}
		}
		r, err := evaluate(ctx, fsys, live[0], name, name, opts)
		if err != nil {
			return ScanResult{}, err
		}
		return ScanResult{Resources: []DiscoveredResource{r}}, nil
	case LayoutCollection:
		if desc.ResourceName != nil && *desc.ResourceName != "" {
			return ScanResult{}, &Error{Reason: ReasonBadDescriptor, Detail: "layout collection must not set resourceName"}
		}
		if len(live) > 0 && live[0].Path == sourceRoot {
			return ScanResult{}, &Error{Reason: ReasonMixedShape, Path: sourceRoot, Detail: "collection source root itself contains a manifest"}
		}
		// FXServer warns about these and keeps going; so do we. Failing the
		// whole component over a directory the server would never load makes
		// an unrelated vendor drop in a customer tree unusable.
		members, skipped := partitionCategories(live)
		members, unreachable := partitionReachable(sourceRoot, members)
		skipped = sortSkipped(append(skipped, unreachable...))
		if _, err := memberPaths(sourceRoot, members); err != nil {
			return ScanResult{}, err
		}
		out := make([]DiscoveredResource, 0, len(members))
		for _, m := range members {
			rel := relPath(sourceRoot, m.Path)
			r, err := evaluate(ctx, fsys, m, rel, path.Base(rel), opts)
			if err != nil {
				return ScanResult{}, err
			}
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return ScanResult{Resources: out, Skipped: skipped}, nil
	default:
		return ScanResult{}, &Error{Reason: ReasonBadDescriptor, Detail: fmt.Sprintf("unknown layout %q", desc.Layout)}
	}
}

// ScanDir is Scan over an OS directory.
func ScanDir(ctx context.Context, dir string, desc domain.ComponentDescriptor) ([]DiscoveredResource, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, &Error{Reason: ReasonIO, Err: err}
	}
	return Scan(ctx, os.DirFS(abs), desc)
}

func normalizeSourceRoot(sr string) (string, error) {
	if sr == "" || sr == "." {
		return ".", nil
	}
	n, err := pathsafe.Normalize(sr)
	if err != nil {
		return "", &Error{Reason: ReasonBadDescriptor, Path: sr, Detail: "sourceRoot", Err: err}
	}
	return n, nil
}

func evaluate(ctx context.Context, fsys fs.FS, m manifestDir, relPath, name string, opts ScanOptions) (DiscoveredResource, error) {
	if _, err := pathsafe.Normalize(relPath); err != nil {
		return DiscoveredResource{}, &Error{Reason: ReasonUnsafeName, Path: m.Path, Err: err}
	}
	src, err := fs.ReadFile(fsys, path.Join(m.Path, m.File))
	if err != nil {
		return DiscoveredResource{}, &Error{Reason: ReasonIO, Path: m.Path, Err: err}
	}
	mo := opts.Manifest
	mo.Legacy = m.File == LegacyManifest
	mo.DefaultName = name
	mo.ChunkName = m.File
	man, err := manifest.Evaluate(ctx, src, mo)
	if err != nil {
		return DiscoveredResource{}, &Error{Reason: ReasonManifest, Path: path.Join(m.Path, m.File), Err: err}
	}
	return DiscoveredResource{Path: relPath, Name: name, ManifestFile: m.File, Dir: m.Path, Manifest: man}, nil
}
