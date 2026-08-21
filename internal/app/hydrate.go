package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// Test hooks (package-private): they let tests interpose between CAS steps
// without touching production paths. Nil in production.
var (
	// hydrateBeforeRecheck runs after the temporary object is written and
	// before the index/fingerprint re-check (CAS step 5).
	hydrateBeforeRecheck func(path string)
	// hydrateWriteHook wraps the temporary-file writer (interrupted-write
	// injection).
	hydrateWriteHook func(path string, w io.Writer) io.Writer
)

const progressStep = 8 << 20

// Hydrate materializes the object behind each selected index pointer at its
// working path following the compare-and-swap sequence of
// docs/spec/checkout-consistency.md §2. It never overwrites a working file
// whose clean result differs from the index pointer (hydrated-modified) and
// never leaves a partially written destination.
func (r *Repo) Hydrate(ctx context.Context, sel Selection, progress ProgressFunc) ([]PathOutcome, error) {
	entries, missing, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, err
	}
	var out []PathOutcome
	for _, m := range missing {
		out = append(out, skip(m, "not in the index or not managed by fxvcs (filter=fxvcs)", ErrNotManaged))
	}
	planned := len(entries) + len(missing)
	progress.emit(planOf(entries, planned, ProgressPhaseWrite))
	// Fetch what the whole selection needs before writing any of it: the
	// downloads are independent and network-bound, while the writes are
	// compare-and-swap against the index and stay one path at a time.
	reuse := r.prefetchSelection(ctx, entries, planned, progress)
	progress.emit(ItemProgress{Stage: StageWriting, Count: len(entries)})
	done, consistent, cerr := r.eachPath(ctx, entries, progress, func(ctx context.Context, e IndexEntry, p ProgressFunc) (PathOutcome, bool) {
		o, ok := r.hydrateOne(ctx, e, p)
		o.Reuse = reuse[e.Path]
		return o, ok
	})
	out = append(out, done...)
	if cerr != nil {
		_ = r.refreshIndexStat(context.WithoutCancel(ctx), consistent)
		return out, cerr
	}
	_ = r.refreshIndexStat(ctx, consistent)
	return out, nil
}

// pathWorkers bounds how many working files are written at once.
//
// The compare-and-swap sequence is per path and takes that path's lock, so
// paths are independent. What they share is this clone's SQLite connection,
// and the journal writes that go through it are short; the work that is not
// short — reading objects, hashing them, writing and fsyncing a file — is what
// overlaps, and it is most of the wall clock of an operation over many assets.
//
// Eight rather than one per core: the limit is the disk's appetite for
// concurrent writes and fsyncs, not arithmetic, and a machine running a game
// server has other things to do with its cores.
const pathWorkers = 8

// eachPath runs do over the selection and reports, alongside the outcomes,
// which paths the working tree now provably agrees with the index about —
// what refreshIndexStat is allowed to name. do says so per path, because
// "this path is consistent" and "this path was written" are different
// questions: an asset that was already in the requested state was not
// written, and its stat cache is exactly as likely to be stale.
func (r *Repo) eachPath(ctx context.Context, entries []IndexEntry, progress ProgressFunc,
	do func(context.Context, IndexEntry, ProgressFunc) (PathOutcome, bool),
) (out []PathOutcome, consistent []string, err error) {
	var emitMu sync.Mutex
	serial := progress
	if progress != nil {
		serial = func(ev ItemProgress) {
			emitMu.Lock()
			defer emitMu.Unlock()
			progress(ev)
		}
	}
	results := make([]PathOutcome, len(entries))
	agrees := make([]bool, len(entries))
	ran := make([]bool, len(entries))
	sem := make(chan struct{}, pathWorkers)
	var wg sync.WaitGroup
	for i, e := range entries {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e IndexEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			wctx := withLockSet(ctx, r.NewLockSet())
			results[i], agrees[i] = do(wctx, e, serial)
			ran[i] = true
		}(i, e)
	}
	wg.Wait()
	for i := range entries {
		if !ran[i] {
			continue
		}
		out = append(out, results[i])
		if agrees[i] {
			consistent = append(consistent, results[i].Path)
		}
	}
	return out, consistent, ctx.Err()
}

