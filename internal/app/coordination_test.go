package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/stack"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// component is a component repository published to a bare remote, the way a
// real one reaches coordination.
type component struct {
	t      *testing.T
	work   *gittest.Repo
	remote string
	id     string
	name   string
}

const manifestBody = "fx_version 'cerulean'\ngame 'gta5'\n"

// componentSpec describes the tree to build.
type componentSpec struct {
	name string
	// resources maps a resource path (relative to sourceRoot) to extra
	// manifest lines.
	resources map[string]string
	// sourceRoot and layout override the detected descriptor.
	sourceRoot   string
	layout       string
	resourceName string
	// activation applies to every catalog entry (default enabled).
	activation domain.Activation
	// after adds ordering constraints, keyed by resource path.
	after map[string][]string
	// persistent declares runtime-created mutable paths and their explicit
	// file/directory kinds, keyed by resource path then declared path.
	persistent map[string]map[string]string
	// id overrides the derived repositoryID, for two repositories that share a
	// display name.
	id string
}

func newComponent(t *testing.T, spec componentSpec) *component {
	t.Helper()
	gittest.RequireGit(t)
	work := gittest.InitRepo(t)
	layout := spec.layout
	if layout == "" {
		layout = domain.LayoutCollection
	}
	root := spec.sourceRoot
	if root == "" {
		root = "."
	}
	for path, extra := range spec.resources {
		rel := path
		if root != "." {
			rel = root + "/" + path
		}
		if path == "." {
			rel = root
		}
		work.WriteFile(rel+"/fxmanifest.lua", []byte(manifestBody+extra))
	}
	id := spec.id
	if id == "" {
		id = "repo_" + strings.ReplaceAll(spec.name, "-", "_") + "_01"
	}
	domainID := "domain_" + strings.ReplaceAll(spec.name, "-", "_") + "_01"
	cfg := fmt.Sprintf(`apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: %s
  storageDomainID: %s
  name: %s
spec:
  profile: fivem
  minimumFXVCSVersion: 0.0.0-dev
  gitIntegrationVersion: 1
  sourceRoot: %s
  layout: %s
`, id, domainID, spec.name, root, layout)
	if spec.resourceName != "" {
		cfg += "  resourceName: " + spec.resourceName + "\n"
	}
	cfg += "  storage:\n    remotes: []\n"
	work.WriteFile(".fxvcs/repository.yaml", []byte(cfg))

	activation := spec.activation
	if activation == "" {
		activation = domain.ActivationEnabled
	}
	cat := fmt.Sprintf("apiVersion: fxvcs.dev/v1alpha1\nkind: ResourceCatalog\nmetadata:\n  repositoryID: %s\nspec:\n  resources:\n", id)
	for path := range spec.resources {
		name := path
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if layout == domain.LayoutSingle {
			name = spec.resourceName
			path = spec.resourceName
		}
		cat += catalogEntry(path, name, activation, spec.after[path], spec.persistent[path])
	}
	work.WriteFile(".fxvcs/resources.yaml", []byte(cat))
	work.Commit("init")

	remote := gittest.BareRemote(t)
	work.Git("remote", "add", "origin", remote)
	work.Git("push", "-q", "origin", "HEAD:refs/heads/main")
	return &component{t: t, work: work, remote: "file://" + remote, id: id, name: spec.name}
}

func catalogEntry(path, name string, activation domain.Activation, after []string, persistent map[string]string) string {
	afterYAML := "[]"
	if len(after) > 0 {
		afterYAML = "[" + strings.Join(after, ", ") + "]"
	}
	persistentPaths := "[]"
	persistentKinds := ""
	if len(persistent) > 0 {
		keys := make([]string, 0, len(persistent))
		for declared := range persistent {
			keys = append(keys, declared)
		}
		sort.Strings(keys)
		quoted := make([]string, len(keys))
		for i, declared := range keys {
			quoted[i] = fmt.Sprintf("%q", declared)
			persistentKinds += fmt.Sprintf("        %q: %s\n", declared, persistent[declared])
		}
		persistentPaths = "[" + strings.Join(quoted, ", ") + "]"
		persistentKinds = "      persistentPathKinds:\n" + persistentKinds
	}
	return fmt.Sprintf(`    - path: %q
      name: %q
      activation: %s
      after: %s
      before: []
      provides: []
      dependencies: []
      games: [gta5]
      serverOnly: false
      persistentPaths: %s
%s
      mutableMode: immutable
      status: present
`, path, name, activation, afterYAML, persistentPaths, persistentKinds)
}

