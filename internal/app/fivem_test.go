package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/profile/fivem/catalog"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/schema"
)

const fivemFixtures = "../../spec/fixtures/fivem"

const fivemRepoYAML = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_fivem_01
  storageDomainID: domain_fivem_01
  name: fivem-collection
spec:
  profile: fivem
  minimumFXVCSVersion: 0.0.0-dev
  gitIntegrationVersion: 1
  sourceRoot: .
  layout: collection
  storage:
    remotes: []
`

// copyTree copies the fixture tree src into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fivemRepo creates a git repository holding the collection fixture and an
// accepted collection descriptor.
func fivemRepo(t *testing.T) (*gittest.Repo, *Repo) {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	copyTree(t, filepath.Join(fivemFixtures, "collection"), r.Root)
	r.WriteFile(".fxvcs/repository.yaml", []byte(fivemRepoYAML))
	r.Commit("init")
	repo := openTest(t, r, t.TempDir())
	return r, repo
}

// expectedCollection is the golden Scan output shape used by the discovery
// tests, reused here to check the catalog matches the manifests.
type expectedCollection struct {
	Resources []struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		Manifest struct {
			Games        []string `json:"games"`
			Provides     []string `json:"provides"`
			Dependencies []string `json:"dependencies"`
			ServerOnly   bool     `json:"serverOnly"`
		} `json:"manifest"`
	} `json:"resources"`
}

func loadExpectedCollection(t *testing.T) expectedCollection {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fivemFixtures, "expected", "collection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp expectedCollection
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatal(err)
	}
	return exp
}

func readCatalog(t *testing.T, repo *Repo) (*domain.ResourceCatalog, []byte) {
	t.Helper()
	cat, raw, exists, err := repo.LoadResourceCatalog()
	if err != nil || !exists {
		t.Fatalf("load catalog: %v exists=%v", err, exists)
	}
	return cat, raw
}

func entry(t *testing.T, cat *domain.ResourceCatalog, path string) domain.CatalogResource {
	t.Helper()
	for _, r := range cat.Spec.Resources {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no entry %q in %+v", path, cat.Spec.Resources)
	return domain.CatalogResource{}
}

func TestResourcesSyncFromScratchMatchesFixturesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	_, repo := fivemRepo(t)
	ctx := context.Background()
	out, err := repo.ResourcesSync(ctx, ResourcesSyncInput{})
	if err != nil {
		t.Fatal(err)
	}
	exp := loadExpectedCollection(t)
	if len(out.Added) != len(exp.Resources) || !out.Written || len(out.Missing) != 0 || len(out.Updated) != 0 {
		t.Fatalf("first sync: %+v", out)
	}
	cat, raw := readCatalog(t, repo)
	if cat.Metadata.RepositoryID != "repo_fivem_01" || cat.Kind != "ResourceCatalog" {
		t.Fatalf("metadata %+v", cat.Metadata)
	}
	if iss := schema.Validate("ResourceCatalog", raw); len(iss) > 0 {
		t.Fatalf("written catalog does not validate: %v", iss)
	}
	if len(cat.Spec.Resources) != len(exp.Resources) {
		t.Fatalf("catalog has %d entries, want %d", len(cat.Spec.Resources), len(exp.Resources))
	}
	for i, want := range exp.Resources {
		got := cat.Spec.Resources[i]
		if got.Path != want.Path || got.Name != want.Name || got.ServerOnly != want.Manifest.ServerOnly ||
			strings.Join(got.Games, ",") != strings.Join(want.Manifest.Games, ",") ||
			strings.Join(got.Provides, ",") != strings.Join(want.Manifest.Provides, ",") ||
			strings.Join(got.Dependencies, ",") != strings.Join(want.Manifest.Dependencies, ",") {
			t.Errorf("entry %d: got %+v want %+v", i, got, want)
		}
		if got.Activation != domain.ActivationPending || got.Status != catalog.StatusPresent || got.MutableMode != catalog.MutableImmutable ||
			len(got.After) != 0 || len(got.Before) != 0 || len(got.PersistentPaths) != 0 {
			t.Errorf("entry %d defaults: %+v", i, got)
		}
	}
	if len(out.Pending) != len(exp.Resources) {
		t.Fatalf("pending %v", out.Pending)
	}
	// Empty lists are written as [] and there is no null anywhere.
	if bytes.Contains(raw, []byte("null")) || !bytes.Contains(raw, []byte("after: []")) {
		t.Fatalf("yaml:\n%s", raw)
	}
	// Second run: no changes, byte-identical file.
	info1, _ := os.Stat(repo.CatalogPath())
	out2, err := repo.ResourcesSync(ctx, ResourcesSyncInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Written || len(out2.Added)+len(out2.Updated)+len(out2.Missing) != 0 || out2.Unchanged != len(exp.Resources) {
		t.Fatalf("second sync: %+v", out2)
	}
	_, raw2 := readCatalog(t, repo)
	if !bytes.Equal(raw, raw2) {
		t.Fatalf("second sync changed bytes:\n%s\n---\n%s", raw, raw2)
	}
	info2, _ := os.Stat(repo.CatalogPath())
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatal("unchanged catalog was rewritten")
	}
	// Encoding is deterministic from the domain value too.
	enc, err := EncodeResourceCatalog(cat)
	if err != nil || !bytes.Equal(enc, raw) {
		t.Fatalf("re-encode differs: %v\n%s", err, enc)
	}
	// Nothing was committed and no temp files were left behind.
	if st, _ := repo.Git.RunString(ctx, "status", "--porcelain"); !strings.Contains(st, "resources.yaml") {
		t.Fatalf("catalog should be an uncommitted change: %q", st)
	}
	entries, _ := os.ReadDir(filepath.Join(repo.Root, ".fxvcs"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestResourcesSyncPreservesOperatorEditsMissingAndNew(t *testing.T) {
	t.Parallel()
	g, repo := fivemRepo(t)
	ctx := context.Background()
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	// Operator edits: activation, ordering, persistentPaths, mutableMode.
	cat, _ := readCatalog(t, repo)
	for i := range cat.Spec.Resources {
		r := &cat.Spec.Resources[i]
		switch r.Path {
		case "chat":
			r.Activation = domain.ActivationEnabled
			r.After = []string{"mapmanager"}
			r.Before = []string{"anticheat"}
			r.PersistentPaths = []string{"data/chatlog"}
		case "anticheat":
			r.Activation = domain.ActivationDisabled
			r.MutableMode = catalog.MutableCopy
		}
	}
	data, err := EncodeResourceCatalog(cat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo.CatalogPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Delete a resource, add a resource, change a manifest.
	if err := os.RemoveAll(filepath.Join(repo.Root, "multi-game")); err != nil {
		t.Fatal(err)
	}
	g.WriteFile("newres/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\nprovide 'newalias'\n"))
	g.WriteFile("anticheat/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\nserver_only 'yes'\ndependency 'chat'\nserver_script 'server/main.lua'\n"))

	out, err := repo.ResourcesSync(ctx, ResourcesSyncInput{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Added, ",") != "newres" || strings.Join(out.Missing, ",") != "multi-game" || strings.Join(out.Updated, ",") != "anticheat" || !out.Written {
		t.Fatalf("sync: %+v", out)
	}
	cat, raw := readCatalog(t, repo)
	if iss := schema.Validate("ResourceCatalog", raw); len(iss) > 0 {
		t.Fatalf("schema: %v", iss)
	}
	chat := entry(t, cat, "chat")
	if chat.Activation != domain.ActivationEnabled || strings.Join(chat.After, ",") != "mapmanager" || strings.Join(chat.Before, ",") != "anticheat" || strings.Join(chat.PersistentPaths, ",") != "data/chatlog" {
		t.Fatalf("operator fields lost: %+v", chat)
	}
	ac := entry(t, cat, "anticheat")
	if ac.Activation != domain.ActivationDisabled || ac.MutableMode != catalog.MutableCopy || strings.Join(ac.Dependencies, ",") != "chat" {
		t.Fatalf("anticheat: %+v", ac)
	}
	mg := entry(t, cat, "multi-game")
	if mg.Status != catalog.StatusMissing || mg.Activation != domain.ActivationPending {
		t.Fatalf("missing tombstone: %+v", mg)
	}
	nr := entry(t, cat, "newres")
	if nr.Activation != domain.ActivationPending || strings.Join(nr.Provides, ",") != "newalias" {
		t.Fatalf("new entry: %+v", nr)
	}
	// Tombstone survives another sync and is never deleted; revive when restored.
	out, err = repo.ResourcesSync(ctx, ResourcesSyncInput{})
	if err != nil || out.Written || strings.Join(out.Missing, ",") != "multi-game" {
		t.Fatalf("resync: %+v %v", out, err)
	}
	copyTree(t, filepath.Join(fivemFixtures, "collection", "multi-game"), filepath.Join(repo.Root, "multi-game"))
	out, err = repo.ResourcesSync(ctx, ResourcesSyncInput{})
	if err != nil || strings.Join(out.Updated, ",") != "multi-game" || len(out.Missing) != 0 {
		t.Fatalf("revive: %+v %v", out, err)
	}
	if e := entry(t, mustCat(t, repo), "multi-game"); e.Status != catalog.StatusPresent {
		t.Fatalf("revived: %+v", e)
	}
	// --new enabled applies to new entries only.
	g.WriteFile("another/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\n"))
	out, err = repo.ResourcesSync(ctx, ResourcesSyncInput{NewActivation: domain.ActivationEnabled})
	if err != nil || strings.Join(out.Added, ",") != "another" {
		t.Fatalf("--new: %+v %v", out, err)
	}
	cat = mustCat(t, repo)
	if entry(t, cat, "another").Activation != domain.ActivationEnabled || entry(t, cat, "newres").Activation != domain.ActivationPending {
		t.Fatalf("--new enabled leaked: %+v", cat.Spec.Resources)
	}
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{NewActivation: "maybe"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad --new: %v", err)
	}
}

func mustCat(t *testing.T, repo *Repo) *domain.ResourceCatalog {
	t.Helper()
	cat, _ := readCatalog(t, repo)
	return cat
}

func TestResourcesSyncFromCfgEnablesOnlyListedNames(t *testing.T) {
	t.Parallel()
	g, repo := fivemRepo(t)
	ctx := context.Background()
	g.WriteFile("cfg/server.cfg", []byte("ensure chat\nexec extra.cfg\nstop anticheat\nensure nosuch\n"))
	g.WriteFile("cfg/extra.cfg", []byte("start bank\nexec @vendor/x.cfg\n"))
	out, err := repo.ResourcesSync(ctx, ResourcesSyncInput{FromCfg: filepath.Join(repo.Root, "cfg", "server.cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Enabled, ",") != "[maps]/[interiors]/bank,chat" || len(out.Warnings) != 2 {
		t.Fatalf("from-cfg: %+v", out)
	}
	cat := mustCat(t, repo)
	for _, r := range cat.Spec.Resources {
		want := domain.ActivationPending
		if r.Path == "chat" || r.Path == "[maps]/[interiors]/bank" {
			want = domain.ActivationEnabled
		}
		if r.Activation != want {
			t.Errorf("%s: %s want %s", r.Path, r.Activation, want)
		}
	}
	// Disabled/enabled decisions are never changed by a later --from-cfg;
	// pending ones are enabled when the cfg starts them.
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "chat", Activation: domain.ActivationDisabled}); err != nil {
		t.Fatal(err)
	}
	g.WriteFile("cfg/server.cfg", []byte("ensure chat\nensure anticheat\n"))
	out, err = repo.ResourcesSync(ctx, ResourcesSyncInput{FromCfg: filepath.Join(repo.Root, "cfg", "server.cfg")})
	if err != nil || strings.Join(out.Enabled, ",") != "anticheat" {
		t.Fatalf("second from-cfg: %+v %v", out, err)
	}
	cat = mustCat(t, repo)
	if entry(t, cat, "chat").Activation != domain.ActivationDisabled || entry(t, cat, "anticheat").Activation != domain.ActivationEnabled {
		t.Fatalf("%+v", cat.Spec.Resources)
	}
	// Refusals surface before anything is written.
	g.WriteFile("cfg/bad.cfg", []byte("exec ../../../etc/passwd\n"))
	before := mustCat(t, repo)
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{FromCfg: filepath.Join(repo.Root, "cfg", "bad.cfg")}); !errors.Is(err, ErrCfg) {
		t.Fatalf("got %v", err)
	}
	if enc1, _ := EncodeResourceCatalog(before); !bytes.Equal(enc1, must(EncodeResourceCatalog(mustCat(t, repo)))) {
		t.Fatal("catalog changed after refused cfg")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestResourcesSet(t *testing.T) {
	t.Parallel()
	_, repo := fivemRepo(t)
	ctx := context.Background()
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "chat", Activation: domain.ActivationEnabled}); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("empty catalog: %v", err)
	}
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	out, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "[maps]/city-map/", Activation: domain.ActivationEnabled, After: []string{"mapmanager", "external-thing"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resource.Activation != domain.ActivationEnabled || strings.Join(out.Resource.After, ",") != "mapmanager,external-thing" || !out.Written {
		t.Fatalf("%+v", out)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], "external-thing") {
		t.Fatalf("warnings: %v", out.Warnings)
	}
	cat, raw := readCatalog(t, repo)
	if iss := schema.Validate("ResourceCatalog", raw); len(iss) > 0 {
		t.Fatalf("schema: %v", iss)
	}
	cm := entry(t, cat, "[maps]/city-map")
	if cm.Activation != domain.ActivationEnabled || strings.Join(cm.After, ",") != "mapmanager,external-thing" {
		t.Fatalf("%+v", cm)
	}
	for _, r := range cat.Spec.Resources {
		if r.Path != "[maps]/city-map" && r.Activation != domain.ActivationPending {
			t.Fatalf("other entry changed: %+v", r)
		}
	}
	// Clear ordering with an explicit empty list; nil leaves it alone.
	out, err = repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "[maps]/city-map", After: []string{}})
	if err != nil || len(out.Resource.After) != 0 || out.Resource.Activation != domain.ActivationEnabled {
		t.Fatalf("%+v %v", out, err)
	}
	// Idempotent: same edit again does not rewrite.
	out, err = repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "[maps]/city-map", Activation: domain.ActivationEnabled})
	if err != nil || out.Written {
		t.Fatalf("%+v %v", out, err)
	}
	// Refusals.
	var nf *ResourceNotFoundError
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "city-map", Activation: domain.ActivationEnabled}); !errors.As(err, &nf) || len(nf.Known) != 7 {
		t.Fatalf("unknown path: %v", err)
	}
	for _, in := range []ResourcesSetInput{
		{ResourcePath: "chat", Activation: "on"},
		{ResourcePath: "chat"},
		{ResourcePath: "chat", After: []string{"bad name"}},
		{ResourcePath: "chat", After: []string{"a", "a"}},
		{ResourcePath: "chat", Before: []string{"chat"}},
		{ResourcePath: "../chat", Activation: domain.ActivationEnabled},
		{ResourcePath: "", Activation: domain.ActivationEnabled},
	} {
		if _, err := repo.ResourcesSet(ctx, in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: got %v", in, err)
		}
	}
	// Sync after set preserves the operator's choice.
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	if entry(t, mustCat(t, repo), "[maps]/city-map").Activation != domain.ActivationEnabled {
		t.Fatal("set lost by sync")
	}
}

func TestResourcesValidateReleaseStyle(t *testing.T) {
	t.Parallel()
	g, repo := fivemRepo(t)
	ctx := context.Background()
	// No catalog yet: every discovered resource is undeclared.
	out, err := repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !errors.Is(err, ErrCatalogNotReleasable) || out == nil || out.Valid || len(out.Diff.Undeclared) != 7 {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	// Pending entries are not releasable.
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !errors.Is(err, ErrCatalogNotReleasable) || out.Valid || len(out.Problems) != 7 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, p := range out.Problems {
		if p.Field != "activation" {
			t.Fatalf("unexpected problem %+v", p)
		}
	}
	// Decide every entry: clean.
	for _, r := range mustCat(t, repo).Spec.Resources {
		if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: r.Path, Activation: domain.ActivationEnabled}); err != nil {
			t.Fatal(err)
		}
	}
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if err != nil || !out.Valid || len(out.Problems) != 0 || out.Resources != 7 {
		t.Fatalf("clean: %+v %v", out, err)
	}
	// External ordering reference: warning only; with KnownNames not even that.
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "chat", After: []string{"external-core"}}); err != nil {
		t.Fatal(err)
	}
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if err != nil || !out.Valid || len(out.Warnings) != 1 {
		t.Fatalf("external ref: %+v %v", out, err)
	}
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{KnownNames: []string{"external-core"}})
	if err != nil || len(out.Warnings) != 0 {
		t.Fatalf("known: %+v %v", out, err)
	}
	// Stale: manifest changed after the catalog was written.
	g.WriteFile("anticheat/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\nserver_script 'server/main.lua'\n"))
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !errors.Is(err, ErrCatalogNotReleasable) || !hasProblem(out, "anticheat", "serverOnly") {
		t.Fatalf("stale: %+v %v", out, err)
	}
	// Undeclared: new directory not in the catalog.
	g.WriteFile("late/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\n"))
	out, _ = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !hasProblem(out, "late", "undeclared") {
		t.Fatalf("undeclared: %+v", out)
	}
	// Missing: directory deleted (with and without a sync tombstone).
	if err := os.RemoveAll(filepath.Join(repo.Root, "chat")); err != nil {
		t.Fatal(err)
	}
	out, _ = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !hasProblem(out, "chat", "missing") {
		t.Fatalf("missing: %+v", out)
	}
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	out, _ = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !hasProblem(out, "chat", "missing") || hasProblem(out, "chat", "status") || !hasProblem(out, "late", "activation") {
		t.Fatalf("after sync: %+v", out)
	}
	// Ordering cycle and duplicate name are problems too.
	cat := mustCat(t, repo)
	for i := range cat.Spec.Resources {
		switch cat.Spec.Resources[i].Path {
		case "anticheat":
			cat.Spec.Resources[i].After = []string{"bank"}
		case "[maps]/[interiors]/bank":
			cat.Spec.Resources[i].After = []string{"anticheat"}
		}
	}
	data, _ := EncodeResourceCatalog(cat)
	if err := os.WriteFile(repo.CatalogPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if out == nil || out.Valid || !strings.Contains(fmt.Sprint(out.Problems), "cycle") {
		t.Fatalf("cycle: %+v", out)
	}
	// Schema-invalid YAML is reported as problems, never trusted.
	if err := os.WriteFile(repo.CatalogPath(), []byte("apiVersion: fxvcs.dev/v1alpha1\nkind: ResourceCatalog\nmetadata:\n  repositoryID: repo_fivem_01\nspec:\n  resources:\n    - path: chat\n      activation: null\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = repo.ResourcesValidate(ctx, ResourcesValidateInput{})
	if !errors.Is(err, ErrCatalogNotReleasable) || out.Valid || len(out.Problems) == 0 {
		t.Fatalf("schema: %+v %v", out, err)
	}
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("sync on invalid catalog must refuse: %v", err)
	}
}

func hasProblem(out *ResourcesValidateOutput, path, field string) bool {
	if out == nil {
		return false
	}
	for _, p := range out.Problems {
		if p.Path == path && p.Field == field {
			return true
		}
	}
	return false
}

func TestResourcesSandboxViolationFailsSyncAndValidate(t *testing.T) {
	t.Parallel()
	g, repo := fivemRepo(t)
	ctx := context.Background()
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(repo.CatalogPath())
	bad, err := os.ReadFile(filepath.Join(fivemFixtures, "limits", "forbidden-io.lua"))
	if err != nil {
		t.Fatal(err)
	}
	g.WriteFile("evil/fxmanifest.lua", bad)
	for name, run := range map[string]func() error{
		"sync":     func() error { _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); return err },
		"validate": func() error { _, err := repo.ResourcesValidate(ctx, ResourcesValidateInput{}); return err },
	} {
		err := run()
		var se *ResourceScanError
		if !errors.As(err, &se) || !errors.Is(err, ErrResourceScan) {
			t.Fatalf("%s: got %v", name, err)
		}
		if len(se.Manifests) != 1 || se.Manifests[0] != "evil/fxmanifest.lua" {
			t.Fatalf("%s: manifests %v", name, se.Manifests)
		}
	}
	after, _ := os.ReadFile(repo.CatalogPath())
	if !bytes.Equal(before, after) {
		t.Fatal("catalog written despite scan failure")
	}
}

func TestResourcesRequireFiveMProfile(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t) // generic profile
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); !errors.Is(err, ErrProfileRequired) || !strings.Contains(err.Error(), "fxvcs init --profile fivem") {
		t.Fatalf("sync: %v", err)
	}
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "x", Activation: domain.ActivationEnabled}); !errors.Is(err, ErrProfileRequired) {
		t.Fatalf("set: %v", err)
	}
	if _, err := repo.ResourcesValidate(ctx, ResourcesValidateInput{}); !errors.Is(err, ErrProfileRequired) {
		t.Fatalf("validate: %v", err)
	}
	// fivem profile without an accepted descriptor.
	var cfg map[string]any
	_ = yaml.Unmarshal([]byte(fivemRepoYAML), &cfg)
	spec := cfg["spec"].(map[string]any)
	delete(spec, "layout")
	delete(spec, "sourceRoot")
	data, _ := yaml.Marshal(cfg)
	r.WriteFile(".fxvcs/repository.yaml", data)
	repo2 := openTest(t, r, cacheDir)
	if _, err := repo2.ResourcesSync(ctx, ResourcesSyncInput{}); !errors.Is(err, ErrDescriptorMissing) {
		t.Fatalf("no descriptor: %v", err)
	}
}

func TestResourcesSyncSingleLayout(t *testing.T) {
	t.Parallel()
	gittest.RequireGit(t)
	g := gittest.InitRepo(t)
	copyTree(t, filepath.Join(fivemFixtures, "single-resource"), g.Root)
	g.WriteFile(".fxvcs/repository.yaml", []byte(strings.Replace(fivemRepoYAML, "layout: collection\n", "layout: single\n  resourceName: inventory\n", 1)))
	g.Commit("init")
	repo := openTest(t, g, t.TempDir())
	out, err := repo.ResourcesSync(context.Background(), ResourcesSyncInput{NewActivation: domain.ActivationEnabled})
	if err != nil || strings.Join(out.Added, ",") != "inventory" {
		t.Fatalf("%+v %v", out, err)
	}
	if v, err := repo.ResourcesValidate(context.Background(), ResourcesValidateInput{}); err != nil || !v.Valid {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestResourcesSyncSerializesOnCatalogLock(t *testing.T) {
	t.Parallel()
	g, repo := fivemRepo(t)
	ctx := context.Background()
	other := openTest(t, g, t.TempDir())
	h, err := other.Lock(ctx, journal.LockPath, repository.ResourcesPath)
	if err != nil {
		t.Fatal(err)
	}
	var busy *lock.BusyError
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); !errors.As(err, &busy) {
		t.Fatalf("sync under a held catalog lock: %v", err)
	}
	if _, err := repo.ResourcesSet(ctx, ResourcesSetInput{ResourcePath: "chat", Activation: domain.ActivationEnabled}); !errors.As(err, &busy) {
		t.Fatalf("set under a held catalog lock: %v", err)
	}
	if _, err := os.Stat(repo.CatalogPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("catalog written while lock was held elsewhere")
	}
	_ = h.Release()
	if _, err := repo.ResourcesSync(ctx, ResourcesSyncInput{}); err != nil {
		t.Fatal(err)
	}
}
