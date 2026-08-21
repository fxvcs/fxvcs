package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/profile/fivem/catalog"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/release"
	"fxvcs.dev/fxvcs/internal/schema"
	"fxvcs.dev/fxvcs/internal/stack"
)

// ErrStack marks a coordination-composition failure.
var ErrStack = errors.New("app: stack")

// ---------------------------------------------------------------------------
// stack init
// ---------------------------------------------------------------------------

// StackInitInput describes the coordination repository to create or adopt.
type StackInitInput struct {
	// Path is any directory inside the Git repository to use.
	Path string
	// Name is the stack display name (default: the directory name).
	Name string
	// Profile is the composition profile (default: fivem).
	Profile string
	// Adopt accepts an existing fxvcs.yaml instead of failing.
	Adopt bool
}

// StackInitResult reports what init did.
type StackInitResult struct {
	Root    string `json:"root"`
	StackID string `json:"stackID"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	File    string `json:"file"`
	Adopted bool   `json:"adopted"`
}

// StackInit creates or adopts a coordination repository. It writes one
// reviewable file and never commits: the stack becomes real when a human
// commits it, which is the whole contract of a GitOps control plane.
func StackInit(ctx context.Context, in StackInitInput, opts Options) (*StackInitResult, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	git := gitclient.New(in.Path)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	root, err := git.TopLevel(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotGitWorktree, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".fxvcs", "repository.yaml")); err == nil {
		return nil, fmt.Errorf("%w: %s is a component repository; a coordination repository must be separate so component access and composition access stay separate",
			ErrInvalidArgument, root)
	}
	name := in.Name
	if name == "" {
		name = filepath.Base(root)
	}
	if !displayNameRE.MatchString(name) {
		return nil, fmt.Errorf("%w: name %q must match %s (pass --name)", ErrInvalidArgument, name, displayNameRE)
	}
	profile := in.Profile
	if profile == "" {
		profile = domain.ProfileFiveM
	}
	if profile != domain.ProfileFiveM && profile != domain.ProfileGeneric {
		return nil, fmt.Errorf("%w: profile %q (generic|fivem)", ErrInvalidArgument, profile)
	}
	existing := false
	if _, err := os.Stat(filepath.Join(root, gitops.StackFile)); err == nil {
		existing = true
	}
	id, err := newID("stack_", opts.Now())
	if err != nil {
		return nil, err
	}
	ws := NewStack(id, name, profile, minimumVersionFor(opts.Installed))
	repo, err := gitops.Init(ctx, root, ws, in.Adopt, gitops.Options{Installed: opts.Installed, GitExecutable: opts.GitExecutable})
	if err != nil {
		return nil, err
	}
	return &StackInitResult{
		Root: repo.Root, StackID: repo.Stack.Metadata.StackID, Name: repo.Stack.Metadata.Name,
		Profile: repo.Stack.Spec.Profile, File: repo.Rel(repo.Path(gitops.StackFile)), Adopted: existing,
	}, nil
}

// ---------------------------------------------------------------------------
// stack add
// ---------------------------------------------------------------------------

// StackAddInput registers one component repository.
type StackAddInput struct {
	// Remote is the component's Git URL.
	Remote string
	// Ref is the branch or tag to follow (default: the remote's default branch).
	Ref string
	// Mount overrides the proposed mount.
	Mount string
	// Alias resolves a display-name collision.
	Alias string
	// Optional marks the component not required for a release.
	Optional bool
}

// ComponentProposal is what add or rescan detected.
type ComponentProposal struct {
	RepositoryID   string                     `json:"repositoryID"`
	Name           string                     `json:"name"`
	Remote         string                     `json:"remote"`
	Ref            string                     `json:"ref,omitempty"`
	Commit         string                     `json:"commit"`
	Profile        string                     `json:"profile"`
	Descriptor     domain.ComponentDescriptor `json:"descriptor"`
	DescriptorFrom string                     `json:"descriptorFrom"`
	Mount          string                     `json:"mount"`
	Alias          string                     `json:"alias,omitempty"`
	Required       bool                       `json:"required"`
	// Resources is the number of resources in the pinned catalog.
	Resources int `json:"resources"`
	// Detected is the layout discovery found in the pinned tree; it is shown
	// next to the component's own declaration so a disagreement is visible.
	Detected *domain.ComponentDescriptor `json:"detected,omitempty"`
}

// StackAddResult reports the registration.
type StackAddResult struct {
	Component ComponentProposal `json:"component"`
	File      string            `json:"file"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// StackAdd fetches a component repository, reads its committed identity
// and layout, proposes a mount, and records it in fxvcs.yaml. It never
// commits.
func (c *Coord) StackAdd(ctx context.Context, in StackAddInput) (*StackAddResult, error) {
	if strings.TrimSpace(in.Remote) == "" {
		return nil, fmt.Errorf("%w: a component needs a Git URL", ErrInvalidArgument)
	}
	staging := filepath.Join(c.GitDir, "fxvcs", "components", ".adding.git")
	if err := os.RemoveAll(staging); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if _, err := c.Git.Run(ctx, "clone", "--mirror", "--quiet", in.Remote, staging); err != nil {
		return nil, fmt.Errorf("%w: cannot read %s: %v", ErrInvalidArgument, in.Remote, err)
	}

	// Read identity from the staging mirror before it gets its final name: the
	// mirror directory is keyed by repositoryID, and the repository is the
	// only authority on what that is.
	mirror := c.Git.WithDir(staging)
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	}
	commit, err := mirror.RevParse(ctx, ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no ref %q", controlplane.ErrNotFound, in.Remote, ref)
	}
	cfgData, err := mirror.CatFileBlob(ctx, commit+":.fxvcs/repository.yaml")
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not an fxvcs repository at %s (run `fxvcs init` there and commit .fxvcs/repository.yaml)",
			ErrInvalidArgument, in.Remote, ref)
	}
	cfg, err := loadRepositoryDocument(cfgData)
	if err != nil {
		return nil, err
	}
	if _, dup := c.Component(cfg.Metadata.RepositoryID); dup {
		return nil, fmt.Errorf("%w: %s (%s) is already registered in this stack", controlplane.ErrExists, cfg.Metadata.Name, cfg.Metadata.RepositoryID)
	}

	// Move the staging mirror into place under its real identity, so the
	// clone we just paid for is the one `stack sync` keeps using.
	final, err := c.MirrorPath(cfg.Metadata.RepositoryID)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(final); err != nil {
		return nil, err
	}
	if err := os.Rename(staging, final); err != nil {
		return nil, err
	}

	res := &StackAddResult{}
	spec := domain.StackComponent{
		RepositoryID: cfg.Metadata.RepositoryID,
		Remote:       in.Remote,
		Ref:          in.Ref,
		Required:     !in.Optional,
		Alias:        in.Alias,
	}
	comp := stack.Component{Spec: spec, Commit: commit, Config: cfg}
	desc, from := comp.Descriptor()

	prop := ComponentProposal{
		RepositoryID: cfg.Metadata.RepositoryID, Name: cfg.Metadata.Name, Remote: in.Remote, Ref: in.Ref,
		Commit: commit, Profile: cfg.Spec.Profile, Descriptor: desc, DescriptorFrom: from, Required: !in.Optional,
	}

	if cfg.Spec.Profile == domain.ProfileFiveM {
		tree, err := c.Tree(ctx, cfg.Metadata.RepositoryID, commit)
		if err != nil {
			return nil, err
		}
		if detected, err := detectInTree(ctx, tree, cfg.Metadata.Name); err == nil {
			prop.Detected = &detected
			if !sameDescriptorValue(detected, desc) {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"the repository declares %s but its files look like %s; check .fxvcs/repository.yaml in %s",
					describeDescriptor(desc), describeDescriptor(detected), cfg.Metadata.Name))
			}
		}
		if cat, err := tree.ResourceCatalog(ctx); err == nil {
			prop.Resources = len(cat.Spec.Resources)
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s has no resource list yet; run `fxvcs resources sync` there and commit .fxvcs/resources.yaml before creating a release", cfg.Metadata.Name))
		}
	}

	// Resolve the display name against the rest of the composition before the
	// mount is proposed: the mount is normally named after it.
	display := cfg.Metadata.Name
	if in.Alias != "" {
		display = in.Alias
	}
	for _, other := range c.Stack.Spec.Components {
		if other.RepositoryID == cfg.Metadata.RepositoryID {
			continue
		}
		if otherName, err := c.displayNameOf(ctx, other); err == nil && otherName == display {
			return nil, fmt.Errorf("%w: another component already resolves to the display name %q; pass --alias to give this one a distinct name",
				ErrInvalidArgument, display)
		}
	}

	mount := in.Mount
	if mount == "" {
		mount, err = stack.ProposeMount(display, desc)
		if err != nil {
			return nil, fmt.Errorf("%w: %v (pass --mount)", ErrInvalidArgument, err)
		}
	}
	spec.Mount = mount
	prop.Mount, prop.Alias = mount, in.Alias
	comp.Spec = spec

	next := *c.Stack
	next.Spec.Components = append(append([]domain.StackComponent{}, c.Stack.Spec.Components...), spec)
	if probs := stack.ValidateComposition(append(c.componentsFor(ctx, next.Spec.Components[:len(next.Spec.Components)-1]), comp)); len(probs) > 0 {
		return nil, &stack.CompositionError{Problems: probs}
	}
	if err := c.SaveStack(&next); err != nil {
		return nil, err
	}
	res.Component = prop
	res.File = c.Rel(c.Path(gitops.StackFile))
	return res, nil
}

