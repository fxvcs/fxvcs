package sdk

import (
	"context"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/release"
)

// ---------------------------------------------------------------------------
// shared release types
// ---------------------------------------------------------------------------

// ReleaseComponentInfo is one pinned component of a release.
type ReleaseComponentInfo struct {
	RepositoryID    string              `json:"repositoryID"`
	RepositoryName  string              `json:"repositoryName"`
	Remote          string              `json:"remote"`
	Commit          string              `json:"commit"`
	AssetRoot       string              `json:"assetRoot"`
	StorageDomainID string              `json:"storageDomainID"`
	Descriptor      ComponentDescriptor `json:"descriptor"`
	DescriptorFrom  string              `json:"descriptorFrom"`
	Mount           string              `json:"mount"`
}

func releaseComponents(in []domain.ReleaseComponent) []ReleaseComponentInfo {
	out := make([]ReleaseComponentInfo, 0, len(in))
	for _, c := range in {
		out = append(out, ReleaseComponentInfo{
			RepositoryID: c.RepositoryID, RepositoryName: c.RepositoryName, Remote: c.Remote,
			Commit: c.Commit, AssetRoot: c.AssetRoot, StorageDomainID: c.StorageDomainID,
			Descriptor: componentDescriptorOf(c.Descriptor), DescriptorFrom: c.DescriptorFrom, Mount: c.Mount,
		})
	}
	return out
}

// ObjectAvailability is one component's storage-verification result.
type ObjectAvailability struct {
	Remotes      []string `json:"remotes,omitempty"`
	Assets       int      `json:"assets"`
	Missing      []string `json:"missing,omitempty"`
	Unverifiable []string `json:"unverifiable,omitempty"`
}

// ComponentPin is what one component contributed to a release.
type ComponentPin struct {
	RepositoryID   string             `json:"repositoryID"`
	RepositoryName string             `json:"repositoryName"`
	Remote         string             `json:"remote"`
	Ref            string             `json:"ref,omitempty"`
	Commit         string             `json:"commit"`
	AssetRoot      string             `json:"assetRoot"`
	Assets         int                `json:"assets"`
	Mount          string             `json:"mount"`
	DescriptorFrom string             `json:"descriptorFrom"`
	Resources      int                `json:"resources"`
	Objects        ObjectAvailability `json:"objects,omitzero"`
	// DurablePin reports whether the component's remote still advertises a
	// branch or tag at this commit.
	DurablePin *bool `json:"durablePin,omitempty"`
}

func componentPins(in []release.ComponentReport) []ComponentPin {
	out := make([]ComponentPin, 0, len(in))
	for _, c := range in {
		out = append(out, ComponentPin{
			RepositoryID: c.RepositoryID, RepositoryName: c.RepositoryName, Remote: c.Remote, Ref: c.Ref,
			Commit: c.Commit, AssetRoot: c.AssetRoot, Assets: c.Assets, Mount: c.Mount,
			DescriptorFrom: c.DescriptorFrom, Resources: c.Resources, DurablePin: c.DurablePin,
			Objects: ObjectAvailability(c.Objects),
		})
	}
	return out
}

// ComponentTag is one durable component tag.
type ComponentTag struct {
	RepositoryID string `json:"repositoryID"`
	Remote       string `json:"remote"`
	Tag          string `json:"tag"`
	Commit       string `json:"commit"`
}

func componentTags(in []release.ComponentTag) []ComponentTag {
	out := make([]ComponentTag, 0, len(in))
	for _, t := range in {
		out = append(out, ComponentTag(t))
	}
	return out
}

// ComponentChange is one component's difference between two releases.
type ComponentChange struct {
	RepositoryID   string `json:"repositoryID"`
	RepositoryName string `json:"repositoryName"`
	Change         string `json:"change"`
	FromCommit     string `json:"fromCommit,omitempty"`
	ToCommit       string `json:"toCommit,omitempty"`
	AssetsChanged  bool   `json:"assetsChanged,omitempty"`
	MountChanged   bool   `json:"mountChanged,omitempty"`
	FromMount      string `json:"fromMount,omitempty"`
	ToMount        string `json:"toMount,omitempty"`
}