// push commits the current worktree and publishes it.
func (c *component) push(msg string) string {
	c.t.Helper()
	c.work.Git("add", "-A")
	c.work.Git("commit", "-q", "-m", msg)
	c.work.Git("push", "-q", "origin", "HEAD:refs/heads/main")
	return strings.TrimSpace(c.work.Git("rev-parse", "HEAD"))
}

// coordFixture creates an empty coordination repository.
func coordFixture(t *testing.T) *Coord {
	t.Helper()
	gittest.RequireGit(t)
	repo := gittest.InitRepo(t)
	res, err := StackInit(context.Background(), StackInitInput{Path: repo.Root, Name: "test-server"}, testOptions(repo))
	if err != nil {
		t.Fatal(err)
	}
	co, err := OpenCoord(context.Background(), res.Root, testOptions(repo))
	if err != nil {
		t.Fatal(err)
	}
	co.Git = co.Git.WithEnv(repo.Env)
	t.Cleanup(func() {})
	return co
}

// testOptions pins the clock and the isolated Git environment so every
// generated document is byte-stable.
func testOptions(repo *gittest.Repo) Options {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return Options{
		Installed: format.MustParseVersion("0.0.0-dev"),
		Operation: "test",
		Now:       func() time.Time { return at },
		// gittest repositories carry their environment on the client; Open
		// builds its own, so tests rewire it after opening.
		GitExecutable: gitExecutable(repo),
	}
}

func gitExecutable(*gittest.Repo) string { return "" }

// add registers a component and rewires the mirror client environment.
func addComponent(t *testing.T, co *Coord, c *component, in StackAddInput) *StackAddResult {
	t.Helper()
	in.Remote = c.remote
	res, err := co.StackAdd(context.Background(), in)
	if err != nil {
		t.Fatalf("stack add %s: %v", c.name, err)
	}
	return res
}

func mustCreate(t *testing.T, co *Coord, in ReleaseCreateInput) *ReleaseCreateResult {
	t.Helper()
	res, err := co.ReleaseCreate(context.Background(), in)
	if err != nil {
		if res != nil {
			for _, p := range res.Problems {
				t.Logf("problem: %s", p)
			}
		}
		t.Fatalf("release create %s: %v", in.Name, err)
	}
	return res
}

// noObjects skips storage verification: these tests are about composition, and
// the storage path has its own coverage.
func createInput(name string) ReleaseCreateInput {
	return ReleaseCreateInput{Name: name, CreatedBy: "test", SkipObjectCheck: true}
}

// ---------------------------------------------------------------------------
// exit criterion: a release is reproducible after component branches advance
// ---------------------------------------------------------------------------

func TestReleaseIsReproducibleAfterComponentBranchesAdvance(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
	addComponent(t, co, maps, StackAddInput{})

	first := mustCreate(t, co, createInput("v1"))
	pinned := first.Components[0].Commit

	// The component moves on, and coordination fetches the new tip.
	maps.work.WriteFile("bank/extra.lua", []byte("-- later work\n"))
	moved := maps.push("more work")
	if moved == pinned {
		t.Fatal("the component did not actually advance")
	}
	if _, err := co.StackSync(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}

	// Re-creating the same content under a new name with the original pin
	// must reproduce the source identity exactly.
	again := mustCreate(t, co, func() ReleaseCreateInput {
		in := createInput("v1-again")
		in.Pins = []string{maps.id + "=" + pinned}
		return in
	}())
	if again.Components[0].Commit != pinned {
		t.Fatalf("pin ignored: %s", again.Components[0].Commit)
	}
	if again.SourceSet != first.SourceSet {
		t.Fatalf("sourceSetDigest changed for the same sources:\n first %s\n again %s", first.SourceSet, again.SourceSet)
	}
	if again.Components[0].AssetRoot != first.Components[0].AssetRoot {
		t.Fatal("assetRoot changed for the same commit")
	}

	// And following the branch instead must produce a different source set.
	moving := mustCreate(t, co, createInput("v2"))
	if moving.Components[0].Commit != moved {
		t.Fatalf("expected the new tip, got %s", moving.Components[0].Commit)
	}
	if moving.SourceSet == first.SourceSet {
		t.Fatal("sourceSetDigest did not change after the component advanced")
	}
}

// ---------------------------------------------------------------------------
// exit criterion: existing release contents cannot be silently overwritten
// ---------------------------------------------------------------------------

