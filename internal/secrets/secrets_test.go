package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestEnvAndEnvfileResolutionUsesKeyedVersions(t *testing.T) {
	key := make([]byte, 32)
	ref := domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}
	envfile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(envfile, []byte("RCON_FILE=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envfile, _ = filepath.EvalSymlinks(envfile)
	resolver, err := NewResolver(map[string]domain.SecretProvider{
		"process": {Type: "env", Aliases: map[string]string{"fivem/rcon-password": "RCON_ENV"}},
		"file":    {Type: "envfile", Path: envfile, Aliases: map[string]string{"fivem/rcon-password": "RCON_FILE"}},
	}, key, func(name string) (string, bool) { return "from-env", name == "RCON_ENV" })
	if err != nil {
		t.Fatal(err)
	}
	env, err := resolver.Resolve(context.Background(), "process", ref)
	if err != nil || string(env.Plaintext) != "from-env" {
		t.Fatalf("env resolve: %#v %v", env, err)
	}
	file, err := resolver.Resolve(context.Background(), "file", ref)
	if err != nil || string(file.Plaintext) != "from-file" {
		t.Fatalf("file resolve: %#v %v", file, err)
	}
	if env.Version == file.Version || env.Version[:12] != "hmac-sha256:" {
		t.Fatalf("unexpected versions: %q %q", env.Version, file.Version)
	}
}

func TestAutomaticProviderNamesAndTargetBoundVersions(t *testing.T) {
	key := make([]byte, 32)
	ref := domain.SecretRef{Namespace: "fivem", Key: "license-key"}
	envfile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(envfile, []byte("FIVEM__LICENSE_KEY=file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envfile, _ = filepath.EvalSymlinks(envfile)
	providers := map[string]domain.SecretProvider{"process": {Type: "env"}, "file": {Type: "envfile", Path: envfile}}
	lookup := func(name string) (string, bool) { return "env-value", name == "FXVCS_SECRET_DEV__FIVEM__LICENSE_KEY" }
	dev, err := NewTargetResolver("dev", providers, key, lookup)
	if err != nil {
		t.Fatal(err)
	}
	fromEnv, err := dev.Resolve(context.Background(), "process", ref)
	if err != nil || string(fromEnv.Plaintext) != "env-value" {
		t.Fatalf("automatic env: %#v %v", fromEnv, err)
	}
	fromFile, err := dev.Resolve(context.Background(), "file", ref)
	if err != nil || string(fromFile.Plaintext) != "file-value" {
		t.Fatalf("automatic envfile: %#v %v", fromFile, err)
	}
	prd, _ := NewTargetResolver("prd", providers, key, func(string) (string, bool) { return "env-value", true })
	other, err := prd.Resolve(context.Background(), "process", ref)
	if err != nil || other.Version == fromEnv.Version {
		t.Fatalf("target scope did not bind secret version: %#v %v", other, err)
	}
}

func TestEnvfilePermissionsAndSymlinksFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "open.env")
	if err := os.WriteFile(path, []byte("TOKEN=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readEnvFile(path)
	if !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("world-readable envfile accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvFile(link); !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("symlink envfile accepted: %v", err)
	}
}

func TestEnvfileRejectsForbiddenRootAndParentSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "safe", "secret.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvfile(path, []string{root}); !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("forbidden-root envfile accepted: %v", err)
	}
	outside := t.TempDir()
	linkedRoot := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linkedRoot); err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(linkedRoot, "secret.env")
	if err := os.WriteFile(filepath.Join(outside, "secret.env"), []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvfile(linkedPath, nil); !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("parent symlink accepted: %v", err)
	}
}

func TestHMACKeyCreationAndUnsafeExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "secret-hmac.key")
	a, err := LoadOrCreateHMACKey(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateHMACKey(path)
	if err != nil || string(a) != string(b) || len(a) != 32 {
		t.Fatalf("key round trip failed: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateHMACKey(path); !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("unsafe key accepted: %v", err)
	}
}
