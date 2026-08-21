package sdk

import (
	"context"
	"errors"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/lock"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/state"
)

// ---- hydration -------------------------------------------------------------

type HydrateRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"` // empty = all tracked paths
}

type HydrateResult struct {
	Items []ItemOutcome `json:"items"`
}

// Hydrate materializes the object behind each selected index pointer at its
// working path (compare-and-swap; never overwrites a modified file). Every
// selected path yields an ItemOutcome; refusals are Skipped with a Reason.
// When any item failed for a typed cause (busy lock, unavailable or corrupt
// object, conflict) the first such error is returned alongside the result.
func (c *Client) Hydrate(ctx context.Context, req HydrateRequest) (*HydrateResult, error) {
	r, err := c.openRepo(ctx, req.Path, "hydrate")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	c.progress(Event{Operation: "hydrate", Kind: EventOperationStarted})
	outs, err := r.Hydrate(ctx, selection(req.Selector), c.itemProgress("hydrate"))
	c.checkoutTerminalOutcomes("hydrate", outs)
	c.progress(Event{Operation: "hydrate", Kind: EventOperationFinished})
	res := &HydrateResult{Items: itemOutcomes(outs)}
	if err != nil {
		return res, translateError(r.Root, err)
	}
	return res, firstItemError(r.Root, outs)
}

type DehydrateRequest = HydrateRequest
type DehydrateResult = HydrateResult

// Dehydrate replaces each selected hydrated-clean working file with its
// index pointer bytes. Modified files (clean result differs from the index
// pointer) are refused; the object bytes are guaranteed cached-and-pinned or
// published before the file is replaced.
func (c *Client) Dehydrate(ctx context.Context, req DehydrateRequest) (*DehydrateResult, error) {
	r, err := c.openRepo(ctx, req.Path, "dehydrate")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	c.progress(Event{Operation: "dehydrate", Kind: EventOperationStarted})
	outs, err := r.Dehydrate(ctx, selection(req.Selector), c.itemProgress("dehydrate"))
	c.checkoutTerminalOutcomes("dehydrate", outs)
	c.progress(Event{Operation: "dehydrate", Kind: EventOperationFinished})
	res := &DehydrateResult{Items: itemOutcomes(outs)}
	if err != nil {
		return res, translateError(r.Root, err)
	}
	return res, firstItemError(r.Root, outs)
}

type DiscardRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
	// All discards the local changes to every tracked asset.
	//
	// It exists so that "everything" cannot be reached by accident. Every other
	// selector in this package reads empty as "all tracked paths", which for an
	// operation that destroys unrecoverable bytes is the wrong default to
	// inherit — a caller that forgets to fill in a selection would wipe the
	// working tree instead of doing nothing. An empty selector is refused here;
	// this field is the way to mean it.
	All bool `json:"all,omitempty"`
}

type DiscardResult = HydrateResult

// Discard throws away the working changes to each selected asset, restoring
// the working file from the index pointer. An asset that is loaded stays
// loaded, with the recorded bytes; an asset whose working file is a pointer to
// some other object gets the index pointer back.
//
// It destroys data on purpose, and it is the only operation here that does:
// the bytes of a changed asset are in no commit and no object store, so there
// is nothing to undo it with. Nothing else in this package calls it. Consult
// the discard action in Status before offering it, and — since the SDK has no
// way to ask — make sure a person meant it.
//
// Assets with no local changes are reported as skipped with a reason, not as
// errors, exactly like Hydrate and Dehydrate.
func (c *Client) Discard(ctx context.Context, req DiscardRequest) (*DiscardResult, error) {
	if len(req.Selector.Paths) == 0 && !req.All {
		return nil, NewError(CodeInvalidArgument, "discard needs the paths to discard").
			WithHint("fxvcs discard <path>   (--all discards your changes to every tracked asset)")
	}
	r, err := c.openRepo(ctx, req.Path, "discard")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	c.progress(Event{Operation: "discard", Kind: EventOperationStarted})
	outs, err := r.Discard(ctx, selection(req.Selector), c.itemProgress("discard"))
	c.checkoutTerminalOutcomes("discard", outs)
	c.progress(Event{Operation: "discard", Kind: EventOperationFinished})
	res := &DiscardResult{Items: itemOutcomes(outs)}
	if err != nil {
		return res, translateError(r.Root, err)
	}
	return res, firstItemError(r.Root, outs)
}

