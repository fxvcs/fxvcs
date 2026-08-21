package filter_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/cli/drivers"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/sdk"
)

// driverEnv makes the test binary behave as the fxvcs CLI when Git launches
// it as filter/diff/merge driver or hook. This avoids a build step and works
// on every OS: os.Executable() is configured as the launcher.
const driverEnv = "FXVCS_TEST_DRIVER"

func TestMain(m *testing.M) {
	if os.Getenv(driverEnv) == "1" {
		args := append([]string{"fxvcs"}, os.Args[1:]...)
		env := cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Extra: drivers.Commands()}
		os.Exit(cli.Run(context.Background(), env, args))
	}
	os.Exit(m.Run())
}

const testDomain = "domain_test_01"

func repositoryYAML(minimum string) string {
	return "apiVersion: fxvcs.dev/v1alpha1\nkind: Repository\nmetadata:\n  repositoryID: repo_test_01\n  storageDomainID: " + testDomain + "\n  name: test\nspec:\n  profile: generic\n  minimumFXVCSVersion: \"" + minimum + "\"\n  gitIntegrationVersion: 1\n  storage:\n    remotes:\n      - name: primary\n        type: filesystem\n        endpoint: ./does-not-exist-yet\n        requiredForPublication: true\n"
}

// fixture is a configured repository with the filter installed.
type fixture struct {
	repo     *gittest.Repo
	cacheDir string
	cache    *cache.Cache
	launcher string
	storeDir string // filesystem remote bound as "primary" (lazily)
}

// isolateProcessGit makes git invocations started by this process (the SDK
// engine) use the fixture's isolated environment, exactly like the drivers
// Git re-executes.
func isolateProcessGit(t *testing.T, r *gittest.Repo) {
	t.Helper()
	for _, kv := range r.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL",
			"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
			driverEnv, cache.EnvDir: // drivers spawned by git inherit these
			t.Setenv(k, v)
		}
	}
}