// componentsFor loads the pinned view of already-registered components, skipping
// any that cannot be read; validation of an addition should not fail because an
// unrelated component has not been fetched yet.
func (c *Coord) componentsFor(ctx context.Context, specs []domain.StackComponent) []stack.Component {
	var out []stack.Component
	for _, s := range specs {
		commit, err := c.ResolveRef(ctx, s.RepositoryID, s.Ref)
		if err != nil {
			continue
		}
		tree, err := c.Tree(ctx, s.RepositoryID, commit)
		if err != nil {
			continue
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			continue
		}
		out = append(out, stack.Component{Spec: s, Commit: commit, Config: cfg})
	}
	return out
}

func (c *Coord) displayNameOf(ctx context.Context, spec domain.StackComponent) (string, error) {
	if spec.Alias != "" {
		return spec.Alias, nil
	}
	commit, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref)
	if err != nil {
		return "", err
	}
	tree, err := c.Tree(ctx, spec.RepositoryID, commit)
	if err != nil {
		return "", err
	}
	cfg, err := tree.RepositoryConfig(ctx)
	if err != nil {
		return "", err
	}
	return cfg.Metadata.Name, nil
}

// ---------------------------------------------------------------------------
// stack sync
// ---------------------------------------------------------------------------

// ComponentSync is one component's fetch outcome.
type ComponentSync struct {
	RepositoryID string `json:"repositoryID"`
	Remote       string `json:"remote"`
	Ref          string `json:"ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	// Previous is the commit the mirror had before the fetch.
	Previous string `json:"previous,omitempty"`
	Updated  bool   `json:"updated"`
	Error    string `json:"error,omitempty"`
}

// StackSyncResult reports every component fetch.
type StackSyncResult struct {
	Components []ComponentSync `json:"components"`
	Fetched    int             `json:"fetched"`
	Updated    int             `json:"updated"`
	Failed     int             `json:"failed"`
}

// StackSync fetches every component repository (or the named ones) into
// its mirror. It is the only stack operation that uses the network.
func (c *Coord) StackSync(ctx context.Context, only []string, progress func(ComponentSync)) (*StackSyncResult, error) {
	res := &StackSyncResult{Components: []ComponentSync{}}
	selected, err := c.selectComponents(only)
	if err != nil {
		return nil, err
	}
	for _, spec := range selected {
		row := ComponentSync{RepositoryID: spec.RepositoryID, Remote: spec.Remote, Ref: spec.Ref}
		if before, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref); err == nil {
			row.Previous = before
		}
		if err := c.SyncComponent(ctx, spec); err != nil {
			row.Error = err.Error()
			res.Failed++
		} else {
			res.Fetched++
			if after, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref); err == nil {
				row.Commit = after
				row.Updated = row.Previous != "" && row.Previous != after
				if row.Updated {
					res.Updated++
				}
			}
		}
		if progress != nil {
			progress(row)
		}
		res.Components = append(res.Components, row)
	}
	if res.Failed > 0 && res.Fetched == 0 {
		return res, fmt.Errorf("%w: no component could be fetched", ErrStack)
	}
	return res, nil
}

func (c *Coord) selectComponents(only []string) ([]domain.StackComponent, error) {
	all := c.Stack.Spec.Components
	if len(only) == 0 {
		return all, nil
	}
	byID := map[string]domain.StackComponent{}
	for _, s := range all {
		byID[s.RepositoryID] = s
	}
	out := make([]domain.StackComponent, 0, len(only))
	for _, name := range only {
		s, ok := byID[name]
		if !ok {
			return nil, fmt.Errorf("%w: %q is not a component of this stack", controlplane.ErrNotFound, name)
		}
		out = append(out, s)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// stack status
// ---------------------------------------------------------------------------

// ComponentStatus is one row of stack status.
type ComponentStatus struct {
	RepositoryID string `json:"repositoryID"`
	Name         string `json:"name,omitempty"`
	Remote       string `json:"remote"`
	Ref          string `json:"ref,omitempty"`
	Mount        string `json:"mount"`
	Required     bool   `json:"required"`
	Fetched      bool   `json:"fetched"`
	Commit       string `json:"commit,omitempty"`
	Resources    int    `json:"resources,omitempty"`
	Error        string `json:"error,omitempty"`
	// ReleasedCommit is what the newest release pins for this component, when
	// there is one; a difference is exactly the drift an operator cares about.
	ReleasedCommit string `json:"releasedCommit,omitempty"`
	Drifted        bool   `json:"drifted,omitempty"`
}

// StackStatusResult is the whole composition at a glance.
type StackStatusResult struct {
	StackID       string            `json:"stackID"`
	Name          string            `json:"name"`
	Profile       string            `json:"profile"`
	Root          string            `json:"root"`
	Components    []ComponentStatus `json:"components"`
	Releases      int               `json:"releases"`
	LatestRelease string            `json:"latestRelease,omitempty"`
	Environments  []string          `json:"environments"`
	Targets       []string          `json:"targets"`
}

// StackStatus reads local state only; it never touches the network.
func (c *Coord) StackStatus(ctx context.Context) (*StackStatusResult, error) {
	res := &StackStatusResult{
		StackID: c.Stack.Metadata.StackID, Name: c.Stack.Metadata.Name,
		Profile: c.Stack.Spec.Profile, Root: c.Root, Components: []ComponentStatus{},
	}
	releases, err := c.ListReleases()
	if err != nil {
		return nil, err
	}
	res.Releases = len(releases)
	pinned := map[string]string{}
	if len(releases) > 0 {
		res.LatestRelease = releases[len(releases)-1]
		if rel, err := c.GetRelease(ctx, res.LatestRelease); err == nil {
			for _, comp := range rel.Spec.Components {
				pinned[comp.RepositoryID] = comp.Commit
			}
		}
	}
	if res.Environments, err = c.ListEnvironments(); err != nil {
		return nil, err
	}
	if res.Targets, err = c.ListTargets(); err != nil {
		return nil, err
	}
	for _, spec := range c.Stack.Spec.Components {
		row := ComponentStatus{
			RepositoryID: spec.RepositoryID, Remote: spec.Remote, Ref: spec.Ref,
			Mount: spec.Mount, Required: spec.Required, Fetched: c.HasMirror(spec.RepositoryID),
			ReleasedCommit: pinned[spec.RepositoryID],
		}
		if row.Fetched {
			commit, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.Commit = commit
				row.Drifted = row.ReleasedCommit != "" && row.ReleasedCommit != commit
				if tree, err := c.Tree(ctx, spec.RepositoryID, commit); err == nil {
					if cfg, err := tree.RepositoryConfig(ctx); err == nil {
						row.Name = cfg.Metadata.Name
						if spec.Alias != "" {
							row.Name = spec.Alias
						}
					}
					if cat, err := tree.ResourceCatalog(ctx); err == nil {
						row.Resources = len(cat.Spec.Resources)
					}
				}
			}
		} else {
			row.Error = "not fetched yet (run: fxvcs stack sync)"
		}
		res.Components = append(res.Components, row)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// stack validate
// ---------------------------------------------------------------------------

// PlannedResource is one row of the resolved activation plan.
type PlannedResource struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	RepositoryName   string `json:"repositoryName,omitempty"`
	Path             string `json:"path,omitempty"`
	ResourceID       string `json:"resourceID,omitempty"`
	Activation       string `json:"activation"`
	ActivationSource string `json:"activationSource"`
	EnsureOrder      int    `json:"ensureOrder"`
	// Mount is where the resource lands in the realization.
	Mount string `json:"mount,omitempty"`
}

// StackValidateResult is the composition report.
type StackValidateResult struct {
	Valid      bool              `json:"valid"`
	Components []ComponentStatus `json:"components"`
	Resources  []PlannedResource `json:"resources"`
	Enabled    int               `json:"enabled"`
	Disabled   int               `json:"disabled"`
	Problems   []stack.Problem   `json:"problems"`
	Warnings   []string          `json:"warnings,omitempty"`
}

// StackValidate resolves the whole composition from the currently fetched
// component refs and reports the plan a release would snapshot. It writes
// nothing and uses no network.
func (c *Coord) StackValidate(ctx context.Context) (*StackValidateResult, error) {
	res := &StackValidateResult{Components: []ComponentStatus{}, Resources: []PlannedResource{}, Problems: []stack.Problem{}}
	comps, rows, probs := c.resolveComponents(ctx)
	res.Components = rows
	res.Problems = append(res.Problems, probs...)
	res.Problems = append(res.Problems, stack.ValidateComposition(comps)...)

	runtimeCat, err := c.RuntimeResources()
	if err != nil {
		return nil, err
	}
	overrides, err := c.Overrides()
	if err != nil {
		return nil, err
	}
	var known []string
	for _, comp := range comps {
		if comp.Catalog == nil {
			continue
		}
		for _, r := range comp.Catalog.Spec.Resources {
			known = append(known, r.Name)
			known = append(known, r.Provides...)
		}
	}
	for _, rr := range runtimeCat.Spec.Resources {
		known = append(known, rr.Name)
	}
	for _, comp := range comps {
		if comp.Catalog == nil {
			continue
		}
		if err := catalog.ValidateForRelease(comp.Catalog, catalog.ValidateOptions{KnownNames: known, AllowPending: true}); err != nil {
			var ve *catalog.ValidationError
			if errors.As(err, &ve) {
				for _, p := range ve.Problems {
					res.Problems = append(res.Problems, stack.Problem{Component: comp.Spec.RepositoryID,
						Field: p.Field, Message: strings.TrimSpace(p.Path + " " + p.Message)})
				}
				continue
			}
			return nil, err
		}
	}

	plan, planProblems := stack.ResolvePlan(stack.PlanInput{Components: comps, Runtime: runtimeCat, Overrides: overrides})
	res.Problems = append(res.Problems, planProblems...)
	mounts := map[string]stack.Component{}
	for _, comp := range comps {
		mounts[comp.Spec.RepositoryID] = comp
	}
	for _, r := range plan.Resources {
		row := PlannedResource{
			Name: r.Name, Source: r.Source, RepositoryName: r.RepositoryName, Path: r.Path,
			ResourceID: r.ResourceID, Activation: string(r.Activation), ActivationSource: r.ActivationSource,
			EnsureOrder: r.EnsureOrder,
		}
		if comp, ok := mounts[r.RepositoryID]; ok {
			desc, _ := comp.Descriptor()
			if m, err := stack.MountFor(comp.Spec.Mount, desc, r.Path); err == nil {
				row.Mount = m
			}
		}
		if r.Activation == domain.ActivationEnabled {
			res.Enabled++
		} else {
			res.Disabled++
		}
		res.Resources = append(res.Resources, row)
	}
	res.Valid = len(res.Problems) == 0
	if !res.Valid {
		return res, &stack.CompositionError{Problems: res.Problems}
	}
	return res, nil
}

// resolveComponents loads every registered component at its configured ref.
func (c *Coord) resolveComponents(ctx context.Context) ([]stack.Component, []ComponentStatus, []stack.Problem) {
	var comps []stack.Component
	var rows []ComponentStatus
	var probs []stack.Problem
	for _, spec := range c.Stack.Spec.Components {
		row := ComponentStatus{RepositoryID: spec.RepositoryID, Remote: spec.Remote, Ref: spec.Ref,
			Mount: spec.Mount, Required: spec.Required, Fetched: c.HasMirror(spec.RepositoryID)}
		commit, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref)
		if err != nil {
			row.Error = err.Error()
			rows = append(rows, row)
			if spec.Required {
				probs = append(probs, stack.Problem{Component: spec.RepositoryID, Field: "ref", Message: err.Error()})
			}
			continue
		}
		row.Commit = commit
		tree, err := c.Tree(ctx, spec.RepositoryID, commit)
		if err != nil {
			row.Error = err.Error()
			rows = append(rows, row)
			probs = append(probs, stack.Problem{Component: spec.RepositoryID, Field: "commit", Message: err.Error()})
			continue
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			row.Error = err.Error()
			rows = append(rows, row)
			probs = append(probs, stack.Problem{Component: spec.RepositoryID, Field: "repository", Message: err.Error()})
			continue
		}
		comp := stack.Component{Spec: spec, Commit: commit, Config: cfg}
		row.Name = comp.DisplayName()
		if cfg.Spec.Profile == domain.ProfileFiveM {
			cat, err := tree.ResourceCatalog(ctx)
			if err != nil {
				probs = append(probs, stack.Problem{Component: spec.RepositoryID, Field: "resources",
					Message: "no .fxvcs/resources.yaml at this commit; run `fxvcs resources sync` in the component and commit it"})
			} else {
				comp.Catalog = cat
				row.Resources = len(cat.Spec.Resources)
			}
		}
		if root, _, err := release.AssetRoot(ctx, tree, cfg.Metadata.StorageDomainID); err == nil {
			comp.AssetRoot = root
		} else {
			probs = append(probs, stack.Problem{Component: spec.RepositoryID, Field: "assets", Message: err.Error()})
		}
		comps = append(comps, comp)
		rows = append(rows, row)
	}
	return comps, rows, probs
}

// ---------------------------------------------------------------------------
// stack rescan
// ---------------------------------------------------------------------------

// RescanFinding is one component's layout review.
type RescanFinding struct {
	RepositoryID string                      `json:"repositoryID"`
	Name         string                      `json:"name"`
	Commit       string                      `json:"commit"`
	Accepted     domain.ComponentDescriptor  `json:"accepted"`
	AcceptedFrom string                      `json:"acceptedFrom"`
	Detected     *domain.ComponentDescriptor `json:"detected,omitempty"`
	Mount        string                      `json:"mount"`
	// ProposedMount is set when the recorded mount no longer fits the layout.
	ProposedMount string `json:"proposedMount,omitempty"`
	// Agrees is false when detection disagrees with the accepted descriptor or
	// the mount no longer fits it.
	Agrees  bool     `json:"agrees"`
	Notes   []string `json:"notes,omitempty"`
	Updated bool     `json:"updated,omitempty"`
}

// StackRescanResult reports the review.
type StackRescanResult struct {
	Findings []RescanFinding `json:"findings"`
	Changed  bool            `json:"changed"`
	File     string          `json:"file,omitempty"`
}

// StackRescan re-detects each component's layout at its current ref and
// reports disagreement with what the stack recorded. It changes nothing
// unless accept is set — a mount is a realization-shape decision, and silently
// moving one would silently move files on every target.
func (c *Coord) StackRescan(ctx context.Context, only []string, accept bool) (*StackRescanResult, error) {
	selected, err := c.selectComponents(only)
	if err != nil {
		return nil, err
	}
	res := &StackRescanResult{Findings: []RescanFinding{}}
	next := *c.Stack
	next.Spec.Components = append([]domain.StackComponent{}, c.Stack.Spec.Components...)

	for _, spec := range selected {
		commit, err := c.ResolveRef(ctx, spec.RepositoryID, spec.Ref)
		if err != nil {
			return nil, err
		}
		tree, err := c.Tree(ctx, spec.RepositoryID, commit)
		if err != nil {
			return nil, err
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			return nil, err
		}
		comp := stack.Component{Spec: spec, Commit: commit, Config: cfg}
		desc, from := comp.Descriptor()
		f := RescanFinding{
			RepositoryID: spec.RepositoryID, Name: comp.DisplayName(), Commit: commit,
			Accepted: desc, AcceptedFrom: from, Mount: spec.Mount, Agrees: true,
		}
		if cfg.Spec.Profile == domain.ProfileFiveM {
			detected, derr := detectInTree(ctx, tree, cfg.Metadata.Name)
			if derr != nil {
				f.Agrees = false
				f.Notes = append(f.Notes, "the files could not be classified: "+derr.Error())
			} else {
				f.Detected = &detected
				if !sameDescriptorValue(detected, desc) {
					f.Agrees = false
					f.Notes = append(f.Notes, fmt.Sprintf("the stack uses %s but the files now look like %s",
						describeDescriptor(desc), describeDescriptor(detected)))
				}
			}
		}
		if probs := stack.ValidateComposition([]stack.Component{comp}); len(probs) > 0 {
			f.Agrees = false
			for _, p := range probs {
				f.Notes = append(f.Notes, p.Message)
			}
			if proposed, err := stack.ProposeMount(comp.DisplayName(), desc); err == nil {
				f.ProposedMount = proposed
			}
		}
		if accept && !f.Agrees {
			if f.Detected != nil && !sameDescriptorValue(*f.Detected, desc) {
				d := *f.Detected
				for i := range next.Spec.Components {
					if next.Spec.Components[i].RepositoryID == spec.RepositoryID {
						next.Spec.Components[i].DescriptorOverride = &d
					}
				}
				desc = d
			}
			mount := f.ProposedMount
			if mount == "" {
				if m, err := stack.ProposeMount(comp.DisplayName(), desc); err == nil {
					mount = m
				}
			}
			if mount != "" && mount != spec.Mount {
				for i := range next.Spec.Components {
					if next.Spec.Components[i].RepositoryID == spec.RepositoryID {
						next.Spec.Components[i].Mount = mount
					}
				}
				f.ProposedMount = mount
			}
			f.Updated = true
			res.Changed = true
		}
		res.Findings = append(res.Findings, f)
	}
	if res.Changed {
		if err := c.SaveStack(&next); err != nil {
			return nil, err
		}
		res.File = c.Rel(c.Path(gitops.StackFile))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// runtime-resources sync
// ---------------------------------------------------------------------------

// RuntimeResourcesSyncInput seeds the coordination runtime catalog.
type RuntimeResourcesSyncInput struct {
	// FromCfg is a server.cfg to read `ensure`/`start` directives from.
	FromCfg string
}

// RuntimeResourcesSyncResult reports the proposal.
type RuntimeResourcesSyncResult struct {
	File string `json:"file"`
	// Proposed are entries added as pending for review.
	Proposed []string `json:"proposed"`
	// Matched are cfg names already supplied by a component repository; they
	// belong to the component and must not be duplicated here.
	Matched []string `json:"matched"`
	// Existing are names already present in the runtime catalog.
	Existing  []string `json:"existing"`
	Files     []string `json:"files,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Written   bool     `json:"written"`
	Resources int      `json:"resources"`
}

