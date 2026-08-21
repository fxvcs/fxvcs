package repository

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/format"
)

const cfg = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_maps_01
  storageDomainID: domain_maps_01
  name: maps
spec:
  profile: fivem
  minimumFXVCSVersion: 0.1.0
  gitIntegrationVersion: 1
  sourceRoot: .
  layout: collection
  storage:
    remotes:
      - name: primary
        type: s3
        endpoint: https://objects.example.com
        bucket: fxvcs-assets
        prefix: repositories/domain_maps_01/
        requiredForPublication: true
`

func TestLoadConfigBytes(t *testing.T) {
	r, err := LoadConfigBytes([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if r.Metadata.RepositoryID != "repo_maps_01" || r.Spec.Layout != "collection" || len(r.Spec.Storage.Remotes) != 1 || !r.Spec.Storage.Remotes[0].RequiredForPublication {
		t.Fatalf("%+v", r)
	}
	if _, err := LoadConfigBytes([]byte(cfg + "  unknownField: 1\n")); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := LoadConfigBytes([]byte("apiVersion: fxvcs.dev/v9\nkind: Repository\n")); !errors.Is(err, format.ErrNewerAPIVersion) {
		t.Fatalf("got %v", err)
	}
	if _, err := LoadConfigBytes([]byte("apiVersion: fxvcs.dev/v1alpha1\nkind: Release\n")); !errors.Is(err, format.ErrUnknownKind) {
		t.Fatalf("got %v", err)
	}
	if _, err := LoadConfigBytes([]byte(": not yaml")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestLocateAndOpen(t *testing.T) {
	root := t.TempDir()
	if _, err := Locate(root); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("got %v", err)
	}
	_ = os.MkdirAll(filepath.Join(root, ".fxvcs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ConfigPath), []byte(cfg), 0o644)
	nested := filepath.Join(root, "a", "b")
	_ = os.MkdirAll(nested, 0o755)
	got, err := Locate(nested)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.Abs(root); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if _, _, err := Open(nested, format.MustParseVersion("0.1.0")); err != nil {
		t.Fatal(err)
	}
	_, _, err = Open(nested, format.MustParseVersion("0.0.9"))
	var ur *format.UpgradeRequiredError
	if !errors.As(err, &ur) {
		t.Fatalf("got %v", err)
	}
}
