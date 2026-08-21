package fivem

import (
	"bytes"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestResourceStateFilesContainNoTokenAndExactExpectedResources(t *testing.T) {
	resources := []domain.ReleaseResource{
		{Name: "chat", Activation: domain.ActivationEnabled, EnsureOrder: 1},
		{Name: "disabled", Activation: domain.ActivationDisabled, EnsureOrder: 2},
		{Name: "map", Activation: domain.ActivationEnabled, EnsureOrder: 0},
	}
	files, err := ResourceStateFiles(resources)
	if err != nil || len(files) != 2 {
		t.Fatalf("files=%#v err=%v", files, err)
	}
	server := string(files[1].Data)
	if !strings.Contains(server, `["map","chat"]`) {
		t.Fatalf("expected resource order missing: %s", server)
	}
	if strings.Contains(server, "secret-token") {
		t.Fatal("helper source contains credential")
	}
	config, err := ResourceStateConfig([]byte("secret-token-1234"))
	if err != nil || !bytes.Equal(config, []byte("set fxvcs_health_token \"secret-token-1234\"\n")) {
		t.Fatalf("config=%q err=%v", config, err)
	}
}

func TestRuntimeConfigOrdersHealthLastAndRejectsCollision(t *testing.T) {
	resources := []domain.ReleaseResource{{Name: "chat", Source: domain.ResourceSourceComponent, Activation: domain.ActivationEnabled, EnsureOrder: 0}}
	config, err := RuntimeConfigWithHealth(resources, []string{HealthResourceConfigPath}, true)
	if err != nil || !strings.HasSuffix(string(config), "ensure chat\nensure fxvcs-health\n") {
		t.Fatalf("config=%q err=%v", config, err)
	}
	resources[0].Name = HealthResourceName
	if _, err := RuntimeConfigWithHealth(resources, nil, true); err == nil {
		t.Fatal("release collision with built-in health resource accepted")
	}
}