// planOf is the operation-wide plan event: how many assets are about to be
// processed and how many bytes they hold.
func planOf(entries []IndexEntry, planned int, phase string) ItemProgress {
	plan := ItemProgress{Planned: planned, PlanPhase: phase}
	for _, e := range entries {
		if e.HasPointer {
			plan.PlannedBytes += e.Pointer.Size
		}
	}
	return plan
}

// prefetchSelection warms the cache for every entry that will need object
// bytes, and reports which asset each arriving object belongs to so the
// operation shows movement while it downloads.
//
// Entries whose content is already local are left out, which is what makes a
// re-run of `hydrate` cost nothing. Entries that will turn out to be skipped
// for another reason (a modified working file) may have their objects fetched
// unnecessarily; that costs bandwidth in an uncommon case and never
// correctness, whereas observing every path twice to find out would cost a
// re-hash of every large file.
// Stages of a load. They are named because they behave differently: resolving
// is index work plus small metadata reads, transferring is bound by the link,
// and writing is bound by the disk and is serialized per path.
const (
	StageResolving        = "resolving"
	StageTransferring     = "downloading"
	StageWriting          = "writing"
	StageVerifying        = "verifying"
	ProgressPhaseTransfer = "transfer"
	ProgressPhaseWrite    = "write"
	ProgressPhaseVerify   = "verify"
)

// Reuse values for PathOutcome and ItemProgress. Empty means the content was
// transferred during this operation.
const (
	// ReuseCached: every object was already in the local object cache.
	ReuseCached = "cached"
	// ReuseShared: identical content to another asset in the same operation,
	// so one transfer served both. This is content addressing paying for
	// itself, and it is worth saying out loud.
	ReuseShared = "shared"
)

