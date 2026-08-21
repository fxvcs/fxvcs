package schema

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Golden fixtures pin the exact bytes produced by the pointer, canonical-JSON
// and identity code. Run `go test ./internal/schema -update` to regenerate them
// after a deliberate format change; any accidental drift fails here.
var update = flag.Bool("update", false, "rewrite golden fixture outputs")

var goldenRoot = filepath.Join(fixtureRoot, "golden")

func goldenInputs(t *testing.T, sub, suffix string) []string {
	t.Helper()
	dir := filepath.Join(goldenRoot, sub)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s inputs under %s", suffix, dir)
	}
	return out
}

// expectGolden compares got with the file at path, or rewrites it with -update.
func expectGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden output %s (run with -update): %v", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("golden mismatch for %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func TestGoldenPointer(t *testing.T) {
	for _, in := range goldenInputs(t, "pointer-v1", ".json") {
		t.Run(filepath.Base(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			if iss := Validate(KindPointer, data); len(iss) > 0 {
				t.Fatalf("decoded-field fixture must validate: %v", iss)
			}
			var pd PointerDocument
			if err := json.Unmarshal(data, &pd); err != nil {
				t.Fatal(err)
			}
			p := pointer.Pointer{StorageDomainID: pd.Domain, Encoding: pd.Encoding, ManifestDigest: pd.Manifest, OID: pd.OID, Size: pd.Size}
			enc, err := p.Encode()
			if err != nil {
				t.Fatal(err)
			}
			out := strings.TrimSuffix(in, ".json") + ".pointer"
			expectGolden(t, out, enc)

			// The committed bytes must decode back to the same fields and re-encode identically.
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := pointer.Decode(raw)
			if err != nil {
				t.Fatalf("decode golden pointer: %v", err)
			}
			if dec != p {
				t.Fatalf("decoded %+v != %+v", dec, p)
			}
			if !bytes.Equal(dec.MustEncode(), raw) {
				t.Fatal("pointer bytes are not a fixed point of decode/encode")
			}
			if len(raw) > pointer.MaxSize {
				t.Fatalf("pointer exceeds MaxSize")
			}
			// Digest of the exact bytes is what the asset index records.
			expectGolden(t, strings.TrimSuffix(in, ".json")+".digest", []byte(canonical.DigestBytes(raw)+"\n"))
		})
	}
}

func TestGoldenObjectManifest(t *testing.T) {
	for _, in := range goldenInputs(t, "object-manifest", ".json") {
		if strings.HasSuffix(in, ".canonical.json") {
			continue
		}
		t.Run(filepath.Base(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			m, err := LoadObjectManifest(data)
			if err != nil {
				t.Fatal(err)
			}
			cj, err := canonical.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := integrity.ManifestDigest(*m)
			if err != nil {
				t.Fatal(err)
			}
			if digest != canonical.DigestBytes(cj) {
				t.Fatalf("ManifestDigest %s != digest of canonical bytes %s", digest, canonical.DigestBytes(cj))
			}
			base := strings.TrimSuffix(in, ".json")
			expectGolden(t, base+".canonical.json", cj)
			expectGolden(t, base+".digest", []byte(digest+"\n"))
			// Canonical bytes are themselves a valid manifest document.
			if iss := Validate(KindObjectManifest, cj); len(iss) > 0 {
				t.Fatal(iss)
			}
		})
	}
}

func TestGoldenAssetIndex(t *testing.T) {
	for _, in := range goldenInputs(t, "asset-index", ".json") {
		if strings.HasSuffix(in, ".canonical.json") {
			continue
		}
		t.Run(filepath.Base(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			idx, err := LoadAssetIndex(data)
			if err != nil {
				t.Fatal(err)
			}
			cj, err := canonical.Marshal(idx)
			if err != nil {
				t.Fatal(err)
			}
			root, err := integrity.AssetRoot(*idx)
			if err != nil {
				t.Fatal(err)
			}
			if root != canonical.DigestBytes(cj) {
				t.Fatalf("AssetRoot %s != digest of canonical bytes %s", root, canonical.DigestBytes(cj))
			}
			base := strings.TrimSuffix(in, ".json")
			expectGolden(t, base+".canonical.json", cj)
			expectGolden(t, base+".assetroot", []byte(root+"\n"))
			if len(idx.Entries) == 0 {
				return
			}
			// Sorting an already sorted index is a no-op; a shuffled copy sorts back.
			shuffled := *idx
			shuffled.Entries = append([]domain.AssetIndexEntry(nil), idx.Entries...)
			for i, j := 0, len(shuffled.Entries)-1; i < j; i, j = i+1, j-1 {
				shuffled.Entries[i], shuffled.Entries[j] = shuffled.Entries[j], shuffled.Entries[i]
			}
			integrity.SortAssetIndex(&shuffled)
			root2, err := integrity.AssetRoot(shuffled)
			if err != nil || root2 != root {
				t.Fatalf("sorted copy root %s (%v) != %s", root2, err, root)
			}
		})
	}
}

