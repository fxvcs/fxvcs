package sdk

import (
	"context"
	"errors"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/stack"
)

// Coordination operations act on a GitOps coordination repository: the
// stack composition, its releases, environments, and targets. They write
// reviewable YAML into the working tree and never run git commit, git push, or
// git tag — except `ReleaseCreate` with TagComponents, which is an explicit,
// plan-first request to create durable component tags.

// ---------------------------------------------------------------------------
// shared types
// ---------------------------------------------------------------------------

// CompositionProblem is one composition or activation-plan finding.
type CompositionProblem struct {
	Component string `json:"component,omitempty"`
	Field     string `json:"field"`
	Message   string `json:"message"`
}

func compositionProblems(in []stack.Problem) []CompositionProblem {
	out := make([]CompositionProblem, 0, len(in))
	for _, p := range in {
		out = append(out, CompositionProblem{Component: p.Component, Field: p.Field, Message: p.Message})
	}
	return out
}

// ComponentDescriptor is a component's accepted source root and layout.
type ComponentDescriptor struct {
	SourceRoot   string `json:"sourceRoot"`
	Layout       string `json:"layout"`
	ResourceName string `json:"resourceName,omitempty"`
}

func componentDescriptorOf(d domain.ComponentDescriptor) ComponentDescriptor {
	out := ComponentDescriptor{SourceRoot: d.SourceRoot, Layout: d.Layout}
	if d.ResourceName != nil {
		out.ResourceName = *d.ResourceName
	}
	return out
}

// ComponentStatus is one registered component's local state.
type ComponentStatus struct {
	RepositoryID   string `json:"repositoryID"`
	Name           string `json:"name,omitempty"`
	Remote         string `json:"remote"`
	Ref            string `json:"ref,omitempty"`
	Mount          string `json:"mount"`
	Required       bool   `json:"required"`
	Fetched        bool   `json:"fetched"`
	Commit         string `json:"commit,omitempty"`
	Resources      int    `json:"resources,omitempty"`
	ReleasedCommit string `json:"releasedCommit,omitempty"`
	Drifted        bool   `json:"drifted,omitempty"`
	Error          string `json:"error,omitempty"`
}

func componentStatuses(in []app.ComponentStatus) []ComponentStatus {
	out := make([]ComponentStatus, 0, len(in))
	for _, c := range in {
		out = append(out, ComponentStatus{
			RepositoryID: c.RepositoryID, Name: c.Name, Remote: c.Remote, Ref: c.Ref, Mount: c.Mount,
			Required: c.Required, Fetched: c.Fetched, Commit: c.Commit, Resources: c.Resources,
			ReleasedCommit: c.ReleasedCommit, Drifted: c.Drifted, Error: c.Error,
		})
	}
	return out
}

// PlannedResource is one entry of a resolved activation plan.
type PlannedResource struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	RepositoryName   string `json:"repositoryName,omitempty"`
	Path             string `json:"path,omitempty"`
	ResourceID       string `json:"resourceID,omitempty"`
	Activation       string `json:"activation"`
	ActivationSource string `json:"activationSource"`
	EnsureOrder      int    `json:"ensureOrder"`
	Mount            string `json:"mount,omitempty"`
}

func plannedResources(in []app.PlannedResource) []PlannedResource {
	out := make([]PlannedResource, 0, len(in))
	for _, r := range in {
		out = append(out, PlannedResource(r))
	}
	return out
}

// openCoord opens the coordination repository containing path.
func (c *Client) openCoord(ctx context.Context, path, operation string) (*app.Coord, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	co, err := app.OpenCoord(ctx, path, app.Options{
		Installed:     c.installed,
		Operation:     operation,
		GitExecutable: c.opts.GitExecutable,
		ConfigDir:     c.opts.ConfigDir,
		CacheDir:      c.opts.CacheDir,
	})
	if err != nil {
		return nil, translateCoordError(path, err)
	}
	return co, nil
}

