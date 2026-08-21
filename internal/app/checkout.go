package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// Checkout-consistency errors. They are the typed causes behind refused or
// aborted per-path operations; the SDK maps them to codes.
var (
	// ErrConflict: the index blob or the working file changed under an
	// operation, or the working file is in a state automation may not touch
	// (hydrated-modified, conflict). Nothing was written.
	ErrConflict = errors.New("app: checkout conflict")
	// ErrIntegrity: a pointer or object failed verification (malformed
	// pointer, manifest mismatch, digest/size mismatch, wrong storage domain).
	ErrIntegrity = errors.New("app: integrity check failed")
	// ErrObjectUnavailable: an object is neither cached nor available from
	// any configured remote.
	ErrObjectUnavailable = errors.New("app: object not available")
	// ErrObjectNotCached: an object is not in the local cache, and the caller
	// asked for an answer without contacting a remote. It is deliberately
	// distinct from ErrObjectUnavailable: this one means "not here yet", which
	// a download can fix, where the other means the content could not be found
	// anywhere this repository can reach.
	ErrObjectNotCached = errors.New("app: object not cached")
	// ErrNotManaged: a selected path has no filter=fxvcs attribute or is not
	// in the index.
	ErrNotManaged = errors.New("app: path is not managed by fxvcs")
)

// Selection names the paths an operation acts on. Empty Paths selects every
// managed path in the index. With Glob, entries are Git pathspecs (glob
// syntax); otherwise they are literal repository-relative paths.
type Selection struct {
	Paths []string
	Glob  bool
}

// IndexEntry is one managed path as recorded in the Git index together with
// its decoded pointer when the blob is one.
type IndexEntry struct {
	Path     string
	BlobOID  string // git object id of the index blob
	Stage    int    // 0 merged; 1..3 unmerged
	Unmerged bool
	// Pointer is set when the blob decodes as a supported pointer.
	Pointer    pointer.Pointer
	HasPointer bool
	// PointerErr records why the blob is not a usable pointer: nil for a
	// historical raw blob (not attributed to any object), otherwise the
	// decode/derivation failure (unsupported version, malformed, manifest
	// mismatch, wrong storage domain).
	PointerErr error
	IsPointer  bool // bytes sniff as a pointer of some version
	BlobSize   int64
}

// ManagedEntries lists index entries whose effective attributes route them
// through the fxvcs filter, restricted to the selection. Unknown literal
// paths (not in the index or not managed) are returned in missing so callers
// can report them per item. Nothing is locked; the result is a snapshot.
func (r *Repo) ManagedEntries(ctx context.Context, sel Selection) (entries []IndexEntry, missing []string, err error) {
	var wanted, pathspecs []string
	for _, p := range sel.Paths {
		p = filepath.ToSlash(p)
		if sel.Glob {
			if err := checkGlob(p); err != nil {
				return nil, nil, err
			}
			pathspecs = append(pathspecs, ":(glob)"+p)
			continue
		}
		n, err := pathsafe.Normalize(p)
		if err != nil {
			return nil, nil, err
		}
		wanted = append(wanted, n)
		pathspecs = append(pathspecs, ":(literal)"+n)
	}
	prefix := []string{"ls-files", "--stage", "-z"}
	if len(pathspecs) > 0 {
		prefix = append(prefix, "--")
	}
	// Batched: a selection may name thousands of paths (a script, an SDK
	// consumer, or `--glob` expanded by the caller), and one command line
	// holding all of them exceeds the operating system's argv limit.
	out, err := r.Git.RunBatched(ctx, prefix, pathspecs)
	if err != nil {
		return nil, nil, err
	}
	var raw []gitclient.StageEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		stage, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, nil, fmt.Errorf("app: parse index stage %q: %w", f[2], err)
		}
		raw = append(raw, gitclient.StageEntry{Mode: f[0], OID: f[1], Stage: stage, Path: p})
	}
	paths := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, e := range raw {
		if !seen[e.Path] {
			seen[e.Path] = true
			paths = append(paths, e.Path)
		}
	}
	attrs, err := r.Git.CheckAttrs(ctx, paths)
	if err != nil {
		return nil, nil, err
	}
	byPath := map[string]*IndexEntry{}
	var order []string
	for _, e := range raw {
		if !attrs[e.Path].IsManaged() {
			continue
		}
		if e.Mode == "120000" || e.Mode == "160000" {
			continue // symlinks and submodules are never assets
		}
		ie, ok := byPath[e.Path]
		if !ok {
			ie = &IndexEntry{Path: e.Path, BlobOID: e.OID, Stage: e.Stage}
			byPath[e.Path] = ie
			order = append(order, e.Path)
		}
		if e.Stage != 0 {
			ie.Unmerged = true
		}
	}
	// Decode pointers in one cat-file batch.
	oids := make([]string, 0, len(order))
	for _, p := range order {
		if !byPath[p].Unmerged {
			oids = append(oids, byPath[p].BlobOID)
		}
	}
	blobs, sizes, err := r.catFileBatch(ctx, oids)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range order {
		ie := byPath[p]
		ie.BlobSize = sizes[ie.BlobOID]
		if b, ok := blobs[ie.BlobOID]; ok {
			r.decodeIndexPointer(ie, b)
		}
		entries = append(entries, *ie)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if !sel.Glob {
		for _, w := range wanted {
			if _, ok := byPath[w]; ok {
				continue
			}
			// A literal directory argument selects every managed entry beneath it.
			prefix := w + "/"
			found := false
			for _, p := range order {
				if strings.HasPrefix(p, prefix) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, w)
			}
		}
	}
	return entries, missing, nil
}