// client returns an SDK client bound to the fixture cache.
func (f *fixture) client(t *testing.T) *sdk.Client {
	t.Helper()
	isolateProcessGit(t, f.repo)
	c, err := sdk.New(sdk.Options{CacheDir: f.cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// publish binds the committed "primary" remote to a filesystem directory and
// publishes every pending object through the engine (fxvcs objects publish).
func (f *fixture) publish(t *testing.T) {
	t.Helper()
	c := f.client(t)
	ctx := context.Background()
	if f.storeDir == "" {
		f.storeDir = t.TempDir()
		if _, err := c.StorageAdd(ctx, sdk.StorageAddRequest{Path: f.repo.Root, Remote: sdk.StorageRemoteSpec{Name: "primary", Type: "filesystem", LocalPath: f.storeDir}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.ObjectsPublish(ctx, sdk.ObjectsPublishRequest{Path: f.repo.Root})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("publish: %v %+v", err, res)
	}
}

// bindPrimary binds the committed "primary" remote to a filesystem directory
// without publishing anything.
func (f *fixture) bindPrimary(t *testing.T) {
	t.Helper()
	if f.storeDir != "" {
		return
	}
	c := f.client(t)
	f.storeDir = t.TempDir()
	if _, err := c.StorageAdd(context.Background(), sdk.StorageAddRequest{Path: f.repo.Root, Remote: sdk.StorageRemoteSpec{Name: "primary", Type: "filesystem", LocalPath: f.storeDir}}); err != nil {
		t.Fatal(err)
	}
}

// storeHas reports whether the bound filesystem remote holds digest.
func (f *fixture) storeHas(t *testing.T, digest string) bool {
	t.Helper()
	if f.storeDir == "" {
		return false
	}
	hexPart := strings.TrimPrefix(digest, "sha256:")
	_, err := os.Stat(filepath.Join(f.storeDir, testDomain, "objects", hexPart[:2], hexPart))
	return err == nil
}

// cache opens the fixture's object cache.
func (f *fixture) objectCache() *cache.Cache {
	c, _ := cache.Open(f.cacheDir)
	return c
}

func setup(t *testing.T, minimum string) *fixture {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	r := gittest.InitRepo(t, driverEnv+"=1", cache.EnvDir+"="+cacheDir)
	if err := filter.Install(context.Background(), r.Client, exe); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".fxvcs/repository.yaml", []byte(repositoryYAML(minimum)))
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	c, err := cache.Open(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{repo: r, cacheDir: cacheDir, cache: c, launcher: exe}
}

func randomAsset(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// expectedPointer recomputes, independently of the engine, the pointer the
// default encoding policy must produce for this content. Small assets stay
// whole/1; anything at or above the chunking floor is split with
// cdc-fastcdc/1, so the test proves the staged bytes against a second
// implementation of the same rules rather than against the engine's own
// output.
func expectedPointer(t *testing.T, content []byte) (pointer.Pointer, []byte) {
	t.Helper()
	p, _, err := expectedContent(content)
	if err != nil {
		t.Fatal(err)
	}
	return p, p.MustEncode()
}

// expectedContent also returns every object digest the pointer depends on, in
// the order Content reports them (manifest first for a chunked encoding).
func expectedContent(content []byte) (pointer.Pointer, []string, error) {
	sum := sha256.Sum256(content)
	oid := cache.Digest(sum[:])
	policy := chunk.DefaultPolicy()
	if policy.Encoding("a.bin", int64(len(content))) == chunk.EncodingWhole {
		p, err := filter.WholePointer(testDomain, oid, int64(len(content)))
		return p, []string{oid}, err
	}
	var chunks []domain.ManifestChunk
	digests := []string{}
	err := chunk.Split(bytes.NewReader(content), chunk.FastCDCv1, func(c chunk.Chunk) error {
		s := sha256.Sum256(c.Data)
		d := cache.Digest(s[:])
		chunks = append(chunks, domain.ManifestChunk{Digest: d, RawSize: int64(len(c.Data)), EncodedSize: int64(len(c.Data))})
		digests = append(digests, d)
		return nil
	})
	if err != nil {
		return pointer.Pointer{}, nil, err
	}
	man := domain.ObjectManifest{
		FormatVersion:   format.ObjectManifestVersion,
		StorageDomainID: testDomain,
		Encoding:        chunk.EncodingFastCDCv1,
		Compression:     "none",
		OID:             oid,
		Size:            int64(len(content)),
		Chunks:          chunks,
	}
	md, err := integrity.ManifestDigest(man)
	if err != nil {
		return pointer.Pointer{}, nil, err
	}
	p := pointer.Pointer{StorageDomainID: testDomain, Encoding: chunk.EncodingFastCDCv1, ManifestDigest: md, OID: oid, Size: int64(len(content))}
	return p, append([]string{md}, digests...), p.Validate()
}

func TestGitAddStagesDeterministicPointerOffline(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	// No network is available to the filter: nothing in the process points at
	// a remote, and the configured storage remote does not exist.
	r.SetEnv("HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "NO_PROXY=")
	asset := randomAsset(3<<20, 42)
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")

	want, wantBytes := expectedPointer(t, asset)
	staged := r.Git("cat-file", "-p", ":a.bin") + "\n"
	if staged != string(wantBytes) {
		t.Fatalf("staged blob is not the expected pointer:\n%s", staged)
	}
	got, err := pointer.Decode([]byte(staged))
	if err != nil || got != want {
		t.Fatalf("decode staged: %+v %v", got, err)
	}
	// Re-adding yields identical bytes (determinism); touch mtime to force
	// a re-clean.
	r.WriteFile("a.bin", asset)
	r.Git("add", "a.bin")
	if r.Git("cat-file", "-p", ":a.bin")+"\n" != string(wantBytes) {
		t.Fatal("re-add produced different pointer bytes")
	}
	// Every object behind the pointer — for this size, a manifest and its
	// chunks — is cached and pinned pending publication.
	_, objects, err := expectedContent(asset)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) < 2 {
		t.Fatalf("a %d-byte asset should be chunked by the default policy, got %d object(s)", len(asset), len(objects))
	}
	var cachedBytes int64
	for _, d := range objects {
		info, err := f.cache.Stat(testDomain, d)
		if err != nil || !info.Pinned {
			t.Fatalf("cache %s: %+v %v", d, info, err)
		}
		if err := f.cache.Verify(testDomain, d); err != nil {
			t.Fatal(err)
		}
		cachedBytes += info.Size
	}
	if cachedBytes < int64(len(asset)) {
		t.Fatalf("cached objects total %d bytes for a %d-byte asset", cachedBytes, len(asset))
	}
	r.Commit("add asset")

	// The working tree still holds the real bytes; status is clean.
	if st := r.Git("status", "--porcelain"); st != "" {
		t.Fatalf("status not clean after add: %q", st)
	}

	// Smudge from cache: remove and check out -> byte-identical.
	if err := os.Remove(filepath.Join(r.Root, "a.bin")); err != nil {
		t.Fatal(err)
	}
	r.Git("checkout", "--", "a.bin")
	if !bytes.Equal(r.ReadFile("a.bin"), asset) {
		t.Fatal("checkout did not reproduce the asset from cache")
	}

	// Diff driver runs through git: compare HEAD pointer with a new version.
	r.WriteFile("a.bin", randomAsset(1<<20, 7))
	out := r.Git("diff", "--", "a.bin")
	if !strings.Contains(out, "fxvcs pointer diff: a.bin") || !strings.Contains(out, "content changed") || !strings.Contains(out, want.OID) {
		t.Fatalf("diff driver output:\n%s", out)
	}
	r.Git("checkout", "--", "a.bin")

	// Fresh clone with the filter configured but an empty cache: the
	// unhydrated default checks out pointer bytes rather than failing.
	bare := gittest.BareRemote(t)
	r.Git("remote", "add", "origin", bare)
	f.publish(t)
	r.Git("push", "-q", "origin", "HEAD:main")
	emptyCache := t.TempDir()
	r.SetEnv(cache.EnvDir + "=" + emptyCache)
	var cloneArgs []string
	for _, e := range filter.GitConfigEntries(f.launcher) {
		cloneArgs = append(cloneArgs, "-c", e.Key+"="+e.Value)
	}
	clone := r.Clone(bare, cloneArgs...)
	if got := clone.ReadFile("a.bin"); !bytes.Equal(got, wantBytes) {
		t.Fatalf("fresh clone with empty cache should hold pointer bytes, got %d bytes", len(got))
	}
	if st := clone.Git("status", "--porcelain"); st != "" {
		t.Fatalf("clone status not clean (pointer pass-through must be idempotent): %q", st)
	}
	// Even with the host cache populated, a checkout without recorded intent
	// keeps the pointer: hydration is an explicit choice, never a side effect
	// of what happens to be cached.
	clone.SetEnv(cache.EnvDir + "=" + f.cacheDir)
	os.Remove(filepath.Join(clone.Root, "a.bin"))
	clone.Git("checkout", "--", "a.bin")
	if !bytes.Equal(clone.ReadFile("a.bin"), wantBytes) {
		t.Fatal("clone hydrated implicitly from the host cache")
	}
	// fxvcs hydrate materializes it (from cache here) and records the intent;
	// a later checkout then reproduces the bytes instead of the pointer.
	c := f.client(t)
	res, err := c.Hydrate(context.Background(), sdk.HydrateRequest{Path: clone.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}})
	if err != nil || len(res.Items) != 1 || res.Items[0].Skipped {
		t.Fatalf("hydrate: %v %+v", err, res)
	}
	if !bytes.Equal(clone.ReadFile("a.bin"), asset) {
		t.Fatal("hydrate did not reproduce the asset")
	}
	if st := clone.Git("status", "--porcelain"); st != "" {
		t.Fatalf("clone status not clean after hydrate: %q", st)
	}
	if res, err := c.Dehydrate(context.Background(), sdk.DehydrateRequest{Path: clone.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil || res.Items[0].Skipped {
		t.Fatalf("dehydrate: %v %+v", err, res)
	}
	if st := clone.Git("status", "--porcelain"); st != "" || !bytes.Equal(clone.ReadFile("a.bin"), wantBytes) {
		t.Fatalf("clone after dehydrate: status %q", st)
	}
	if _, err := c.Hydrate(context.Background(), sdk.HydrateRequest{Path: clone.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(clone.Root, "a.bin"))
	clone.Git("checkout", "--", "a.bin")
	if !bytes.Equal(clone.ReadFile("a.bin"), asset) {
		t.Fatal("checkout of a hydrated path did not reproduce the asset")
	}
	// Modify the hydrated file and stage it with ordinary Git: a new pointer
	// is produced and its object is pending publication in the clone.
	modified := append(append([]byte(nil), asset...), []byte("tail")...)
	clone.WriteFile("a.bin", modified)
	clone.Git("add", "a.bin")
	newPtr, newBytes := expectedPointer(t, modified)
	if got, _ := clone.Client.CatFileBlob(context.Background(), ":a.bin"); !bytes.Equal(got, newBytes) {
		t.Fatalf("modified asset not staged as the expected pointer:\n%s", got)
	}
	objs, err := c.ObjectsStatus(context.Background(), sdk.ObjectsStatusRequest{Path: clone.Root})
	if err != nil {
		t.Fatal(err)
	}
	_, newObjects, err := expectedContent(modified)
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, o := range objs.Objects {
		if o.Pending {
			pending[o.Digest] = true
		}
	}
	for _, d := range newObjects {
		if !pending[d] {
			t.Fatalf("object %s behind the new pointer %s is not pending in the clone ledger: %+v", d, newPtr.OID, objs.Objects)
		}
	}
	stat, err := c.Status(context.Background(), sdk.StatusRequest{Path: clone.Root})
	if err != nil || len(stat.Entries) != 1 || stat.Entries[0].State != sdk.AssetHydratedClean || !stat.Entries[0].Pending {
		t.Fatalf("clone status after add: %v %+v", err, stat)
	}
	clone.Git("checkout", "HEAD", "--", "a.bin") // back to the committed version (from cache)
	if !bytes.Equal(clone.ReadFile("a.bin"), asset) {
		t.Fatal("checkout HEAD did not restore the committed asset")
	}
	// With the object gone from every source, checkout of the hydrated path
	// fails instead of quietly writing a pointer.
	clone.SetEnv(cache.EnvDir + "=" + t.TempDir())
	os.Remove(filepath.Join(clone.Root, "a.bin"))
	if _, stderr, err := clone.GitErr("checkout", "--", "a.bin"); err == nil || !strings.Contains(stderr, "fxvcs load a.bin") {
		t.Fatalf("hydrated path with unavailable object: err=%v stderr=%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(clone.Root, "a.bin")); err == nil {
		t.Fatal("a file appeared although the smudge failed")
	}
}

func TestOneShotFilters(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	asset := randomAsset(70000, 3)
	_, wantBytes := expectedPointer(t, asset)
	run := func(stdin []byte, args ...string) ([]byte, string, error) {
		cmd := exec.Command(f.launcher, args...)
		cmd.Dir = r.Root
		cmd.Env = r.Env
		cmd.Stdin = bytes.NewReader(stdin)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		out, err := cmd.Output()
		return out, errb.String(), err
	}
	out, errs, err := run(asset, "clean-filter", "a.bin")
	if err != nil || !bytes.Equal(out, wantBytes) {
		t.Fatalf("clean-filter: %v out=%q err=%q", err, out, errs)
	}
	out, errs, err = run(wantBytes, "smudge-filter", "a.bin")
	if err != nil || !bytes.Equal(out, asset) {
		t.Fatalf("smudge-filter: %v len=%d err=%q", err, len(out), errs)
	}
	// Non-pointer input to smudge passes through; pointer input to clean too.
	out, _, err = run([]byte("raw"), "smudge-filter", "a.bin")
	if err != nil || string(out) != "raw" {
		t.Fatalf("smudge pass-through: %v %q", err, out)
	}
}

func TestUpgradeRequiredRefusesFilter(t *testing.T) {
	f := setup(t, "99.0.0")
	r := f.repo
	r.WriteFile("a.bin", []byte("asset"))
	_, stderr, err := r.GitErr("add", "a.bin")
	if err == nil {
		t.Fatal("git add succeeded although the repository requires a newer fxvcs")
	}
	for _, want := range []string{"upgrade", "0.0.0-dev", "99.0.0", "fxvcs upgrade --check"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// Nothing was staged and nothing was ingested.
	if out := r.Git("ls-files", "--stage", "a.bin"); out != "" {
		t.Fatalf("a.bin staged despite refusal: %s", out)
	}
	if n, _ := f.cache.TotalBytes(); n != 0 {
		t.Fatalf("cache received %d bytes despite refusal", n)
	}
	// Non-attributed files still work through Git.
	r.WriteFile("plain.txt", []byte("ok"))
	r.Git("add", "plain.txt")
}

func TestAutoCRLFDoesNotAlterPointer(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	r.Git("config", "core.autocrlf", "true")
	asset := []byte("line1\nline2\r\nline3\n" + string(randomAsset(5000, 9)))
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")
	_, wantBytes := expectedPointer(t, asset)
	staged, err := r.Client.CatFileBlob(context.Background(), ":a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, wantBytes) {
		t.Fatalf("pointer bytes altered under core.autocrlf=true:\n%q", staged)
	}
	if bytes.Contains(staged, []byte("\r")) {
		t.Fatal("pointer contains CR")
	}
	r.Commit("crlf")
	os.Remove(filepath.Join(r.Root, "a.bin"))
	r.Git("checkout", "--", "a.bin")
	if !bytes.Equal(r.ReadFile("a.bin"), asset) {
		t.Fatal("asset bytes altered on checkout under autocrlf (-text must win)")
	}
}

func TestPrePushBlocksUntilPublished(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	if _, err := filter.InstallPrePushHook(context.Background(), r.Client, f.launcher); err != nil {
		t.Fatal(err)
	}
	asset := randomAsset(100000, 5)
	r.WriteFile("a.bin", asset)
	r.WriteFile("notes.txt", []byte("plain"))
	r.Commit("asset")
	want, _ := expectedPointer(t, asset)
	bare := gittest.BareRemote(t)
	r.Git("remote", "add", "origin", bare)

	// The required remote is not bound on this machine: nothing can be
	// published, so the push is refused with the digest, remote, and remedy.
	_, stderr, err := r.GitErr("push", "-q", "origin", "HEAD:main")
	if err == nil {
		t.Fatal("push succeeded while the object could not be published")
	}
	if !strings.Contains(stderr, want.OID) || !strings.Contains(stderr, "primary") || !strings.Contains(stderr, "storage add") {
		t.Fatalf("pre-push message lacks digest/remote/remedy:\n%s", stderr)
	}
	// Bare remote must not have received the ref.
	c := r.Client.WithDir(bare)
	if out, _ := c.RunString(context.Background(), "rev-parse", "--verify", "-q", "refs/heads/main"); out != "" {
		t.Fatalf("remote ref exists after rejected push: %s", out)
	}
	// Bind the remote: git push now publishes automatically and succeeds
	// without a separate `fxvcs objects publish`.
	f.bindPrimary(t)
	_, stderr, err = r.GitErr("push", "-q", "origin", "HEAD:main")
	if err != nil {
		t.Fatalf("push failed after binding the remote: %v\n%s", err, stderr)
	}
	if out, _ := c.RunString(context.Background(), "rev-parse", "--verify", "-q", "refs/heads/main"); out == "" {
		t.Fatal("push did not land after automatic publication")
	}
	if !f.storeHas(t, want.OID) {
		t.Fatal("object not uploaded to the required remote by pre-push")
	}
	if pinned, _ := f.objectCache().IsPinned(testDomain, want.OID); pinned {
		t.Fatal("pin not settled after pre-push publication")
	}
	// A second commit with only plain files pushes without complaint; a
	// second asset is published on push as well (existing remote ref path).
	r.WriteFile("notes.txt", []byte("more"))
	r.Commit("plain")
	r.Git("push", "-q", "origin", "HEAD:main")
	second := randomAsset(1000, 6)
	r.WriteFile("b.bin", second)
	r.Commit("second asset")
	want2, _ := expectedPointer(t, second)
	r.Git("push", "-q", "origin", "HEAD:main")
	if !f.storeHas(t, want2.OID) {
		t.Fatal("second object not published on push")
	}
	// Deleting a remote branch never runs the barrier against local objects.
	r.Git("push", "-q", "origin", "HEAD~2:refs/heads/tmp")
	r.Git("push", "-q", "origin", ":refs/heads/tmp")
}

func TestMergeDriverConflictThroughGit(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	base := randomAsset(4000, 1)
	ours := randomAsset(4000, 2)
	theirs := randomAsset(4000, 3)
	r.WriteFile("a.bin", base)
	r.Commit("base")
	r.Git("checkout", "-q", "-b", "theirs")
	r.WriteFile("a.bin", theirs)
	r.Commit("theirs")
	r.Git("checkout", "-q", "main")
	r.WriteFile("a.bin", ours)
	r.Commit("ours")

	stdout, stderr, err := r.GitErr("merge", "--no-edit", "theirs")
	if err == nil {
		t.Fatal("merge of two changed assets did not conflict")
	}
	if !strings.Contains(stdout+stderr, "CONFLICT") || !strings.Contains(stderr, "both pointer candidates") {
		t.Fatalf("expected CONFLICT and the driver explanation:\n%s\n%s", stdout, stderr)
	}
	entries, err := r.Client.LsFilesStage(context.Background(), "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 unmerged stages, got %+v", entries)
	}
	oursP, _ := expectedPointer(t, ours)
	theirsP, _ := expectedPointer(t, theirs)
	baseP, _ := expectedPointer(t, base)
	wantByStage := map[int]pointer.Pointer{1: baseP, 2: oursP, 3: theirsP}
	for _, e := range entries {
		blob, err := r.Client.CatFileBlob(context.Background(), e.OID)
		if err != nil {
			t.Fatal(err)
		}
		p, err := pointer.Decode(blob)
		if err != nil {
			t.Fatalf("stage %d blob is not a valid pointer: %v", e.Stage, err)
		}
		if p != wantByStage[e.Stage] {
			t.Fatalf("stage %d pointer mismatch", e.Stage)
		}
	}
	// The working file holds both candidates in the documented layout.
	wf := r.ReadFile("a.bin")
	if !bytes.HasPrefix(wf, []byte(filter.ConflictOurs)) || !bytes.Contains(wf, []byte(filter.ConflictSep)) || !bytes.HasSuffix(wf, []byte(filter.ConflictTheirs)) {
		t.Fatalf("conflict layout not in working file:\n%s", wf)
	}
	// Ordinary resolution: checkout --theirs hydrates from cache, add re-cleans.
	r.Git("checkout", "--theirs", "--", "a.bin")
	if !bytes.Equal(r.ReadFile("a.bin"), theirs) {
		t.Fatal("checkout --theirs did not hydrate their asset")
	}
	r.Git("add", "a.bin")
	entries, _ = r.Client.LsFilesStage(context.Background(), "a.bin")
	if len(entries) != 1 || entries[0].Stage != 0 {
		t.Fatalf("not resolved: %+v", entries)
	}
	blob, _ := r.Client.CatFileBlob(context.Background(), ":a.bin")
	if p, _ := pointer.Decode(blob); p != theirsP {
		t.Fatal("resolved pointer is not theirs")
	}
	r.Git("commit", "-q", "-m", "merged")

	// Fast merge: only one side changed the asset -> clean merge, no driver
	// conflict.
	r.Git("checkout", "-q", "-b", "side")
	r.WriteFile("a.bin", randomAsset(4000, 4))
	r.Commit("side change")
	r.Git("checkout", "-q", "main")
	r.WriteFile("plain.txt", []byte("x"))
	r.Commit("main plain")
	if _, stderr, err := r.GitErr("merge", "--no-edit", "side"); err != nil {
		t.Fatalf("one-sided merge failed: %v\n%s", err, stderr)
	}
	sideP, _ := expectedPointer(t, randomAsset(4000, 4))
	blob, _ = r.Client.CatFileBlob(context.Background(), "HEAD:a.bin")
	if p, _ := pointer.Decode(blob); p != sideP {
		t.Fatal("one-sided merge did not take the changed side")
	}
}

func TestDriverExitCodes(t *testing.T) {
	// merge-driver with non-pointer input exits non-zero and leaves %A alone.
	dir := t.TempDir()
	o := filepath.Join(dir, "O")
	a := filepath.Join(dir, "A")
	b := filepath.Join(dir, "B")
	os.WriteFile(o, nil, 0o644)
	os.WriteFile(a, []byte("raw"), 0o644)
	os.WriteFile(b, []byte("raw2"), 0o644)
	var errb bytes.Buffer
	env := cli.Env{Stdout: &bytes.Buffer{}, Stderr: &errb, Extra: drivers.Commands()}
	code := cli.Run(context.Background(), env, []string{"fxvcs", "merge-driver", o, a, b, "x.bin"})
	if code == 0 || !strings.Contains(errb.String(), "not an fxvcs pointer") {
		t.Fatalf("exit %d stderr %q", code, errb.String())
	}
	if got, _ := os.ReadFile(a); string(got) != "raw" {
		t.Fatal("%A modified")
	}
	fmt.Fprintln(&errb) // keep buffer used
}

// TestStageActionMatchesWhatGitAddRecords holds the stage affordance to the
// only standard that means anything: what `git add` actually records.
//
// The affordance exists because "would staging this asset do the right thing"
// is not answerable from outside the filter. An interface can see that a file
// is a placeholder; it cannot see that the clean filter passes pointer bytes
// through untouched, and so cannot tell whether staging an unloaded asset
// quietly commits a placeholder over a texture (it does not) or does nothing
// at all (it does). Getting that wrong in either direction is expensive: a
// greyed-out control for a safe operation, or a commit that replaces an
// asset with its stand-in.
//
// So for every asset state this stages the file for real and compares the
// index blob before and after. `available` must mean the blob moves, `done`
// must mean it does not.
func TestStageActionMatchesWhatGitAddRecords(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	asset := randomAsset(3<<20, 77)
	for _, p := range []string{"loaded.bin", "unloaded.bin", "edited.bin"} {
		r.WriteFile(p, asset)
	}
	r.Git("add", ".")
	r.Commit("assets")
	f.publish(t)

	c := f.client(t)
	if _, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"unloaded.bin"}}}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile("edited.bin", append(bytes.Clone(asset), []byte(" LOCAL EDIT")...))

	res, err := c.Status(ctx, sdk.StatusRequest{Path: r.Root})
	if err != nil {
		t.Fatal(err)
	}
	stage := map[string]sdk.AssetAction{}
	states := map[string]sdk.AssetState{}
	for _, e := range res.Entries {
		states[e.Path] = e.State
		for _, a := range e.Actions {
			if a.Action == sdk.ActionStage {
				stage[e.Path] = a
			}
		}
	}
	for _, want := range []struct {
		path      string
		state     sdk.AssetState
		available bool
		done      bool
	}{
		{"loaded.bin", sdk.AssetHydratedClean, false, true},
		{"unloaded.bin", sdk.AssetUnhydratedClean, false, true},
		{"edited.bin", sdk.AssetHydratedDirty, true, false},
	} {
		t.Run(want.path, func(t *testing.T) {
			if states[want.path] != want.state {
				t.Fatalf("fixture is in state %q, expected %q", states[want.path], want.state)
			}
			a, ok := stage[want.path]
			if !ok {
				t.Fatal("status reports no stage action")
			}
			if a.Available != want.available || a.Done != want.done {
				t.Fatalf("stage advertised as %+v", a)
			}
			if !a.Available && a.Reason == "" {
				t.Fatal("a control the interface cannot explain to the user")
			}

			// Now stage it for real and see whether the index moved.
			before := r.Git("rev-parse", ":"+want.path)
			r.Git("add", want.path)
			after := r.Git("rev-parse", ":"+want.path)
			if moved := before != after; moved != want.available {
				t.Fatalf("status said staging was available=%v, but git add %s the index blob",
					want.available, movedWord(moved))
			}
			// Whatever happened, the index still holds a pointer — staging an
			// unloaded asset must never record placeholder bytes as content.
			blob, err := r.Client.CatFileBlob(ctx, ":"+want.path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pointer.Decode(blob); err != nil {
				t.Fatalf("index no longer holds a pointer: %v", err)
			}
		})
	}
	// The one asset whose staging was offered is the one whose bytes are now
	// recorded: the edit reached the index rather than being lost or refused.
	edited, _ := r.Client.CatFileBlob(ctx, ":edited.bin")
	p, err := pointer.Decode(edited)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := expectedPointer(t, append(bytes.Clone(asset), []byte(" LOCAL EDIT")...))
	if p != want {
		t.Fatalf("staged pointer %+v, want %+v", p, want)
	}
}

func movedWord(moved bool) string {
	if moved {
		return "moved"
	}
	return "left"
}

// TestObjectReadServesTheCommittedBytes is the case the read primitive exists
// for: showing an asset as it was committed while the working file holds
// something else.
//
// Both encodings are covered on purpose. A whole/1 asset is one stored object
// and would work under almost any implementation; a chunked asset is a
// manifest plus its pieces, has to be reassembled in manifest order, and is
// the path a caller must never have to reimplement. The fixture chunks
// anything at or above 1 MiB, so big.bin exercises it for real.
func TestObjectReadServesTheCommittedBytes(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	small := randomAsset(100<<10, 11) // below the chunking floor: whole/1
	big := randomAsset(3<<20, 12)     // chunked
	r.WriteFile("small.bin", small)
	r.WriteFile("big.bin", big)
	r.Git("add", ".")
	r.Commit("assets")

	committed := map[string]string{
		// Git output arrives without its trailing newline, which the SDK
		// tolerates: one of these is passed exactly as captured.
		"small.bin": r.Git("cat-file", "-p", ":small.bin"),
		"big.bin":   r.Git("cat-file", "-p", ":big.bin") + "\n",
	}
	if p, err := pointer.Decode([]byte(committed["big.bin"])); err != nil || p.Encoding == pointer.EncodingWhole {
		t.Fatalf("fixture did not chunk big.bin: %+v %v", p, err)
	}

	// The working files now differ from what was committed. This is the whole
	// point: the comparison is against a file that has been edited.
	r.WriteFile("small.bin", []byte("replaced entirely"))
	r.WriteFile("big.bin", append(bytes.Clone(big), []byte(" LOCAL EDIT")...))

	c := f.client(t)
	for _, tc := range []struct {
		path string
		want []byte
	}{
		{"small.bin", small},
		{"big.bin", big},
	} {
		t.Run(tc.path, func(t *testing.T) {
			onDisk, err := os.ReadFile(filepath.Join(r.Root, tc.path))
			if err != nil {
				t.Fatal(err)
			}

			info, err := c.ObjectStat(ctx, sdk.ObjectStatRequest{Path: r.Root, Pointer: committed[tc.path]})
			if err != nil {
				t.Fatal(err)
			}
			if !info.Cached {
				t.Fatalf("content reported as unreadable while it is in the cache: %+v", info)
			}
			if info.Size != int64(len(tc.want)) {
				t.Fatalf("stat says %d bytes, the committed asset is %d", info.Size, len(tc.want))
			}

			res, err := c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: committed[tc.path]})
			if err != nil {
				t.Fatal(err)
			}
			defer res.Content.Close()
			got, err := io.ReadAll(res.Content)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("read %d bytes, want the committed %d", len(got), len(tc.want))
			}
			if res.Length != int64(len(tc.want)) {
				t.Fatalf("result promised %d bytes, produced %d", res.Length, len(got))
			}

			// The read must not have disturbed the file being compared.
			after, err := os.ReadFile(filepath.Join(r.Root, tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, onDisk) {
				t.Fatal("reading the committed content modified the working file")
			}
		})
	}

	// Ranges, on the chunked asset, where they matter: one inside a single
	// chunk and one crossing chunk boundaries. Both must agree with the plain
	// slice of the original content.
	for _, span := range []struct{ off, length int64 }{
		{0, 1024},
		{1 << 20, 4096},          // around the first chunk boundary
		{700 << 10, 1500 << 10},  // spans several chunks
		{int64(len(big)) - 7, 0}, // to the end
		{int64(len(big)) + 99, 8},
	} {
		res, err := c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root,
			Pointer: committed["big.bin"], Offset: span.off, Length: span.length})
		if err != nil {
			t.Fatalf("range %d+%d: %v", span.off, span.length, err)
		}
		got, err := io.ReadAll(res.Content)
		_ = res.Content.Close()
		if err != nil {
			t.Fatalf("range %d+%d: %v", span.off, span.length, err)
		}
		want := []byte{}
		if span.off < int64(len(big)) {
			end := int64(len(big))
			if span.length > 0 && span.off+span.length < end {
				end = span.off + span.length
			}
			want = big[span.off:end]
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("range %d+%d returned %d bytes, want %d", span.off, span.length, len(got), len(want))
		}
		if res.Length != int64(len(want)) {
			t.Fatalf("range %d+%d promised %d bytes, produced %d", span.off, span.length, res.Length, len(got))
		}
	}
}

// TestObjectReadSeparatesNotCachedFromNotFound.
//
// An interface has to tell "the bytes are elsewhere" from "something is
// wrong", because the first has a button and the second has an apology.
func TestObjectReadSeparatesNotCachedFromNotFound(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	asset := randomAsset(2<<20, 13)
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")
	r.Commit("asset")
	ptrText := r.Git("cat-file", "-p", ":a.bin") + "\n"

	// Publishing unpins the objects, so the cache may then evict them: this is
	// an ordinary machine that has not loaded this asset.
	f.publish(t)
	c := f.client(t)
	if _, err := c.CachePrune(ctx, sdk.CachePruneRequest{Path: r.Root, TargetBytes: 0}); err != nil {
		t.Fatal(err)
	}

	info, err := c.ObjectStat(ctx, sdk.ObjectStatRequest{Path: r.Root, Pointer: ptrText})
	if err != nil {
		t.Fatal(err)
	}
	if info.Cached {
		t.Fatal("stat claims content is readable after the cache was emptied")
	}
	_, err = c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: ptrText})
	if err == nil {
		t.Fatal("reading evicted content succeeded")
	}
	if sdk.CodeOf(err) != sdk.CodeNotCached {
		t.Fatalf("evicted content reported as %s, which an interface cannot offer a fetch for: %v", sdk.CodeOf(err), err)
	}

	// The offer that code makes possible.
	if _, err := c.ObjectFetch(ctx, sdk.ObjectFetchRequest{Path: r.Root, Pointer: ptrText}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	res, err := c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: ptrText})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Content)
	_ = res.Content.Close()
	if err != nil || !bytes.Equal(got, asset) {
		t.Fatalf("content after fetch: %d bytes, %v", len(got), err)
	}
	// Fetching put the bytes in the cache and nowhere else.
	if working, err := os.ReadFile(filepath.Join(r.Root, "a.bin")); err != nil || !bytes.Equal(working, asset) {
		t.Fatalf("fetch disturbed the working tree: %v", err)
	}

	// A pointer for content no store has is a different answer again.
	gone, _ := expectedPointer(t, randomAsset(4096, 99))
	_, err = c.ObjectFetch(ctx, sdk.ObjectFetchRequest{Path: r.Root, Pointer: string(gone.MustEncode())})
	if err == nil {
		t.Fatal("fetching content nothing has succeeded")
	}
	if sdk.CodeOf(err) != sdk.CodeNotFound {
		t.Fatalf("unfetchable content reported as %s: %v", sdk.CodeOf(err), err)
	}
}

