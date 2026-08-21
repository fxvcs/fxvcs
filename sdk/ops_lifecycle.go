package sdk

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// ---- init / clone / setup / upgrade ----------------------------------------

type InitRequest struct {
	Path            string `json:"path"`
	Name            string `json:"name"`
	Profile         string `json:"profile"` // generic | fivem
	StorageDomainID string `json:"storageDomainID,omitempty"`
	// FiveM descriptor overrides; empty runs discovery.
	SourceRoot   string `json:"sourceRoot,omitempty"`
	Layout       string `json:"layout,omitempty"`
	ResourceName string `json:"resourceName,omitempty"`
	// RemoteName/RemotePath add one committed filesystem remote and bind it.
	RemoteName string `json:"remoteName,omitempty"`
	RemotePath string `json:"remotePath,omitempty"`
	// LauncherPath is passed to setup.
	LauncherPath string `json:"launcherPath,omitempty"`
}

// FiveMDescriptor is the accepted layout written to repository.yaml.
type FiveMDescriptor struct {
	SourceRoot           string   `json:"sourceRoot"`
	Layout               string   `json:"layout"`
	ResourceName         string   `json:"resourceName,omitempty"`
	ResourceNameProposed bool     `json:"resourceNameProposed,omitempty"`
	Resources            []string `json:"resources"`
	// Ignored lists manifest directories FXServer would not load from this
	// source root (behind a plain directory, or inside a [bracket] category).
	// They are not part of the component; they are reported so the operator
	// can see what will never deploy.
	Ignored []string `json:"ignored,omitempty"`
}

// FiveMCandidate is one plausible layout reported when discovery is ambiguous.
type FiveMCandidate struct {
	SourceRoot   string   `json:"sourceRoot"`
	Layout       string   `json:"layout"`
	ResourceName string   `json:"resourceName,omitempty"`
	Resources    []string `json:"resources"`
}

// DiscoveryFailure is the Details payload of a CodeValidation error from
// init --profile fivem: the fail-closed reason and the candidates to choose
// from with --source-root/--layout/--resource-name.
type DiscoveryFailure struct {
	Reason     string           `json:"reason"`
	Path       string           `json:"path,omitempty"`
	Detail     string           `json:"detail,omitempty"`
	Candidates []FiveMCandidate `json:"candidates,omitempty"`
}

type InitResult struct {
	RepositoryRoot  string `json:"repositoryRoot"`
	RepositoryID    string `json:"repositoryID"`
	StorageDomainID string `json:"storageDomainID"`
	FilesWritten    []string
	Name            string           `json:"name,omitempty"`
	Profile         string           `json:"profile,omitempty"`
	Descriptor      *FiveMDescriptor `json:"descriptor,omitempty"`
	Setup           *SetupResult     `json:"setup,omitempty"`
	Track           *TrackResult     `json:"track,omitempty"`
}

// Init writes .fxvcs/repository.yaml into the Git working tree containing
// Path, then runs setup and, for the fivem profile, tracks the binary
// presets. It never commits.
func (c *Client) Init(ctx context.Context, req InitRequest) (*InitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	c.progress(Event{Operation: "init", Kind: EventOperationStarted, Path: req.Path})
	res, err := app.Init(ctx, app.InitInput{
		Path: req.Path, Name: req.Name, Profile: req.Profile, StorageDomainID: req.StorageDomainID,
		SourceRoot: req.SourceRoot, Layout: req.Layout, ResourceName: req.ResourceName,
		RemoteName: req.RemoteName, RemotePath: req.RemotePath, LauncherPath: req.LauncherPath,
	}, c.appOptions("init"))
	if err != nil {
		return nil, translateLifecycleError(res.Root, err)
	}
	out := &InitResult{RepositoryRoot: res.Root, RepositoryID: res.RepositoryID, StorageDomainID: res.StorageDomainID, FilesWritten: res.FilesWritten, Name: res.Name, Profile: res.Profile}
	if res.Detection != nil {
		out.Descriptor = descriptorOf(*res.Detection)
	}
	s := setupResultOf(res.Root, res.Setup)
	out.Setup = &s
	if res.Track != nil {
		out.Track = &TrackResult{RulesAdded: res.Track.RulesAdded, RulesPresent: res.Track.RulesPresent}
	}
	c.progress(Event{Operation: "init", Kind: EventOperationFinished, Path: res.Root})
	return out, nil
}

