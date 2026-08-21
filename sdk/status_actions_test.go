package sdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/format"
)

// TestStatusListsWhatEachAssetAllows is the desktop's file list.
//
// The application asks for the assets, gets back what may be done to each one,
// and renders its controls from that — without knowing the rule that decides
// whether a modified file may be thrown away. A refusal arrives with both the
// sentence to show and a code to branch on.
func TestStatusListsWhatEachAssetAllows(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := writeRepo(t, "0.1.0", 1)
	cacheDir := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...) //nolint:gosec // fixed test arguments
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+filepath.Join(dir, ".nogitconfig"),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { //nolint:gosec // test tree
			t.Fatal(err)
		}
	}

	repo, err := app.Open(ctx, dir, app.Options{Installed: format.MustParseVersion("0.1.0"), CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StorageAdd(ctx, app.StorageAddInput{Name: "primary", Type: "filesystem",
		Path: t.TempDir(), Local: true, RequiredForPublication: true}); err != nil {
		t.Fatal(err)
	}
	write(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	content := bytes.Repeat([]byte("asset payload "), 4096)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "keep.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"keep.bin", "edited.bin"} {
		if _, err := repo.Ingest(ctx, bytes.NewReader(content), p); err != nil {
			t.Fatal(err)
		}
		write(p, ing.Pointer.MustEncode())
	}
	run("add", ".")
	run("commit", "-qm", "assets")
	_ = repo.Close()

	// edited.bin now holds bytes that exist nowhere else.
	write("edited.bin", append(bytes.Clone(content), []byte(" LOCAL EDIT")...))

	c, _ := New(Options{InstalledVersion: "0.1.0", CacheDir: cacheDir})
	res, err := c.Status(ctx, StatusRequest{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]StatusEntry{}
	for _, e := range res.Entries {
		byPath[e.Path] = e
	}
	action := func(path string, want Action) AssetAction {
		t.Helper()
		for _, a := range byPath[path].Actions {
			if a.Action == want {
				return a
			}
		}
		t.Fatalf("%s carries no %q action: %+v", path, want, byPath[path].Actions)
		return AssetAction{}
	}

	// An untouched placeholder: loadable, and unloading is already satisfied.
	if a := action("keep.bin", ActionLoad); !a.Available {
		t.Fatalf("load not offered for a placeholder: %+v", a)
	}
	if a := action("keep.bin", ActionUnload); a.Available || !a.Done {
		t.Fatalf("unload should read as done, got %+v", a)
	}

	// A locally modified file: neither, with an explanation and a code.
	unload := action("edited.bin", ActionUnload)
	if unload.Available || unload.Done {
		t.Fatalf("unload offered for a modified file: %+v", unload)
	}
	if unload.Code != CodeConflict {
		t.Fatalf("refusal not classified as a conflict: %+v", unload)
	}
	if unload.Reason == "" {
		t.Fatal("a refusal the interface cannot explain to the user")
	}
	if a := action("edited.bin", ActionLoad); a.Available || a.Code != CodeConflict {
		t.Fatalf("load offered for a modified file: %+v", a)
	}

	// The refusal is the operation's refusal, not a second opinion about it:
	// asking for the unload anyway yields the same code and the same sentence.
	_, err = c.Dehydrate(ctx, DehydrateRequest{Path: dir, Selector: PathSelector{Paths: []string{"edited.bin"}}})
	if err == nil {
		t.Fatal("the operation accepted what the listing said it would refuse")
	}
	if CodeOf(err) != unload.Code || !strings.Contains(err.Error(), unload.Reason) {
		t.Fatalf("operation disagrees with the listing:\n op:      %v (%s)\n listing: %s (%s)",
			err, CodeOf(err), unload.Reason, unload.Code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "edited.bin")) //nolint:gosec // test tree
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte(" LOCAL EDIT")) {
		t.Fatal("the refused unload destroyed the only copy of the edit")
	}

	// The two affordances an interface has nothing else to derive them from.
	// Staging the edited file records something; staging the untouched
	// placeholder does not, and discarding is offered for exactly the file
	// that has something to lose.
	if a := action("edited.bin", ActionStage); !a.Available {
		t.Fatalf("stage not offered for a locally changed asset: %+v", a)
	}
	if a := action("keep.bin", ActionStage); a.Available || !a.Done {
		t.Fatalf("staging an unloaded asset should read as done, got %+v", a)
	}
	if a := action("edited.bin", ActionDiscard); !a.Available {
		t.Fatalf("discard not offered for a locally changed asset: %+v", a)
	}
	if a := action("keep.bin", ActionDiscard); a.Available || !a.Done {
		t.Fatalf("discard offered for an asset with no local changes: %+v", a)
	}

	// How much the asset changed, which is the number a diff of two
	// placeholders cannot produce.
	edited := byPath["edited.bin"]
	if edited.SizeDelta == nil {
		t.Fatal("a changed asset reports no measurable change")
	}
	if *edited.SizeDelta != int64(len(" LOCAL EDIT")) {
		t.Fatalf("change is %+d bytes, want %+d", *edited.SizeDelta, len(" LOCAL EDIT"))
	}
	if edited.WorkingSize != edited.Size+*edited.SizeDelta {
		t.Fatalf("working size %d, index size %d, delta %+d do not agree",
			edited.WorkingSize, edited.Size, *edited.SizeDelta)
	}
	if keep := byPath["keep.bin"]; keep.SizeDelta != nil {
		t.Fatalf("a placeholder reported a %d byte change", *keep.SizeDelta)
	}
}

// TestDiscardWillNotGuessWhatToThrowAway.
//
// Every other selector in this package reads empty as "all tracked paths".
// Discard destroys bytes that are in no commit and no object store, so
// inheriting that default would turn a caller's forgotten field into a wiped
// working tree. Meaning "everything" has to be typed.
func TestDiscardWillNotGuessWhatToThrowAway(t *testing.T) {
	dir := writeRepo(t, "0.1.0", 1)
	c, _ := New(Options{InstalledVersion: "0.1.0", CacheDir: t.TempDir()})
	_, err := c.Discard(context.Background(), DiscardRequest{Path: dir})
	if err == nil {
		t.Fatal("an empty selection was accepted as 'discard everything'")
	}
	if CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("refusal classified as %s: %v", CodeOf(err), err)
	}
	var se *Error
	if !errors.As(err, &se) || se.Hint == "" {
		t.Fatalf("a refusal with no way forward: %v", err)
	}
}
