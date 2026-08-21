package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/credentials"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

// openTestWithConfig opens the fixture repository with an isolated credential
// directory, so a test never reads or writes the developer's real one.
func openTestWithConfig(t *testing.T, r *gittest.Repo, cacheDir, configDir string) *Repo {
	t.Helper()
	repo, err := Open(context.Background(), r.Root, Options{
		Installed: format.MustParseVersion("0.0.0-dev"), Operation: "test",
		CacheDir: cacheDir, ConfigDir: configDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	repo.Git = repo.Git.WithEnv(r.Env)
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestStorageAddS3DeclaresDescriptorWithoutSecrets(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTestWithConfig(t, r, cacheDir, t.TempDir())
	ctx := context.Background()

	v, err := repo.StorageAdd(ctx, StorageAddInput{
		Name: "cdn", Type: "s3", Bucket: "fxvcs-assets", Endpoint: "https://objects.example.com",
		Prefix: "repositories/team", Region: "auto", Addressing: "path", RequiredForPublication: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Committed || !v.Bound || v.Bucket != "fxvcs-assets" || v.Region != "auto" {
		t.Fatalf("%+v", v)
	}
	if v.HasCredentials {
		t.Fatal("a freshly declared remote should not claim stored credentials")
	}
	cfg, err := os.ReadFile(filepath.Join(repo.Root, ".fxvcs", "repository.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(cfg)
	for _, want := range []string{"name: cdn", "type: s3", "bucket: fxvcs-assets", "endpoint: https://objects.example.com", "prefix: repositories/team", "region: auto", "addressing: path"} {
		if !strings.Contains(text, want) {
			t.Fatalf("committed config missing %q:\n%s", want, text)
		}
	}
	// The committed file is a working-tree change for review, never a commit.
	if out := r.Git("log", "--oneline"); strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("storage add committed something: %s", out)
	}

	// Validation: a bucket is required, and the addressing/endpoint inputs are
	// checked before anything is written.
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "bad", Type: "s3"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "bad", Type: "s3", Bucket: "b", Addressing: "sideways"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "bad", Type: "s3", Bucket: "b", Endpoint: "objects.example.com"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("got %v", err)
	}
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "bad", Type: "gcs", Bucket: "b"}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("got %v", err)
	}
}

func TestStorageLoginStoresSecretsOutsideTheRepository(t *testing.T) {
	r, cacheDir := testRepo(t)
	configDir := t.TempDir()
	repo := openTestWithConfig(t, r, cacheDir, configDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "cdn", Type: "s3", Bucket: "assets"}); err != nil {
		t.Fatal(err)
	}

	res, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "cdn", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s3cr3t"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Profile != repo.Domain+"/cdn" {
		t.Fatalf("profile %q", res.Profile)
	}
	if res.AccessKeyID != "AKIAEXAMPLE" {
		t.Fatalf("result should echo the non-secret half: %+v", res)
	}
	data, err := os.ReadFile(res.CredentialsFile) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "s3cr3t") {
		t.Fatal("the secret was not stored")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(res.CredentialsFile)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("credential file mode is %#o, want 0600", info.Mode().Perm())
		}
	}
	// Nothing about the secret reaches the repository: not the working tree,
	// not the Git directory, not the local configuration.
	for _, root := range []string{repo.Root, repo.GitCommonDir} {
		if found := grepTree(t, root, "s3cr3t"); found != "" {
			t.Fatalf("the secret leaked into %s", found)
		}
	}

	// The remote now reports a stored login, still without exposing it.
	v, err := repo.remoteView("cdn")
	if err != nil {
		t.Fatal(err)
	}
	if !v.HasCredentials {
		t.Fatal("stored login not reported")
	}

	// Forgetting removes it.
	res, err = repo.StorageLogin(ctx, StorageLoginInput{Remote: "cdn", Forget: true})
	if err != nil || !res.Removed {
		t.Fatalf("%+v %v", res, err)
	}
	data, _ = os.ReadFile(res.CredentialsFile) //nolint:gosec // test-owned temp dir
	if strings.Contains(string(data), "s3cr3t") {
		t.Fatal("the secret survived --forget")
	}
}

