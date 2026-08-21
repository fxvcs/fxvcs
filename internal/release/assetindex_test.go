package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
	"fxvcs.dev/fxvcs/internal/integrity"
)

const testDomain = "domain_assets_01"

func pointerFor(t *testing.T, body string) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	p, err := filter.WholePointer(testDomain, "sha256:"+hex.EncodeToString(sum[:]), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pinnedTree commits files into a repository and returns a Tree over the
// resulting commit, the way release creation reads a component.
func pinnedTree(t *testing.T, files map[string][]byte) *gitops.Tree {
	t.Helper()
	gittest.RequireGit(t)
	work := gittest.InitRepo(t)
	for path, data := range files {
		work.WriteFile(path, data)
	}
	work.WriteFile("fxvcs.yaml", []byte("apiVersion: fxvcs.dev/v1alpha1\nkind: Stack\nmetadata:\n  stackID: stack_t\n  name: t\nspec:\n  profile: fivem\n  minimumFXVCSVersion: 0.0.0-dev\n  components: []\n"))
	commit := work.Commit("init")

	// A coordination repository reads component history out of a mirror; here
	// the repository under test is its own mirror.
	co, err := gitops.OpenAt(context.Background(), work.Root, gitops.Options{Installed: format.MustParseVersion("0.0.0-dev")})
	if err != nil {
		t.Fatal(err)
	}
	co.Git = co.Git.WithEnv(work.Env)
	mirror, err := co.MirrorPath("self")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := co.Git.Run(context.Background(), "clone", "--mirror", "--quiet", work.Root, mirror); err != nil {
		t.Fatal(err)
	}
	tree, err := co.Tree(context.Background(), "self", strings.TrimSpace(commit))
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestAssetIndexContainsExactlyThePlaceholders(t *testing.T) {
	ctx := context.Background()
	tree := pinnedTree(t, map[string][]byte{
		"assets/city.ytd":  pointerFor(t, "city"),
		"assets/bank.ymap": pointerFor(t, "bank"),
		"src/main.lua":     []byte("-- ordinary source, stored in Git itself\n"),
		"docs/readme.md":   []byte("# readme\n"),
		"assets/notes.txt": []byte("a small text file that is not a placeholder\n"),
	})

	idx, entries, err := AssetIndex(ctx, tree, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "assets/bank.ymap,assets/city.ytd" {
		t.Fatalf("index contains %v", paths)
	}
	if idx.StorageDomainID != testDomain || idx.FormatVersion != format.AssetIndexVersion {
		t.Fatalf("%+v", idx)
	}
	for _, e := range entries {
		if e.Mode != "100644" || !strings.HasPrefix(e.PointerDigest, "sha256:") {
			t.Fatalf("%+v", e)
		}
	}
	// The root is the digest of the canonical index and is stable.
	root, _, err := AssetRoot(ctx, tree, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	want, err := integrity.AssetRoot(idx)
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("%s != %s", root, want)
	}
	for range 3 {
		again, _, err := AssetRoot(ctx, tree, testDomain)
		if err != nil || again != root {
			t.Fatalf("asset root is not deterministic: %s %v", again, err)
		}
	}
}

func TestAssetIndexOfATreeWithoutAssetsIsStable(t *testing.T) {
	tree := pinnedTree(t, map[string][]byte{"src/main.lua": []byte("-- nothing tracked\n")})
	root, idx, err := AssetRoot(context.Background(), tree, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 0 {
		t.Fatalf("%+v", idx.Entries)
	}
	empty, err := integrity.AssetRoot(domain.AssetIndex{
		FormatVersion: format.AssetIndexVersion, StorageDomainID: testDomain, Entries: []domain.AssetIndexEntry{}})
	if err != nil {
		t.Fatal(err)
	}
	if root != empty {
		t.Fatalf("an empty tree does not have the canonical empty identity: %s != %s", root, empty)
	}
}

func TestAssetIndexRefusesAPlaceholderFromAnotherStorageDomain(t *testing.T) {
	tree := pinnedTree(t, map[string][]byte{"assets/city.ytd": pointerFor(t, "city")})
	_, _, err := AssetIndex(context.Background(), tree, "domain_someone_else_01")
	if err == nil {
		t.Fatal("a placeholder from another access boundary was accepted")
	}
	if !strings.Contains(err.Error(), "access boundaries") {
		t.Fatalf("the error does not explain the boundary: %v", err)
	}
}

func TestAssetIndexRefusesADamagedPlaceholder(t *testing.T) {
	broken := append([]byte(nil), pointerFor(t, "city")...)
	broken = []byte(strings.Replace(string(broken), "size ", "size not-a-number\n#", 1))
	tree := pinnedTree(t, map[string][]byte{"assets/city.ytd": broken})
	if _, _, err := AssetIndex(context.Background(), tree, testDomain); err == nil {
		t.Fatal("a damaged placeholder was silently skipped")
	}
}

func TestPointersAreKeyedByPath(t *testing.T) {
	tree := pinnedTree(t, map[string][]byte{
		"assets/city.ytd": pointerFor(t, "city"),
		"src/main.lua":    []byte("-- source\n"),
	})
	ptrs, err := Pointers(context.Background(), tree, testDomain)
	if err != nil {
		t.Fatal(err)
	}
	if len(ptrs) != 1 {
		t.Fatalf("%+v", ptrs)
	}
	p, ok := ptrs["assets/city.ytd"]
	if !ok || p.StorageDomainID != testDomain || p.Size != int64(len("city")) {
		t.Fatalf("%+v", ptrs)
	}
}

func TestTreeFilesystemSeesOnlyThePinnedCommit(t *testing.T) {
	ctx := context.Background()
	tree := pinnedTree(t, map[string][]byte{
		"resources/chat/fxmanifest.lua": []byte("fx_version 'cerulean'\n"),
		"resources/chat/client.lua":     []byte("-- client\n"),
	})
	fsys, err := tree.FS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("resources/chat/fxmanifest.lua")
	if err != nil || !strings.Contains(string(data), "cerulean") {
		t.Fatalf("%q %v", data, err)
	}
	entries, err := fsys.ReadDir("resources/chat")
	if err != nil || len(entries) != 2 {
		t.Fatalf("%+v %v", entries, err)
	}
	if _, err := fsys.Open("resources/chat/../../../etc/passwd"); err == nil {
		t.Fatal("the tree filesystem allowed a path outside the commit")
	}
	if _, err := fsys.Open("not-here"); err == nil {
		t.Fatal("a missing path was opened")
	}
}
