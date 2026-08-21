package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

// Targets and environments are mutable desired state. Both are ordinary
// committed YAML: `target set` edits a file, and the change becomes
// authoritative through the same review a code change gets. Nothing here
// deploys, and nothing here can hold a secret value — the schemas have no
// property for one.

// ---------------------------------------------------------------------------
// environments
// ---------------------------------------------------------------------------

// EnvironmentSetInput creates or updates one environment.
type EnvironmentSetInput struct {
	Name string
	// Inputs are non-secret values, "key=value". Setting a key to the empty
	// string removes it.
	Inputs []string
	// SecretScope is the logical secret namespace scope (default: the name).
	SecretScope string
}

// EnvironmentResult is one environment record.
type EnvironmentResult struct {
	Name        string            `json:"name"`
	File        string            `json:"file"`
	Revision    int               `json:"revision"`
	Inputs      map[string]string `json:"inputs"`
	SecretScope string            `json:"secretScope"`
	Created     bool              `json:"created,omitempty"`
	Changed     bool              `json:"changed,omitempty"`
	// UsedBy names targets referencing this environment.
	UsedBy []string `json:"usedBy,omitempty"`
}

// EnvironmentSet creates or updates an environment. Every change that alters
// content bumps the revision, because a realization's identity includes the
// environment revision: an unchanged number would let a target look reconciled
// against configuration that moved underneath it.
func (c *Coord) EnvironmentSet(ctx context.Context, in EnvironmentSetInput) (*EnvironmentResult, error) {
	env, err := c.GetEnvironment(in.Name)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, controlplane.ErrNotFound):
		created = true
		env = domain.Environment{
			Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindEnvironment},
			Metadata: domain.NamedMetadata{Name: in.Name},
			Spec:     domain.EnvironmentSpec{Revision: 0, Inputs: map[string]string{}, SecretScope: in.Name},
		}
	default:
		return nil, err
	}
	// Snapshot by value: Inputs is a map, so a plain struct copy would alias
	// it and every comparison below would report "unchanged".
	before := domain.EnvironmentSpec{Revision: env.Spec.Revision, SecretScope: env.Spec.SecretScope, Inputs: map[string]string{}}
	for k, v := range env.Spec.Inputs {
		before.Inputs[k] = v
	}
	if env.Spec.Inputs == nil {
		env.Spec.Inputs = map[string]string{}
	}
	for _, kv := range in.Inputs {
		key, value, ok := strings.Cut(kv, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: --input wants key=value, got %q", ErrInvalidArgument, kv)
		}
		if value == "" {
			delete(env.Spec.Inputs, key)
			continue
		}
		env.Spec.Inputs[key] = value
	}
	if in.SecretScope != "" {
		env.Spec.SecretScope = in.SecretScope
	}
	changed := created || !sameEnvironmentSpec(before, env.Spec)
	if changed {
		env.Spec.Revision++
	}
	path, err := c.SaveEnvironment(env)
	if err != nil {
		return nil, err
	}
	res := &EnvironmentResult{
		Name: env.Metadata.Name, File: c.Rel(path), Revision: env.Spec.Revision,
		Inputs: env.Spec.Inputs, SecretScope: env.Spec.SecretScope, Created: created, Changed: changed,
	}
	res.UsedBy = c.targetsUsingEnvironment(ctx, env.Metadata.Name)
	return res, nil
}

func sameEnvironmentSpec(a, b domain.EnvironmentSpec) bool {
	if a.SecretScope != b.SecretScope || len(a.Inputs) != len(b.Inputs) {
		return false
	}
	for k, v := range a.Inputs {
		if b.Inputs[k] != v {
			return false
		}
	}
	return true
}

// EnvironmentShow reads one environment. It never shows a secret value
// because an environment never holds one; it holds the scope those values are
// looked up in.
func (c *Coord) EnvironmentShow(ctx context.Context, name string) (*EnvironmentResult, error) {
	env, err := c.GetEnvironment(name)
	if err != nil {
		return nil, err
	}
	inputs := env.Spec.Inputs
	if inputs == nil {
		inputs = map[string]string{}
	}
	return &EnvironmentResult{
		Name: env.Metadata.Name, File: c.Rel(c.EnvironmentPath(name)), Revision: env.Spec.Revision,
		Inputs: inputs, SecretScope: env.Spec.SecretScope, UsedBy: c.targetsUsingEnvironment(ctx, name),
	}, nil
}

// EnvironmentList lists every environment.
func (c *Coord) EnvironmentList(ctx context.Context) ([]EnvironmentResult, error) {
	names, err := c.ListEnvironments()
	if err != nil {
		return nil, err
	}
	out := make([]EnvironmentResult, 0, len(names))
	for _, n := range names {
		env, err := c.EnvironmentShow(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, *env)
	}
	return out, nil
}

