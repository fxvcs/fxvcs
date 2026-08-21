package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/state"
)

// Finding kinds (sdk.DoctorFinding.Kind).
const (
	FindingIndexWorktree = "index-worktree" // state table: hydrated-modified, conflict, missing
	FindingStateDerived  = "state-derived"  // state.db row/journal disagrees with facts, or is unusable
	FindingPublication   = "publication"    // ledger vs cache vs index disagreements
	FindingCache         = "cache"          // cached object disagrees with its pointer
	FindingGitConfig     = "git-config"     // driver config / hook missing or stale
)

// Finding is one doctor result.
type Finding struct {
	Path       string
	Kind       string
	Message    string
	Repairable bool
}

// DoctorOptions configures Doctor and Repair.
type DoctorOptions struct {
	// Launcher is the absolute launcher path the Git configuration must
	// point at ("" = the recorded path, with exact legacy-default migration,
	// else the canonical per-user default).
	Launcher string
}

// diagnosis is the read-only fact set shared by Doctor and Repair.
type diagnosis struct {
	entries      []IndexEntry
	byPath       map[string]IndexEntry
	rows         map[string]state.PathRow
	obs          map[string]observation
	stateDB      *state.DB // nil when absent or unusable
	stateErr     error     // open error when unusable (corrupt, needs migration, newer)
	ledger       *state.Ledger
	ledgerErr    error
	interrupted  []state.JournalRow
	findings     []Finding
	launcher     string
	configStale  bool
	hookMissing  bool
	hookForeign  string
	unrecorded   []IndexEntry // cached objects referenced by the index without a ledger row
	missingPaths []IndexEntry // present pointer, absent working file
}

func (d *diagnosis) add(f Finding) { d.findings = append(d.findings, f) }