func TestExistingReleaseCannotBeSilentlyOverwritten(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, maps, StackAddInput{})
	first := mustCreate(t, co, createInput("v1"))

	// Re-running with identical inputs is a no-op, not a rewrite.
	same, err := co.ReleaseCreate(context.Background(), createInput("v1"))
	if err != nil {
		t.Fatalf("recreating an identical release should succeed: %v", err)
	}
	if same.Written {
		t.Fatal("an identical release was rewritten")
	}
	if same.Digest != first.Digest {
		t.Fatalf("digest changed: %s -> %s", first.Digest, same.Digest)
	}

	// Different content under the same name is refused.
	maps.work.WriteFile("city-map/new.lua", []byte("-- change\n"))
	maps.push("change")
	if _, err := co.StackSync(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
	if !errors.Is(err, controlplane.ErrImmutable) {
		t.Fatalf("want ErrImmutable, got %v", err)
	}
	if res != nil && res.Written {
		t.Fatal("the release file was rewritten anyway")
	}

	// The record on disk still describes the original.
	onDisk, err := co.GetRelease(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := integrity.ReleaseDigest(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if digest != first.Digest {
		t.Fatal("the stored release changed")
	}
}

// ---------------------------------------------------------------------------
// exit criterion: missing access, missing objects, duplicate mounts, and
// mismatched repository IDs fail before deployment
// ---------------------------------------------------------------------------

func TestMissingRepositoryAccessFailsClosed(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, maps, StackAddInput{})

	// The mirror disappears the way a revoked credential or a deleted remote
	// would present: the pinned history is simply not there.
	mirror, err := co.MirrorPath(maps.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}
	if _, err := co.ReleaseCreate(context.Background(), createInput("v1")); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := os.Stat(co.ReleasePath("v1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a release file was written despite unreachable component history")
	}
}

func TestDuplicateAndOverlappingMountsFailClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, first, second, want string }{
		{"identical", "resources/[shared]", "resources/[shared]", "already used"},
		{"nested", "resources/[shared]", "resources/[shared]/[inner]", "overlaps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			co := coordFixture(t)
			a := newComponent(t, componentSpec{name: "alpha", resources: map[string]string{"one": ""}})
			b := newComponent(t, componentSpec{name: "beta", resources: map[string]string{"two": ""}})
			addComponent(t, co, a, StackAddInput{Mount: tc.first})

			_, err := co.StackAdd(context.Background(), StackAddInput{Remote: b.remote, Mount: tc.second})
			if err == nil {
				t.Fatal("the second component was accepted at a conflicting mount")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not explain the conflict: %v", err)
			}
		})
	}
}

func TestMismatchedRepositoryIDFailsClosed(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, maps, StackAddInput{})

	// A fork that keeps the remote but changes its identity: coordination must
	// notice rather than silently release someone else's repository.
	ws := *co.Stack
	ws.Spec.Components = []domain.StackComponent{{
		RepositoryID: "repo_someone_else_01", Remote: maps.remote, Required: true, Mount: "resources/[maps]",
	}}
	if err := co.SaveStack(&ws); err != nil {
		t.Fatal(err)
	}
	// The mirror for the new ID does not exist, which is itself the fail-closed
	// path; place the fetched history there to reach the identity check.
	from, _ := co.MirrorPath(maps.id)
	to, _ := co.MirrorPath("repo_someone_else_01")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
	if err == nil {
		t.Fatal("a release was created for a repository whose identity does not match")
	}
	if !hasReleaseProblem(res, "declares repositoryID") {
		t.Fatalf("no identity problem reported: %v / %+v", err, problems(res))
	}
}

