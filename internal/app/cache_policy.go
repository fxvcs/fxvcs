package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/journal"
)

// CacheLimit is the size this clone trims the host cache back to after an
// operation adds objects. Zero means no automatic trimming.
func (r *Repo) CacheLimit() int64 {
	if r.Local.Spec.Cache == nil {
		return 0
	}
	return r.Local.Spec.Cache.LimitBytes
}

// SetCacheLimit records the automatic-trim budget in local configuration. It
// is a machine-local preference: how much disk this computer is willing to
// spend on cached asset data is not a property of the repository.
func (r *Repo) SetCacheLimit(ctx context.Context, limitBytes int64) error {
	if limitBytes < 0 {
		return fmt.Errorf("%w: cache limit %d", ErrInvalidArgument, limitBytes)
	}
	h, err := r.Lock(ctx, journal.LockPublication, "local-config")
	if err != nil {
		return err
	}
	defer func() { _ = h.Release() }()
	lc, err := LoadLocalConfig(r.Paths.ConfigYAML)
	if err != nil {
		return err
	}
	if limitBytes == 0 {
		lc.Spec.Cache = nil
	} else {
		lc.Spec.Cache = &domain.CacheSettings{LimitBytes: limitBytes}
	}
	if err := SaveLocalConfig(r.Paths.ConfigYAML, lc); err != nil {
		return err
	}
	r.Local = lc
	return nil
}

// enforceCacheLimit trims the cache back to the configured budget after an
// operation added objects to it.
//
// It is deliberately best effort and never fails the operation that called it:
// hydrating an asset must not fail because a cache eviction could not take a
// lock, and the objects that matter — anything pending publication — are
// pinned and cannot be evicted at all. A missed trim is recovered by the next
// operation or by `fxvcs cache prune`.
func (r *Repo) enforceCacheLimit(ctx context.Context) { r.enforceCacheLimitReserving(ctx, 0) }

// enforceCacheLimitReserving trims to the budget but never below reserve
// bytes. The caller passes the size of the asset it is about to read: a limit
// smaller than one asset must not make that asset unreadable by evicting the
// chunks that were just fetched for it.
func (r *Repo) enforceCacheLimitReserving(ctx context.Context, reserve int64) {
	limit := r.CacheLimit()
	if limit <= 0 {
		return
	}
	if reserve > limit {
		limit = reserve
	}
	// Measuring the cache means walking every object in it, and this runs
	// after every ingested file. Staging a directory of assets therefore cost
	// one full walk per file — quadratic in a cache that is, by design,
	// shared by every clone on the host and much larger than any one
	// repository. Skip the walk while the last measurement plus what has been
	// stored since cannot have reached the budget.
	// Snapshot the counter before the walk, not after: bytes stored while the
	// walk is running may be missed by it, and counting them as already
	// measured would understate the cache from then on.
	written := r.Cache.BytesWritten()
	if !r.budget.mustMeasure(written, limit) {
		return
	}
	total, err := r.Cache.TotalBytes()
	if err != nil {
		return
	}
	r.budget.measured(total, written)
	if total <= limit {
		return
	}
	h, err := r.TryLock(ctx, journal.LockCachePrune, "cache")
	if err != nil {
		return // another process is already pruning
	}
	defer func() { _ = h.Release() }()
	pruneWritten := r.Cache.BytesWritten()
	res, perr := r.Cache.Prune(limit)
	if perr != nil {
		r.budget.forget()
		return
	}
	// Prune already measured the cache, so record what it left rather than
	// walking again to find out. When it could not get under the budget the
	// excess is pinned — pending publication, and never evictable — and no
	// amount of re-measuring will change that until those objects are
	// published, so say so and stop asking after every file.
	r.budget.pruned(res.BytesAfter, pruneWritten, res.BytesAfter > limit)
}

// cacheBudget remembers the last measurement of the cache so the budget check
// does not have to repeat it for every file.
//
// The estimate only counts what this process stored, and the cache is shared,
// so it can be an underestimate — which is why it also expires. Being wrong
// briefly means trimming slightly late, never storing anything unsafely.
type cacheBudget struct {
	mu      sync.Mutex
	known   bool
	total   int64     // bytes at the last measurement
	written int64     // Cache.BytesWritten() at the last measurement
	at      time.Time // when it was taken
	// pinnedFloor records that pruning ran and could not get under the
	// budget. The excess is pending publication and cannot be evicted, so
	// measuring again only repeats the same answer.
	pinnedFloor bool
}

// budgetTTL bounds how stale an estimate may be, so growth by another process
// on the same host is noticed without every caller re-measuring.
//
// The clock here is deliberately the wall clock rather than the repository's
// injectable one: this is a staleness bound on a cache measurement, not
// repository time, and a caller that froze the clock for deterministic
// timestamps would otherwise switch the bound off entirely and let the cache
// exceed its budget without limit.
const budgetTTL = 30 * time.Second

// floorSlack is how much new content may be stored while sitting on the
// pinned floor before it is worth measuring again.
const floorSlack = 64 << 20

func (b *cacheBudget) mustMeasure(written, limit int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known || time.Since(b.at) > budgetTTL {
		return true
	}
	grown := written - b.written
	if b.pinnedFloor {
		return grown > floorSlack
	}
	return b.total+grown > limit
}

func (b *cacheBudget) measured(total, written int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known, b.total, b.written, b.at = true, total, written, time.Now()
	b.pinnedFloor = false
}

func (b *cacheBudget) pruned(total, written int64, atFloor bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known, b.total, b.written, b.at = true, total, written, time.Now()
	b.pinnedFloor = atFloor
}

func (b *cacheBudget) forget() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.known, b.pinnedFloor = false, false
}