func descriptorOf(d discovery.Detection) *FiveMDescriptor {
	out := &FiveMDescriptor{SourceRoot: d.Descriptor.SourceRoot, Layout: d.Descriptor.Layout, ResourceNameProposed: d.ResourceNameProposed, Resources: append([]string{}, d.Resources...)}
	for _, sk := range d.Skipped {
		out.Ignored = append(out.Ignored, sk.Path)
	}
	if d.Descriptor.ResourceName != nil {
		out.ResourceName = *d.Descriptor.ResourceName
	}
	return out
}

func (c *Client) appOptions(operation string) app.Options {
	return app.Options{Installed: c.installed, Operation: operation, GitExecutable: c.opts.GitExecutable, CacheDir: c.opts.CacheDir, LauncherSourcePath: c.opts.LauncherSourcePath, LockTimeout: c.opts.LockTimeout}
}

// translateLifecycleError maps lifecycle sentinels before the generic table.
// testLauncherHint names the supported way to run a downstream test suite
// against a real repository: the test binary serves as its own driver.
const testLauncherHint = "under `go test`, delegate Git driver invocations from TestMain: see fxvcs.dev/fxvcs/sdk/driver (driver.RunFromEnv), and keep LauncherPath inside a temp directory"

func translateLifecycleError(root string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	var de *discovery.Error
	switch {
	case errors.Is(err, app.ErrDiscovery) && errors.As(err, &de):
		e := WrapError(CodeValidation, err.Error(), err)
		d := &DiscoveryFailure{Reason: string(de.Reason), Path: de.Path, Detail: de.Detail}
		if de.Err != nil {
			d.Detail = strings.TrimSpace(d.Detail + " " + de.Err.Error())
		}
		for _, cand := range de.Candidates {
			d.Candidates = append(d.Candidates, FiveMCandidate{SourceRoot: cand.SourceRoot, Layout: cand.Layout, ResourceName: cand.ResourceName, Resources: append([]string{}, cand.Resources...)})
		}
		e.Details = d
		return e
	case errors.Is(err, app.ErrAlreadyInitialized):
		return WrapError(CodeConflict, err.Error(), err).WithHint("fxvcs setup   (to configure this clone) or fxvcs status")
	case errors.Is(err, app.ErrUpgradeConflict):
		return WrapError(CodeConflict, err.Error(), err).WithHint("fxvcs upgrade   (review the plan), then: fxvcs upgrade --apply")
	case errors.Is(err, app.ErrLauncherVersionConflict):
		return WrapError(CodeConflict, err.Error(), err).WithHint("install a newer fxvcs build, or retry with --repair-launcher only when this build is trusted")
	case errors.Is(err, app.ErrDirty):
		return WrapError(CodeConflict, err.Error(), err).WithHint("commit or stash the change (git add/commit or git stash), then run the command again")
	case errors.Is(err, app.ErrRuleCovered):
		return WrapError(CodeConflict, err.Error(), err).WithHint("fxvcs tracking check <path>   (shows which rule already covers it)")
	case errors.Is(err, app.ErrNotTracked):
		return WrapError(CodeInvalidArgument, err.Error(), err).WithHint("fxvcs track <path>   (or a pattern: fxvcs track --glob '*.ext')")
	case errors.Is(err, app.ErrSetupRequired):
		return WrapError(CodeInvalidArgument, err.Error(), err).WithHint("fxvcs setup")
	case errors.Is(err, app.ErrTestBinaryLauncher):
		// Keep the translated code; only name the supported pattern, so the
		// natural response is to wire up delegation rather than to skip the
		// test or defeat the guard.
		e := translateError(root, err)
		var le *Error
		if errors.As(e, &le) && le.Hint == "" {
			le.Hint = testLauncherHint
		}
		return e
	case errors.Is(err, app.ErrStagedMismatch):
		return WrapError(CodeIntegrity, err.Error(), err)
	case errors.Is(err, pathsafe.ErrEmpty), errors.Is(err, pathsafe.ErrBadSegment), errors.Is(err, pathsafe.ErrBadCharacter), errors.Is(err, pathsafe.ErrReservedName), errors.Is(err, pathsafe.ErrNotNormalized):
		return WrapError(CodeInvalidArgument, err.Error(), err)
	}
	return translateError(root, err)
}

