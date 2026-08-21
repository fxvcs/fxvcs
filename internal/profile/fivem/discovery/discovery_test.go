package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/manifest"
)

var update = flag.Bool("update", false, "rewrite golden files under spec/fixtures/fivem/expected")

const fixtureRoot = "../../../../spec/fixtures/fivem"

func fixtureFS(t *testing.T, name string) (string, os.FileInfo) {
	t.Helper()
	dir := filepath.Join(fixtureRoot, name)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return dir, info
}

func str(s string) *string { return &s }

// golden is the JSON shape written to spec/fixtures/fivem/expected/<tree>.json.
type golden struct {
	Detection Detection            `json:"detection"`
	Resources []DiscoveredResource `json:"resources"`
}

func TestValidTreesGolden(t *testing.T) {
	cases := []struct {
		tree string
		want domain.ComponentDescriptor
	}{
		{"single-resource", domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutSingle, ResourceName: str("single-resource")}},
		{"collection", domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutCollection}},
		{"resources-wrapper", domain.ComponentDescriptor{SourceRoot: "resources", Layout: LayoutCollection}},
	}
	for _, tc := range cases {
		t.Run(tc.tree, func(t *testing.T) {
			dir, _ := fixtureFS(t, tc.tree)
			det, err := DetectDir(dir)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if diff := cmp.Diff(tc.want, det.Descriptor); diff != "" {
				t.Fatalf("descriptor (-want +got):\n%s", diff)
			}
			res, err := ScanDir(context.Background(), dir, det.Descriptor)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			got, err := json.MarshalIndent(golden{Detection: det, Resources: res}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			gp := filepath.Join(fixtureRoot, "expected", tc.tree+".json")
			if *update {
				if err := os.WriteFile(gp, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(gp)
			if err != nil {
				t.Fatalf("golden missing (run with -update): %v", err)
			}
			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Fatalf("golden mismatch for %s (-want +got):\n%s", tc.tree, diff)
			}
			// Scan twice: deterministic.
			again, err := ScanDir(context.Background(), dir, det.Descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(res, again); diff != "" {
				t.Fatalf("scan is not deterministic:\n%s", diff)
			}
		})
	}
}

func TestCollectionSemantics(t *testing.T) {
	dir, _ := fixtureFS(t, "collection")
	res, err := ScanDir(context.Background(), dir, domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutCollection})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]DiscoveredResource{}
	for _, r := range res {
		byPath[r.Path] = r
	}
	if len(res) != 7 {
		t.Fatalf("want 7 resources, got %d", len(res))
	}
	if r := byPath["runtime-reqs"]; len(r.Manifest.Dependencies) != 1 || r.Manifest.Dependencies[0] != "mapmanager" ||
		len(r.Manifest.RuntimeRequirements) != 6 || r.Manifest.RuntimeRequirements[0] != "/assetpacks" {
		t.Fatalf("runtime requirements not split from dependencies: %+v", r.Manifest)
	}
	if r := byPath["[maps]/[interiors]/bank"]; r.Name != "bank" || !r.Manifest.IsMap || len(r.Manifest.Dependencies) != 2 {
		t.Fatalf("bank: %+v", r)
	}
	if r := byPath["chat"]; r.ManifestFile != LegacyManifest || !r.Manifest.LegacyFormat || r.Manifest.Name != "chat" {
		t.Fatalf("chat: %+v", r)
	}
	if r := byPath["mapmanager-fork"]; len(r.Manifest.Provides) != 1 || r.Manifest.Provides[0] != "mapmanager" {
		t.Fatalf("provide: %+v", r)
	}
	if r := byPath["anticheat"]; !r.Manifest.ServerOnly {
		t.Fatalf("server_only: %+v", r)
	}
	if r := byPath["multi-game"]; len(r.Manifest.Games) != 2 || r.Manifest.Games[0] != "gta5" || r.Manifest.Games[1] != "rdr3" {
		t.Fatalf("games: %+v", r)
	}
	if r := byPath["[maps]/city-map"]; len(r.Manifest.DataFiles) != 1 || r.Manifest.DataFiles[0].Path != "stream/city_props.ytyp" {
		t.Fatalf("data_file: %+v", r)
	}
}

