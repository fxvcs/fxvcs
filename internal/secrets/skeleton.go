package secrets

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"fxvcs.dev/fxvcs/internal/domain"
)

type EnvfileRequirement struct {
	Reference string   `json:"reference"`
	Variable  string   `json:"variable"`
	Locations []string `json:"locations"`
}

type EnvfileSkeleton struct {
	Provider     string               `json:"provider"`
	Path         string               `json:"path"`
	Content      []byte               `json:"content"`
	Requirements []EnvfileRequirement `json:"requirements"`
}

// EnvfileVariable is the standard, reversible local envfile encoding for a
// provider-neutral namespace/key reference. Exact host aliases may override
// it for legacy names.
func EnvfileVariable(ref domain.SecretRef) string {
	return variableSegment(ref.Namespace) + "__" + variableSegment(ref.Key)
}

func variableSegment(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' {
			return '_'
		}
		return unicode.ToUpper(r)
	}, value)
}

// EnvfileSkeletons derives value-free dotenv files from the target's typed
// secret inputs. It never reads provider files or secret values. One skeleton
// is returned for each selected envfile provider, sorted by provider alias.
func EnvfileSkeletons(target domain.Target, binding domain.AgentTargetBinding) ([]EnvfileSkeleton, error) {
	type collected struct {
		ref       domain.SecretRef
		locations []string
	}
	byProvider := map[string]map[string]*collected{}
	if target.Spec.Hooks != nil {
		phases := []struct {
			name     string
			requests []domain.HookRequest
		}{
			{"postMaterialize", target.Spec.Hooks.PostMaterialize},
			{"preActivate", target.Spec.Hooks.PreActivate},
			{"postActivate", target.Spec.Hooks.PostActivate},
			{"onFailure", target.Spec.Hooks.OnFailure},
		}
		for _, phase := range phases {
			for i, request := range phase.requests {
				names := make([]string, 0, len(request.Inputs))
				for name := range request.Inputs {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					input := request.Inputs[name]
					if input.Secret == nil {
						continue
					}
					providerAlias := target.Spec.Secrets.DefaultProvider
					if selected, ok := target.Spec.Secrets.Namespaces[input.Secret.Namespace]; ok {
						providerAlias = selected.Provider
					}
					if providerAlias == "" {
						return nil, fmt.Errorf("%w: no provider selected for %s", ErrUnknownProvider, reference(*input.Secret))
					}
					provider, ok := binding.Secrets[providerAlias]
					if !ok {
						return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, providerAlias)
					}
					if provider.Type != "envfile" {
						continue
					}
					if byProvider[providerAlias] == nil {
						byProvider[providerAlias] = map[string]*collected{}
					}
					refKey := reference(*input.Secret)
					item := byProvider[providerAlias][refKey]
					if item == nil {
						item = &collected{ref: *input.Secret}
						byProvider[providerAlias][refKey] = item
					}
					item.locations = append(item.locations, fmt.Sprintf("/spec/hooks/%s/%d/inputs/%s", phase.name, i, name))
				}
			}
		}
	}
	collectRuntime := func(ref domain.SecretRef, location string) error {
		providerAlias := target.Spec.Secrets.DefaultProvider
		if selected, ok := target.Spec.Secrets.Namespaces[ref.Namespace]; ok {
			providerAlias = selected.Provider
		}
		if providerAlias == "" {
			return fmt.Errorf("%w: no provider selected for %s", ErrUnknownProvider, reference(ref))
		}
		provider, ok := binding.Secrets[providerAlias]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownProvider, providerAlias)
		}
		if provider.Type != "envfile" {
			return nil
		}
		if byProvider[providerAlias] == nil {
			byProvider[providerAlias] = map[string]*collected{}
		}
		refKey := reference(ref)
		item := byProvider[providerAlias][refKey]
		if item == nil {
			item = &collected{ref: ref}
			byProvider[providerAlias][refKey] = item
		}
		item.locations = append(item.locations, location)
		return nil
	}
	if binding.Runtime.RCON != nil {
		if err := collectRuntime(binding.Runtime.RCON.PasswordSecret, "/agentConfig/runtime/rcon/passwordSecret"); err != nil {
			return nil, err
		}
	}
	if binding.Runtime.ResourceState != nil {
		if err := collectRuntime(binding.Runtime.ResourceState.TokenSecret, "/agentConfig/runtime/resourceState/tokenSecret"); err != nil {
			return nil, err
		}
	}

	aliases := make([]string, 0, len(byProvider))
	for alias := range byProvider {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	out := make([]EnvfileSkeleton, 0, len(aliases))
	for _, alias := range aliases {
		provider := binding.Secrets[alias]
		refs := make([]string, 0, len(byProvider[alias]))
		for ref := range byProvider[alias] {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		variables := map[string]string{}
		requirements := make([]EnvfileRequirement, 0, len(refs))
		for _, refKey := range refs {
			item := byProvider[alias][refKey]
			variable := provider.Aliases[refKey]
			if variable == "" {
				variable = EnvfileVariable(item.ref)
			}
			if previous, exists := variables[variable]; exists && previous != refKey {
				return nil, fmt.Errorf("%w: provider %s maps both %s and %s to %s", ErrUnsafeProvider, alias, previous, refKey, variable)
			}
			variables[variable] = refKey
			sort.Strings(item.locations)
			requirements = append(requirements, EnvfileRequirement{Reference: refKey, Variable: variable, Locations: item.locations})
		}
		sort.Slice(requirements, func(i, j int) bool { return requirements[i].Variable < requirements[j].Variable })
		var content bytes.Buffer
		fmt.Fprintf(&content, "# Required by target %s; values intentionally omitted.\n", target.Metadata.Name)
		for _, requirement := range requirements {
			content.WriteString(requirement.Variable)
			content.WriteString("=\n")
		}
		out = append(out, EnvfileSkeleton{Provider: alias, Path: provider.Path, Content: content.Bytes(), Requirements: requirements})
	}
	return out, nil
}