type SetupRequest struct {
	Path string `json:"path"`
	// LauncherPath is the absolute stable driver launcher to install; empty
	// selects the per-user default location.
	LauncherPath string `json:"launcherPath,omitempty"`
	// RepairLauncher explicitly trusts this client's candidate when the
	// installed launcher reports the same version but has different bytes.
	RepairLauncher bool `json:"repairLauncher,omitempty"`
}

type SetupResult struct {
	RepositoryRoot        string `json:"repositoryRoot"`
	LauncherPath          string `json:"launcherPath"`
	GitIntegrationVersion int    `json:"gitIntegrationVersion"`
	// LauncherInstalled reports that the trusted candidate was installed or
	// updated at LauncherPath.
	LauncherInstalled     bool            `json:"launcherInstalled,omitempty"`
	LauncherOutcome       LauncherOutcome `json:"launcherOutcome,omitempty"`
	LegacyLauncherPath    string          `json:"legacyLauncherPath,omitempty"`
	LegacyLauncherOutcome LauncherOutcome `json:"legacyLauncherOutcome,omitempty"`
	ConfigKeys            []string        `json:"configKeys,omitempty"`
	HookPath              string          `json:"hookPath,omitempty"`
	HookInstalled         bool            `json:"hookInstalled,omitempty"`
	// HookForeign reports a pre-push hook not managed by fxvcs; it was left
	// untouched and HookInstructions explains how to chain it.
	HookForeign      bool   `json:"hookForeign,omitempty"`
	HookInstructions string `json:"hookInstructions,omitempty"`
	StateCreated     bool   `json:"stateCreated,omitempty"`
	LedgerCreated    bool   `json:"ledgerCreated,omitempty"`
}

func setupResultOf(root string, s app.SetupResult) SetupResult {
	return SetupResult{RepositoryRoot: root, LauncherPath: s.LauncherPath, GitIntegrationVersion: SupportedFormats().GitIntegrationGeneration,
		LauncherInstalled: s.LauncherInstalled, LauncherOutcome: LauncherOutcome(s.LauncherOutcome), LegacyLauncherPath: s.LegacyLauncherPath, LegacyLauncherOutcome: LauncherOutcome(s.LegacyLauncherOutcome), ConfigKeys: s.ConfigKeys, HookPath: s.HookPath, HookInstalled: s.HookInstalled,
		HookForeign: s.HookForeign, HookInstructions: s.HookInstructions, StateCreated: s.StateCreated, LedgerCreated: s.LedgerCreated}
}

// Setup configures an existing clone: launcher binary, repository-local
// driver configuration, managed pre-push hook, local config, and the local
// databases. Idempotent; never commits.
func (c *Client) Setup(ctx context.Context, req SetupRequest) (*SetupResult, error) {
	r, err := c.openRepo(ctx, req.Path, "setup")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	s, err := r.Setup(ctx, app.SetupInput{LauncherPath: req.LauncherPath, RepairLauncher: req.RepairLauncher})
	if err != nil {
		return nil, translateLifecycleError(r.Root, err)
	}
	if s.HookForeign {
		c.progress(Event{Operation: "setup", Kind: EventWarning, Message: s.HookInstructions})
	}
	res := setupResultOf(r.Root, s)
	return &res, nil
}

