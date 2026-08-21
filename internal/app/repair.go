package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/state"
)

// RepairReport lists what Repair rebuilt and what it refused to touch.
type RepairReport struct {
	Rebuilt []string
	Refused []Finding
}

// Repair rebuilds only safe derived state under the checkout-worktree lock:
// state.db rows from facts (index + working scan), resolution of interrupted
// operations (keep-dirty is never overwritten), ledger rows for cached
// objects the index references, pointer re-materialization for missing
// working files, and the Git driver configuration and pre-push hook. Every
// hydrated-modified or conflict path is refused and listed. No remote is
// contacted while a lock is held.
func (r *Repo) Repair(ctx context.Context, opts DoctorOptions) (RepairReport, error) {
	rep := RepairReport{Rebuilt: []string{}, Refused: []Finding{}}
	h, err := r.Lock(ctx, journal.LockCheckoutWorktree, "")
	if err != nil {
		return rep, err
	}
	var rewritten []string
	err = r.repairLocked(ctx, opts, &rep, &rewritten)
	_ = h.Release()
	// Git re-hashes the rewritten pointers (runs the clean filter): only
	// after every lock is released.
	_ = r.refreshIndexStat(ctx, rewritten)
	return rep, err
}

func (r *Repo) repairLocked(ctx context.Context, opts DoctorOptions, rep *RepairReport, rewritten *[]string) error {

	// A corrupt state.db is derived: drop it so it can be rebuilt from facts.
	if _, err := r.StateIfExists(ctx); err != nil && errors.Is(err, state.ErrCorrupt) {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(r.Paths.StateDB + suffix)
		}
		rep.Rebuilt = append(rep.Rebuilt, "state.db: corrupt database removed")
	}
	d, err := r.diagnose(ctx, opts)
	if err != nil {
		return err
	}
	if d.ledgerErr != nil {
		// Authoritative pending state: never rebuilt here.
		for _, f := range d.findings {
			if f.Kind == FindingPublication && !f.Repairable {
				rep.Refused = append(rep.Refused, f)
			}
		}
	}
	db, err := r.State(ctx)
	if err != nil {
		return err
	}

	// 1. Rebuild every row from facts. Facts were observed by diagnose (with
	// the stat cache) so the transaction stays short.
	facts := make([]state.Fact, 0, len(d.entries))
	for _, e := range d.entries {
		if e.Unmerged {
			continue
		}
		obs := d.obs[e.Path]
		f := state.Fact{Path: e.Path}
		if e.HasPointer {
			f.IndexPointerOID = e.Pointer.OID
		}
		if obs.Present {
			f.Working = state.WorkingFact{Present: true, IsPointer: obs.Row.Intent == state.IntentUnhydrated, Digest: obs.Row.WorkingDigest, Fingerprint: obs.Row.Fingerprint}
		}
		facts = append(facts, f)
	}
	report, err := db.Rebuild(ctx, func(yield func(state.Fact) bool) {
		for _, f := range facts {
			if !yield(f) {
				return
			}
		}
	})
	if err != nil {
		return err
	}
	rep.Rebuilt = append(rep.Rebuilt, fmt.Sprintf("state.db: %d row(s) rebuilt from the index and working tree", report.Rows))
	for _, p := range report.Modified {
		rep.Refused = append(rep.Refused, Finding{Path: p, Kind: FindingIndexWorktree, Message: "working file is modified (hydrated-modified); never overwritten — commit it with git add or discard the change"})
	}
	for _, p := range report.Conflicts {
		if _, missing := isMissing(d, p); missing {
			continue // handled below
		}
		rep.Refused = append(rep.Refused, Finding{Path: p, Kind: FindingIndexWorktree, Message: "conflict: working file and index disagree; resolve with git checkout -- " + p + " or git add"})
	}
	for _, e := range d.entries {
		if e.Unmerged || e.PointerErr != nil {
			msg := "unmerged index entry; resolve the merge first"
			if e.PointerErr != nil {
				msg = "invalid index pointer: " + e.PointerErr.Error()
			}
			rep.Refused = append(rep.Refused, Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: msg})
		}
	}

	// 2. Interrupted operations.
	for _, j := range d.interrupted {
		decision := state.KeepDirty
		if e, ok := d.byPath[j.Path]; ok {
			decision = state.Reconcile(j, e.BlobOID, d.obs[j.Path].Row.Fingerprint)
		} else if absPath, err := r.abs(j.Path); err == nil {
			fp, _ := state.StatFingerprint(absPath)
			decision = state.Reconcile(j, "", fp)
		}
		if decision == state.KeepDirty {
			rep.Refused = append(rep.Refused, Finding{Path: j.Path, Kind: FindingStateDerived, Message: fmt.Sprintf("interrupted %s operation: the working file changed since it started (keep-dirty); left untouched", j.Operation)})
			continue
		}
		if err := db.ResolveInterrupted(ctx, j.OpID, decision); err != nil {
			return err
		}
		rep.Rebuilt = append(rep.Rebuilt, fmt.Sprintf("journal: %s %s resolved (%s)", j.Operation, j.Path, decision))
	}
	// Temporary objects left by aborted operations. Only stale files are
	// removed: a per-path operation in another process may hold a fresh one.
	if ents, err := os.ReadDir(r.Paths.TmpDir); err == nil {
		cutoff := r.Now().Add(-time.Hour)
		for _, ent := range ents {
			if info, err := ent.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(r.Paths.TmpDir, ent.Name()))
			}
		}
	}

	// 3. Missing working files: rewrite the index pointer (unhydrated).
	for _, e := range d.missingPaths {
		if o := r.rewritePointer(ctx, db, e); o.Skipped {
			rep.Refused = append(rep.Refused, Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: o.Reason})
		} else {
			rep.Rebuilt = append(rep.Rebuilt, "worktree: pointer rewritten for missing file "+e.Path)
			*rewritten = append(*rewritten, e.Path)
		}
	}

	// 4. Ledger rows for cached objects the index references.
	if len(d.unrecorded) > 0 && d.ledgerErr == nil {
		l, err := r.Ledger(ctx)
		if err != nil {
			return err
		}
		for _, e := range d.unrecorded {
			p := e.Pointer
			// No remote is contacted while a lock is held: a damaged manifest
			// is reported here, not replaced, because replacing it means
			// fetching.
			content, cerr := r.Content(withoutHealing(ctx), p)
			if cerr != nil {
				rep.Refused = append(rep.Refused, Finding{Path: e.Path, Kind: FindingPublication, Message: "cannot resolve the objects behind " + p.OID + ": " + cerr.Error()})
				continue
			}
			for _, o := range content.Objects {
				// Only cached bytes may be recorded as pending: a pin without
				// an object would promise durability that does not exist.
				if !r.Cache.Has(r.Domain, o.Digest) {
					continue
				}
				if _, recorded, err := l.Object(ctx, o.Digest); err != nil {
					return err
				} else if recorded {
					continue
				}
				if err := r.Cache.Pin(r.Domain, o.Digest); err != nil {
					return err
				}
				row := state.ObjectRecord{Digest: o.Digest, StorageDomainID: p.StorageDomainID, Size: o.Size, Encoding: o.Kind}
				if o.Kind == ObjectKindWhole {
					row.ManifestDigest = p.ManifestDigest
				}
				if err := l.RecordIngest(ctx, row); err != nil {
					return err
				}
				if err := l.AddReferrer(ctx, o.Digest, e.Path, "index"); err != nil {
					return err
				}
				rep.Rebuilt = append(rep.Rebuilt, "publication.db: "+o.Digest+" recorded as pending (referenced by "+e.Path+")")
			}
		}
	}

	// 5. Git configuration and hook.
	if d.configStale {
		if err := filter.Install(ctx, r.Git, d.launcher); err != nil {
			return err
		}
		rep.Rebuilt = append(rep.Rebuilt, "git config: filter/diff/merge drivers point at "+d.launcher)
	}
	if d.hookMissing {
		res, err := filter.InstallPrePushHook(ctx, r.Git, d.launcher)
		if err != nil {
			return err
		}
		rep.Rebuilt = append(rep.Rebuilt, "git hooks: pre-push installed at "+res.Path)
	}
	if d.hookForeign != "" {
		rep.Refused = append(rep.Refused, Finding{Kind: FindingGitConfig, Message: "pre-push hook at " + d.hookForeign + " is not managed by fxvcs; chain the launcher manually (fxvcs doctor prints the line)"})
	}
	return nil
}

