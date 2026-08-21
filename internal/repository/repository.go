// Package repository locates and opens an FXVCS repository checkout and runs
// the mandatory open-preflight: envelope/version negotiation and the
// minimumFXVCSVersion / gitIntegrationVersion gate. Every SDK operation, the
// CLI, the Git filter/diff/merge drivers, and the agent call Preflight before
// touching any state.
package repository

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

// Committed file locations (working tree, relative to the repository root).
const (
	ConfigPath    = ".fxvcs/repository.yaml"
	ResourcesPath = ".fxvcs/resources.yaml"
)

// ErrNotRepository is returned when no .fxvcs/repository.yaml is found.
var ErrNotRepository = errors.New("repository: not an fxvcs repository (no .fxvcs/repository.yaml)")

// Locate walks up from start until it finds ConfigPath and returns the root.
func Locate(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, ConfigPath)); err == nil && !st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotRepository
		}
		dir = parent
	}
}

// LoadConfigBytes parses committed repository configuration. It validates the
// envelope before decoding the body so an unknown/newer apiVersion is rejected
// with a typed error rather than a field error.
func LoadConfigBytes(data []byte) (*domain.Repository, error) {
	var env format.Envelope
	if err := yaml.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("repository: parse envelope: %w", err)
	}
	if err := format.ValidateEnvelope(env, format.KindRepository); err != nil {
		return nil, err
	}
	var repo domain.Repository
	dec := yaml.NewDecoder(bytesReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&repo); err != nil {
		return nil, fmt.Errorf("repository: parse %s: %w", ConfigPath, err)
	}
	return &repo, nil
}

// LoadConfig reads and parses ConfigPath under root.
func LoadConfig(root string) (*domain.Repository, error) {
	data, err := os.ReadFile(filepath.Join(root, ConfigPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotRepository
		}
		return nil, err
	}
	return LoadConfigBytes(data)
}

// Preflight enforces the client gate. On failure it returns
// *format.UpgradeRequiredError (or a format sentinel) and the caller must not
// proceed with any normal operation, migration, filter, or agent work.
func Preflight(root string, repo *domain.Repository, installed format.Version) error {
	if err := format.CheckMinimumVersion(installed, repo.Spec.MinimumFXVCSVersion, root); err != nil {
		return err
	}
	if repo.Spec.GitIntegrationVersion != 0 {
		if err := format.CheckGitIntegrationGeneration(repo.Spec.GitIntegrationVersion, root, installed); err != nil {
			return err
		}
	}
	return nil
}

// Open locates, loads, and preflights in one call.
func Open(start string, installed format.Version) (root string, repo *domain.Repository, err error) {
	root, err = Locate(start)
	if err != nil {
		return "", nil, err
	}
	repo, err = LoadConfig(root)
	if err != nil {
		return root, nil, err
	}
	if err := Preflight(root, repo, installed); err != nil {
		return root, repo, err
	}
	return root, repo, nil
}
