package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/release"
	"fxvcs.dev/fxvcs/internal/stack"
)

// ReleaseCreateInput describes the release to build.
type ReleaseCreateInput struct {
	Name string
	// Pins are "<repositoryID>=<ref>" overrides of the stack ref.
	Pins []string
	// Enable and Disable are release-only activation overrides.
	Enable, Disable []string
	VariantOf       string
	CreatedBy       string
	// TagComponents creates durable namespaced tags in every component remote
	// before the release is written.
	TagComponents bool
	// SkipObjectCheck skips storage verification entirely.
	SkipObjectCheck bool
	// AllowUnverifiedObjects proceeds when a required remote cannot be reached.
	AllowUnverifiedObjects bool
	// Force is not accepted: an existing release is never overwritten. The
	// field exists so callers cannot invent one.
}

// ReleaseCreateResult is the built release and its review material.
type ReleaseCreateResult struct {
	Name       string                    `json:"name"`
	File       string                    `json:"file"`
	Written    bool                      `json:"written"`
	Digest     string                    `json:"digest"`
	SourceSet  string                    `json:"sourceSetDigest"`
	Components []release.ComponentReport `json:"components"`
	Resources  []PlannedResource         `json:"resources"`
	Enabled    int                       `json:"enabled"`
	Disabled   int                       `json:"disabled"`
	Tags       []release.ComponentTag    `json:"tags,omitempty"`
	Warnings   []string                  `json:"warnings,omitempty"`
	Problems   []stack.Problem           `json:"problems,omitempty"`
	Release    *domain.Release           `json:"release,omitempty"`
	// Change is the diff against the previous release, when there is one.
	Change *release.ChangeSet `json:"change,omitempty"`
}

// ReleaseCreate resolves the composition, builds the immutable record, and
// writes it. It never commits: the release exists once a human commits the
// file, which keeps the Git provider's review, history, and protection rules
// in the loop.
func (c *Coord) ReleaseCreate(ctx context.Context, in ReleaseCreateInput) (*ReleaseCreateResult, error) {
	pins, err := parsePins(in.Pins)
	if err != nil {
		return nil, err
	}
	createdBy := in.CreatedBy
	if createdBy == "" {
		createdBy = c.gitIdentity(ctx)
	}
	create := release.CreateInput{
		Name: in.Name, Pins: pins, Enable: in.Enable, Disable: in.Disable,
		VariantOf: in.VariantOf, CreatedBy: createdBy, Now: c.opts.Now(),
		AllowUnverifiedObjects: in.AllowUnverifiedObjects,
	}
	if !in.SkipObjectCheck {
		checker, err := c.ObjectChecker(ctx)
		if err != nil {
			return nil, err
		}
		create.Objects = checker
	}
	if in.VariantOf != "" {
		if _, err := c.GetRelease(ctx, in.VariantOf); err != nil {
			return nil, fmt.Errorf("%w (--variant-of names a release that does not exist here)", err)
		}
	}

	built, buildErr := release.Create(ctx, c.Repo, create)
	if built == nil {
		return nil, buildErr
	}
	res := &ReleaseCreateResult{
		Name: in.Name, Components: built.Components, Warnings: built.Warnings,
		Problems: built.Problems, Resources: []PlannedResource{},
	}
	if buildErr != nil {
		return res, buildErr
	}

	rel := built.Release
	res.SourceSet = rel.Spec.SourceSetDigest
	res.Release = &rel
	if res.Digest, err = integrity.ReleaseDigest(rel); err != nil {
		return res, err
	}
	res.Resources, res.Enabled, res.Disabled = plannedRows(built.Plan.Resources, built.Release.Spec.Components)

	// Tag before writing. A release that advertises durable component pins it
	// failed to create would be worse than no release at all.
	if in.TagComponents {
		tags, err := release.TagComponents(ctx, c.Repo, rel)
		res.Tags = tags
		if err != nil {
			return res, err
		}
	}

	path, written, err := c.WriteRelease(ctx, rel)
	res.File, res.Written = c.Rel(path), written
	if err != nil {
		return res, err
	}
	if prev := c.previousRelease(ctx, in.Name); prev != nil {
		cs := release.Diff(*prev, rel)
		res.Change = &cs
	}
	return res, nil
}

// previousRelease returns the newest release other than name, for the change
// set. Names sort lexically, which is what `v1.2.0` style names want.
func (c *Coord) previousRelease(ctx context.Context, name string) *domain.Release {
	names, err := c.ListReleases()
	if err != nil {
		return nil
	}
	for i := len(names) - 1; i >= 0; i-- {
		if names[i] == name {
			continue
		}
		rel, err := c.GetRelease(ctx, names[i])
		if err != nil {
			continue
		}
		return &rel
	}
	return nil
}