type CloneRequest struct {
	URL string `json:"url"`
	// Directory is the destination (absolute, or relative to Cwd); empty
	// derives it from the URL.
	Directory string `json:"directory,omitempty"`
	// Cwd resolves a relative Directory ("" = process working directory).
	Cwd string `json:"cwd,omitempty"`
	// Hydrate runs the hydrate operation over the whole checkout after setup.
	// The default is an unhydrated checkout (pointer bytes), which is always
	// safe and needs no object access.
	Hydrate bool `json:"hydrate,omitempty"`
	// Blobless clones with --filter=blob:none.
	Blobless     bool   `json:"blobless,omitempty"`
	LauncherPath string `json:"launcherPath,omitempty"`
}

type CloneResult struct {
	RepositoryRoot string         `json:"repositoryRoot"`
	Setup          *SetupResult   `json:"setup,omitempty"`
	Hydrate        *HydrateResult `json:"hydrate,omitempty"`
}

// Clone runs git clone, then setup, then optionally hydrate.
func (c *Client) Clone(ctx context.Context, req CloneRequest) (*CloneResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	c.progress(Event{Operation: "clone", Kind: EventOperationStarted, Message: req.URL})
	res, err := app.Clone(ctx, app.CloneInput{URL: req.URL, Directory: req.Directory, Cwd: req.Cwd, Blobless: req.Blobless, LauncherPath: req.LauncherPath}, c.appOptions("clone"))
	if err != nil {
		return nil, translateLifecycleError(res.Root, err)
	}
	s := setupResultOf(res.Root, res.Setup)
	out := &CloneResult{RepositoryRoot: res.Root, Setup: &s}
	if req.Hydrate {
		h, err := c.Hydrate(ctx, HydrateRequest{Path: res.Root})
		if err != nil {
			return out, err
		}
		out.Hydrate = h
	}
	c.progress(Event{Operation: "clone", Kind: EventOperationFinished, Path: res.Root})
	return out, nil
}

// UpgradeMode selects check (read-only), plan (default), or apply.
type UpgradeMode string

const (
	UpgradeCheck UpgradeMode = "check"
	UpgradePlan  UpgradeMode = "plan"
	UpgradeApply UpgradeMode = "apply"
)

type UpgradeRequest struct {
	Path           string      `json:"path"`
	Mode           UpgradeMode `json:"mode"`
	RepairLauncher bool        `json:"repairLauncher,omitempty"`
}

// UpgradeStep is one planned change; ExpectedBefore protects managed files.
type UpgradeStep struct {
	Kind           string `json:"kind"` // committed-file | git-config | hook | launcher | local-state
	Target         string `json:"target"`
	FromVersion    string `json:"fromVersion"`
	ToVersion      string `json:"toVersion"`
	ExpectedBefore string `json:"expectedBefore,omitempty"` // sha256 of managed content
	Description    string `json:"description"`
}

type UpgradeResult struct {
	RepositoryRoot string        `json:"repositoryRoot"`
	UpToDate       bool          `json:"upToDate"`
	Steps          []UpgradeStep `json:"steps"`
	Applied        bool          `json:"applied"`
	// AppliedSteps lists the steps that completed in apply mode (a prefix of
	// Steps when an error interrupted the run).
	AppliedSteps        []UpgradeStep `json:"appliedSteps,omitempty"`
	Notes               []string      `json:"notes,omitempty"`
	InstalledVersion    string        `json:"installedVersion,omitempty"`
	MinimumFXVCSVersion string        `json:"minimumFXVCSVersion,omitempty"`
	LauncherPath        string        `json:"launcherPath,omitempty"`
}

func upgradeStepsOf(in []app.UpgradeStep) []UpgradeStep {
	out := make([]UpgradeStep, 0, len(in))
	for _, s := range in {
		out = append(out, UpgradeStep(s))
	}
	return out
}