// diagnose collects every fact read-only. It never creates or migrates a
// database and never writes a working file.
func (r *Repo) diagnose(ctx context.Context, opts DoctorOptions) (*diagnosis, error) {
	d := &diagnosis{byPath: map[string]IndexEntry{}, rows: map[string]state.PathRow{}, obs: map[string]observation{}}
	entries, _, err := r.ManagedEntries(ctx, Selection{})
	if err != nil {
		return nil, err
	}
	d.entries = entries
	for _, e := range entries {
		d.byPath[e.Path] = e
	}
	// state.db
	db, err := r.StateIfExists(ctx)
	switch {
	case err == nil:
		d.stateDB = db
		list, err := db.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range list {
			d.rows[row.Path] = row
		}
		d.interrupted, err = db.ListInterrupted(ctx)
		if err != nil {
			return nil, err
		}
	case errors.Is(err, state.ErrNotFound):
	default:
		d.stateErr = err
		var oe *state.OpenError
		msg := "state.db is unusable: " + err.Error()
		if errors.As(err, &oe) {
			msg = "state.db: " + oe.Err.Error() + "; " + oe.Guidance.Message
		}
		d.add(Finding{Kind: FindingStateDerived, Message: msg, Repairable: errors.Is(err, state.ErrCorrupt)})
	}
	// publication.db
	l, err := r.LedgerIfExists(ctx)
	switch {
	case err == nil:
		d.ledger = l
	case errors.Is(err, state.ErrNotFound):
	default:
		d.ledgerErr = err
		var oe *state.OpenError
		msg := "publication.db is unusable: " + err.Error()
		if errors.As(err, &oe) {
			msg = "publication.db: " + oe.Err.Error() + "; " + oe.Guidance.Message
		}
		d.add(Finding{Kind: FindingPublication, Message: msg, Repairable: false})
	}
	required := r.RequiredRemotes()

	// Per path: facts vs rows.
	referenced := map[string]bool{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var prev *state.PathRow
		if row, ok := d.rows[e.Path]; ok {
			prev = &row
		}
		indexOID := ""
		if e.HasPointer {
			indexOID = e.Pointer.OID
		}
		obs, err := r.observePath(e.Path, indexOID, prev)
		if err != nil {
			return nil, err
		}
		d.obs[e.Path] = obs
		switch {
		case e.Unmerged:
			d.add(Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: "unmerged index entry (conflict); resolve with git checkout --ours/--theirs and git add"})
			continue
		case e.PointerErr != nil:
			d.add(Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: "invalid index pointer (conflict): " + e.PointerErr.Error()})
			continue
		case !e.HasPointer:
			continue // historical raw blob: not attributed to an object yet
		}
		cond := obs.Row.Condition()
		switch cond {
		case state.HydratedModified:
			d.add(Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: "working file is modified (hydrated-modified); commit it with git add or discard the change; repair never overwrites it"})
		case state.Conflict:
			d.add(Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: "working file holds a pointer to " + obs.Row.WorkingDigest + " but the index pointer is " + indexOID + " (conflict); resolve with git checkout -- " + e.Path})
		case state.Missing:
			d.add(Finding{Path: e.Path, Kind: FindingIndexWorktree, Message: "working file is missing; repair rewrites the index pointer (unhydrated)", Repairable: true})
			d.missingPaths = append(d.missingPaths, e)
		}
		if prev != nil {
			switch {
			case prev.IndexPointerDigest != "" && prev.IndexPointerDigest != indexOID:
				d.add(Finding{Path: e.Path, Kind: FindingStateDerived, Message: "state row records index pointer " + prev.IndexPointerDigest + " but the index holds " + indexOID, Repairable: true})
			case obs.Present && prev.Intent != obs.Row.Intent:
				d.add(Finding{Path: e.Path, Kind: FindingStateDerived, Message: fmt.Sprintf("state row records intent %s but the working file is %s", prev.Intent, obs.Row.Intent), Repairable: true})
			case obs.Rehashed && cond != state.HydratedModified && cond != state.Conflict:
				d.add(Finding{Path: e.Path, Kind: FindingStateDerived, Message: "state row fingerprint is stale (file changed on disk); the digest was recomputed", Repairable: true})
			}
		} else if obs.Present && obs.Row.Intent == state.IntentHydrated && d.stateErr == nil {
			d.add(Finding{Path: e.Path, Kind: FindingStateDerived, Message: "hydrated working file has no state row; a checkout would treat the path as unhydrated", Repairable: true})
		}
		// Publication and cache facts for every object behind the pointer:
		// one for whole/1, a manifest plus its chunks for a chunked encoding.
		// Reporting, never repairing: doctor writes nothing.
		content, cerr := r.Content(withoutHealing(ctx), e.Pointer)
		if cerr != nil {
			d.add(Finding{Path: e.Path, Kind: FindingPublication, Message: "cannot resolve the objects behind " + indexOID + ": " + cerr.Error()})
			continue
		}
		for _, o := range content.Objects {
			referenced[o.Digest] = true
		}
		for _, o := range content.Objects {
			if !r.Cache.Has(r.Domain, o.Digest) {
				continue
			}
			if info, err := r.Cache.Stat(r.Domain, o.Digest); err == nil && info.Size != o.Size {
				d.add(Finding{Path: e.Path, Kind: FindingCache, Message: fmt.Sprintf("cached object %s has size %d but the manifest says %d; fxvcs verify reports it, hydrate re-fetches", o.Digest, info.Size, o.Size)})
			}
		}
		missing, unrecorded := 0, 0
		var firstMissing, firstUnrecorded, missingDetail string
		for _, o := range content.Objects {
			cached := r.Cache.Has(r.Domain, o.Digest)
			recorded, published := false, false
			if d.ledger != nil {
				_, recorded, _ = d.ledger.Object(ctx, o.Digest)
				_, published = publicationFlags(ctx, d.ledger, o.Digest, required)
			}
			switch {
			case !cached && !published:
				if ok, detail := r.remoteHasAll(ctx, o.Digest); !ok {
					missing++
					if firstMissing == "" {
						firstMissing, missingDetail = o.Digest, detail
					}
				}
			case cached && !recorded && d.ledgerErr == nil:
				unrecorded++
				if firstUnrecorded == "" {
					firstUnrecorded = o.Digest
				}
			}
		}
		total := len(content.Objects)
		if missing > 0 {
			d.add(Finding{Path: e.Path, Kind: FindingPublication,
				Message: objectPhrase(missing, total, firstMissing) + " is neither cached nor available on every required remote (" + missingDetail + "); run fxvcs objects publish where it was added"})
		}
		if unrecorded > 0 {
			d.add(Finding{Path: e.Path, Kind: FindingPublication, Repairable: true,
				Message: "cached " + objectPhrase(unrecorded, total, firstUnrecorded) + " referenced by the index has no publication ledger row; repair records it as pending"})
			d.unrecorded = append(d.unrecorded, e)
		}
	}
	// Rows for paths no longer managed.
	for p := range d.rows {
		if _, ok := d.byPath[p]; !ok {
			d.add(Finding{Path: p, Kind: FindingStateDerived, Message: "state row exists for a path that is no longer a managed index entry", Repairable: true})
		}
	}
	// Interrupted operations.
	for _, j := range d.interrupted {
		decision := state.KeepDirty
		if e, ok := d.byPath[j.Path]; ok {
			decision = state.Reconcile(j, e.BlobOID, d.obs[j.Path].Row.Fingerprint)
		} else if absPath, err := r.abs(j.Path); err == nil {
			fp, _ := state.StatFingerprint(absPath)
			decision = state.Reconcile(j, "", fp)
		}
		d.add(Finding{Path: j.Path, Kind: FindingStateDerived, Message: fmt.Sprintf("interrupted %s operation (phase %s) → %s", j.Operation, j.Phase, decision), Repairable: decision != state.KeepDirty})
	}
	// Ledger vs cache.
	if d.ledger != nil {
		pending, err := d.ledger.Pending(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, digest := range pending {
			if !r.Cache.Has(r.Domain, digest) {
				d.add(Finding{Kind: FindingPublication, Message: "object " + digest + " is pending publication but its bytes are not in the local cache; re-add the file (git add) or fetch it from a remote"})
			}
		}
	}
	if list, err := r.Cache.List(); err == nil {
		for _, ce := range list {
			if ce.Domain != r.Domain || !ce.Pinned {
				continue
			}
			recorded := false
			if d.ledger != nil {
				_, recorded, _ = d.ledger.Object(ctx, ce.Digest)
			}
			if !recorded && !referenced[ce.Digest] {
				d.add(Finding{Kind: FindingPublication, Message: "cached object " + ce.Digest + " is pinned but has no ledger row and no index referrer; it will stay pinned until recorded or removed"})
			}
		}
	}
	// Git configuration and hook.
	launcher := opts.Launcher
	if launcher == "" {
		launcher, _, err = r.resolveLauncherPath("")
	}
	if err != nil {
		return nil, err
	}
	d.launcher = launcher
	for _, want := range filter.GitConfigEntries(launcher) {
		got, ok, err := r.Git.ConfigGet(ctx, want.Key)
		if err != nil {
			return nil, err
		}
		if !ok || got != want.Value {
			d.configStale = true
			have := "is not set"
			if ok {
				have = "is " + got
			}
			d.add(Finding{Kind: FindingGitConfig, Message: want.Key + " " + have + "; expected " + want.Value, Repairable: true})
		}
	}
	hooksDir, err := r.Git.GitPath(ctx, "hooks")
	if err != nil {
		return nil, err
	}
	hook := filepath.Join(hooksDir, "pre-push")
	if b, err := os.ReadFile(hook); err != nil { //nolint:gosec // hooks dir resolved by git
		if errors.Is(err, os.ErrNotExist) {
			d.hookMissing = true
			d.add(Finding{Kind: FindingGitConfig, Message: "pre-push hook is not installed (" + hook + "); pushes are not checked for unpublished objects", Repairable: true})
		} else {
			return nil, err
		}
	} else if !strings.Contains(string(b), filter.HookMarker) {
		d.hookForeign = hook
		d.add(Finding{Kind: FindingGitConfig, Message: strings.TrimSpace(filter.PrePushRefusalMessage(filter.HookInstallResult{Path: hook, HooksPath: hooksDir}, launcher))})
	}
	return d, nil
}

