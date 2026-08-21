package filter

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/pointer"
)

var update = flag.Bool("update", false, "rewrite spec/fixtures/git-integration expectations")

const fixtureDir = "../../spec/fixtures/git-integration"

var installedTest = format.MustParseVersion("0.0.0-dev")

// Fixture pointers: deterministic digests derived from small known contents.
func fixturePointer(t *testing.T, content string) []byte {
	t.Helper()
	oid := sha256Digest([]byte(content))
	p, err := WholePointer(testDomain, oid, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	return p.MustEncode()
}

func checkFixture(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(fixtureDir, name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v (run with -update to create)", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("fixture %s mismatch\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestMergeSemanticsAndFixtures(t *testing.T) {
	base := fixturePointer(t, "asset version 1")
	ours := fixturePointer(t, "asset version 2 (ours)")
	theirs := fixturePointer(t, "asset version 3 (theirs)")
	checkFixture(t, "pointer-base.txt", base)
	checkFixture(t, "pointer-ours.txt", ours)
	checkFixture(t, "pointer-theirs.txt", theirs)

	cases := []struct {
		name          string
		b, o, th      []byte
		wantOut       []byte
		wantOutcome   MergeOutcome
		wantErr       error
		fixtureOutput string
	}{
		{"identical", base, ours, ours, ours, MergeClean, nil, ""},
		{"only-theirs-changed", base, base, theirs, theirs, MergeClean, nil, "merge-take-theirs.expected"},
		{"only-ours-changed", base, ours, base, ours, MergeClean, nil, "merge-take-ours.expected"},
		{"both-changed", base, ours, theirs, nil, MergeConflict, nil, "merge-conflict.expected"},
		{"both-added-no-base", nil, ours, theirs, nil, MergeConflict, nil, "merge-conflict-no-base.expected"},
		{"base-not-pointer", []byte("raw historical blob"), ours, theirs, nil, MergeConflict, nil, ""},
		{"ours-not-pointer", base, []byte("raw"), theirs, nil, MergeConflict, ErrNotPointerInput, ""},
		{"theirs-not-pointer", base, ours, []byte("raw"), nil, MergeConflict, ErrNotPointerInput, ""},
		{"identical-non-pointer", base, []byte("raw"), []byte("raw"), nil, MergeConflict, ErrNotPointerInput, ""},
		{"unsupported-version", base, []byte("version https://fxvcs.dev/pointer/v9\n"), theirs, nil, MergeConflict, ErrUnsupportedPointer, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, outcome, err := Merge(tc.b, tc.o, tc.th, installedTest)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome %v want %v", outcome, tc.wantOutcome)
			}
			if tc.wantOutcome == MergeConflict {
				want := ConflictOurs + string(tc.o) + ConflictSep + string(tc.th) + ConflictTheirs
				if string(out) != want {
					t.Fatalf("conflict layout:\n%s", out)
				}
				// Both candidates are recoverable verbatim.
				body := strings.TrimPrefix(string(out), ConflictOurs)
				o, th, _ := strings.Cut(body, ConflictSep)
				th = strings.TrimSuffix(th, ConflictTheirs)
				if _, err := pointer.Decode([]byte(o)); err != nil {
					t.Fatalf("ours candidate not decodable: %v", err)
				}
				if _, err := pointer.Decode([]byte(th)); err != nil {
					t.Fatalf("theirs candidate not decodable: %v", err)
				}
			} else if !bytes.Equal(out, tc.wantOut) {
				t.Fatalf("result:\n%s\nwant:\n%s", out, tc.wantOut)
			}
			if tc.fixtureOutput != "" {
				checkFixture(t, tc.fixtureOutput, out)
			}
		})
	}
}

func TestRunMergeDriverFiles(t *testing.T) {
	dir := t.TempDir()
	base := fixturePointer(t, "asset version 1")
	ours := fixturePointer(t, "asset version 2 (ours)")
	theirs := fixturePointer(t, "asset version 3 (theirs)")
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o, a, b := write("O", base), write("A", ours), write("B", theirs)
	res, err := RunMergeDriver(o, a, b, "x.bin", installedTest)
	if err != nil || res.Outcome != MergeConflict {
		t.Fatalf("%+v %v", res, err)
	}
	got, _ := os.ReadFile(a)
	if !bytes.HasPrefix(got, []byte(ConflictOurs)) {
		t.Fatalf("%%A not rewritten with conflict layout: %q", got)
	}
	// Non-pointer ours: %A untouched, error.
	a2 := write("A2", []byte("raw"))
	if _, err := RunMergeDriver(o, a2, b, "x.bin", installedTest); !errors.Is(err, ErrNotPointerInput) {
		t.Fatalf("expected ErrNotPointerInput, got %v", err)
	}
	if got, _ := os.ReadFile(a2); string(got) != "raw" {
		t.Fatalf("%%A was modified on error: %q", got)
	}
	// Clean take-theirs writes %A.
	a3 := write("A3", base)
	res, err = RunMergeDriver(o, a3, b, "x.bin", installedTest)
	if err != nil || res.Outcome != MergeClean {
		t.Fatalf("%+v %v", res, err)
	}
	if got, _ := os.ReadFile(a3); !bytes.Equal(got, theirs) {
		t.Fatalf("take-theirs not written")
	}
}

func TestDiffDriverRenderAndFixture(t *testing.T) {
	dir := t.TempDir()
	oldP := fixturePointer(t, "asset version 1")
	newP := fixturePointer(t, "asset version 2 (ours)")
	oldF := filepath.Join(dir, "old")
	newF := filepath.Join(dir, "new")
	os.WriteFile(oldF, oldP, 0o644)
	os.WriteFile(newF, newP, 0o644)
	var out bytes.Buffer
	args := []string{"assets/a.bin", oldF, "1111111111111111111111111111111111111111", "100644", newF, "2222222222222222222222222222222222222222", "100644"}
	if err := RunDiffDriver(args, installedTest, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), dir) {
		t.Fatal("diff output leaks host paths")
	}
	checkFixture(t, "diff-pointer-vs-pointer.expected", out.Bytes())

	// Added file: old side is /dev/null with zero hex.
	out.Reset()
	args = []string{"assets/a.bin", "/dev/null", strings.Repeat("0", 40), ".", newF, "2222222222222222222222222222222222222222", "100644"}
	if err := RunDiffDriver(args, installedTest, &out); err != nil {
		t.Fatal(err)
	}
	checkFixture(t, "diff-added.expected", out.Bytes())

	// Hydrated working file vs its own pointer: content unchanged.
	hyd := filepath.Join(dir, "hydrated")
	os.WriteFile(hyd, []byte("asset version 2 (ours)"), 0o644)
	out.Reset()
	args = []string{"assets/a.bin", newF, "2222222222222222222222222222222222222222", "100644", hyd, strings.Repeat("0", 40), "100644"}
	// note: worktree side hex is zeros in git's convention but the file exists;
	// ReadSide must therefore not treat a zero hex with a real path as absent
	// unless mode is "." — check summary says unchanged.
	if err := RunDiffDriver(args, installedTest, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "content unchanged") || !strings.Contains(out.String(), "file (not a pointer)") {
		t.Fatalf("hydrated comparison:\n%s", out.String())
	}
	checkFixture(t, "diff-pointer-vs-hydrated.expected", out.Bytes())

	// Unsupported pointer version fails closed.
	bad := filepath.Join(dir, "bad")
	os.WriteFile(bad, []byte("version https://fxvcs.dev/pointer/v7\n"), 0o644)
	args = []string{"a.bin", oldF, "1", "100644", bad, "2", "100644"}
	if err := RunDiffDriver(args, installedTest, &out); !errors.Is(err, ErrUnsupportedPointer) {
		t.Fatalf("expected ErrUnsupportedPointer, got %v", err)
	}
	if err := RunDiffDriver([]string{"a.bin"}, installedTest, &out); err != nil {
		t.Fatal(err)
	}
}

