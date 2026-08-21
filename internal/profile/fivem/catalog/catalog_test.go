package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

const fixtureRoot = "../../../../spec/fixtures/fivem"

func scanCollection(t *testing.T) []discovery.DiscoveredResource {
	t.Helper()
	res, err := discovery.ScanDir(context.Background(), filepath.Join(fixtureRoot, "collection"), domain.ComponentDescriptor{SourceRoot: ".", Layout: discovery.LayoutCollection})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func find(cat *domain.ResourceCatalog, path string) domain.CatalogResource {
	for _, r := range cat.Spec.Resources {
		if r.Path == path {
			return r
		}
	}
	return domain.CatalogResource{}
}

func TestSyncFromScratchAndIdempotent(t *testing.T) {
	res := scanCollection(t)
	cat, rep, err := Sync(nil, res, domain.ActivationPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Added) != 7 || len(cat.Spec.Resources) != 7 {
		t.Fatalf("report %+v", rep)
	}
	// `/`-prefixed manifest dependencies are runtime requirements, not resource
	// dependencies: they never enter the dependency graph.
	rr := find(cat, "runtime-reqs")
	if len(rr.Dependencies) != 1 || rr.Dependencies[0] != "mapmanager" || len(rr.RuntimeRequirements) != 6 {
		t.Fatalf("runtime requirements leaked into dependencies: %+v", rr)
	}
	for i := 1; i < len(cat.Spec.Resources); i++ {
		if cat.Spec.Resources[i-1].Path >= cat.Spec.Resources[i].Path {
			t.Fatal("not sorted by path")
		}
	}
	bank := find(cat, "[maps]/[interiors]/bank")
	if bank.Activation != domain.ActivationPending || bank.Status != StatusPresent || bank.MutableMode != MutableImmutable ||
		strings.Join(bank.Dependencies, ",") != "city-map,mapmanager" || bank.After == nil || bank.PersistentPaths == nil {
		t.Fatalf("bank: %+v", bank)
	}
	if fork := find(cat, "mapmanager-fork"); fork.Provides[0] != "mapmanager" {
		t.Fatalf("provides: %+v", fork)
	}
	if ac := find(cat, "anticheat"); !ac.ServerOnly {
		t.Fatalf("serverOnly: %+v", ac)
	}
	if mg := find(cat, "multi-game"); strings.Join(mg.Games, ",") != "gta5,rdr3" {
		t.Fatalf("games: %+v", mg)
	}

	again, rep2, err := Sync(cat, res, domain.ActivationPending)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(cat, again); diff != "" {
		t.Fatalf("not idempotent:\n%s", diff)
	}
	if rep2.Changed() || len(rep2.Unchanged) != 7 {
		t.Fatalf("second sync report: %+v", rep2)
	}
	if cat.Kind != "ResourceCatalog" || cat.APIVersion == "" {
		t.Fatalf("envelope: %+v", cat.Envelope)
	}
}

func TestSyncPreservesOperatorFieldsAndTombstones(t *testing.T) {
	res := scanCollection(t)
	cat, _, err := Sync(nil, res, domain.ActivationPending)
	if err != nil {
		t.Fatal(err)
	}
	cat.Metadata.RepositoryID = "repo_x"
	for i := range cat.Spec.Resources {
		r := &cat.Spec.Resources[i]
		r.Activation = domain.ActivationEnabled
		if r.Path == "chat" {
			r.After = []string{"mapmanager"}
			r.PersistentPaths = []string{"data/logs"}
			r.PersistentPathKinds = map[string]string{"data/logs": "directory"}
			r.MutableMode = MutableCopy
			r.Provides = []string{"stale-provide"} // will be refreshed
		}
	}
	// Add a tombstone-to-be and drop one discovered resource.
	cat.Spec.Resources = append(cat.Spec.Resources, domain.CatalogResource{Path: "gone", Name: "gone", Activation: domain.ActivationDisabled, MutableMode: MutableImmutable, Status: StatusPresent})
	var subset []discovery.DiscoveredResource
	for _, r := range res {
		if r.Path != "anticheat" {
			subset = append(subset, r)
		}
	}
	out, rep, err := Sync(cat, subset, domain.ActivationPending)
	if err != nil {
		t.Fatal(err)
	}
	if out.Metadata.RepositoryID != "repo_x" {
		t.Fatal("metadata lost")
	}
	chat := find(out, "chat")
	if chat.Activation != domain.ActivationEnabled || chat.After[0] != "mapmanager" || chat.PersistentPaths[0] != "data/logs" || chat.PersistentPathKinds["data/logs"] != "directory" || chat.MutableMode != MutableCopy {
		t.Fatalf("operator fields not preserved: %+v", chat)
	}
	if len(chat.Provides) != 0 {
		t.Fatalf("provides not refreshed: %+v", chat)
	}
	if strings.Join(rep.Refreshed, ",") != "chat" {
		t.Fatalf("refreshed: %+v", rep)
	}
	for _, p := range []string{"gone", "anticheat"} {
		e := find(out, p)
		if e.Status != StatusMissing || e.Path == "" {
			t.Fatalf("%s should be a missing tombstone: %+v", p, e)
		}
	}
	if strings.Join(rep.Missing, ",") != "anticheat,gone" {
		t.Fatalf("missing: %+v", rep)
	}
	if ac := find(out, "anticheat"); ac.Activation != domain.ActivationEnabled {
		t.Fatalf("tombstone must keep activation: %+v", ac)
	}
	// Reappearance revives and keeps operator fields.
	back, rep3, err := Sync(out, res, domain.ActivationPending)
	if err != nil {
		t.Fatal(err)
	}
	if ac := find(back, "anticheat"); ac.Status != StatusPresent || ac.Activation != domain.ActivationEnabled {
		t.Fatalf("revive: %+v", ac)
	}
	if strings.Join(rep3.Revived, ",") != "anticheat" {
		t.Fatalf("revived: %+v", rep3)
	}
	// Inputs are not mutated.
	if find(cat, "anticheat").Status != StatusPresent {
		t.Fatal("Sync mutated its input")
	}
}

func TestSyncRejectsBadInput(t *testing.T) {
	res := scanCollection(t)
	if _, _, err := Sync(nil, res, domain.Activation("maybe")); !errors.Is(err, ErrSync) {
		t.Fatalf("got %v", err)
	}
	dup := append([]discovery.DiscoveredResource{}, res...)
	dup = append(dup, res[0])
	if _, _, err := Sync(nil, dup, domain.ActivationPending); !errors.Is(err, ErrSync) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := Sync(nil, []discovery.DiscoveredResource{{Path: "x", Name: "x"}}, domain.ActivationPending); !errors.Is(err, ErrSync) {
		t.Fatalf("nil manifest: got %v", err)
	}
}

func releasable(t *testing.T) *domain.ResourceCatalog {
	t.Helper()
	cat, _, err := Sync(nil, scanCollection(t), domain.ActivationEnabled)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

func problems(err error) []string {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := make([]string, 0, len(ve.Problems))
	for _, p := range ve.Problems {
		out = append(out, p.Path+"/"+p.Field)
	}
	return out
}

func TestValidateForRelease(t *testing.T) {
	cat := releasable(t)
	if err := ValidateForRelease(cat, ValidateOptions{}); err != nil {
		t.Fatalf("clean catalog should validate: %v", err)
	}
	// Bring in every problem class.
	bad := releasable(t)
	set := func(path string, f func(r *domain.CatalogResource)) {
		for i := range bad.Spec.Resources {
			if bad.Spec.Resources[i].Path == path {
				f(&bad.Spec.Resources[i])
			}
		}
	}
	set("chat", func(r *domain.CatalogResource) { r.Activation = domain.ActivationPending })
	set("anticheat", func(r *domain.CatalogResource) { r.Status = StatusMissing })
	set("multi-game", func(r *domain.CatalogResource) {
		r.After = []string{"nowhere"}
		r.MutableMode = "rw"
		r.PersistentPaths = []string{"data", "data/sub", "../up"}
		r.PersistentPathKinds = map[string]string{"data": "directory", "data/sub": "directory", "other": "file"}
	})
	set("[maps]/city-map", func(r *domain.CatalogResource) { r.Before = []string{"mapmanager"} }) // mapmanager -> city-map (dependency) and city-map -> mapmanager: cycle
	bad.Spec.Resources = append(bad.Spec.Resources, domain.CatalogResource{Path: "dup", Name: "chat", Activation: domain.ActivationEnabled, MutableMode: MutableImmutable, Status: StatusPresent})
	err := ValidateForRelease(bad, ValidateOptions{})
	if !errors.Is(err, ErrNotReleasable) {
		t.Fatalf("got %v", err)
	}
	got := strings.Join(problems(err), " ")
	if msg := err.Error(); !strings.Contains(msg, "cycle:") || !strings.Contains(msg, "city-map") || !strings.Contains(msg, "mapmanager-fork") {
		t.Errorf("cycle not reported: %s", msg)
	}
	for _, want := range []string{"/ordering", "anticheat/status", "chat/activation", "dup/name", "multi-game/after", "multi-game/mutableMode", "multi-game/persistentPaths", "multi-game/persistentPathKinds"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing problem %q in %s", want, got)
		}
	}
	// Known external names satisfy ordering references.
	ok := releasable(t)
	for i := range ok.Spec.Resources {
		if ok.Spec.Resources[i].Path == "chat" {
			ok.Spec.Resources[i].After = []string{"monitor"}
		}
	}
	if err := ValidateForRelease(ok, ValidateOptions{}); err == nil {
		t.Fatal("unknown after name must fail")
	}
	if err := ValidateForRelease(ok, ValidateOptions{KnownNames: []string{"monitor"}}); err != nil {
		t.Fatalf("known name: %v", err)
	}
	if err := ValidateForRelease(nil, ValidateOptions{}); err == nil {
		t.Fatal("nil catalog")
	}
}

func TestDiff(t *testing.T) {
	res := scanCollection(t)
	cat := releasable(t)
	if d := Diff(cat, res); !d.Clean() || d.Err() != nil {
		t.Fatalf("expected clean: %+v", d)
	}
	// Drift: drop chat from the catalog (undeclared), add ghost (missing),
	// change anticheat's manifest-derived field (stale).
	var kept []domain.CatalogResource
	for _, r := range cat.Spec.Resources {
		if r.Path == "chat" {
			continue
		}
		if r.Path == "anticheat" {
			r.ServerOnly = false
			r.Dependencies = []string{"x"}
		}
		kept = append(kept, r)
	}
	kept = append(kept, domain.CatalogResource{Path: "ghost", Name: "ghost", Status: StatusPresent})
	cat.Spec.Resources = kept
	d := Diff(cat, res)
	if strings.Join(d.Undeclared, ",") != "chat" || strings.Join(d.Missing, ",") != "ghost" || len(d.Stale) != 2 {
		t.Fatalf("diff: %+v", d)
	}
	if !errors.Is(d.Err(), ErrCatalogDrift) {
		t.Fatalf("err: %v", d.Err())
	}
	// A pinned tombstone is drift even when the directory is back.
	tomb, _, _ := Sync(nil, res, domain.ActivationEnabled)
	tomb.Spec.Resources[0].Status = StatusMissing
	if d := Diff(tomb, res); len(d.Stale) != 1 || d.Stale[0].Field != "status" {
		t.Fatalf("tombstone: %+v", d)
	}
}