func selection(s PathSelector) app.Selection { return app.Selection{Paths: s.Paths, Glob: s.Glob} }

func itemOutcomes(outs []app.PathOutcome) []ItemOutcome {
	items := make([]ItemOutcome, 0, len(outs))
	for _, o := range outs {
		items = append(items, ItemOutcome{Path: o.Path, Digest: o.Digest, Bytes: o.Bytes,
			Skipped: o.Skipped, Reason: o.Reason, Reuse: Reuse(o.Reuse)})
	}
	return items
}

// firstItemError decides the operation error from per-item outcomes.
// Refusals (a path that is not managed, changed locally, or in conflict) are
// reported per item and do not fail the operation unless nothing else could
// be done: the error is returned only when no item succeeded or was already
// in the requested state. Real failures (busy lock, unavailable or corrupt
// object, storage or I/O errors) always surface as the typed error of the
// first such item while the per-item outcomes stay available.
func firstItemError(root string, outs []app.PathOutcome) error {
	var firstRefusal *app.PathOutcome
	effective := 0
	for i := range outs {
		o := &outs[i]
		switch {
		case o.Err == nil:
			effective++ // done, or already in the requested state
		case isItemRefusal(o.Err):
			if firstRefusal == nil {
				firstRefusal = o
			}
		default:
			return translateCheckoutError(root, o.Err)
		}
	}
	if effective == 0 && firstRefusal != nil {
		err := translateCheckoutError(root, firstRefusal.Err)
		var se *Error
		if errors.As(err, &se) {
			se.Message = "nothing could be done: " + firstRefusal.Path + ": " + firstRefusal.Reason
			if errors.Is(firstRefusal.Err, app.ErrNotManaged) {
				se.Hint = "fxvcs track " + firstRefusal.Path + "   (fxvcs status lists tracked assets)"
			}
		}
		return err
	}
	return nil
}

// isItemRefusal reports the per-item causes that are choices for the user
// rather than failures of the operation.
func isItemRefusal(err error) bool {
	return errors.Is(err, app.ErrNotManaged) || errors.Is(err, app.ErrConflict)
}

// checkoutCode classifies a checkout-engine cause. It is shared by the error
// translation and by the action listing so that a refusal a caller is told
// about in advance carries the same code as the error it would have got by
// attempting the operation.
func checkoutCode(err error) (Code, bool) {
	switch {
	case errors.Is(err, app.ErrConflict):
		return CodeConflict, true
	case errors.Is(err, app.ErrIntegrity):
		return CodeIntegrity, true
	case errors.Is(err, app.ErrObjectNotCached):
		return CodeNotCached, true
	case errors.Is(err, app.ErrObjectUnavailable), errors.Is(err, objectstore.ErrNotFound), errors.Is(err, app.ErrNotManaged):
		return CodeNotFound, true
	}
	return "", false
}

// translateCheckoutError maps checkout-engine causes to codes, then falls
// back to the shared translation.
func translateCheckoutError(root string, err error) error {
	var be *lock.BusyError
	if errors.As(err, &be) {
		return translateError(root, err)
	}
	if code, ok := checkoutCode(err); ok {
		return WrapError(code, err.Error(), err)
	}
	return translateError(root, err)
}