// ResourceChange is one resource's difference between two releases.
type ResourceChange struct {
	Name           string `json:"name"`
	Change         string `json:"change"`
	Source         string `json:"source"`
	RepositoryName string `json:"repositoryName,omitempty"`
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
}

// ReleaseChange is the portable change set between two releases.
type ReleaseChange struct {
	From string `json:"from"`
	To   string `json:"to"`
	// SameSource reports an identical sourceSetDigest: the same component
	// commits, assets, and mounts, whatever the activation plan says.
	SameSource bool              `json:"sameSource"`
	Components []ComponentChange `json:"components"`
	Resources  []ResourceChange  `json:"resources"`
	Enabled    int               `json:"enabled"`
	Disabled   int               `json:"disabled"`
}

func releaseChange(cs release.ChangeSet) *ReleaseChange {
	out := &ReleaseChange{From: cs.From, To: cs.To, SameSource: cs.SameSource,
		Enabled: cs.Enabled, Disabled: cs.Disabled,
		Components: make([]ComponentChange, 0, len(cs.Components)),
		Resources:  make([]ResourceChange, 0, len(cs.Resources))}
	for _, c := range cs.Components {
		out.Components = append(out.Components, ComponentChange(c))
	}
	for _, r := range cs.Resources {
		out.Resources = append(out.Resources, ResourceChange(r))
	}
	return out
}

// ---------------------------------------------------------------------------
// release create
// ---------------------------------------------------------------------------

// ReleaseCreateRequest builds an immutable release.
type ReleaseCreateRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Pins are "<repositoryID>=<ref>" overrides of the stack ref.
	Pins []string `json:"pins,omitempty"`
	// Enable and Disable are release-only activation overrides selected by
	// resource name, "<repositoryID>=<path>" identity, or runtime/external ID.
	Enable    []string `json:"enable,omitempty"`
	Disable   []string `json:"disable,omitempty"`
	VariantOf string   `json:"variantOf,omitempty"`
	CreatedBy string   `json:"createdBy,omitempty"`
	// TagComponents creates durable namespaced tags in every component remote
	// before the release is written. A tagging failure means no release.
	TagComponents bool `json:"tagComponents,omitempty"`
	// SkipObjectCheck skips storage verification entirely.
	SkipObjectCheck bool `json:"skipObjectCheck,omitempty"`
	// AllowUnverifiedObjects proceeds when a required storage cannot be
	// reached from this machine.
	AllowUnverifiedObjects bool `json:"allowUnverifiedObjects,omitempty"`
}

// ReleaseCreateResult is the built release and its review material.
type ReleaseCreateResult struct {
	Name            string               `json:"name"`
	File            string               `json:"file"`
	Written         bool                 `json:"written"`
	Digest          string               `json:"digest,omitempty"`
	SourceSetDigest string               `json:"sourceSetDigest,omitempty"`
	Components      []ComponentPin       `json:"components"`
	Resources       []PlannedResource    `json:"resources"`
	Enabled         int                  `json:"enabled"`
	Disabled        int                  `json:"disabled"`
	Tags            []ComponentTag       `json:"tags,omitempty"`
	Change          *ReleaseChange       `json:"change,omitempty"`
	Warnings        []string             `json:"warnings,omitempty"`
	Problems        []CompositionProblem `json:"problems,omitempty"`
}