// TestObjectReadRefusesCorruptChunks proves the guarantee the read primitive
// makes, including for a range.
//
// A ranged read cannot check the whole-content digest — it is defined over
// bytes the range deliberately skips — so what protects it is that every chunk
// it touches is read to the end and checked against its manifest entry, even
// when the range wanted only part of it. Without that, a range would happily
// serve the middle of a chunk whose bytes had rotted.
func TestObjectReadRefusesCorruptChunks(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	asset := randomAsset(3<<20, 21)
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")
	r.Commit("asset")
	ptrText := r.Git("cat-file", "-p", ":a.bin") + "\n"

	_, digests, err := expectedContent(asset)
	if err != nil {
		t.Fatal(err)
	}
	// digests[0] is the manifest; corrupt a chunk in the middle of the asset.
	chunkDigest := digests[len(digests)/2]
	hexPart := strings.TrimPrefix(chunkDigest, "sha256:")
	objPath := filepath.Join(f.cacheDir, "objects", testDomain, hexPart[:2], hexPart[2:4], hexPart)
	raw, err := os.ReadFile(objPath) //nolint:gosec // test tree
	if err != nil {
		t.Fatalf("fixture chunk not in the cache: %v", err)
	}
	raw[len(raw)/2] ^= 0xFF
	// Cached objects are stored read-only, which is why this takes a chmod:
	// nothing in normal operation rewrites one in place.
	if err := os.Chmod(objPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, raw, 0o600); err != nil { //nolint:gosec // test tree
		t.Fatal(err)
	}

	c := f.client(t)
	// A whole read fails, and fails as an integrity problem rather than as
	// short or truncated content.
	res, err := c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: ptrText})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Content)
	_ = res.Content.Close()
	if err == nil {
		t.Fatal("a whole read returned content built from a corrupt chunk")
	}

	// A range landing inside the corrupt chunk fails too, even though it wants
	// only a few bytes from the middle of it.
	var offset int64
	for _, d := range digests[1 : len(digests)/2] {
		hp := strings.TrimPrefix(d, "sha256:")
		st, err := os.Stat(filepath.Join(f.cacheDir, "objects", testDomain, hp[:2], hp[2:4], hp))
		if err != nil {
			t.Fatal(err)
		}
		offset += st.Size()
	}
	res, err = c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: ptrText, Offset: offset + 16, Length: 64})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Content)
	_ = res.Content.Close()
	if err == nil {
		t.Fatal("a ranged read served 64 unchecked bytes out of a corrupt chunk")
	}
}

