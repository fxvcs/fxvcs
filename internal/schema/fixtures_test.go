package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/format"
)

var fixtureRoot = filepath.Join("..", "..", "spec", "fixtures")

// dirKinds maps a fixture directory name to the document kind it holds.
var dirKinds = map[string]string{
	"repository":               format.KindRepository,
	"resource-catalog":         format.KindResourceCatalog,
	"runtime-resource-catalog": format.KindRuntimeResourceCatalog,
	"resource-overrides":       format.KindResourceOverrides,
	"stack":                    format.KindStack,
	"release":                  format.KindRelease,
	"environment":              format.KindEnvironment,
	"target":                   format.KindTarget,
	"agent-config":             format.KindAgentConfig,
	"pointer":                  KindPointer,
	"object-manifest":          KindObjectManifest,
	"asset-index":              KindAssetIndex,
	"hydration-state-row":      KindHydrationStateRow,
	"publication-ledger":       KindPublicationLedger,
}

func fixtureFiles(t *testing.T, sub string) map[string][]string {
	t.Helper()
	root := filepath.Join(fixtureRoot, sub)
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	out := map[string][]string{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, ok := dirKinds[d.Name()]; !ok {
			t.Fatalf("fixture directory %s has no kind mapping", d.Name())
		}
		entries, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			ext := filepath.Ext(e.Name())
			if ext == ".yaml" || ext == ".json" {
				out[d.Name()] = append(out[d.Name()], filepath.Join(root, d.Name(), e.Name()))
			}
		}
		sort.Strings(out[d.Name()])
	}
	return out
}

