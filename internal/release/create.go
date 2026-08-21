package release

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/profile/fivem/catalog"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/stack"
)

// ErrNotReleasable aggregates everything that stopped a release.
var ErrNotReleasable = errors.New("release: composition is not releasable")

// NotReleasableError carries every problem found in one pass.
type NotReleasableError struct{ Problems []stack.Problem }

func (e *NotReleasableError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return "release: not releasable: " + strings.Join(parts, "; ")
}

func (e *NotReleasableError) Is(target error) bool { return target == ErrNotReleasable }

// ObjectChecker verifies that the objects a pinned component references are
// already durable in storage. Release creation checks this independently of
// the pre-push barrier, because a user can bypass Git client hooks and a
// release whose assets exist only on a laptop is not deployable.
type ObjectChecker interface {
	CheckComponent(ctx context.Context, cfg *domain.Repository, pointers map[string]pointer.Pointer) (ObjectReport, error)
}

// ObjectReport is one component's object-availability result.
type ObjectReport struct {
	// Remotes are the remote names required for publication.
	Remotes []string `json:"remotes"`
	// Assets is how many placeholder paths were checked.
	Assets int `json:"assets"`
	// Missing describes assets absent from a required remote.
	Missing []string `json:"missing,omitempty"`
	// Unverifiable names required remotes this machine cannot reach, with the
	// reason. A release is not written on an unverifiable remote unless the
	// caller explicitly allows it.
	Unverifiable []string `json:"unverifiable,omitempty"`
}

// Pin selects the commit for one component.
type Pin struct {
	RepositoryID string
	// Ref is a branch, tag, or full commit id. Empty uses the stack ref.
	Ref string
}

// CreateInput describes the release to build.
type CreateInput struct {
	Name string
	// Pins override the stack ref per component.
	Pins []Pin
	// Enable and Disable are release-only activation overrides, selected by
	// resource name, "<repositoryID>:<path>", or a runtime/external ID.
	Enable, Disable []string
	// VariantOf groups this release with another for humans; it never affects
	// identity or source equality.
	VariantOf string
	// CreatedBy is recorded in the release metadata.
	CreatedBy string
	// Now is the creation timestamp.
	Now time.Time
	// MinimumFXVCSVersion is recorded on the release; empty computes the
	// maximum requirement across the stack and pinned components.
	MinimumFXVCSVersion string
	// Objects verifies storage availability; nil skips the check.
	Objects ObjectChecker
	// AllowUnverifiedObjects keeps going when a required remote cannot be
	// reached from this machine.
	AllowUnverifiedObjects bool
}

// ComponentReport is what one component contributed.
type ComponentReport struct {
	RepositoryID   string       `json:"repositoryID"`
	RepositoryName string       `json:"repositoryName"`
	Remote         string       `json:"remote"`
	Ref            string       `json:"ref,omitempty"`
	Commit         string       `json:"commit"`
	AssetRoot      string       `json:"assetRoot"`
	Assets         int          `json:"assets"`
	Mount          string       `json:"mount"`
	DescriptorFrom string       `json:"descriptorFrom"`
	Resources      int          `json:"resources"`
	Objects        ObjectReport `json:"objects,omitzero"`
	// DurablePin reports whether the component's remote still advertises a ref
	// at this commit.
	DurablePin *bool `json:"durablePin,omitempty"`
}

// CreateResult is the built release plus everything worth reviewing.
type CreateResult struct {
	Release    domain.Release    `json:"release"`
	Components []ComponentReport `json:"components"`
	Plan       stack.Plan        `json:"-"`
	Warnings   []string          `json:"warnings,omitempty"`
	Problems   []stack.Problem   `json:"problems,omitempty"`
}

