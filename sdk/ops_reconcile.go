package sdk

import (
	"context"

	"fxvcs.dev/fxvcs/internal/activationwindow"
	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/readiness"
	"fxvcs.dev/fxvcs/internal/reconcile"
	"fxvcs.dev/fxvcs/internal/supervisor"
)

type ReconcileTargetRequest struct {
	Path            string `json:"path"`
	Target          string `json:"target"`
	AgentConfigPath string `json:"agentConfigPath,omitempty"`
	Apply           bool   `json:"apply,omitempty"`
}

type ReconcileAction struct {
	Phase  string `json:"phase"`
	Action string `json:"action"`
}

type ReconcileRefusal struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

type ReconcileContentRequirement struct {
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Present   bool   `json:"present"`
	Available bool   `json:"available"`
}

type ReconcileResourceOperation struct {
	Action string `json:"action"`
	Name   string `json:"name,omitempty"`
}

type ReconcileTargetResult struct {
	Target               string                        `json:"target"`
	RealizationID        string                        `json:"realizationID,omitempty"`
	CurrentRealizationID string                        `json:"currentRealizationID,omitempty"`
	RequiredBytes        int64                         `json:"requiredBytes"`
	Applicable           bool                          `json:"applicable"`
	Applied              bool                          `json:"applied"`
	Actions              []ReconcileAction             `json:"actions,omitempty"`
	LifecycleMode        string                        `json:"lifecycleMode,omitempty"`
	ResourceOperations   []ReconcileResourceOperation  `json:"resourceOperations,omitempty"`
	Refusals             []ReconcileRefusal            `json:"refusals,omitempty"`
	MissingContent       []ReconcileContentRequirement `json:"missingContent,omitempty"`
}

// ReconcileTarget resolves and verifies the complete dry-run plan. Apply is
// explicit and runs the same final preflight under the target lock before any
// activation. Systemd full restart, source-verified credentialed RCON resource
// lifecycle/status, portable TCP/HTTP readiness, and the authenticated
// loopback resource-state helper are implemented; txAdmin control remains
// fail-closed pending its live-lab gate.
func (c *Client) ReconcileTarget(ctx context.Context, req ReconcileTargetRequest) (*ReconcileTargetResult, error) {
	if req.Target == "" {
		return nil, NewError(CodeInvalidArgument, "target is required")
	}
	binding, config, err := loadAgentBinding(req.AgentConfigPath, req.Target)
	if err != nil {
		return nil, err
	}
	coord, err := c.openCoord(ctx, req.Path, "reconcile target")
	if err != nil {
		return nil, err
	}
	if req.Apply {
		c.progress(Event{Operation: "reconcile target", Kind: EventOperationStarted, Path: req.Target})
		var adapter supervisor.Adapter = supervisor.Manual{}
		if binding.Runtime.Supervisor == "systemd" {
			adapter = supervisor.Systemd{Unit: binding.Runtime.SystemdUnit, Runner: supervisor.CommandRunner{}}
		}
		serverEndpoint := binding.Runtime.RCON
		if serverEndpoint == nil && binding.Runtime.ResourceState != nil {
			serverEndpoint = &domain.RCONEndpointRef{Host: binding.Runtime.ResourceState.Host, Port: binding.Runtime.ResourceState.Port}
		}
		applied, applyErr := coord.ApplyTarget(ctx, req.Target, *config, binding, app.ApplyDependencies{
			Supervisor:               adapter,
			PlayerCountObserver:      supervisor.FiveMHTTPPlayerCount{Endpoint: serverEndpoint},
			Readiness:                readiness.Runner{Checkers: readiness.PortableCheckers(serverEndpoint)},
			EvaluateActivationWindow: activationwindow.Evaluate,
			LockTimeout:              c.opts.LockTimeout,
			Progress: func(stage string) {
				c.progress(Event{Operation: "reconcile target", Kind: EventStage, Path: req.Target, Stage: Stage(stage)})
			},
		})
		out := publicReconcilePlan(applied.Plan)
		out.Applied = applied.Applied
		if applyErr != nil {
			c.progress(Event{Operation: "reconcile target", Kind: EventOperationFinished, Path: req.Target, Message: applyErr.Error()})
			return out, translateCoordError(req.Path, applyErr)
		}
		c.progress(Event{Operation: "reconcile target", Kind: EventOperationFinished, Path: req.Target})
		return out, nil
	}
	plan, err := coord.ResolveTargetPlan(ctx, req.Target, *config, binding)
	if err != nil {
		return nil, translateCoordError(req.Path, err)
	}
	return publicReconcilePlan(plan), nil
}

func publicReconcilePlan(plan reconcile.PlanResult) *ReconcileTargetResult {
	out := &ReconcileTargetResult{Target: plan.Target, RealizationID: plan.RealizationID, CurrentRealizationID: plan.CurrentRealizationID, RequiredBytes: plan.RequiredBytes, Applicable: plan.Applicable(), LifecycleMode: string(plan.LifecycleMode)}
	for _, action := range plan.Actions {
		out.Actions = append(out.Actions, ReconcileAction(action))
	}
	for _, refusal := range plan.Refusals {
		out.Refusals = append(out.Refusals, ReconcileRefusal{Code: string(refusal.Code), Path: refusal.Path, Message: refusal.Message})
	}
	for _, content := range plan.MissingContent {
		out.MissingContent = append(out.MissingContent, ReconcileContentRequirement(content))
	}
	for _, operation := range plan.ResourceOperations {
		out.ResourceOperations = append(out.ResourceOperations, ReconcileResourceOperation(operation))
	}
	return out
}
