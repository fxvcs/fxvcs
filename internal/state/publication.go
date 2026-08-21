package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/migrate"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

// Ledger is an open $GIT_COMMON_DIR/fxvcs/publication.db.
type Ledger struct{ *handle }

// OpenLedger opens (and with Options.Create creates) the shared publication
// ledger. Same negotiation rules as Open; on inconsistency the guidance is
// restore-from-backup, because pending pins are authoritative.
func OpenLedger(ctx context.Context, path string, opts Options) (*Ledger, error) {
	h, err := open(ctx, path, KindPublicationLedger, PublicationMigrations(), opts)
	if err != nil {
		return nil, err
	}
	return &Ledger{h}, nil
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ErrInvalidDigest: object digests must be sha256:<64 hex>.
var ErrInvalidDigest = errors.New("state: invalid object digest")

func checkDigest(d string) error {
	if !digestRE.MatchString(d) {
		return fmt.Errorf("%w: %q", ErrInvalidDigest, d)
	}
	return nil
}

// ObjectRecord describes a locally ingested object.
type ObjectRecord struct {
	Digest          string
	StorageDomainID string
	Size            int64
	Encoding        string
	ManifestDigest  string
	IngestedNS      int64
	Pending         bool
}

// PublicationSummary is the aggregate publication state of every unique
// object known to a repository ledger. Pending counts objects missing from at
// least one currently required remote; it is deliberately independent of the
// pin bit, which can already be settled when a required remote is added later.
type PublicationSummary struct {
	Objects        int
	Bytes          int64
	PendingObjects int
	PendingBytes   int64
	Remotes        []PublicationRemoteUsage
}

// PublicationRemoteUsage is the unique-object footprint recorded for one
// remote. Replicated objects count once on each remote, so usage across
// remotes is not additive. It is ledger knowledge, not a live inventory.
type PublicationRemoteUsage struct {
	Name    string
	Objects int
	Bytes   int64
}

// RecordIngest durably records an ingested object as pending publication
// (pinned). Re-recording an existing digest keeps its earlier ingest time and
// pending flag.
func (l *Ledger) RecordIngest(ctx context.Context, rec ObjectRecord) error {
	if err := checkDigest(rec.Digest); err != nil {
		return err
	}
	if rec.ManifestDigest != "" {
		if err := checkDigest(rec.ManifestDigest); err != nil {
			return err
		}
	}
	if rec.StorageDomainID == "" || rec.Encoding == "" || rec.Size < 0 {
		return fmt.Errorf("state: object record for %s is incomplete", rec.Digest)
	}
	ts := rec.IngestedNS
	if ts == 0 {
		ts = l.nowNS()
	}
	return l.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec(`INSERT INTO objects (digest, storage_domain_id, size, encoding, manifest_digest, ingested_ns, pending)
			VALUES (?,?,?,?,?,?,1) ON CONFLICT(digest) DO NOTHING`,
			rec.Digest, rec.StorageDomainID, rec.Size, rec.Encoding, rec.ManifestDigest, ts)
		return err
	})
}

// Object returns the record for a digest.
func (l *Ledger) Object(ctx context.Context, digest string) (ObjectRecord, bool, error) {
	var r ObjectRecord
	var pending int
	err := l.db.QueryRowContext(ctx, "SELECT digest, storage_domain_id, size, encoding, manifest_digest, ingested_ns, pending FROM objects WHERE digest = ?", digest).
		Scan(&r.Digest, &r.StorageDomainID, &r.Size, &r.Encoding, &r.ManifestDigest, &r.IngestedNS, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	r.Pending = pending == 1
	return r, err == nil, err
}

// AddReferrer records that a Git pointer at path (in a commit, or "index")
// references the object.
func (l *Ledger) AddReferrer(ctx context.Context, digest, path, commitOrIndex string) error {
	if err := checkDigest(digest); err != nil {
		return err
	}
	p, err := pathsafe.Normalize(path)
	if err != nil {
		return err
	}
	if commitOrIndex == "" {
		return errors.New("state: referrer needs a commit id or \"index\"")
	}
	return l.tx(ctx, func(tx migrate.Tx) error {
		_, err := tx.Exec("INSERT INTO referrers (digest, path, commit_or_index) VALUES (?,?,?) ON CONFLICT DO NOTHING", digest, p, commitOrIndex)
		return err
	})
}

// Referrers lists (path, commitOrIndex) pairs for a digest.
func (l *Ledger) Referrers(ctx context.Context, digest string) ([][2]string, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT path, commit_or_index FROM referrers WHERE digest = ? ORDER BY path, commit_or_index", digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var p, c string
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out = append(out, [2]string{p, c})
	}
	return out, rows.Err()
}

