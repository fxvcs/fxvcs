package sdk

import (
	"context"
	"errors"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/catalog"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/profile/fivem/manifest"
	"fxvcs.dev/fxvcs/internal/schema"
)

// ---- FiveM profile ---------------------------------------------------------

// CatalogResource is one entry of .fxvcs/resources.yaml as seen through the SDK.
type CatalogResource struct {
	Path                string            `json:"path"`
	Name                string            `json:"name"`
	Activation          string            `json:"activation"`
	After               []string          `json:"after"`
	Before              []string          `json:"before"`
	Provides            []string          `json:"provides"`
	Dependencies        []string          `json:"dependencies"`
	Games               []string          `json:"games"`
	ServerOnly          bool              `json:"serverOnly"`
	PersistentPaths     []string          `json:"persistentPaths"`
	PersistentPathKinds map[string]string `json:"persistentPathKinds,omitempty"`
	MutableMode         string            `json:"mutableMode"`
	Status              string            `json:"status"`
}

func catalogResourceOf(r domain.CatalogResource) CatalogResource {
	return CatalogResource{
		Path: r.Path, Name: r.Name, Activation: string(r.Activation),
		After: nonNil(r.After), Before: nonNil(r.Before), Provides: nonNil(r.Provides),
		Dependencies: nonNil(r.Dependencies), Games: nonNil(r.Games), ServerOnly: r.ServerOnly,
		PersistentPaths: nonNil(r.PersistentPaths), PersistentPathKinds: nonNilMap(r.PersistentPathKinds), MutableMode: r.MutableMode, Status: r.Status,
	}
}

func nonNilMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

type ResourcesSyncRequest struct {
	Path string `json:"path"`
	// NewActivation applies to newly discovered resources: pending (default),
	// enabled, or disabled.
	NewActivation string `json:"newActivation,omitempty"`
	FromCfg       string `json:"fromCfg,omitempty"`
}

type ResourcesSyncResult struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Missing []string `json:"missing"`
	Pending []string `json:"pending"`
	// CatalogPath is the repository-relative catalog file.
	CatalogPath string `json:"catalogPath,omitempty"`
	// Enabled lists entries whose activation was supplied by --from-cfg.
	Enabled []string `json:"enabled,omitempty"`
	// Unchanged counts entries the sync left untouched.
	Unchanged int `json:"unchanged,omitempty"`
	// Written is true when the catalog file changed on disk.
	Written bool `json:"written,omitempty"`
	// Warnings are cfg-import notes (skipped exec, ignored stop, bad names).
	Warnings []string `json:"warnings,omitempty"`
}

// ResourcesSync scans the accepted source root, evaluates every manifest in
// the sandbox, and updates .fxvcs/resources.yaml preserving operator-owned
// fields. It never commits and is idempotent.
func (c *Client) ResourcesSync(ctx context.Context, req ResourcesSyncRequest) (*ResourcesSyncResult, error) {
	r, err := c.openRepo(ctx, req.Path, "resources sync")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	c.progress(Event{Operation: "resources sync", Kind: EventOperationStarted})
	out, err := r.ResourcesSync(ctx, app.ResourcesSyncInput{NewActivation: domain.Activation(req.NewActivation), FromCfg: req.FromCfg})
	if err != nil {
		return nil, translateFivemError(r.Root, err)
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "resources sync", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	for _, p := range out.Missing {
		c.progress(Event{Operation: "resources sync", Kind: EventWarning, Code: CodeNotFound, Path: p, Message: "resource directory missing; entry kept as status missing"})
	}
	c.progress(Event{Operation: "resources sync", Kind: EventOperationFinished, Path: out.CatalogPath})
	return &ResourcesSyncResult{
		Added: nonNil(out.Added), Updated: nonNil(out.Updated), Missing: nonNil(out.Missing), Pending: nonNil(out.Pending),
		CatalogPath: out.CatalogPath, Enabled: out.Enabled, Unchanged: out.Unchanged, Written: out.Written, Warnings: out.Warnings,
	}, nil
}

type ResourcesSetRequest struct {
	Path         string   `json:"path"`
	ResourcePath string   `json:"resourcePath"` // literal
	Activation   string   `json:"activation,omitempty"`
	After        []string `json:"after,omitempty"`
	Before       []string `json:"before,omitempty"`
}

type ResourcesSetResult struct {
	ResourcePath string `json:"resourcePath"`
	Activation   string `json:"activation"`
	// Resource is the full entry after the edit.
	Resource *CatalogResource `json:"resource,omitempty"`
	// Warnings name after/before references that no local resource satisfies.
	Warnings []string `json:"warnings,omitempty"`
	Written  bool     `json:"written,omitempty"`
}

// ResourcesSet edits activation and ordering of one catalog entry identified
// by its literal path. It never commits or deploys.
func (c *Client) ResourcesSet(ctx context.Context, req ResourcesSetRequest) (*ResourcesSetResult, error) {
	r, err := c.openRepo(ctx, req.Path, "resources set")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := r.ResourcesSet(ctx, app.ResourcesSetInput{ResourcePath: req.ResourcePath, Activation: domain.Activation(req.Activation), After: req.After, Before: req.Before})
	if err != nil {
		return nil, translateFivemError(r.Root, err)
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "resources set", Kind: EventWarning, Code: CodeValidation, Path: out.Resource.Path, Message: w})
	}
	res := catalogResourceOf(out.Resource)
	return &ResourcesSetResult{ResourcePath: res.Path, Activation: res.Activation, Resource: &res, Warnings: out.Warnings, Written: out.Written}, nil
}

