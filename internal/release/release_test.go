package release

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

func rel(name string, comps []domain.ReleaseComponent, res []domain.ReleaseResource, sourceSet string) domain.Release {
	return domain.Release{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRelease},
		Metadata: domain.ReleaseMetadata{Name: name, CreatedAt: "2026-03-01T12:00:00Z", CreatedBy: "test"},
		Spec: domain.ReleaseSpec{
			StackID: "stack_test", SourceSetDigest: sourceSet, Components: comps, Resources: res,
		},
	}
}

func comp(id, name, commit, assetRoot, mount string) domain.ReleaseComponent {
	return domain.ReleaseComponent{
		RepositoryID: id, RepositoryName: name, Remote: "https://example.invalid/" + name + ".git",
		Commit: commit, AssetRoot: assetRoot, StorageDomainID: "domain_" + id, Mount: mount,
		Descriptor:     domain.ComponentDescriptor{SourceRoot: ".", Layout: domain.LayoutCollection},
		DescriptorFrom: domain.DescriptorFromComponent,
	}
}

func planned(name string, activation domain.Activation, order int, source string) domain.ReleaseResource {
	return domain.ReleaseResource{
		Source: domain.ResourceSourceComponent, RepositoryID: "repo_a", RepositoryName: "a",
		Path: name, Name: name, Activation: activation, ActivationSource: source, EnsureOrder: order,
	}
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commit1 = "1111111111111111111111111111111111111111"
	commit2 = "2222222222222222222222222222222222222222"
)

func TestDiffReportsSourceEqualitySeparatelyFromActivation(t *testing.T) {
	base := rel("v1",
		[]domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")},
		[]domain.ReleaseResource{planned("chat", domain.ActivationEnabled, 0, domain.ActivationSourceComponent)},
		digestA)
	variant := rel("v1-dev",
		[]domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")},
		[]domain.ReleaseResource{planned("chat", domain.ActivationDisabled, 0, domain.ActivationSourceRelease)},
		digestA)

	cs := Diff(base, variant)
	if !cs.SameSource {
		t.Fatal("two releases pinning the same commits were not reported as same-source")
	}
	if len(cs.Resources) != 1 || cs.Resources[0].Change != "activation" {
		t.Fatalf("%+v", cs.Resources)
	}
	if cs.Resources[0].From != "enabled" || cs.Resources[0].To != "disabled" {
		t.Fatalf("%+v", cs.Resources[0])
	}
	if cs.Enabled != 0 || cs.Disabled != 1 {
		t.Fatalf("counts %d/%d", cs.Enabled, cs.Disabled)
	}
}

