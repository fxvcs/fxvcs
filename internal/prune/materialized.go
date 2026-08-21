package prune

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/materialize"
)

type realizationMetadata struct {
	Files []struct {
		Digest string `json:"digest"`
	} `json:"files"`
}

// ObserveMaterialized collects ordinary digest objects and every published or
// staged realization metadata reference. Any malformed retained metadata fails
// closed so copy-mode resources are never mistaken for unreferenced cache.
func ObserveMaterialized(deployRoot string) ([]MaterializedObject, error) {
	root := filepath.Join(deployRoot, layout.MaterializedDir)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return []MaterializedObject{}, nil
	} else if err != nil {
		return nil, err
	}
	references := map[string]map[string]bool{}
	if err := observeMetadataRoot(filepath.Join(deployRoot, layout.ReleasesDir), "", references); err != nil {
		return nil, err
	}
	if err := observeMetadataRoot(filepath.Join(deployRoot, layout.StateDir, "staging"), "staging:", references); err != nil {
		return nil, err
	}
	var out []MaterializedObject
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("prune: materialized entry %s is not an ordinary file", path)
		}
		hex := filepath.Base(path)
		digest := "sha256:" + hex
		expected, pathErr := layout.MaterializedPath(deployRoot, digest)
		if pathErr != nil || expected != path {
			return fmt.Errorf("prune: unexpected materialized entry %s", path)
		}
		refs := references[digest]
		item := MaterializedObject{Digest: digest, Path: path, Bytes: info.Size(), References: make([]string, 0, len(refs))}
		for ref := range refs {
			item.References = append(item.References, ref)
		}
		sort.Strings(item.References)
		out = append(out, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

func observeMetadataRoot(root, prefix string, references map[string]map[string]bool) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return fmt.Errorf("prune: unexpected realization entry %s", filepath.Join(root, entry.Name()))
		}
		metadataPath := filepath.Join(root, entry.Name(), layout.RuntimeDir, "realization.json")
		linked, err := os.Lstat(metadataPath)
		if err != nil || !linked.Mode().IsRegular() {
			return fmt.Errorf("prune: retained realization metadata is not an ordinary file: %w", err)
		}
		file, err := os.Open(metadataPath)
		if err != nil {
			return fmt.Errorf("prune: read retained realization metadata: %w", err)
		}
		opened, statErr := file.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(linked, opened) {
			_ = file.Close()
			return fmt.Errorf("prune: retained realization metadata changed while opening")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, 16<<20+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(data) > 16<<20 {
			return fmt.Errorf("prune: invalid retained realization metadata: %v", errors.Join(readErr, closeErr))
		}
		var metadata realizationMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return fmt.Errorf("prune: decode retained realization metadata: %w", err)
		}
		ref := prefix + entry.Name()
		for _, file := range metadata.Files {
			if !integrity.IsDigest(file.Digest) {
				continue
			}
			if references[file.Digest] == nil {
				references[file.Digest] = map[string]bool{}
			}
			references[file.Digest][ref] = true
		}
	}
	return nil
}

func applyMaterializedGC(plan Plan) error {
	if len(plan.MaterializedCandidates) == 0 {
		return nil
	}
	observed, err := ObserveMaterialized(plan.DeployRoot)
	if err != nil {
		return err
	}
	byDigest := make(map[string]MaterializedObject, len(observed))
	for _, object := range observed {
		byDigest[object.Digest] = object
	}
	store := materialize.Store{DeployRoot: plan.DeployRoot}
	for _, candidate := range plan.MaterializedCandidates {
		object, exists := byDigest[candidate.Digest]
		if !exists {
			if _, err := os.Lstat(candidate.Path); errors.Is(err, os.ErrNotExist) {
				continue
			}
			return ErrPlanChanged
		}
		if object.Path != candidate.Path || object.Bytes != candidate.Bytes || len(object.References) != 0 {
			return ErrPlanChanged
		}
		if err := store.Verify(candidate.Digest, candidate.Bytes); err != nil {
			return err
		}
		if err := os.Remove(candidate.Path); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(candidate.Path)); err != nil {
			return err
		}
	}
	return nil
}
