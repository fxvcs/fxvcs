package verifier

import (
	"context"
	"errors"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

type fakeAdapter struct{ version int }

func (fakeAdapter) Implementation() string { return "rcon-resource-state" }
func (a fakeAdapter) Version() int         { return a.version }
func (fakeAdapter) Verify(context.Context, domain.RuntimeResource, domain.VerifierBinding) (string, error) {
	return "typed-test-rcon", nil
}

func external() domain.RuntimeResource {
	return domain.RuntimeResource{ID: "external:host/custom", Source: domain.ResourceSourceExternal, Activation: domain.ActivationEnabled, Verifier: &domain.VerifierContract{Contract: "resource-present", Version: 1}, Lifecycle: &domain.LifecycleContract{Ownership: "external", Actions: "observe-only"}}
}

func TestVerifyExternalRequiresExactAllowlistedImplementation(t *testing.T) {
	binding := map[string]domain.VerifierBinding{"resource-present": {Implementation: "rcon-resource-state", ImplementationVersion: 1, EndpointRef: "local-rcon"}}
	evidence, err := VerifyExternal(context.Background(), []domain.RuntimeResource{external()}, binding, Registry{"rcon-resource-state": fakeAdapter{version: 1}})
	if err != nil || evidence["external:host/custom"] == "" {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	if _, err := VerifyExternal(context.Background(), []domain.RuntimeResource{external()}, binding, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing implementation accepted: %v", err)
	}
	if _, err := VerifyExternal(context.Background(), []domain.RuntimeResource{external()}, binding, Registry{"rcon-resource-state": fakeAdapter{version: 2}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wrong implementation version accepted: %v", err)
	}
}