func TestStorageLoginRefusals(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTestWithConfig(t, r, cacheDir, t.TempDir())
	ctx := context.Background()

	if _, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "nope", AccessKeyID: "a", SecretAccessKey: "b"}); !errors.Is(err, ErrNoSuchRemote) {
		t.Fatalf("got %v", err)
	}
	// "primary" is the committed filesystem remote of the fixture.
	if _, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "primary", AccessKeyID: "a", SecretAccessKey: "b"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a filesystem remote needs no credentials: %v", err)
	}
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "cdn", Type: "s3", Bucket: "assets"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "cdn", AccessKeyID: "a"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("an empty secret must be refused: %v", err)
	}
}

func TestS3CredentialResolutionOrder(t *testing.T) {
	r, cacheDir := testRepo(t)
	configDir := t.TempDir()
	repo := openTestWithConfig(t, r, cacheDir, configDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "cdn", Type: "s3", Bucket: "assets"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "cdn", AccessKeyID: "stored", SecretAccessKey: "stored-secret"}); err != nil {
		t.Fatal(err)
	}

	got := func() string {
		c, err := repo.s3Credentials("cdn", repo.localRemote("cdn"))
		if err != nil {
			t.Fatal(err)
		}
		return c.AccessKeyID
	}
	if got() != "stored" {
		t.Fatalf("stored login not used: %q", got())
	}
	t.Setenv("FXVCS_S3_ACCESS_KEY_ID", "generic")
	t.Setenv("FXVCS_S3_SECRET_ACCESS_KEY", "generic-secret")
	if got() != "generic" {
		t.Fatalf("unscoped environment keys should beat a stored login: %q", got())
	}
	t.Setenv("FXVCS_S3_CDN_ACCESS_KEY_ID", "scoped")
	t.Setenv("FXVCS_S3_CDN_SECRET_ACCESS_KEY", "scoped-secret")
	if got() != "scoped" {
		t.Fatalf("remote-scoped environment keys should win: %q", got())
	}
}

func TestS3ConfigMergesCommittedAndLocalValues(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTestWithConfig(t, r, cacheDir, t.TempDir())
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{
		Name: "cdn", Type: "s3", Bucket: "assets", Endpoint: "https://objects.example.com", Region: "eu-west-1",
	}); err != nil {
		t.Fatal(err)
	}
	// A machine behind an in-office mirror overrides only the endpoint.
	if _, err := repo.StorageAdd(ctx, StorageAddInput{
		Name: "cdn", Type: "s3", Bucket: "assets", Endpoint: "https://mirror.lan:9000", Addressing: "path",
	}); err != nil {
		t.Fatal(err)
	}
	v, err := repo.remoteView("cdn")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.s3Config(v)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://mirror.lan:9000" {
		t.Fatalf("local endpoint override ignored: %q", cfg.Endpoint)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("committed region should survive a local override of something else: %q", cfg.Region)
	}
	if cfg.Bucket != "assets" || cfg.Addressing != "path" {
		t.Fatalf("%+v", cfg)
	}
	// The committed file still describes the shared location, not this
	// machine's mirror.
	data, err := os.ReadFile(filepath.Join(repo.Root, ".fxvcs", "repository.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mirror.lan") {
		t.Fatalf("a machine-local endpoint leaked into committed configuration:\n%s", data)
	}
}

func TestCredentialStoreRefusesOpenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not comparable on Windows")
	}
	dir := t.TempDir()
	store, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("domain_x/cdn", credentials.Entry{Type: "s3", AccessKeyID: "a", SecretAccessKey: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("domain_x/cdn"); !errors.Is(err, credentials.ErrPermissions) {
		t.Fatalf("got %v, want ErrPermissions", err)
	}
}

// grepTree returns the first file under root whose bytes contain needle.
func grepTree(t *testing.T, root, needle string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil //nolint:nilerr // unreadable entries are not the subject of this check
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // test-owned temp tree
		if rerr == nil && strings.Contains(string(b), needle) {
			found = path
		}
		return nil
	})
	return found
}