// Upgrade plans (default), checks, or applies the FXVCS-owned upgrades of
// this repository and clone. Apply never commits and stops with CodeConflict
// when managed content differs from the plan.
func (c *Client) Upgrade(ctx context.Context, req UpgradeRequest) (*UpgradeResult, error) {
	r, err := c.openRepo(ctx, req.Path, "upgrade")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	mode := req.Mode
	if mode == "" {
		mode = UpgradePlan
	}
	res, err := r.Upgrade(ctx, string(mode), req.RepairLauncher)
	out := &UpgradeResult{RepositoryRoot: r.Root, UpToDate: res.UpToDate, Steps: upgradeStepsOf(res.Steps), Applied: res.Applied,
		AppliedSteps: upgradeStepsOf(res.AppliedSteps), Notes: res.Notes, InstalledVersion: res.InstalledVersion, MinimumFXVCSVersion: res.MinimumFXVCSVersion, LauncherPath: res.LauncherPath}
	if len(out.AppliedSteps) == 0 {
		out.AppliedSteps = nil
	}
	if err != nil {
		return out, translateLifecycleError(r.Root, err)
	}
	return out, nil
}

// ---- tracking --------------------------------------------------------------

type TrackRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
	// Force allows a glob that would capture known source formats.
	Force bool `json:"force,omitempty"`
}

type TrackResult struct {
	RulesAdded   []string `json:"rulesAdded"`
	RulesPresent []string `json:"rulesPresent"`
}

// repoRelative converts a caller path (absolute or relative to base) into a
// normalized repository-relative path.
func repoRelative(root, base, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(base, p)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", WrapError(CodePathEscape, p+" is outside the repository", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", NewError(CodePathEscape, p+" is outside the repository")
	}
	n, err := pathsafe.Normalize(rel)
	if err != nil {
		return "", translateLifecycleError(root, err)
	}
	return n, nil
}

func (c *Client) trackInput(r *app.Repo, base string, sel PathSelector, force bool) (app.TrackInput, error) {
	in := app.TrackInput{Glob: sel.Glob, Force: force}
	for _, p := range sel.Paths {
		if sel.Glob {
			in.Patterns = append(in.Patterns, p)
			continue
		}
		rel, err := repoRelative(r.Root, base, p)
		if err != nil {
			return in, err
		}
		in.Patterns = append(in.Patterns, rel)
	}
	return in, nil
}

// Track adds literal paths (escaped and anchored) or --glob patterns to the
// FXVCS-managed .gitattributes block. History is never rewritten.
func (c *Client) Track(ctx context.Context, req TrackRequest) (*TrackResult, error) {
	r, err := c.openRepo(ctx, req.Path, "track")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	in, err := c.trackInput(r, req.Path, req.Selector, req.Force)
	if err != nil {
		return nil, err
	}
	res, err := r.Track(ctx, in)
	if err != nil {
		return nil, translateLifecycleError(r.Root, err)
	}
	return &TrackResult{RulesAdded: res.RulesAdded, RulesPresent: res.RulesPresent}, nil
}

type UntrackRequest = TrackRequest

type UntrackResult struct {
	RulesRemoved []string `json:"rulesRemoved"`
	// PathsRequiringHydration lists paths whose current object must be
	// hydrated before they can be staged as ordinary Git blobs.
	PathsRequiringHydration []string `json:"pathsRequiringHydration"`
	RulesMissing            []string `json:"rulesMissing,omitempty"`
}

// Untrack removes rules from the managed block and reports pointer paths
// that now need hydration. Nothing is hydrated.
func (c *Client) Untrack(ctx context.Context, req UntrackRequest) (*UntrackResult, error) {
	r, err := c.openRepo(ctx, req.Path, "untrack")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	in, err := c.trackInput(r, req.Path, req.Selector, true)
	if err != nil {
		return nil, err
	}
	res, err := r.Untrack(ctx, in)
	if err != nil {
		return nil, translateLifecycleError(r.Root, err)
	}
	out := &UntrackResult{RulesRemoved: res.RulesRemoved, PathsRequiringHydration: res.PathsRequiringHydration, RulesMissing: res.RulesMissing}
	if len(out.RulesMissing) == 0 {
		out.RulesMissing = nil
	}
	return out, nil
}

