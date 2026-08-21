package sdk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeRepo(t *testing.T, minimum string, gitGen int) string {
	t.Helper()
	dir := t.TempDir()
	if git, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command(git, "init", "-q", dir)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogitconfig"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fxvcs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "apiVersion: fxvcs.dev/v1alpha1\nkind: Repository\nmetadata:\n  repositoryID: repo_test_01\n  storageDomainID: domain_test_01\n  name: test\nspec:\n  profile: generic\n  minimumFXVCSVersion: \"" + minimum + "\"\n  gitIntegrationVersion: " + itoa(gitGen) + "\n  storage:\n    remotes: []\n"
	if err := os.WriteFile(filepath.Join(dir, ".fxvcs", "repository.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestCompatibilityOK(t *testing.T) {
	dir := writeRepo(t, "0.1.0", 1)
	c, err := New(Options{InstalledVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Compatibility(context.Background(), CompatibilityRequest{Path: filepath.Join(dir, "sub", "dir")})
	if err != nil {
		t.Fatal(err)
	}
	if res.UpgradeRequired != nil || res.RepositoryID != "repo_test_01" {
		t.Fatalf("unexpected %+v", res)
	}
}

// TestBelowMinimumClientCanOnlyReportUpgrade proves the client gate:
// a client below minimumFXVCSVersion reports the typed requirement and every
// other operation returns CodeUpgradeRequired without doing work.
func TestBelowMinimumClientCanOnlyReportUpgrade(t *testing.T) {
	dir := writeRepo(t, "9.0.0", 1)
	c, _ := New(Options{InstalledVersion: "0.1.0"})
	ctx := context.Background()
	res, err := c.Compatibility(ctx, CompatibilityRequest{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.UpgradeRequired == nil || res.UpgradeRequired.RequiredVersion != "9.0.0" || res.UpgradeRequired.InstalledVersion != "0.1.0" {
		t.Fatalf("expected upgrade requirement, got %+v", res)
	}
	ops := map[string]func() error{
		"setup":           func() error { _, e := c.Setup(ctx, SetupRequest{Path: dir}); return e },
		"upgrade":         func() error { _, e := c.Upgrade(ctx, UpgradeRequest{Path: dir, Mode: UpgradeCheck}); return e },
		"track":           func() error { _, e := c.Track(ctx, TrackRequest{Path: dir}); return e },
		"untrack":         func() error { _, e := c.Untrack(ctx, UntrackRequest{Path: dir}); return e },
		"tracking-check":  func() error { _, e := c.TrackingCheck(ctx, TrackingCheckRequest{Path: dir}); return e },
		"scan":            func() error { _, e := c.Scan(ctx, ScanRequest{Path: dir}); return e },
		"migrate":         func() error { _, e := c.Migrate(ctx, MigrateRequest{Path: dir}); return e },
		"hydrate":         func() error { _, e := c.Hydrate(ctx, HydrateRequest{Path: dir}); return e },
		"dehydrate":       func() error { _, e := c.Dehydrate(ctx, DehydrateRequest{Path: dir}); return e },
		"status":          func() error { _, e := c.Status(ctx, StatusRequest{Path: dir}); return e },
		"verify":          func() error { _, e := c.Verify(ctx, VerifyRequest{Path: dir}); return e },
		"doctor":          func() error { _, e := c.Doctor(ctx, DoctorRequest{Path: dir}); return e },
		"repair":          func() error { _, e := c.Repair(ctx, RepairRequest{Path: dir}); return e },
		"objects-status":  func() error { _, e := c.ObjectsStatus(ctx, ObjectsStatusRequest{Path: dir}); return e },
		"summary":         func() error { _, e := c.PublicationSummary(ctx, PublicationSummaryRequest{Path: dir}); return e },
		"objects-publish": func() error { _, e := c.ObjectsPublish(ctx, ObjectsPublishRequest{Path: dir}); return e },
		"cache-status":    func() error { _, e := c.CacheStatus(ctx, CacheStatusRequest{Path: dir}); return e },
		"cache-prune":     func() error { _, e := c.CachePrune(ctx, CachePruneRequest{Path: dir}); return e },
		"storage-add":     func() error { _, e := c.StorageAdd(ctx, StorageAddRequest{Path: dir}); return e },
		"storage-list":    func() error { _, e := c.StorageList(ctx, StorageListRequest{Path: dir}); return e },
		"storage-test":    func() error { _, e := c.StorageTest(ctx, StorageTestRequest{Path: dir}); return e },
		"resources-sync":  func() error { _, e := c.ResourcesSync(ctx, ResourcesSyncRequest{Path: dir}); return e },
		"resources-set":   func() error { _, e := c.ResourcesSet(ctx, ResourcesSetRequest{Path: dir}); return e },
	}
	for name, op := range ops {
		err := op()
		var se *Error
		if !errors.As(err, &se) || se.Code != CodeUpgradeRequired {
			t.Errorf("%s: expected upgrade-required, got %v", name, err)
			continue
		}
		if _, ok := se.Details.(*UpgradeRequired); !ok {
			t.Errorf("%s: missing typed details", name)
		}
	}
}

func TestNewerGitIntegrationGenerationIsUpgradeRequired(t *testing.T) {
	dir := writeRepo(t, "0.1.0", 2)
	c, _ := New(Options{InstalledVersion: "0.1.0"})
	res, err := c.Compatibility(context.Background(), CompatibilityRequest{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.UpgradeRequired == nil || res.UpgradeRequired.Reason != "gitIntegrationVersion" {
		t.Fatalf("got %+v", res.UpgradeRequired)
	}
}

func TestNewerAPIVersionRejectedBeforeWrites(t *testing.T) {
	dir := writeRepo(t, "0.1.0", 1)
	p := filepath.Join(dir, ".fxvcs", "repository.yaml")
	b, _ := os.ReadFile(p)
	b = []byte("apiVersion: fxvcs.dev/v2alpha1\n" + string(b)[len("apiVersion: fxvcs.dev/v1alpha1\n"):])
	_ = os.WriteFile(p, b, 0o644)
	c, _ := New(Options{InstalledVersion: "0.1.0"})
	_, err := c.Compatibility(context.Background(), CompatibilityRequest{Path: dir})
	if CodeOf(err) != CodeUpgradeRequired {
		t.Fatalf("got %v", err)
	}
	_, err = c.Hydrate(context.Background(), HydrateRequest{Path: dir})
	if CodeOf(err) != CodeUpgradeRequired {
		t.Fatalf("got %v", err)
	}
}

func TestCancellationAndArgumentErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := writeRepo(t, "0.1.0", 1)
	c, _ := New(Options{InstalledVersion: "0.1.0"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Hydrate(ctx, HydrateRequest{Path: dir})
	if CodeOf(err) != CodeCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Compatibility(context.Background(), CompatibilityRequest{Path: t.TempDir()}); CodeOf(err) != CodeNotRepository {
		t.Fatalf("got %v", err)
	}
	if _, err := New(Options{InstalledVersion: "nope"}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("got %v", err)
	}
}
