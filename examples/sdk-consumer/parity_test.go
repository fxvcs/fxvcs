package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/activation"
	"fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/state"
	"fxvcs.dev/fxvcs/sdk"
)

// The parity test proves the CLI is a presentation adapter: for every
// operation, `fxvcs --json <cmd>` and the SDK consumer produce identical
// results. The consumer (main.go) imports only fxvcs.dev/fxvcs/sdk.

const repoYAML = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_parity_01
  storageDomainID: domain_parity_01
  name: parity
spec:
  profile: generic
  minimumFXVCSVersion: 0.0.0-dev
  gitIntegrationVersion: 1
  storage:
    remotes:
      - name: primary
        type: filesystem
        requiredForPublication: true
`

func setupRepo(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command(git, "init", "-q", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogitconfig"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	_ = os.MkdirAll(filepath.Join(dir, ".fxvcs"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".fxvcs", "repository.yaml"), []byte(repoYAML), 0o644)
	t.Setenv("FXVCS_CACHE_DIR", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", t.TempDir())
	return dir
}

func cliJSON(t *testing.T, dir string, args ...string) map[string]any {
	t.Helper()
	var out, errb bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errb, Getwd: func() (string, error) { return dir, nil }}
	code := cli.Run(context.Background(), env, append([]string{"fxvcs", "--json"}, args...))
	// Diagnostics (doctor/verify) exit non-zero when they report findings but
	// still print the full result; only a missing payload is a failure here.
	if code != 0 && out.Len() == 0 {
		t.Fatalf("cli %v exit %d: %s %s", args, code, out.String(), errb.String())
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("cli json: %v\n%s", err, out.String())
	}
	return m
}

func sdkJSON(t *testing.T, dir, op string) map[string]any {
	t.Helper()
	c, err := sdk.New(sdk.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Dispatch(context.Background(), c, op, dir)
	if err != nil {
		t.Fatalf("sdk %s: %v", op, err)
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func TestCLIAndSDKConsumerAgree(t *testing.T) {
	dir := setupRepo(t)
	// Bind the committed remote so storage list has a real binding.
	_ = cliJSON(t, dir, "storage", "add", "--name", "primary", "--type", "filesystem", "--path", t.TempDir())
	cases := []struct {
		op   string
		args []string
	}{
		{"compat", []string{"compat"}},
		{"storage-list", []string{"storage", "list"}},
		{"objects-status", []string{"objects", "status"}},
		{"cache-status", []string{"cache", "status"}},
		{"status", []string{"status"}},
		{"verify", []string{"verify"}},
		{"doctor", []string{"doctor"}},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			got := sdkJSON(t, dir, c.op)
			want := cliJSON(t, dir, c.args...)
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			if !bytes.Equal(g, w) {
				t.Fatalf("mismatch\nsdk: %s\ncli: %s", g, w)
			}
		})
	}

	t.Run("deployments-list", func(t *testing.T) {
		configPath := setupDeploymentState(t)
		got := sdkJSON(t, configPath, "deployments-list")
		want := cliJSON(t, dir, "deployments", "list", "--agent-config", configPath)
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		if !bytes.Equal(g, w) {
			t.Fatalf("mismatch\nsdk: %s\ncli: %s", g, w)
		}
	})
}

func setupDeploymentState(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := fmt.Sprintf(`apiVersion: fxvcs.dev/v1alpha1
kind: AgentConfig
spec:
  agentID: consumer-test-agent
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
	started := time.Date(2026, 8, 20, 20, 0, 0, 0, time.UTC)
	realizationID := "rz_consumer"
	realizationPath, _ := layout.Realization(root, realizationID)
	if err := os.MkdirAll(realizationPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := db.PutDeployment(ctx, domain.Deployment{
		RealizationID: realizationID,
		Target:        "dev",
		Inputs: domain.RealizationInputs{
			ReleaseName:         "v1",
			EnvironmentName:     "staging",
			EnvironmentRevision: 1,
		},
		Phase: "active-healthy", StartedAt: started.Format(time.RFC3339Nano), Outcome: "healthy",
	}); err != nil {
		t.Fatal(err)
	}
	if err := activation.Switch(root, "", realizationID); err != nil {
		t.Fatal(err)
	}
	if err := db.ObserveTargetState(ctx, state.TargetState{CurrentRealizationID: realizationID, LastKnownGoodRealizationID: realizationID, ObservedAt: started.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// TestConsumerImportsOnlySDK guards the "without importing internal packages"
// exit criterion for the consumer program itself.
func TestConsumerImportsOnlySDK(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(src, []byte("fxvcs.dev/fxvcs/internal")) {
		t.Fatal("consumer imports an internal package")
	}
}
