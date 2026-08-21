package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// Migration errors.
var (
	// ErrNotTracked: a selected path matches no managed rule.
	ErrNotTracked = errors.New("app: path is not tracked by an FXVCS rule (run: fxvcs track <path> or fxvcs track --glob <pattern>)")
	// ErrDirty: the working file differs from the index; migrate refuses.
	ErrDirty = errors.New("app: working file differs from the index; commit or stash it before migrating")
	// ErrSetupRequired: the filter driver is not configured in this clone.
	ErrSetupRequired = errors.New("app: filter driver is not configured in this clone (run: fxvcs setup)")
	// ErrStagedMismatch: git add did not stage the expected pointer.
	ErrStagedMismatch = errors.New("app: staged blob is not the expected pointer; check the filter installation")
)

// MigrateInput selects index entries to convert.
type MigrateInput struct {
	Paths    []string
	Glob     bool
	Progress ProgressFunc
}

// MigrateOutcome is one path's result.
type MigrateOutcome struct {
	Path    string
	Digest  string
	Bytes   int64
	Skipped bool
	Reason  string
}

// Migrate converts selected index entries that are ordinary blobs into
// pointers without rewriting history: for each path it ingests the working
// file (r.Ingest: cache, pin, ledger) and then runs `git add --renormalize
// -- <path>` so
// the clean filter stages the identical deterministic pointer while the
// working tree keeps the hydrated bytes; the hydration state row is
// committed as hydrated-clean through the operation journal. Every path is
// validated (in index, tracked by a managed rule, clean against the index)
// before the first change. Already-pointer entries are reported as skipped.
func (r *Repo) Migrate(ctx context.Context, in MigrateInput) ([]MigrateOutcome, error) {
	if len(in.Paths) == 0 {
		return nil, fmt.Errorf("%w: at least one path or --glob pattern is required", ErrInvalidArgument)
	}
	if _, ok, err := r.Git.ConfigGet(ctx, "filter.fxvcs.process"); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrSetupRequired
	}
	paths, err := r.selectIndexPaths(ctx, in)
	if err != nil {
		return nil, err
	}
	attrs, err := r.Git.CheckAttrs(ctx, paths)
	if err != nil {
		return nil, err
	}
	type item struct {
		path  string
		entry gitclient.StageEntry
		size  int64
	}
	progress := in.Progress
	progress.emit(ItemProgress{Stage: StageResolving, Count: len(paths)})
	var todo []item
	var out []MigrateOutcome
	for n, p := range paths {
		progress.emit(ItemProgress{Stage: StageResolving, Index: n, Count: len(paths)})
		if !attrs[p].IsManaged() {
			if in.Glob {
				out = append(out, MigrateOutcome{Path: p, Skipped: true, Reason: "not tracked by an FXVCS rule"})
				continue
			}
			return nil, fmt.Errorf("%w: %s", ErrNotTracked, p)
		}
		entries, err := r.Git.LsFilesStage(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(entries) != 1 || entries[0].Stage != 0 || entries[0].Path != p {
			return nil, fmt.Errorf("%w: %s is not a merged index entry", ErrInvalidArgument, p)
		}
		blob, err := r.Git.CatFileBlob(ctx, ":"+p)
		if err != nil {
			return nil, err
		}
		if pointer.Sniff(blob) {
			out = append(out, MigrateOutcome{Path: p, Skipped: true, Reason: "already a pointer"})
			continue
		}
		abs := filepath.Join(r.Root, filepath.FromSlash(p))
		fi, statErr := os.Lstat(abs)
		if statErr != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: %s is not a regular working file", ErrInvalidArgument, p)
		}
		// Clean against the index means: the raw working bytes hash to the
		// index blob. `git diff` cannot be used here because the new rule
		// already routes the path through the clean filter, which is exactly
		// the difference migrate is about to stage.
		oid, err := r.rawBlobID(ctx, abs)
		if err != nil {
			return nil, err
		}
		if oid != entries[0].OID {
			return nil, fmt.Errorf("%w: %s", ErrDirty, p)
		}
		todo = append(todo, item{path: p, entry: entries[0], size: fi.Size()})
	}
	var plannedBytes int64
	for _, it := range todo {
		plannedBytes += it.size
	}
	progress.emit(ItemProgress{Planned: len(out) + len(todo), PlannedBytes: plannedBytes, PlanPhase: ProgressPhaseWrite})
	for _, it := range todo {
		progress.emit(ItemProgress{Path: it.path, Total: it.size, Phase: ProgressPhaseWrite})
		o, err := r.migrateOne(ctx, it.path, it.entry, func(n int64) {
			progress.emit(ItemProgress{Path: it.path, Bytes: n, Total: it.size, Phase: ProgressPhaseWrite})
		})
		if err != nil {
			return out, err
		}
		out = append(out, o)
		progress.emit(ItemProgress{Path: it.path, Digest: o.Digest, Bytes: o.Bytes, Total: it.size, Done: true, Phase: ProgressPhaseWrite})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// rawBlobID hashes a working file as an unfiltered Git blob.
func (r *Repo) rawBlobID(ctx context.Context, abs string) (string, error) {
	f, err := os.Open(abs) //nolint:gosec // repository-relative, pathsafe-normalized
	if err != nil {
		return "", err
	}
	defer f.Close()
	out, err := r.Git.RunInput(ctx, f, "hash-object", "--stdin", "--no-filters")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// selectIndexPaths resolves literal paths (must be in the index) or expands
// glob pathspecs against the index.
func (r *Repo) selectIndexPaths(ctx context.Context, in MigrateInput) ([]string, error) {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	if !in.Glob {
		for _, p := range in.Paths {
			n, err := pathsafe.Normalize(p)
			if err != nil {
				return nil, err
			}
			entries, err := r.Git.LsFilesStage(ctx, n)
			if err != nil {
				return nil, err
			}
			found := false
			for _, e := range entries {
				if e.Path == n {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: %s is not in the index", ErrInvalidArgument, n)
			}
			add(n)
		}
		return paths, nil
	}
	for _, g := range in.Paths {
		if g == "" {
			return nil, fmt.Errorf("%w: empty glob", ErrInvalidArgument)
		}
		matched, err := r.lsFiles(ctx, "--", ":(glob)"+g)
		if err != nil {
			return nil, err
		}
		for _, p := range matched {
			add(p)
		}
	}
	return paths, nil
}

// migrateOne runs the journaled conversion of one path.
func (r *Repo) migrateOne(ctx context.Context, p string, entry gitclient.StageEntry, onProgress func(int64)) (MigrateOutcome, error) {
	abs := filepath.Join(r.Root, filepath.FromSlash(p))
	fp, err := state.StatFingerprint(abs)
	if err != nil {
		return MigrateOutcome{}, err
	}
	db, err := r.State(ctx)
	if err != nil {
		return MigrateOutcome{}, err
	}
	// Journal start under the path lock, then release it: ingestion streams
	// the file and `git add` spawns the filter, neither may run under a lock.
	h, err := r.Lock(ctx, journal.LockPath, p)
	if err != nil {
		return MigrateOutcome{}, err
	}
	op, err := db.BeginOperation(ctx, state.OpMigrate, p, state.Expectation{IndexOID: entry.OID, Fingerprint: fp})
	_ = h.Release()
	if err != nil {
		return MigrateOutcome{}, err
	}
	abort := func(reason string, cause error) (MigrateOutcome, error) {
		_ = op.Abort(ctx, reason)
		return MigrateOutcome{}, cause
	}
	f, err := os.Open(abs) //nolint:gosec // repository-relative, pathsafe-normalized
	if err != nil {
		return abort("open: "+err.Error(), err)
	}
	var source io.Reader = f
	if onProgress != nil {
		source = &progressReader{r: f, step: progressStep, next: progressStep, fn: onProgress}
	}
	ing, err := r.Ingest(ctx, source, p)
	_ = f.Close()
	if err != nil {
		return abort("ingest: "+err.Error(), err)
	}
	if onProgress != nil {
		onProgress(ing.Size)
	}
	if err := op.Advance(ctx, state.PhasePinned); err != nil {
		return abort("journal", err)
	}
	// Re-check the expectations, then stage through Git so the clean filter
	// produces the identical pointer and Git's stat cache is refreshed.
	if fp2, err := state.StatFingerprint(abs); err != nil || fp2 != fp {
		return abort("working file changed during ingest", fmt.Errorf("%w: %s changed while migrating", ErrDirty, p))
	}
	// --renormalize re-runs the clean filter even when Git's stat cache says
	// the file is unchanged (it is: only its attributes changed).
	if _, err := r.Git.Run(ctx, "add", "--renormalize", "--", p); err != nil {
		return abort("git add: "+err.Error(), err)
	}
	staged, err := r.Git.CatFileBlob(ctx, ":"+p)
	if err != nil {
		return abort("read staged blob", err)
	}
	want := ing.Pointer.MustEncode()
	if !bytes.Equal(bytes.TrimRight(staged, "\n"), bytes.TrimRight(want, "\n")) {
		return abort("staged blob mismatch", fmt.Errorf("%w: %s", ErrStagedMismatch, p))
	}
	h, err = r.Lock(ctx, journal.LockPath, p)
	if err != nil {
		return abort("lock", err)
	}
	defer func() { _ = h.Release() }()
	fpFinal, _ := state.StatFingerprint(abs)
	row := state.PathRow{
		Path:               p,
		Intent:             state.IntentHydrated,
		IndexPointerDigest: ing.Digest,
		WorkingDigest:      ing.Digest,
		Fingerprint:        fpFinal,
		LastVerifiedNS:     r.Now().UnixNano(),
	}
	if err := op.Commit(ctx, row); err != nil {
		return MigrateOutcome{}, err
	}
	return MigrateOutcome{Path: p, Digest: ing.Digest, Bytes: ing.Size}, nil
}
