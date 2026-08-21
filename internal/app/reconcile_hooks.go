package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/templatehook"
)

// ResolveAssemblyHooks validates pinned template identity and host allowlists,
// resolves typed inputs, and renders every pre-activation template output in
// deterministic phase/request order. It performs no deployment-root writes.
func (c *Coord) ResolveAssemblyHooks(ctx context.Context, target domain.Target, environment domain.Environment, rel domain.Release, binding domain.AgentTargetBinding, secretResolver templatehook.SecretResolver) (AssemblyOptions, error) {
	if target.Spec.Hooks == nil {
		return AssemblyOptions{HookDigests: map[string]string{}, SecretVersions: map[string]string{}}, nil
	}
	if len(target.Spec.Hooks.PostActivate) > 0 || len(target.Spec.Hooks.OnFailure) > 0 {
		return AssemblyOptions{}, errors.New("reconcile: template.v1 postActivate/onFailure outputs cannot mutate an immutable selected realization")
	}
	phases := [][]domain.HookRequest{target.Spec.Hooks.PostMaterialize, target.Spec.Hooks.PreActivate}
	options := AssemblyOptions{HookDigests: map[string]string{}, SecretVersions: map[string]string{}}
	destinations := map[string]string{}
	for _, requests := range phases {
		for _, request := range requests {
			definition, ok := binding.Hooks[request.ID]
			if !ok || request.Type != "template.v1" || definition.Type != request.Type || request.Template == nil {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s has no compatible template.v1 host binding", request.ID)
			}
			hookDigest, err := canonical.Digest(struct {
				Request    domain.HookRequest
				Definition domain.HookDefinition
			}{request, definition})
			if err != nil {
				return AssemblyOptions{}, err
			}
			options.HookDigests[request.ID] = hookDigest
			validationRoot := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "realization")
			if _, err := layout.HookDestination(validationRoot, definition.AllowedDestRoot, request.Destination); err != nil {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s destination: %w", request.ID, err)
			}
			if definition.AllowedDestRoot != layout.RuntimeDir+"/"+layout.RuntimeConfig {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s binding root %s is outside the v1 runtime config boundary", request.ID, definition.AllowedDestRoot)
			}
			destinationKey := strings.ToLower(request.Destination)
			if previous, exists := destinations[destinationKey]; exists {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hooks %s and %s share destination %s", previous, request.ID, request.Destination)
			}
			destinations[destinationKey] = request.ID

			templateKey := request.Template.Repository + ":" + request.Template.Path
			if !containsString(definition.AllowedTemplates, templateKey) {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s template %s is not host-allowlisted", request.ID, templateKey)
			}
			component, err := templateComponent(rel, request.Template.Repository)
			if err != nil {
				return AssemblyOptions{}, err
			}
			tree, err := c.Tree(ctx, component.RepositoryID, component.Commit)
			if err != nil {
				return AssemblyOptions{}, err
			}
			entries, err := tree.Entries(ctx)
			if err != nil {
				return AssemblyOptions{}, err
			}
			var templateSize int64 = -1
			for _, entry := range entries {
				if entry.Path == request.Template.Path {
					if !entry.IsRegular() {
						return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s template is not an ordinary Git blob", request.ID)
					}
					templateSize = entry.Size
					break
				}
			}
			if templateSize < 0 {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s template %s is absent from pinned component", request.ID, templateKey)
			}
			if templateSize > templatehook.DefaultMaxTemplateBytes {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s: %w", request.ID, templatehook.ErrTemplateTooLarge)
			}
			source, err := tree.ReadFile(ctx, request.Template.Path)
			if err != nil {
				return AssemblyOptions{}, err
			}
			if int64(len(source)) != templateSize {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s template size changed while reading", request.ID)
			}
			digest := sha256.Sum256(source)
			gotDigest := "sha256:" + hex.EncodeToString(digest[:])
			if gotDigest != request.Template.Digest {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s template hashes to %s, target pins %s", request.ID, gotDigest, request.Template.Digest)
			}
			resolved, err := templatehook.ResolveInputs(ctx, request.Inputs, templatehook.InputContext{Target: target, Environment: environment, Release: rel, Secrets: secretResolver})
			if err != nil {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s inputs: %w", request.ID, err)
			}
			for ref, version := range resolved.SecretVersions {
				if previous, exists := options.SecretVersions[ref]; exists && previous != version {
					return AssemblyOptions{}, fmt.Errorf("reconcile: secret %s changed during preflight", ref)
				}
				options.SecretVersions[ref] = version
			}
			rendered, err := (templatehook.Renderer{MaxOutputBytes: definition.MaxOutputBytes}).Render(request.ID, source, resolved.Inputs)
			if err != nil {
				return AssemblyOptions{}, fmt.Errorf("reconcile: hook %s render: %w", request.ID, err)
			}
			mode := request.Mode
			if mode == "" {
				mode = "0644"
				if resolved.ContainsSecret {
					mode = "0600"
				}
			}
			options.Hooks = append(options.Hooks, AssemblyHookOutput{ID: request.ID, Destination: request.Destination, Mode: mode, Rendered: rendered, ContainsSecret: resolved.ContainsSecret})
		}
	}
	return options, nil
}

func templateComponent(rel domain.Release, name string) (domain.ReleaseComponent, error) {
	var matches []domain.ReleaseComponent
	for _, component := range rel.Spec.Components {
		if component.RepositoryName == name || component.RepositoryID == name {
			matches = append(matches, component)
		}
	}
	if len(matches) != 1 {
		return domain.ReleaseComponent{}, fmt.Errorf("reconcile: template repository %q resolves to %d release components", name, len(matches))
	}
	return matches[0], nil
}

func containsString(values []string, want string) bool {
	values = append([]string(nil), values...)
	sort.Strings(values)
	i := sort.SearchStrings(values, want)
	return i < len(values) && values[i] == want
}

func resolveResourceStateSecret(ctx context.Context, target domain.Target, binding domain.AgentTargetBinding, resolver templatehook.SecretResolver) ([]byte, string, error) {
	endpoint := binding.Runtime.ResourceState
	if endpoint == nil {
		return nil, "", errors.New("reconcile: fxvcs-resource-state requires an agent-local loopback endpoint binding")
	}
	if resolver == nil {
		return nil, "", errors.New("reconcile: fxvcs-resource-state requires a protected secret resolver")
	}
	provider, err := providerForSecret(target, endpoint.TokenSecret)
	if err != nil {
		return nil, "", err
	}
	value, err := resolver.Resolve(ctx, provider, endpoint.TokenSecret)
	if err != nil {
		return nil, "", fmt.Errorf("reconcile: resolve resource-state credential: %w", err)
	}
	return append([]byte(nil), value.Plaintext...), value.Version, nil
}

func providerForSecret(target domain.Target, ref domain.SecretRef) (string, error) {
	provider := target.Spec.Secrets.DefaultProvider
	if selected, ok := target.Spec.Secrets.Namespaces[ref.Namespace]; ok {
		provider = selected.Provider
	}
	if provider == "" {
		return "", fmt.Errorf("reconcile: no provider selected for %s/%s", ref.Namespace, ref.Key)
	}
	return provider, nil
}
