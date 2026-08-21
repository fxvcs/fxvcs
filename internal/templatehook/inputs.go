package templatehook

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/secrets"
)

var ErrInputSource = errors.New("templatehook: unsupported input source")

type SecretResolver interface {
	Resolve(context.Context, string, domain.SecretRef) (secrets.Value, error)
}

type InputContext struct {
	Target      domain.Target
	Environment domain.Environment
	Release     domain.Release
	Secrets     SecretResolver
}

type ResolvedInputs struct {
	Inputs         map[string]Input
	SecretVersions map[string]string
	ContainsSecret bool
}

var targetInputKeys = map[string]string{
	"server.hostname":           "hostname",
	"server.projectName":        "projectName",
	"server.projectDescription": "projectDescription",
	"server.tags":               "tags",
	"server.locale":             "locale",
}

// ResolveInputs maps the schema-checked source union through an explicit
// exposable-field allowlist. It resolves each logical secret once and returns
// only opaque versions alongside sensitivity-preserving template inputs.
func ResolveInputs(ctx context.Context, sources map[string]domain.InputSource, inputContext InputContext) (ResolvedInputs, error) {
	out := ResolvedInputs{Inputs: map[string]Input{}, SecretVersions: map[string]string{}}
	secretCache := map[string]secrets.Value{}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		source := sources[name]
		switch {
		case source.Literal != nil:
			out.Inputs[name] = PublicInput(*source.Literal)
		case source.Release != nil:
			if *source.Release != "id" {
				return ResolvedInputs{}, fmt.Errorf("%w: release path %q is not exposable", ErrInputSource, *source.Release)
			}
			out.Inputs[name] = PublicInput(inputContext.Release.Metadata.Name)
		case source.Target != nil:
			key, allowed := targetInputKeys[*source.Target]
			if !allowed {
				return ResolvedInputs{}, fmt.Errorf("%w: target path %q is not exposable", ErrInputSource, *source.Target)
			}
			value, exists := inputContext.Environment.Spec.Inputs[key]
			if !exists {
				return ResolvedInputs{}, fmt.Errorf("%w: target path %q has no environment input %q", ErrInputSource, *source.Target, key)
			}
			out.Inputs[name] = PublicInput(value)
		case source.Secret != nil:
			if inputContext.Secrets == nil {
				return ResolvedInputs{}, fmt.Errorf("%w: secret resolver unavailable", ErrInputSource)
			}
			provider := inputContext.Target.Spec.Secrets.DefaultProvider
			if selected, ok := inputContext.Target.Spec.Secrets.Namespaces[source.Secret.Namespace]; ok {
				provider = selected.Provider
			}
			if provider == "" {
				return ResolvedInputs{}, fmt.Errorf("%w: no provider selected for %s/%s", ErrInputSource, source.Secret.Namespace, source.Secret.Key)
			}
			refKey := source.Secret.Namespace + "/" + source.Secret.Key
			cacheKey := provider + "\x00" + refKey
			value, exists := secretCache[cacheKey]
			if !exists {
				var err error
				value, err = inputContext.Secrets.Resolve(ctx, provider, *source.Secret)
				if err != nil {
					return ResolvedInputs{}, fmt.Errorf("templatehook: resolve %s through %s: %w", refKey, provider, err)
				}
				if !utf8.Valid(value.Plaintext) {
					return ResolvedInputs{}, fmt.Errorf("%w: secret %s is not UTF-8 text", ErrInputSource, refKey)
				}
				secretCache[cacheKey] = value
			}
			if previous, exists := out.SecretVersions[refKey]; exists && previous != value.Version {
				return ResolvedInputs{}, fmt.Errorf("%w: secret %s resolved to conflicting versions", ErrInputSource, refKey)
			}
			out.SecretVersions[refKey] = value.Version
			out.Inputs[name] = SecretInput(string(value.Plaintext))
			out.ContainsSecret = true
		default:
			return ResolvedInputs{}, fmt.Errorf("%w: input %q has no discriminator", ErrInputSource, name)
		}
	}
	return out, nil
}