// TestObjectReadRefusesForeignDomains: the object cache is shared by every
// repository on the machine, partitioned by storage domain, and content is
// never deduplicated across that boundary. A pointer naming another domain
// must not resolve here even when this cache happens to hold the digest — the
// partition is an access boundary, not a filing convenience.
func TestObjectReadRefusesForeignDomains(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	asset := randomAsset(64<<10, 31)
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")
	r.Commit("asset")
	ptrText := r.Git("cat-file", "-p", ":a.bin") + "\n"

	foreign := strings.Replace(ptrText, testDomain, "domain_test_02", 1)
	if foreign == ptrText {
		t.Fatal("fixture pointer does not name the test domain")
	}
	c := f.client(t)
	for _, call := range []struct {
		name string
		err  error
	}{
		{"read", func() error {
			res, err := c.ObjectRead(ctx, sdk.ObjectReadRequest{Path: r.Root, Pointer: foreign})
			if err == nil {
				_ = res.Content.Close()
			}
			return err
		}()},
		{"stat", func() error {
			_, err := c.ObjectStat(ctx, sdk.ObjectStatRequest{Path: r.Root, Pointer: foreign})
			return err
		}()},
		{"fetch", func() error {
			_, err := c.ObjectFetch(ctx, sdk.ObjectFetchRequest{Path: r.Root, Pointer: foreign})
			return err
		}()},
	} {
		if call.err == nil {
			t.Fatalf("%s served content for another storage domain", call.name)
		}
		if sdk.CodeOf(call.err) != sdk.CodeIntegrity {
			t.Fatalf("%s refused a foreign domain as %s: %v", call.name, sdk.CodeOf(call.err), call.err)
		}
	}
}

