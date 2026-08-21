// Package prune plans and applies exact target-local realization pruning.
// Planning is pure and apply revalidates every protection under the target lock.
package prune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/state"
)

var (
	ErrPlanChanged = errors.New("prune: plan protections changed")
	ErrRateLimit   = errors.New("prune: deletion rate limit reached")
)

type Realization struct {
	RealizationID string    `json:"realizationID"`
	Path          string    `json:"path"`
	CreatedAt     time.Time `json:"createdAt"`
	Bytes         int64     `json:"bytes"`
	InProgress    bool      `json:"inProgress,omitempty"`
}

type Candidate struct {
	RealizationID string `json:"realizationID"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
}

type MaterializedObject struct {
	Digest     string   `json:"digest"`
	Path       string   `json:"path"`
	Bytes      int64    `json:"bytes"`
	References []string `json:"references"`
}

type MaterializedCandidate struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
}

type PlanInput struct {
	DeployRoot       string
	Realizations     []Realization
	CurrentID        string
	LastKnownGoodID  string
	Retained         map[string]string
	MinimumRetention int
	Keep             int
	Materialized     []MaterializedObject
}

type Plan struct {
	PlanID                     string                  `json:"planID"`
	DeployRoot                 string                  `json:"deployRoot"`
	CurrentID                  string                  `json:"currentID,omitempty"`
	LastKnownGoodID            string                  `json:"lastKnownGoodID,omitempty"`
	Keep                       int                     `json:"keep"`
	Candidates                 []Candidate             `json:"candidates"`
	EstimatedBytes             int64                   `json:"estimatedBytes"`
	MaterializedCandidates     []MaterializedCandidate `json:"materializedCandidates,omitempty"`
	EstimatedMaterializedBytes int64                   `json:"estimatedMaterializedBytes,omitempty"`
}

func Build(in PlanInput) (Plan, error) {
	keep := max(in.Keep, in.MinimumRetention)
	if keep < 1 {
		return Plan{}, errors.New("prune: retention must be at least one")
	}
	realizations := append([]Realization(nil), in.Realizations...)
	sort.Slice(realizations, func(i, j int) bool {
		if realizations[i].CreatedAt.Equal(realizations[j].CreatedAt) {
			return realizations[i].RealizationID > realizations[j].RealizationID
		}
		return realizations[i].CreatedAt.After(realizations[j].CreatedAt)
	})
	protected := map[string]bool{in.CurrentID: true, in.LastKnownGoodID: true}
	delete(protected, "")
	for id := range in.Retained {
		protected[id] = true
	}
	for i, realization := range realizations {
		if i < keep || realization.InProgress {
			protected[realization.RealizationID] = true
		}
	}
	plan := Plan{DeployRoot: in.DeployRoot, CurrentID: in.CurrentID, LastKnownGoodID: in.LastKnownGoodID, Keep: keep}
	for i := len(realizations) - 1; i >= 0; i-- { // oldest first
		realization := realizations[i]
		if protected[realization.RealizationID] {
			continue
		}
		expected, err := layout.Realization(in.DeployRoot, realization.RealizationID)
		if err != nil || filepath.Clean(realization.Path) != expected {
			return Plan{}, fmt.Errorf("prune: invalid realization path for %s", realization.RealizationID)
		}
		if realization.Bytes < 0 {
			return Plan{}, errors.New("prune: negative candidate size")
		}
		plan.Candidates = append(plan.Candidates, Candidate{RealizationID: realization.RealizationID, Path: expected, Bytes: realization.Bytes})
		plan.EstimatedBytes += realization.Bytes
	}
	pruned := map[string]bool{}
	for _, candidate := range plan.Candidates {
		pruned[candidate.RealizationID] = true
	}
	objects := append([]MaterializedObject(nil), in.Materialized...)
	sort.Slice(objects, func(i, j int) bool { return objects[i].Digest < objects[j].Digest })
	for _, object := range objects {
		expected, err := layout.MaterializedPath(in.DeployRoot, object.Digest)
		if err != nil || filepath.Clean(object.Path) != expected || object.Bytes < 0 {
			return Plan{}, fmt.Errorf("prune: invalid materialized object %s", object.Digest)
		}
		protected := false
		for _, reference := range object.References {
			if !pruned[reference] {
				protected = true
				break
			}
		}
		if protected {
			continue
		}
		plan.MaterializedCandidates = append(plan.MaterializedCandidates, MaterializedCandidate{Digest: object.Digest, Path: expected, Bytes: object.Bytes})
		plan.EstimatedMaterializedBytes += object.Bytes
	}
	digest, err := canonical.Digest(struct {
		DeployRoot   string                  `json:"deployRoot"`
		Current      string                  `json:"current"`
		LKG          string                  `json:"lkg"`
		Keep         int                     `json:"keep"`
		Candidates   []Candidate             `json:"candidates"`
		Materialized []MaterializedCandidate `json:"materialized"`
	}{plan.DeployRoot, plan.CurrentID, plan.LastKnownGoodID, plan.Keep, plan.Candidates, plan.MaterializedCandidates})
	if err != nil {
		return Plan{}, err
	}
	plan.PlanID = "prune_" + strings.TrimPrefix(digest, "sha256:")[:24]
	return plan, nil
}

type ApplyOptions struct {
	DB                  *state.AgentDB
	Now                 func() time.Time
	MaxDeletionsPerHour int
	Failpoint           func(string) error
}

// Apply re-observes current, LKG, retained records and open operations before
// deleting. The caller must hold the target lock. Each deletion is renamed to
// a same-filesystem trash path under state/ before recursive removal, with an
// operation journal spanning the durability boundary.
func Apply(ctx context.Context, plan Plan, options ApplyOptions) error {
	if options.DB == nil {
		return errors.New("prune: database required")
	}
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	current, err := activation.ObserveCurrent(plan.DeployRoot)
	if errors.Is(err, activation.ErrNoCurrent) {
		current = ""
	} else if err != nil {
		return err
	}
	targetState, err := options.DB.TargetState(ctx)
	if err != nil {
		return err
	}
	if current != plan.CurrentID || targetState.LastKnownGoodRealizationID != plan.LastKnownGoodID {
		return ErrPlanChanged
	}
	retained, err := options.DB.RetainedRealizations(ctx)
	if err != nil {
		return err
	}
	open, err := options.DB.OpenOperations(ctx)
	if err != nil {
		return err
	}
	protected := map[string]bool{current: true, targetState.LastKnownGoodRealizationID: true}
	for id := range retained {
		protected[id] = true
	}
	for _, operation := range open {
		if operation.RealizationID != "" {
			protected[operation.RealizationID] = true
		}
	}
	if options.MaxDeletionsPerHour >= 0 {
		recent, err := options.DB.PruneDeletionTimes(ctx, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		pending := 0
		for _, candidate := range plan.Candidates {
			deleted, err := options.DB.HasPruneDeletion(ctx, plan.PlanID+"-"+candidate.RealizationID)
			if err != nil {
				return err
			}
			if !deleted {
				pending++
			}
		}
		if len(recent)+pending > options.MaxDeletionsPerHour {
			return ErrRateLimit
		}
	}
	encoded, _ := json.Marshal(plan)
	if err := options.DB.PutPrunePlan(ctx, state.PrunePlanRecord{PlanID: plan.PlanID, CreatedAt: now.Format(time.RFC3339Nano), Status: "applying", Candidates: encoded}); err != nil {
		return err
	}
	trashRoot := filepath.Join(plan.DeployRoot, layout.StateDir, "prune-trash")
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return err
	}
	for _, candidate := range plan.Candidates {
		if protected[candidate.RealizationID] {
			return ErrPlanChanged
		}
		expected, err := layout.Realization(plan.DeployRoot, candidate.RealizationID)
		if err != nil || candidate.Path != expected {
			return ErrPlanChanged
		}
		opID := plan.PlanID + "-" + candidate.RealizationID
		if _, err := os.Lstat(candidate.Path); errors.Is(err, os.ErrNotExist) {
			deleted, evidenceErr := options.DB.HasPruneDeletion(ctx, opID)
			if evidenceErr != nil {
				return evidenceErr
			}
			if deleted {
				continue
			}
			return ErrPlanChanged
		} else if err != nil {
			return err
		}
		payload, _ := json.Marshal(candidate)
		if err := options.DB.BeginOrReopenPruneOperation(ctx, state.OperationRecord{OperationID: opID, Operation: "prune", RealizationID: candidate.RealizationID, Phase: "planned", Payload: payload}); err != nil {
			return err
		}
		if options.Failpoint != nil {
			if err := options.Failpoint("before-rename"); err != nil {
				return err
			}
		}
		trash := filepath.Join(trashRoot, plan.PlanID+"-"+candidate.RealizationID)
		if err := os.Rename(candidate.Path, trash); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(candidate.Path)); err != nil {
			return err
		}
		if err := options.DB.AdvanceOperation(ctx, opID, "renamed", payload, ""); err != nil {
			return err
		}
		if options.Failpoint != nil {
			if err := options.Failpoint("after-rename"); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(trash); err != nil {
			return err
		}
		if err := syncDir(trashRoot); err != nil {
			return err
		}
		if options.Failpoint != nil {
			if err := options.Failpoint("after-remove"); err != nil {
				return err
			}
		}
		if err := options.DB.RecordPruneDeletion(ctx, opID, candidate.RealizationID, now); err != nil {
			return err
		}
		if err := options.DB.AdvanceOperation(ctx, opID, "removed", payload, "committed"); err != nil {
			return err
		}
	}
	if err := applyMaterializedGC(plan); err != nil {
		return err
	}
	return options.DB.PutPrunePlan(ctx, state.PrunePlanRecord{PlanID: plan.PlanID, CreatedAt: now.Format(time.RFC3339Nano), Status: "applied", Candidates: encoded, AppliedAt: now.Format(time.RFC3339Nano), Outcome: "committed"})
}

// Recover resumes or aborts an open prune operation from its journal phase and
// exact source/trash facts. Any contradictory combination fails closed.
func Recover(ctx context.Context, deployRoot, planID string, options ApplyOptions) error {
	if options.DB == nil {
		return errors.New("prune: database required")
	}
	now := time.Now().UTC()
	if options.Now != nil {
		now = options.Now().UTC()
	}
	open, err := options.DB.OpenOperations(ctx)
	if err != nil {
		return err
	}
	for _, operation := range open {
		if operation.Operation != "prune" || !strings.HasPrefix(operation.OperationID, planID+"-") {
			continue
		}
		var candidate Candidate
		if err := json.Unmarshal(operation.Payload, &candidate); err != nil {
			return fmt.Errorf("%w: invalid prune journal", ErrPlanChanged)
		}
		expected, err := layout.Realization(deployRoot, candidate.RealizationID)
		if err != nil || expected != candidate.Path {
			return ErrPlanChanged
		}
		trashRoot := filepath.Join(deployRoot, layout.StateDir, "prune-trash")
		trash := filepath.Join(trashRoot, planID+"-"+candidate.RealizationID)
		_, sourceErr := os.Lstat(expected)
		_, trashErr := os.Lstat(trash)
		sourceExists := sourceErr == nil
		trashExists := trashErr == nil
		switch operation.Phase {
		case "planned":
			if sourceExists && !trashExists {
				if err := options.DB.AdvanceOperation(ctx, operation.OperationID, "not-renamed", operation.Payload, "aborted"); err != nil {
					return err
				}
				continue
			}
		case "renamed":
			if !sourceExists && trashExists {
				if err := os.RemoveAll(trash); err != nil {
					return err
				}
				if err := syncDir(trashRoot); err != nil {
					return err
				}
				trashExists = false
			}
			// The durable renamed phase proves the source was moved into the
			// operation-specific trash path. If both paths are absent, removal
			// completed before a crash and only idempotent evidence remains.
			if !sourceExists && !trashExists {
				if err := options.DB.RecordPruneDeletion(ctx, operation.OperationID, candidate.RealizationID, now); err != nil {
					return err
				}
				if err := options.DB.AdvanceOperation(ctx, operation.OperationID, "removed", operation.Payload, "recovered-committed"); err != nil {
					return err
				}
				continue
			}
		}
		return fmt.Errorf("%w: prune recovery source=%t trash=%t phase=%s", ErrPlanChanged, sourceExists, trashExists, operation.Phase)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
