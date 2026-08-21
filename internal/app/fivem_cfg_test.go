package app

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var updateCfgGolden = flag.Bool("update", false, "rewrite spec/fixtures/fivem/cfg/expected/*.json")

const cfgFixtures = "../../spec/fixtures/fivem/cfg"

func TestParseServerCfgFixture(t *testing.T) {
	got, err := ParseServerCfg(filepath.Join(cfgFixtures, "server", "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.Abs(filepath.Join(cfgFixtures, "server")); filepath.Clean(got.Root) != want {
		if r, err := filepath.EvalSymlinks(want); err != nil || r != got.Root {
			t.Fatalf("root %q, want %q", got.Root, want)
		}
	}
	got.Root = "" // not part of the golden (absolute)
	goldenPath := filepath.Join(cfgFixtures, "expected", "server.json")
	if *updateCfgGolden {
		data, _ := json.MarshalIndent(struct {
			Resources []string `json:"resources"`
			Files     []string `json:"files"`
			Warnings  []string `json:"warnings"`
		}{got.Resources, got.Files, got.Warnings}, "", "  ")
		if err := os.WriteFile(goldenPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var want CfgImport
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("cfg import mismatch (-want +got):\n%s", diff)
	}
	if !got.Has("chat") || got.Has("nope") {
		t.Fatal("Has")
	}
}

func TestParseServerCfgRefusals(t *testing.T) {
	cases := []struct {
		file, reason string
	}{
		{filepath.Join("cycle", "a.cfg"), "cycle"},
		{filepath.Join("escape", "server.cfg"), "escapes-root"},
		{filepath.Join("escape", "absolute.cfg"), "absolute-path"},
		{filepath.Join("escape", "missing.cfg"), "unreadable"},
		{filepath.Join("escape", "does-not-exist.cfg"), "unreadable"},
	}
	for _, c := range cases {
		_, err := ParseServerCfg(filepath.Join(cfgFixtures, c.file))
		var ce *CfgError
		if !errors.As(err, &ce) || ce.Reason != c.reason || !errors.Is(err, ErrCfg) {
			t.Errorf("%s: got %v, want reason %s", c.file, err, c.reason)
		}
	}
}

func TestParseServerCfgDepthLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= CfgMaxExecDepth+1; i++ {
		body := "ensure r" + string(rune('a'+i%26)) + "\nexec " + "l" + itoa(i+1) + ".cfg\n"
		if err := os.WriteFile(filepath.Join(dir, "l"+itoa(i)+".cfg"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ParseServerCfg(filepath.Join(dir, "l0.cfg"))
	var ce *CfgError
	if !errors.As(err, &ce) || ce.Reason != "too-deep" {
		t.Fatalf("got %v", err)
	}
}

func TestCfgTokens(t *testing.T) {
	cases := map[string][]string{
		`ensure a b`:                     {"ensure", "a", "b"},
		`  start "x y" 'z'  # c`:         {"start", "x y", "z"},
		`# only comment`:                 nil,
		``:                               nil,
		`sv_hostname "a # b"`:            {"sv_hostname", "a # b"},
		"ensure\tchat\r":                 {"ensure", "chat"},
		`ensure "unterminated`:           {"ensure", "unterminated"},
		`ensure ""`:                      {"ensure", ""},
		`exec sub/dir/x.cfg   # comment`: {"exec", "sub/dir/x.cfg"},
	}
	for in, want := range cases {
		if diff := cmp.Diff(want, cfgTokens(in)); diff != "" {
			t.Errorf("%q: %s", in, diff)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
