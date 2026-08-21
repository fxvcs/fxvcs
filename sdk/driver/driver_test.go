package driver_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/sdk"
	"fxvcs.dev/fxvcs/sdk/driver"
)

// TestMain is the pattern this package exists to make available to downstream
// modules: Git launches this test binary as the driver, and the invocation is
// delegated back into the CLI instead of re-running the suite.
func TestMain(m *testing.M) {
	if code, ok := driver.RunFromEnv(context.Background()); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// TestDelegatedDriverCleansThroughGit drives a real repository end to end with
// the test binary installed as the launcher: `git add` must run the delegated
// clean filter and stage a pointer rather than the file bytes.
func TestDelegatedDriverCleansThroughGit(t *testing.T) {
	gittest.RequireGit(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	repo := gittest.InitRepo(t, driver.EnvVar+"=1", cache.EnvDir+"="+cacheDir)
	// The git invocations the SDK makes in-process inherit this environment,
	// and so do the drivers Git spawns from them.
	for _, kv := range repo.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL",
			"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
			driver.EnvVar, cache.EnvDir:
			t.Setenv(k, v)
		}
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "local"))

	c, err := sdk.New(sdk.Options{CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Init(ctx, sdk.InitRequest{Path: repo.Root, Name: "driver-test", Profile: "generic", LauncherPath: exe}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := c.Track(ctx, sdk.TrackRequest{Path: repo.Root, Selector: sdk.PathSelector{Paths: []string{"*.bin"}, Glob: true}}); err != nil {
		t.Fatalf("track: %v", err)
	}

	payload := bytes.Repeat([]byte("fxvcs-driver-entrypoint"), 512)
	repo.WriteFile("asset.bin", payload)
	repo.Git("add", "asset.bin")

	staged := repo.Git("show", ":asset.bin")
	if !pointer.Sniff([]byte(staged)) {
		t.Fatalf("git add staged raw bytes, so the delegated clean filter never ran:\n%.200s", staged)
	}
	// repo.Git trims stdout; the pointer format requires the final newline.
	if _, err := pointer.Decode([]byte(staged + "\n")); err != nil {
		t.Fatalf("staged blob is not a valid pointer: %v", err)
	}

	// And the smudge path: a fresh checkout of the committed pointer must
	// restore the original bytes through the delegated driver.
	repo.Commit("add asset")
	if err := os.Remove(filepath.Join(repo.Root, "asset.bin")); err != nil {
		t.Fatal(err)
	}
	repo.Git("checkout", "--", "asset.bin")
	if got := repo.ReadFile("asset.bin"); !bytes.Equal(got, payload) {
		t.Fatalf("checkout restored %d bytes, want %d", len(got), len(payload))
	}
}

// TestRunReportsExitStatus checks Run returns the CLI exit contract rather
// than exiting the process.
func TestRunReportsExitStatus(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := driver.Run(context.Background(), []string{"no-such-driver"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatalf("unknown command returned 0; stderr=%q", stderr.String())
	}
}

func TestIsDriverCommand(t *testing.T) {
	for _, name := range []string{"filter-process", "clean-filter", "smudge-filter", "diff-driver", "merge-driver", "pre-push"} {
		if !driver.IsDriverCommand(name) {
			t.Errorf("IsDriverCommand(%q) = false, want true", name)
		}
	}
	if driver.IsDriverCommand("-test.run") {
		t.Error("IsDriverCommand(\"-test.run\") = true; test flags must not be mistaken for driver commands")
	}
	if len(driver.Commands()) == 0 {
		t.Error("Commands() is empty")
	}
}