// checkGlob applies the pathsafe rules that still hold for a glob pattern
// (relative, no NUL/backslash, no ".." segment); wildcard characters are
// allowed because Git, not the filesystem, expands them.
func checkGlob(p string) error {
	if p == "" {
		return pathsafe.ErrEmpty
	}
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("%w: NUL or backslash in %q", pathsafe.ErrBadCharacter, p)
	}
	if strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') {
		return fmt.Errorf("%w: %q", pathsafe.ErrAbsolute, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: %q", pathsafe.ErrTraversal, p)
		}
	}
	return nil
}

// decodeIndexPointer fills the pointer fields of ie from blob bytes, applying
// the same strictness as the filter: unsupported/malformed pointers and
// pointers whose manifest does not derive from (domain, oid, size) are
// integrity failures; a wrong storage domain is refused as well.
func (r *Repo) decodeIndexPointer(ie *IndexEntry, blob []byte) {
	if !pointer.Sniff(blob) {
		return
	}
	ie.IsPointer = true
	p, err := pointer.Decode(blob)
	if err != nil {
		ie.PointerErr = fmt.Errorf("%w: %v", ErrIntegrity, err)
		return
	}
	if err := checkPointer(r.Domain, p); err != nil {
		ie.PointerErr = err
		return
	}
	ie.Pointer, ie.HasPointer = p, true
}

// checkPointer verifies a decoded pointer against the repository's storage
// domain and, for whole/1, the derived manifest digest. A chunked encoding
// names a manifest object that this local check cannot read, so its manifest
// is validated where it is loaded (Repo.Content); an encoding this binary does
// not implement at all is rejected here with the upgrade hint.
func checkPointer(domainID string, p pointer.Pointer) error {
	if p.StorageDomainID != domainID {
		return fmt.Errorf("%w: pointer storage domain %s does not match repository domain %s", ErrIntegrity, p.StorageDomainID, domainID)
	}
	if p.Encoding != pointer.EncodingWhole {
		if _, err := chunk.ParamsFor(p.Encoding); err != nil {
			return fmt.Errorf("%w: encoding %q is not supported by this fxvcs; run: fxvcs upgrade --check", ErrIntegrity, p.Encoding)
		}
		return nil
	}
	want, err := filter.WholePointer(p.StorageDomainID, p.OID, p.Size)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if want.ManifestDigest != p.ManifestDigest {
		return fmt.Errorf("%w: manifest %s does not match whole/1 derivation %s", ErrIntegrity, p.ManifestDigest, want.ManifestDigest)
	}
	return nil
}