// MarkPublished records that the object is durable on remote.
func (l *Ledger) MarkPublished(ctx context.Context, digest, remote string) error {
	if err := checkDigest(digest); err != nil {
		return err
	}
	if remote == "" {
		return errors.New("state: remote name required")
	}
	return l.tx(ctx, func(tx migrate.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM objects WHERE digest = ?", digest).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("state: object %s not recorded", digest)
		}
		_, err := tx.Exec("INSERT INTO remotes (digest, remote_name, published_ns) VALUES (?,?,?) ON CONFLICT DO NOTHING", digest, remote, l.nowNS())
		return err
	})
}

// PublishedRemotes lists remotes that hold the object.
func (l *Ledger) PublishedRemotes(ctx context.Context, digest string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT remote_name FROM remotes WHERE digest = ? ORDER BY remote_name", digest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PublishedIndex returns every recorded publication as digest → remote → true.
//
// It is one scan of a table that holds one row per (object, remote) pair,
// which is cheap next to what it replaces: the alternative is a query per
// object, and callers that are about to decide whether thousands of objects
// need uploading would otherwise pay a round trip each.
func (l *Ledger) PublishedIndex(ctx context.Context) (map[string]map[string]bool, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT digest, remote_name FROM remotes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var digest, remote string
		if err := rows.Scan(&digest, &remote); err != nil {
			return nil, err
		}
		if out[digest] == nil {
			out[digest] = map[string]bool{}
		}
		out[digest][remote] = true
	}
	return out, rows.Err()
}