func (r *Repo) prefetchSelection(ctx context.Context, entries []IndexEntry, planned int, progress ProgressFunc) map[string]string {
	// One pass, one entry per distinct pointer, one object counted once.
	//
	// Both halves of that matter. A repository duplicates content constantly —
	// the same font, icon, or animation vendored into a dozen resources — so
	// the same pointer appears under many paths. Walking the pointers instead
	// of the entries counted a duplicated file's objects once per copy, which
	// left its row claiming to need N times what it actually downloads: it sat
	// at exactly 1/N forever, never completed, and never gave up its place on
	// screen while everything else scrolled past underneath.
	//
	// Deduplicating also removes real work: one download serves every path
	// that shares the content.
	// Resolve first, and concurrently. A chunked asset's chunk list lives in
	// its manifest object, so classifying the selection needs every manifest
	// present. Reading them one at a time — which is what asking for each
	// asset's content in turn does — is one network round trip per asset
	// before a single chunk is fetched: on a few hundred assets that is a
	// minute of an operation that appears to be doing nothing at all.
	progress.emit(ItemProgress{Stage: StageResolving, Count: len(entries)})
	var manifests []string
	seenManifest := map[string]bool{}
	for _, e := range entries {
		if _, ok := pointerEntryUsable(e); !ok {
			continue
		}
		d := e.Pointer.ManifestDigest
		if e.Pointer.Encoding == pointer.EncodingWhole || seenManifest[d] || r.Cache.Has(r.Domain, d) {
			continue
		}
		seenManifest[d] = true
		manifests = append(manifests, d)
	}
	if len(manifests) > 0 {
		resolved := 0
		_ = r.fetchMany(ctx, manifests, false, func(string) {
			resolved++
			progress.emit(ItemProgress{Stage: StageResolving, Index: resolved, Count: len(manifests)})
		})
	}

	var ptrs []pointer.Pointer
	owner := map[string]IndexEntry{} // object digest -> the asset it is shown under
	size := map[string]int64{}       // object digest -> bytes to fetch
	total := map[string]int64{}      // asset path -> bytes to fetch
	parts := map[string]int{}        // asset path -> objects to fetch
	partIndex := map[string]int{}    // object digest -> stable position in asset
	seenPointer := map[string]bool{}
	reuse := map[string]string{}
	var downloadBytes, reusedBytes int64
	for _, e := range entries {
		if _, ok := pointerEntryUsable(e); !ok {
			continue
		}
		switch {
		case r.HasContent(ctx, e.Pointer):
			reuse[e.Path] = ReuseCached
			reusedBytes += e.Pointer.Size
			continue
		case seenPointer[e.Pointer.OID]:
			// Another asset in this same operation has identical content, so
			// its download serves this path too.
			reuse[e.Path] = ReuseShared
			reusedBytes += e.Pointer.Size
			continue
		}
		seenPointer[e.Pointer.OID] = true
		c, err := r.Content(ctx, e.Pointer)
		if err != nil {
			continue // reported per path when the file is materialized
		}
		ptrs = append(ptrs, e.Pointer)
		for _, o := range c.Objects {
			// Count only what will actually be fetched: an object already
			// cached, or already claimed by another asset, is never reported
			// again and would leave this row permanently short of its total.
			if _, taken := owner[o.Digest]; taken || r.Cache.Has(r.Domain, o.Digest) {
				continue
			}
			owner[o.Digest] = e
			size[o.Digest] = o.Size
			total[e.Path] += o.Size
			parts[e.Path]++
			partIndex[o.Digest] = parts[e.Path]
			downloadBytes += o.Size
		}
	}
	// Restate the plan in the terms the byte figure now uses: what has to be
	// transferred, which is less than what will be written whenever content is
	// duplicated or partly cached. Without this the summary counts downloaded
	// bytes towards a written-bytes total and can never reach the end.
	progress.emit(ItemProgress{
		Planned: planned, PlannedBytes: downloadBytes, PlanPhase: ProgressPhaseTransfer,
		ReusedItems: len(reuse), ReusedBytes: reusedBytes,
	})
	if len(ptrs) == 0 {
		return reuse
	}
	progress.emit(ItemProgress{Stage: StageTransferring, Count: len(ptrs)})

	var mu sync.Mutex
	got := map[string]int64{}
	wire := map[string]int64{}
	partGot := map[string]int64{}
	partWire := map[string]int64{}
	done := map[string]int{}
	// report is the per-asset transfer state as it stands now.
	report := func(e IndexEntry, digest string) ItemProgress {
		return ItemProgress{
			Path: e.Path, Digest: e.Pointer.OID,
			Bytes: got[e.Path], Total: total[e.Path],
			Chunks: parts[e.Path], ChunksDone: done[e.Path], Fetched: true,
			Phase: ProgressPhaseTransfer, WireBytes: wire[e.Path],
			PartID: digest, PartBytes: partGot[digest], PartTotal: size[digest], PartWireBytes: partWire[digest],
			PartIndex: partIndex[digest], PartCount: parts[e.Path],
		}
	}
	// Fetches run concurrently, so the emit itself is serialized: ProgressFunc
	// promises consumers one call at a time, and reporting from six goroutines
	// would quietly break that for anything reading the stream directly.
	emit := func(digest string, chunkDone bool, delta int64) {
		mu.Lock()
		defer mu.Unlock()
		e, ok := owner[digest]
		if !ok {
			return
		}
		if chunkDone {
			done[e.Path]++
		}
		got[e.Path] += delta
		partGot[digest] += delta
		if delta > 0 {
			wire[e.Path] += delta
			partWire[digest] += delta
		}
		ev := report(e, digest)
		ev.PartBytes = partGot[digest]
		ev.WireBytes = wire[e.Path]
		progress.emit(ev)
	}
	r.PrefetchProgress(ctx, ptrs,
		func(digest string) { emit(digest, true, 0) },
		func(digest string, delta int64) { emit(digest, false, delta) },
	)
	return reuse
}