// Create resolves the composition and builds an immutable release record. It
// does not write, commit, or tag anything.
func Create(ctx context.Context, repo *gitops.Repo, in CreateInput) (*CreateResult, error) {
	if err := gitops.ValidateName("release", in.Name); err != nil {
		return nil, err
	}
	ws := repo.Stack
	if len(ws.Spec.Components) == 0 {
		return nil, fmt.Errorf("%w: this stack has no components (add one with: fxvcs stack add <git-url>)", controlplane.ErrInvalid)
	}
	pinned := map[string]string{}
	for _, p := range in.Pins {
		if _, ok := repo.Component(p.RepositoryID); !ok {
			return nil, fmt.Errorf("%w: --pin names %s, which is not a component of this stack", controlplane.ErrInvalid, p.RepositoryID)
		}
		pinned[p.RepositoryID] = p.Ref
	}

	res := &CreateResult{}
	var comps []stack.Component
	minimum := format.Version{}
	if ws.Spec.MinimumFXVCSVersion != "" {
		v, err := format.ParseVersion(ws.Spec.MinimumFXVCSVersion)
		if err != nil {
			return nil, err
		}
		minimum = v
	}

	// --- pin, read, and describe every component --------------------------
	for _, spec := range ws.Spec.Components {
		ref := spec.Ref
		if r, ok := pinned[spec.RepositoryID]; ok && r != "" {
			ref = r
		}
		commit, err := repo.ResolveRef(ctx, spec.RepositoryID, ref)
		if err != nil {
			if !spec.Required && errors.Is(err, controlplane.ErrNotFound) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("optional component %s is not available and was left out of this release: %v", spec.RepositoryID, err))
				continue
			}
			return nil, err
		}
		tree, err := repo.Tree(ctx, spec.RepositoryID, commit)
		if err != nil {
			return nil, err
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			return nil, err
		}
		if v := cfg.Spec.MinimumFXVCSVersion; v != "" {
			parsed, perr := format.ParseVersion(v)
			if perr == nil && parsed.Compare(minimum) > 0 {
				minimum = parsed
			}
		}
		c := stack.Component{Spec: spec, Commit: commit, Config: cfg}
		desc, from := c.Descriptor()

		root, idx, err := AssetRoot(ctx, tree, cfg.Metadata.StorageDomainID)
		if err != nil {
			return nil, err
		}
		c.AssetRoot = root

		report := ComponentReport{
			RepositoryID: spec.RepositoryID, RepositoryName: c.DisplayName(), Remote: spec.Remote,
			Ref: ref, Commit: commit, AssetRoot: root, Assets: len(idx.Entries),
			Mount: spec.Mount, DescriptorFrom: from,
		}

		if cfg.Spec.Profile == domain.ProfileFiveM {
			cat, err := tree.ResourceCatalog(ctx)
			if err != nil {
				// A missing list and an invalid one need different answers:
				// one is "run resources sync", the other is "your list says
				// something impossible", and telling an operator the first
				// when it is the second sends them to the wrong file.
				if !errors.Is(err, controlplane.ErrNotFound) {
					res.Problems = append(res.Problems, stack.Problem{
						Component: spec.RepositoryID, Field: "resources", Message: err.Error()})
					comps = append(comps, c)
					res.Components = append(res.Components, report)
					continue
				}
				return nil, fmt.Errorf("%w: component %s has no resource list at %s; run `fxvcs resources sync` there and commit .fxvcs/resources.yaml",
					controlplane.ErrNotFound, spec.RepositoryID, shortCommit(commit))
			}
			if cat.Metadata.RepositoryID != cfg.Metadata.RepositoryID {
				res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "resources",
					Message: fmt.Sprintf(".fxvcs/resources.yaml names repository %s but .fxvcs/repository.yaml names %s", cat.Metadata.RepositoryID, cfg.Metadata.RepositoryID)})
			}
			c.Catalog = cat
			report.Resources = len(cat.Spec.Resources)

			// Re-scan the pinned tree. The committed catalog is a claim; the
			// tree is the fact, and a release must not ship a claim.
			fsys, err := tree.FS(ctx)
			if err != nil {
				return nil, err
			}
			found, err := discovery.Scan(ctx, fsys, desc)
			if err != nil {
				res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "scan", Message: err.Error()})
			} else if diff := catalog.Diff(cat, found); !diff.Clean() {
				for _, p := range diff.Undeclared {
					res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "resources",
						Message: fmt.Sprintf("%s exists in the pinned tree but not in .fxvcs/resources.yaml; run `fxvcs resources sync` in the component and commit it", p)})
				}
				for _, p := range diff.Missing {
					res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "resources",
						Message: fmt.Sprintf("%s is listed in .fxvcs/resources.yaml but has no directory in the pinned tree", p)})
				}
				for _, s := range diff.Stale {
					res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "resources",
						Message: fmt.Sprintf("%s %s is stale: the list says %q, the manifest says %q", s.Path, s.Field, s.Catalog, s.Manifest)})
				}
			}
		}

		if in.Objects != nil {
			ptrs, err := Pointers(ctx, tree, cfg.Metadata.StorageDomainID)
			if err != nil {
				return nil, err
			}
			rep, err := in.Objects.CheckComponent(ctx, cfg, ptrs)
			if err != nil {
				return nil, err
			}
			report.Objects = rep
			for _, m := range rep.Missing {
				res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "storage", Message: m})
			}
			for _, u := range rep.Unverifiable {
				if in.AllowUnverifiedObjects {
					res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s", spec.RepositoryID, u))
				} else {
					res.Problems = append(res.Problems, stack.Problem{Component: spec.RepositoryID, Field: "storage",
						Message: u + " (re-run with --allow-unverified-objects to create the release anyway)"})
				}
			}
		}

		if durable, err := repo.RemoteHasRef(ctx, spec, commit); err == nil {
			report.DurablePin = &durable
			if !durable {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s is pinned at %s, which no branch or tag on %s currently points to; it can be garbage-collected. Consider --tag-components.",
					spec.RepositoryID, shortCommit(commit), spec.Remote))
			}
		}

		comps = append(comps, c)
		res.Components = append(res.Components, report)
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("%w: no component of this stack could be pinned", controlplane.ErrInvalid)
	}

	// --- composition ------------------------------------------------------
	res.Problems = append(res.Problems, stack.ValidateComposition(comps)...)

	// --- per-component catalog release validation -------------------------
	var known []string
	for _, c := range comps {
		if c.Catalog == nil {
			continue
		}
		for _, r := range c.Catalog.Spec.Resources {
			known = append(known, r.Name)
			known = append(known, r.Provides...)
		}
	}
	runtimeCat, err := repo.RuntimeResources()
	if err != nil {
		return nil, err
	}
	for _, rr := range runtimeCat.Spec.Resources {
		known = append(known, rr.Name)
	}
	for _, c := range comps {
		if c.Catalog == nil {
			continue
		}
		if err := catalog.ValidateForRelease(c.Catalog, catalog.ValidateOptions{KnownNames: known, AllowPending: true}); err != nil {
			var ve *catalog.ValidationError
			if errors.As(err, &ve) {
				for _, p := range ve.Problems {
					res.Problems = append(res.Problems, stack.Problem{Component: c.Spec.RepositoryID,
						Field: p.Field, Message: strings.TrimSpace(p.Path + " " + p.Message)})
				}
				continue
			}
			return nil, err
		}
	}

	// --- activation plan --------------------------------------------------
	overrides, err := repo.Overrides()
	if err != nil {
		return nil, err
	}
	var relOverrides []stack.Override
	for _, s := range in.Enable {
		relOverrides = append(relOverrides, stack.Override{Selector: s, Activation: domain.ActivationEnabled})
	}
	for _, s := range in.Disable {
		relOverrides = append(relOverrides, stack.Override{Selector: s, Activation: domain.ActivationDisabled})
	}
	plan, planProblems := stack.ResolvePlan(stack.PlanInput{
		Components: comps, Runtime: runtimeCat, Overrides: overrides, ReleaseOverrides: relOverrides,
	})
	res.Problems = append(res.Problems, planProblems...)
	if len(res.Problems) > 0 {
		return res, &NotReleasableError{Problems: res.Problems}
	}
	res.Plan = plan

	// --- assemble ---------------------------------------------------------
	minVersion := in.MinimumFXVCSVersion
	if minVersion == "" && minimum != (format.Version{}) {
		minVersion = minimum.String()
	}
	spec := domain.ReleaseSpec{
		StackID:             ws.Metadata.StackID,
		MinimumFXVCSVersion: minVersion,
		VariantOf:           in.VariantOf,
		Components:          make([]domain.ReleaseComponent, 0, len(comps)),
		RuntimeResources:    plan.RuntimeResources,
		Resources:           plan.Resources,
	}
	sorted := append([]stack.Component(nil), comps...)
	stack.SortComponents(sorted)
	for _, c := range sorted {
		desc, from := c.Descriptor()
		spec.Components = append(spec.Components, domain.ReleaseComponent{
			RepositoryID:    c.Spec.RepositoryID,
			RepositoryName:  c.DisplayName(),
			Remote:          c.Spec.Remote,
			Commit:          c.Commit,
			AssetRoot:       c.AssetRoot,
			StorageDomainID: c.Config.Metadata.StorageDomainID,
			Descriptor:      desc,
			DescriptorFrom:  from,
			Mount:           c.Spec.Mount,
		})
	}
	digest, err := integrity.SourceSetDigest(spec)
	if err != nil {
		return nil, err
	}
	spec.SourceSetDigest = digest

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	res.Release = domain.Release{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRelease},
		Metadata: domain.ReleaseMetadata{
			Name:      in.Name,
			CreatedAt: now.UTC().Format(time.RFC3339),
			CreatedBy: in.CreatedBy,
		},
		Spec: spec,
	}
	sort.Slice(res.Components, func(i, j int) bool { return res.Components[i].RepositoryID < res.Components[j].RepositoryID })
	return res, nil
}

