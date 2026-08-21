package app

import (
	"reflect"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/reconcile"
)

func TestDiffComponentResourcesProducesDeterministicTypedOperations(t *testing.T) {
	t.Parallel()
	previous := domain.Release{Spec: domain.ReleaseSpec{
		Components: []domain.ReleaseComponent{{RepositoryID: "repo-a", Commit: "same"}, {RepositoryID: "repo-b", Commit: "old"}},
		Resources: []domain.ReleaseResource{
			{Source: domain.ResourceSourceComponent, RepositoryID: "repo-a", Name: "old-map", Activation: domain.ActivationEnabled, EnsureOrder: 0},
			{Source: domain.ResourceSourceComponent, RepositoryID: "repo-b", Name: "hud", Activation: domain.ActivationEnabled, EnsureOrder: 1},
		},
	}}
	next := domain.Release{Spec: domain.ReleaseSpec{
		Components: []domain.ReleaseComponent{{RepositoryID: "repo-a", Commit: "same"}, {RepositoryID: "repo-b", Commit: "new"}},
		Resources: []domain.ReleaseResource{
			{Source: domain.ResourceSourceComponent, RepositoryID: "repo-b", Name: "hud", Activation: domain.ActivationEnabled, EnsureOrder: 0},
			{Source: domain.ResourceSourceComponent, RepositoryID: "repo-a", Name: "new-map", Activation: domain.ActivationEnabled, EnsureOrder: 1},
		},
	}}
	operations, hot, err := diffComponentResources(previous, next)
	want := []reconcile.ResourceOperation{{Action: "refresh"}, {Action: "stop", Name: "old-map"}, {Action: "restart", Name: "hud"}, {Action: "ensure", Name: "new-map"}}
	if err != nil || !hot || !reflect.DeepEqual(operations, want) {
		t.Fatalf("hot=%v operations=%#v want=%#v err=%v", hot, operations, want, err)
	}
}

func TestDiffComponentResourcesRequiresFullRestartForHostOwnedChanges(t *testing.T) {
	t.Parallel()
	previous := domain.Release{Spec: domain.ReleaseSpec{Resources: []domain.ReleaseResource{{Source: domain.ResourceSourceRuntime, Name: "sessionmanager", Activation: domain.ActivationEnabled}}}}
	next := previous
	next.Spec.Resources = append([]domain.ReleaseResource(nil), previous.Spec.Resources...)
	next.Spec.Resources[0].Activation = domain.ActivationDisabled
	operations, hot, err := diffComponentResources(previous, next)
	if err != nil || hot || operations != nil {
		t.Fatalf("host-owned change hot=%v operations=%#v err=%v", hot, operations, err)
	}
}

func TestSameNonReleaseInputsRequiresFullRestartForEnvironmentOrSecrets(t *testing.T) {
	t.Parallel()
	base := domain.RealizationInputs{ReleaseName: "v1", ReleaseDigest: "one", SourceSetDigest: "one", EnvironmentName: "dev", EnvironmentRevision: 1, SecretVersions: map[string]string{"x": "v1"}}
	next := base
	next.ReleaseName, next.ReleaseDigest, next.SourceSetDigest = "v2", "two", "two"
	if !sameNonReleaseInputs(base, next) {
		t.Fatal("release-only change was not eligible for a resource diff")
	}
	next.EnvironmentRevision = 2
	if sameNonReleaseInputs(base, next) {
		t.Fatal("environment change was incorrectly hot-reload eligible")
	}
}
