package secrets

import (
	"errors"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestEnvfileSkeletonsAreValueFreeAndTraceable(t *testing.T) {
	target := domain.Target{Metadata: domain.NamedMetadata{Name: "dev"}, Spec: domain.TargetSpec{
		Secrets: domain.SecretPolicy{DefaultProvider: "local", Namespaces: map[string]domain.NamespaceProvider{"cloud": {Provider: "managed"}}},
		Hooks: &domain.HookRequests{PostMaterialize: []domain.HookRequest{{Inputs: map[string]domain.InputSource{
			"license": {Secret: &domain.SecretRef{Namespace: "fivem", Key: "license-key"}},
			"rcon":    {Secret: &domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}},
			"remote":  {Secret: &domain.SecretRef{Namespace: "cloud", Key: "token"}},
		}}}},
	}}
	binding := domain.AgentTargetBinding{Secrets: map[string]domain.SecretProvider{
		"local":   {Type: "envfile", Path: "/etc/fxvcs/dev.env", Aliases: map[string]string{"fivem/license-key": "LEGACY_LICENSE"}},
		"managed": {Type: "fxvcs-cloud"},
	}}
	got, err := EnvfileSkeletons(target, binding)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != "local" || got[0].Path != "/etc/fxvcs/dev.env" {
		t.Fatalf("unexpected skeletons: %#v", got)
	}
	want := "# Required by target dev; values intentionally omitted.\nFIVEM__RCON_PASSWORD=\nLEGACY_LICENSE=\n"
	if string(got[0].Content) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got[0].Content, want)
	}
	if len(got[0].Requirements) != 2 || len(got[0].Requirements[0].Locations) != 1 {
		t.Fatalf("missing requirement origins: %#v", got[0].Requirements)
	}
}

func TestEnvfileSkeletonsRefuseMissingProviderAndAliasCollision(t *testing.T) {
	secretA := domain.SecretRef{Namespace: "one", Key: "token"}
	secretB := domain.SecretRef{Namespace: "two", Key: "token"}
	target := domain.Target{Spec: domain.TargetSpec{Secrets: domain.SecretPolicy{DefaultProvider: "local"}, Hooks: &domain.HookRequests{PostMaterialize: []domain.HookRequest{{Inputs: map[string]domain.InputSource{
		"a": {Secret: &secretA}, "b": {Secret: &secretB},
	}}}}}}
	if _, err := EnvfileSkeletons(target, domain.AgentTargetBinding{}); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("missing provider: %v", err)
	}
	binding := domain.AgentTargetBinding{Secrets: map[string]domain.SecretProvider{"local": {Type: "envfile", Path: "/tmp/x", Aliases: map[string]string{"one/token": "TOKEN", "two/token": "TOKEN"}}}}
	if _, err := EnvfileSkeletons(target, binding); !errors.Is(err, ErrUnsafeProvider) {
		t.Fatalf("alias collision: %v", err)
	}
}

func TestEnvfileSkeletonsIncludeRuntimeCredentials(t *testing.T) {
	target := domain.Target{Metadata: domain.NamedMetadata{Name: "dev"}, Spec: domain.TargetSpec{Secrets: domain.SecretPolicy{DefaultProvider: "file"}}}
	binding := domain.AgentTargetBinding{
		Runtime: domain.RuntimeBinding{
			RCON:          &domain.RCONEndpointRef{PasswordSecret: domain.SecretRef{Namespace: "fivem", Key: "rcon-password"}},
			ResourceState: &domain.ResourceStateEndpointRef{TokenSecret: domain.SecretRef{Namespace: "fivem", Key: "health-token"}},
		},
		Secrets: map[string]domain.SecretProvider{"file": {Type: "envfile", Path: "/etc/fxvcs/dev.env"}},
	}
	skeletons, err := EnvfileSkeletons(target, binding)
	if err != nil || len(skeletons) != 1 {
		t.Fatalf("skeletons=%#v err=%v", skeletons, err)
	}
	want := "# Required by target dev; values intentionally omitted.\nFIVEM__HEALTH_TOKEN=\nFIVEM__RCON_PASSWORD=\n"
	if string(skeletons[0].Content) != want {
		t.Fatalf("content=%q want=%q", skeletons[0].Content, want)
	}
}