func (c *Client) itemProgress(op string) app.ProgressFunc {
	return func(p app.ItemProgress) {
		if p.IsStage() {
			c.progress(Event{Operation: op, Kind: EventStage, Stage: Stage(p.Stage), Index: p.Index, Count: p.Count})
			return
		}
		if p.IsPlan() {
			// The plan carries counts, not a list: a consumer that wants the
			// paths gets them as items start, and an operation over hundreds
			// of assets should not build a second copy of the selection just
			// to announce it.
			c.progress(Event{Operation: op, Kind: EventPlan, Phase: Phase(p.PlanPhase), Count: p.Planned, Total: p.PlannedBytes,
				ReusedItems: p.ReusedItems, ReusedBytes: p.ReusedBytes})
			return
		}
		if p.Fetched {
			// The item's data is crossing the network, ahead of the file
			// being produced. Reporting it as a finished item would claim
			// work that has not happened, so it is a transfer-phase progress
			// event and the item count does not move.
			c.progress(Event{Operation: op, Kind: EventItemProgress, Phase: PhaseTransfer,
				Path: p.Path, Digest: p.Digest, Bytes: p.Bytes, Total: p.Total,
				Chunks: p.Chunks, ChunksDone: p.ChunksDone, WireBytes: p.WireBytes,
				PartID: p.PartID, PartBytes: p.PartBytes, PartTotal: p.PartTotal,
				PartWireBytes: p.PartWireBytes, PartIndex: p.PartIndex, PartCount: p.PartCount})
			return
		}
		kind := EventItemProgress
		switch {
		case p.Done:
			kind = EventItemFinished
		case p.Bytes == 0:
			kind = EventItemStarted
		}
		phase := Phase(p.Phase)
		if phase == "" {
			phase = PhaseWrite
		}
		c.progress(Event{Operation: op, Kind: kind, Phase: phase,
			Path: p.Path, Digest: p.Digest, Bytes: p.Bytes, Total: p.Total,
			Chunks: p.Chunks, ChunksDone: p.ChunksDone, WireBytes: p.WireBytes,
			PartID: p.PartID, PartBytes: p.PartBytes, PartTotal: p.PartTotal,
			PartWireBytes: p.PartWireBytes, PartIndex: p.PartIndex, PartCount: p.PartCount})
	}
}

// checkoutTerminalOutcomes closes every planned item that did not already
// emit a successful finish. This is essential progress information rather
// than result decoration: without it, already-correct and refused paths leave
// every consumer's denominator permanently short.
func (c *Client) checkoutTerminalOutcomes(op string, outs []app.PathOutcome) {
	for _, out := range outs {
		if !out.Skipped {
			continue // the app emitted EventItemFinished while writing it
		}
		kind := EventItemSkipped
		if out.Err != nil && !isItemRefusal(out.Err) {
			kind = EventItemFailed
		}
		c.progress(Event{Operation: op, Kind: kind, Path: out.Path, Digest: out.Digest,
			Bytes: out.Bytes, Total: out.Bytes, Message: out.Reason})
	}
}

// AssetState is the checkout consistency state (docs/spec/checkout-consistency.md).
type AssetState string

const (
	AssetUnhydratedClean AssetState = "unhydrated-clean"
	AssetHydratedClean   AssetState = "hydrated-clean"
	AssetHydratedDirty   AssetState = "hydrated-modified"
	AssetConflict        AssetState = "conflict"
	AssetMissing         AssetState = "missing"
)

type StatusEntry struct {
	Path        string     `json:"path"`
	State       AssetState `json:"state"`
	IndexDigest string     `json:"indexDigest,omitempty"`
	// Size is the asset's size as recorded in the index — the size of the
	// content the pointer names, not of the file currently on disk.
	Size int64 `json:"size,omitempty"`
	// WorkingSize is what the working file occupies right now: the asset's
	// real size when it is loaded, a couple of hundred bytes of placeholder
	// when it is not, zero when the file is absent.
	WorkingSize int64 `json:"workingSize,omitempty"`
	// SizeDelta is WorkingSize - Size, the amount an asset grew or shrank
	// locally. It is the number Git cannot give you for a managed asset: a
	// diff sees two placeholders and reports one line changed either way,
	// whatever happened to the 40 MB behind them.
	//
	// It is nil whenever the two sizes are not comparable — every state in
	// which the working file is a placeholder — because subtracting an asset's
	// size from its pointer's would report a large deletion for a file nobody
	// touched. Non-nil zero is a real answer: a change that kept the size.
	SizeDelta *int64 `json:"sizeDelta,omitempty"`
	Cached    bool   `json:"cached"`
	Published bool   `json:"published"`
	// Pending reports an object ingested locally that is not yet published
	// to every requiredForPublication remote.
	Pending bool `json:"pending,omitempty"`
	// Detail explains conflict and missing states.
	Detail string `json:"detail,omitempty"`
	// Actions is what the engine would do to this asset right now: one entry
	// per action, whether it is available, and why not when it is not.
	//
	// It is here so a user interface can enable, disable, and explain its
	// controls without re-deriving the rules from State. Those rules decide
	// things like whether a locally modified file may be replaced by its
	// placeholder, and a second implementation of them — in a process that
	// can destroy work by getting it wrong — is not something to ship. The
	// operations consult the same decision, so an action listed as available
	// is one the operation accepts, and a reason given here is the reason the
	// operation would give.
	Actions []AssetAction `json:"actions,omitempty"`
}