// translateCoordError maps coordination errors to typed SDK errors.
func translateCoordError(root string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	var ce *stack.CompositionError
	if errors.As(err, &ce) {
		e := WrapError(CodeValidation, err.Error(), err)
		issues := make([]ValidationIssue, 0, len(ce.Problems))
		for _, p := range ce.Problems {
			issues = append(issues, ValidationIssue{Path: p.Component, Message: p.Field + ": " + p.Message})
		}
		e.Details = issues
		return e
	}
	var de *gitops.DocumentError
	if errors.As(err, &de) {
		return WrapError(CodeValidation, err.Error(), err)
	}
	switch {
	case errors.Is(err, gitops.ErrNotStack):
		return WrapError(CodeNotRepository, err.Error(), err).
			WithHint("run this inside your coordination repository, or set one up with: fxvcs stack init")
	case errors.Is(err, controlplane.ErrNotFound):
		return WrapError(CodeNotFound, err.Error(), err)
	case errors.Is(err, controlplane.ErrExists):
		return WrapError(CodeConflict, err.Error(), err)
	case errors.Is(err, controlplane.ErrImmutable):
		return WrapError(CodeConflict, err.Error(), err).
			WithHint("pick a new release name; a release name is permanently bound to its contents")
	case errors.Is(err, controlplane.ErrInvalid), errors.Is(err, app.ErrStack):
		return WrapError(CodeInvalidArgument, err.Error(), err)
	}
	return translateFivemError(root, err)
}

// ---------------------------------------------------------------------------
// stack init / add / rescan
// ---------------------------------------------------------------------------

// StackInitRequest creates or adopts a coordination repository.
type StackInitRequest struct {
	Path    string `json:"path"`
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	Adopt   bool   `json:"adopt,omitempty"`
}