// ComponentTag is the durable ref `--tag-components` plans for one component.
type ComponentTag struct {
	RepositoryID string `json:"repositoryID"`
	Remote       string `json:"remote"`
	Tag          string `json:"tag"`
	Commit       string `json:"commit"`
}

// TagName is the namespaced component tag for a release.
func TagName(release, repositoryID string) string {
	return "fxvcs/components/" + release + "/" + repositoryID
}

// PlanComponentTags lists the tags `--tag-components` would create.
func PlanComponentTags(rel domain.Release) []ComponentTag {
	out := make([]ComponentTag, 0, len(rel.Spec.Components))
	for _, c := range rel.Spec.Components {
		out = append(out, ComponentTag{
			RepositoryID: c.RepositoryID, Remote: c.Remote,
			Tag: TagName(rel.Metadata.Name, c.RepositoryID), Commit: c.Commit,
		})
	}
	return out
}

// TagComponents creates and pushes every planned tag. A failure anywhere
// aborts with an error and the caller must not write the release: a partially
// tagged release would advertise durable pins it does not have.
func TagComponents(ctx context.Context, repo *gitops.Repo, rel domain.Release) ([]ComponentTag, error) {
	tags := PlanComponentTags(rel)
	msg := "fxvcs release " + rel.Metadata.Name
	for i, t := range tags {
		comp, ok := repo.Component(t.RepositoryID)
		if !ok {
			return tags[:i], fmt.Errorf("%w: %s is no longer a component of this stack", controlplane.ErrNotFound, t.RepositoryID)
		}
		if err := repo.TagComponent(ctx, comp, t.Tag, t.Commit, msg); err != nil {
			return tags[:i], fmt.Errorf("tagging %s failed, so the release was not written: %w", t.RepositoryID, err)
		}
	}
	return tags, nil
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
