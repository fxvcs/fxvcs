package sdk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepositoryIdentifyComponentAndStackFromNestedPaths(t *testing.T) {
	requireGit(t)
	c, err := New(Options{InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}

	component := writeRepo(t, "9.0.0", 1)
	got, err := c.RepositoryIdentify(context.Background(), RepositoryIdentifyRequest{
		Path: filepath.Join(component, "sub", "dir"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.RepositoryRoot != canonicalPath(t, component) || got.Kind != RepositoryKindComponent {
		t.Fatalf("component identification = %+v", got)
	}

	stack := initGitWorktree(t)
	if _, err := c.StackInit(context.Background(), StackInitRequest{Path: stack, Name: "test-stack"}); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(stack, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = c.RepositoryIdentify(context.Background(), RepositoryIdentifyRequest{Path: nested})
	if err != nil {
		t.Fatal(err)
	}
	if got.RepositoryRoot != canonicalPath(t, stack) || got.Kind != RepositoryKindStack {
		t.Fatalf("stack identification = %+v", got)
	}
}

func TestRepositoryIdentifyRejectsPlainAndAmbiguousGitWorktrees(t *testing.T) {
	requireGit(t)
	c, _ := New(Options{InstalledVersion: "0.1.0"})

	plain := initGitWorktree(t)
	if _, err := c.RepositoryIdentify(context.Background(), RepositoryIdentifyRequest{Path: plain}); CodeOf(err) != CodeNotRepository {
		t.Fatalf("plain Git error = %v", err)
	}

	ambiguous := writeRepo(t, "0.1.0", 1)
	if err := os.WriteFile(filepath.Join(ambiguous, "fxvcs.yaml"), []byte("not: relevant\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RepositoryIdentify(context.Background(), RepositoryIdentifyRequest{Path: ambiguous}); CodeOf(err) != CodeConflict {
		t.Fatalf("ambiguous repository error = %v", err)
	}
}

func TestRepositoryIdentifyValidatesMarkersAndCancellation(t *testing.T) {
	requireGit(t)
	c, _ := New(Options{InstalledVersion: "0.1.0"})

	stack := initGitWorktree(t)
	if err := os.WriteFile(filepath.Join(stack, "fxvcs.yaml"), []byte("kind: definitely-not-a-stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RepositoryIdentify(context.Background(), RepositoryIdentifyRequest{Path: stack}); CodeOf(err) != CodeValidation {
		t.Fatalf("invalid stack error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.RepositoryIdentify(ctx, RepositoryIdentifyRequest{Path: stack})
	if CodeOf(err) != CodeCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func initGitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "init", "-q", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogitconfig"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}
