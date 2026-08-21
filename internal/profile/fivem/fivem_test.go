package fivem

import (
	"bytes"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

func TestDefaultBinaryPatternsExcludeSource(t *testing.T) {
	for _, p := range DefaultBinaryPatterns {
		if !strings.HasPrefix(p, "*.") {
			t.Errorf("pattern %q must be an extension glob", p)
		}
		switch strings.TrimPrefix(p, "*.") {
		case "lua", "js", "ts", "json", "cfg", "meta", "xml", "html", "css", "md", "txt":
			t.Errorf("source format %q must not be a default binary pattern", p)
		}
	}
	seen := map[string]bool{}
	for _, p := range DefaultBinaryPatterns {
		if seen[p] {
			t.Errorf("duplicate %q", p)
		}
		seen[p] = true
	}
	for _, must := range []string{"*.ytd", "*.ydr", "*.yft", "*.ybn", "*.ymap", "*.ytyp", "*.awc", "*.rpf"} {
		if !seen[must] {
			t.Errorf("missing %q", must)
		}
	}
}

func TestAttributesBlock(t *testing.T) {
	got := string(AttributesBlock([]string{"*.ytd", " *.ydr ", "*.ytd", "", "my dir/*.ymap"}))
	want := BlockStart + "\n" +
		"*.ytd filter=fxvcs diff=fxvcs merge=fxvcs -text\n" +
		"*.ydr filter=fxvcs diff=fxvcs merge=fxvcs -text\n" +
		"\"my dir/*.ymap\" filter=fxvcs diff=fxvcs merge=fxvcs -text\n" +
		BlockEnd + "\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if b := string(AttributesBlock(nil)); b != BlockStart+"\n"+BlockEnd+"\n" {
		t.Fatalf("empty block: %q", b)
	}
}

func TestMergeAttributes(t *testing.T) {
	block := AttributesBlock([]string{"*.ytd"})
	block2 := AttributesBlock([]string{"*.ytd", "*.ydr"})

	t.Run("append to file without trailing newline", func(t *testing.T) {
		got := MergeAttributes([]byte("*.png binary"), block)
		if string(got) != "*.png binary\n"+string(block) {
			t.Fatalf("%q", got)
		}
	})
	t.Run("append to empty", func(t *testing.T) {
		if got := MergeAttributes(nil, block); !bytes.Equal(got, block) {
			t.Fatalf("%q", got)
		}
	})
	t.Run("replace preserves surrounding bytes", func(t *testing.T) {
		existing := []byte("# user header\r\n*.png binary\r\n" + string(block) + "*.md text\n\n# tail without newline")
		got := MergeAttributes(existing, block2)
		want := "# user header\r\n*.png binary\r\n" + string(block2) + "*.md text\n\n# tail without newline"
		if string(got) != want {
			t.Fatalf("got:\n%q\nwant:\n%q", got, want)
		}
		// Idempotent.
		if again := MergeAttributes(got, block2); !bytes.Equal(again, got) {
			t.Fatal("not idempotent")
		}
	})
	t.Run("crlf terminated markers", func(t *testing.T) {
		existing := []byte("a\r\n" + BlockStart + "\r\nold\r\n" + BlockEnd + "\r\nb\r\n")
		got := MergeAttributes(existing, block)
		if string(got) != "a\r\n"+string(block)+"b\r\n" {
			t.Fatalf("%q", got)
		}
	})
	t.Run("missing end marker extends to EOF", func(t *testing.T) {
		existing := []byte("a\n" + BlockStart + "\nold\n")
		got := MergeAttributes(existing, block)
		if string(got) != "a\n"+string(block) {
			t.Fatalf("%q", got)
		}
	})
	t.Run("marker text inside a user line is not a marker", func(t *testing.T) {
		existing := []byte("# note: " + BlockStart + " is used by fxvcs\n*.png binary\n")
		got := MergeAttributes(existing, block)
		if string(got) != string(existing)+string(block) {
			t.Fatalf("%q", got)
		}
	})
	t.Run("empty block removes the section", func(t *testing.T) {
		existing := []byte("a\n" + string(block) + "b\n")
		if got := MergeAttributes(existing, nil); string(got) != "a\nb\n" {
			t.Fatalf("%q", got)
		}
	})
}

func TestClassify(t *testing.T) {
	cat := &domain.ResourceCatalog{Spec: domain.ResourceCatalogSpec{Resources: []domain.CatalogResource{
		{Path: "[maps]/city-map", Name: "city-map", Dependencies: []string{"mapmanager"}},
		{Path: "[maps]/[interiors]/bank", Name: "bank", Dependencies: []string{"city-map"}},
		{Path: "mapmanager-fork", Name: "mapmanager-fork", Provides: []string{"mapmanager"}},
		{Path: "chat", Name: "chat"},
	}}}
	desc := domain.ComponentDescriptor{SourceRoot: ".", Layout: discovery.LayoutCollection}
	got := Classify([]string{
		"chat/html/index.html",
		"chat/fxmanifest.lua",
		"[maps]/city-map/stream/city.ytd",       // depended on by bank
		"[maps]/[interiors]/bank/stream/b.ymap", // leaf
		"mapmanager-fork/mapmanager_server.lua", // depended on via provide
		"server.cfg",
		"[maps]/README.md",
		"./chat/html/index.html", // duplicate after normalization
	}, cat, desc)
	want := map[string]ChangeKind{
		"chat/html/index.html":                  ChangeHotReloadSafe,
		"chat/fxmanifest.lua":                   ChangeRestartRequired,
		"[maps]/city-map/stream/city.ytd":       ChangeRestartRequired,
		"[maps]/[interiors]/bank/stream/b.ymap": ChangeHotReloadSafe,
		"mapmanager-fork/mapmanager_server.lua": ChangeRestartRequired,
		"server.cfg":                            ChangeGlobal,
		"[maps]/README.md":                      ChangeGlobal,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		if want[c.Path] != c.Kind {
			t.Errorf("%s: got %s, want %s (%s)", c.Path, c.Kind, want[c.Path], c.Reason)
		}
		if c.Kind != ChangeGlobal && c.Resource == "" {
			t.Errorf("%s: missing resource", c.Path)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Path >= got[i].Path {
			t.Fatal("not sorted")
		}
	}
	// Single-resource layout: everything belongs to the resource.
	name := "inventory"
	single := Classify([]string{"fxmanifest.lua", "client/main.lua"}, nil, domain.ComponentDescriptor{SourceRoot: ".", Layout: discovery.LayoutSingle, ResourceName: &name})
	if single[0].Kind != ChangeHotReloadSafe || single[1].Kind != ChangeRestartRequired || single[0].Resource != "inventory" {
		t.Fatalf("%+v", single)
	}
}
