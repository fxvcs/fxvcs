package app

import (
	"context"
	"errors"

	"fxvcs.dev/fxvcs/internal/state"
)

// PathStatus is the derived checkout state of one managed path.
type PathStatus struct {
	Path        string
	State       state.Condition
	IndexDigest string // pointer OID from the index ("" when the blob is not a pointer)
	Size        int64  // pointer size (the asset's size as recorded in the index) when known
	// WorkingSize is what the working file occupies on disk right now: the
	// asset's real size when it is hydrated, a couple of hundred bytes of
	// placeholder when it is not, zero when the file is absent. It costs
	// nothing — the stat that derives the state already carries it.
	WorkingSize int64
	// SizeDelta is WorkingSize - Size: how much the asset grew or shrank
	// locally. It is nil when the two are not comparable, which is every state
	// in which the working file is a placeholder — subtracting an asset's size
	// from its pointer's would report a 40 MB deletion for a file nobody
	// touched. Non-nil and zero is a real answer: a change that kept the size.
	SizeDelta *int64
	Cached    bool   // object bytes present in the local cache
	Published bool   // recorded on every requiredForPublication remote (ledger)
	Pending   bool   // ingested locally, not yet published everywhere required
	Detail    string // human explanation for conflict/missing rows
	// Actions is what the engine would do to this asset right now, and why
	// not when it would refuse. It is the same decision the operations make;
	// see actions.go.
	Actions []Availability
}

// Status derives the consistency state of every selected managed path from
// the index pointer, the working file, and the state row. Unchanged files are
// not re-hashed: the stat fingerprint recorded in state.db decides whether
// the recorded digest is still trustworthy. Rows re-derived here are written
// back opportunistically (per-path lock, no wait) so the next status is fast.
func (r *Repo) Status(ctx context.Context, sel Selection) ([]PathStatus, []PathOutcome, error) {
	entries, missing, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, nil, err
	}
	var skipped []PathOutcome
	for _, m := range missing {
		skipped = append(skipped, skip(m, "not in the index or not managed by fxvcs (filter=fxvcs)", ErrNotManaged))
	}
	// Recorded rows for the selected paths only. A listing scoped to one
	// directory must cost what it selected, not what the repository holds:
	// reading every row to answer a question about twelve assets is the kind
	// of fixed cost that decides whether a caller can afford to ask at all.
	// Unscoped, one query still beats thousands of parameterized ones.
	rows := map[string]state.PathRow{}
	db, err := r.StateIfExists(ctx)
	switch {
	case err == nil:
		var list []state.PathRow
		if len(sel.Paths) == 0 {
			list, err = db.List(ctx)
		} else {
			paths := make([]string, 0, len(entries))
			for _, e := range entries {
				paths = append(paths, e.Path)
			}
			list, err = db.ListPaths(ctx, paths)
		}
		if err != nil {
			return nil, nil, err
		}
		for _, row := range list {
			rows[row.Path] = row
		}
	case errors.Is(err, state.ErrNotFound):
	default:
		return nil, nil, err
	}
	ledger, err := r.LedgerIfExists(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return nil, nil, err
	}
	required := r.RequiredRemotes()
	// Resolve each asset's storage plan once and answer the ledger for every
	// object behind the whole selection in one batched pass. Done per asset
	// this was two database round trips per stored object — and a chunked
	// asset is a manifest plus a chunk per piece, so a listing spent most of
	// its time asking the same small database the same small question.
	contents := r.localContents(ctx, entries)
	var pendingIdx map[string]bool
	var remoteIdx map[string]map[string]bool
	if ledger != nil {
		pendingIdx, remoteIdx, err = ledger.PublicationIndex(ctx, contentDigests(entries, contents))
		if err != nil {
			return nil, nil, err
		}
	}
	out := make([]PathStatus, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ps := PathStatus{Path: e.Path}
		if e.HasPointer {
			ps.IndexDigest, ps.Size = e.Pointer.OID, e.Pointer.Size
		}
		var prev *state.PathRow
		if row, ok := rows[e.Path]; ok {
			prev = &row
		}
		obs, err := r.observePath(e.Path, ps.IndexDigest, prev)
		if err != nil {
			return nil, nil, err
		}
		if obs.Present {
			ps.WorkingSize = obs.Row.Fingerprint.Size
			// Only real bytes may be measured against the recorded size. A
			// placeholder's length says nothing about the asset it stands for.
			if obs.Row.Intent == state.IntentHydrated && e.HasPointer {
				delta := ps.WorkingSize - ps.Size
				ps.SizeDelta = &delta
			}
		}
		switch {
		case e.Unmerged:
			ps.State, ps.Detail = state.Conflict, "unmerged index entry"
		case e.PointerErr != nil:
			ps.State, ps.Detail = state.Conflict, "invalid index pointer: "+e.PointerErr.Error()
		case !e.HasPointer:
			ps.State, ps.Detail = state.Conflict, "index blob is not an fxvcs pointer (not migrated)"
		default:
			ps.State = obs.Row.Condition()
			if ps.State == state.Conflict {
				ps.Detail = "working file holds a pointer to " + obs.Row.WorkingDigest + ", index pointer is " + ps.IndexDigest
			}
		}
		if c, ok := contents[e.Path]; ok {
			// A chunked pointer is "cached" only when every object behind it
			// is; its publication state is the state of those objects, not of
			// a whole-file object that was never stored.
			ps.Cached = r.contentCached(c)
			if ledger != nil {
				ps.Pending, ps.Published = contentPublication(c, pendingIdx, remoteIdx, required)
			}
		}
		ps.Actions = []Availability{
			loadAvailability(e, obs),
			unloadAvailability(e, obs),
			publicationAvailability(e, ps.Pending, required),
			stageAvailability(e, obs),
			discardAvailability(e, obs),
		}
		if obs.Rehashed && e.HasPointer && !e.Unmerged {
			if wdb, err := r.State(ctx); err == nil {
				_ = r.refreshRow(ctx, wdb, obs)
			}
		}
		out = append(out, ps)
	}
	return out, skipped, nil
}

