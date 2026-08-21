package app

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
)

// setupManyAssets builds a repository with n hydrated, published assets.
func setupManyAssets(t *testing.T, n, size int) (*Repo, []string) {
	t.Helper()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	names := make([]string, n)
	contents := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("a%03d.bin", i)
		names[i] = name
		content := asset(uint64(i)+3000, size)
		contents[name] = content
		ing, err := repo.Ingest(ctx, bytes.NewReader(content), name)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(name, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	for name, content := range contents {
		r.WriteFile(name, content)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	return repo, names
}

// TestConcurrentPathsKeepTheirOrderAndTheirLocks covers what changed when
// load and unload stopped processing one path at a time.
//
// Three things have to survive it: results stay in the order the caller asked
// for, every path is accounted for exactly once, and no path operation trips
// the lock hierarchy. That last one is the reason each unit of work carries
// its own journal.Sequence — one shared between them recorded independent
// path locks as though a single operation were taking several out of order,
// and refused them.
func TestConcurrentPathsKeepTheirOrderAndTheirLocks(t *testing.T) {
	repo, names := setupManyAssets(t, 24, 64<<10)
	ctx := context.Background()

	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOrderedAndComplete(t, "unload", outs, names)
	for _, o := range outs {
		if o.Err != nil {
			t.Fatalf("unload %s: %v", o.Path, o.Err)
		}
	}
	outs, err = repo.Hydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertOrderedAndComplete(t, "load", outs, names)
	for _, o := range outs {
		if o.Err != nil {
			t.Fatalf("load %s: %v", o.Path, o.Err)
		}
	}
}

func assertOrderedAndComplete(t *testing.T, what string, outs []PathOutcome, names []string) {
	t.Helper()
	if len(outs) != len(names) {
		t.Fatalf("%s reported %d outcomes for %d paths", what, len(outs), len(names))
	}
	for i, n := range names {
		if outs[i].Path != n {
			t.Fatalf("%s outcome %d is %q, expected %q: results must stay in the order the caller asked for",
				what, i, outs[i].Path, n)
		}
	}
}

// TestConcurrentPathsDeliverProgressOneEventAtATime holds the package to the
// contract ProgressFunc documents. Consumers are told they need no
// synchronisation of their own, so fanning out makes serialising the events
// this package's job.
func TestConcurrentPathsDeliverProgressOneEventAtATime(t *testing.T) {
	repo, _ := setupManyAssets(t, 16, 64<<10)
	ctx := context.Background()
	var mu sync.Mutex
	var inFlight, overlaps, events int
	observe := func(ItemProgress) {
		mu.Lock()
		inFlight++
		if inFlight > 1 {
			overlaps++
		}
		events++
		mu.Unlock()
		// Widen the window a real consumer would have.
		for i := 0; i < 1000; i++ {
			_ = i
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	if _, err := repo.Dehydrate(ctx, Selection{}, observe); err != nil {
		t.Fatal(err)
	}
	if events == 0 {
		t.Fatal("no progress events observed")
	}
	if overlaps != 0 {
		t.Fatalf("progress was delivered concurrently %d times in %d events", overlaps, events)
	}
}
