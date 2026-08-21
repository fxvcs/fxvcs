package app

import (
	"context"

	"fxvcs.dev/fxvcs/internal/state"
)

// Discard throws away the working changes to each selected asset, restoring
// the working file from the index pointer.
//
// It is the only operation in this package whose purpose is to destroy bytes
// that exist nowhere else, so it is also the only one that is never implicit:
// nothing calls it as a step of something larger, and no repair, checkout, or
// hook path reaches it. A caller has to ask for it by name, having been told by
// the discard action (actions.go) exactly which assets it would affect.
//
// The working file is restored in the form it currently has. A file holding
// real bytes is rewritten with the real bytes of the index pointer, so a
// texture stays a texture; a file holding a pointer to some other object — the
// conflict state — is rewritten with the index pointer, because materializing
// megabytes the user never asked to download would be a strange way to resolve
// a disagreement between two pointers. Either way the result is the state the
// index describes, which is what "discard my changes" means.
func (r *Repo) Discard(ctx context.Context, sel Selection, progress ProgressFunc) ([]PathOutcome, error) {
	entries, missing, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, err
	}
	var out []PathOutcome
	for _, m := range missing {
		out = append(out, skip(m, "not in the index or not managed by fxvcs (filter=fxvcs)", ErrNotManaged))
	}
	progress.emit(planOf(entries, len(entries)+len(missing), ProgressPhaseWrite))
	var consistent []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			_ = r.refreshIndexStat(context.WithoutCancel(ctx), consistent)
			return out, err
		}
		o, agrees := r.discardOne(ctx, e, progress)
		if agrees {
			consistent = append(consistent, o.Path)
		}
		out = append(out, o)
	}
	_ = r.refreshIndexStat(ctx, consistent)
	return out, nil
}

// discardOne restores one asset from its index pointer. The second result is
// hydrateOne's: whether the working file provably agrees with the index.
func (r *Repo) discardOne(ctx context.Context, e IndexEntry, progress ProgressFunc) (PathOutcome, bool) {
	if o, ok := pointerEntryUsable(e); !ok {
		return o, false
	}
	db, obs, o, ok := r.observeEntry(ctx, e)
	if !ok {
		return o, false
	}
	// The same decision the listing reports (see actions.go).
	switch av := discardAvailability(e, obs); {
	case av.Done:
		_ = r.refreshRow(ctx, db, obs)
		// Nothing to discard means the working file already cleans to the
		// index pointer: worth naming to Git for the reason hydrateOne gives.
		return PathOutcome{Path: e.Path, Digest: e.Pointer.OID, Skipped: true, Reason: av.Reason}, true
	case !av.Available:
		return skip(e.Path, av.Reason, av.Err), false
	}
	if obs.Row.Intent == state.IntentUnhydrated {
		// The working file is a pointer to another object. Restoring the index
		// pointer destroys nothing: the bytes it stands for were never here.
		// ensureRecoverable is deliberately not called for the same reason.
		o = r.placeholder(ctx, e, obs, db, state.OpDiscard, progress)
		return o, !o.Skipped
	}
	o = r.materialize(ctx, e, obs, db, state.OpDiscard, progress)
	return o, !o.Skipped
}