// ReleaseCreate resolves every component, re-scans its pinned tree, validates
// the composition, resolves the activation plan, verifies object availability,
// and writes a reviewable immutable record. It never commits.
func (c *Client) ReleaseCreate(ctx context.Context, req ReleaseCreateRequest) (*ReleaseCreateResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release create")
	if err != nil {
		return nil, err
	}
	c.progress(Event{Operation: "release create", Kind: EventOperationStarted, Message: req.Name})
	out, err := co.ReleaseCreate(ctx, app.ReleaseCreateInput{
		Name: req.Name, Pins: req.Pins, Enable: req.Enable, Disable: req.Disable,
		VariantOf: req.VariantOf, CreatedBy: req.CreatedBy, TagComponents: req.TagComponents,
		SkipObjectCheck: req.SkipObjectCheck, AllowUnverifiedObjects: req.AllowUnverifiedObjects,
	})
	if out == nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &ReleaseCreateResult{
		Name: out.Name, File: out.File, Written: out.Written, Digest: out.Digest, SourceSetDigest: out.SourceSet,
		Components: componentPins(out.Components), Resources: plannedResources(out.Resources),
		Enabled: out.Enabled, Disabled: out.Disabled, Tags: componentTags(out.Tags),
		Warnings: out.Warnings, Problems: compositionProblems(out.Problems),
	}
	if out.Change != nil {
		res.Change = releaseChange(*out.Change)
	}
	for _, w := range res.Warnings {
		c.progress(Event{Operation: "release create", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	for _, p := range res.Problems {
		c.progress(Event{Operation: "release create", Kind: EventWarning, Code: CodeValidation, Path: p.Component, Message: p.Message})
	}
	c.progress(Event{Operation: "release create", Kind: EventOperationFinished, Path: res.File})
	if err != nil {
		return res, translateCoordError(co.Root, err)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// release inspect / list
// ---------------------------------------------------------------------------

// ReleaseInspectRequest explains one release.
type ReleaseInspectRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// CompareTo adds the change set against another release.
	CompareTo string `json:"compareTo,omitempty"`
}

// ReleaseInspectResult is one release explained from its record alone.
type ReleaseInspectResult struct {
	Name                string                 `json:"name"`
	File                string                 `json:"file"`
	CreatedAt           string                 `json:"createdAt"`
	CreatedBy           string                 `json:"createdBy,omitempty"`
	StackID             string                 `json:"stackID"`
	Digest              string                 `json:"digest"`
	SourceSetDigest     string                 `json:"sourceSetDigest"`
	VariantOf           string                 `json:"variantOf,omitempty"`
	MinimumFXVCSVersion string                 `json:"minimumFXVCSVersion,omitempty"`
	Components          []ReleaseComponentInfo `json:"components"`
	Resources           []PlannedResource      `json:"resources"`
	Enabled             int                    `json:"enabled"`
	Disabled            int                    `json:"disabled"`
	SameSourceAs        []string               `json:"sameSourceAs,omitempty"`
	Change              *ReleaseChange         `json:"change,omitempty"`
}

// ReleaseInspect explains a release: what it pins, what it starts, and which
// other releases share its source.
func (c *Client) ReleaseInspect(ctx context.Context, req ReleaseInspectRequest) (*ReleaseInspectResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release inspect")
	if err != nil {
		return nil, err
	}
	out, err := co.ReleaseInspect(ctx, req.Name, req.CompareTo)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &ReleaseInspectResult{
		Name: out.Name, File: out.File, CreatedAt: out.CreatedAt, CreatedBy: out.CreatedBy,
		StackID: out.StackID, Digest: out.Digest, SourceSetDigest: out.SourceSetDigest,
		VariantOf: out.VariantOf, MinimumFXVCSVersion: out.MinimumVersion,
		Components: releaseComponents(out.Components), Resources: plannedResources(out.Resources),
		Enabled: out.Enabled, Disabled: out.Disabled, SameSourceAs: out.SameSourceAs,
	}
	if out.Change != nil {
		res.Change = releaseChange(*out.Change)
	}
	return res, nil
}

// ReleaseListRequest lists releases.
type ReleaseListRequest struct {
	Path string `json:"path"`
}

// ReleaseSummary is one release in a listing.
type ReleaseSummary struct {
	Name            string   `json:"name"`
	CreatedAt       string   `json:"createdAt"`
	CreatedBy       string   `json:"createdBy,omitempty"`
	Components      int      `json:"components"`
	Enabled         int      `json:"enabled"`
	SourceSetDigest string   `json:"sourceSetDigest"`
	VariantOf       string   `json:"variantOf,omitempty"`
	UsedBy          []string `json:"usedBy,omitempty"`
}

// ReleaseListResult is every release with the targets that select it.
type ReleaseListResult struct {
	Releases []ReleaseSummary `json:"releases"`
}

// ReleaseList lists every release in the coordination repository.
func (c *Client) ReleaseList(ctx context.Context, req ReleaseListRequest) (*ReleaseListResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release list")
	if err != nil {
		return nil, err
	}
	rows, err := co.ReleaseList(ctx)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &ReleaseListResult{Releases: make([]ReleaseSummary, 0, len(rows))}
	for _, r := range rows {
		res.Releases = append(res.Releases, ReleaseSummary(r))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// release notes / export / import
// ---------------------------------------------------------------------------

// ReleaseNotesRequest drafts notes for one release.
type ReleaseNotesRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Since is the base release (default: the previous one by name).
	Since string `json:"since,omitempty"`
}

// ReleaseNotesResult is the draft and the change set behind it.
type ReleaseNotesResult struct {
	Release  string        `json:"release"`
	Since    string        `json:"since,omitempty"`
	Markdown string        `json:"markdown"`
	Change   ReleaseChange `json:"change"`
}

// ReleaseNotes drafts Markdown notes from the change set between two
// releases. The draft is a separate artifact: editing it can never change the
// release it describes.
func (c *Client) ReleaseNotes(ctx context.Context, req ReleaseNotesRequest) (*ReleaseNotesResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release notes generate")
	if err != nil {
		return nil, err
	}
	out, err := co.ReleaseNotes(ctx, req.Name, req.Since)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	return &ReleaseNotesResult{Release: out.Release, Since: out.Since, Markdown: out.Markdown, Change: *releaseChange(out.Change)}, nil
}

// ReleaseExportRequest exports one release as a portable document.
type ReleaseExportRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// ReleaseExportResult carries the portable document.
type ReleaseExportResult struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
	// Document is the portable YAML. The caller decides where it goes.
	Document []byte `json:"-"`
}

// ReleaseExport returns one release as a portable YAML document. Nothing about
// targets, environments, credentials, or host paths travels with it.
func (c *Client) ReleaseExport(ctx context.Context, req ReleaseExportRequest) (*ReleaseExportResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release export")
	if err != nil {
		return nil, err
	}
	out, err := co.ReleaseExport(ctx, req.Name)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	return &ReleaseExportResult{Name: out.Name, Digest: out.Digest, Bytes: out.Bytes, Document: out.Document}, nil
}

// ReleaseImportRequest imports a portable release document.
type ReleaseImportRequest struct {
	Path string `json:"path"`
	// Document is the portable YAML to import.
	Document []byte `json:"-"`
}

// ReleaseImportResult reports the import.
type ReleaseImportResult struct {
	Name     string   `json:"name"`
	File     string   `json:"file"`
	Digest   string   `json:"digest"`
	Written  bool     `json:"written"`
	Warnings []string `json:"warnings,omitempty"`
}

// ReleaseImport validates and stores a portable release document. An existing
// release with the same name and different content is refused: a release name
// is permanently bound to its contents.
func (c *Client) ReleaseImport(ctx context.Context, req ReleaseImportRequest) (*ReleaseImportResult, error) {
	co, err := c.openCoord(ctx, req.Path, "release import")
	if err != nil {
		return nil, err
	}
	out, err := co.ReleaseImport(ctx, req.Document)
	if out == nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &ReleaseImportResult{
		Name: out.Release.Metadata.Name, File: out.Path, Digest: out.Digest,
		Written: out.Written, Warnings: out.Warnings,
	}
	for _, w := range res.Warnings {
		c.progress(Event{Operation: "release import", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	if err != nil {
		return res, translateCoordError(co.Root, err)
	}
	return res, nil
}