// remoteHasAll probes every required remote for digest (bounded, read-only,
// no lock). detail explains a negative answer.
// objectPhrase names the affected objects. A whole/1 pointer has exactly one,
// and keeps the plain wording; a chunked pointer summarises so a single bad
// asset cannot print hundreds of lines.
func objectPhrase(count, total int, first string) string {
	if total == 1 {
		return "object " + first
	}
	return fmt.Sprintf("%d of the %d objects behind this path (first: %s)", count, total, first)
}

func (r *Repo) remoteHasAll(ctx context.Context, digest string) (bool, string) {
	required := r.RequiredRemotes()
	if len(required) == 0 {
		return false, "no remote is requiredForPublication and the object is not recorded locally"
	}
	for _, name := range required {
		s, _, err := r.Store(name)
		if err != nil {
			return false, name + ": " + err.Error()
		}
		ok, err := s.Has(ctx, r.Domain, digest)
		if err != nil {
			return false, name + ": " + err.Error()
		}
		if !ok {
			return false, "missing on " + name
		}
	}
	return true, ""
}

// Doctor reports every disagreement between the index, the working tree,
// state.db, the publication ledger, the cache, and the Git driver
// configuration. It is read-only: no database is created or migrated and no
// working file is written.
func (r *Repo) Doctor(ctx context.Context, opts DoctorOptions) ([]Finding, error) {
	d, err := r.diagnose(ctx, opts)
	if err != nil {
		return nil, err
	}
	if d.findings == nil {
		d.findings = []Finding{}
	}
	return d.findings, nil
}