// StackInitResult reports what init wrote.
type StackInitResult struct {
	Root    string `json:"root"`
	StackID string `json:"stackID"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	File    string `json:"file"`
	Adopted bool   `json:"adopted"`
}

// StackInit creates or adopts the coordination repository. It writes one
// file and never commits.
func (c *Client) StackInit(ctx context.Context, req StackInitRequest) (*StackInitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	out, err := app.StackInit(ctx, app.StackInitInput{
		Path: req.Path, Name: req.Name, Profile: req.Profile, Adopt: req.Adopt,
	}, app.Options{Installed: c.installed, Operation: "stack init", GitExecutable: c.opts.GitExecutable})
	if err != nil {
		return nil, translateCoordError(req.Path, err)
	}
	res := StackInitResult(*out)
	return &res, nil
}

// StackAddRequest registers one component repository.
type StackAddRequest struct {
	Path     string `json:"path"`
	Remote   string `json:"remote"`
	Ref      string `json:"ref,omitempty"`
	Mount    string `json:"mount,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// ComponentProposal is a detected component and its proposed placement.
type ComponentProposal struct {
	RepositoryID   string               `json:"repositoryID"`
	Name           string               `json:"name"`
	Remote         string               `json:"remote"`
	Ref            string               `json:"ref,omitempty"`
	Commit         string               `json:"commit"`
	Profile        string               `json:"profile"`
	Descriptor     ComponentDescriptor  `json:"descriptor"`
	DescriptorFrom string               `json:"descriptorFrom"`
	Detected       *ComponentDescriptor `json:"detected,omitempty"`
	Mount          string               `json:"mount"`
	Alias          string               `json:"alias,omitempty"`
	Required       bool                 `json:"required"`
	Resources      int                  `json:"resources"`
}

// StackAddResult reports the registration.
type StackAddResult struct {
	Component ComponentProposal `json:"component"`
	File      string            `json:"file"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// StackAdd fetches a component repository, reads its committed identity
// and layout, proposes a mount, and records it in fxvcs.yaml.
func (c *Client) StackAdd(ctx context.Context, req StackAddRequest) (*StackAddResult, error) {
	co, err := c.openCoord(ctx, req.Path, "stack add")
	if err != nil {
		return nil, err
	}
	c.progress(Event{Operation: "stack add", Kind: EventOperationStarted, Message: req.Remote})
	out, err := co.StackAdd(ctx, app.StackAddInput{
		Remote: req.Remote, Ref: req.Ref, Mount: req.Mount, Alias: req.Alias, Optional: req.Optional,
	})
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "stack add", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	res := &StackAddResult{File: out.File, Warnings: out.Warnings, Component: componentProposal(out.Component)}
	c.progress(Event{Operation: "stack add", Kind: EventOperationFinished, Path: out.File})
	return res, nil
}

func componentProposal(p app.ComponentProposal) ComponentProposal {
	out := ComponentProposal{
		RepositoryID: p.RepositoryID, Name: p.Name, Remote: p.Remote, Ref: p.Ref, Commit: p.Commit,
		Profile: p.Profile, Descriptor: componentDescriptorOf(p.Descriptor), DescriptorFrom: p.DescriptorFrom,
		Mount: p.Mount, Alias: p.Alias, Required: p.Required, Resources: p.Resources,
	}
	if p.Detected != nil {
		d := componentDescriptorOf(*p.Detected)
		out.Detected = &d
	}
	return out
}

// StackRescanRequest re-detects component layouts.
type StackRescanRequest struct {
	Path string `json:"path"`
	// Components limits the rescan to these repository IDs.
	Components []string `json:"components,omitempty"`
	// Accept records the detected layout and proposed mount instead of only
	// reporting them.
	Accept bool `json:"accept,omitempty"`
}

// RescanFinding is one component's layout review.
type RescanFinding struct {
	RepositoryID  string               `json:"repositoryID"`
	Name          string               `json:"name"`
	Commit        string               `json:"commit"`
	Accepted      ComponentDescriptor  `json:"accepted"`
	AcceptedFrom  string               `json:"acceptedFrom"`
	Detected      *ComponentDescriptor `json:"detected,omitempty"`
	Mount         string               `json:"mount"`
	ProposedMount string               `json:"proposedMount,omitempty"`
	Agrees        bool                 `json:"agrees"`
	Notes         []string             `json:"notes,omitempty"`
	Updated       bool                 `json:"updated,omitempty"`
}

// StackRescanResult reports the review.
type StackRescanResult struct {
	Findings []RescanFinding `json:"findings"`
	Changed  bool            `json:"changed"`
	File     string          `json:"file,omitempty"`
}

// StackRescan re-detects each component's layout and reports disagreement
// with the recorded mount and descriptor. It changes nothing unless Accept is
// set.
func (c *Client) StackRescan(ctx context.Context, req StackRescanRequest) (*StackRescanResult, error) {
	co, err := c.openCoord(ctx, req.Path, "stack rescan")
	if err != nil {
		return nil, err
	}
	out, err := co.StackRescan(ctx, req.Components, req.Accept)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &StackRescanResult{Changed: out.Changed, File: out.File, Findings: make([]RescanFinding, 0, len(out.Findings))}
	for _, f := range out.Findings {
		row := RescanFinding{
			RepositoryID: f.RepositoryID, Name: f.Name, Commit: f.Commit, Accepted: componentDescriptorOf(f.Accepted),
			AcceptedFrom: f.AcceptedFrom, Mount: f.Mount, ProposedMount: f.ProposedMount,
			Agrees: f.Agrees, Notes: f.Notes, Updated: f.Updated,
		}
		if f.Detected != nil {
			d := componentDescriptorOf(*f.Detected)
			row.Detected = &d
		}
		if !f.Agrees {
			for _, n := range f.Notes {
				c.progress(Event{Operation: "stack rescan", Kind: EventWarning, Code: CodeValidation, Path: f.Name, Message: n})
			}
		}
		res.Findings = append(res.Findings, row)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// stack sync / status / validate
// ---------------------------------------------------------------------------

// StackSyncRequest fetches component repositories.
type StackSyncRequest struct {
	Path       string   `json:"path"`
	Components []string `json:"components,omitempty"`
}

// ComponentSync is one component's fetch outcome.
type ComponentSync struct {
	RepositoryID string `json:"repositoryID"`
	Remote       string `json:"remote"`
	Ref          string `json:"ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Previous     string `json:"previous,omitempty"`
	Updated      bool   `json:"updated"`
	Error        string `json:"error,omitempty"`
}

// StackSyncResult reports every fetch.
type StackSyncResult struct {
	Components []ComponentSync `json:"components"`
	Fetched    int             `json:"fetched"`
	Updated    int             `json:"updated"`
	Failed     int             `json:"failed"`
}

// StackSync fetches every component repository into its local mirror.
func (c *Client) StackSync(ctx context.Context, req StackSyncRequest) (*StackSyncResult, error) {
	co, err := c.openCoord(ctx, req.Path, "stack sync")
	if err != nil {
		return nil, err
	}
	c.progress(Event{Operation: "stack sync", Kind: EventOperationStarted})
	out, err := co.StackSync(ctx, req.Components, func(row app.ComponentSync) {
		ev := Event{Operation: "stack sync", Kind: EventItemFinished, Path: row.RepositoryID}
		if row.Error != "" {
			ev.Kind, ev.Code, ev.Message = EventWarning, CodeGit, row.Error
		} else if row.Updated {
			ev.Message = "updated"
		}
		c.progress(ev)
	})
	if out == nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &StackSyncResult{Fetched: out.Fetched, Updated: out.Updated, Failed: out.Failed,
		Components: make([]ComponentSync, 0, len(out.Components))}
	for _, row := range out.Components {
		res.Components = append(res.Components, ComponentSync(row))
	}
	c.progress(Event{Operation: "stack sync", Kind: EventOperationFinished})
	if err != nil {
		return res, translateCoordError(co.Root, err)
	}
	return res, nil
}

// StackStatusRequest reads local composition state.
type StackStatusRequest struct {
	Path string `json:"path"`
}

// StackStatusResult is the composition at a glance.
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

// StackStatus reads local state only; it never uses the network.
func (c *Client) StackStatus(ctx context.Context, req StackStatusRequest) (*StackStatusResult, error) {
	co, err := c.openCoord(ctx, req.Path, "stack status")
	if err != nil {
		return nil, err
	}
	out, err := co.StackStatus(ctx)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	return &StackStatusResult{
		StackID: out.StackID, Name: out.Name, Profile: out.Profile, Root: out.Root,
		Components: componentStatuses(out.Components), Releases: out.Releases, LatestRelease: out.LatestRelease,
		Environments: out.Environments, Targets: out.Targets,
	}, nil
}

// StackValidateRequest checks the whole composition.
type StackValidateRequest struct {
	Path string `json:"path"`
}

// StackValidateResult is the composition report and resolved plan.
type StackValidateResult struct {
	Valid      bool                 `json:"valid"`
	Components []ComponentStatus    `json:"components"`
	Resources  []PlannedResource    `json:"resources"`
	Enabled    int                  `json:"enabled"`
	Disabled   int                  `json:"disabled"`
	Problems   []CompositionProblem `json:"problems"`
	Warnings   []string             `json:"warnings,omitempty"`
}

// StackValidate resolves the composition from the fetched component refs
// and reports the plan a release would snapshot. It writes nothing. When the
// composition is not valid the result lists every problem and the error has
// CodeValidation.
func (c *Client) StackValidate(ctx context.Context, req StackValidateRequest) (*StackValidateResult, error) {
	co, err := c.openCoord(ctx, req.Path, "stack validate")
	if err != nil {
		return nil, err
	}
	out, err := co.StackValidate(ctx)
	if out == nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &StackValidateResult{
		Valid: out.Valid, Components: componentStatuses(out.Components), Resources: plannedResources(out.Resources),
		Enabled: out.Enabled, Disabled: out.Disabled, Problems: compositionProblems(out.Problems), Warnings: out.Warnings,
	}
	for _, p := range res.Problems {
		c.progress(Event{Operation: "stack validate", Kind: EventWarning, Code: CodeValidation, Path: p.Component, Message: p.Message})
	}
	if err != nil {
		return res, translateCoordError(co.Root, err)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// runtime-resources sync
// ---------------------------------------------------------------------------

// RuntimeResourcesSyncRequest seeds coordination-owned runtime/external
// resource candidates from an existing server configuration.
type RuntimeResourcesSyncRequest struct {
	Path    string `json:"path"`
	FromCfg string `json:"fromCfg"`
}

// RuntimeResourcesSyncResult reports the proposal.
type RuntimeResourcesSyncResult struct {
	File      string   `json:"file"`
	Proposed  []string `json:"proposed"`
	Matched   []string `json:"matched"`
	Existing  []string `json:"existing"`
	Files     []string `json:"files,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	Written   bool     `json:"written"`
	Resources int      `json:"resources"`
}

// RuntimeResourcesSync proposes runtime/external entries from a server.cfg.
// Every proposal is pending: the source kind of a resource that is only
// mentioned in a cfg is genuinely unknown. It never commits.
func (c *Client) RuntimeResourcesSync(ctx context.Context, req RuntimeResourcesSyncRequest) (*RuntimeResourcesSyncResult, error) {
	co, err := c.openCoord(ctx, req.Path, "runtime-resources sync")
	if err != nil {
		return nil, err
	}
	out, err := co.RuntimeResourcesSync(ctx, app.RuntimeResourcesSyncInput{FromCfg: req.FromCfg})
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "runtime-resources sync", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	res := RuntimeResourcesSyncResult(*out)
	return &res, nil
}
