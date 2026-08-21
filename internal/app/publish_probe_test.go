package app

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// TestPublishAsksEachRemoteOncePerObject is the regression for a publish that
// spent longer asking than uploading.
//
// Every stored object used to cost three existence questions on the way to one
// upload: the planner's bulk call, another inside the uploader, and a third
// inside the store's own Put. For a few thousand chunks against a remote with
// an ordinary round trip, the two redundant ones were minutes of nothing
// happening. The planner's answer is the one to keep — it is bulk — so the
// uploader must not re-ask what it was already told.
func TestPublishAsksEachRemoteOncePerObject(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	const assets = 8
	for i := 0; i < assets; i++ {
		name := fmt.Sprintf("a%02d.bin", i)
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(i)+40, 3<<20)), name)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(name, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")

	inner, _, err := repo.Store("primary")
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingStore{inner: inner}
	batching := &batchingStore{countingStore: counting}
	repo.setStore("primary", batching)

	published, failed, err := repo.ObjectsPublish(ctx, nil, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	objects := len(published)
	if objects < assets {
		t.Fatalf("expected at least one object per asset, got %d", objects)
	}
	// The planner asked in bulk and every planned item was reported absent,
	// so the uploader has nothing left to ask: not one per-object probe
	// should reach the remote from this layer. (The store's own Put still
	// checks before it writes, which is below this wrapper and is the single
	// check that remains.)
	if probes := counting.has.Load(); probes != 0 {
		t.Fatalf("publish issued %d per-object existence probes for %d objects; "+
			"the planner's bulk answer is not being used", probes, objects)
	}
	if batches := batching.batches.Load(); batches != 1 {
		t.Fatalf("expected one bulk existence call, got %d", batches)
	}
}