func TestDiffDistinguishesSourceFromAssetChanges(t *testing.T) {
	base := rel("v1", []domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")}, nil, digestA)

	sourceOnly := rel("v2", []domain.ReleaseComponent{comp("repo_a", "a", commit2, digestA, "resources/[a]")}, nil, digestB)
	cs := Diff(base, sourceOnly)
	if cs.Components[0].Change != "updated" || cs.Components[0].AssetsChanged {
		t.Fatalf("a code-only change was reported as an asset change: %+v", cs.Components[0])
	}

	withAssets := rel("v2", []domain.ReleaseComponent{comp("repo_a", "a", commit2, digestB, "resources/[a]")}, nil, digestB)
	cs = Diff(base, withAssets)
	if !cs.Components[0].AssetsChanged {
		t.Fatalf("an asset change was not reported: %+v", cs.Components[0])
	}
}

func TestDiffReportsAddedRemovedAndRemounted(t *testing.T) {
	base := rel("v1", []domain.ReleaseComponent{
		comp("repo_a", "a", commit1, digestA, "resources/[a]"),
		comp("repo_gone", "gone", commit1, digestA, "resources/[gone]"),
	}, []domain.ReleaseResource{planned("old", domain.ActivationEnabled, 0, domain.ActivationSourceComponent)}, digestA)
	next := rel("v2", []domain.ReleaseComponent{
		comp("repo_a", "a", commit1, digestA, "resources/[moved]"),
		comp("repo_new", "new", commit2, digestB, "resources/[new]"),
	}, []domain.ReleaseResource{planned("fresh", domain.ActivationEnabled, 0, domain.ActivationSourceComponent)}, digestB)

	cs := Diff(base, next)
	got := map[string]ComponentChange{}
	for _, c := range cs.Components {
		got[c.RepositoryID] = c
	}
	if got["repo_a"].Change != "updated" || !got["repo_a"].MountChanged {
		t.Fatalf("remount not reported: %+v", got["repo_a"])
	}
	if got["repo_new"].Change != "added" || got["repo_gone"].Change != "removed" {
		t.Fatalf("%+v", cs.Components)
	}
	var changes []string
	for _, r := range cs.Resources {
		changes = append(changes, r.Name+":"+r.Change)
	}
	if strings.Join(changes, ",") != "fresh:added,old:removed" {
		t.Fatalf("resource changes %v", changes)
	}
}

func TestDiffAgainstNothingReportsEverythingAsAdded(t *testing.T) {
	next := rel("v1", []domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")},
		[]domain.ReleaseResource{planned("chat", domain.ActivationEnabled, 0, domain.ActivationSourceComponent)}, digestA)
	cs := Diff(domain.Release{}, next)
	if cs.SameSource {
		t.Fatal("a first release cannot share source with nothing")
	}
	if cs.Components[0].Change != "added" || cs.Resources[0].Change != "added" {
		t.Fatalf("%+v %+v", cs.Components, cs.Resources)
	}
}

func TestNotesExplainSameSourceAndActivation(t *testing.T) {
	base := rel("v1", []domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")},
		[]domain.ReleaseResource{planned("chat", domain.ActivationEnabled, 0, domain.ActivationSourceComponent)}, digestA)
	next := rel("v1-dev", []domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")},
		[]domain.ReleaseResource{planned("chat", domain.ActivationDisabled, 0, domain.ActivationSourceRelease)}, digestA)

	md := Notes(Diff(base, next), next)
	for _, want := range []string{"# v1-dev", "Same source as the previous release", "`chat` is now **disabled**", "0 resources enabled, 1 disabled"} {
		if !strings.Contains(md, want) {
			t.Fatalf("notes are missing %q:\n%s", want, md)
		}
	}
	// The draft must never claim to be the release record.
	if strings.Contains(md, "sourceSetDigest") || strings.Contains(md, digestA) {
		t.Fatalf("notes leak record internals:\n%s", md)
	}
}

func TestNotesAreDeterministic(t *testing.T) {
	base := rel("v1", []domain.ReleaseComponent{comp("repo_a", "a", commit1, digestA, "resources/[a]")}, nil, digestA)
	next := rel("v2", []domain.ReleaseComponent{
		comp("repo_a", "a", commit2, digestB, "resources/[a]"),
		comp("repo_b", "b", commit1, digestA, "resources/[b]"),
	}, []domain.ReleaseResource{
		planned("one", domain.ActivationEnabled, 0, domain.ActivationSourceComponent),
		planned("two", domain.ActivationEnabled, 1, domain.ActivationSourceComponent),
	}, digestB)
	first := Notes(Diff(base, next), next)
	for range 5 {
		if got := Notes(Diff(base, next), next); got != first {
			t.Fatalf("notes are not deterministic:\n%s\n---\n%s", first, got)
		}
	}
}

func TestComponentTagsAreNamespacedPerRelease(t *testing.T) {
	r := rel("v1.4.0", []domain.ReleaseComponent{
		comp("repo_a", "a", commit1, digestA, "resources/[a]"),
		comp("repo_b", "b", commit2, digestB, "resources/[b]"),
	}, nil, digestA)
	tags := PlanComponentTags(r)
	if len(tags) != 2 {
		t.Fatalf("%+v", tags)
	}
	if tags[0].Tag != "fxvcs/components/v1.4.0/repo_a" || tags[0].Commit != commit1 {
		t.Fatalf("%+v", tags[0])
	}
	// Two releases must never plan the same tag for one component.
	other := PlanComponentTags(rel("v1.5.0", r.Spec.Components, nil, digestA))
	if other[0].Tag == tags[0].Tag {
		t.Fatal("component tags are not namespaced by release")
	}
}