// RuntimeResourcesSync proposes coordination-owned runtime/external entries
// from an existing server.cfg. Every proposal is `pending` with an
// `external:` ID: the source kind of a resource that is merely mentioned in a
// cfg is genuinely unknown, and guessing it from a familiar name is how a
// server ends up depending on a resource nobody owns. It never commits.
func (c *Coord) RuntimeResourcesSync(ctx context.Context, in RuntimeResourcesSyncInput) (*RuntimeResourcesSyncResult, error) {
	if strings.TrimSpace(in.FromCfg) == "" {
		return nil, fmt.Errorf("%w: runtime-resources sync needs --from-cfg <server.cfg>", ErrInvalidArgument)
	}
	imported, err := ParseServerCfg(in.FromCfg)
	if err != nil {
		return nil, err
	}
	cat, err := c.RuntimeResources()
	if err != nil {
		return nil, err
	}
	res := &RuntimeResourcesSyncResult{
		File: c.Rel(c.Path(gitops.RuntimeResourcesFile)), Files: imported.Files, Warnings: imported.Warnings,
		Proposed: []string{}, Matched: []string{}, Existing: []string{},
	}

	fromComponents := map[string]string{}
	comps, _, _ := c.resolveComponents(ctx)
	for _, comp := range comps {
		if comp.Catalog == nil {
			continue
		}
		for _, r := range comp.Catalog.Spec.Resources {
			fromComponents[r.Name] = comp.DisplayName()
			for _, p := range r.Provides {
				fromComponents[p] = comp.DisplayName()
			}
		}
	}
	known := map[string]bool{}
	for _, rr := range cat.Spec.Resources {
		known[rr.Name] = true
	}

	added := 0
	for _, name := range imported.Resources {
		switch {
		case known[name]:
			res.Existing = append(res.Existing, name)
		case fromComponents[name] != "":
			res.Matched = append(res.Matched, name)
		default:
			if err := discovery.ValidateResourceName(name); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			cat.Spec.Resources = append(cat.Spec.Resources, domain.RuntimeResource{
				ID:         "external:unreviewed/" + name,
				Name:       name,
				Source:     domain.ResourceSourceExternal,
				Activation: domain.ActivationPending,
				Verifier:   &domain.VerifierContract{Contract: "resource-present", Version: 1},
				Lifecycle:  &domain.LifecycleContract{Ownership: "external", Actions: "observe-only"},
			})
			res.Proposed = append(res.Proposed, name)
			added++
		}
	}
	sort.Slice(cat.Spec.Resources, func(i, j int) bool { return cat.Spec.Resources[i].ID < cat.Spec.Resources[j].ID })
	res.Resources = len(cat.Spec.Resources)
	if added > 0 {
		if err := c.SaveRuntimeResources(cat); err != nil {
			return nil, err
		}
		res.Written = true
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func detectInTree(ctx context.Context, tree *gitops.Tree, rootName string) (domain.ComponentDescriptor, error) {
	fsys, err := tree.FS(ctx)
	if err != nil {
		return domain.ComponentDescriptor{}, err
	}
	det, err := discovery.Detect(fsys, rootName)
	if err != nil {
		return domain.ComponentDescriptor{}, err
	}
	return det.Descriptor, nil
}

func sameDescriptorValue(a, b domain.ComponentDescriptor) bool {
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

func describeDescriptor(d domain.ComponentDescriptor) string {
	root := d.SourceRoot
	if root == "" || root == "." {
		root = "the repository root"
	}
	if d.Layout == domain.LayoutSingle && d.ResourceName != nil {
		return fmt.Sprintf("one resource named %q in %s", *d.ResourceName, root)
	}
	return "a folder of resources in " + root
}

func loadRepositoryDocument(data []byte) (*domain.Repository, error) {
	cfg, err := schema.LoadRepository(data)
	if err != nil {
		return nil, err
	}
	if cfg.Metadata.RepositoryID == "" {
		return nil, fmt.Errorf("%w: the component declares no repositoryID", ErrInvalidArgument)
	}
	return cfg, nil
}
