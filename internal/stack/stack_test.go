package stack

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

func str(s string) *string { return &s }

func component(id, name, mount, layout string, resourceName *string, resources ...domain.CatalogResource) Component {
	return Component{
		Spec: domain.StackComponent{RepositoryID: id, Remote: "https://example.invalid/" + name + ".git", Mount: mount, Required: true},
		Config: &domain.Repository{
			Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRepository},
			Metadata: domain.RepositoryMetadata{RepositoryID: id, StorageDomainID: "domain_" + id, Name: name},
			Spec:     domain.RepositorySpec{Profile: domain.ProfileFiveM, SourceRoot: ".", Layout: layout, ResourceName: resourceName},
		},
		Catalog: &domain.ResourceCatalog{
			Metadata: domain.ResourceCatalogMetadata{RepositoryID: id},
			Spec:     domain.ResourceCatalogSpec{Resources: resources},
		},
		Commit:    strings.Repeat("a", 40),
		AssetRoot: "sha256:" + strings.Repeat("b", 64),
	}
}

func res(path, name string, activation domain.Activation, after ...string) domain.CatalogResource {
	return domain.CatalogResource{Path: path, Name: name, Activation: activation, After: after, Status: domain.ResourceStatusPresent}
}

func names(p Plan) []string {
	out := make([]string, 0, len(p.Resources))
	for _, r := range p.Resources {
		out = append(out, r.Name)
	}
	return out
}

func problemText(probs []Problem) string {
	parts := make([]string, 0, len(probs))
	for _, p := range probs {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// mounts and descriptors
// ---------------------------------------------------------------------------

func TestMountMustMatchTheAcceptedLayout(t *testing.T) {
	cases := []struct {
		name, mount, layout string
		resourceName        *string
		wantProblem         string
	}{
		{"collection in a category", "resources/[maps]", domain.LayoutCollection, nil, ""},
		{"collection as a plain folder", "resources/maps", domain.LayoutCollection, nil, "bracketed category"},
		{"single inside a category", "resources/[inventory]/inventory", domain.LayoutSingle, str("inventory"), ""},
		{"single as a bare category", "resources/[inventory]", domain.LayoutSingle, str("inventory"), "resource directory name"},
		{"single directly under resources", "resources/inventory", domain.LayoutSingle, str("inventory"), "bracketed category"},
		{"outside resources", "server-data/[maps]", domain.LayoutCollection, nil, "under resources/"},
		{"escaping the realization", "resources/../../etc", domain.LayoutCollection, nil, "safe relative path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := component("repo_a", "a", tc.mount, tc.layout, tc.resourceName, res("x", "x", domain.ActivationEnabled))
			probs := ValidateComposition([]Component{c})
			got := problemText(probs)
			switch {
			case tc.wantProblem == "" && got != "":
				t.Fatalf("unexpected problem: %s", got)
			case tc.wantProblem != "" && !strings.Contains(got, tc.wantProblem):
				t.Fatalf("want a problem mentioning %q, got %q", tc.wantProblem, got)
			}
		})
	}
}

func TestOverlappingMountsAreRefused(t *testing.T) {
	a := component("repo_a", "a", "resources/[shared]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	b := component("repo_b", "b", "resources/[shared]/[inner]", domain.LayoutCollection, nil, res("y", "y", domain.ActivationEnabled))
	if got := problemText(ValidateComposition([]Component{a, b})); !strings.Contains(got, "overlaps") {
		t.Fatalf("want an overlap problem, got %q", got)
	}
}

func TestComponentIdentityMustMatchTheStackRecord(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	c.Config.Metadata.RepositoryID = "repo_somewhere_else"
	if got := problemText(ValidateComposition([]Component{c})); !strings.Contains(got, "declares repositoryID") {
		t.Fatalf("want an identity problem, got %q", got)
	}
}

func TestAliasResolvesADisplayNameCollision(t *testing.T) {
	a := component("repo_a", "tools", "resources/[tools]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	b := component("repo_b", "tools", "resources/[community-tools]", domain.LayoutCollection, nil, res("y", "y", domain.ActivationEnabled))
	if got := problemText(ValidateComposition([]Component{a, b})); !strings.Contains(got, "already used") {
		t.Fatalf("want a display-name problem, got %q", got)
	}
	b.Spec.Alias = "community-tools"
	if got := problemText(ValidateComposition([]Component{a, b})); got != "" {
		t.Fatalf("an alias did not resolve the collision: %s", got)
	}
	if b.DisplayName() != "community-tools" {
		t.Fatalf("display name %q", b.DisplayName())
	}
}

func TestDescriptorOverrideIsRecordedAsSuch(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	if _, from := c.Descriptor(); from != domain.DescriptorFromComponent {
		t.Fatalf("provenance %q", from)
	}
	if c.OverridesComponent() {
		t.Fatal("no override was recorded")
	}
	c.Spec.DescriptorOverride = &domain.ComponentDescriptor{SourceRoot: "src", Layout: domain.LayoutCollection}
	desc, from := c.Descriptor()
	if from != domain.DescriptorFromStack || desc.SourceRoot != "src" {
		t.Fatalf("%+v %q", desc, from)
	}
	if !c.OverridesComponent() {
		t.Fatal("a descriptor that disagrees with the component was not flagged")
	}
}

func TestProposeMount(t *testing.T) {
	got, err := ProposeMount("maps", domain.ComponentDescriptor{Layout: domain.LayoutCollection})
	if err != nil || got != "resources/[maps]" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = ProposeMount("inventory", domain.ComponentDescriptor{Layout: domain.LayoutSingle, ResourceName: str("inventory")})
	if err != nil || got != "resources/[inventory]/inventory" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ProposeMount("../evil", domain.ComponentDescriptor{Layout: domain.LayoutCollection}); err == nil {
		t.Fatal("an unsafe display name produced a mount")
	}
}

// ---------------------------------------------------------------------------
// activation plan
// ---------------------------------------------------------------------------

func TestPlanAppliesOverridesInPrecedenceOrderAndRecordsProvenance(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil,
		res("keep", "keep", domain.ActivationEnabled),
		res("coord", "coord", domain.ActivationEnabled),
		res("rel", "rel", domain.ActivationEnabled),
	)
	disabled := domain.ActivationDisabled
	plan, probs := ResolvePlan(PlanInput{
		Components: []Component{c},
		Overrides: &domain.ResourceOverrides{Spec: domain.ResourceOverridesSpec{Resources: []domain.ResourceOverride{
			{RepositoryID: "repo_a", Path: "coord", Activation: &disabled},
			{RepositoryID: "repo_a", Path: "rel", Activation: &disabled},
		}}},
		ReleaseOverrides: []Override{{Selector: "rel", Activation: domain.ActivationEnabled}},
	})
	if len(probs) > 0 {
		t.Fatalf("unexpected problems: %s", problemText(probs))
	}
	got := map[string]string{}
	for _, r := range plan.Resources {
		got[r.Name] = string(r.Activation) + "/" + r.ActivationSource
	}
	want := map[string]string{
		"keep":  "enabled/" + domain.ActivationSourceComponent,
		"coord": "disabled/" + domain.ActivationSourceCoordination,
		"rel":   "enabled/" + domain.ActivationSourceRelease,
	}
	for name, w := range want {
		if got[name] != w {
			t.Fatalf("%s: got %s, want %s", name, got[name], w)
		}
	}
}

func TestPlanRefusesAnUndecidedResource(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationPending))
	_, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if !strings.Contains(problemText(probs), "still pending") {
		t.Fatalf("want a pending problem, got %q", problemText(probs))
	}
}

