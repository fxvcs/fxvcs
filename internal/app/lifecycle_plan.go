package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/reconcile"
)

const maxLifecycleMetadataBytes = 16 << 20

func (c *Coord) lifecyclePlan(ctx context.Context, deployRoot, current string, desired domain.RealizationInputs, target domain.Target, next domain.Release) (reconcile.LifecycleMode, []reconcile.ResourceOperation, error) {
	if current == "" {
		return reconcile.LifecycleFullRestart, nil, nil
	}
	currentInputs, err := readRealizationInputs(deployRoot, current)
	if err != nil {
		return "", nil, fmt.Errorf("reconcile: read current lifecycle evidence: %w", err)
	}
	if !sameNonReleaseInputs(currentInputs, desired) {
		return reconcile.LifecycleFullRestart, nil, nil
	}
	previousTargetView := target
	previousTargetView.Spec.Release = currentInputs.ReleaseName
	previousTargetDigest, err := canonical.Digest(previousTargetView)
	if err != nil || previousTargetDigest != currentInputs.TargetContentDigest {
		return reconcile.LifecycleFullRestart, nil, nil
	}
	previous, err := c.GetRelease(ctx, currentInputs.ReleaseName)
	if err != nil {
		// A removed historical Release prevents a precise resource diff. A full
		// restart remains safe and does not infer hot operations.
		return reconcile.LifecycleFullRestart, nil, nil
	}
	operations, hot, err := diffComponentResources(previous, next)
	if err != nil {
		return "", nil, err
	}
	if !hot {
		return reconcile.LifecycleFullRestart, nil, nil
	}
	if len(operations) == 0 {
		return reconcile.LifecycleNone, nil, nil
	}
	return reconcile.LifecycleHotReload, operations, nil
}

func readRealizationInputs(deployRoot, realizationID string) (domain.RealizationInputs, error) {
	root := filepath.Join(deployRoot, layout.ReleasesDir, realizationID)
	runtimeDir := filepath.Join(root, layout.RuntimeDir)
	metadataPath := filepath.Join(runtimeDir, "realization.json")
	for _, directory := range []string{root, runtimeDir} {
		info, err := os.Lstat(directory)
		if err != nil {
			return domain.RealizationInputs{}, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return domain.RealizationInputs{}, fmt.Errorf("%s is not an ordinary directory", directory)
		}
	}
	before, err := os.Lstat(metadataPath)
	if err != nil {
		return domain.RealizationInputs{}, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > maxLifecycleMetadataBytes {
		return domain.RealizationInputs{}, errors.New("realization metadata is not a bounded ordinary file")
	}
	file, err := os.Open(metadataPath)
	if err != nil {
		return domain.RealizationInputs{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return domain.RealizationInputs{}, errors.New("realization metadata changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLifecycleMetadataBytes+1))
	if err != nil {
		return domain.RealizationInputs{}, err
	}
	if len(data) > maxLifecycleMetadataBytes {
		return domain.RealizationInputs{}, errors.New("realization metadata exceeds the bounded size")
	}
	var metadata AssemblyMetadata
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&metadata); err != nil {
		return domain.RealizationInputs{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return domain.RealizationInputs{}, errors.New("realization metadata contains trailing JSON")
		}
		return domain.RealizationInputs{}, fmt.Errorf("realization metadata trailing data: %w", err)
	}
	if metadata.FormatVersion != assemblyMetadataVersion || metadata.RealizationID != realizationID {
		return domain.RealizationInputs{}, errors.New("realization metadata identity mismatch")
	}
	return metadata.Inputs, nil
}

func sameNonReleaseInputs(left, right domain.RealizationInputs) bool {
	left.ReleaseName, left.ReleaseDigest, left.SourceSetDigest, left.TargetContentDigest = "", "", "", ""
	right.ReleaseName, right.ReleaseDigest, right.SourceSetDigest, right.TargetContentDigest = "", "", "", ""
	return reflect.DeepEqual(left, right)
}

func diffComponentResources(previous, next domain.Release) ([]reconcile.ResourceOperation, bool, error) {
	previousHostOwned := hostOwnedResources(previous.Spec.Resources)
	nextHostOwned := hostOwnedResources(next.Spec.Resources)
	if !reflect.DeepEqual(previousHostOwned, nextHostOwned) || !reflect.DeepEqual(previous.Spec.RuntimeResources, next.Spec.RuntimeResources) {
		return nil, false, nil
	}
	previousCommits := componentCommits(previous.Spec.Components)
	nextCommits := componentCommits(next.Spec.Components)
	old := componentResourceMap(previous.Spec.Resources)
	newResources := componentResourceMap(next.Spec.Resources)

	type orderedOperation struct {
		operation reconcile.ResourceOperation
		order     int
	}
	var stops, starts []orderedOperation
	for name, oldResource := range old {
		newResource, exists := newResources[name]
		oldEnabled := oldResource.Activation == domain.ActivationEnabled
		newEnabled := exists && newResource.Activation == domain.ActivationEnabled
		if oldEnabled && !newEnabled {
			stops = append(stops, orderedOperation{operation: reconcile.ResourceOperation{Action: "stop", Name: name}, order: oldResource.EnsureOrder})
			continue
		}
		if oldEnabled && newEnabled && (!reflect.DeepEqual(oldResource, newResource) || previousCommits[oldResource.RepositoryID] != nextCommits[newResource.RepositoryID]) {
			starts = append(starts, orderedOperation{operation: reconcile.ResourceOperation{Action: "restart", Name: name}, order: newResource.EnsureOrder})
		}
	}
	for name, newResource := range newResources {
		oldResource, exists := old[name]
		oldEnabled := exists && oldResource.Activation == domain.ActivationEnabled
		if newResource.Activation == domain.ActivationEnabled && !oldEnabled {
			starts = append(starts, orderedOperation{operation: reconcile.ResourceOperation{Action: "ensure", Name: name}, order: newResource.EnsureOrder})
		}
	}
	sort.Slice(stops, func(i, j int) bool {
		if stops[i].order == stops[j].order {
			return stops[i].operation.Name > stops[j].operation.Name
		}
		return stops[i].order > stops[j].order
	})
	sort.Slice(starts, func(i, j int) bool {
		if starts[i].order == starts[j].order {
			return starts[i].operation.Name < starts[j].operation.Name
		}
		return starts[i].order < starts[j].order
	})
	operations := []reconcile.ResourceOperation{{Action: "refresh"}}
	for _, item := range stops {
		operations = append(operations, item.operation)
	}
	for _, item := range starts {
		operations = append(operations, item.operation)
	}
	if len(operations) == 1 {
		return nil, true, nil
	}
	return operations, true, nil
}

func hostOwnedResources(resources []domain.ReleaseResource) []domain.ReleaseResource {
	var out []domain.ReleaseResource
	for _, resource := range resources {
		if resource.Source != domain.ResourceSourceComponent {
			out = append(out, resource)
		}
	}
	return out
}

func componentResourceMap(resources []domain.ReleaseResource) map[string]domain.ReleaseResource {
	out := map[string]domain.ReleaseResource{}
	for _, resource := range resources {
		if resource.Source == domain.ResourceSourceComponent {
			out[resource.Name] = resource
		}
	}
	return out
}

func componentCommits(components []domain.ReleaseComponent) map[string]string {
	out := make(map[string]string, len(components))
	for _, component := range components {
		out[component.RepositoryID] = component.Commit
	}
	return out
}