func TestLauncherPaths(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		goos string
		env  map[string]string
		home string
		want string
	}{
		{"linux", map[string]string{}, "/home/dev", "/home/dev/.local/share/fxvcs/bin/fxvcs"},
		{"linux", map[string]string{"XDG_DATA_HOME": "/data"}, "/home/dev", "/data/fxvcs/bin/fxvcs"},
		{"darwin", map[string]string{}, "/Users/dev", "/Users/dev/Library/Application Support/fxvcs/bin/fxvcs"},
		{"darwin", map[string]string{"XDG_DATA_HOME": "/data"}, "/Users/dev", "/Users/dev/Library/Application Support/fxvcs/bin/fxvcs"},
		{"windows", map[string]string{"LOCALAPPDATA": `C:\Users\dev\AppData\Local`}, `C:\Users\dev`, `C:\Users\dev\AppData\Local\fxvcs\bin\fxvcs.exe`},
		{"windows", map[string]string{}, `C:\Users\dev`, `C:\Users\dev\AppData\Local\fxvcs\bin\fxvcs.exe`},
	}
	for _, tc := range cases {
		got, err := LauncherPath(tc.goos, env(tc.env), tc.home)
		if err != nil || got != tc.want {
			t.Errorf("%s %v: got %q %v want %q", tc.goos, tc.env, got, err, tc.want)
		}
	}
	if _, err := LauncherPath("linux", env(nil), ""); err == nil {
		t.Error("expected error without home")
	}
	if got, ok, err := LegacyLauncherPath("darwin", env(map[string]string{}), "/Users/dev"); err != nil || !ok || got != "/Users/dev/.local/share/fxvcs/bin/fxvcs" {
		t.Errorf("legacy macOS path: %q %v %v", got, ok, err)
	}
	if got, ok, err := LegacyLauncherPath("darwin", env(map[string]string{"XDG_DATA_HOME": "/data"}), "/Users/dev"); err != nil || !ok || got != "/data/fxvcs/bin/fxvcs" {
		t.Errorf("legacy XDG macOS path: %q %v %v", got, ok, err)
	}
	if _, ok, err := LegacyLauncherPath("linux", env(nil), "/home/dev"); err != nil || ok {
		t.Errorf("linux unexpectedly has legacy path: %v %v", ok, err)
	}
	if p, err := DefaultLauncherPath(); err != nil || !strings.HasSuffix(filepath.ToSlash(p), "/fxvcs/bin/fxvcs") && !strings.HasSuffix(filepath.ToSlash(p), "/fxvcs/bin/fxvcs.exe") {
		t.Errorf("DefaultLauncherPath %q %v", p, err)
	}
}