// gitIdentity records who created a release, using the same identity Git
// would put on a commit. A release record is an audit trail; leaving the
// creator blank makes it a worse one, and asking for a flag every time makes
// it a flag people pass as "-".
func (c *Coord) gitIdentity(ctx context.Context) string {
	name, okName, _ := c.Git.ConfigGet(ctx, "user.name")
	email, okEmail, _ := c.Git.ConfigGet(ctx, "user.email")
	switch {
	case okName && okEmail:
		return fmt.Sprintf("%s <%s>", name, email)
	case okName:
		return name
	case okEmail:
		return email
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

func parsePins(pins []string) ([]release.Pin, error) {
	out := make([]release.Pin, 0, len(pins))
	for _, p := range pins {
		id, ref, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(id) == "" || strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("%w: --pin wants <repositoryID>=<ref>, got %q", ErrInvalidArgument, p)
		}
		out = append(out, release.Pin{RepositoryID: strings.TrimSpace(id), Ref: strings.TrimSpace(ref)})
	}
	return out, nil
}

func plannedRows(res []domain.ReleaseResource, comps []domain.ReleaseComponent) ([]PlannedResource, int, int) {
	byID := map[string]domain.ReleaseComponent{}
	for _, c := range comps {
		byID[c.RepositoryID] = c
	}
	rows := make([]PlannedResource, 0, len(res))
	var enabled, disabled int
	for _, r := range res {
		row := PlannedResource{
			Name: r.Name, Source: r.Source, RepositoryName: r.RepositoryName, Path: r.Path,
			ResourceID: r.ResourceID, Activation: string(r.Activation), ActivationSource: r.ActivationSource,
			EnsureOrder: r.EnsureOrder,
		}
		if comp, ok := byID[r.RepositoryID]; ok {
			if m, err := stack.MountFor(comp.Mount, comp.Descriptor, r.Path); err == nil {
				row.Mount = m
			}
		}
		if r.Activation == domain.ActivationEnabled {
			enabled++
		} else {
			disabled++
		}
		rows = append(rows, row)
	}
	return rows, enabled, disabled
}

// ---------------------------------------------------------------------------
// inspect / list
// ---------------------------------------------------------------------------

// ReleaseInspectResult explains one release.
type ReleaseInspectResult struct {
	Name            string                    `json:"name"`
	File            string                    `json:"file"`
	CreatedAt       string                    `json:"createdAt"`
	CreatedBy       string                    `json:"createdBy,omitempty"`
	StackID         string                    `json:"stackID"`
	Digest          string                    `json:"digest"`
	SourceSetDigest string                    `json:"sourceSetDigest"`
	VariantOf       string                    `json:"variantOf,omitempty"`
	MinimumVersion  string                    `json:"minimumFXVCSVersion,omitempty"`
	Components      []domain.ReleaseComponent `json:"components"`
	Resources       []PlannedResource         `json:"resources"`
	Enabled         int                       `json:"enabled"`
	Disabled        int                       `json:"disabled"`
	// SameSourceAs lists other releases with an identical sourceSetDigest.
	SameSourceAs []string `json:"sameSourceAs,omitempty"`
	// Change is the diff against a requested comparison release.
	Change *release.ChangeSet `json:"change,omitempty"`
}

// ReleaseInspect explains a release from its record alone.
func (c *Coord) ReleaseInspect(ctx context.Context, name, compareTo string) (*ReleaseInspectResult, error) {
	rel, err := c.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	digest, err := integrity.ReleaseDigest(rel)
	if err != nil {
		return nil, err
	}
	rows, enabled, disabled := plannedRows(rel.Spec.Resources, rel.Spec.Components)
	res := &ReleaseInspectResult{
		Name: name, File: c.Rel(c.ReleasePath(name)), CreatedAt: rel.Metadata.CreatedAt, CreatedBy: rel.Metadata.CreatedBy,
		StackID: rel.Spec.StackID, Digest: digest, SourceSetDigest: rel.Spec.SourceSetDigest,
		VariantOf: rel.Spec.VariantOf, MinimumVersion: rel.Spec.MinimumFXVCSVersion,
		Components: rel.Spec.Components, Resources: rows, Enabled: enabled, Disabled: disabled,
	}
	names, err := c.ListReleases()
	if err != nil {
		return nil, err
	}
	for _, other := range names {
		if other == name {
			continue
		}
		o, err := c.GetRelease(ctx, other)
		if err != nil {
			continue
		}
		if o.Spec.SourceSetDigest == rel.Spec.SourceSetDigest {
			res.SameSourceAs = append(res.SameSourceAs, other)
		}
	}
	if compareTo != "" {
		base, err := c.GetRelease(ctx, compareTo)
		if err != nil {
			return nil, err
		}
		cs := release.Diff(base, rel)
		res.Change = &cs
	}
	return res, nil
}

// ReleaseListRow is one release in a listing.
type ReleaseListRow struct {
	Name            string `json:"name"`
	CreatedAt       string `json:"createdAt"`
	CreatedBy       string `json:"createdBy,omitempty"`
	Components      int    `json:"components"`
	Enabled         int    `json:"enabled"`
	SourceSetDigest string `json:"sourceSetDigest"`
	VariantOf       string `json:"variantOf,omitempty"`
	// UsedBy names targets whose desired state is this release.
	UsedBy []string `json:"usedBy,omitempty"`
}

// ReleaseList lists every release with the targets that select it.
func (c *Coord) ReleaseList(ctx context.Context) ([]ReleaseListRow, error) {
	names, err := c.ListReleases()
	if err != nil {
		return nil, err
	}
	targets, err := c.ListTargets()
	if err != nil {
		return nil, err
	}
	desired := map[string][]string{}
	for _, t := range targets {
		if tgt, err := c.GetTarget(ctx, t); err == nil {
			desired[tgt.Spec.Release] = append(desired[tgt.Spec.Release], t)
		}
	}
	rows := make([]ReleaseListRow, 0, len(names))
	for _, n := range names {
		rel, err := c.GetRelease(ctx, n)
		if err != nil {
			return nil, err
		}
		row := ReleaseListRow{
			Name: n, CreatedAt: rel.Metadata.CreatedAt, CreatedBy: rel.Metadata.CreatedBy,
			Components: len(rel.Spec.Components), SourceSetDigest: rel.Spec.SourceSetDigest,
			VariantOf: rel.Spec.VariantOf, UsedBy: desired[n],
		}
		for _, r := range rel.Spec.Resources {
			if r.Activation == domain.ActivationEnabled {
				row.Enabled++
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// notes / export / import
// ---------------------------------------------------------------------------

// ReleaseNotesResult is the generated draft plus the change set it came from.
type ReleaseNotesResult struct {
	Release  string            `json:"release"`
	Since    string            `json:"since,omitempty"`
	Markdown string            `json:"markdown"`
	Change   release.ChangeSet `json:"change"`
	// File is set when the draft was written to disk.
	File string `json:"file,omitempty"`
}

// ReleaseNotes drafts Markdown notes from the change set between two releases.
// The draft is never written into the release record: prose is a separate,
// separately versioned artifact, so editing it cannot change release identity.
func (c *Coord) ReleaseNotes(ctx context.Context, name, since string) (*ReleaseNotesResult, error) {
	rel, err := c.GetRelease(ctx, name)
	if err != nil {
		return nil, err
	}
	var base domain.Release
	if since != "" {
		if base, err = c.GetRelease(ctx, since); err != nil {
			return nil, err
		}
	} else if prev := c.previousRelease(ctx, name); prev != nil {
		base = *prev
	}
	cs := release.Diff(base, rel)
	return &ReleaseNotesResult{Release: name, Since: base.Metadata.Name, Markdown: release.Notes(cs, rel), Change: cs}, nil
}

// ReleaseExportResult is a portable release document.
type ReleaseExportResult struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	// Document is the portable YAML.
	Document []byte `json:"-"`
	Bytes    int    `json:"bytes"`
	File     string `json:"file,omitempty"`
}

// ReleaseExport returns one release as a portable document.
func (c *Coord) ReleaseExport(ctx context.Context, name string) (*ReleaseExportResult, error) {
	_, data, digest, err := release.Export(ctx, c.Repo, name)
	if err != nil {
		return nil, err
	}
	return &ReleaseExportResult{Name: name, Digest: digest, Document: data, Bytes: len(data)}, nil
}

// ReleaseImport validates and stores a portable release document.
func (c *Coord) ReleaseImport(ctx context.Context, data []byte) (*release.ImportResult, error) {
	res, err := release.Import(ctx, c.Repo, data)
	if res != nil {
		res.Path = c.Rel(res.Path)
	}
	if err != nil && errors.Is(err, controlplane.ErrImmutable) {
		return res, err
	}
	return res, err
}