func TestPlanRefusesAMissingResource(t *testing.T) {
	gone := res("x", "x", domain.ActivationEnabled)
	gone.Status = domain.ResourceStatusMissing
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, gone)
	_, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if !strings.Contains(problemText(probs), "missing") {
		t.Fatalf("want a missing problem, got %q", problemText(probs))
	}
}

func TestPlanRefusesADuplicateResourceName(t *testing.T) {
	a := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("chat", "chat", domain.ActivationEnabled))
	b := component("repo_b", "b", "resources/[b]", domain.LayoutCollection, nil, res("chat", "chat", domain.ActivationEnabled))
	_, probs := ResolvePlan(PlanInput{Components: []Component{a, b}})
	if !strings.Contains(problemText(probs), "declared twice") {
		t.Fatalf("want a duplicate-name problem, got %q", problemText(probs))
	}
}

func TestPlanRefusesAnUnresolvableOrderingReference(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled, "nowhere"))
	_, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if !strings.Contains(problemText(probs), "no resource in this composition provides") {
		t.Fatalf("want an unresolved-reference problem, got %q", problemText(probs))
	}
}

func TestPlanResolvesOrderingThroughProvides(t *testing.T) {
	provider := res("mm", "mapmanager-fork", domain.ActivationEnabled)
	provider.Provides = []string{"mapmanager"}
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil,
		provider, res("city", "city", domain.ActivationEnabled, "mapmanager"))
	plan, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if len(probs) > 0 {
		t.Fatalf("provides did not satisfy the reference: %s", problemText(probs))
	}
	if got := names(plan); got[0] != "mapmanager-fork" {
		t.Fatalf("order %v", got)
	}
}

