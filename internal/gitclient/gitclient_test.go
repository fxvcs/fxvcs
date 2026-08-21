package gitclient_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

func TestRevParseHelpers(t *testing.T) {
	r := gittest.InitRepo(t)
	ctx := context.Background()
	top, err := r.Client.TopLevel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(t, top, r.Root) {
		t.Fatalf("toplevel %q != root %q", top, r.Root)
	}
	gd, err := r.Client.GitDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(gd) || filepath.Base(gd) != ".git" {
		t.Fatalf("git dir %q", gd)
	}
	cd, err := r.Client.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(t, cd, gd) {
		t.Fatalf("common dir %q != git dir %q", cd, gd)
	}
	fx, err := r.Client.GitPath(ctx, "fxvcs")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(fx) || filepath.Base(fx) != "fxvcs" {
		t.Fatalf("git-path fxvcs = %q", fx)
	}
	hooks, err := r.Client.GitPath(ctx, "hooks")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(hooks) != "hooks" {
		t.Fatalf("hooks = %q", hooks)
	}
}

func TestConfigAndTypedErrors(t *testing.T) {
	r := gittest.InitRepo(t)
	ctx := context.Background()
	if err := r.Client.ConfigSet(ctx, "filter.fxvcs.required", "true"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := r.Client.ConfigGet(ctx, "filter.fxvcs.required")
	if err != nil || !ok || v != "true" {
		t.Fatalf("get: %q %v %v", v, ok, err)
	}
	_, ok, err = r.Client.ConfigGet(ctx, "filter.fxvcs.absent")
	if err != nil || ok {
		t.Fatalf("absent key: ok=%v err=%v", ok, err)
	}
	if err := r.Client.ConfigUnset(ctx, "filter.fxvcs.required"); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.ConfigUnset(ctx, "filter.fxvcs.required"); err != nil {
		t.Fatalf("unset absent should be nil: %v", err)
	}

	_, err = r.Client.Run(ctx, "rev-parse", "--verify", "definitely-not-a-ref")
	var ge *gitclient.Error
	if !errors.As(err, &ge) || ge.ExitCode == 0 || ge.Stderr == "" {
		t.Fatalf("expected typed error with stderr, got %v", err)
	}

	outside := gitclient.New(t.TempDir()).WithEnv(r.Env)
	_, err = outside.TopLevel(ctx)
	if !errors.Is(err, gitclient.ErrNotRepository) {
		t.Fatalf("expected ErrNotRepository, got %v", err)
	}

	missing := &gitclient.Client{Executable: filepath.Join(t.TempDir(), "no-such-git")}
	if missing.Available() {
		t.Fatal("missing git reported available")
	}
	_, err = missing.Version(ctx)
	if !errors.Is(err, gitclient.ErrGitNotFound) {
		t.Fatalf("expected ErrGitNotFound, got %v", err)
	}
}

func TestCheckAttrHashLsFilesCatFile(t *testing.T) {
	r := gittest.InitRepo(t)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	r.WriteFile("a.bin", []byte("hello\n"))
	r.WriteFile("b.txt", []byte("text\n"))
	r.Git("add", ".")

	a, err := r.Client.CheckAttr(ctx, "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if a != (gitclient.Attrs{Filter: "fxvcs", Diff: "fxvcs", Merge: "fxvcs", Text: "unset"}) || !a.IsManaged() {
		t.Fatalf("attrs %+v", a)
	}
	m, err := r.Client.CheckAttrs(ctx, []string{"a.bin", "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if m["b.txt"].IsManaged() || m["b.txt"].Filter != "unspecified" {
		t.Fatalf("b.txt attrs %+v", m["b.txt"])
	}

	oid, err := r.Client.HashObject(ctx, strings.NewReader("hello\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	if oid != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("hash-object = %s", oid)
	}
	entries, err := r.Client.LsFilesStage(ctx, "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Mode != "100644" || entries[0].OID != oid || entries[0].Stage != 0 || entries[0].Path != "a.bin" {
		t.Fatalf("ls-files --stage: %+v", entries)
	}
	blob, err := r.Client.CatFileBlob(ctx, ":a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "hello\n" {
		t.Fatalf("cat-file = %q", blob)
	}

	c := r.Commit("one")
	paths, err := r.Client.DiffTreePaths(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != ".gitattributes,a.bin,b.txt" {
		t.Fatalf("diff-tree paths %v", paths)
	}
	commits, err := r.Client.RevList(ctx, "HEAD")
	if err != nil || len(commits) != 1 || commits[0] != c {
		t.Fatalf("rev-list %v %v", commits, err)
	}
}

func TestBareRemoteAndClone(t *testing.T) {
	r := gittest.InitRepo(t)
	r.WriteFile("f.txt", []byte("x"))
	r.Commit("init")
	bare := gittest.BareRemote(t)
	r.Git("remote", "add", "origin", bare)
	r.Git("push", "-q", "origin", "HEAD:main")
	c := r.Clone(bare)
	if string(c.ReadFile("f.txt")) != "x" {
		t.Fatal("clone content mismatch")
	}
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb))
}

// TestRunBatchedSurvivesAnArgvThatWouldNotFit is the regression this exists
// for: a repository with thousands of managed paths produces a command line
// that the operating system refuses ("argument list too long"), and every
// Git call built from a repository-sized path list must go through batching.
func TestRunBatchedSurvivesAnArgvThatWouldNotFit(t *testing.T) {
	r := gittest.InitRepo(t)
	r.WriteFile("tracked.txt", []byte("x"))
	r.Commit("one file")
	ctx := context.Background()

	pathspecs := make([]string, 0, 30000)
	total := 0
	for i := 0; i < 30000; i++ {
		p := fmt.Sprintf(":(literal)resources/pack%05d/stream/asset%05d.ytd", i/100, i)
		pathspecs = append(pathspecs, p)
		total += len(p) + 1
	}
	if total < 1<<20 {
		t.Fatalf("the fixture must exceed a 1 MiB argv to be meaningful; it is %d bytes", total)
	}
	prefix := []string{"ls-files", "--stage", "-z", "--"}
	// One command holding all of them is what used to happen.
	if _, err := r.Client.Run(ctx, append(append([]string{}, prefix...), pathspecs...)...); err == nil {
		t.Skip("this platform accepted a 1 MiB argv; the batching guard cannot be demonstrated here")
	}
	// Batched, the same query runs and reports nothing, because none of those
	// paths are in the index.
	out, err := r.Client.RunBatched(ctx, prefix, pathspecs)
	if err != nil {
		t.Fatalf("batched run failed: %v", err)
	}
	if n := len(bytes.Trim(out, "\x00")); n != 0 {
		t.Fatalf("unexpected output (%d bytes): %q", n, out)
	}
}

// TestRunRetryWaitsOutABusyIndex covers the collision FXVCS lives with by
// design: no daemon arbitrates between the CLI, the desktop application and
// the editor, so two of them writing the index at once is ordinary. The
// holder releases in milliseconds, and the only thing a caller has to do is
// be willing to ask again.
func TestRunRetryWaitsOutABusyIndex(t *testing.T) {
	r := gittest.InitRepo(t)
	r.WriteFile("tracked.txt", []byte("x"))
	r.Commit("one file")
	ctx := context.Background()

	gitDir, err := r.Client.GitDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(gitDir, "index.lock")
	held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_ = held.Close()

	args := []string{"update-index", "--refresh", "--", "tracked.txt"}
	// One attempt against a held index fails, and it fails in the way the
	// classifier has to recognise.
	_, err = r.Client.Run(ctx, args...)
	if err == nil {
		t.Skip("this git did not need the index lock; contention cannot be demonstrated here")
	}
	if !gitclient.IsLockContention(err) {
		t.Fatalf("a held index.lock was not classified as contention: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.Remove(lockPath)
	}()
	if _, err := r.Client.RunRetry(ctx, gitclient.DefaultRetry, args...); err != nil {
		t.Fatalf("retry did not survive a lock released mid-wait: %v", err)
	}
}

// TestIsLockContentionIgnoresOrdinaryFailures keeps the classifier narrow: a
// command that failed for a reason retrying cannot fix must fail at once,
// because a retried mistake is the same mistake plus a delay.
func TestIsLockContentionIgnoresOrdinaryFailures(t *testing.T) {
	r := gittest.InitRepo(t)
	ctx := context.Background()
	_, err := r.Client.Run(ctx, "cat-file", "-p", "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if gitclient.IsLockContention(err) {
		t.Fatalf("an ordinary failure was classified as lock contention: %v", err)
	}
	if gitclient.IsLockContention(nil) {
		t.Fatal("nil is not contention")
	}
}
