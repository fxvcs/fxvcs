package app

import "fxvcs.dev/fxvcs/internal/state"

// What can be done to one asset right now, and why not when the answer is no.
//
// This exists so the decision lives in exactly one place. A user interface has
// to know whether to offer "unload" for a file before the user clicks it, and
// the only alternative to asking the engine is re-deriving the rule from the
// asset's state — which means two implementations of "may I replace these
// bytes", drifting apart, one of them in a process that can destroy work by
// getting it wrong.
//
// The operations themselves consult these same functions, so an action the
// listing offers is an action the operation accepts, and a reason the listing
// gives is the reason the operation would give.

// Action names something the engine can do to an asset.
type Action string

const (
	// ActionLoad materializes the real bytes at the working path.
	ActionLoad Action = "load"
	// ActionUnload replaces the working file with its placeholder.
	ActionUnload Action = "unload"
	// ActionPublish uploads the asset's objects to every required remote.
	ActionPublish Action = "publish"
	// ActionStage records the working file in the Git index.
	//
	// Unlike the others there is no fxvcs operation behind it: the operation is
	// `git add`, and whether Git *can* stage a path is Git's business. What an
	// interface cannot see, and what this answers, is what the clean filter
	// would do to those bytes on the way in — which is the difference between
	// staging a texture and staging a placeholder.
	ActionStage Action = "stage"
	// ActionDiscard throws away working changes, restoring the working file
	// from the index pointer.
	ActionDiscard Action = "discard"
)

// Availability is the answer for one action on one asset.
type Availability struct {
	Action Action
	// Available is whether the operation would act. False covers both refusal
	// and "nothing to do".
	Available bool
	// Done is true when the asset is already in the requested state, which is
	// a different thing from being refused: an interface should show it as
	// satisfied, not as an error.
	Done bool
	// Reason explains a false Available, in the same words the operation uses.
	Reason string
	// Err is the typed cause when the action is refused rather than done.
	Err error
}

// loadAvailability decides whether an asset can be materialized. hydrateOne
// uses it for its own refusals.
func loadAvailability(e IndexEntry, obs observation) Availability {
	av := Availability{Action: ActionLoad}
	if o, ok := pointerEntryUsable(e); !ok {
		av.Reason, av.Err = o.Reason, o.Err
		return av
	}
	if obs.Present {
		switch {
		case obs.Row.Intent == state.IntentHydrated && obs.Row.WorkingDigest == e.Pointer.OID:
			av.Done, av.Reason = true, "already hydrated"
			return av
		case obs.Row.Intent == state.IntentHydrated:
			av.Reason, av.Err = "working file is modified (hydrated-modified); commit it with git add or discard the change first", ErrConflict
			return av
		case obs.Row.WorkingDigest != e.Pointer.OID:
			av.Reason, av.Err = "working file holds a pointer to a different object than the index (conflict); run fxvcs doctor", ErrConflict
			return av
		}
	}
	av.Available = true
	return av
}

// unloadAvailability decides whether an asset can be replaced by its
// placeholder. dehydrateOne uses it for its own refusals.
//
// The rule that matters: a modified working file is never replaced. Its bytes
// exist nowhere else — not in the object store, not in Git — so overwriting
// them would be an unrecoverable loss with no undo.
func unloadAvailability(e IndexEntry, obs observation) Availability {
	av := Availability{Action: ActionUnload}
	if o, ok := pointerEntryUsable(e); !ok {
		av.Reason, av.Err = o.Reason, o.Err
		return av
	}
	switch {
	case !obs.Present:
		av.Reason = "working file is missing; run git checkout -- " + e.Path
		return av
	case obs.Row.Intent == state.IntentUnhydrated && obs.Row.WorkingDigest == e.Pointer.OID:
		av.Done, av.Reason = true, "already a pointer"
		return av
	case obs.Row.Intent == state.IntentUnhydrated:
		av.Reason, av.Err = "working file holds a pointer to a different object than the index (conflict); run fxvcs doctor", ErrConflict
		return av
	case obs.Row.WorkingDigest != e.Pointer.OID:
		av.Reason, av.Err = "working file is modified (hydrated-modified); commit it with git add or discard the change first", ErrConflict
		return av
	}
	av.Available = true
	return av
}

// stageAvailability decides whether staging the working file would change the
// index. It is the one action whose operation is Git's (`git add`), so what it
// answers is not "may I stage" but "would staging this asset record anything".
//
// The distinction that matters is invisible from outside the filter: the clean
// filter passes pointer bytes through unchanged (internal/filter/engine.go), so
// staging an unhydrated asset re-stages the pointer already in the index — a
// no-op, not a way to accidentally commit a placeholder over a texture. A
// hydrated file is re-ingested and cleans back to a pointer, so it too records
// nothing unless its bytes actually changed. That leaves hydrated-modified as
// the only state in which staging does something, which is what an interface
// should be enabling the control for.
func stageAvailability(e IndexEntry, obs observation) Availability {
	av := Availability{Action: ActionStage}
	if o, ok := pointerEntryUsable(e); !ok {
		av.Reason, av.Err = o.Reason, o.Err
		return av
	}
	switch {
	case !obs.Present:
		// Git would stage a deletion. That is a legitimate thing to want and a
		// surprising thing to get from a control named after the file.
		av.Reason = "working file is missing; staging would record a deletion — run git rm " + e.Path + " if that is what you mean"
		return av
	case obs.Row.Intent == state.IntentUnhydrated && obs.Row.WorkingDigest == e.Pointer.OID:
		av.Done, av.Reason = true, "nothing to stage; the working file is the index pointer"
		return av
	case obs.Row.Intent == state.IntentUnhydrated:
		av.Reason, av.Err = "working file holds a pointer to a different object than the index (conflict); run fxvcs doctor", ErrConflict
		return av
	case obs.Row.WorkingDigest == e.Pointer.OID:
		av.Done, av.Reason = true, "nothing to stage; the working file cleans to the index pointer"
		return av
	}
	av.Available = true
	return av
}

// discardAvailability decides whether working changes can be thrown away.
// discardOne uses it for its own refusals.
//
// It is the deliberate inverse of unload: where unload refuses to replace bytes
// that exist nowhere else, discard exists to destroy exactly those bytes on
// request. So the rule is not "are these bytes safe to lose" — they are not,
// and that is the point — but "is there a recorded state to return to", which
// is true whenever the working file disagrees with the index pointer.
func discardAvailability(e IndexEntry, obs observation) Availability {
	av := Availability{Action: ActionDiscard}
	if o, ok := pointerEntryUsable(e); !ok {
		av.Reason, av.Err = o.Reason, o.Err
		return av
	}
	switch {
	case !obs.Present:
		av.Reason = "working file is missing; there is no change to discard — run fxvcs repair to rewrite the placeholder"
		return av
	case obs.Row.WorkingDigest == e.Pointer.OID:
		av.Done, av.Reason = true, "no local changes to discard"
		return av
	}
	av.Available = true
	return av
}

// publishAvailability decides whether the asset has anything to upload.
// pending comes from the shared ledger; required is the configured set of
// remotes that must hold every object before Git refs may reference them.
func publicationAvailability(e IndexEntry, pending bool, required []string) Availability {
	av := Availability{Action: ActionPublish}
	if o, ok := pointerEntryUsable(e); !ok {
		av.Reason, av.Err = o.Reason, o.Err
		return av
	}
	switch {
	case len(required) == 0:
		av.Reason = "no storage is marked as required before push; add one with fxvcs storage add"
		return av
	case !pending:
		av.Done, av.Reason = true, "already uploaded"
		return av
	}
	av.Available = true
	return av
}