func (c *Coord) targetsUsingEnvironment(ctx context.Context, name string) []string {
	names, err := c.ListTargets()
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range names {
		if tgt, err := c.GetTarget(ctx, t); err == nil && tgt.Spec.Environment == name {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

// TargetSetInput creates or updates one target's desired state.
type TargetSetInput struct {
	Name string
	// Release is the desired release. Promotion and rollback are both this.
	Release string
	// Environment, AgentID, RuntimeRef, Keep, and SecretProvider are required
	// when the target is created and optional afterwards.
	Environment    string
	AgentID        string
	RuntimeRef     string
	Keep           int
	SecretProvider string
}

// TargetResult is one target's desired state.
type TargetResult struct {
	Name        string `json:"name"`
	File        string `json:"file"`
	Release     string `json:"release"`
	Environment string `json:"environment"`
	AgentID     string `json:"agentID"`
	Authority   string `json:"authority"`
	RuntimeRef  string `json:"runtimeRef"`
	Keep        int    `json:"keep"`
	// SecretProvider is the default provider alias; provider implementations,
	// endpoints, and values stay on the agent.
	SecretProvider string `json:"secretProvider"`
	Created        bool   `json:"created,omitempty"`
	Changed        bool   `json:"changed,omitempty"`
	// PreviousRelease is what the target wanted before this change.
	PreviousRelease string `json:"previousRelease,omitempty"`
	// ReleaseExists reports whether the desired release record is present here.
	ReleaseExists bool `json:"releaseExists"`
	// Enabled counts the resources the desired release starts.
	Enabled int `json:"enabled,omitempty"`
	// EnvironmentRevision is the revision of the referenced environment.
	EnvironmentRevision int      `json:"environmentRevision,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

// TargetSet creates or updates a target. The release must already exist as a
// record here: pointing a target at a name nobody has committed is how a fleet
// ends up waiting for a release that will never arrive.
func (c *Coord) TargetSet(ctx context.Context, in TargetSetInput) (*TargetResult, error) {
	tgt, err := c.GetTarget(ctx, in.Name)
	created := false
	switch {
	case err == nil:
	case errors.Is(err, controlplane.ErrNotFound):
		created = true
		tgt = domain.Target{
			Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindTarget},
			Metadata: domain.NamedMetadata{Name: in.Name},
			Spec: domain.TargetSpec{
				Authority: domain.AuthorityGitOps,
				Retention: domain.RetentionPolicy{Keep: 3},
			},
		}
	default:
		return nil, err
	}
	previous := tgt.Spec.Release
	before := tgt.Spec

	if in.Release != "" {
		if _, err := c.GetRelease(ctx, in.Release); err != nil {
			return nil, fmt.Errorf("%w (create it first with: fxvcs release create %s)", err, in.Release)
		}
		tgt.Spec.Release = in.Release
	}
	if in.Environment != "" {
		if _, err := c.GetEnvironment(in.Environment); err != nil {
			return nil, fmt.Errorf("%w (create it first with: fxvcs environment set %s)", err, in.Environment)
		}
		tgt.Spec.Environment = in.Environment
	}
	if in.AgentID != "" {
		tgt.Spec.AgentID = in.AgentID
	}
	if in.RuntimeRef != "" {
		tgt.Spec.RuntimeRef = in.RuntimeRef
	}
	if in.Keep > 0 {
		tgt.Spec.Retention.Keep = in.Keep
	}
	if in.SecretProvider != "" {
		tgt.Spec.Secrets.DefaultProvider = in.SecretProvider
	}
	if created {
		var missing []string
		for _, f := range []struct{ name, value string }{
			{"--release", tgt.Spec.Release},
			{"--environment", tgt.Spec.Environment},
			{"--agent", tgt.Spec.AgentID},
			{"--runtime", tgt.Spec.RuntimeRef},
			{"--secret-provider", tgt.Spec.Secrets.DefaultProvider},
		} {
			if f.value == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("%w: a new target needs %s", ErrInvalidArgument, strings.Join(missing, ", "))
		}
	}
	changed := created || !sameTargetSpec(before, tgt.Spec)
	path, err := c.SaveTarget(tgt)
	if err != nil {
		return nil, err
	}
	res := c.targetResult(ctx, tgt, path)
	res.Created, res.Changed, res.PreviousRelease = created, changed, previous
	return res, nil
}

func sameTargetSpec(a, b domain.TargetSpec) bool {
	return a.Release == b.Release && a.Environment == b.Environment && a.AgentID == b.AgentID &&
		a.RuntimeRef == b.RuntimeRef && a.Retention.Keep == b.Retention.Keep &&
		a.Secrets.DefaultProvider == b.Secrets.DefaultProvider
}

// TargetShow reads one target's desired state and what the release it selects
// would start. Locally observed deployment state belongs to the agent (Stage
// 4) and is deliberately absent here.
func (c *Coord) TargetShow(ctx context.Context, name string) (*TargetResult, error) {
	tgt, err := c.GetTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	return c.targetResult(ctx, tgt, c.TargetPath(name)), nil
}

// TargetList lists every target.
func (c *Coord) TargetList(ctx context.Context) ([]TargetResult, error) {
	names, err := c.ListTargets()
	if err != nil {
		return nil, err
	}
	out := make([]TargetResult, 0, len(names))
	for _, n := range names {
		t, err := c.TargetShow(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

func (c *Coord) targetResult(ctx context.Context, tgt domain.Target, path string) *TargetResult {
	res := &TargetResult{
		Name: tgt.Metadata.Name, File: c.Rel(path), Release: tgt.Spec.Release, Environment: tgt.Spec.Environment,
		AgentID: tgt.Spec.AgentID, Authority: tgt.Spec.Authority, RuntimeRef: tgt.Spec.RuntimeRef,
		Keep: tgt.Spec.Retention.Keep, SecretProvider: tgt.Spec.Secrets.DefaultProvider,
	}
	if rel, err := c.GetRelease(ctx, tgt.Spec.Release); err == nil {
		res.ReleaseExists = true
		for _, r := range rel.Spec.Resources {
			if r.Activation == domain.ActivationEnabled {
				res.Enabled++
			}
		}
	} else {
		res.Warnings = append(res.Warnings, fmt.Sprintf("release %q has no record in this stack yet", tgt.Spec.Release))
	}
	if env, err := c.GetEnvironment(tgt.Spec.Environment); err == nil {
		res.EnvironmentRevision = env.Spec.Revision
	} else {
		res.Warnings = append(res.Warnings, fmt.Sprintf("environment %q has no record in this stack yet", tgt.Spec.Environment))
	}
	return res
}