func TestMissingObjectsFailBeforeRelease(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	store := t.TempDir()
	maps := newComponentWithStorage(t, "maps", store)
	addComponent(t, co, maps, StackAddInput{})

	// Storage is bound but empty: the placeholder in the pinned tree has no
	// object behind it.
	if _, err := co.StorageBind(StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	in := createInput("v1")
	in.SkipObjectCheck = false
	res, err := co.ReleaseCreate(context.Background(), in)
	if err == nil {
		t.Fatal("a release was created while its assets were missing from storage")
	}
	if !hasReleaseProblem(res, "not in storage") {
		t.Fatalf("no storage problem reported: %+v", problems(res))
	}

	// With the object present the same release builds.
	writeObject(t, store, maps)
	if _, err := co.ReleaseCreate(context.Background(), in); err != nil {
		t.Fatalf("release refused with the object present: %v", err)
	}
}

func TestUnreachableStorageIsUnverifiableNotAbsent(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponentWithStorage(t, "maps", t.TempDir())
	addComponent(t, co, maps, StackAddInput{})

	// No local binding at all: the honest answer is "cannot tell", and that
	// must not be silently read as "everything is fine".
	in := createInput("v1")
	in.SkipObjectCheck = false
	res, err := co.ReleaseCreate(context.Background(), in)
	if err == nil {
		t.Fatal("a release was created without being able to check storage")
	}
	if !hasReleaseProblem(res, "cannot be reached") {
		t.Fatalf("want an unverifiable-storage problem, got %+v", problems(res))
	}

	in.AllowUnverifiedObjects = true
	ok, err := co.ReleaseCreate(context.Background(), in)
	if err != nil {
		t.Fatalf("--allow-unverified-objects did not let the release through: %v", err)
	}
	if len(ok.Warnings) == 0 {
		t.Fatal("an unverified release was created without a warning")
	}
}

// ---------------------------------------------------------------------------
// exit criterion: layout fixtures produce deterministic detection/validation
// ---------------------------------------------------------------------------

func TestComponentLayoutFixtures(t *testing.T) {
	t.Parallel()
	t.Run("single resource mounts inside a category", func(t *testing.T) {
		co := coordFixture(t)
		inv := newComponent(t, componentSpec{
			name: "inventory", layout: domain.LayoutSingle, resourceName: "inventory",
			resources: map[string]string{".": ""},
		})
		res := addComponent(t, co, inv, StackAddInput{})
		if res.Component.Mount != "resources/[inventory]/inventory" {
			t.Fatalf("mount %q", res.Component.Mount)
		}
		rel := mustCreate(t, co, createInput("v1"))
		if got := rel.Resources[0].Mount; got != "resources/[inventory]/inventory" {
			t.Fatalf("resource mount %q", got)
		}
	})

	t.Run("collection mounts as a category", func(t *testing.T) {
		co := coordFixture(t)
		maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
		res := addComponent(t, co, maps, StackAddInput{})
		if res.Component.Mount != "resources/[maps]" {
			t.Fatalf("mount %q", res.Component.Mount)
		}
		rel := mustCreate(t, co, createInput("v1"))
		want := map[string]string{"bank": "resources/[maps]/bank", "city-map": "resources/[maps]/city-map"}
		for _, r := range rel.Resources {
			if want[r.Name] != r.Mount {
				t.Fatalf("%s mounted at %q, want %q", r.Name, r.Mount, want[r.Name])
			}
		}
	})

	t.Run("conventional resources wrapper", func(t *testing.T) {
		co := coordFixture(t)
		c := newComponent(t, componentSpec{name: "wrapped", sourceRoot: "resources",
			resources: map[string]string{"one": "", "two": ""}})
		res := addComponent(t, co, c, StackAddInput{})
		if res.Component.Descriptor.SourceRoot != "resources" {
			t.Fatalf("source root %q", res.Component.Descriptor.SourceRoot)
		}
		mustCreate(t, co, createInput("v1"))
	})

	t.Run("nested bracket categories are preserved", func(t *testing.T) {
		co := coordFixture(t)
		c := newComponent(t, componentSpec{name: "nested", resources: map[string]string{"[cars]/sultan": "", "[cars]/banshee": ""}})
		addComponent(t, co, c, StackAddInput{})
		rel := mustCreate(t, co, createInput("v1"))
		for _, r := range rel.Resources {
			if !strings.HasPrefix(r.Mount, "resources/[nested]/[cars]/") {
				t.Fatalf("%s mounted at %q; the nested category was lost", r.Name, r.Mount)
			}
		}
	})

	t.Run("a resource name may not be claimed twice", func(t *testing.T) {
		co := coordFixture(t)
		a := newComponent(t, componentSpec{name: "alpha", resources: map[string]string{"chat": ""}})
		b := newComponent(t, componentSpec{name: "beta", resources: map[string]string{"chat": ""}})
		addComponent(t, co, a, StackAddInput{})
		addComponent(t, co, b, StackAddInput{})
		res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
		if err == nil {
			t.Fatal("two resources named chat were accepted")
		}
		if !hasReleaseProblem(res, "declared twice") {
			t.Fatalf("no name-collision problem: %+v", problems(res))
		}
	})

	t.Run("two repositories may not resolve to one display name", func(t *testing.T) {
		co := coordFixture(t)
		a := newComponent(t, componentSpec{name: "tools", resources: map[string]string{"one": ""}})
		b := newComponent(t, componentSpec{name: "tools", id: "repo_tools_02", resources: map[string]string{"two": ""}})
		addComponent(t, co, a, StackAddInput{})
		_, err := co.StackAdd(context.Background(), StackAddInput{Remote: b.remote})
		if err == nil {
			t.Fatal("a duplicate display name was accepted without an alias")
		}
		if !strings.Contains(err.Error(), "--alias") {
			t.Fatalf("the error does not point at the fix: %v", err)
		}
		// With an alias it is accepted and the release records the alias.
		res, err := co.StackAdd(context.Background(), StackAddInput{Remote: b.remote, Alias: "community-tools"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Component.Mount != "resources/[community-tools]" {
			t.Fatalf("mount %q should follow the alias", res.Component.Mount)
		}
	})
}

// ---------------------------------------------------------------------------
// exit criterion: pending, missing, stale, cycles, and descriptor
// disagreement fail before release creation
// ---------------------------------------------------------------------------

func TestPendingResourceFailsBeforeRelease(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}, activation: domain.ActivationPending})
	addComponent(t, co, maps, StackAddInput{})

	res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
	if err == nil {
		t.Fatal("a release was created with an undecided resource")
	}
	if !hasReleaseProblem(res, "pending") {
		t.Fatalf("no pending problem: %+v", problems(res))
	}

	// A release-only override decides it, and the provenance says so.
	in := createInput("v1")
	in.Enable = []string{"city-map"}
	ok := mustCreate(t, co, in)
	if ok.Resources[0].ActivationSource != domain.ActivationSourceRelease {
		t.Fatalf("provenance %q", ok.Resources[0].ActivationSource)
	}
}

