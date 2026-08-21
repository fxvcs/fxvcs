package app

import (
	"testing"
	"time"
)

// TestCacheBudgetSkipsMeasuringUntilItCouldMatter: measuring the cache means
// walking every object in it, and the check runs after every ingested file.
func TestCacheBudgetSkipsMeasuringUntilItCouldMatter(t *testing.T) {
	var b cacheBudget
	const limit = 1000
	if !b.mustMeasure(0, limit) {
		t.Fatal("an unmeasured budget must measure")
	}
	b.measured(400, 0)
	if b.mustMeasure(100, limit) {
		t.Fatal("measured 400 with 100 written since: cannot have reached 1000")
	}
	if !b.mustMeasure(700, limit) {
		t.Fatal("measured 400 with 700 written since: must measure")
	}
}

// TestCacheBudgetTTLIsNotDefeatedByACallLoop: a tight loop of ingests must not
// keep the estimate fresh forever, or growth by another process on the same
// host would never be noticed.
func TestCacheBudgetTTLIsNotDefeatedByACallLoop(t *testing.T) {
	var b cacheBudget
	b.measured(400, 0)
	for i := 0; i < 100; i++ {
		if b.mustMeasure(0, 1000) {
			t.Fatal("re-measured while comfortably under budget")
		}
	}
	// Age the measurement past the bound.
	b.mu.Lock()
	b.at = time.Now().Add(-budgetTTL - time.Second)
	b.mu.Unlock()
	if !b.mustMeasure(0, 1000) {
		t.Fatal("an estimate older than the staleness bound must be re-measured")
	}
}

// TestCacheBudgetStopsReMeasuringOnThePinnedFloor is the regression for a
// cliff that made a configured budget worse than none.
//
// Pruning never evicts a pinned object, so a repository holding more pending
// content than its budget sits permanently over it. Every ingested file then
// measured the cache and ran a prune that measured it again — two full walks
// per file, to re-derive an answer that cannot change until those objects are
// published.
func TestCacheBudgetStopsReMeasuringOnThePinnedFloor(t *testing.T) {
	var b cacheBudget
	const limit = 1000
	// Pruning ran and left 5000 bytes, all pinned.
	b.pruned(5000, 0, true)
	if b.mustMeasure(1<<10, limit) {
		t.Fatal("re-measured while sitting on the pinned floor")
	}
	// Enough new content to be worth another look.
	if !b.mustMeasure(floorSlack+1, limit) {
		t.Fatal("must measure again once meaningfully more has been stored")
	}
	// A prune that got under the budget leaves no floor.
	b.pruned(500, 0, false)
	if b.mustMeasure(100, limit) {
		t.Fatal("a successful prune must not leave the floor set")
	}
}

// TestCacheBudgetUsesTheWallClock: Options.Now is a public injection point for
// deterministic timestamps. If the staleness bound honoured it, a consumer
// with a frozen clock would switch the bound off and let the cache exceed its
// budget without limit.
func TestCacheBudgetUsesTheWallClock(t *testing.T) {
	r, cacheDir := testRepo(t)
	frozen := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := openTest(t, r, cacheDir)
	repo.opts.Now = func() time.Time { return frozen }
	if got := repo.Now(); !got.Equal(frozen) {
		t.Fatalf("clock not frozen: %v", got)
	}
	var b cacheBudget
	b.measured(400, 0)
	b.mu.Lock()
	b.at = time.Now().Add(-budgetTTL - time.Second)
	b.mu.Unlock()
	if !b.mustMeasure(0, 1000) {
		t.Fatal("the staleness bound follows the frozen repository clock; it must not")
	}
}
