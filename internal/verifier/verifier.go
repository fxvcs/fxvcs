// Package verifier resolves portable external-resource contracts to explicit
// agent-local implementations. Verification is observe-only: adapters cannot
// install, start, stop, or mutate resources.
package verifier

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
)

var ErrUnsupported = errors.New("verifier: implementation unavailable")

type Observation struct {
	ResourceID            string `json:"resourceID"`
	Contract              string `json:"contract"`
	ContractVersion       int    `json:"contractVersion"`
	Implementation        string `json:"implementation"`
	ImplementationVersion int    `json:"implementationVersion"`
	EvidenceSource        string `json:"evidenceSource"`
}

// Adapter is a typed, bounded host implementation. EndpointRef is passed only
// to the adapter and is excluded from realization identity and evidence.
type Adapter interface {
	Implementation() string
	Version() int
	Verify(context.Context, domain.RuntimeResource, domain.VerifierBinding) (string, error)
}

type Registry map[string]Adapter

// VerifyExternal verifies every enabled external resource in stable ID order
// and returns a safe canonical evidence digest per resource.
func VerifyExternal(ctx context.Context, resources []domain.RuntimeResource, bindings map[string]domain.VerifierBinding, registry Registry) (map[string]string, error) {
	selected := make([]domain.RuntimeResource, 0)
	for _, resource := range resources {
		if resource.Source == domain.ResourceSourceExternal && resource.Activation == domain.ActivationEnabled {
			selected = append(selected, resource)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	out := make(map[string]string, len(selected))
	for _, resource := range selected {
		if resource.Verifier == nil || resource.Lifecycle == nil || resource.Lifecycle.Ownership != "external" || resource.Lifecycle.Actions != "observe-only" {
			return nil, fmt.Errorf("%w: %s lacks an observe-only external contract", ErrUnsupported, resource.ID)
		}
		binding, ok := bindings[resource.Verifier.Contract]
		if !ok {
			return nil, fmt.Errorf("%w: %s contract %s has no target binding", ErrUnsupported, resource.ID, resource.Verifier.Contract)
		}
		adapter := registry[binding.Implementation]
		if adapter == nil || adapter.Implementation() != binding.Implementation || adapter.Version() != binding.ImplementationVersion {
			return nil, fmt.Errorf("%w: %s needs %s v%d", ErrUnsupported, resource.ID, binding.Implementation, binding.ImplementationVersion)
		}
		source, err := adapter.Verify(ctx, resource, binding)
		if err != nil {
			return nil, fmt.Errorf("verifier: %s: %w", resource.ID, err)
		}
		if source == "" {
			return nil, fmt.Errorf("verifier: %s returned empty evidence source", resource.ID)
		}
		digest, err := canonical.Digest(Observation{ResourceID: resource.ID, Contract: resource.Verifier.Contract, ContractVersion: resource.Verifier.Version, Implementation: binding.Implementation, ImplementationVersion: binding.ImplementationVersion, EvidenceSource: source})
		if err != nil {
			return nil, err
		}
		out[resource.ID] = digest
	}
	return out, nil
}