func isMissing(d *diagnosis, path string) (IndexEntry, bool) {
	for _, e := range d.missingPaths {
		if e.Path == path {
			return e, true
		}
	}
	return IndexEntry{}, false
}

// rewritePointer materializes the exact index pointer bytes at a missing
// working path through the CAS sequence (expectation: no file, same index
// blob). It runs under the checkout-worktree lock and takes the path lock.
func (r *Repo) rewritePointer(ctx context.Context, db *state.DB, e IndexEntry) PathOutcome {
	dest, err := r.abs(e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	h, err := r.Lock(ctx, journal.LockPath, e.Path)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	defer func() { _ = h.Release() }()
	expected := state.Expectation{IndexOID: e.BlobOID}
	op, err := db.BeginOperation(ctx, state.OpRepair, e.Path, expected)
	if err != nil {
		return skip(e.Path, err.Error(), err)
	}
	enc, err := e.Pointer.Encode()
	if err != nil {
		_ = op.Abort(ctx, err.Error())
		return skip(e.Path, err.Error(), err)
	}
	tmp, err := r.tmpFile(dest, false)
	if err != nil {
		_ = op.Abort(ctx, err.Error())
		return skip(e.Path, err.Error(), err)
	}
	if err := prepareTmp(tmp, func(w io.Writer) error { _, e := w.Write(enc); return e }); err != nil {
		_ = op.Abort(ctx, err.Error())
		return skip(e.Path, err.Error(), err)
	}
	if o, ok := r.recheck(ctx, e.Path, dest, expected, op); !ok {
		_ = os.Remove(tmp.Name())
		return o
	}
	if err := r.renameOver(tmp.Name(), dest); err != nil {
		_ = op.Abort(ctx, err.Error())
		return skip(e.Path, err.Error(), err)
	}
	fp, _ := state.StatFingerprint(dest)
	final := state.PathRow{Path: e.Path, Intent: state.IntentUnhydrated, IndexPointerDigest: e.Pointer.OID, WorkingDigest: e.Pointer.OID, Fingerprint: fp, LastVerifiedNS: r.Now().UnixNano()}
	if err := op.Commit(ctx, final); err != nil {
		return skip(e.Path, err.Error(), err)
	}
	return PathOutcome{Path: e.Path, Digest: e.Pointer.OID, Bytes: int64(len(enc))}
}