// pointerEntryUsable reports the common per-item refusals shared by hydrate
// and dehydrate for an index entry.
func pointerEntryUsable(e IndexEntry) (PathOutcome, bool) {
	switch {
	case e.Unmerged:
		return skip(e.Path, "path is unmerged; resolve the conflict first (git checkout --ours/--theirs, git add)", ErrConflict), false
	case e.PointerErr != nil:
		return skip(e.Path, "index pointer is invalid: "+e.PointerErr.Error(), e.PointerErr), false
	case !e.HasPointer:
		return skip(e.Path, "index blob is not an fxvcs pointer (not migrated); run fxvcs migrate", nil), false
	}
	return PathOutcome{}, true
}

// hydrateOne materializes one asset. Its second result is what eachPath
// documents: whether the working file is now known to clean to the index
// pointer, which is true both when this call wrote it and when it was already
// hydrated, and false for every refusal.
func (r *Repo) hydrateOne(ctx context.Context, e IndexEntry, progress ProgressFunc) (PathOutcome, bool) {
	if o, ok := pointerEntryUsable(e); !ok {
		return o, false
	}
	// Step 1: observe the working file (with the stat cache) and retain the
	// expectations. Anything but a clean pointer or an absent file is refused
	// before any object I/O.
	db, obs, o, ok := r.observeEntry(ctx, e)
	if !ok {
		return o, false
	}
	// The same decision the listing reports (see actions.go): an interface
	// that offered this action was told it would be accepted.
	switch av := loadAvailability(e, obs); {
	case av.Done:
		_ = r.refreshRow(ctx, db, obs)
		// Done means the observation proved WorkingDigest is the index
		// pointer, so Git's stat cache may name this path as modified while
		// nothing about it has changed. Nothing here will write it, and
		// re-running the command would skip it again, so this is the only
		// opportunity to say so.
		return PathOutcome{Path: e.Path, Digest: e.Pointer.OID, Bytes: e.Pointer.Size, Skipped: true, Reason: av.Reason}, true
	case !av.Available:
		return skip(e.Path, av.Reason, av.Err), false
	}
	o = r.materialize(ctx, e, obs, db, state.OpHydrate, progress)
	return o, !o.Skipped
}

// observeEntry is CAS step 1: open the state database, read the recorded row,
// and derive the current observation of the working file. It is shared by every
// per-path mutation so they all judge the same picture.
func (r *Repo) observeEntry(ctx context.Context, e IndexEntry) (*state.DB, observation, PathOutcome, bool) {
	db, err := r.State(ctx)
	if err != nil {
		return nil, observation{}, skip(e.Path, err.Error(), err), false
	}
	var prev *state.PathRow
	if row, ok, err := db.Get(ctx, e.Path); err != nil {
		return nil, observation{}, skip(e.Path, err.Error(), err), false
	} else if ok {
		prev = &row
	}
	obs, err := r.observePath(e.Path, e.Pointer.OID, prev)
	if err != nil {
		return nil, observation{}, skip(e.Path, err.Error(), err), false
	}
	return db, obs, PathOutcome{}, true
}

// materialize is CAS steps 2-7 for writing the object bytes of the index
// pointer at the working path. The caller has already decided that this is
// allowed; op names what the journal records, because crash recovery cares
// what was being written and not which command asked for it.
// materialize writes the content at the working path, healing a corrupt
// cached object once if it meets one.
func (r *Repo) materialize(ctx context.Context, e IndexEntry, obs observation, db *state.DB, op state.Operation, progress ProgressFunc) PathOutcome {
	o, corrupt := r.materializeAttempt(ctx, e, obs, db, op, progress, true)
	if corrupt == nil {
		return o
	}
	// The path lock and the journal operation are both released by now: the
	// attempt aborted and returned rather than fetching while holding them,
	// because a fetch is an unbounded network request and the compare-and-swap
	// sequence does not hold locks across one.
	healed, herr := r.repairCorrupt(ctx, corrupt)
	if !healed {
		reason := "write temporary object: " + corrupt.Error()
		if herr != nil {
			reason += " (refetch failed: " + herr.Error() + ")"
		}
		return skip(e.Path, reason, corrupt)
	}
	// The premise is re-observed: the working tree may have moved while the
	// object was refetched. One retry only — a second corruption is a failing
	// disk, not a transient.
	db2, obs2, o2, ok := r.observeEntry(ctx, e)
	if !ok {
		return o2
	}
	o2, _ = r.materializeAttempt(ctx, e, obs2, db2, op, progress, false)
	return o2
}