func TestPlanRefusesAnOrderingCycle(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil,
		res("one", "one", domain.ActivationEnabled, "two"),
		res("two", "two", domain.ActivationEnabled, "one"),
	)
	_, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if !strings.Contains(problemText(probs), "cycle") {
		t.Fatalf("want a cycle problem, got %q", problemText(probs))
	}
}

// A dependency loop is common in the wild and FXServer resolves it at start
// time, so it orders the plan when it can and never blocks a release.
func TestDependencyLoopOrdersInsteadOfFailing(t *testing.T) {
	a := res("a", "a", domain.ActivationEnabled)
	a.Dependencies = []string{"b"}
	b := res("b", "b", domain.ActivationEnabled)
	b.Dependencies = []string{"a"}
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, a, b)
	plan, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if len(probs) > 0 {
		t.Fatalf("a dependency loop blocked a release: %s", problemText(probs))
	}
	if len(plan.Resources) != 2 {
		t.Fatalf("plan %v", names(plan))
	}
}

func TestDependenciesOrderThePlanWhenTheyCan(t *testing.T) {
	dependent := res("bank", "bank", domain.ActivationEnabled)
	dependent.Dependencies = []string{"city-map"}
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil,
		dependent, res("city-map", "city-map", domain.ActivationEnabled))
	plan, probs := ResolvePlan(PlanInput{Components: []Component{c}})
	if len(probs) > 0 {
		t.Fatal(problemText(probs))
	}
	if got := names(plan); got[0] != "city-map" || got[1] != "bank" {
		t.Fatalf("a dependency did not order the plan: %v", got)
	}
}

func TestRuntimeResourcesComeFirstAndKeepTheirProvenance(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	plan, probs := ResolvePlan(PlanInput{
		Components: []Component{c},
		Runtime: &domain.RuntimeResourceCatalog{Spec: domain.RuntimeResourceCatalogSpec{Resources: []domain.RuntimeResource{
			{ID: "runtime:system/monitor", Name: "monitor", Source: domain.ResourceSourceRuntime, Activation: domain.ActivationEnabled},
		}}},
	})
	if len(probs) > 0 {
		t.Fatal(problemText(probs))
	}
	if got := names(plan); got[0] != "monitor" {
		t.Fatalf("order %v", got)
	}
	if plan.Resources[0].ActivationSource != domain.ActivationSourceRuntime {
		t.Fatalf("provenance %q", plan.Resources[0].ActivationSource)
	}
	if len(plan.RuntimeResources) != 1 {
		t.Fatalf("the runtime entry was not snapshotted: %+v", plan.RuntimeResources)
	}
}

func TestPlanIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	build := func(order ...string) Plan {
		var comps []Component
		for _, id := range order {
			comps = append(comps, component(id, id, "resources/["+id+"]", domain.LayoutCollection, nil,
				res(id+"-one", id+"-one", domain.ActivationEnabled),
				res(id+"-two", id+"-two", domain.ActivationEnabled)))
		}
		plan, probs := ResolvePlan(PlanInput{Components: comps})
		if len(probs) > 0 {
			t.Fatal(problemText(probs))
		}
		return plan
	}
	first := strings.Join(names(build("repo_a", "repo_b")), ",")
	second := strings.Join(names(build("repo_b", "repo_a")), ",")
	if first != second {
		t.Fatalf("plan depends on stack file order:\n%s\n%s", first, second)
	}
}

func TestOverrideForAnUnknownResourceIsReported(t *testing.T) {
	c := component("repo_a", "a", "resources/[a]", domain.LayoutCollection, nil, res("x", "x", domain.ActivationEnabled))
	off := domain.ActivationDisabled
	_, probs := ResolvePlan(PlanInput{
		Components: []Component{c},
		Overrides: &domain.ResourceOverrides{Spec: domain.ResourceOverridesSpec{Resources: []domain.ResourceOverride{
			{RepositoryID: "repo_a", Path: "does-not-exist", Activation: &off},
		}}},
	})
	if !strings.Contains(problemText(probs), "matches no resource") {
		t.Fatalf("a stale override passed silently: %q", problemText(probs))
	}
}

func TestMountForResolvesRealizationPaths(t *testing.T) {
	collection := domain.ComponentDescriptor{SourceRoot: ".", Layout: domain.LayoutCollection}
	got, err := MountFor("resources/[maps]", collection, "[cars]/sultan")
	if err != nil || got != "resources/[maps]/[cars]/sultan" {
		t.Fatalf("%q %v", got, err)
	}
	single := domain.ComponentDescriptor{SourceRoot: ".", Layout: domain.LayoutSingle, ResourceName: str("inventory")}
	got, err = MountFor("resources/[inventory]/inventory", single, "inventory")
	if err != nil || got != "resources/[inventory]/inventory" {
		t.Fatalf("%q %v", got, err)
	}
}