func TestQuoteAndConfigEntries(t *testing.T) {
	cases := []struct{ in, goos, want string }{
		{"/usr/local/bin/fxvcs", "linux", "/usr/local/bin/fxvcs"},
		{"/Users/dev name/.local/share/fxvcs/bin/fxvcs", "darwin", `"/Users/dev name/.local/share/fxvcs/bin/fxvcs"`},
		{`C:\Users\dev\AppData\Local\fxvcs\bin\fxvcs.exe`, "windows", "C:/Users/dev/AppData/Local/fxvcs/bin/fxvcs.exe"},
		{`C:\Users\dev name\AppData\Local\fxvcs\bin\fxvcs.exe`, "windows", `"C:/Users/dev name/AppData/Local/fxvcs/bin/fxvcs.exe"`},
		{`/opt/it's$/fxvcs`, "linux", `"/opt/it's\$/fxvcs"`},
	}
	for _, tc := range cases {
		if got := QuoteCommandPath(tc.in, tc.goos); got != tc.want {
			t.Errorf("QuoteCommandPath(%q,%s) = %q want %q", tc.in, tc.goos, got, tc.want)
		}
	}
	entries := gitConfigEntries(`C:\fx dir\fxvcs.exe`, "windows")
	want := map[string]string{
		"filter.fxvcs.process":  `"C:/fx dir/fxvcs.exe" filter-process`,
		"filter.fxvcs.required": "true",
		"diff.fxvcs.command":    `"C:/fx dir/fxvcs.exe" diff-driver`,
		"merge.fxvcs.driver":    `"C:/fx dir/fxvcs.exe" merge-driver %O %A %B %P`,
		"merge.fxvcs.name":      "fxvcs pointer merge",
		"merge.fxvcs.recursive": "binary",
	}
	if len(entries) != len(want) {
		t.Fatalf("entries %v", entries)
	}
	for _, e := range entries {
		if want[e.Key] != e.Value {
			t.Errorf("%s = %q want %q", e.Key, e.Value, want[e.Key])
		}
	}
}

func TestInstallAndHook(t *testing.T) {
	r := gittest.InitRepo(t)
	ctx := context.Background()
	launcher := filepath.Join(t.TempDir(), "with space", "fxvcs")
	if err := Install(ctx, r.Client, launcher); err != nil {
		t.Fatal(err)
	}
	for _, e := range GitConfigEntries(launcher) {
		v, ok, err := r.Client.ConfigGet(ctx, e.Key)
		if err != nil || !ok || v != e.Value {
			t.Fatalf("%s = %q (%v %v) want %q", e.Key, v, ok, err, e.Value)
		}
	}
	if err := Install(ctx, r.Client, "relative/fxvcs"); err == nil {
		t.Fatal("relative launcher accepted")
	}
	res, err := InstallPrePushHook(ctx, r.Client, launcher)
	if err != nil || res.Updated {
		t.Fatalf("%+v %v", res, err)
	}
	script, _ := os.ReadFile(res.Path)
	if !strings.Contains(string(script), "pre-push \"$@\"") || !strings.Contains(string(script), HookMarker) {
		t.Fatalf("hook script:\n%s", script)
	}
	// Refresh of a managed hook is allowed.
	res, err = InstallPrePushHook(ctx, r.Client, launcher)
	if err != nil || !res.Updated {
		t.Fatalf("refresh %+v %v", res, err)
	}
	// A user hook is never overwritten.
	os.WriteFile(res.Path, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if _, err := InstallPrePushHook(ctx, r.Client, launcher); !errors.Is(err, ErrHookExists) {
		t.Fatalf("user hook overwritten: %v", err)
	}
	if got, _ := os.ReadFile(res.Path); string(got) != "#!/bin/sh\necho mine\n" {
		t.Fatal("user hook content changed")
	}
	// core.hooksPath is honored.
	custom := t.TempDir()
	r.Git("config", "core.hooksPath", custom)
	res, err = InstallPrePushHook(ctx, r.Client, launcher)
	if err != nil || filepath.Dir(res.Path) != filepath.Clean(custom) {
		t.Fatalf("hooksPath not honored: %+v %v", res, err)
	}
	if err := Uninstall(ctx, r.Client); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.Client.ConfigGet(ctx, "filter.fxvcs.process"); ok {
		t.Fatal("uninstall left config")
	}
}
