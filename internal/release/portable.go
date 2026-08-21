package release

import (
	"context"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/schema"
)

// A Release record is already portable: it names components by stable ID and
// remote, pins commits and asset roots, and carries its own resolved plan. So
// export is a validated copy out and import is a validated copy back, both
// guarded by the release digest. Nothing about a target, an environment, a
// credential, or a host path travels with it.

// Export returns one release as a portable YAML document and its digest. The
// caller decides where the bytes go; this package never touches stdout.
func Export(ctx context.Context, repo *gitops.Repo, name string) (domain.Release, []byte, string, error) {
	rel, err := repo.GetRelease(ctx, name)
	if err != nil {
		return domain.Release{}, nil, "", err
	}
	digest, err := integrity.ReleaseDigest(rel)
	if err != nil {
		return domain.Release{}, nil, "", err
	}
	data, err := yaml.Marshal(rel)
	if err != nil {
		return domain.Release{}, nil, "", err
	}
	return rel, data, digest, nil
}

// ImportResult reports what an import did.
type ImportResult struct {
	Release domain.Release `json:"release"`
	Digest  string         `json:"digest"`
	Path    string         `json:"path"`
	// Written is false when an identical record was already present.
	Written bool `json:"written"`
	// Warnings name components the importing stack does not register.
	Warnings []string `json:"warnings,omitempty"`
}

// Import validates a portable release document and writes it into the
// coordination repository. It refuses to replace an existing release whose
// content differs, and it never rewrites the imported record: a release keeps
// the identity it was created with, in every repository that holds it.
func Import(ctx context.Context, repo *gitops.Repo, data []byte) (*ImportResult, error) {
	rel, err := schema.LoadRelease(data)
	if err != nil {
		return nil, err
	}
	digest, err := integrity.ReleaseDigest(*rel)
	if err != nil {
		return nil, err
	}
	res := &ImportResult{Release: *rel, Digest: digest}
	if got := repo.Stack.Metadata.StackID; got != rel.Spec.StackID {
		res.Warnings = append(res.Warnings, fmt.Sprintf("this release was created in stack %s; you are importing it into %s", rel.Spec.StackID, got))
	}
	for _, c := range rel.Spec.Components {
		if _, ok := repo.Component(c.RepositoryID); !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("component %s (%s) is not registered in this stack, so `fxvcs stack sync` cannot fetch it", c.RepositoryID, c.RepositoryName))
		}
	}
	path, written, err := repo.WriteRelease(ctx, *rel)
	if err != nil {
		if errors.Is(err, controlplane.ErrImmutable) {
			return res, err
		}
		return nil, err
	}
	res.Path, res.Written = path, written
	return res, nil
}