func TestUndeclaredAndMissingResourcesFailBeforeRelease(t *testing.T) {
	t.Parallel()
	t.Run("a resource in the tree that the list does not declare", func(t *testing.T) {
		co := coordFixture(t)
		maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
		addComponent(t, co, maps, StackAddInput{})
		maps.work.WriteFile("secret-shop/fxmanifest.lua", []byte(manifestBody))
		maps.push("add a resource without syncing the list")
		if _, err := co.StackSync(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
		res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
		if err == nil {
			t.Fatal("a release shipped a resource nobody declared")
		}
		if !hasReleaseProblem(res, "resources sync") {
			t.Fatalf("the problem does not name the fix: %+v", problems(res))
		}
	})

	t.Run("a declared resource whose folder is gone", func(t *testing.T) {
		co := coordFixture(t)
		maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
		addComponent(t, co, maps, StackAddInput{})
		if err := os.RemoveAll(filepath.Join(maps.work.Root, "bank")); err != nil {
			t.Fatal(err)
		}
		maps.push("delete a resource without syncing the list")
		if _, err := co.StackSync(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
		res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
		if err == nil {
			t.Fatal("a release referenced a resource that no longer exists")
		}
		if !hasReleaseProblem(res, "no directory in the pinned tree") {
			t.Fatalf("no missing-directory problem: %+v", problems(res))
		}
	})
}

func TestOrderingCycleFailsBeforeRelease(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{
		name:      "maps",
		resources: map[string]string{"one": "", "two": ""},
		after:     map[string][]string{"one": {"two"}, "two": {"one"}},
	})
	addComponent(t, co, maps, StackAddInput{})
	res, err := co.ReleaseCreate(context.Background(), createInput("v1"))
	if err == nil {
		t.Fatal("a start order was invented for a cycle")
	}
	if !hasReleaseProblem(res, "cycle") {
		t.Fatalf("no cycle problem: err=%v problems=%+v", err, problems(res))
	}
}

func TestDescriptorIsSnapshottedAndLaterLayoutChangesFailClosed(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	inv := newComponent(t, componentSpec{
		name: "inventory", layout: domain.LayoutSingle, resourceName: "inventory",
		resources: map[string]string{".": ""},
	})
	addComponent(t, co, inv, StackAddInput{})
	first := mustCreate(t, co, createInput("v1"))
	if first.Release.Spec.Components[0].Descriptor.Layout != domain.LayoutSingle {
		t.Fatal("the release did not snapshot the accepted layout")
	}

	// The component turns itself into a collection. The recorded mount is a
	// single-resource mount, so the next release must refuse rather than
	// quietly reshape the realization.
	cfg := strings.Replace(string(inv.work.ReadFile(".fxvcs/repository.yaml")),
		"layout: single\n  resourceName: inventory\n", "layout: collection\n", 1)
	inv.work.WriteFile(".fxvcs/repository.yaml", []byte(cfg))
	if err := os.Remove(filepath.Join(inv.work.Root, "fxmanifest.lua")); err != nil {
		t.Fatal(err)
	}
	inv.work.WriteFile("shop/fxmanifest.lua", []byte(manifestBody))
	cat := strings.Replace(string(inv.work.ReadFile(".fxvcs/resources.yaml")), `path: "inventory"`, `path: "shop"`, 1)
	cat = strings.Replace(cat, `name: "inventory"`, `name: "shop"`, 1)
	inv.work.WriteFile(".fxvcs/resources.yaml", []byte(cat))
	inv.push("become a collection")
	if _, err := co.StackSync(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}

	res, err := co.ReleaseCreate(context.Background(), createInput("v2"))
	if err == nil {
		t.Fatal("a layout change silently reshaped the realization")
	}
	if !hasReleaseProblem(res, "bracketed category") && !hasReleaseProblem(res, "mount") {
		t.Fatalf("no mount problem: %+v", problems(res))
	}

	// The existing release is untouched by any of this.
	stored, err := co.GetRelease(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Components[0].Descriptor.Layout != domain.LayoutSingle {
		t.Fatal("an existing release changed shape when the component did")
	}

	// rescan reports it and only changes anything when explicitly accepted.
	rep, err := co.StackRescan(context.Background(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Changed {
		t.Fatal("rescan changed the stack without --accept")
	}
	if rep.Findings[0].Agrees {
		t.Fatal("rescan did not notice the layout change")
	}
	accepted, err := co.StackRescan(context.Background(), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Changed || !accepted.Findings[0].Updated {
		t.Fatal("rescan --accept recorded nothing")
	}
	if _, err := co.ReleaseCreate(context.Background(), createInput("v2")); err != nil {
		t.Fatalf("release still refused after an accepted rescan: %v", err)
	}
}

// ---------------------------------------------------------------------------
// exit criterion: stack init creates or adopts, and never commits
// ---------------------------------------------------------------------------

func TestStackInitCreatesAdoptsAndNeverCommits(t *testing.T) {
	t.Parallel()
	gittest.RequireGit(t)
	repo := gittest.InitRepo(t)
	ctx := context.Background()

	res, err := StackInit(ctx, StackInitInput{Path: repo.Root, Name: "server"}, testOptions(repo))
	if err != nil {
		t.Fatal(err)
	}
	if res.File != gitops.StackFile {
		t.Fatalf("file %q", res.File)
	}
	if out := strings.TrimSpace(repo.Git("status", "--porcelain")); !strings.Contains(out, gitops.StackFile) {
		t.Fatalf("fxvcs.yaml is not an uncommitted change: %q", out)
	}
	if log, _, _ := repo.GitErr("log", "--oneline"); strings.Contains(log, "fxvcs") {
		t.Fatal("stack init created a commit")
	}

	// A second init refuses; --adopt accepts what is there.
	if _, err := StackInit(ctx, StackInitInput{Path: repo.Root}, testOptions(repo)); !errors.Is(err, controlplane.ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	adopted, err := StackInit(ctx, StackInitInput{Path: repo.Root, Adopt: true}, testOptions(repo))
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Adopted || adopted.StackID != res.StackID {
		t.Fatalf("adopt created a new stack: %+v", adopted)
	}
}

func TestStackInitRefusesAComponentRepository(t *testing.T) {
	t.Parallel()
	gittest.RequireGit(t)
	repo := gittest.InitRepo(t)
	repo.WriteFile(".fxvcs/repository.yaml", []byte("apiVersion: fxvcs.dev/v1alpha1\nkind: Repository\n"))
	_, err := StackInit(context.Background(), StackInitInput{Path: repo.Root}, testOptions(repo))
	if err == nil || !strings.Contains(err.Error(), "separate") {
		t.Fatalf("want a refusal explaining the separation, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// activation plan: provenance, overrides, and idempotent rediscovery
// ---------------------------------------------------------------------------

func TestCoordinationOverridesRecordTheirProvenance(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
	addComponent(t, co, maps, StackAddInput{})

	overrides := fmt.Sprintf(`apiVersion: fxvcs.dev/v1alpha1
kind: ResourceOverrides
spec:
  resources:
    - repositoryID: %s
      path: bank
      activation: disabled
`, maps.id)
	if err := os.WriteFile(co.Path(gitops.OverridesFile), []byte(overrides), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustCreate(t, co, createInput("v1"))
	got := map[string]string{}
	for _, r := range res.Resources {
		got[r.Name] = r.Activation + "/" + r.ActivationSource
	}
	if got["bank"] != "disabled/"+domain.ActivationSourceCoordination {
		t.Fatalf("bank: %s", got["bank"])
	}
	if got["city-map"] != "enabled/"+domain.ActivationSourceComponent {
		t.Fatalf("city-map: %s", got["city-map"])
	}
}

func TestRediscoveryIsIdempotentAndCannotMutateAnExistingRelease(t *testing.T) {
	t.Parallel()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
	addComponent(t, co, maps, StackAddInput{})
	first := mustCreate(t, co, createInput("v1"))

	// Validating repeatedly must produce the same plan every time.
	var plans []string
	for range 3 {
		res, err := co.StackValidate(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, planString(res.Resources))
	}
	if plans[0] != plans[1] || plans[1] != plans[2] {
		t.Fatalf("validation is not deterministic:\n%s\n%s\n%s", plans[0], plans[1], plans[2])
	}

	// Turning a resource off in coordination changes the next release, not the
	// one that already exists.
	overrides := fmt.Sprintf("apiVersion: fxvcs.dev/v1alpha1\nkind: ResourceOverrides\nspec:\n  resources:\n    - repositoryID: %s\n      path: bank\n      activation: disabled\n", maps.id)
	if err := os.WriteFile(co.Path(gitops.OverridesFile), []byte(overrides), 0o644); err != nil {
		t.Fatal(err)
	}
	second := mustCreate(t, co, createInput("v2"))
	if second.Enabled != 1 {
		t.Fatalf("the override did not apply: %d enabled", second.Enabled)
	}
	if second.SourceSet != first.SourceSet {
		t.Fatal("changing activation changed the source identity")
	}
	stored, err := co.GetRelease(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range stored.Spec.Resources {
		if r.Name == "bank" && r.Activation != domain.ActivationEnabled {
			t.Fatal("an existing release changed when a later override was written")
		}
	}
}

func planString(rows []PlannedResource) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%d %s %s %s\n", r.EnsureOrder, r.Name, r.Activation, r.ActivationSource)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// portability, desired state, notes
// ---------------------------------------------------------------------------

func TestReleaseExportImportPreservesIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, source, maps, StackAddInput{})
	created := mustCreate(t, source, createInput("v1"))

	exported, err := source.ReleaseExport(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if exported.Digest != created.Digest {
		t.Fatal("export changed the release digest")
	}

	target := coordFixture(t)
	imported, err := target.ReleaseImport(ctx, exported.Document)
	if err != nil {
		t.Fatal(err)
	}
	if !imported.Written || imported.Digest != created.Digest {
		t.Fatalf("import changed the release: %+v", imported)
	}
	if len(imported.Warnings) == 0 {
		t.Fatal("importing into a stack that does not register the component was not flagged")
	}
	// Re-importing the same document is a no-op; a different one is refused.
	again, err := target.ReleaseImport(ctx, exported.Document)
	if err != nil || again.Written {
		t.Fatalf("re-import: written=%v err=%v", again.Written, err)
	}
	tampered := strings.Replace(string(exported.Document), "createdBy: test", "createdBy: someone-else", 1)
	if _, err := target.ReleaseImport(ctx, []byte(tampered)); !errors.Is(err, controlplane.ErrImmutable) {
		t.Fatalf("want ErrImmutable, got %v", err)
	}
}

func TestTargetAndEnvironmentDesiredState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, maps, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	mustCreate(t, co, createInput("v2"))

	// A target cannot point at a release nobody has.
	_, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v9", Environment: "prd",
		AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "envfile"})
	if !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// ...nor at an environment nobody has.
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd",
		AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "envfile"}); !errors.Is(err, controlplane.ErrNotFound) {
		t.Fatalf("want ErrNotFound for the environment, got %v", err)
	}

	env, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}})
	if err != nil {
		t.Fatal(err)
	}
	if env.Revision != 1 || env.Inputs["hostname"] != "Example" {
		t.Fatalf("%+v", env)
	}
	// An unchanged write does not bump the revision; a real change does.
	same, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Example"}})
	if err != nil || same.Revision != 1 {
		t.Fatalf("revision moved without a change: %+v %v", same, err)
	}
	changed, err := co.EnvironmentSet(ctx, EnvironmentSetInput{Name: "prd", Inputs: []string{"hostname=Other"}})
	if err != nil || changed.Revision != 2 {
		t.Fatalf("revision did not move on a change: %+v %v", changed, err)
	}

	created, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1", Environment: "prd",
		AgentID: "agent-a", RuntimeRef: "fxserver", SecretProvider: "envfile"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.Authority != domain.AuthorityGitOps || !created.ReleaseExists {
		t.Fatalf("%+v", created)
	}
	// Promotion and rollback are the same edit.
	promoted, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v2"})
	if err != nil || promoted.PreviousRelease != "v1" || promoted.Release != "v2" {
		t.Fatalf("promote: %+v %v", promoted, err)
	}
	rolledBack, err := co.TargetSet(ctx, TargetSetInput{Name: "prd", Release: "v1"})
	if err != nil || rolledBack.Release != "v1" {
		t.Fatalf("rollback: %+v %v", rolledBack, err)
	}
	// Creating a target without the details it needs is a usage error, not a
	// half-written record.
	if _, err := co.TargetSet(ctx, TargetSetInput{Name: "dev", Release: "v1"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if _, err := os.Stat(co.TargetPath("dev")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an incomplete target was written")
	}
}

func TestReleaseNotesReportSourceEqualitySeparately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": "", "bank": ""}})
	addComponent(t, co, maps, StackAddInput{})
	mustCreate(t, co, createInput("v1"))
	in := createInput("v1-dev")
	in.Disable = []string{"bank"}
	in.VariantOf = "v1"
	mustCreate(t, co, in)

	notes, err := co.ReleaseNotes(ctx, "v1-dev", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !notes.Change.SameSource {
		t.Fatal("a variant that pins the same commits was not reported as same-source")
	}
	if !strings.Contains(notes.Markdown, "Same source as the previous release") {
		t.Fatalf("notes do not say the source is identical:\n%s", notes.Markdown)
	}
	if !strings.Contains(notes.Markdown, "`bank` is now **disabled**") {
		t.Fatalf("notes do not report the activation change:\n%s", notes.Markdown)
	}
}

func TestRuntimeResourcesSyncLeavesAmbiguousEntriesPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	co := coordFixture(t)
	maps := newComponent(t, componentSpec{name: "maps", resources: map[string]string{"city-map": ""}})
	addComponent(t, co, maps, StackAddInput{})

	cfg := filepath.Join(t.TempDir(), "server.cfg")
	if err := os.WriteFile(cfg, []byte("ensure city-map\nensure monitor\nensure mapmanager\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := co.RuntimeResourcesSync(ctx, RuntimeResourcesSyncInput{FromCfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "city-map" {
		t.Fatalf("component-provided resources were not recognised: %+v", res.Matched)
	}
	if len(res.Proposed) != 2 {
		t.Fatalf("proposed %+v", res.Proposed)
	}
	cat, err := co.RuntimeResources()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range cat.Spec.Resources {
		if r.Activation != domain.ActivationPending {
			t.Fatalf("%s was decided automatically: %s", r.Name, r.Activation)
		}
		if r.Source != domain.ResourceSourceExternal {
			t.Fatalf("%s was guessed as %s; a cfg cannot tell us who owns a resource", r.Name, r.Source)
		}
	}
	// A pending runtime entry blocks a release until someone decides it.
	if _, err := co.ReleaseCreate(ctx, createInput("v1")); err == nil {
		t.Fatal("a release was created with undecided runtime resources")
	}
	// Re-running is idempotent.
	second, err := co.RuntimeResourcesSync(ctx, RuntimeResourcesSyncInput{FromCfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Proposed) != 0 || len(second.Existing) != 2 {
		t.Fatalf("second run was not idempotent: %+v", second)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func problems(res *ReleaseCreateResult) []stack.Problem {
	if res == nil {
		return nil
	}
	return res.Problems
}

func hasReleaseProblem(res *ReleaseCreateResult, substr string) bool {
	for _, p := range problems(res) {
		if strings.Contains(p.Message, substr) {
			return true
		}
	}
	return false
}

// newComponentWithStorage builds a component that declares a required
// filesystem remote and tracks one asset as a placeholder.
func newComponentWithStorage(t *testing.T, name, store string) *component {
	t.Helper()
	c := newComponent(t, componentSpec{name: name, resources: map[string]string{"city-map": ""}})
	cfg := string(c.work.ReadFile(".fxvcs/repository.yaml"))
	cfg = strings.Replace(cfg, "  storage:\n    remotes: []\n",
		"  storage:\n    remotes:\n      - name: primary\n        type: filesystem\n        requiredForPublication: true\n", 1)
	c.work.WriteFile(".fxvcs/repository.yaml", []byte(cfg))
	c.work.WriteFile("city-map/city.ytd", testPointerBytes(t, name))
	c.push("track an asset")
	return c
}

// assetBody is the content the fixture placeholder stands for.
func assetBody(name string) []byte { return []byte("asset bytes for " + name + "\n") }

func storageDomainOf(name string) string {
	return "domain_" + strings.ReplaceAll(name, "-", "_") + "_01"
}

// testPointerBytes is a valid whole/1 placeholder. The test never materializes
// the file; only whether storage holds its object is under test.
func testPointerBytes(t *testing.T, name string) []byte {
	t.Helper()
	body := assetBody(name)
	sum := sha256.Sum256(body)
	p, err := filter.WholePointer(storageDomainOf(name), "sha256:"+hex.EncodeToString(sum[:]), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeObject puts the object the placeholder refers to into a filesystem
// store, the way `fxvcs objects publish` would.
func writeObject(t *testing.T, store string, c *component) {
	t.Helper()
	s, err := filesystem.New(store)
	if err != nil {
		t.Fatal(err)
	}
	body := assetBody(c.name)
	sum := sha256.Sum256(body)
	if err := s.Put(context.Background(), storageDomainOf(c.name), "sha256:"+hex.EncodeToString(sum[:]), int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}