func TestSchemasCompile(t *testing.T) {
	if err := Compile(); err != nil {
		t.Fatal(err)
	}
	for _, k := range KnownKinds() {
		if _, err := SchemaJSON(k); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestEveryKindHasFixtures(t *testing.T) {
	valid := fixtureFiles(t, "documents")
	bad := fixtureFiles(t, "malformed")
	for dir, kind := range dirKinds {
		if len(valid[dir]) < 2 {
			t.Errorf("%s (%s): want at least 2 valid fixtures, got %d", dir, kind, len(valid[dir]))
		}
		if len(bad[dir]) < 1 {
			t.Errorf("%s (%s): want at least 1 malformed fixture", dir, kind)
		}
	}
	for _, kind := range KnownKinds() {
		found := false
		for _, k := range dirKinds {
			if k == kind {
				found = true
			}
		}
		if !found {
			t.Errorf("kind %s has no fixture directory", kind)
		}
	}
}

func TestValidFixtures(t *testing.T) {
	for dir, files := range fixtureFiles(t, "documents") {
		kind := dirKinds[dir]
		for _, f := range files {
			t.Run(dir+"/"+filepath.Base(f), func(t *testing.T) {
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				// Explicit kind.
				if iss := Validate(kind, data); len(iss) > 0 {
					t.Fatalf("expected valid, got:\n%s", joinIssues(iss))
				}
				spec, _ := lookup(kind)
				if spec.envelope {
					// Auto-detected kind.
					if iss := Validate("", data); len(iss) > 0 {
						t.Fatalf("auto-detect: %s", joinIssues(iss))
					}
					// Wrong explicit kind must be rejected at the envelope.
					other := format.KindRepository
					if kind == other {
						other = format.KindTarget
					}
					iss := Validate(other, data)
					if len(iss) == 0 || !errors.Is(iss, format.ErrUnknownKind) {
						t.Fatalf("expected envelope kind mismatch, got %v", iss)
					}
				}
				v, err := Load(kind, data)
				if err != nil {
					t.Fatal(err)
				}
				if spec.newValue == nil {
					return
				}
				roundTrip(t, kind, v)
			})
		}
	}
}

// roundTrip proves yaml -> struct -> yaml -> struct and
// struct -> canonical JSON -> struct are lossless.
func roundTrip(t *testing.T, kind string, v any) {
	t.Helper()
	spec, _ := lookup(kind)
	opts := []cmp.Option{cmpopts.EquateEmpty()}

	if spec.envelope { // committed documents are YAML; low-level formats are JSON only
		y, err := yaml.Marshal(v)
		if err != nil {
			t.Fatalf("yaml marshal: %v", err)
		}
		again, err := Load(kind, y)
		if err != nil {
			t.Fatalf("re-validate marshalled yaml:\n%s\n%v", y, err)
		}
		if diff := cmp.Diff(v, again, opts...); diff != "" {
			t.Fatalf("yaml round trip mismatch (-orig +again):\n%s", diff)
		}
	}

	cj, err := canonical.Marshal(v)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	// Canonical JSON is also valid input.
	fromCanon, err := Load(kind, cj)
	if err != nil {
		t.Fatalf("validate canonical json: %v", err)
	}
	if diff := cmp.Diff(v, fromCanon, opts...); diff != "" {
		t.Fatalf("canonical round trip mismatch:\n%s", diff)
	}
	// Plain encoding/json decode of canonical bytes into a fresh struct.
	fresh := spec.newValue()
	dec := json.NewDecoder(bytes.NewReader(cj))
	dec.DisallowUnknownFields()
	if err := dec.Decode(fresh); err != nil {
		t.Fatalf("json decode canonical: %v", err)
	}
	if diff := cmp.Diff(v, fresh, opts...); diff != "" {
		t.Fatalf("json round trip mismatch:\n%s", diff)
	}
	// Canonical encoding is stable across a round trip.
	cj2, err := canonical.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cj, cj2) {
		t.Fatalf("canonical bytes changed after round trip")
	}
}

func TestMalformedFixtures(t *testing.T) {
	for dir, files := range fixtureFiles(t, "malformed") {
		kind := dirKinds[dir]
		for _, f := range files {
			t.Run(dir+"/"+filepath.Base(f), func(t *testing.T) {
				data, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				expectPath := strings.TrimSuffix(f, filepath.Ext(f)) + ".expect"
				want, err := os.ReadFile(expectPath)
				if err != nil {
					t.Fatalf("every malformed fixture needs a sibling .expect file: %v", err)
				}
				expect := strings.TrimSpace(string(want))
				if expect == "" {
					t.Fatalf("%s is empty", expectPath)
				}
				iss := Validate(kind, data)
				if len(iss) == 0 {
					t.Fatalf("expected issues, document validated")
				}
				if !strings.Contains(iss[0].Error(), expect) {
					t.Fatalf("first issue %q does not contain %q\nall issues:\n%s", iss[0].Error(), expect, joinIssues(iss))
				}
				if _, err := Load(kind, data); err == nil {
					t.Fatalf("Load must fail")
				}
			})
		}
	}
}

func TestNewerAPIVersionIsTypedAndPrecedesSchema(t *testing.T) {
	// The body is deliberately garbage: only the envelope may be inspected.
	doc := []byte("apiVersion: fxvcs.dev/v2alpha1\nkind: Repository\nspec: 42\n")
	iss := Validate("", doc)
	if len(iss) != 1 {
		t.Fatalf("want exactly one issue, got %v", iss)
	}
	if !errors.Is(iss, format.ErrNewerAPIVersion) {
		t.Fatalf("want ErrNewerAPIVersion, got %v", iss)
	}
	if iss[0].Path != "/apiVersion" {
		t.Fatalf("path %q", iss[0].Path)
	}
	_, err := Load(format.KindRepository, doc)
	if !errors.Is(err, format.ErrNewerAPIVersion) {
		t.Fatalf("Load: %v", err)
	}
	// Unknown group is the other sentinel.
	iss = Validate("", []byte("apiVersion: other.dev/v1\nkind: Repository\n"))
	if !errors.Is(iss, format.ErrUnknownAPIVersion) {
		t.Fatalf("got %v", iss)
	}
}

func TestValidateRejectsNonMapping(t *testing.T) {
	if iss := Validate("", []byte("- a\n- b\n")); len(iss) == 0 || !strings.Contains(iss[0].Message, "mapping") {
		t.Fatalf("got %v", iss)
	}
	if iss := Validate("", nil); len(iss) == 0 {
		t.Fatal("empty input must fail")
	}
	if iss := Validate("Nope", []byte("a: 1\n")); len(iss) == 0 || !errors.Is(iss, format.ErrUnknownKind) {
		t.Fatalf("got %v", iss)
	}
}

func TestSchemaOnlyKindsRequireExplicitKind(t *testing.T) {
	data := []byte(`{"schemaVersion":1,"objects":[]}`)
	if iss := Validate(KindPublicationLedger, data); len(iss) > 0 {
		t.Fatal(iss)
	}
	v, err := Load(KindPublicationLedger, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(map[string]any); !ok {
		t.Fatalf("schema-only kinds return the generic map, got %T", v)
	}
	if iss := Validate("", data); len(iss) == 0 || iss[0].Path != "/kind" {
		t.Fatalf("got %v", iss)
	}
}

func joinIssues(iss Issues) string {
	var b strings.Builder
	for _, i := range iss {
		b.WriteString("  ")
		b.WriteString(i.Error())
		b.WriteByte('\n')
	}
	return b.String()
}