// TestEmptyManagedFileDoesNotBreakMerge pins the round trip for a zero-length
// managed file. Git stores the working file's size in the index and treats a
// recorded size of 0 as "racily smudged, size unknown", forcing DATA_CHANGED
// unless the index blob is the empty blob. Cleaning an empty file to a pointer
// therefore left the entry permanently stat-dirty: `git diff --quiet` exited 1
// while `git status` reported clean, which made `git stash create` exit 1 and
// every `git merge` die with "fatal: stash failed".
func TestEmptyManagedFileDoesNotBreakMerge(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	asset := randomAsset(4096, 7)
	r.WriteFile("real.bin", asset)
	r.WriteFile("empty.bin", nil)
	r.Git("add", ".")

	// The empty file is stored inline as the empty blob, not as a pointer.
	if got := r.Git("cat-file", "-s", ":empty.bin"); got != "0" {
		t.Fatalf("empty.bin staged as %s bytes, want the empty blob (0)", got)
	}
	if got, want := r.Git("rev-parse", ":empty.bin"), r.Git("hash-object", "-t", "blob", os.DevNull); got != want {
		t.Fatalf("empty.bin staged as %s, want the empty blob %s", got, want)
	}
	// The real asset is still a pointer.
	if !pointer.Sniff([]byte(r.Git("cat-file", "-p", ":real.bin") + "\n")) {
		t.Fatal("real.bin was not staged as a pointer")
	}
	r.Commit("base")

	// status and diff agree, so stash and merge work.
	if st := r.Git("status", "--porcelain"); st != "" {
		t.Fatalf("status not clean: %q", st)
	}
	if _, _, err := r.GitErr("diff", "--quiet"); err != nil {
		t.Fatalf("git diff --quiet reports a phantom change: %v (paths: %q)", err, r.Git("diff", "--name-only"))
	}
	if _, stderr, err := r.GitErr("stash", "create"); err != nil {
		t.Fatalf("git stash create failed: %v %s", err, stderr)
	}
	r.Git("checkout", "-b", "side")
	r.Git("commit", "--allow-empty", "-m", "s")
	r.Git("checkout", "-")
	r.Git("commit", "--allow-empty", "-m", "t")
	if _, stderr, err := r.GitErr("merge", "--no-ff", "side", "-m", "merge"); err != nil {
		t.Fatalf("git merge failed: %v %s", err, stderr)
	}

	// The empty file survives a checkout round trip as a zero-byte file.
	if err := os.Remove(filepath.Join(r.Root, "empty.bin")); err != nil {
		t.Fatal(err)
	}
	r.Git("checkout", "--", "empty.bin")
	if got := r.ReadFile("empty.bin"); len(got) != 0 {
		t.Fatalf("checkout produced %d bytes for an empty file: %q", len(got), got)
	}
	if st := r.Git("status", "--porcelain"); st != "" {
		t.Fatalf("status not clean after checkout: %q", st)
	}
}