func (r *Repo) materializeAttempt(ctx context.Context, e IndexEntry, obs observation, db *state.DB, op state.Operation, progress ProgressFunc, mayRepair bool) (PathOutcome, error) {
	ptr := e.Pointer
	dest, err := r.abs(e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err), nil
	}
	expected := state.Expectation{IndexOID: e.BlobOID, Fingerprint: obs.Row.Fingerprint}
	progress.emit(ItemProgress{Path: e.Path, Digest: ptr.OID, Total: ptr.Size})

	// Step 3 (network part) happens before the path lock: resolve the object
	// manifest and fetch every object the pointer needs into the cache,
	// verified while streaming.
	if _, err := r.EnsureContent(ctx, ptr, nil); err != nil {
		return skip(e.Path, err.Error(), err), nil
	}

	// Step 2: path lock. Bounded local work only from here on.
	h, err := r.Lock(ctx, journal.LockPath, e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err), nil
	}
	defer func() { _ = h.Release() }()
	jop, err := db.BeginOperation(ctx, op, e.Path, expected)
	if err != nil {
		return skip(e.Path, err.Error(), err), nil
	}
	abort := func(reason string, cause error) PathOutcome {
		_ = jop.Abort(ctx, reason)
		return skip(e.Path, reason, cause)
	}

	// Steps 3-4: stream the cached objects into a temporary file through the
	// verifying reader (every chunk against its digest, the whole
	// reconstruction against the pointer oid and size at EOF).
	// Under the path lock: report damage rather than fetching a replacement,
	// which the caller does after releasing.
	rd, err := r.OpenContent(withoutHealing(ctx), ptr)
	if err != nil {
		if mayRepair && isRepairable(err) {
			_ = jop.Abort(ctx, "cached object was corrupt; refetching")
			return PathOutcome{}, err
		}
		return abort("object "+ptr.OID+" is not available locally: "+err.Error(), err), nil
	}
	defer rd.Close()
	tmp, err := r.tmpFile(dest, false)
	if err != nil {
		return abort(err.Error(), err), nil
	}
	var written int64
	writeErr := prepareTmp(tmp, func(w io.Writer) error {
		if hydrateWriteHook != nil {
			w = hydrateWriteHook(e.Path, w)
		}
		pw := &progressWriter{w: w, step: progressStep, report: func(n int64) {
			progress.emit(ItemProgress{Path: e.Path, Digest: ptr.OID, Bytes: n, Total: ptr.Size})
		}}
		n, err := io.Copy(pw, rd)
		written = n
		if err != nil {
			if errors.Is(err, cache.ErrCorrupt) {
				return fmt.Errorf("%w: %w", ErrIntegrity, err)
			}
			return err
		}
		return nil
	})
	if writeErr != nil {
		// A corrupt cached object used to be caught before the write, by
		// re-hashing every object behind every pointer on every load. It is
		// caught here instead, which costs nothing until it actually happens
		// — and then heals, so a load still recovers from a damaged cache
		// exactly as docs/spec/failure-semantics.md promises.
		if mayRepair && isRepairable(writeErr) {
			// Hand it back for the caller to heal once the lock is released.
			_ = os.Remove(tmp.Name())
			_ = jop.Abort(ctx, "cached object was corrupt; refetching")
			return PathOutcome{}, writeErr
		}
		return abort("write temporary object: "+writeErr.Error(), writeErr), nil
	}
	_ = jop.Advance(ctx, state.PhaseFetched)
	_ = jop.Advance(ctx, state.PhaseVerified)
	if hydrateBeforeRecheck != nil {
		hydrateBeforeRecheck(e.Path)
	}
	// Step 5-6: re-check the premise under the lock.
	if o, ok := r.recheck(ctx, e.Path, dest, expected, jop); !ok {
		_ = os.Remove(tmp.Name())
		return o, nil
	}
	// Step 7: atomic replace, then commit row + journal in one transaction.
	if err := r.renameOver(tmp.Name(), dest); err != nil {
		return abort("replace working file: "+err.Error(), err), nil
	}
	fp, _ := state.StatFingerprint(dest)
	final := state.PathRow{Path: e.Path, Intent: state.IntentHydrated, IndexPointerDigest: ptr.OID, WorkingDigest: ptr.OID, Fingerprint: fp, LastVerifiedNS: r.Now().UnixNano()}
	if err := jop.Commit(ctx, final); err != nil {
		return skip(e.Path, "working file replaced but state row not committed: "+err.Error(), err), nil
	}
	progress.emit(ItemProgress{Path: e.Path, Digest: ptr.OID, Bytes: written, Total: ptr.Size, Done: true})
	return PathOutcome{Path: e.Path, Digest: ptr.OID, Bytes: written}, nil
}

