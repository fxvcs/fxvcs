package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

// A literal directory argument selects every managed entry beneath it and is
// not itself reported as missing (nor escalated to an operation error).
func TestManagedEntriesDirectoryArgument(t *testing.T) {
	gittest.RequireGit(t)
	r := gittest.InitRepo(t)
	r.WriteFile(".fxvcs/repository.yaml", []byte(repoYAML))
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	// Stage pointers directly (no filter configured in this repo) so the
	// index contains managed entries under res/stream/.
	repo := openTest(t, r, t.TempDir())
	ctx := context.Background()
	for _, name := range []string{"res/stream/a.bin", "res/stream/b.bin", "other/c.bin"} {
		ing, err := repo.Ingest(ctx, bytes.NewReader([]byte(name)), name)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.MkdirAll(filepath.Join(r.Root, filepath.Dir(name)), 0o755)
		r.WriteFile(name, ing.Pointer.MustEncode())
	}
	r.Git("add", "-A")
	entries, missing, err := repo.ManagedEntries(ctx, Selection{Paths: []string{"res", "nope", "res/stream/a.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "res/stream/a.bin" || entries[1].Path != "res/stream/b.bin" {
		t.Fatalf("entries %+v", entries)
	}
	if len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("missing %v", missing)
	}
}