func TestGoldenRelease(t *testing.T) {
	for _, in := range goldenInputs(t, "release", ".yaml") {
		t.Run(filepath.Base(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			rel, err := LoadRelease(data)
			if err != nil {
				t.Fatal(err)
			}
			ssd, err := integrity.SourceSetDigest(rel.Spec)
			if err != nil {
				t.Fatal(err)
			}
			if ssd != rel.Spec.SourceSetDigest {
				t.Fatalf("fixture sourceSetDigest %s != computed %s", rel.Spec.SourceSetDigest, ssd)
			}
			rd, err := integrity.ReleaseDigest(*rel)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSuffix(in, ".yaml")
			expectGolden(t, base+".sourcesetdigest", []byte(ssd+"\n"))
			expectGolden(t, base+".releasedigest", []byte(rd+"\n"))

			// Semantic identity: reformatting the YAML must not change either digest.
			reformatted := bytes.ReplaceAll(data, []byte("\n"), []byte("   \n"))
			rel2, err := LoadRelease(reformatted)
			if err != nil {
				t.Fatalf("trailing whitespace variant: %v", err)
			}
			rd2, _ := integrity.ReleaseDigest(*rel2)
			if rd2 != rd {
				t.Fatalf("release digest depends on YAML presentation")
			}
			// Signatures are excluded from the release digest.
			unsigned := *rel
			unsigned.Spec.Signature = nil
			rd3, _ := integrity.ReleaseDigest(unsigned)
			if rd3 != rd {
				t.Fatalf("release digest must exclude signature")
			}
			// Display names are excluded from sourceSetDigest.
			renamed := rel.Spec
			renamed.Components = append([]domain.ReleaseComponent(nil), rel.Spec.Components...)
			renamed.Components[0].RepositoryName = "renamed"
			ssd2, _ := integrity.SourceSetDigest(renamed)
			if ssd2 != ssd {
				t.Fatalf("sourceSetDigest must exclude display names")
			}
		})
	}
	// The two committed variants share sources: identical sourceSetDigest,
	// different releaseDigest.
	full, _ := os.ReadFile(filepath.Join(goldenRoot, "release", "full.sourcesetdigest"))
	variant, _ := os.ReadFile(filepath.Join(goldenRoot, "release", "variant.sourcesetdigest"))
	if len(full) == 0 || !bytes.Equal(full, variant) {
		t.Fatalf("variant fixtures should share sourceSetDigest: %q vs %q", full, variant)
	}
	fullR, _ := os.ReadFile(filepath.Join(goldenRoot, "release", "full.releasedigest"))
	variantR, _ := os.ReadFile(filepath.Join(goldenRoot, "release", "variant.releasedigest"))
	if len(fullR) == 0 || bytes.Equal(fullR, variantR) {
		t.Fatalf("variant fixtures must have distinct release digests")
	}
}

func TestGoldenCanonicalJSON(t *testing.T) {
	for _, in := range goldenInputs(t, "canonical-json", ".input.json") {
		t.Run(filepath.Base(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			cj, err := canonical.Recanonicalize(data)
			if err != nil {
				t.Fatal(err)
			}
			// Idempotent.
			cj2, err := canonical.Recanonicalize(cj)
			if err != nil || !bytes.Equal(cj, cj2) {
				t.Fatalf("recanonicalize not idempotent: %v", err)
			}
			base := strings.TrimSuffix(in, ".input.json")
			expectGolden(t, base+".canonical.json", cj)
			expectGolden(t, base+".digest", []byte(canonical.DigestBytes(cj)+"\n"))
		})
	}
}