func TestDetectFailures(t *testing.T) {
	cases := []struct {
		tree   string
		reason Reason
	}{
		{"ambiguous-mixed", ReasonMixedShape},
		{"duplicate-manifests", ReasonDuplicateManifests},
		{"vendor-only", ReasonVendorOnly},
	}
	for _, tc := range cases {
		t.Run(tc.tree, func(t *testing.T) {
			dir, _ := fixtureFS(t, tc.tree)
			_, err := DetectDir(dir)
			var de *Error
			if !errors.As(err, &de) || de.Reason != tc.reason {
				t.Fatalf("got %v, want reason %s", err, tc.reason)
			}
			if !errors.Is(err, ErrDiscovery) {
				t.Fatal("errors.Is(ErrDiscovery) must hold")
			}
		})
	}
}

func manifestFile(extra string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("fx_version 'cerulean'\ngame 'gta5'\n" + extra)}
}

func TestDetectSyntheticShapes(t *testing.T) {
	t.Run("no manifests", func(t *testing.T) {
		_, err := Detect(fstest.MapFS{"README.md": {Data: []byte("x")}}, "empty")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonNoManifests {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("root and wrapper both reachable", func(t *testing.T) {
		fsys := fstest.MapFS{
			"a/fxmanifest.lua":           manifestFile(""),
			"resources/b/fxmanifest.lua": manifestFile(""),
		}
		_, err := Detect(fsys, "x")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonAmbiguousSourceRoot || len(de.Candidates) != 2 {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("non-conventional wrapper is not guessed", func(t *testing.T) {
		fsys := fstest.MapFS{"src/[core]/a/fxmanifest.lua": manifestFile("")}
		_, err := Detect(fsys, "x")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonAmbiguousSourceRoot {
			t.Fatalf("got %v", err)
		}
		if len(de.Candidates) != 1 || de.Candidates[0].SourceRoot != "src" || de.Candidates[0].Resources[0] != "[core]/a" {
			t.Fatalf("candidates: %+v", de.Candidates)
		}
		// But an explicit descriptor scans it fine.
		res, err := Scan(context.Background(), fsys, domain.ComponentDescriptor{SourceRoot: "src", Layout: LayoutCollection})
		if err != nil || len(res) != 1 || res[0].Path != "[core]/a" {
			t.Fatalf("scan: %v %+v", err, res)
		}
	})
	t.Run("unreachable plain nesting is skipped, not fatal", func(t *testing.T) {
		fsys := fstest.MapFS{
			"a/fxmanifest.lua":       manifestFile(""),
			"plain/b/fxmanifest.lua": manifestFile(""),
		}
		det, err := Detect(fsys, "x")
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if len(det.Resources) != 1 || det.Resources[0] != "a" {
			t.Fatalf("resources: %+v", det.Resources)
		}
		// "plain" itself is what FXServer would try to load as a resource and
		// warn about; "plain/b" is never visited. Both are reported skipped.
		if len(det.Skipped) != 1 || det.Skipped[0].Path != "plain/b" || det.Skipped[0].Reason != ReasonUnreachable {
			t.Fatalf("skipped: %+v", det.Skipped)
		}
		// A scan with the accepted descriptor agrees.
		res, err := ScanTree(context.Background(), fsys, det.Descriptor, ScanOptions{})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(res.Resources) != 1 || len(res.Skipped) != 1 || res.Skipped[0].Path != "plain/b" {
			t.Fatalf("scan: %+v", res)
		}
	})
	t.Run("deeply unreachable resource does not fail the component", func(t *testing.T) {
		// The customer shape: a vendor drop under two plain directories.
		fsys := fstest.MapFS{
			"[core]/keep/fxmanifest.lua":                             manifestFile(""),
			"[hold]/[bobby]/SHIELD SCRIPT/the zip/ld/fxmanifest.lua": manifestFile(""),
		}
		det, err := Detect(fsys, "x")
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if len(det.Resources) != 1 || det.Resources[0] != "[core]/keep" {
			t.Fatalf("resources: %+v", det.Resources)
		}
		if len(det.Skipped) != 1 || det.Skipped[0].Reason != ReasonUnreachable {
			t.Fatalf("skipped: %+v", det.Skipped)
		}
	})
	t.Run("duplicate resource names across categories", func(t *testing.T) {
		fsys := fstest.MapFS{
			"[a]/chat/fxmanifest.lua": manifestFile(""),
			"[b]/chat/fxmanifest.lua": manifestFile(""),
		}
		_, err := Detect(fsys, "x")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonDuplicateName {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unsafe resource name", func(t *testing.T) {
		fsys := fstest.MapFS{"my chat/fxmanifest.lua": manifestFile("")}
		_, err := Detect(fsys, "x")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonUnsafeName {
			t.Fatalf("got %v", err)
		}
		fsys = fstest.MapFS{".hidden/fxmanifest.lua": manifestFile("")}
		_, err = Detect(fsys, "x")
		if !errors.As(err, &de) || de.Reason != ReasonVendorOnly {
			t.Fatalf("dot dir: got %v", err)
		}
	})
	t.Run("unsafe single-resource proposal", func(t *testing.T) {
		_, err := Detect(fstest.MapFS{"fxmanifest.lua": manifestFile("")}, "my repo")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonUnsafeName {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bracket dir with manifest is skipped, and still scanned as a category", func(t *testing.T) {
		// FXServer warns "is a category, but has a resource manifest", ignores
		// the manifest, and keeps scanning the category's children.
		fsys := fstest.MapFS{
			"[maps]/fxmanifest.lua":      manifestFile(""),
			"[maps]/bank/fxmanifest.lua": manifestFile(""),
		}
		det, err := Detect(fsys, "x")
		if err != nil {
			t.Fatalf("got %v", err)
		}
		if len(det.Resources) != 1 || det.Resources[0] != "[maps]/bank" {
			t.Fatalf("resources: %+v", det.Resources)
		}
		if len(det.Skipped) != 1 || det.Skipped[0].Path != "[maps]" || det.Skipped[0].Reason != ReasonCategoryManifest {
			t.Fatalf("skipped: %+v", det.Skipped)
		}
		// Nothing but a category manifest is not a component.
		_, err = Detect(fstest.MapFS{"[maps]/fxmanifest.lua": manifestFile("")}, "x")
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonCategoryManifest {
			t.Fatalf("category only: got %v", err)
		}
	})
	t.Run("nested resource inside a resource is not scanned", func(t *testing.T) {
		fsys := fstest.MapFS{
			"a/fxmanifest.lua":        manifestFile(""),
			"a/vendor/fxmanifest.lua": manifestFile(""),
		}
		det, err := Detect(fsys, "x")
		if err != nil || len(det.Resources) != 1 {
			t.Fatalf("%v %+v", err, det)
		}
	})
}

func TestScanFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("manifest error is typed", func(t *testing.T) {
		fsys := fstest.MapFS{"a/fxmanifest.lua": {Data: []byte("fx_version 'cerulean'\ngame 'gta5'\nos.exit(1)")}}
		_, err := Scan(ctx, fsys, domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutCollection})
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonManifest || !errors.Is(err, manifest.ErrForbidden) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("single without resourceName", func(t *testing.T) {
		_, err := Scan(ctx, fstest.MapFS{"fxmanifest.lua": manifestFile("")}, domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutSingle})
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonBadDescriptor {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("single with nested", func(t *testing.T) {
		fsys := fstest.MapFS{"fxmanifest.lua": manifestFile(""), "n/fxmanifest.lua": manifestFile("")}
		_, err := Scan(ctx, fsys, domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutSingle, ResourceName: str("x")})
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonMixedShape {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("collection with root manifest", func(t *testing.T) {
		fsys := fstest.MapFS{"fxmanifest.lua": manifestFile("")}
		_, err := Scan(ctx, fsys, domain.ComponentDescriptor{SourceRoot: ".", Layout: LayoutCollection})
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonMixedShape {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad sourceRoot", func(t *testing.T) {
		for _, sr := range []string{"../x", "a//b", "missing"} {
			_, err := Scan(ctx, fstest.MapFS{"a/fxmanifest.lua": manifestFile("")}, domain.ComponentDescriptor{SourceRoot: sr, Layout: LayoutCollection})
			var de *Error
			if !errors.As(err, &de) || de.Reason != ReasonBadDescriptor {
				t.Fatalf("%s: got %v", sr, err)
			}
		}
	})
	t.Run("unknown layout", func(t *testing.T) {
		_, err := Scan(ctx, fstest.MapFS{"a/fxmanifest.lua": manifestFile("")}, domain.ComponentDescriptor{SourceRoot: ".", Layout: "weird"})
		var de *Error
		if !errors.As(err, &de) || de.Reason != ReasonBadDescriptor {
			t.Fatalf("got %v", err)
		}
	})
}

func TestValidateResourceName(t *testing.T) {
	for _, ok := range []string{"chat", "es_extended", "city-map", "qb.core", "A1"} {
		if err := ValidateResourceName(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".hidden", "[maps]", "a b", "a/b", "a\\b", "CON", "trailing.", "-lead"} {
		if err := ValidateResourceName(bad); err == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