// localContents resolves the storage plan of every selected asset once, from
// local data only.
//
// Once, because deciding whether an asset is cached and deciding whether it is
// published both need the same answer, and for a chunked asset that answer is
// a manifest read from disk. Locally, because a listing must never download:
// resolving a missing manifest from a remote would turn opening a folder into
// a transfer, one round trip per unresolved asset. An asset whose manifest is
// not here is simply absent from the result, and reports neither cached nor
// published — which is what it already did when its manifest could not be
// read.
func (r *Repo) localContents(ctx context.Context, entries []IndexEntry) map[string]Content {
	out := make(map[string]Content, len(entries))
	for _, e := range entries {
		if !e.HasPointer {
			continue
		}
		c, err := r.ContentLocal(ctx, e.Pointer)
		if err != nil {
			continue
		}
		out[e.Path] = c
	}
	return out
}

// contentDigests lists every distinct object digest the selection depends on,
// in entry order so the batched queries are stable.
func contentDigests(entries []IndexEntry, contents map[string]Content) []string {
	seen := make(map[string]bool, len(contents))
	out := make([]string, 0, len(contents))
	for _, e := range entries {
		for _, o := range contents[e.Path].Objects {
			if !seen[o.Digest] {
				seen[o.Digest] = true
				out = append(out, o.Digest)
			}
		}
	}
	return out
}

// contentCached reports that every object behind resolved content is present
// locally.
func (r *Repo) contentCached(c Content) bool {
	for _, o := range c.Objects {
		if !r.Cache.Has(r.Domain, o.Digest) {
			return false
		}
	}
	return true
}

// contentPublication aggregates the publication state of every object behind
// an asset: pending if any object is still pinned, published only when all of
// them are on every required remote.
func contentPublication(c Content, pendingIdx map[string]bool, remoteIdx map[string]map[string]bool, required []string) (pending, published bool) {
	published = true
	for _, o := range c.Objects {
		p, pub := objectPublication(o.Digest, pendingIdx, remoteIdx, required)
		pending = pending || p
		published = published && pub
	}
	return pending, published
}

// objectPublication projects the batched ledger read for one digest. A digest
// the ledger has never seen reports neither: nothing is known about it, which
// is not the same as knowing it is unpublished.
func objectPublication(digest string, pendingIdx map[string]bool, remoteIdx map[string]map[string]bool, required []string) (pending, published bool) {
	p, known := pendingIdx[digest]
	if !known {
		return false, false
	}
	if len(required) == 0 {
		return p, false
	}
	have := remoteIdx[digest]
	for _, n := range required {
		if !have[n] {
			return p, false
		}
	}
	return p, true
}

// publicationFlags projects the ledger for one digest.
func publicationFlags(ctx context.Context, l *state.Ledger, digest string, required []string) (pending, published bool) {
	rec, ok, err := l.Object(ctx, digest)
	if err != nil || !ok {
		return false, false
	}
	pending = rec.Pending
	pub, err := l.PublishedRemotes(ctx, digest)
	if err != nil {
		return pending, false
	}
	have := map[string]bool{}
	for _, n := range pub {
		have[n] = true
	}
	if len(required) == 0 {
		return pending, false
	}
	for _, n := range required {
		if !have[n] {
			return pending, false
		}
	}
	return pending, true
}