// isRepairable reports that an error names a specific cached object, which is
// what makes evicting and refetching a targeted act rather than a guess.
func isRepairable(err error) bool {
	var ce *cache.CorruptError
	return errors.As(err, &ce) && ce.Digest != ""
}

// repairCorrupt evicts and refetches the object an error names.
//
// Verification happens while bytes stream, so the digest under suspicion is
// several frames away from whoever can act on it; cache.CorruptError carries
// it out. healed reports that the object was replaced and the operation is
// worth retrying. A cache with no remote to refetch from heals nothing, and
// the original integrity error stands.
func (r *Repo) repairCorrupt(ctx context.Context, err error) (healed bool, ferr error) {
	return r.repairCorruptProgress(ctx, err, nil)
}

func (r *Repo) repairCorruptProgress(ctx context.Context, err error, onBytes func(int64)) (healed bool, ferr error) {
	var ce *cache.CorruptError
	if !errors.As(err, &ce) || ce.Digest == "" {
		return false, nil
	}
	// The damaged object is not evicted first: FetchObject writes the
	// replacement over it once the bytes have arrived, so a fetch with nowhere
	// to fetch from leaves the object and its pin alone instead of destroying
	// the evidence and the pin together.
	if ferr := r.fetchObject(ctx, ce.Digest, onBytes); ferr != nil {
		return false, ferr
	}
	return true, nil
}

// recheck is CAS step 5/6: it re-reads the index blob and the working
// fingerprint and aborts the operation when either moved.
func (r *Repo) recheck(ctx context.Context, path, dest string, expected state.Expectation, op *state.Op) (PathOutcome, bool) {
	oid, err := r.indexBlobOID(ctx, path)
	if err != nil {
		_ = op.Abort(ctx, "re-read index: "+err.Error())
		return skip(path, err.Error(), err), false
	}
	fp, err := state.StatFingerprint(dest)
	if err != nil {
		_ = op.Abort(ctx, "re-stat working file: "+err.Error())
		return skip(path, err.Error(), err), false
	}
	if fp != expected.Fingerprint {
		_ = op.Abort(ctx, string(state.KeepDirty))
		return skip(path, "working file changed during the operation; nothing was written", ErrConflict), false
	}
	if oid != expected.IndexOID {
		_ = op.Abort(ctx, string(state.IndexMoved))
		return skip(path, "index entry changed during the operation; nothing was written", ErrConflict), false
	}
	return PathOutcome{}, true
}

