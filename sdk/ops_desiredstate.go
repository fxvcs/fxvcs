package sdk

import (
	"context"

	"fxvcs.dev/fxvcs/internal/app"
)

// Targets and environments are mutable desired state in the coordination
// repository. These operations edit reviewable YAML; promotion, rollback, and
// environment changes all become authoritative through ordinary Git review.
// No secret value can pass through them — the schemas have no property for one.

// ---------------------------------------------------------------------------
// environments
// ---------------------------------------------------------------------------

// EnvironmentSetRequest creates or updates one environment.
type EnvironmentSetRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Inputs are non-secret "key=value" pairs; an empty value removes the key.
	Inputs []string `json:"inputs,omitempty"`
	// SecretScope is the logical secret scope (default: the environment name).
	SecretScope string `json:"secretScope,omitempty"`
}

// Environment is one environment record.
type Environment struct {
	Name        string            `json:"name"`
	File        string            `json:"file"`
	Revision    int               `json:"revision"`
	Inputs      map[string]string `json:"inputs"`
	SecretScope string            `json:"secretScope"`
	Created     bool              `json:"created,omitempty"`
	Changed     bool              `json:"changed,omitempty"`
	UsedBy      []string          `json:"usedBy,omitempty"`
}

// EnvironmentSet creates or updates an environment, bumping its revision
// whenever the content actually changes.
func (c *Client) EnvironmentSet(ctx context.Context, req EnvironmentSetRequest) (*Environment, error) {
	co, err := c.openCoord(ctx, req.Path, "environment set")
	if err != nil {
		return nil, err
	}
	out, err := co.EnvironmentSet(ctx, app.EnvironmentSetInput{Name: req.Name, Inputs: req.Inputs, SecretScope: req.SecretScope})
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := Environment(*out)
	return &res, nil
}

// EnvironmentShowRequest reads one environment.
type EnvironmentShowRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// EnvironmentShow reads one environment. It shows the logical secret scope,
// never a secret value: an environment record cannot hold one.
func (c *Client) EnvironmentShow(ctx context.Context, req EnvironmentShowRequest) (*Environment, error) {
	co, err := c.openCoord(ctx, req.Path, "environment show")
	if err != nil {
		return nil, err
	}
	out, err := co.EnvironmentShow(ctx, req.Name)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := Environment(*out)
	return &res, nil
}

// EnvironmentListRequest lists environments.
type EnvironmentListRequest struct {
	Path string `json:"path"`
}

// EnvironmentListResult is every environment.
type EnvironmentListResult struct {
	Environments []Environment `json:"environments"`
}

// EnvironmentList lists every environment in the coordination repository.
func (c *Client) EnvironmentList(ctx context.Context, req EnvironmentListRequest) (*EnvironmentListResult, error) {
	co, err := c.openCoord(ctx, req.Path, "environment list")
	if err != nil {
		return nil, err
	}
	rows, err := co.EnvironmentList(ctx)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &EnvironmentListResult{Environments: make([]Environment, 0, len(rows))}
	for _, r := range rows {
		res.Environments = append(res.Environments, Environment(r))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// targets
// ---------------------------------------------------------------------------

// TargetSetRequest creates or updates one target's desired state.
type TargetSetRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
	// Release is the desired release. Promotion and rollback are both this.
	Release string `json:"release,omitempty"`
	// The rest are required when the target is created, optional afterwards.
	Environment    string `json:"environment,omitempty"`
	AgentID        string `json:"agentID,omitempty"`
	RuntimeRef     string `json:"runtimeRef,omitempty"`
	Keep           int    `json:"keep,omitempty"`
	SecretProvider string `json:"secretProvider,omitempty"`
}

// Target is one target's desired state.
type Target struct {
	Name        string `json:"name"`
	File        string `json:"file"`
	Release     string `json:"release"`
	Environment string `json:"environment"`
	AgentID     string `json:"agentID"`
	Authority   string `json:"authority"`
	RuntimeRef  string `json:"runtimeRef"`
	Keep        int    `json:"keep"`
	// SecretProvider is the default provider alias. Provider implementations,
	// endpoints, credentials, and values stay on the agent.
	SecretProvider      string   `json:"secretProvider"`
	Created             bool     `json:"created,omitempty"`
	Changed             bool     `json:"changed,omitempty"`
	PreviousRelease     string   `json:"previousRelease,omitempty"`
	ReleaseExists       bool     `json:"releaseExists"`
	Enabled             int      `json:"enabled,omitempty"`
	EnvironmentRevision int      `json:"environmentRevision,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

// TargetSet assigns a release to a target, creating the target when it does
// not exist. The release must already have a record here.
func (c *Client) TargetSet(ctx context.Context, req TargetSetRequest) (*Target, error) {
	co, err := c.openCoord(ctx, req.Path, "target set")
	if err != nil {
		return nil, err
	}
	out, err := co.TargetSet(ctx, app.TargetSetInput{
		Name: req.Name, Release: req.Release, Environment: req.Environment, AgentID: req.AgentID,
		RuntimeRef: req.RuntimeRef, Keep: req.Keep, SecretProvider: req.SecretProvider,
	})
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	for _, w := range out.Warnings {
		c.progress(Event{Operation: "target set", Kind: EventWarning, Code: CodeValidation, Path: out.Name, Message: w})
	}
	res := Target(*out)
	return &res, nil
}

// TargetShowRequest reads one target.
type TargetShowRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// TargetShow reads one target's desired state. Locally observed deployment
// state belongs to the agent and is not part of this result.
func (c *Client) TargetShow(ctx context.Context, req TargetShowRequest) (*Target, error) {
	co, err := c.openCoord(ctx, req.Path, "target show")
	if err != nil {
		return nil, err
	}
	out, err := co.TargetShow(ctx, req.Name)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := Target(*out)
	return &res, nil
}

// TargetListRequest lists targets.
type TargetListRequest struct {
	Path string `json:"path"`
}

// TargetListResult is every target.
type TargetListResult struct {
	Targets []Target `json:"targets"`
}

// TargetList lists every target in the coordination repository.
func (c *Client) TargetList(ctx context.Context, req TargetListRequest) (*TargetListResult, error) {
	co, err := c.openCoord(ctx, req.Path, "target list")
	if err != nil {
		return nil, err
	}
	rows, err := co.TargetList(ctx)
	if err != nil {
		return nil, translateCoordError(co.Root, err)
	}
	res := &TargetListResult{Targets: make([]Target, 0, len(rows))}
	for _, r := range rows {
		res.Targets = append(res.Targets, Target(r))
	}
	return res, nil
}
