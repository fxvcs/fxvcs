// Package runtimecheck validates Release runtime-resource requirements against
// an agent-bound installed system-resources inventory without claiming those
// target-provided bytes are Release-owned.
package runtimecheck

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

var ErrRuntimeRequirement = errors.New("runtimecheck: runtime requirement unavailable")

type Evidence struct {
	SystemResourcesDigest string
	Inventory             []string
	Verified              []string
}

// ValidateSystemResources proves enabled runtime:system entries exist as
// ordinary directories beneath a symlink-free installed inventory root.
func ValidateSystemResources(root string, resources []domain.RuntimeResource) (Evidence, error) {
	var required []domain.RuntimeResource
	for _, resource := range resources {
		if resource.Source == domain.ResourceSourceRuntime && resource.Activation == domain.ActivationEnabled {
			required = append(required, resource)
		}
	}
	if len(required) == 0 {
		return Evidence{Inventory: []string{}, Verified: []string{}}, nil
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Evidence{}, fmt.Errorf("%w: system resources root must be clean and absolute", ErrRuntimeRequirement)
	}
	if err := validateDirectoryPath(root); err != nil {
		return Evidence{}, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Evidence{}, err
	}
	inventory := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return Evidence{}, fmt.Errorf("%w: inventory entry %s is a symlink", ErrRuntimeRequirement, entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return Evidence{}, err
		}
		if info.IsDir() {
			inventory = append(inventory, entry.Name())
		}
	}
	sort.Strings(inventory)
	sort.Slice(required, func(i, j int) bool { return required[i].ID < required[j].ID })
	verified := make([]string, 0, len(required))
	for _, resource := range required {
		const prefix = "runtime:system/"
		if !strings.HasPrefix(resource.ID, prefix) {
			return Evidence{}, fmt.Errorf("%w: unsupported runtime provider in %s", ErrRuntimeRequirement, resource.ID)
		}
		relative, err := pathsafe.Normalize(strings.TrimPrefix(resource.ID, prefix))
		if err != nil {
			return Evidence{}, fmt.Errorf("%w: %s: %v", ErrRuntimeRequirement, resource.ID, err)
		}
		full, err := pathsafe.Join(root, relative)
		if err != nil {
			return Evidence{}, err
		}
		if err := validateDirectoryPath(full); err != nil {
			return Evidence{}, fmt.Errorf("%w: %s: %v", ErrRuntimeRequirement, resource.ID, err)
		}
		verified = append(verified, resource.ID)
	}
	digest, err := canonical.Digest(struct {
		FormatVersion int      `json:"formatVersion"`
		Inventory     []string `json:"inventory"`
	}{1, inventory})
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{SystemResourcesDigest: digest, Inventory: inventory, Verified: verified}, nil
}

func validateDirectoryPath(path string) error {
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume+string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: %s is not an ordinary directory", ErrRuntimeRequirement, current)
		}
	}
	return nil
}