type TrackingCheckRequest struct {
	Path string `json:"path"`
	File string `json:"file"`
}

type TrackingCheckResult struct {
	File          string `json:"file"`
	Tracked       bool   `json:"tracked"`
	MatchedRule   string `json:"matchedRule,omitempty"`
	RuleSource    string `json:"ruleSource,omitempty"` // fxvcs-block | user | other | none
	AttributesRaw string `json:"attributesRaw,omitempty"`
}

// TrackingCheck explains which rule makes a path tracked, using Git's own
// pattern matching.
func (c *Client) TrackingCheck(ctx context.Context, req TrackingCheckRequest) (*TrackingCheckResult, error) {
	r, err := c.openRepo(ctx, req.Path, "tracking check")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if req.File == "" {
		return nil, NewError(CodeInvalidArgument, "tracking check requires a path").WithHint("fxvcs tracking check <path>")
	}
	rel, err := repoRelative(r.Root, req.Path, req.File)
	if err != nil {
		return nil, err
	}
	res, err := r.TrackingCheck(ctx, rel)
	if err != nil {
		return nil, translateLifecycleError(r.Root, err)
	}
	return &TrackingCheckResult{File: res.File, Tracked: res.Tracked, MatchedRule: res.MatchedRule, RuleSource: res.RuleSource, AttributesRaw: res.AttributesRaw}, nil
}

type ScanRequest struct {
	Path         string `json:"path"`
	MinimumBytes int64  `json:"minimumBytes"`
}

type ScanCandidate struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	Tracked bool   `json:"tracked"`
}

type ScanResult struct {
	Candidates []ScanCandidate `json:"candidates"`
}

// Scan lists large tracked and untracked (not ignored) files as tracking
// candidates. Read-only.
func (c *Client) Scan(ctx context.Context, req ScanRequest) (*ScanResult, error) {
	r, err := c.openRepo(ctx, req.Path, "scan")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cands, err := r.Scan(ctx, req.MinimumBytes)
	if err != nil {
		return nil, translateLifecycleError(r.Root, err)
	}
	res := &ScanResult{Candidates: make([]ScanCandidate, 0, len(cands))}
	for _, cd := range cands {
		res.Candidates = append(res.Candidates, ScanCandidate{Path: cd.Path, Bytes: cd.Bytes, Tracked: cd.Tracked})
	}
	return res, nil
}

type MigrateRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
}

type MigrateResult struct {
	Items []ItemOutcome `json:"items"`
}

// Migrate converts selected ordinary index blobs under managed rules into
// pointers (ingest + git add) without rewriting history.
func (c *Client) Migrate(ctx context.Context, req MigrateRequest) (*MigrateResult, error) {
	r, err := c.openRepo(ctx, req.Path, "migrate")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	in := app.MigrateInput{Glob: req.Selector.Glob}
	for _, p := range req.Selector.Paths {
		if req.Selector.Glob {
			in.Paths = append(in.Paths, p)
			continue
		}
		rel, err := repoRelative(r.Root, req.Path, p)
		if err != nil {
			return nil, err
		}
		in.Paths = append(in.Paths, rel)
	}
	c.progress(Event{Operation: "migrate", Kind: EventOperationStarted})
	in.Progress = c.itemProgress("migrate")
	items, err := r.Migrate(ctx, in)
	res := &MigrateResult{Items: make([]ItemOutcome, 0, len(items))}
	for _, it := range items {
		res.Items = append(res.Items, ItemOutcome{Path: it.Path, Digest: it.Digest, Bytes: it.Bytes, Skipped: it.Skipped, Reason: it.Reason})
		if it.Skipped {
			c.progress(Event{Operation: "migrate", Kind: EventItemSkipped, Path: it.Path, Digest: it.Digest, Bytes: it.Bytes, Message: it.Reason})
		}
	}
	c.progress(Event{Operation: "migrate", Kind: EventOperationFinished})
	if err != nil {
		return res, translateLifecycleError(r.Root, err)
	}
	return res, nil
}
