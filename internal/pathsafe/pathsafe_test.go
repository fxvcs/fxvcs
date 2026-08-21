package pathsafe

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeAccepts(t *testing.T) {
	for _, p := range []string{"a", "a/b", "resources/[maps]/city", "stream/x.ytd", "[core]/chat/fxmanifest.lua", "a b/c"} {
		if got, err := Normalize(p); err != nil || got != p {
			t.Errorf("%q: got %q %v", p, got, err)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := map[string]error{
		"":               ErrEmpty,
		"/abs":           ErrAbsolute,
		"C:/x":           ErrAbsolute,
		"c:x":            ErrAbsolute,
		"a/../b":         ErrTraversal,
		"..":             ErrTraversal,
		"../x":           ErrTraversal,
		"./a":            ErrTraversal,
		"a/./b":          ErrTraversal,
		"a//b":           ErrBadSegment,
		"a/":             ErrBadSegment,
		"a\\b":           ErrBadCharacter,
		"a\x00b":         ErrBadCharacter,
		"ab:c":           ErrBadCharacter,
		"a*b":            ErrBadCharacter,
		"a\x7f":          ErrBadCharacter,
		"con":            ErrReservedName,
		"COM1.txt":       ErrReservedName,
		"x/nul/y":        ErrReservedName,
		"trailing.":      ErrReservedName,
		"trailing" + " ": ErrReservedName, // trailing space is the point
		".git/config":    ErrReservedName,
		"a/.GIT":         ErrReservedName,
		"\xff":           ErrBadCharacter,
	}
	for p, want := range cases {
		if _, err := Normalize(p); !errors.Is(err, want) {
			t.Errorf("%q: got %v want %v", p, err, want)
		}
	}
}

func TestJoinContainment(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	ok, err := Join(root, "resources/[maps]/city/fxmanifest.lua")
	if err != nil || !Within(root, ok) {
		t.Fatalf("join failed: %v", err)
	}
	for _, rel := range []string{"../etc/passwd", "a/../../x", "/etc/passwd", "a\\..\\..\\x", "..", "C:\\Windows"} {
		if _, err := Join(root, rel); err == nil {
			t.Errorf("%q escaped root", rel)
		}
	}
	if _, err := Join("relative/root", "a"); !errors.Is(err, ErrAbsolute) {
		t.Errorf("relative root accepted: %v", err)
	}
	if _, err := Join(root+string(filepath.Separator)+"..", "a"); !errors.Is(err, ErrNotNormalized) {
		t.Errorf("unclean root accepted: %v", err)
	}
}

func TestWithin(t *testing.T) {
	root := "/deploy/root"
	if runtime.GOOS == "windows" {
		root = `C:\deploy\root`
	}
	if !Within(root, filepath.Join(root, "releases", "r1")) || !Within(root, root) {
		t.Fatal("expected within")
	}
	if Within(root, filepath.Join(root, "..", "sibling")) || Within(root, root+"2") {
		t.Fatal("expected not within")
	}
}

func TestOverlaps(t *testing.T) {
	if !Overlaps("resources/[maps]", "resources/[maps]/city") || !Overlaps("a", "a") || Overlaps("resources/[maps]", "resources/[maps2]") {
		t.Fatal("overlap logic wrong")
	}
	if !IsBracketCategory("[maps]") || IsBracketCategory("maps") || IsBracketCategory("[]") {
		t.Fatal("bracket detection wrong")
	}
}