// ensureCached makes sure the object is in the cache and verifies, fetching
// from configured remotes when needed. No lock is held.
func (r *Repo) ensureCached(ctx context.Context, digest string) error {
	return r.ensureCachedProgress(ctx, digest, nil)
}

// ensureCachedProgress is ensureCached with byte-level fetch reporting.
func (r *Repo) ensureCachedProgress(ctx context.Context, digest string, onBytes func(int64)) error {
	// Presence only. This function has no recorded size to compare against —
	// its callers pass a bare digest, and a manifest's own size is not written
	// down anywhere before it is fetched — but every caller then reads the
	// object through the verifying reader (loadManifest full-reads it,
	// materialize streams it), so a bad entry is caught there and repaired by
	// repairCorrupt rather than by re-hashing every object on every load.
	if r.Cache.Has(r.Domain, digest) {
		return nil
	}
	if err := r.fetchObject(ctx, digest, onBytes); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, objectstore.ErrNotFound) {
			return fmt.Errorf("%w: object %s is not cached and no configured remote has it; run fxvcs objects publish on the machine that added it", ErrObjectUnavailable, digest)
		}
		if errors.Is(err, cache.ErrCorrupt) {
			// A remote answered, with the wrong bytes. That is a broken or
			// misconfigured remote, not a missing object, and reporting it as
			// missing sends the operator to publish something that is already
			// there.
			return fmt.Errorf("%w: object %s could not be fetched: %w", ErrIntegrity, digest, err)
		}
		return fmt.Errorf("%w: object %s is not cached and could not be fetched: %w", ErrObjectUnavailable, digest, err)
	}
	return nil
}

// refreshRow writes a re-derived row (stat-cache refresh) under the path
// lock when it is free; a busy lock means an operation is rewriting it.
func (r *Repo) refreshRow(ctx context.Context, db *state.DB, obs observation) error {
	if !obs.Rehashed {
		return nil
	}
	h, err := r.TryLock(ctx, journal.LockPath, obs.Row.Path)
	if err != nil {
		return nil
	}
	defer func() { _ = h.Release() }()
	return db.Upsert(ctx, obs.Row)
}

// Dehydrate replaces each selected hydrated-clean working file with the
// exact pointer bytes of its index entry. A file whose clean result differs
// from the index pointer (hydrated-modified) is refused, as is a file whose
// object is neither cached (and pinned or published) nor re-ingestible.
func (r *Repo) Dehydrate(ctx context.Context, sel Selection, progress ProgressFunc) ([]PathOutcome, error) {
	entries, missing, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, err
	}
	var out []PathOutcome
	for _, m := range missing {
		out = append(out, skip(m, "not in the index or not managed by fxvcs (filter=fxvcs)", ErrNotManaged))
	}
	progress.emit(planOf(entries, len(entries)+len(missing), ProgressPhaseVerify))
	progress.emit(ItemProgress{Stage: StageVerifying, Count: len(entries)})
	done, consistent, cerr := r.eachPath(ctx, entries, progress, r.dehydrateOne)
	out = append(out, done...)
	if cerr != nil {
		_ = r.refreshIndexStat(context.WithoutCancel(ctx), consistent)
		return out, cerr
	}
	_ = r.refreshIndexStat(ctx, consistent)
	return out, nil
}