// TestSkippedPathStillRefreshesGitStatCache is the phantom-changes
// regression. Git's index records each entry's stat data; when it no longer
// matches the working file, Git reports the path as modified and — this is
// the part that makes it lasting — `git status` does not repair it. Only
// naming the path to `git update-index --refresh` does, which is what an
// operation does after it writes a working file.
//
// An operation that finds a path already in the requested state writes
// nothing and used to say nothing to Git either, so a path whose refresh was
// missed once stayed modified for good: re-running load or unload skipped it
// again. In a repository of a few hundred assets that reads as hundreds of
// changes nobody made.
func TestSkippedPathStillRefreshesGitStatCache(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	asset := randomAsset(1<<20, 11)
	r.WriteFile("a.bin", asset)
	r.Git("add", ".")
	r.Commit("add asset")
	f.publish(t)
	c := f.client(t)

	// Leave Git's stat cache stale: with the filter not marked required the
	// operation refuses to touch the index, which is the same position an
	// operation reaches when Git's index is busy or the process is killed
	// between writing the file and refreshing.
	r.Git("config", "filter.fxvcs.required", "false")
	if res, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil || res.Items[0].Skipped {
		t.Fatalf("dehydrate: %v %+v", err, res)
	}
	r.Git("config", "filter.fxvcs.required", "true")
	if st := r.Git("status", "--porcelain"); st == "" {
		t.Skip("git refreshed the stat cache on its own; nothing to regress against")
	}

	// The asset is already unloaded, so this run writes nothing at all.
	res, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}})
	if err != nil || !res.Items[0].Skipped {
		t.Fatalf("second dehydrate should skip: %v %+v", err, res)
	}
	if st := r.Git("status", "--porcelain"); st != "" {
		t.Fatalf("a skipped path left Git reporting a phantom change: %q", st)
	}

	// The same for a load that finds the asset already loaded.
	if res, err := c.Hydrate(ctx, sdk.HydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil || res.Items[0].Skipped {
		t.Fatalf("hydrate: %v %+v", err, res)
	}
	r.Git("config", "filter.fxvcs.required", "false")
	if res, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil || res.Items[0].Skipped {
		t.Fatalf("dehydrate: %v %+v", err, res)
	}
	if _, err := c.Hydrate(ctx, sdk.HydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}}); err != nil {
		t.Fatal(err)
	}
	r.Git("config", "filter.fxvcs.required", "true")
	res, err = c.Hydrate(ctx, sdk.HydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin"}}})
	if err != nil || !res.Items[0].Skipped {
		t.Fatalf("second hydrate should skip: %v %+v", err, res)
	}
	if st := r.Git("status", "--porcelain"); st != "" {
		t.Fatalf("a skipped load left Git reporting a phantom change: %q", st)
	}
}

