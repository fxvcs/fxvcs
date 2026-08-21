package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

const fivemFixtureRoot = "../../spec/fixtures/fivem"

func fivemCheckout(t *testing.T) string {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	src := filepath.Join(fivemFixtureRoot, "collection")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		r.WriteFile(filepath.ToSlash(rel), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".fxvcs/repository.yaml", []byte("apiVersion: fxvcs.dev/v1alpha1\nkind: Repository\nmetadata:\n  repositoryID: repo_cli\n  storageDomainID: domain_cli\n  name: cli\nspec:\n  profile: fivem\n  minimumFXVCSVersion: 0.0.0-dev\n  gitIntegrationVersion: 1\n  sourceRoot: .\n  layout: collection\n  storage:\n    remotes: []\n"))
	r.WriteFile("cfg/server.cfg", []byte("ensure chat\nexec more.cfg\n"))
	r.WriteFile("cfg/more.cfg", []byte("start anticheat\nexec @x/y.cfg\n"))
	r.Commit("init")
	return r.Root
}

func TestResourcesCommands(t *testing.T) {
	dir := fivemCheckout(t)
	t.Setenv("FXVCS_CACHE_DIR", t.TempDir())

	code, out, errOut := run(t, dir, "resources", "sync", "--from-cfg", filepath.Join(dir, "cfg", "server.cfg"))
	if code != ExitOK || !strings.Contains(out, "resource list updated") || !strings.Contains(out, "enabled  chat") || !strings.Contains(out, "started by your server.cfg") || !strings.Contains(out, "warning:") || !strings.Contains(out, "5 resources are undecided") {
		t.Fatalf("sync: %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = run(t, dir, "--json", "resources", "sync")
	if code != ExitOK {
		t.Fatalf("sync json: %d %s", code, out)
	}
	var sync map[string]any
	if err := json.Unmarshal([]byte(out), &sync); err != nil || len(sync["pending"].([]any)) != 5 || sync["written"] != nil {
		t.Fatalf("sync json payload: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".fxvcs", "resources.yaml")); err != nil {
		t.Fatal(err)
	}

	code, out, _ = run(t, dir, "resources", "set", "[maps]/city-map", "--activation", "enabled", "--after", "mapmanager,external")
	if code != ExitOK || !strings.Contains(out, "[maps]/city-map is enabled (saved)") || !strings.Contains(out, "starts after: mapmanager, external") || !strings.Contains(out, "warning:") {
		t.Fatalf("set: %d %s", code, out)
	}
	code, _, errOut = run(t, dir, "resources", "set", "nope", "--activation", "enabled")
	if code != ExitFailure || strings.Contains(errOut, "not-found") || !strings.Contains(errOut, "error: ") || !strings.Contains(errOut, "known paths") {
		t.Fatalf("set unknown: %d %s", code, errOut)
	}
	code, _, errOut = run(t, dir, "--verbose", "resources", "set", "nope", "--activation", "enabled")
	if code != ExitFailure || !strings.Contains(errOut, "code: not-found") {
		t.Fatalf("set unknown verbose: %d %s", code, errOut)
	}
	code, _, errOut = run(t, dir, "resources", "set", "chat", "--activation", "maybe")
	if code != ExitUsage || strings.Contains(errOut, "invalid-argument") || !strings.Contains(errOut, "error: ") {
		t.Fatalf("set bad value: %d %s", code, errOut)
	}
	if code, _, _ := run(t, dir, "resources", "set"); code != ExitUsage {
		t.Fatalf("set without arg: %d", code)
	}

	code, out, _ = run(t, dir, "resources", "validate")
	if code != ExitFailure || !strings.Contains(out, "NOT READY FOR RELEASE") || !strings.Contains(out, "pending") || !strings.Contains(out, "hint: fxvcs resources set") {
		t.Fatalf("validate pending: %d %s", code, out)
	}
	code, out, _ = run(t, dir, "--json", "resources", "validate")
	var v map[string]any
	if code != ExitFailure || json.Unmarshal([]byte(out), &v) != nil || v["valid"] != false || len(v["problems"].([]any)) != 4 {
		t.Fatalf("validate json: %d %s", code, out)
	}
	for _, p := range []string{"[maps]/[interiors]/bank", "mapmanager-fork", "multi-game", "runtime-reqs"} {
		if code, _, e := run(t, dir, "resources", "set", p, "--activation", "disabled"); code != ExitOK {
			t.Fatalf("set %s: %d %s", p, code, e)
		}
	}
	code, out, _ = run(t, dir, "resources", "validate", "--known", "external")
	if code != ExitOK || !strings.Contains(out, "ready for release (7 resources)") {
		t.Fatalf("validate clean: %d %s", code, out)
	}
	// Sandbox violation: exit 1 with the manifest named.
	bad, _ := os.ReadFile(filepath.Join(fivemFixtureRoot, "limits", "forbidden-io.lua"))
	_ = os.MkdirAll(filepath.Join(dir, "evil"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "evil", "fxmanifest.lua"), bad, 0o644)
	code, _, errOut = run(t, dir, "resources", "validate")
	if code != ExitFailure || !strings.Contains(errOut, "evil/fxmanifest.lua") {
		t.Fatalf("validate sandbox: %d %s", code, errOut)
	}
	code, _, errOut = run(t, dir, "resources", "sync")
	if code != ExitFailure || !strings.Contains(errOut, "evil/fxmanifest.lua") {
		t.Fatalf("sync sandbox: %d %s", code, errOut)
	}
}

func TestResourcesCommandsRequireFiveMProfile(t *testing.T) {
	gittest.RequireGit(t)
	dir := repoWithMinimum(t, "0.0.0-dev")
	t.Setenv("FXVCS_CACHE_DIR", t.TempDir())
	code, _, errOut := run(t, dir, "resources", "sync")
	if code != ExitUsage || !strings.Contains(errOut, "error: this is not a FiveM repository (its profile is \"generic\")") || !strings.Contains(errOut, "hint: set one up with: fxvcs init --profile fivem") {
		t.Fatalf("%d %s", code, errOut)
	}
}
