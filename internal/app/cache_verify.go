package app

import (
	"context"
	"sort"

	"fxvcs.dev/fxvcs/internal/journal"
)

// CacheVerifyReport is the result of re-reading cached objects.
type CacheVerifyReport struct {
	// Checked and Bytes count what was read.
	Checked int
	Bytes   int64
	// Corrupt lists objects whose bytes no longer hash to their name.
	Corrupt []CacheCorruption
	// Removed counts corrupt objects dropped by Repair.
	Removed int
	// Kept counts corrupt objects that were left in place because they are
	// pinned: their bytes are the only copy of something not yet published,
	// so deleting them would destroy data rather than recover it.
	Kept []CacheCorruption
}

// CacheVerifyProgress describes one cache object before and after its bytes
// are hashed. An empty Digest is the operation-wide plan.
type CacheVerifyProgress struct {
	Done, Total    int
	Domain, Digest string
	Bytes          int64
	Started        bool
	Err            error
}

// CacheCorruption is one object that failed verification.
type CacheCorruption struct {
	Domain string
	Digest string
	Bytes  int64
	Pinned bool
	Reason string
}

// CacheVerifyOptions configures CacheVerify.
type CacheVerifyOptions struct {
	// AllDomains verifies every storage domain in the cache, not only this
	// repository's.
	AllDomains bool
	// Repair removes corrupt objects so the next operation refetches them.
	// A corrupt object that is pinned (pending publication) is never removed:
	// see CacheVerifyReport.Kept.
	Repair bool
}

// CacheVerify re-reads cached objects and checks their bytes against their
// digests. This is the only operation that detects silent disk corruption: an
// object is normally verified when it is read, so an object that is never read
// can rot unnoticed until a deployment needs it.
//
// It writes nothing unless Repair is set, and it holds the cache-prune lock
// only while removing objects, never while reading them.
func (r *Repo) CacheVerify(ctx context.Context, opts CacheVerifyOptions, progress func(CacheVerifyProgress)) (CacheVerifyReport, error) {
	var rep CacheVerifyReport
	entries, err := r.Cache.List()
	if err != nil {
		return rep, err
	}
	var subject []int
	for i, e := range entries {
		if opts.AllDomains || e.Domain == r.Domain {
			subject = append(subject, i)
		}
	}
	if progress != nil {
		progress(CacheVerifyProgress{Total: len(subject)})
	}
	for n, i := range subject {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		e := entries[i]
		if progress != nil {
			progress(CacheVerifyProgress{Done: n, Total: len(subject), Domain: e.Domain, Digest: e.Digest, Bytes: e.Size, Started: true})
		}
		rep.Checked++
		rep.Bytes += e.Size
		verifyErr := r.Cache.Verify(e.Domain, e.Digest)
		if verifyErr != nil {
			rep.Corrupt = append(rep.Corrupt, CacheCorruption{Domain: e.Domain, Digest: e.Digest, Bytes: e.Size, Pinned: e.Pinned, Reason: verifyErr.Error()})
		}
		if progress != nil {
			progress(CacheVerifyProgress{Done: n + 1, Total: len(subject), Domain: e.Domain, Digest: e.Digest, Bytes: e.Size, Err: verifyErr})
		}
	}
	sort.Slice(rep.Corrupt, func(i, j int) bool { return rep.Corrupt[i].Digest < rep.Corrupt[j].Digest })
	if !opts.Repair || len(rep.Corrupt) == 0 {
		return rep, nil
	}
	h, err := r.Lock(ctx, journal.LockCachePrune, "cache")
	if err != nil {
		return rep, err
	}
	defer func() { _ = h.Release() }()
	for _, c := range rep.Corrupt {
		if c.Pinned {
			// Pinned means "the only copy of something not yet published".
			// Removing it would turn recoverable corruption into lost work;
			// the operator re-adds the file instead.
			rep.Kept = append(rep.Kept, c)
			continue
		}
		if err := r.Cache.Remove(c.Domain, c.Digest, false); err != nil {
			rep.Kept = append(rep.Kept, CacheCorruption{Domain: c.Domain, Digest: c.Digest, Bytes: c.Bytes, Reason: err.Error()})
			continue
		}
		rep.Removed++
	}
	return rep, nil
}