// catFileBatch returns the bytes of blobs no larger than pointer.MaxSize (a
// larger blob cannot be a pointer and is never read) plus every blob's size.
func (r *Repo) catFileBatch(ctx context.Context, oids []string) (blobs map[string][]byte, sizes map[string]int64, err error) {
	blobs, sizes = map[string][]byte{}, map[string]int64{}
	if len(oids) == 0 {
		return blobs, sizes, nil
	}
	var in bytes.Buffer
	for _, o := range oids {
		in.WriteString(o)
		in.WriteByte('\n')
	}
	out, err := r.Git.RunInput(ctx, &in, "cat-file", "--batch-check")
	if err != nil {
		return nil, nil, err
	}
	var small []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "blob" {
			continue
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		sizes[f[0]] = n
		if n <= pointer.MaxSize {
			small = append(small, f[0])
		}
	}
	if len(small) == 0 {
		return blobs, sizes, nil
	}
	in.Reset()
	for _, o := range small {
		in.WriteString(o)
		in.WriteByte('\n')
	}
	out, err = r.Git.RunInput(ctx, &in, "cat-file", "--batch")
	if err != nil {
		return nil, nil, err
	}
	rd := bufio.NewReader(bytes.NewReader(out))
	for {
		hdr, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		f := strings.Fields(hdr)
		if len(f) < 3 {
			continue
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(rd, b); err != nil {
			return nil, nil, fmt.Errorf("app: cat-file --batch truncated: %w", err)
		}
		_, _ = rd.ReadByte() // trailing LF
		blobs[f[0]] = b
	}
	return blobs, sizes, nil
}

// indexBlobOID re-reads the stage-0 index blob id for one path ("" when the
// path is not in the index or is unmerged). It is the bounded Git read of CAS
// step 5.
func (r *Repo) indexBlobOID(ctx context.Context, path string) (string, error) {
	entries, err := r.Git.LsFilesStage(ctx, ":(literal)"+path)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || entries[0].Stage != 0 {
		return "", nil
	}
	return entries[0].OID, nil
}

// refreshIndexStat asks Git to re-read the given paths from the working
// tree (`update-index --refresh -- <paths>`, near enough the call git-lfs
// makes after writing files). Git treats a size change as a modification without
// looking at the content, so a working file that just went from pointer to
// object (or back) would show as modified in `git status` until Git re-runs
// the clean filter on it; doing it here keeps status clean immediately.
//
// Naming a path stages its clean result, so a caller may only pass paths whose
// clean result is known to equal the index entry already — a file this
// operation just wrote, or one an availability check proved is already in the
// requested state. A locally modified asset must never appear here: this call
// would stage the modification.
//
// `git status` does not repair this on its own. It reports the entry as
// modified and leaves the stat data alone, so a path that misses its refresh
// stays modified in every interface that asks Git what changed until some
// later operation names it again. That is why the walk continues past a
// failing batch instead of returning, and why the invocation is retried: with
// no daemon between the CLI and the desktop application, another process
// holding index.lock for a moment is ordinary.
//
// The call is deliberately not quiet: `-q` suppresses Git's message for a
// path that turns out to need updating, and it suppresses the "Unable to
// create index.lock" fatal with it, leaving an exit code and an empty stderr
// that nothing can tell apart from any other failure. Losing the retry is a
// worse trade than the occasional informational line on a discarded stdout.
//
// It only runs when the fxvcs filter is configured and required, and never
// while an FXVCS lock is held (it spawns the filter). The error it returns is
// for tests and for callers that can report it; the operation itself is
// complete either way, and the next one over the same paths refreshes them.
func (r *Repo) refreshIndexStat(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if v, ok, err := r.Git.ConfigGet(ctx, "filter.fxvcs.process"); err != nil || !ok || v == "" {
		return nil
	}
	if v, ok, err := r.Git.ConfigGet(ctx, "filter.fxvcs.required"); err != nil || !ok || v != "true" {
		return nil
	}
	// Batched: a hydrate over a large repository refreshes thousands of paths,
	// and one command line holding all of them exceeds the operating system's
	// argv limit.
	var errs []error
	_ = gitclient.BatchPaths(paths, false, func(batch []string) error {
		args := make([]string, 0, 4+len(batch))
		args = append(args, "update-index", "--refresh", "--")
		args = append(args, batch...)
		if _, err := r.Git.RunRetry(ctx, gitclient.DefaultRetry, args...); err != nil {
			errs = append(errs, err)
		}
		return nil // one busy batch must not cost the rest their refresh
	})
	return errors.Join(errs...)
}

// abs returns the working-tree location of a repository-relative path.
func (r *Repo) abs(path string) (string, error) { return pathsafe.Join(r.Root, path) }

// observation is what one look at a working file yields together with the
// row it implies. It uses the stat-cache: when the recorded fingerprint
// matches, the recorded digest is reused instead of re-hashing the file.
type observation struct {
	Row      state.PathRow
	Present  bool
	Rehashed bool
}

// observePath derives the state row for path from the index pointer OID, the
// working file and (optionally) the previous row.
func (r *Repo) observePath(path, indexOID string, prev *state.PathRow) (observation, error) {
	absPath, err := r.abs(path)
	if err != nil {
		return observation{}, err
	}
	fp, err := state.StatFingerprint(absPath)
	if err != nil {
		return observation{}, err
	}
	obs := observation{Row: state.PathRow{Path: path, IndexPointerDigest: indexOID}}
	if fp.IsZero() {
		obs.Row.Intent = state.IntentUnhydrated
		if prev != nil && prev.Intent != "" {
			obs.Row.Intent = prev.Intent
		}
		if prev != nil {
			obs.Row.Generation = prev.Generation
		}
		return obs, nil
	}
	obs.Present = true
	if prev != nil && !prev.Fingerprint.IsZero() && prev.Fingerprint == fp && prev.WorkingDigest != "" {
		obs.Row.Intent = prev.Intent
		obs.Row.WorkingDigest = prev.WorkingDigest
		obs.Row.Fingerprint = fp
		obs.Row.Generation = prev.Generation
		obs.Row.LastVerifiedNS = prev.LastVerifiedNS
		return obs, nil
	}
	wf, err := state.ScanFile(absPath)
	if err != nil {
		return observation{}, err
	}
	obs.Rehashed = true
	obs.Row.Fingerprint = wf.Fingerprint
	obs.Row.WorkingDigest = wf.Digest
	if wf.IsPointer {
		obs.Row.Intent = state.IntentUnhydrated
	} else {
		obs.Row.Intent = state.IntentHydrated
	}
	if prev != nil {
		obs.Row.Generation = prev.Generation
	}
	obs.Row.LastVerifiedNS = r.Now().UnixNano()
	return obs, nil
}

// PathOutcome reports one path's result inside a multi-path operation.
type PathOutcome struct {
	Path    string
	Digest  string
	Bytes   int64
	Skipped bool
	Reason  string
	// Reuse says the content did not have to be transferred: ReuseCached when
	// the objects were already local, ReuseShared when another asset in the
	// same operation has identical content. Empty means it was transferred.
	Reuse string
	// Err is the typed cause when the item failed (nil for plain refusals
	// such as "already hydrated").
	Err error
}

func skip(path, reason string, err error) PathOutcome {
	return PathOutcome{Path: path, Skipped: true, Reason: reason, Err: err}
}

// ItemProgress is one progress event of a per-path operation.
type ItemProgress struct {
	Path   string
	Digest string
	Bytes  int64
	Total  int64
	Done   bool
	// Planned and PlannedBytes mark the one event an operation emits when its
	// exact work is known: it is about to process Planned items totalling
	// PlannedBytes. Path is empty on that event. Metadata-dependent operations
	// may report indeterminate resolving progress before this event.
	//
	// It exists so a display can show "12 of 600" and a bar that reaches the
	// end. Without it a long operation can only count what it has already
	// seen, which is precisely the number that is not useful while waiting.
	Planned      int
	PlannedBytes int64
	// Fetched marks a transfer report from background prefetching: object
	// bytes arrived for Path, but the file has not been written yet. It moves
	// the transferred total without claiming the item is done, which is the
	// difference between "downloading" and "written to the working tree".
	Fetched bool
	// Chunks and ChunksDone count the stored objects behind the item and how
	// many have arrived. A whole-blob asset has one; a chunked asset has its
	// manifest plus a chunk per content-defined piece, which is what makes
	// "12 of 47 chunks" meaningful to show.
	Chunks, ChunksDone int
	// Reuse says where an item's content came from when it did not have to be
	// transferred; empty means it was.
	Reuse string
	// Stage, with an empty Path, announces which part of the operation is
	// running. A load is three different kinds of work — reading the index and
	// resolving what each asset is made of, transferring what is missing, then
	// writing files — and they have different bottlenecks and different
	// progress. Index and Count position the stage when it can count itself.
	Stage string
	// Index and Count position a stage that can count itself.
	Index, Count int
	// ReusedItems and ReusedBytes accompany a plan event: how many of the
	// planned assets need no transfer at all, and how much that saves. Content
	// addressing is what makes it possible — one copy of a font vendored into
	// forty resources is one stored object — and an operation that silently
	// skips 40% of its bytes should say so.
	ReusedItems int
	ReusedBytes int64
	// PlanPhase says which kind of work PlannedBytes measures. Consumers must
	// not average a transfer denominator with a write denominator: each phase
	// has its own progress and rate.
	PlanPhase string
	// Phase identifies per-item work. Part fields identify the object/chunk
	// within a transfer so SDK consumers can show item-part progress without
	// reconstructing it from asset totals.
	Phase                                          string
	PartID                                         string
	PartBytes, PartTotal, WireBytes, PartWireBytes int64
	PartIndex, PartCount                           int
}

// IsPlan reports the operation-wide plan event.
func (p ItemProgress) IsPlan() bool { return p.Planned > 0 && p.Path == "" }

// IsStage reports a stage announcement.
func (p ItemProgress) IsStage() bool { return p.Stage != "" && p.Path == "" }

// ProgressFunc receives ItemProgress events (may be nil).
//
// Delivery is serialized: an operation that runs work concurrently still
// calls this one event at a time, so a consumer needs no synchronization of
// its own. Callers that fan out must hold their own lock across the call.
type ProgressFunc func(ItemProgress)

func (f ProgressFunc) emit(ev ItemProgress) {
	if f != nil {
		f(ev)
	}
}

// progressWriter counts bytes and reports at most every step bytes.
type progressWriter struct {
	w      io.Writer
	n      int64
	last   int64
	step   int64
	report func(n int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.n-p.last >= p.step {
		p.last = p.n
		p.report(p.n)
	}
	return n, err
}

// tmpFile creates the temporary destination for an atomic replace of dest.
// It lives under $GIT_DIR/fxvcs/tmp so the final rename is same-filesystem
// whenever the Git dir shares the worktree's filesystem; nextToDest asks for
// the fallback location beside the destination.
func (r *Repo) tmpFile(dest string, nextToDest bool) (*os.File, error) {
	dir := r.Paths.TmpDir
	if nextToDest {
		dir = filepath.Dir(dest)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, ".fxvcs-"+filepath.Base(dest)+"-*.tmp")
}

// prepareTmp writes the temporary object, fsyncs and closes it. On failure
// the temporary file is removed.
func prepareTmp(tmp *os.File, write func(w io.Writer) error) error {
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := write(tmp); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// renameOver atomically moves the prepared temporary file over dest,
// preserving dest's permission bits. When the rename crosses filesystems the
// bytes are copied to a temporary file beside dest first and renamed from
// there. tmpName never survives the call; dest is untouched on failure.
func (r *Repo) renameOver(tmpName, dest string) error {
	if fi, err := os.Stat(dest); err == nil {
		_ = os.Chmod(tmpName, fi.Mode().Perm())
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	err := os.Rename(tmpName, dest)
	if err == nil {
		syncDir(filepath.Dir(dest))
		return nil
	}
	if filepath.Dir(tmpName) == filepath.Dir(dest) {
		_ = os.Remove(tmpName)
		return err
	}
	side, serr := r.tmpFile(dest, true)
	if serr != nil {
		_ = os.Remove(tmpName)
		return err
	}
	src, oerr := os.Open(tmpName)
	if oerr != nil {
		_ = side.Close()
		_ = os.Remove(side.Name())
		_ = os.Remove(tmpName)
		return err
	}
	perr := prepareTmp(side, func(w io.Writer) error { _, e := io.Copy(w, src); return e })
	_ = src.Close()
	_ = os.Remove(tmpName)
	if perr != nil {
		return perr
	}
	return r.renameOver(side.Name(), dest)
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ---------------------------------------------------------------------------
// Filter-facing helpers (used through internal/filter/wire)
// ---------------------------------------------------------------------------

// HydrationIntent reports the recorded intent for a path. known is false when
// state.db does not exist or holds no row: the unhydrated default applies.
func (r *Repo) HydrationIntent(ctx context.Context, path string) (hydrated, known bool, err error) {
	db, err := r.StateIfExists(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, false, nil
		}
		return false, false, err
	}
	row, ok, err := db.Get(ctx, path)
	if err != nil || !ok {
		return false, ok, err
	}
	return row.Intent == state.IntentHydrated, true, nil
}

// RecordHydrated notes, best effort, that the working file at path holds the
// real bytes of digest (the clean filter just ingested them from it). The
// row is only written when the path lock is free; a busy lock means an
// operation on that path is in flight and will write the authoritative row.
func (r *Repo) RecordHydrated(ctx context.Context, path, digest string, size int64) error {
	if _, err := pathsafe.Normalize(path); err != nil {
		return err
	}
	h, err := r.TryLock(ctx, journal.LockPath, path)
	if err != nil {
		return nil // busy: skip; not an error for the filter
	}
	defer func() { _ = h.Release() }()
	db, err := r.State(ctx)
	if err != nil {
		return err
	}
	// The index pointer is not known here (Git stages the result after the
	// filter answers); keep what a previous row recorded and let status /
	// doctor compare against the live index.
	row := state.PathRow{Path: path, Intent: state.IntentHydrated, WorkingDigest: digest, LastVerifiedNS: r.Now().UnixNano()}
	if prev, ok, _ := db.Get(ctx, path); ok {
		row.Generation = prev.Generation
		row.IndexPointerDigest = prev.IndexPointerDigest
	}
	// Racy-Git guard: a file modified within the last second may still be
	// changing; then leave the fingerprint empty so status re-hashes once
	// instead of trusting a digest that may not match the final bytes.
	if absPath, err := r.abs(path); err == nil {
		if fp, err := state.StatFingerprint(absPath); err == nil && !fp.IsZero() && fp.Size == size && fp.MtimeNS < r.Now().Add(-time.Second).UnixNano() {
			row.Fingerprint = fp
		}
	}
	return db.Upsert(ctx, row)
}

// PublicationMissing returns the requiredForPublication remotes that hold no
// publication record for digest. When a required remote is bound on this
// machine and the ledger has no record, the remote is probed (bounded, no
// lock held) and a positive answer is recorded so a clone that did not
// publish the object itself is not blocked from pushing history containing
// it. ptr supplies the object metadata for the record.
func (r *Repo) PublicationMissing(ctx context.Context, ptr pointer.Pointer) ([]string, error) {
	required := r.RequiredRemotes()
	if len(required) == 0 {
		return nil, nil
	}
	// Every object behind the pointer must be durable, not only the whole-file
	// digest: a chunked asset is unreadable elsewhere until its manifest and
	// all of its chunks are published.
	content, err := r.Content(ctx, ptr)
	if err != nil {
		return nil, err
	}
	l, err := r.LedgerIfExists(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}
	var missing []string
	for _, name := range required {
		complete := true
		for _, o := range content.Objects {
			published := false
			if l != nil {
				pub, err := l.PublishedRemotes(ctx, o.Digest)
				if err != nil {
					return nil, err
				}
				for _, n := range pub {
					if n == name {
						published = true
					}
				}
			}
			if published {
				continue
			}
			s, _, err := r.Store(name)
			if err == nil {
				ok, herr := s.Has(ctx, ptr.StorageDomainID, o.Digest)
				if herr == nil && ok {
					if rerr := r.recordPublished(ctx, o, ptr, name); rerr != nil {
						return nil, rerr
					}
					continue
				}
			}
			complete = false
			break
		}
		if !complete {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// PublicationMissingMany is the batched form of PublicationMissing: it answers
// the same question for many pointers with a bounded number of round trips
// instead of one per pointer.
//
// The pre-push barrier asks this about every asset a push touches. Asked one
// pointer at a time against a network object store, that is one sequential
// HTTP request per asset before a single byte is uploaded — tens of seconds
// for a few hundred assets on an ordinary internet link, with nothing to show
// for it. Batched, it is one ledger scan plus one bulk existence query per
// remote.
//
// The returned map is keyed by pointer oid.
func (r *Repo) PublicationMissingMany(ctx context.Context, ptrs []pointer.Pointer) (map[string][]string, error) {
	out := make(map[string][]string, len(ptrs))
	required := r.RequiredRemotes()
	if len(required) == 0 || len(ptrs) == 0 {
		return out, nil
	}
	// Resolve every pointer to its objects (local work: manifests come from
	// the cache), and collect the union of digests to ask about.
	contents := make([]Content, 0, len(ptrs))
	union := map[string]bool{}
	for _, ptr := range ptrs {
		c, err := r.Content(ctx, ptr)
		if err != nil {
			return nil, err
		}
		contents = append(contents, c)
		for _, o := range c.Objects {
			union[o.Digest] = true
		}
	}
	digests := make([]string, 0, len(union))
	for d := range union {
		digests = append(digests, d)
	}
	sort.Strings(digests)

	// One ledger scan for what is already known to be published.
	published := map[string]map[string]bool{}
	if l, err := r.LedgerIfExists(ctx); err == nil {
		published, err = l.PublishedIndex(ctx)
		if err != nil {
			return nil, err
		}
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}

	// One bulk existence query per remote for everything the ledger does not
	// already account for, so a clone that did not publish an object itself is
	// not asked to re-upload it.
	confirmed := map[string][]string{}
	onRemote := map[string]map[string]bool{}
	for _, name := range required {
		var unknown []string
		for _, d := range digests {
			if !published[d][name] {
				unknown = append(unknown, d)
			}
		}
		if len(unknown) == 0 {
			continue
		}
		have := r.remoteHas(ctx, name, unknown)
		onRemote[name] = have
		for d, ok := range have {
			if ok {
				confirmed[d] = append(confirmed[d], name)
			}
		}
	}
	if len(confirmed) > 0 {
		l, err := r.Ledger(ctx)
		if err != nil {
			return nil, err
		}
		if err := l.MarkPublishedMany(ctx, confirmed); err != nil {
			return nil, err
		}
		settled, err := l.Settle(ctx, required)
		if err != nil {
			return nil, err
		}
		for _, d := range settled {
			_ = r.Cache.Unpin(r.Domain, d)
		}
	}

	for i, c := range contents {
		var missing []string
		for _, name := range required {
			complete := true
			for _, o := range c.Objects {
				if published[o.Digest][name] || onRemote[name][o.Digest] {
					continue
				}
				complete = false
				break
			}
			if !complete {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			out[ptrs[i].OID] = missing
		}
	}
	return out, nil
}

// remoteHas answers existence for many digests, using the backend's bulk
// query when it has one and falling back to per-object checks otherwise. An
// unreachable or unbound remote answers "no" for everything, which makes the
// push refuse rather than assume durability.
func (r *Repo) remoteHas(ctx context.Context, name string, digests []string) map[string]bool {
	have := make(map[string]bool, len(digests))
	s, _, err := r.Store(name)
	if err != nil {
		return have
	}
	if bh, ok := s.(objectstore.BatchHaser); ok {
		if got, err := bh.BatchHas(ctx, r.Domain, digests); err == nil {
			return got
		}
	}
	for _, d := range digests {
		ok, err := s.Has(ctx, r.Domain, d)
		have[d] = err == nil && ok
	}
	return have
}

// recordPublished records a remote-verified publication in the ledger.
func (r *Repo) recordPublished(ctx context.Context, o ContentObject, ptr pointer.Pointer, remote string) error {
	l, err := r.Ledger(ctx)
	if err != nil {
		return err
	}
	if _, ok, err := l.Object(ctx, o.Digest); err != nil {
		return err
	} else if !ok {
		row := state.ObjectRecord{Digest: o.Digest, StorageDomainID: ptr.StorageDomainID, Size: o.Size, Encoding: o.Kind}
		if o.Kind == ObjectKindWhole {
			row.ManifestDigest = ptr.ManifestDigest
		}
		if err := l.RecordIngest(ctx, row); err != nil {
			return err
		}
	}
	if err := l.MarkPublished(ctx, o.Digest, remote); err != nil {
		return err
	}
	settled, err := l.Settle(ctx, r.RequiredRemotes())
	if err != nil {
		return err
	}
	for _, d := range settled {
		_ = r.Cache.Unpin(r.Domain, d)
	}
	return nil
}
