package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/prune"
	"fxvcs.dev/fxvcs/internal/state"
)

func deploymentFixture(t *testing.T) (context.Context, *Client, string, string, []string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	emptyRoot := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := fmt.Sprintf(`apiVersion: fxvcs.dev/v1alpha1
kind: AgentConfig
spec:
  agentID: test-agent
  git:
    shallow: false
  targets:
    - target: dev
      deployRoot: %q
      serverData: %q
      runtime:
        supervisor: manual
      trust:
        coordination:
          authority: signed-commit-tip
          signatures: required
    - target: new
      deployRoot: %q
      serverData: %q
      runtime:
        supervisor: manual
      trust:
        coordination:
          authority: signed-commit-tip
          signatures: required
  safetyFloors:
    minimumRetention: 2
    maxDeletionsPerHour: 10
    maxAutomaticRollbacks: 1
    disableAutomaticRollback: false
`, root, filepath.Join(t.TempDir(), "server"), emptyRoot, filepath.Join(t.TempDir(), "new-server"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(root, layout.StateDir, "agent.db"), state.Options{Create: true, Target: "dev", InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"rz_one", "rz_two", "rz_three", "rz_four"}
	t0 := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	for i, id := range ids {
		path, _ := layout.Realization(root, id)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "payload"), make([]byte, i+1), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := db.PutDeployment(ctx, domain.Deployment{
			RealizationID: id,
			Target:        "dev",
			Inputs: domain.RealizationInputs{
				ReleaseName:         fmt.Sprintf("v%d", i+1),
				EnvironmentName:     "staging",
				EnvironmentRevision: i + 1,
			},
			Phase:     "active-healthy",
			StartedAt: t0.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			Outcome:   "healthy",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := activation.Switch(root, "", "rz_four"); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveTargetState(ctx, state.TargetState{
		CurrentRealizationID:       "rz_four",
		LastKnownGoodRealizationID: "rz_three",
		ObservedAt:                 t0.Add(5 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, client, configPath, root, ids
}

func TestDeploymentsListIncludesBoundTargetWithoutState(t *testing.T) {
	ctx, client, configPath, _, _ := deploymentFixture(t)
	result, err := client.DeploymentsList(ctx, DeploymentsListRequest{AgentConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Targets) != 2 || result.Targets[0].Target != "dev" || result.Targets[1].Target != "new" {
		t.Fatalf("unexpected bound targets: %#v", result.Targets)
	}
	if result.Targets[1].CurrentRealizationID != "" || len(result.Targets[1].Deployments) != 0 {
		t.Fatalf("uninitialized target should be empty: %#v", result.Targets[1])
	}
}

func TestDeploymentsListUsesFilesystemCurrentAndDurableLKG(t *testing.T) {
	ctx, client, configPath, root, _ := deploymentFixture(t)
	result, err := client.DeploymentsList(ctx, DeploymentsListRequest{AgentConfigPath: configPath, Target: "dev"})
	if err != nil {
		t.Fatalf("%v: %v", err, err.(*Error).cause)
	}
	if len(result.Targets) != 1 {
		t.Fatalf("unexpected target count: %#v", result)
	}
	history := result.Targets[0]
	if history.DeployRoot != root || history.CurrentRealizationID != "rz_four" || history.LastKnownGoodRealizationID != "rz_three" {
		t.Fatalf("unexpected target summary: %#v", result)
	}
	if len(history.Deployments) != 4 || history.Deployments[0].RealizationID != "rz_four" || history.Deployments[0].Release != "v4" {
		t.Fatalf("unexpected deployment history: %#v", history.Deployments)
	}
}

func TestDeploymentsPruneIsDryRunByDefaultAndAppliesExactPlan(t *testing.T) {
	ctx, client, configPath, _, ids := deploymentFixture(t)
	req := DeploymentsPruneRequest{AgentConfigPath: configPath, Target: "dev", Keep: 2}
	plan, err := client.DeploymentsPrune(ctx, req)
	if err != nil {
		t.Fatalf("%v: %v", err, err.(*Error).cause)
	}
	if plan.Applied || len(plan.Candidates) != 2 || plan.Candidates[0].RealizationID != ids[0] || plan.Candidates[1].RealizationID != ids[1] {
		t.Fatalf("unexpected dry-run plan: %#v", plan)
	}
	for _, candidate := range plan.Candidates {
		if _, err := os.Stat(candidate.Path); err != nil {
			t.Fatalf("dry run removed %s: %v", candidate.Path, err)
		}
	}
	req.Apply = true
	req.ExpectedPlanID = plan.PlanID
	applied, err := client.DeploymentsPrune(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.PlanID != plan.PlanID {
		t.Fatalf("unexpected apply result: %#v", applied)
	}
	for _, candidate := range plan.Candidates {
		if _, err := os.Stat(candidate.Path); !os.IsNotExist(err) {
			t.Fatalf("applied candidate remains %s: %v", candidate.Path, err)
		}
	}
}

func TestDeploymentsPruneRejectsNegativeKeep(t *testing.T) {
	client, err := New(Options{InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DeploymentsPrune(context.Background(), DeploymentsPruneRequest{Target: "dev", Keep: -1})
	if CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}

func TestDeploymentsPruneResumesReviewedInterruptedPlan(t *testing.T) {
	ctx, client, configPath, root, _ := deploymentFixture(t)
	binding, config, err := loadAgentBinding(configPath, "dev")
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(root, layout.StateDir, "agent.db"), state.Options{Target: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildPrunePlan(ctx, db, binding, config.Spec.SafetyFloors.MinimumRetention, 2)
	if err != nil {
		t.Fatal(err)
	}
	crash := errors.New("simulated prune crash")
	err = prune.Apply(ctx, plan, prune.ApplyOptions{DB: db, MaxDeletionsPerHour: 10, Failpoint: func(point string) error {
		if point == "after-rename" {
			return crash
		}
		return nil
	}})
	if !errors.Is(err, crash) {
		t.Fatalf("failpoint did not fire: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := client.DeploymentsPrune(ctx, DeploymentsPruneRequest{AgentConfigPath: configPath, Target: "dev", Keep: 2, Apply: true, ExpectedPlanID: plan.PlanID})
	if err != nil || !resumed.Applied || resumed.PlanID != plan.PlanID {
		t.Fatalf("resume result=%#v err=%v", resumed, err)
	}
	for _, candidate := range plan.Candidates {
		if _, err := os.Lstat(candidate.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate remains after resume: %s: %v", candidate.Path, err)
		}
	}
}