// Action names something the engine can do to an asset.
type Action string

const (
	// ActionLoad materializes the real bytes at the working path.
	ActionLoad Action = "load"
	// ActionUnload replaces the working file with its placeholder.
	ActionUnload Action = "unload"
	// ActionPublish uploads the asset's objects to every required storage.
	// ObjectsPublish with a Selector naming the path performs it.
	ActionPublish Action = "publish"
	// ActionStage records the working file in the Git index.
	//
	// It is the one action with no method on this client: the operation is
	// `git add`, and whether Git can stage a path is Git's business. What it
	// answers is the part Git cannot tell a caller — what the clean filter
	// would make of those bytes on the way in, and therefore whether staging
	// this asset would record anything at all. Staging an unloaded asset is a
	// no-op (the filter passes pointer bytes through unchanged), which is why
	// it reports Done rather than Available.
	ActionStage Action = "stage"
	// ActionDiscard throws away working changes, restoring the working file
	// from the index pointer. Discard performs it.
	ActionDiscard Action = "discard"
)

// AssetAction is one action's availability for one asset.
type AssetAction struct {
	Action Action `json:"action"`
	// Available is whether the operation would act on this asset.
	Available bool `json:"available"`
	// Done means the asset is already in that state — satisfied, not refused.
	// A control for it should read as complete rather than as an error.
	Done bool `json:"done,omitempty"`
	// Reason explains a false Available, in the words the operation uses.
	Reason string `json:"reason,omitempty"`
	// Code classifies a refusal for callers that branch on it rather than
	// display it.
	Code Code `json:"code,omitempty"`
}

type StatusRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
}

type StatusResult struct {
	Entries []StatusEntry `json:"entries"`
	// Skipped lists selected paths that are not managed index entries.
	Skipped []ItemOutcome `json:"skipped,omitempty"`
}

// Status derives the consistency state of every selected managed path from
// the index pointer, the working file, and the stat cache in state.db.
func (c *Client) Status(ctx context.Context, req StatusRequest) (*StatusResult, error) {
	r, err := c.openRepo(ctx, req.Path, "status")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	entries, skipped, err := r.Status(ctx, selection(req.Selector))
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &StatusResult{Entries: make([]StatusEntry, 0, len(entries)), Skipped: itemOutcomes(skipped)}
	if len(res.Skipped) == 0 {
		res.Skipped = nil
	}
	for _, e := range entries {
		entry := StatusEntry{Path: e.Path, State: AssetState(e.State), IndexDigest: e.IndexDigest,
			Size: e.Size, WorkingSize: e.WorkingSize, SizeDelta: e.SizeDelta,
			Cached: e.Cached, Published: e.Published, Pending: e.Pending, Detail: e.Detail}
		for _, a := range e.Actions {
			entry.Actions = append(entry.Actions, AssetAction{
				Action: Action(a.Action), Available: a.Available, Done: a.Done,
				Reason: a.Reason, Code: availabilityCode(a.Err),
			})
		}
		res.Entries = append(res.Entries, entry)
	}
	return res, nil
}

type VerifyRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
	// Remote also checks required-remote availability.
	Remote bool `json:"remote,omitempty"`
}