// PublicationIndex answers, for the given object digests only, whether each is
// pending and which remotes hold it.
//
// It exists because the alternative is two queries per object, and a listing
// asks about every object behind every asset it reports: one chunked file is a
// manifest plus dozens of chunks, so a few thousand assets is tens of thousands
// of round trips to answer a question the database can answer in a handful.
//
// It is deliberately scoped to the digests asked about rather than scanning the
// tables: a listing restricted to one directory must not pay for the size of
// the repository's ledger. Digests are queried in batches because SQLite bounds
// how many parameters one statement may carry.
//
// A digest with no object row is absent from pending, which is how a caller
// distinguishes "recorded and already uploaded" from "this ledger has never
// seen it".
func (l *Ledger) PublicationIndex(ctx context.Context, digests []string) (pending map[string]bool, remotes map[string]map[string]bool, err error) {
	pending, remotes = map[string]bool{}, map[string]map[string]bool{}
	if len(digests) == 0 {
		return pending, remotes, nil
	}
	// Comfortably under SQLITE_MAX_VARIABLE_NUMBER, which is 999 on older
	// builds and 32766 on current ones.
	const batch = 500
	for start := 0; start < len(digests); start += batch {
		end := min(start+batch, len(digests))
		chunk := digests[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, d := range chunk {
			if err := checkDigest(d); err != nil {
				return nil, nil, err
			}
			args[i] = d
			holders[i] = "?"
		}
		in := "(" + strings.Join(holders, ",") + ")"
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
		objectQuery := "SELECT digest, pending FROM objects WHERE digest IN " + in
		if err := scanRows(ctx, l, objectQuery, args, func(rows *sql.Rows) error {
			var d string
			var p int
			if err := rows.Scan(&d, &p); err != nil {
				return err
			}
			pending[d] = p == 1
			return nil
		}); err != nil {
			return nil, nil, err
		}
		//nolint:gosec // G202: as above
		remoteQuery := "SELECT digest, remote_name FROM remotes WHERE digest IN " + in
		if err := scanRows(ctx, l, remoteQuery, args, func(rows *sql.Rows) error {
			var d, name string
			if err := rows.Scan(&d, &name); err != nil {
				return err
			}
			if remotes[d] == nil {
				remotes[d] = map[string]bool{}
			}
			remotes[d][name] = true
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	return pending, remotes, nil
}

// scanRows runs a query and applies scan to each row, closing the rows however
// it ends.
func scanRows(ctx context.Context, l *Ledger, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// MarkPublishedMany records many (object, remote) publications in one
// transaction. Digests with no object row are skipped rather than failing the
// batch: a remote may hold objects this clone never ingested.
func (l *Ledger) MarkPublishedMany(ctx context.Context, byDigest map[string][]string) error {
	if len(byDigest) == 0 {
		return nil
	}
	digests := make([]string, 0, len(byDigest))
	for d := range byDigest {
		if err := checkDigest(d); err != nil {
			return err
		}
		digests = append(digests, d)
	}
	sort.Strings(digests)
	now := l.nowNS()
	return l.tx(ctx, func(tx migrate.Tx) error {
		for _, d := range digests {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM objects WHERE digest = ?", d).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			for _, remote := range byDigest[d] {
				if remote == "" {
					return errors.New("state: remote name required")
				}
				if _, err := tx.Exec("INSERT INTO remotes (digest, remote_name, published_ns) VALUES (?,?,?) ON CONFLICT DO NOTHING", d, remote, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// PublishedRemotesMany returns, for each digest asked about, the remotes
// recorded as holding it.
//
// It is PublicationIndex without the pending half, for the caller that only
// needs to know who already has the object — publication planning, which asks
// about every object it might upload and would otherwise pay for a second
// query per batch whose answer it discards.
func (l *Ledger) PublishedRemotesMany(ctx context.Context, digests []string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	const batch = 500
	for start := 0; start < len(digests); start += batch {
		end := min(start+batch, len(digests))
		chunk := digests[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, d := range chunk {
			if err := checkDigest(d); err != nil {
				return nil, err
			}
			args[i] = d
			holders[i] = "?"
		}
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
		q := "SELECT digest, remote_name FROM remotes WHERE digest IN (" + strings.Join(holders, ",") + ")"
		if err := scanRows(ctx, l, q, args, func(rows *sql.Rows) error {
			var d, name string
			if err := rows.Scan(&d, &name); err != nil {
				return err
			}
			if out[d] == nil {
				out[d] = map[string]bool{}
			}
			out[d][name] = true
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReferrersMany returns every (path, commitOrIndex) pair for each digest, in
// the same order as Referrers, using one query per batch instead of one query
// per object.
func (l *Ledger) ReferrersMany(ctx context.Context, digests []string) (map[string][][2]string, error) {
	out := make(map[string][][2]string, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	const batch = 500
	for start := 0; start < len(digests); start += batch {
		end := min(start+batch, len(digests))
		chunk := digests[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, d := range chunk {
			if err := checkDigest(d); err != nil {
				return nil, err
			}
			args[i] = d
			holders[i] = "?"
		}
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
		q := "SELECT digest, path, commit_or_index FROM referrers WHERE digest IN (" + strings.Join(holders, ",") + ") ORDER BY digest, path, commit_or_index"
		if err := scanRows(ctx, l, q, args, func(rows *sql.Rows) error {
			var d, path, commitOrIndex string
			if err := rows.Scan(&d, &path, &commitOrIndex); err != nil {
				return err
			}
			out[d] = append(out[d], [2]string{path, commitOrIndex})
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReferrerPathsMany returns the distinct paths referring to each digest, in
// path order, for many digests at once.
//
// Publication planning wants the paths behind every object it is about to
// upload, so it can name assets rather than digests. Asked one object at a
// time that is a query per object, and a chunked asset is a manifest plus
// dozens of chunks.
func (l *Ledger) ReferrerPathsMany(ctx context.Context, digests []string) (map[string][]string, error) {
	out := make(map[string][]string, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	seen := map[string]map[string]bool{}
	const batch = 500
	for start := 0; start < len(digests); start += batch {
		end := min(start+batch, len(digests))
		chunk := digests[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, d := range chunk {
			if err := checkDigest(d); err != nil {
				return nil, err
			}
			args[i] = d
			holders[i] = "?"
		}
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
		q := "SELECT digest, path FROM referrers WHERE digest IN (" + strings.Join(holders, ",") + ") ORDER BY digest, path"
		if err := scanRows(ctx, l, q, args, func(rows *sql.Rows) error {
			var d, p string
			if err := rows.Scan(&d, &p); err != nil {
				return err
			}
			if seen[d] == nil {
				seen[d] = map[string]bool{}
			}
			if seen[d][p] {
				return nil
			}
			seen[d][p] = true
			out[d] = append(out[d], p)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ObjectsMany returns the recorded rows for the given digests, in one query
// per batch instead of one per digest.
func (l *Ledger) ObjectsMany(ctx context.Context, digests []string) (map[string]ObjectRecord, error) {
	out := make(map[string]ObjectRecord, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	const batch = 500
	for start := 0; start < len(digests); start += batch {
		end := min(start+batch, len(digests))
		chunk := digests[start:end]
		args := make([]any, len(chunk))
		holders := make([]string, len(chunk))
		for i, d := range chunk {
			if err := checkDigest(d); err != nil {
				return nil, err
			}
			args[i] = d
			holders[i] = "?"
		}
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
		q := `SELECT digest, storage_domain_id, size, encoding, manifest_digest, ingested_ns, pending
			FROM objects WHERE digest IN (` + strings.Join(holders, ",") + `)`
		if err := scanRows(ctx, l, q, args, func(rows *sql.Rows) error {
			var r ObjectRecord
			var pending int
			if err := rows.Scan(&r.Digest, &r.StorageDomainID, &r.Size, &r.Encoding, &r.ManifestDigest, &r.IngestedNS, &pending); err != nil {
				return err
			}
			r.Pending = pending == 1
			out[r.Digest] = r
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RecordIngestMany records many ingested objects and, when referrerPath is
// set, the path that refers to all of them — in one transaction.
//
// One transaction, not one per object, because the ledger is opened
// synchronous=FULL: a row at a time meant an fsync per row, and a single
// chunked asset is a manifest plus dozens of chunks. Ingesting one file paid
// dozens of fsyncs to record objects whose bytes were already durable, which
// is what made re-deriving a file from its own content expensive enough to be
// worth avoiding.
//
// Re-recording an existing digest keeps its earlier ingest time and pending
// flag, exactly as RecordIngest does.
func (l *Ledger) RecordIngestMany(ctx context.Context, recs []ObjectRecord, referrerPath, commitOrIndex string) error {
	if len(recs) == 0 {
		return nil
	}
	for _, rec := range recs {
		if err := checkDigest(rec.Digest); err != nil {
			return err
		}
		if rec.ManifestDigest != "" {
			if err := checkDigest(rec.ManifestDigest); err != nil {
				return err
			}
		}
		if rec.StorageDomainID == "" || rec.Encoding == "" || rec.Size < 0 {
			return fmt.Errorf("state: object record for %s is incomplete", rec.Digest)
		}
	}
	var path string
	if referrerPath != "" {
		if commitOrIndex == "" {
			return errors.New("state: referrer needs a commit id or \"index\"")
		}
		p, err := pathsafe.Normalize(referrerPath)
		if err != nil {
			return err
		}
		path = p
	}
	now := l.nowNS()
	return l.tx(ctx, func(tx migrate.Tx) error {
		for _, rec := range recs {
			ts := rec.IngestedNS
			if ts == 0 {
				ts = now
			}
			if _, err := tx.Exec(`INSERT INTO objects (digest, storage_domain_id, size, encoding, manifest_digest, ingested_ns, pending)
				VALUES (?,?,?,?,?,?,1) ON CONFLICT(digest) DO NOTHING`,
				rec.Digest, rec.StorageDomainID, rec.Size, rec.Encoding, rec.ManifestDigest, ts); err != nil {
				return err
			}
			if path == "" {
				continue
			}
			if _, err := tx.Exec("INSERT INTO referrers (digest, path, commit_or_index) VALUES (?,?,?) ON CONFLICT DO NOTHING",
				rec.Digest, path, commitOrIndex); err != nil {
				return err
			}
		}
		return nil
	})
}

// Pending returns digests still pinned that are missing from at least one of
// requiredRemotes (ordered by digest). With no required remotes every pinned
// object is pending: an object never becomes evictable by having no
// destination.
func (l *Ledger) Pending(ctx context.Context, requiredRemotes []string) ([]string, error) {
	rows, err := l.db.QueryContext(ctx, "SELECT digest FROM objects WHERE pending = 1 ORDER BY digest")
	if err != nil {
		return nil, err
	}
	var pinned []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		pinned = append(pinned, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(requiredRemotes) == 0 {
		return pinned, nil
	}
	// One set query, not one per pinned object: a repository with thousands of
	// pending chunks asked this question once per chunk, and the answer is a
	// join the database can do in a single pass.
	satisfied, err := l.satisfiedBy(ctx, requiredRemotes)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range pinned {
		if !satisfied[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// satisfiedBy returns the digests published to every one of remotes.
func (l *Ledger) satisfiedBy(ctx context.Context, remotes []string) (map[string]bool, error) {
	args := make([]any, 0, len(remotes)+1)
	holders := make([]string, len(remotes))
	for i, n := range remotes {
		holders[i] = "?"
		args = append(args, n)
	}
	args = append(args, len(remotes))
	//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every name is a bound parameter
	q := `SELECT digest FROM remotes WHERE remote_name IN (` + strings.Join(holders, ",") + `)
		GROUP BY digest HAVING COUNT(DISTINCT remote_name) >= ?`
	out := map[string]bool{}
	if err := scanRows(ctx, l, q, args, func(rows *sql.Rows) error {
		var d string
		if err := rows.Scan(&d); err != nil {
			return err
		}
		out[d] = true
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// IsPending reports whether the object is still pinned (unknown digests are
// not pending).
func (l *Ledger) IsPending(ctx context.Context, digest string) (bool, error) {
	var p int
	err := l.db.QueryRowContext(ctx, "SELECT pending FROM objects WHERE digest = ?", digest).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return p == 1, err
}

// Settle clears the pin of every object present on all requiredRemotes and
// returns the digests released. It is the only way pending becomes 0, so the
// caller decides explicitly when an object may be evicted from the cache.
func (l *Ledger) Settle(ctx context.Context, requiredRemotes []string) ([]string, error) {
	if len(requiredRemotes) == 0 {
		return nil, errors.New("state: Settle requires at least one required remote")
	}
	pending, err := l.Pending(ctx, nil)
	if err != nil {
		return nil, err
	}
	satisfied, err := l.satisfiedBy(ctx, requiredRemotes)
	if err != nil {
		return nil, err
	}
	var released []string
	for _, d := range pending {
		if satisfied[d] {
			released = append(released, d)
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	// One transaction, one statement per batch: unpinning thousands of objects
	// a row at a time was a query and an update each, inside a transaction
	// whose cost is already paid once.
	err = l.tx(ctx, func(tx migrate.Tx) error {
		const batch = 500
		for start := 0; start < len(released); start += batch {
			end := min(start+batch, len(released))
			chunk := released[start:end]
			args := make([]any, len(chunk))
			holders := make([]string, len(chunk))
			for i, d := range chunk {
				args[i] = d
				holders[i] = "?"
			}
			//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every digest is a bound parameter
			q := "UPDATE objects SET pending = 0 WHERE digest IN (" + strings.Join(holders, ",") + ")"
			if _, err := tx.Exec(q, args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

// All returns every object record ordered by digest.
func (l *Ledger) All(ctx context.Context) ([]ObjectRecord, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT digest, storage_domain_id, size, encoding, manifest_digest, ingested_ns, pending FROM objects ORDER BY digest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObjectRecord
	for rows.Next() {
		var r ObjectRecord
		var pending int
		if err := rows.Scan(&r.Digest, &r.StorageDomainID, &r.Size, &r.Encoding, &r.ManifestDigest, &r.IngestedNS, &pending); err != nil {
			return nil, err
		}
		r.Pending = pending == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Summary returns publication totals in one aggregate query. It reads only
// the compact publication ledger: no manifests, cache entries, files, or
// remote stores are consulted.
func (l *Ledger) Summary(ctx context.Context, requiredRemotes []string) (PublicationSummary, error) {
	var out PublicationSummary
	unique := make([]string, 0, len(requiredRemotes))
	seen := make(map[string]bool, len(requiredRemotes))
	for _, name := range requiredRemotes {
		if !seen[name] {
			seen[name] = true
			unique = append(unique, name)
		}
	}
	if len(unique) == 0 {
		err := l.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM objects`).
			Scan(&out.Objects, &out.Bytes)
		if err != nil {
			return out, err
		}
	} else {
		args := make([]any, 0, len(unique)+2)
		holders := make([]string, len(unique))
		for i, name := range unique {
			holders[i] = "?"
			args = append(args, name)
		}
		args = append(args, len(unique), len(unique))
		// One row leaves the database. Each object is counted once even when it is
		// absent from several required remotes, so Bytes and PendingBytes use the
		// same unique-object denominator.
		//nolint:gosec // G202: the only concatenated text is a generated run of "?"; every remote name is a bound parameter
		q := `WITH coverage AS (
		SELECT o.digest, o.size, COUNT(r.remote_name) AS published_count
		FROM objects o
		LEFT JOIN remotes r ON r.digest = o.digest AND r.remote_name IN (` + strings.Join(holders, ",") + `)
		GROUP BY o.digest, o.size
	)
	SELECT COUNT(*), COALESCE(SUM(size), 0),
		COALESCE(SUM(CASE WHEN published_count < ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN published_count < ? THEN size ELSE 0 END), 0)
	FROM coverage`
		if err := l.db.QueryRowContext(ctx, q, args...).Scan(
			&out.Objects, &out.Bytes, &out.PendingObjects, &out.PendingBytes,
		); err != nil {
			return out, err
		}
	}

	// A second aggregate gives per-remote recorded footprint. Keeping it
	// separate from the coverage CTE avoids multiplying the global totals by
	// the number of remotes and still returns only one row per backend.
	err := scanRows(ctx, l, `
		SELECT r.remote_name, COUNT(*), COALESCE(SUM(o.size), 0)
		FROM remotes r
		JOIN objects o ON o.digest = r.digest
		GROUP BY r.remote_name
		ORDER BY r.remote_name`, nil, func(rows *sql.Rows) error {
		var usage PublicationRemoteUsage
		if err := rows.Scan(&usage.Name, &usage.Objects, &usage.Bytes); err != nil {
			return err
		}
		out.Remotes = append(out.Remotes, usage)
		return nil
	})
	return out, err
}

// MissingOn returns, in digest order, every recorded object that lacks a
// publication record on at least one of the given remotes. It is a single
// query over the local ledger; no store is contacted.
func (l *Ledger) MissingOn(ctx context.Context, remotes []string) ([]string, error) {
	if len(remotes) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(remotes)+1)
	placeholders := ""
	for i, n := range remotes {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, n)
	}
	args = append(args, len(remotes))
	rows, err := l.db.QueryContext(ctx, `
		SELECT o.digest
		FROM objects o
		LEFT JOIN remotes r ON r.digest = o.digest AND r.remote_name IN (`+placeholders+`)
		GROUP BY o.digest
		HAVING COUNT(r.remote_name) < ?
		ORDER BY o.digest`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