// TestRefreshNeverStagesAModifiedAsset guards the constraint the refresh is
// built on. `git update-index --refresh -- <paths>` does not merely refresh
// the named paths: it stages their clean result. That is harmless for a file
// whose clean result is already what the index holds, and it is why only
// paths proved to be in that state may be named. A locally modified asset
// must reach `git add` by the operator's decision and no other way.
func TestRefreshNeverStagesAModifiedAsset(t *testing.T) {
	f := setup(t, "0.0.0-dev")
	r := f.repo
	ctx := context.Background()
	r.WriteFile("a.bin", randomAsset(1<<20, 12))
	r.WriteFile("b.bin", randomAsset(1<<20, 13))
	r.Git("add", ".")
	r.Commit("add assets")
	f.publish(t)
	c := f.client(t)

	staged := r.Git("rev-parse", ":a.bin")
	r.WriteFile("a.bin", randomAsset(1<<20, 14))
	// b.bin unloads, so the operation does reach the index; a.bin is
	// hydrated-modified and must be refused and left alone.
	res, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: r.Root, Selector: sdk.PathSelector{Paths: []string{"a.bin", "b.bin"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Path == "a.bin" && !it.Skipped {
			t.Fatalf("modified asset was not refused: %+v", it)
		}
	}
	if got := r.Git("rev-parse", ":a.bin"); got != staged {
		t.Fatalf("the modification was staged: index moved %s -> %s", staged, got)
	}
	if st := r.Git("status", "--porcelain", "--", "a.bin"); !strings.HasPrefix(st, " M") {
		t.Fatalf("a.bin should be modified and unstaged, got %q", st)
	}
}
