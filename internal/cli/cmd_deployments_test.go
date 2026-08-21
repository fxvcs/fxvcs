package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/state"
	"fxvcs.dev/fxvcs/sdk"
)

func cliDeploymentFixture(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := fmt.Sprintf(`apiVersion: fxvcs.dev/v1alpha1
kind: AgentConfig
spec:
  agentID: cli-test-agent
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
  safetyFloors:
    minimumRetention: 2
    maxDeletionsPerHour: 10
    maxAutomaticRollbacks: 1
    disableAutomaticRollback: false
`, root, filepath.Join(t.TempDir(), "server"))
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.OpenAgent(ctx, filepath.Join(root, layout.StateDir, "agent.db"), state.Options{Create: true, Target: "dev", InstalledVersion: sdk.Version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t0 := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	for i, id := range []string{"rz_one", "rz_two", "rz_three", "rz_four"} {
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
	if err := db.ObserveTargetState(ctx, state.TargetState{CurrentRealizationID: "rz_four", LastKnownGoodRealizationID: "rz_three", ObservedAt: t0.Add(5 * time.Minute).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestDeploymentsCLIJSONParity(t *testing.T) {
	configPath := cliDeploymentFixture(t)
	code, out, errout := run(t, t.TempDir(), "--json", "deployments", "list", "--agent-config", configPath)
	if code != 0 {
		t.Fatalf("list failed: code=%d stderr=%s", code, errout)
	}
	var listed sdk.DeploymentsListResult
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("invalid list JSON: %v\n%s", err, out)
	}
	if len(listed.Targets) != 1 || listed.Targets[0].Target != "dev" || listed.Targets[0].CurrentRealizationID != "rz_four" || len(listed.Targets[0].Deployments) != 4 {
		t.Fatalf("unexpected list result: %#v", listed)
	}

	code, out, errout = run(t, t.TempDir(), "--json", "deployments", "prune", "dev", "--agent-config", configPath, "--keep", "2")
	if code != 0 {
		t.Fatalf("prune failed: code=%d stderr=%s", code, errout)
	}
	var planned sdk.DeploymentsPruneResult
	if err := json.Unmarshal([]byte(out), &planned); err != nil {
		t.Fatalf("invalid prune JSON: %v\n%s", err, out)
	}
	if planned.Applied || len(planned.Candidates) != 2 || planned.PlanID == "" {
		t.Fatalf("unexpected prune result: %#v", planned)
	}
}