type VerifyIssue struct {
	Path    string `json:"path"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Digest  string `json:"digest,omitempty"`
}

type VerifyResult struct {
	Checked int           `json:"checked"`
	Issues  []VerifyIssue `json:"issues"`
}

// Verify checks pointer syntax and manifest derivation for every selected
// managed index entry, verifies cached objects byte-for-byte, and with Remote
// probes every requiredForPublication remote. It writes nothing.
func (c *Client) Verify(ctx context.Context, req VerifyRequest) (*VerifyResult, error) {
	r, err := c.openDiagnostic(ctx, req.Path, "verify")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	checked, issues, err := r.Verify(ctx, selection(req.Selector), app.VerifyOptions{Remote: req.Remote})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &VerifyResult{Checked: checked, Issues: make([]VerifyIssue, 0, len(issues))}
	for _, is := range issues {
		code := CodeIntegrity
		switch is.Kind {
		case app.IssueNotFound:
			code = CodeNotFound
		case app.IssueStorage:
			code = CodeStorage
		}
		res.Issues = append(res.Issues, VerifyIssue{Path: is.Path, Code: code, Message: is.Message, Digest: is.Digest})
	}
	return res, nil
}

// ---- diagnostics -----------------------------------------------------------

type DoctorRequest struct {
	Path string `json:"path"`
	// Launcher is the absolute launcher path the Git driver configuration
	// must point at (default: the per-user launcher location).
	Launcher string `json:"launcher,omitempty"`
}

type DoctorFinding struct {
	Path       string `json:"path,omitempty"`
	Kind       string `json:"kind"` // index-worktree | state-derived | publication | cache | git-config
	Message    string `json:"message"`
	Repairable bool   `json:"repairable"`
}

type DoctorResult struct {
	Findings []DoctorFinding `json:"findings"`
}

// Doctor reports index/worktree/state/publication/cache/git-config
// disagreements. It is read-only: it never creates or migrates a database and
// never writes a working file.
func (c *Client) Doctor(ctx context.Context, req DoctorRequest) (*DoctorResult, error) {
	r, err := c.openDiagnostic(ctx, req.Path, "doctor")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	findings, err := r.Doctor(ctx, app.DoctorOptions{Launcher: req.Launcher})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	return &DoctorResult{Findings: doctorFindings(findings)}, nil
}

func doctorFindings(fs []app.Finding) []DoctorFinding {
	out := make([]DoctorFinding, 0, len(fs))
	for _, f := range fs {
		out = append(out, DoctorFinding{Path: f.Path, Kind: f.Kind, Message: f.Message, Repairable: f.Repairable})
	}
	return out
}

type RepairRequest struct {
	Path string `json:"path"`
	// Launcher is the absolute launcher path to (re)install into the Git
	// driver configuration and hook (default: the per-user launcher location).
	Launcher string `json:"launcher,omitempty"`
}

type RepairResult struct {
	Rebuilt []string        `json:"rebuilt"`
	Refused []DoctorFinding `json:"refused"` // ambiguous dirty-file changes are never touched
}

// Repair rebuilds only safe derived state (state.db rows and journal, ledger
// rows for cached objects the index references, pointers for missing working
// files, Git driver configuration and hook) under the checkout-worktree
// lock. Hydrated-modified and conflict paths are refused and listed.
func (c *Client) Repair(ctx context.Context, req RepairRequest) (*RepairResult, error) {
	r, err := c.openRepo(ctx, req.Path, "repair")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rep, err := r.Repair(ctx, app.DoctorOptions{Launcher: req.Launcher})
	res := &RepairResult{Rebuilt: rep.Rebuilt, Refused: doctorFindings(rep.Refused)}
	if res.Rebuilt == nil {
		res.Rebuilt = []string{}
	}
	if err != nil {
		if errors.Is(err, state.ErrCorrupt) {
			return res, WrapError(CodeIntegrity, err.Error(), err)
		}
		return res, translateError(r.Root, err)
	}
	return res, nil
}

// availabilityCode classifies why an action is unavailable. An action that is
// merely done, or that has nothing to do, carries no code — only a reason.
func availabilityCode(err error) Code {
	if err == nil {
		return ""
	}
	if code, ok := checkoutCode(err); ok {
		return code
	}
	return CodeOf(err)
}
