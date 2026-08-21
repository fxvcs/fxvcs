package schema

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func validAgentConfig() *domain.AgentConfig {
	return &domain.AgentConfig{Spec: domain.AgentConfigSpec{
		AgentID: "agent-host-a",
		Targets: []domain.AgentTargetBinding{{
			Target:     "dev",
			DeployRoot: "/srv/fxvcs/dev",
			ServerData: "/srv/fxserver/dev",
			Runtime:    domain.RuntimeBinding{Supervisor: "manual"},
		}},
		SafetyFloors: domain.AgentSafetyFloors{MinimumRetention: 1},
	}}
}

func requireAgentIssue(t *testing.T, config *domain.AgentConfig, path, message string) {
	t.Helper()
	for _, issue := range checkAgentConfig(config) {
		if issue.Path == path && strings.Contains(issue.Message, message) {
			return
		}
	}
	t.Fatalf("missing issue path=%q message containing %q; got %#v", path, message, checkAgentConfig(config))
}

func TestAgentConfigSemanticValidation(t *testing.T) {
	t.Run("valid manual binding", func(t *testing.T) {
		if issues := checkAgentConfig(validAgentConfig()); len(issues) != 0 {
			t.Fatalf("unexpected issues: %v", issues)
		}
	})

	t.Run("duplicate target", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets = append(config.Spec.Targets, domain.AgentTargetBinding{
			Target: "dev", DeployRoot: "/srv/fxvcs/dev-two", ServerData: "/srv/fxserver/dev-two",
			Runtime: domain.RuntimeBinding{Supervisor: "manual"},
		})
		requireAgentIssue(t, config, "/spec/targets/1/target", "duplicate target")
	})

	t.Run("nested deploy roots", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets = append(config.Spec.Targets, domain.AgentTargetBinding{
			Target: "stg", DeployRoot: "/srv/fxvcs/dev/nested", ServerData: "/srv/fxserver/stg",
			Runtime: domain.RuntimeBinding{Supervisor: "manual"},
		})
		requireAgentIssue(t, config, "/spec/targets", "roots overlap")
	})

	t.Run("txadmin requires txData", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Runtime.Supervisor = "txadmin"
		requireAgentIssue(t, config, "/spec/targets/0/runtime/txData", "requires txData")
	})

	t.Run("systemd requires unit", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Runtime.Supervisor = "systemd"
		requireAgentIssue(t, config, "/spec/targets/0/runtime/systemdUnit", "requires systemdUnit")
	})

	t.Run("resource state requires numeric loopback", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Runtime.ResourceState = &domain.ResourceStateEndpointRef{Host: "localhost", Port: 30120, TokenSecret: domain.SecretRef{Namespace: "fivem", Key: "health-token"}}
		requireAgentIssue(t, config, "/spec/targets/0/runtime/resourceState/host", "numeric loopback")
	})

	t.Run("envfile requires absolute path", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Secrets = map[string]domain.SecretProvider{
			"local-file": {Type: "envfile", Path: "relative.env"},
		}
		requireAgentIssue(t, config, "/spec/targets/0/secrets/local-file/path", "absolute and clean")
	})

	t.Run("cloud provider remains gated", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Secrets = map[string]domain.SecretProvider{
			"cloud": {Type: "fxvcs-cloud"},
		}
		requireAgentIssue(t, config, "/spec/targets/0/secrets/cloud/type", "not implemented in Stage 4")
	})

	t.Run("hook output cannot enter resources", func(t *testing.T) {
		config := validAgentConfig()
		config.Spec.Targets[0].Hooks = map[string]domain.HookDefinition{
			"render": {Type: "template.v1", AllowedDestRoot: "resources/generated", TimeoutSeconds: 1, MaxOutputBytes: 1},
		}
		requireAgentIssue(t, config, "/spec/targets/0/hooks/render/allowedDestinationRoot", "outside resources")
	})
}