// dehydrateOne replaces one asset with its placeholder. The second result is
// hydrateOne's: whether the working file provably agrees with the index.
func (r *Repo) dehydrateOne(ctx context.Context, e IndexEntry, progress ProgressFunc) (PathOutcome, bool) {
	if o, ok := pointerEntryUsable(e); !ok {
		return o, false
	}
	db, obs, o, ok := r.observeEntry(ctx, e)
	if !ok {
		return o, false
	}
	switch av := unloadAvailability(e, obs); {
	case av.Done:
		_ = r.refreshRow(ctx, db, obs)
		// Already a pointer, and proved to be the index pointer: see
		// hydrateOne for why a skip still refreshes Git's stat cache.
		return PathOutcome{Path: e.Path, Digest: e.Pointer.OID, Skipped: true, Reason: av.Reason}, true
	case !av.Available:
		return skip(e.Path, av.Reason, av.Err), false
	}
	dest, err := r.abs(e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err), false
	}
	// The bytes about to be removed from the working tree must be recoverable:
	// cached and protected (pinned or published), else re-ingested from the
	// file itself (idempotent, verifies the digest). Discard is the one caller
	// that skips this, because destroying those bytes is what it was asked to
	// do.
	progress.emit(ItemProgress{Path: e.Path, Digest: e.Pointer.OID, Total: e.Pointer.Size, Phase: ProgressPhaseVerify})
	if err := r.ensureRecoverable(ctx, e.Pointer, dest, e.Path, func(n int64) {
		progress.emit(ItemProgress{Path: e.Path, Digest: e.Pointer.OID, Bytes: n, Total: e.Pointer.Size, Phase: ProgressPhaseVerify})
	}); err != nil {
		return skip(e.Path, err.Error(), err), false
	}
	o = r.placeholder(ctx, e, obs, db, state.OpDehydrate, progress)
	return o, !o.Skipped
}

// placeholder is CAS steps 2-7 for writing the index pointer bytes at the
// working path. Unlike materialize it needs no object I/O: the bytes it writes
// are the index entry itself.
func (r *Repo) placeholder(ctx context.Context, e IndexEntry, obs observation, db *state.DB, op state.Operation, progress ProgressFunc) PathOutcome {
	ptr := e.Pointer
	dest, err := r.abs(e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	expected := state.Expectation{IndexOID: e.BlobOID, Fingerprint: obs.Row.Fingerprint}
	progress.emit(ItemProgress{Path: e.Path, Digest: ptr.OID, Total: ptr.Size})
	h, err := r.Lock(ctx, journal.LockPath, e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	defer func() { _ = h.Release() }()
	jop, err := db.BeginOperation(ctx, op, e.Path, expected)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	abort := func(reason string, cause error) PathOutcome {
		_ = jop.Abort(ctx, reason)
		return skip(e.Path, reason, cause)
	}
	enc, err := ptr.Encode()
	if err != nil {
		return abort(err.Error(), err)
	}
	tmp, err := r.tmpFile(dest, false)
	if err != nil {
		return abort(err.Error(), err)
	}
	if err := prepareTmp(tmp, func(w io.Writer) error { _, e := w.Write(enc); return e }); err != nil {
		return abort("write temporary pointer: "+err.Error(), err)
	}
	_ = jop.Advance(ctx, state.PhaseVerified)
	if hydrateBeforeRecheck != nil {
		hydrateBeforeRecheck(e.Path)
	}
	if o, ok := r.recheck(ctx, e.Path, dest, expected, jop); !ok {
		_ = os.Remove(tmp.Name())
		return o
	}
	if err := r.renameOver(tmp.Name(), dest); err != nil {
		return abort("replace working file: "+err.Error(), err)
	}
	fp, _ := state.StatFingerprint(dest)
	final := state.PathRow{Path: e.Path, Intent: state.IntentUnhydrated, IndexPointerDigest: ptr.OID, WorkingDigest: ptr.OID, Fingerprint: fp, LastVerifiedNS: r.Now().UnixNano()}
	if err := jop.Commit(ctx, final); err != nil {
		return skip(e.Path, "working file replaced but state row not committed: "+err.Error(), err)
	}
	// The byte figure is the working-tree space reclaimed, not the couple of
	// hundred bytes of pointer written in its place. Reporting the pointer
	// size against the asset size puts two unrelated quantities either side
	// of the same ratio, so a display counts kibibytes towards a mebibyte
	// total and progress looks stalled. Reclaimed space is also the number a
	// person unloading assets actually wants.
	progress.emit(ItemProgress{Path: e.Path, Digest: ptr.OID, Bytes: ptr.Size, Total: ptr.Size, Done: true})
	return PathOutcome{Path: e.Path, Digest: ptr.OID, Bytes: ptr.Size}
}
