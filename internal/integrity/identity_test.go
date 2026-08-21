package integrity

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

func d(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func TestManifestDigestDeterministic(t *testing.T) {
	m := domain.ObjectManifest{FormatVersion: 1, StorageDomainID: "domain_x", Encoding: "whole/1", Compression: "none", OID: d("a"), Size: 10,
		Chunks: []domain.ManifestChunk{{Digest: d("a"), RawSize: 10, EncodedSize: 10}}}
	a, err := ManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := ManifestDigest(m)
	if a != b || !IsDigest(a) {
		t.Fatal("nondeterministic")
	}
	m.Size = 11
	if _, err := ManifestDigest(m); err == nil {
		t.Fatal("size mismatch accepted")
	}
	m.Size = 10
	m.FormatVersion = 2
	if _, err := ManifestDigest(m); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestAssetRootEmptyIdentityIsStable(t *testing.T) {
	a, err := AssetRoot(domain.AssetIndex{FormatVersion: 1, StorageDomainID: "domain_x"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := AssetRoot(domain.AssetIndex{FormatVersion: 1, StorageDomainID: "domain_x", Entries: []domain.AssetIndexEntry{}})
	if a != b {
		t.Fatal("nil and empty entries hash differently")
	}
}

func TestAssetRootOrderingAndValidation(t *testing.T) {
	idx := domain.AssetIndex{FormatVersion: 1, StorageDomainID: "domain_x", Entries: []domain.AssetIndexEntry{
		{Path: "b.ytd", Mode: "100644", PointerDigest: d("b")},
		{Path: "a.ytd", Mode: "100644", PointerDigest: d("a")},
	}}
	if _, err := AssetRoot(idx); err == nil {
		t.Fatal("unsorted accepted")
	}
	SortAssetIndex(&idx)
	r1, err := AssetRoot(idx)
	if err != nil {
		t.Fatal(err)
	}
	idx.Entries[0].Mode = "100755"
	r2, _ := AssetRoot(idx)
	if r1 == r2 {
		t.Fatal("mode change did not change root")
	}
	idx.Entries[0].Path = "../a.ytd"
	if _, err := AssetRoot(idx); err == nil {
		t.Fatal("traversal accepted")
	}
	idx.Entries[0].Path = "a.ytd"
	idx.Entries[0].Mode = "120000"
	if _, err := AssetRoot(idx); err == nil {
		t.Fatal("symlink mode accepted")
	}
}

func sampleRelease() domain.Release {
	rn := "inventory"
	return domain.Release{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRelease},
		Metadata: domain.ReleaseMetadata{Name: "v1.4.0", CreatedAt: "2026-08-15T00:00:00Z", CreatedBy: "scott"},
		Spec: domain.ReleaseSpec{
			StackID: "stack_01",
			Components: []domain.ReleaseComponent{
				{RepositoryID: "repo_maps_01", RepositoryName: "maps", Remote: "git@github.com:x/maps.git", Commit: strings.Repeat("a", 40), AssetRoot: d("1"), StorageDomainID: "domain_maps_01",
					Descriptor: domain.ComponentDescriptor{SourceRoot: ".", Layout: "collection"}, DescriptorFrom: "component", Mount: "resources/[maps]"},
				{RepositoryID: "repo_inv_01", RepositoryName: "inventory", Remote: "git@github.com:x/inv.git", Commit: strings.Repeat("b", 40), AssetRoot: d("2"), StorageDomainID: "domain_inv_01",
					Descriptor: domain.ComponentDescriptor{SourceRoot: ".", Layout: "single", ResourceName: &rn}, DescriptorFrom: "component", Mount: "resources/[inventory]/inventory"},
			},
			RuntimeResources: []domain.RuntimeResource{
				{ID: "external:host/custom-anticheat", Name: "custom-anticheat", Source: "external", Activation: "enabled",
					Verifier: &domain.VerifierContract{Contract: "resource-present", Version: 1}, Lifecycle: &domain.LifecycleContract{Ownership: "external", Actions: "observe-only"}},
				{ID: "runtime:system/monitor", Name: "monitor", Source: "runtime", Activation: "enabled"},
			},
			Resources: []domain.ReleaseResource{
				{Source: "component", RepositoryID: "repo_maps_01", RepositoryName: "maps", Path: "city-map", Name: "city-map", Activation: "enabled", ActivationSource: "component", EnsureOrder: 0},
			},
		},
	}
}

func TestSourceSetDigestExcludesDisplayAndActivation(t *testing.T) {
	r := sampleRelease()
	base, err := SourceSetDigest(r.Spec)
	if err != nil {
		t.Fatal(err)
	}
	// Display rename, activation flip, order change, and runtime name change must not alter it.
	r2 := sampleRelease()
	r2.Spec.Components[0].RepositoryName = "maps-renamed"
	r2.Spec.Resources[0].Activation = "disabled"
	r2.Spec.Resources[0].ActivationSource = "release-override"
	r2.Spec.RuntimeResources[1].Name = "renamed"
	r2.Spec.RuntimeResources[0], r2.Spec.RuntimeResources[1] = r2.Spec.RuntimeResources[1], r2.Spec.RuntimeResources[0]
	r2.Spec.Components[0], r2.Spec.Components[1] = r2.Spec.Components[1], r2.Spec.Components[0]
	if got, _ := SourceSetDigest(r2.Spec); got != base {
		t.Fatal("display/activation/order changed sourceSetDigest")
	}
	// Commit, mount, verifier version, and descriptor change must alter it.
	for _, mut := range []func(*domain.ReleaseSpec){
		func(s *domain.ReleaseSpec) { s.Components[0].Commit = strings.Repeat("c", 40) },
		func(s *domain.ReleaseSpec) { s.Components[0].Mount = "resources/[maps2]" },
		func(s *domain.ReleaseSpec) { s.Components[0].AssetRoot = d("9") },
		func(s *domain.ReleaseSpec) { s.RuntimeResources[0].Verifier.Version = 2 },
		func(s *domain.ReleaseSpec) { s.RuntimeResources[0].Lifecycle.Actions = "restart" },
		func(s *domain.ReleaseSpec) { s.Components[1].Descriptor.Layout = "collection" },
		func(s *domain.ReleaseSpec) { s.RuntimeResources = s.RuntimeResources[:1] },
	} {
		r3 := sampleRelease()
		mut(&r3.Spec)
		if got, _ := SourceSetDigest(r3.Spec); got == base {
			t.Fatal("source change did not alter sourceSetDigest")
		}
	}
	// Release digest changes when display name changes; signature excluded.
	rd1, err := ReleaseDigest(r)
	if err != nil {
		t.Fatal(err)
	}
	rs := sampleRelease()
	rs.Spec.Signature = &domain.ReleaseSignature{Profile: "ssh-ed25519-v1", Signatures: []string{"x"}}
	if rd2, _ := ReleaseDigest(rs); rd2 != rd1 {
		t.Fatal("signature altered release digest")
	}
	if rd3, _ := ReleaseDigest(r2); rd3 == rd1 {
		t.Fatal("release digest ignored spec change")
	}
}