type ResourcesValidateRequest struct {
	Path string `json:"path"`
	// KnownNames are resource names supplied by other components, the runtime
	// catalog, or externals; after/before may reference them.
	KnownNames []string `json:"knownNames,omitempty"`
}

// ResourceProblem is one release-style validation finding.
type ResourceProblem struct {
	Path    string `json:"path"` // catalog entry path ("" for catalog-wide)
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ResourcesValidateResult struct {
	CatalogPath string            `json:"catalogPath"`
	Valid       bool              `json:"valid"`
	Resources   int               `json:"resources"`
	Problems    []ResourceProblem `json:"problems"`
	Warnings    []string          `json:"warnings,omitempty"`
	Undeclared  []string          `json:"undeclared"`
	Missing     []string          `json:"missing"`
}

// ResourcesValidate runs the release-style catalog validation: it re-scans
// the source root, diffs the committed catalog against the rescan, and runs
// release validation. When the catalog is not releasable the result lists
// every problem and the error has CodeValidation; scan failures (sandbox
// violations, ambiguous trees) return only an error.
func (c *Client) ResourcesValidate(ctx context.Context, req ResourcesValidateRequest) (*ResourcesValidateResult, error) {
	r, err := c.openRepo(ctx, req.Path, "resources validate")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out, err := r.ResourcesValidate(ctx, app.ResourcesValidateInput{KnownNames: req.KnownNames})
	if out == nil {
		return nil, translateFivemError(r.Root, err)
	}
	res := &ResourcesValidateResult{
		CatalogPath: out.CatalogPath, Valid: out.Valid, Resources: out.Resources,
		Problems: make([]ResourceProblem, 0, len(out.Problems)), Warnings: out.Warnings,
		Undeclared: nonNil(out.Diff.Undeclared), Missing: nonNil(out.Diff.Missing),
	}
	for _, p := range out.Problems {
		res.Problems = append(res.Problems, ResourceProblem{Path: p.Path, Field: p.Field, Message: p.Message})
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "resources validate", Kind: EventWarning, Code: CodeValidation, Message: w})
	}
	if err != nil {
		e := WrapError(CodeValidation, err.Error(), err)
		issues := make([]ValidationIssue, 0, len(res.Problems))
		for _, p := range res.Problems {
			issues = append(issues, ValidationIssue{Path: p.Path, Message: p.Field + ": " + p.Message})
		}
		e.Details = issues
		return res, e
	}
	return res, nil
}

// translateFivemError maps catalog/discovery/manifest errors before falling
// back to the generic translation.
func translateFivemError(root string, err error) error {
	if err == nil {
		return nil
	}
	var se *Error
	if errors.As(err, &se) {
		return err
	}
	var nf *app.ResourceNotFoundError
	if errors.As(err, &nf) {
		e := WrapError(CodeNotFound, err.Error(), err)
		e.Details = struct {
			ResourcePath string   `json:"resourcePath"`
			KnownPaths   []string `json:"knownPaths"`
		}{nf.Path, nonNil(nf.Known)}
		return e
	}
	var ci *app.CatalogInvalidError
	if errors.As(err, &ci) {
		e := WrapError(CodeValidation, err.Error(), err)
		issues := make([]ValidationIssue, 0, len(ci.Issues))
		for _, is := range ci.Issues {
			issues = append(issues, ValidationIssue{Path: ci.File + "#" + is.Path, Message: is.Message})
		}
		e.Details = issues
		return e
	}
	var cfg *app.CfgError
	if errors.As(err, &cfg) {
		code := CodeValidation
		switch cfg.Reason {
		case "absolute-path", "escapes-root":
			code = CodePathEscape
		case "unreadable":
			code = CodeIO
		}
		return WrapError(code, err.Error(), err)
	}
	var scan *app.ResourceScanError
	if errors.As(err, &scan) {
		e := WrapError(CodeValidation, err.Error(), err)
		var de *discovery.Error
		if errors.As(err, &de) {
			switch de.Reason {
			case discovery.ReasonIO:
				e.Code = CodeIO
			case discovery.ReasonNoManifests:
				e.Code = CodeNotFound
			}
		}
		if len(scan.Manifests) > 0 {
			issues := make([]ValidationIssue, 0, len(scan.Manifests))
			for _, m := range scan.Manifests {
				issues = append(issues, ValidationIssue{Path: m, Message: scan.Err.Error()})
			}
			e.Details = issues
		}
		return e
	}
	switch {
	case errors.Is(err, app.ErrProfileRequired):
		return WrapError(CodeInvalidArgument, err.Error(), err).WithHint("set one up with: fxvcs init --profile fivem")
	case errors.Is(err, app.ErrDescriptorMissing):
		return WrapError(CodeInvalidArgument, err.Error(), err).WithHint("fxvcs init --profile fivem --source-root <folder> --layout single|collection")
	case errors.Is(err, catalog.ErrSync):
		return WrapError(CodeInvalidArgument, err.Error(), err)
	case errors.Is(err, catalog.ErrNotReleasable), errors.Is(err, catalog.ErrCatalogDrift), errors.Is(err, app.ErrCatalogNotReleasable),
		errors.Is(err, manifest.ErrLimit), errors.Is(err, manifest.ErrForbidden), errors.Is(err, manifest.ErrInvalid), errors.Is(err, manifest.ErrSyntax):
		return WrapError(CodeValidation, err.Error(), err)
	}
	if _, ok := errors.AsType[schema.Issues](err); ok {
		return WrapError(CodeValidation, err.Error(), err)
	}
	return translateError(root, err)
}
