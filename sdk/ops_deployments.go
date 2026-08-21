package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/health"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/prune"
	"fxvcs.dev/fxvcs/internal/schema"
	"fxvcs.dev/fxvcs/internal/state"
)

const defaultAgentConfigPath = "/etc/fxvcs/agent.yaml"

type DeploymentRecord struct {
	RealizationID       string `json:"realizationID"`
	Release             string `json:"release"`
	Environment         string `json:"environment"`
	EnvironmentRevision int    `json:"environmentRevision"`
	Phase               string `json:"phase"`
	RetryHeld           bool   `json:"retryHeld"`
	AttemptInvalidated  bool   `json:"attemptInvalidated"`
	StartedAt           string `json:"startedAt"`
	FinishedAt          string `json:"finishedAt,omitempty"`
	Outcome             string `json:"outcome,omitempty"`
}

type DeploymentsListRequest struct {
	AgentConfigPath string `json:"agentConfigPath,omitempty"`
	Target          string `json:"target,omitempty"`
}

type TargetDeploymentHistory struct {
	Target                     string             `json:"target"`
	DeployRoot                 string             `json:"deployRoot"`
	CurrentRealizationID       string             `json:"currentRealizationID,omitempty"`
	LastKnownGoodRealizationID string             `json:"lastKnownGoodRealizationID,omitempty"`
	Deployments                []DeploymentRecord `json:"deployments"`
}

type DeploymentsListResult struct {
	Targets []TargetDeploymentHistory `json:"targets"`
}

func (c *Client) DeploymentsList(ctx context.Context, req DeploymentsListRequest) (*DeploymentsListResult, error) {
	config, err := loadAgentConfig(req.AgentConfigPath)
	if err != nil {
		return nil, err
	}
	bindings := append([]domain.AgentTargetBinding(nil), config.Spec.Targets...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Target < bindings[j].Target })
	result := &DeploymentsListResult{Targets: make([]TargetDeploymentHistory, 0, len(bindings))}
	for _, binding := range bindings {
		if req.Target != "" && binding.Target != req.Target {
			continue
		}
		history, err := c.targetDeploymentHistory(ctx, binding)
		if err != nil {
			return nil, err
		}
		result.Targets = append(result.Targets, history)
	}
	if req.Target != "" && len(result.Targets) == 0 {
		return nil, NewError(CodeNotFound, "target binding not found in agent config: "+req.Target)
	}
	return result, nil
}

func (c *Client) targetDeploymentHistory(ctx context.Context, binding domain.AgentTargetBinding) (TargetDeploymentHistory, error) {
	history := TargetDeploymentHistory{Target: binding.Target, DeployRoot: binding.DeployRoot, Deployments: []DeploymentRecord{}}
	current, currentErr := activation.ObserveCurrent(binding.DeployRoot)
	if errors.Is(currentErr, activation.ErrNoCurrent) {
		current = ""
	} else if currentErr != nil {
		return TargetDeploymentHistory{}, WrapError(CodeConflict, currentErr.Error(), currentErr)
	}
	history.CurrentRealizationID = current
	dbPath := filepath.Join(binding.DeployRoot, layout.StateDir, "agent.db")
	if _, err := os.Lstat(dbPath); errors.Is(err, os.ErrNotExist) {
		return history, nil
	} else if err != nil {
		return TargetDeploymentHistory{}, WrapError(CodeIO, "inspect target state", err)
	}
	db, err := state.OpenAgent(ctx, dbPath, state.Options{Target: binding.Target, InstalledVersion: c.installed.String()})
	if err != nil {
		return TargetDeploymentHistory{}, translateError(binding.DeployRoot, err)
	}
	defer db.Close()
	rows, err := db.Deployments(ctx)
	if err != nil {
		return TargetDeploymentHistory{}, WrapError(CodeIO, "read deployment history", err)
	}
	targetState, err := db.TargetState(ctx)
	if err != nil {
		return TargetDeploymentHistory{}, WrapError(CodeIO, "read target state", err)
	}
	history.LastKnownGoodRealizationID = targetState.LastKnownGoodRealizationID
	history.Deployments = make([]DeploymentRecord, 0, len(rows))
	for _, row := range rows {
		history.Deployments = append(history.Deployments, deploymentRecord(row))
	}
	return history, nil
}

