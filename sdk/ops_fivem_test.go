package sdk

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

const fivemFixtures = "../spec/fixtures/fivem"

const fivemRepoYAML = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_fivem_sdk
  storageDomainID: domain_fivem_sdk
  name: fivem
spec:
  profile: fivem
  minimumFXVCSVersion: 0.0.0-dev
  gitIntegrationVersion: 1
  sourceRoot: .
  layout: collection
  storage:
    remotes: []
`

func fivemCheckout(t *testing.T) *gittest.Repo {
	t.Helper()
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	src := filepath.Join(fivemFixtures, "collection")
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
	r.WriteFile(".fxvcs/repository.yaml", []byte(fivemRepoYAML))
	r.Commit("init")
	return r
}

func TestResourcesSyncSetValidateThroughSDK(t *testing.T) {
	r := fivemCheckout(t)
	var events []Event
	c, err := New(Options{InstalledVersion: "0.0.0-dev", CacheDir: t.TempDir(), Progress: func(ev Event) { events = append(events, ev) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	res, err := c.ResourcesSync(ctx, ResourcesSyncRequest{Path: filepath.Join(r.Root, "chat")})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 7 || len(res.Pending) != 7 || !res.Written || res.CatalogPath != ".fxvcs/resources.yaml" {
		t.Fatalf("%+v", res)
	}
	if len(events) < 2 || events[0].Kind != EventOperationStarted || events[len(events)-1].Kind != EventOperationFinished {
		t.Fatalf("events %+v", events)
	}
	// Idempotent through the SDK too.
	res, err = c.ResourcesSync(ctx, ResourcesSyncRequest{Path: r.Root})
	if err != nil || res.Written || len(res.Added) != 0 || res.Unchanged != 7 {
		t.Fatalf("%+v %v", res, err)
	}
	// Bad --new is invalid-argument.
	if _, err := c.ResourcesSync(ctx, ResourcesSyncRequest{Path: r.Root, NewActivation: "yes"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("%v", err)
	}

	// set: unknown path is not-found with known paths in details.
	_, err = c.ResourcesSet(ctx, ResourcesSetRequest{Path: r.Root, ResourcePath: "nope", Activation: "enabled"})
	var se *Error
	if !errors.As(err, &se) || se.Code != CodeNotFound || se.Details == nil {
		t.Fatalf("%v", err)
	}
	if _, err := c.ResourcesSet(ctx, ResourcesSetRequest{Path: r.Root, ResourcePath: "chat", Activation: "sometimes"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("%v", err)
	}
	if _, err := c.ResourcesSet(ctx, ResourcesSetRequest{Path: r.Root, ResourcePath: "../chat", Activation: "enabled"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("%v", err)
	}
	set, err := c.ResourcesSet(ctx, ResourcesSetRequest{Path: r.Root, ResourcePath: "chat", Activation: "enabled", After: []string{"mapmanager", "outside"}})
	if err != nil || set.Activation != "enabled" || set.Resource == nil || len(set.Warnings) != 1 || !set.Written {
		t.Fatalf("%+v %v", set, err)
	}

	// validate: pending entries make it not releasable, with details.
	v, err := c.ResourcesValidate(ctx, ResourcesValidateRequest{Path: r.Root})
	if CodeOf(err) != CodeValidation || v == nil || v.Valid || len(v.Problems) != 6 || len(v.Warnings) != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	if !errors.As(err, &se) || se.Details == nil {
		t.Fatalf("details missing: %v", err)
	}
	for _, p := range v.Problems {
		if _, err := c.ResourcesSet(ctx, ResourcesSetRequest{Path: r.Root, ResourcePath: p.Path, Activation: "disabled"}); err != nil {
			t.Fatal(err)
		}
	}
	v, err = c.ResourcesValidate(ctx, ResourcesValidateRequest{Path: r.Root, KnownNames: []string{"outside"}})
	if err != nil || !v.Valid || len(v.Warnings) != 0 || v.Resources != 7 {
		t.Fatalf("%+v %v", v, err)
	}

	// Sandbox violation: typed validation error naming the manifest, from both ops.
	bad, _ := os.ReadFile(filepath.Join(fivemFixtures, "limits", "forbidden-io.lua"))
	r.WriteFile("evil/fxmanifest.lua", bad)
	for _, op := range []func() error{
		func() error { _, e := c.ResourcesSync(ctx, ResourcesSyncRequest{Path: r.Root}); return e },
		func() error { _, e := c.ResourcesValidate(ctx, ResourcesValidateRequest{Path: r.Root}); return e },
	} {
		err := op()
		if !errors.As(err, &se) || se.Code != CodeValidation || !strings.Contains(se.Message, "evil/fxmanifest.lua") {
			t.Fatalf("%v", err)
		}
		issues, ok := se.Details.([]ValidationIssue)
		if !ok || len(issues) != 1 || issues[0].Path != "evil/fxmanifest.lua" {
			t.Fatalf("details %+v", se.Details)
		}
	}
	// A validate result is still returned when the catalog itself is invalid
	// (as opposed to the scan failing).
	_ = os.RemoveAll(filepath.Join(r.Root, "evil"))
	_ = os.RemoveAll(filepath.Join(r.Root, "chat"))
	v, err = c.ResourcesValidate(ctx, ResourcesValidateRequest{Path: r.Root})
	if CodeOf(err) != CodeValidation || v == nil || len(v.Missing) != 1 || v.Missing[0] != "chat" {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestResourcesOpsRequireFiveMProfile(t *testing.T) {
	if _, err := os.Stat(filepath.Join(fivemFixtures, "collection")); err != nil {
		t.Skip("fixtures not available")
	}
	gittest.RequireGit(t)
	dir := writeRepo(t, "0.0.0-dev", 1)
	c, _ := New(Options{InstalledVersion: "0.0.0-dev", CacheDir: t.TempDir()})
	ctx := context.Background()
	for name, op := range map[string]func() error{
		"sync": func() error { _, e := c.ResourcesSync(ctx, ResourcesSyncRequest{Path: dir}); return e },
		"set": func() error {
			_, e := c.ResourcesSet(ctx, ResourcesSetRequest{Path: dir, ResourcePath: "x", Activation: "enabled"})
			return e
		},
		"validate": func() error { _, e := c.ResourcesValidate(ctx, ResourcesValidateRequest{Path: dir}); return e },
	} {
		err := op()
		if CodeOf(err) != CodeInvalidArgument || !strings.Contains(err.Error(), "fxvcs init --profile fivem") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
