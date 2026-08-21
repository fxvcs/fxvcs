package app

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestStorageStatsSeparatesTheTwoSavings checks the report against a
// repository whose savings are known by construction, and keeps the two
// mechanisms apart: whole files that are byte-identical, and pieces shared
// between files that only resemble each other. They scale differently, so a
// single number would hide which one a repository is actually getting.
func TestStorageStatsSeparatesTheTwoSavings(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	const size = 4 << 20
	base := asset(400, size)
	// A near-identical variant: same content with one region rewritten, which
	// is what a re-exported asset looks like.
	variant := bytes.Clone(base)
	for i := 2 << 20; i < 2<<20+4096; i++ {
		variant[i] ^= 0x33
	}
	unrelated := asset(401, size)

	files := []struct {
		path string
		data []byte
	}{
		{"a/base.bin", base},
		{"b/base.bin", base}, // byte-identical copy
		{"c/base.bin", base}, // byte-identical copy
		{"d/variant.bin", variant},
		{"e/unrelated.bin", unrelated},
	}
	for _, f := range files {
		ing, err := repo.Ingest(ctx, bytes.NewReader(f.data), f.path)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(f.path, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")

	st, err := repo.StorageStats(ctx, Selection{}, StorageStatsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Assets != 5 || st.Contents != 3 {
		t.Fatalf("%d assets / %d contents, want 5 / 3", st.Assets, st.Contents)
	}
	if st.Unresolved != 0 {
		t.Fatalf("%d assets could not be examined although everything is local", st.Unresolved)
	}
	if st.LogicalBytes != 5*size {
		t.Fatalf("logical bytes %d, want %d", st.LogicalBytes, 5*size)
	}

	// Two of the three copies are pure duplication.
	if st.IdenticalFiles != 2 || st.IdenticalFileBytes != 2*size {
		t.Fatalf("identical files %d / %d bytes, want 2 / %d", st.IdenticalFiles, st.IdenticalFileBytes, 2*size)
	}
	// The variant shares most of its chunks with the base, so the second
	// mechanism must show a real saving too — and the two must add up.
	if st.SharedPartBytes <= 0 {
		t.Fatalf("a near-identical variant produced no shared-part saving: %+v", st)
	}
	if st.IdenticalFileBytes+st.SharedPartBytes != st.SavedBytes() {
		t.Fatalf("the mechanisms do not add up: %d + %d != %d",
			st.IdenticalFileBytes, st.SharedPartBytes, st.SavedBytes())
	}
	if st.FilesSharingParts < 2 {
		t.Fatalf("%d contents share parts; the base and its variant should", st.FilesSharingParts)
	}
	// Stored bytes must be real: never more than the logical size, and enough
	// for the unrelated asset plus one copy of the base.
	if st.StoredBytes >= st.LogicalBytes || st.StoredBytes < 2*size {
		t.Fatalf("stored bytes %d against logical %d look wrong", st.StoredBytes, st.LogicalBytes)
	}
	if pct := st.SavedFraction(); pct <= 0.4 || pct >= 1 {
		t.Fatalf("saved fraction %.2f is not plausible for this fixture", pct)
	}

	// The most-reused object names the paths that share it, counting every
	// path rather than every distinct content.
	top, err := repo.TopSharedObjects(ctx, Selection{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) == 0 {
		t.Fatal("no shared objects reported")
	}
	if top[0].Users < 3 {
		t.Fatalf("the most reused object is used by %d paths; three files hold identical content", top[0].Users)
	}
	if top[0].SavedBytes != top[0].Bytes*int64(top[0].Users-1) {
		t.Fatalf("saving for a shared object is miscomputed: %+v", top[0])
	}
}

// TestStorageStatsDoesNotFetchByDefault: a report must not quietly pull a
// repository's worth of metadata over the network, and must say when its
// answer is therefore incomplete.
func TestStorageStatsDoesNotFetchByDefault(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("a%d.bin", i)
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(500+i), 3<<20)), path)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(path, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}

	// A machine that has the index but none of the objects.
	fresh := openTest(t, r, t.TempDir())
	if _, err := fresh.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	blocked := &countingStore{inner: mustStore(t, fresh, "primary")}
	fresh.setStore("primary", blocked)

	st, err := fresh.StorageStats(ctx, Selection{}, StorageStatsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.gets.Load() != 0 {
		t.Fatalf("the default report made %d network reads", blocked.gets.Load())
	}
	if st.Unresolved != 3 {
		t.Fatalf("%d unresolved assets reported, want 3", st.Unresolved)
	}
	if st.SavedBytes() != 0 {
		t.Fatalf("an unexaminable repository must not claim a saving, got %d", st.SavedBytes())
	}

	// Asking for it explicitly resolves them.
	st, err = fresh.StorageStats(ctx, Selection{}, StorageStatsOptions{Fetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.Unresolved != 0 {
		t.Fatalf("%d assets still unresolved with Fetch set", st.Unresolved)
	}
	if blocked.gets.Load() == 0 {
		t.Fatal("Fetch did not read anything")
	}
}