type DeploymentPruneCandidate struct {
	RealizationID string `json:"realizationID"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
}

type DeploymentsPruneRequest struct {
	AgentConfigPath string `json:"agentConfigPath,omitempty"`
	Target          string `json:"target"`
	Keep            int    `json:"keep,omitempty"`
	Apply           bool   `json:"apply,omitempty"`
	ExpectedPlanID  string `json:"expectedPlanID,omitempty"`
}

type DeploymentsPruneResult struct {
	Target                     string                                 `json:"target"`
	PlanID                     string                                 `json:"planID"`
	Applied                    bool                                   `json:"applied"`
	Keep                       int                                    `json:"keep"`
	EstimatedBytes             int64                                  `json:"estimatedBytes"`
	Candidates                 []DeploymentPruneCandidate             `json:"candidates"`
	MaterializedCandidates     []DeploymentPruneMaterializedCandidate `json:"materializedCandidates,omitempty"`
	EstimatedMaterializedBytes int64                                  `json:"estimatedMaterializedBytes,omitempty"`
}

type DeploymentPruneMaterializedCandidate struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
}

func (c *Client) DeploymentsPrune(ctx context.Context, req DeploymentsPruneRequest) (*DeploymentsPruneResult, error) {
	if req.Keep < 0 {
		return nil, NewError(CodeInvalidArgument, "keep must not be negative")
	}
	binding, config, err := loadAgentBinding(req.AgentConfigPath, req.Target)
	if err != nil {
		return nil, err
	}
	db, err := state.OpenAgent(ctx, filepath.Join(binding.DeployRoot, layout.StateDir, "agent.db"), state.Options{Target: req.Target, InstalledVersion: c.installed.String()})
	if err != nil {
		return nil, translateError(binding.DeployRoot, err)
	}
	defer db.Close()
	plan, err := buildPrunePlan(ctx, db, binding, config.Spec.SafetyFloors.MinimumRetention, req.Keep)
	if err != nil {
		return nil, WrapError(CodeValidation, "plan deployment pruning", err)
	}
	result := publicPrunePlan(req.Target, plan)
	if !req.Apply {
		return result, nil
	}
	resumeInterrupted := false
	if req.ExpectedPlanID != "" && req.ExpectedPlanID != plan.PlanID {
		stored, exists, storedErr := db.PrunePlan(ctx, req.ExpectedPlanID)
		if storedErr != nil {
			return result, WrapError(CodeIO, "read interrupted prune plan", storedErr)
		}
		if !exists || stored.Status != "applying" || json.Unmarshal(stored.Candidates, &plan) != nil || plan.PlanID != req.ExpectedPlanID || plan.DeployRoot != binding.DeployRoot {
			return result, WrapError(CodeConflict, "prune plan changed; review the new dry run", prune.ErrPlanChanged)
		}
		resumeInterrupted = true
		result = publicPrunePlan(req.Target, plan)
	}
	lockHandle, err := activation.AcquireTargetLock(ctx, binding.DeployRoot, "deployments prune", c.opts.LockTimeout)
	if err != nil {
		return result, translateError(binding.DeployRoot, err)
	}
	defer func() { _ = lockHandle.Release() }()
	if resumeInterrupted {
		if err := prune.Recover(ctx, binding.DeployRoot, plan.PlanID, prune.ApplyOptions{DB: db}); err != nil {
			return result, WrapError(CodeConflict, "recover interrupted prune plan", err)
		}
		if err := prune.Apply(ctx, plan, prune.ApplyOptions{DB: db, MaxDeletionsPerHour: config.Spec.SafetyFloors.MaxDeletionsPerHour}); err != nil {
			return result, WrapError(CodeIO, "resume interrupted prune plan", err)
		}
		result.Applied = true
		return result, nil
	}
	// Rebuild under the lock so minimum retention and every filesystem fact are
	// part of the exact plan being applied.
	lockedPlan, err := buildPrunePlan(ctx, db, binding, config.Spec.SafetyFloors.MinimumRetention, req.Keep)
	if err != nil {
		return result, WrapError(CodeValidation, "revalidate deployment pruning", err)
	}
	if lockedPlan.PlanID != plan.PlanID {
		return result, WrapError(CodeConflict, "prune plan changed while waiting for the target lock", prune.ErrPlanChanged)
	}
	if err := prune.Apply(ctx, lockedPlan, prune.ApplyOptions{DB: db, MaxDeletionsPerHour: config.Spec.SafetyFloors.MaxDeletionsPerHour}); err != nil {
		code := CodeIO
		if errors.Is(err, prune.ErrPlanChanged) || errors.Is(err, prune.ErrRateLimit) {
			code = CodeConflict
		}
		return result, WrapError(code, err.Error(), err)
	}
	result.Applied = true
	return result, nil
}

func publicPrunePlan(target string, plan prune.Plan) *DeploymentsPruneResult {
	result := &DeploymentsPruneResult{Target: target, PlanID: plan.PlanID, Keep: plan.Keep, EstimatedBytes: plan.EstimatedBytes, Candidates: make([]DeploymentPruneCandidate, 0, len(plan.Candidates)), EstimatedMaterializedBytes: plan.EstimatedMaterializedBytes, MaterializedCandidates: make([]DeploymentPruneMaterializedCandidate, 0, len(plan.MaterializedCandidates))}
	for _, candidate := range plan.Candidates {
		result.Candidates = append(result.Candidates, DeploymentPruneCandidate(candidate))
	}
	for _, candidate := range plan.MaterializedCandidates {
		result.MaterializedCandidates = append(result.MaterializedCandidates, DeploymentPruneMaterializedCandidate(candidate))
	}
	return result
}

func loadAgentBinding(path, target string) (domain.AgentTargetBinding, *domain.AgentConfig, error) {
	if target == "" {
		return domain.AgentTargetBinding{}, nil, NewError(CodeInvalidArgument, "target is required")
	}
	config, err := loadAgentConfig(path)
	if err != nil {
		return domain.AgentTargetBinding{}, nil, err
	}
	for _, binding := range config.Spec.Targets {
		if binding.Target == target {
			return binding, config, nil
		}
	}
	return domain.AgentTargetBinding{}, nil, NewError(CodeNotFound, "target binding not found in agent config: "+target)
}

func loadAgentConfig(path string) (*domain.AgentConfig, error) {
	if path == "" {
		path = os.Getenv("FXVCS_AGENT_CONFIG")
		if path == "" {
			path = defaultAgentConfigPath
		}
	}
	data, err := os.ReadFile(path) //nolint:gosec // G703: path is the explicit host-operator AgentConfig location, not a repository path
	if err != nil {
		return nil, WrapError(CodeNotFound, "read agent config "+path, err)
	}
	config, err := schema.LoadAgentConfig(data)
	if err != nil {
		return nil, WrapError(CodeValidation, "invalid agent config "+path, err)
	}
	return config, nil
}

func deploymentRecord(dep domain.Deployment) DeploymentRecord {
	return DeploymentRecord{RealizationID: dep.RealizationID, Release: dep.Inputs.ReleaseName, Environment: dep.Inputs.EnvironmentName, EnvironmentRevision: dep.Inputs.EnvironmentRevision, Phase: dep.Phase, RetryHeld: dep.RetryHeld, AttemptInvalidated: dep.AttemptInvalidated, StartedAt: dep.StartedAt, FinishedAt: dep.FinishedAt, Outcome: dep.Outcome}
}

func buildPrunePlan(ctx context.Context, db *state.AgentDB, binding domain.AgentTargetBinding, minimumRetention, keep int) (prune.Plan, error) {
	rows, err := db.Deployments(ctx)
	if err != nil {
		return prune.Plan{}, err
	}
	current, err := activation.ObserveCurrent(binding.DeployRoot)
	if errors.Is(err, activation.ErrNoCurrent) {
		current = ""
	} else if err != nil {
		return prune.Plan{}, err
	}
	targetState, err := db.TargetState(ctx)
	if err != nil {
		return prune.Plan{}, err
	}
	retained, err := db.RetainedRealizations(ctx)
	if err != nil {
		return prune.Plan{}, err
	}
	materialized, err := prune.ObserveMaterialized(binding.DeployRoot)
	if err != nil {
		return prune.Plan{}, err
	}
	facts := make([]prune.Realization, 0, len(rows))
	for _, row := range rows {
		path, pathErr := layout.Realization(binding.DeployRoot, row.RealizationID)
		if pathErr != nil {
			return prune.Plan{}, pathErr
		}
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil || !info.IsDir() {
			return prune.Plan{}, statErr
		}
		created, parseErr := time.Parse(time.RFC3339Nano, row.StartedAt)
		if parseErr != nil {
			return prune.Plan{}, parseErr
		}
		bytes, sizeErr := directoryBytes(path)
		if sizeErr != nil {
			return prune.Plan{}, sizeErr
		}
		phase := health.Phase(row.Phase)
		inProgress := phase == health.PhaseStaging || phase == health.PhaseActiveGrace || phase == health.PhaseRollingBack
		facts = append(facts, prune.Realization{RealizationID: row.RealizationID, Path: path, CreatedAt: created, Bytes: bytes, InProgress: inProgress})
	}
	if keep == 0 {
		keep = minimumRetention
	}
	return prune.Build(prune.PlanInput{DeployRoot: binding.DeployRoot, Realizations: facts, CurrentID: current, LastKnownGoodID: targetState.LastKnownGoodRealizationID, Retained: retained, Keep: keep, MinimumRetention: minimumRetention, Materialized: materialized})
}

func directoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}
