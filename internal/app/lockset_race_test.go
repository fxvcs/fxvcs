package app

import (
	"context"
	"testing"
)

// TestConcurrentPathsDoNotShareOneLockSequence covers the paths that reach a
// lock without going through Repo.Lock.
//
// lock.Set owns a journal.Sequence, which records what one operation holds so
// it can enforce the acquisition order, and it is not safe to share. Giving
// each unit of work its own set is only worth anything if every acquisition
// inside that work actually uses it: a call that reaches for the repository's
// shared set instead puts eight goroutines back on one Sequence, appending to
// and removing from the same slice.
//
// The route here is the ordinary one. A path that is already unloaded, whose
// stat-cache row is missing so its fingerprint had to be re-derived, refreshes
// that row under its path lock — which is what a second unload does on a clone
// whose state has been rebuilt.
//
// Run with -race; without it this test still exercises the branch but can only
// fail loudly rather than precisely.
func TestConcurrentPathsDoNotShareOneLockSequence(t *testing.T) {
	repo, names := setupManyAssets(t, 24, 32<<10)
	ctx := context.Background()
	if outs, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
		t.Fatalf("first unload: %v", err)
	} else if len(outs) != len(names) {
		t.Fatalf("first unload reported %d of %d", len(outs), len(names))
	}
	// Drop the stat cache, so every path is observed afresh and rehashed.
	db, err := repo.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec("DELETE FROM paths"); err != nil {
		t.Fatal(err)
	}
	// Every path is already done, every fingerprint is rehashed: all eight
	// workers take a path lock to refresh a row at once.
	outs, err := repo.Dehydrate(ctx, Selection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != len(names) {
		t.Fatalf("second unload reported %d outcomes for %d paths", len(outs), len(names))
	}
	for i, n := range names {
		if outs[i].Path != n {
			t.Fatalf("outcome %d is %q, expected %q", i, outs[i].Path, n)
		}
		if !outs[i].Skipped {
			t.Fatalf("%s was not reported as already unloaded", n)
		}
	}
	// The refresh must actually have happened: a row per path, or the lock
	// order rejected it and the stat cache is silently stale.
	var rows int
	if err := db.SQL().QueryRow("SELECT count(*) FROM paths").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(names) {
		t.Fatalf("stat cache holds %d rows for %d paths; the refresh was skipped", rows, len(names))
	}
}
