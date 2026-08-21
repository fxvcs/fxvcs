package templatehook

import (
	"context"
	"errors"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/secrets"
)

type fakeSecrets struct {
	calls int
	value secrets.Value
}

func (f *fakeSecrets) Resolve(context.Context, string, domain.SecretRef) (secrets.Value, error) {
	f.calls++
	return f.value, nil
}

func str(value string) *string { return &value }

func TestResolveInputsUsesExplicitPublicPathsAndSecretVersions(t *testing.T) {
	resolver := &fakeSecrets{value: secrets.Value{Plaintext: []byte("secret"), Version: "hmac-sha256:version"}}
	ctx := InputContext{
		Target:      domain.Target{Spec: domain.TargetSpec{Secrets: domain.SecretPolicy{DefaultProvider: "local"}}},
		Environment: domain.Environment{Spec: domain.EnvironmentSpec{Inputs: map[string]string{"hostname": "Example"}}},
		Release:     domain.Release{Metadata: domain.ReleaseMetadata{Name: "v1"}},
		Secrets:     resolver,
	}
	ref := &domain.SecretRef{Namespace: "fivem", Key: "license-key"}
	got, err := ResolveInputs(context.Background(), map[string]domain.InputSource{
		"host":    {Target: str("server.hostname")},
		"release": {Release: str("id")},
		"literal": {Literal: str("fixed")},
		"secretA": {Secret: ref},
		"secretB": {Secret: ref},
	}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ContainsSecret || got.SecretVersions["fivem/license-key"] != "hmac-sha256:version" || resolver.calls != 1 {
		t.Fatalf("bad secret resolution: %#v calls=%d", got, resolver.calls)
	}
	rendered, err := (Renderer{MaxOutputBytes: 128}).Render("inputs", []byte(`{{.Inputs.host}} {{.Inputs.release}} {{.Inputs.literal}} {{cfgQuote .Inputs.secretA}}`), got.Inputs)
	if err != nil || string(rendered) != `Example v1 fixed "secret"` {
		t.Fatalf("rendered=%q err=%v", rendered, err)
	}
}

func TestResolveInputsRefusesUnexposedOrMissingPaths(t *testing.T) {
	ctx := InputContext{Environment: domain.Environment{Spec: domain.EnvironmentSpec{Inputs: map[string]string{}}}}
	for _, source := range []domain.InputSource{{Target: str("spec.agentID")}, {Target: str("server.hostname")}, {Release: str("sourceSetDigest")}} {
		if _, err := ResolveInputs(context.Background(), map[string]domain.InputSource{"x": source}, ctx); !errors.Is(err, ErrInputSource) {
			t.Fatalf("source %#v: %v", source, err)
		}
	}
}
