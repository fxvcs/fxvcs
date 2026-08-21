package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/objectstore/filesystem"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/state"
)

var remoteNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// StorageAddInput describes a remote to register.
type StorageAddInput struct {
	Name string
	Type string // filesystem | s3
	Path string // filesystem: absolute directory on this machine
	// S3 descriptor. Bucket, Endpoint, Prefix, Region, and Addressing are
	// non-secret and go into the committed configuration for a shared remote;
	// credentials never do (see StorageLogin).
	Bucket     string
	Endpoint   string
	Prefix     string
	Region     string
	Addressing string
	// Local registers a clone-local remote only (not written to the committed
	// configuration). When false the remote is declared in
	// .fxvcs/repository.yaml if absent (a reviewable working-tree edit; never
	// committed by FXVCS) and its machine-specific binding is stored locally.
	Local                  bool
	RequiredForPublication bool
}

// StorageAdd registers a storage remote. By default it declares the non-secret
// remote descriptor in the committed configuration (if not already declared)
// so every clone shares it, and binds the machine-specific location in local
// configuration. With Local it only records the clone-local remote. It never
// commits and never writes credentials or paths into committed files.
func (r *Repo) StorageAdd(ctx context.Context, in StorageAddInput) (RemoteView, error) {
	if !remoteNameRE.MatchString(in.Name) {
		return RemoteView{}, fmt.Errorf("%w: remote name %q", ErrInvalidArgument, in.Name)
	}
	switch in.Type {
	case "filesystem":
		if in.Path == "" || !filepath.IsAbs(in.Path) {
			return RemoteView{}, fmt.Errorf("%w: filesystem remote requires an absolute --path", ErrInvalidArgument)
		}
	case "s3":
		if strings.TrimSpace(in.Bucket) == "" {
			return RemoteView{}, fmt.Errorf("%w: s3 remote requires --bucket", ErrInvalidArgument)
		}
		if in.Addressing != "" && in.Addressing != s3.AddressingPath && in.Addressing != s3.AddressingVirtualHost {
			return RemoteView{}, fmt.Errorf("%w: --addressing %q (want %s or %s)", ErrInvalidArgument, in.Addressing, s3.AddressingPath, s3.AddressingVirtualHost)
		}
		if in.Endpoint != "" && !strings.HasPrefix(in.Endpoint, "https://") && !strings.HasPrefix(in.Endpoint, "http://") {
			return RemoteView{}, fmt.Errorf("%w: --endpoint %q must be an http(s) URL", ErrInvalidArgument, in.Endpoint)
		}
	default:
		return RemoteView{}, fmt.Errorf("%w: %q (supported: filesystem, s3)", ErrUnsupportedType, in.Type)
	}
	committed := false
	for _, c := range r.Config.Spec.Storage.Remotes {
		if c.Name == in.Name {
			committed = true
			if c.Type != in.Type {
				return RemoteView{}, fmt.Errorf("%w: committed remote %q is type %s", ErrInvalidArgument, in.Name, c.Type)
			}
		}
	}
	if in.Type == "filesystem" {
		if _, err := filesystem.New(filepath.Clean(in.Path)); err != nil {
			return RemoteView{}, err
		}
	}
	if !committed && !in.Local {
		rem := domain.StorageRemote{Name: in.Name, Type: in.Type, RequiredForPublication: in.RequiredForPublication}
		if in.Type == "s3" {
			// Only the non-secret descriptor is committed; where the bucket is
			// must be shared, how this machine signs for it must not.
			rem.Bucket, rem.Endpoint, rem.Prefix, rem.Region, rem.Addressing = in.Bucket, in.Endpoint, in.Prefix, in.Region, in.Addressing
		}
		if err := r.declareCommittedRemote(rem); err != nil {
			return RemoteView{}, err
		}
		committed = true
	}
	h, err := r.Lock(ctx, journal.LockPublication, "local-config")
	if err != nil {
		return RemoteView{}, err
	}
	defer func() { _ = h.Release() }()
	// Re-read under the lock.
	lc, err := LoadLocalConfig(r.Paths.ConfigYAML)
	if err != nil {
		return RemoteView{}, err
	}
	entry := domain.LocalRemote{Name: in.Name, Type: in.Type, Local: !committed, RequiredForPublication: !committed && in.RequiredForPublication}
	switch in.Type {
	case "filesystem":
		entry.Path = filepath.Clean(in.Path)
	case "s3":
		// A committed remote already carries the descriptor; record locally
		// only what this machine adds or overrides.
		if !committed {
			entry.Bucket, entry.Prefix = in.Bucket, in.Prefix
		}
		entry.Endpoint, entry.Region, entry.Addressing = in.Endpoint, in.Region, in.Addressing
		if committed {
			for _, c := range r.Config.Spec.Storage.Remotes {
				if c.Name != in.Name {
					continue
				}
				// Drop local values that merely repeat the committed record so
				// the local file stays a list of genuine differences.
				if c.Endpoint == entry.Endpoint {
					entry.Endpoint = ""
				}
				if c.Region == entry.Region {
					entry.Region = ""
				}
				if c.Addressing == entry.Addressing {
					entry.Addressing = ""
				}
				if c.Bucket != in.Bucket && in.Bucket != "" {
					entry.Bucket = in.Bucket
				}
				if c.Prefix != in.Prefix && in.Prefix != "" {
					entry.Prefix = in.Prefix
				}
			}
		}
	}
	replaced := false
	for i, l := range lc.Spec.Remotes {
		if l.Name == in.Name {
			lc.Spec.Remotes[i] = entry
			replaced = true
		}
	}
	if !replaced {
		lc.Spec.Remotes = append(lc.Spec.Remotes, entry)
	}
	sort.Slice(lc.Spec.Remotes, func(i, j int) bool { return lc.Spec.Remotes[i].Name < lc.Spec.Remotes[j].Name })
	if err := SaveLocalConfig(r.Paths.ConfigYAML, lc); err != nil {
		return RemoteView{}, err
	}
	r.Local = lc
	delete(r.stores, in.Name)
	for _, v := range r.Remotes() {
		if v.Name == in.Name {
			return v, nil
		}
	}
	return RemoteView{}, ErrNoSuchRemote
}

// declareCommittedRemote appends a remote descriptor to
// .fxvcs/repository.yaml in the working tree, preserving the file's existing
// formatting and comments, validates the result, and reloads r.Config. The
// change is left uncommitted for review.
func (r *Repo) declareCommittedRemote(rem domain.StorageRemote) error {
	path := filepath.Join(r.Root, filepath.FromSlash(repository.ConfigPath))
	data, err := os.ReadFile(path) //nolint:gosec // repository config inside the checkout
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%w: empty %s", ErrInvalidArgument, repository.ConfigPath)
	}
	spec := mappingValue(doc.Content[0], "spec")
	if spec == nil {
		return fmt.Errorf("%w: %s has no spec", ErrInvalidArgument, repository.ConfigPath)
	}
	storage := mappingValue(spec, "storage")
	if storage == nil {
		storage = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "storage"}, storage)
	}
	remotes := mappingValue(storage, "remotes")
	if remotes == nil || remotes.Kind != yaml.SequenceNode {
		remotes = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		storage.Content = append(storage.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "remotes"}, remotes)
	}
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		str("name"), str(rem.Name),
		str("type"), str(rem.Type),
	}}
	// Non-secret S3 descriptor fields, in schema order. Credentials are never
	// among them.
	for _, kv := range [][2]string{
		{"endpoint", rem.Endpoint}, {"bucket", rem.Bucket}, {"prefix", rem.Prefix},
		{"region", rem.Region}, {"addressing", rem.Addressing},
	} {
		if kv[1] != "" {
			entry.Content = append(entry.Content, str(kv[0]), str(kv[1]))
		}
	}
	entry.Content = append(entry.Content,
		str("requiredForPublication"),
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(rem.RequiredForPublication)},
	)
	remotes.Content = append(remotes.Content, entry)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	cfg, err := repository.LoadConfigBytes(b.Bytes())
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, b.Bytes(), fi.Mode().Perm()); err != nil {
		return err
	}
	r.Config = cfg
	return nil
}

// StorageTest runs the backend capability probe for one remote.
func (r *Repo) StorageTest(ctx context.Context, name string) ([]objectstore.Capability, error) {
	s, _, err := r.Store(name)
	if err != nil {
		return nil, err
	}
	t, ok := s.(objectstore.Tester)
	if !ok {
		return []objectstore.Capability{{Name: "reachable", Supported: true, Detail: "backend has no capability probe"}}, nil
	}
	return t.Test(ctx, r.Domain)
}

// ---------------------------------------------------------------------------
// Objects: pending/published state and explicit publication
// ---------------------------------------------------------------------------

// ObjectState is one ledger row projected for callers.
type ObjectState struct {
	Digest    string
	Bytes     int64
	Pending   bool
	Remotes   map[string]bool
	Referrers []string
}

// PublicationSummary is the repository ledger's unique-object footprint and
// the part of it not yet present on every required remote.
type PublicationSummary struct {
	Objects        int
	Bytes          int64
	PendingObjects int
	PendingBytes   int64
	Remotes        []PublicationRemoteUsage
}

// PublicationRemoteUsage is the repository ledger's recorded unique-object
// footprint for one configured storage backend.
type PublicationRemoteUsage struct {
	Name    string
	Objects int
	Bytes   int64
}

// PublicationSummary reads aggregate publication state without inspecting
// manifests, the cache, the working tree, or any remote store.
func (r *Repo) PublicationSummary(ctx context.Context) (PublicationSummary, error) {
	l, err := r.LedgerIfExists(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return PublicationSummary{}, err
	}
	var st state.PublicationSummary
	if l != nil {
		st, err = l.Summary(ctx, r.RequiredRemotes())
		if err != nil {
			return PublicationSummary{}, err
		}
	}
	out := PublicationSummary{
		Objects: st.Objects, Bytes: st.Bytes,
		PendingObjects: st.PendingObjects, PendingBytes: st.PendingBytes,
		Remotes: []PublicationRemoteUsage{},
	}
	byName := make(map[string]state.PublicationRemoteUsage, len(st.Remotes))
	for _, usage := range st.Remotes {
		byName[usage.Name] = usage
	}
	// The public result describes current storage backends. Include configured
	// remotes with no publication rows as zero rather than leaking removed,
	// historical ledger names into StorageList's current model.
	for _, remote := range r.Remotes() {
		usage := byName[remote.Name]
		out.Remotes = append(out.Remotes, PublicationRemoteUsage{
			Name: remote.Name, Objects: usage.Objects, Bytes: usage.Bytes,
		})
	}
	sort.Slice(out.Remotes, func(i, j int) bool { return out.Remotes[i].Name < out.Remotes[j].Name })
	return out, nil
}

// ObjectsStatus lists every locally ingested object with per-remote state.
func (r *Repo) ObjectsStatus(ctx context.Context) ([]ObjectState, error) {
	l, err := r.LedgerIfExists(ctx)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return []ObjectState{}, nil
		}
		return nil, err
	}
	required := r.RequiredRemotes()
	recs, err := l.All(ctx)
	if err != nil {
		return nil, err
	}
	digests := make([]string, len(recs))
	for i, rec := range recs {
		digests[i] = rec.Digest
	}
	published, err := l.PublishedRemotesMany(ctx, digests)
	if err != nil {
		return nil, err
	}
	referrers, err := l.ReferrersMany(ctx, digests)
	if err != nil {
		return nil, err
	}
	out := make([]ObjectState, 0, len(recs))
	for _, rec := range recs {
		remotes := map[string]bool{}
		for _, n := range required {
			remotes[n] = false
		}
		for n := range published[rec.Digest] {
			remotes[n] = true
		}
		var rs []string
		for _, ref := range referrers[rec.Digest] {
			rs = append(rs, ref[0]+"@"+ref[1])
		}
		out = append(out, ObjectState{Digest: rec.Digest, Bytes: rec.Size, Pending: rec.Pending, Remotes: remotes, Referrers: rs})
	}
	return out, nil
}

// PublishOutcome reports one digest×remote upload.
type PublishOutcome struct {
	Digest string
	Remote string
	Bytes  int64
	Err    error
	// Paths are the repository paths referring to the object, for display.
	Paths []string
}

// largeObjectBytes is where an upload stops being one buffered request and
// becomes a multipart one that holds several part buffers at a time, and
// largeUploads is how many of those may run at once whatever the object
// concurrency is.
const (
	largeObjectBytes = 16 << 20
	largeUploads     = 2
)

// publicationBatch is how many publication rows are committed at once. It is
// a compromise between fsyncs and how much proven work a crash may discard.
const publicationBatch = 256

// PublishItem is one planned upload (digest → remote) with the metadata a
// UI needs to render it before, during, and after transfer.
type PublishItem struct {
	Digest string
	Remote string
	Bytes  int64    // object size (0 when unknown until opened)
	Paths  []string // repository paths referring to the object, for display
	// knownAbsent records that the planner already asked this remote, in one
	// bulk call, and it does not hold the object. Without it every upload
	// re-asked per object, which for thousands of small objects cost more
	// round trips than the transfers themselves.
	knownAbsent bool
}

// isLarge reports that this upload may be a multipart one, which holds
// several part buffers at a time. An unrecorded size counts as large: the
// gate exists to bound memory, and an object whose size is unknown is exactly
// the one that must not be assumed small.
func (p PublishItem) isLarge() bool { return p.Bytes <= 0 || p.Bytes >= largeObjectBytes }

// PublishEvent is the streamed progress record. A consumer receives, in
// order: one Plan event (Items lists every upload with sizes; nothing has
// started), then per item Started / Progress* / Finished-or-Failed, possibly
// interleaved across up to Concurrency items, then one Done event.
type PublishEvent struct {
	Kind        PublishEventKind
	Item        PublishItem // for Started/Progress/Finished/Failed
	Transferred int64       // bytes transferred so far for Item (Progress) or total (Finished)
	Err         error       // Failed only
	Plan        []PublishItem
	TotalBytes  int64 // Plan/Done: sum of planned bytes; Done: bytes uploaded
	Skipped     int   // Done: uploads skipped because the remote already had the object
}

// PublishEventKind enumerates PublishEvent kinds.
type PublishEventKind string

const (
	PublishPlan       PublishEventKind = "plan"
	PublishStarted    PublishEventKind = "started"
	PublishProgressed PublishEventKind = "progress"
	PublishFinished   PublishEventKind = "finished"
	PublishFailed     PublishEventKind = "failed"
	PublishDone       PublishEventKind = "done"
)

// PublishProgress receives per-object events. Deprecated shim over
// PublishEvent kept for callers that only need start/finish; new callers use
// PublishOptions.Events.
type PublishProgress func(digest, remote string, bytes int64, done bool, err error)

// PublishOptions tunes ObjectsPublish.
type PublishOptions struct {
	// Remotes to publish to; empty = every requiredForPublication remote.
	Remotes []string
	// Selection restricts publication to the objects behind the given managed
	// paths, which is what makes a per-asset "upload" control possible: the
	// publish action is reported per asset, so it has to be actionable per
	// asset.
	//
	// Nil means the whole repository, and that is not the same as selecting
	// every path. An object can be pending with no index entry referring to it
	// any more — a path whose content changed again before it was uploaded, or
	// one that has since been deleted — and a repository-wide publish must
	// still upload it, because a Git ref that reaches it is refused until it
	// is there.
	Selection *Selection
	// Concurrency bounds simultaneous uploads (default 4).
	Concurrency int
	// Events receives the streamed plan/progress; may be nil.
	Events func(PublishEvent)
	// ProgressStep is the byte interval between Progress events per item
	// (default 4 MiB).
	ProgressStep int64
}

// ObjectsPublish uploads every recorded object that lacks a publication
// record on the target remotes, records publication in the ledger, and
// settles pins whose required remotes are all satisfied. It never pushes Git
// refs. It first computes the full plan (so a UI can show pending items),
// then uploads with bounded concurrency; ledger writes are serialized. No
// lock is held during transfers.
func (r *Repo) ObjectsPublish(ctx context.Context, remotes []string, progress PublishProgress) (published, failed []PublishOutcome, err error) {
	var events func(PublishEvent)
	if progress != nil {
		events = func(ev PublishEvent) {
			switch ev.Kind {
			case PublishStarted:
				progress(ev.Item.Digest, ev.Item.Remote, 0, false, nil)
			case PublishFinished:
				progress(ev.Item.Digest, ev.Item.Remote, ev.Transferred, true, nil)
			case PublishFailed:
				progress(ev.Item.Digest, ev.Item.Remote, 0, true, ev.Err)
			}
		}
	}
	return r.Publish(ctx, PublishOptions{Remotes: remotes, Events: events})
}

// Publish is ObjectsPublish with streamed events and bounded concurrency.
func (r *Repo) Publish(ctx context.Context, opts PublishOptions) (published, failed []PublishOutcome, err error) {
	remotes := opts.Remotes
	if len(remotes) == 0 {
		remotes = r.RequiredRemotes()
	}
	if len(remotes) == 0 {
		return nil, nil, fmt.Errorf("%w: no remotes are marked requiredForPublication and none were named", ErrInvalidArgument)
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultPublishConcurrency(r, remotes)
	}
	if opts.ProgressStep <= 0 {
		opts.ProgressStep = 4 << 20
	}
	emit := func(ev PublishEvent) {
		if opts.Events != nil {
			opts.Events(ev)
		}
	}
	l, err := r.Ledger(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Plan: every (object, remote) lacking a publication record. One local
	// ledger query; the store is only contacted for planned items.
	candidates, err := l.MissingOn(ctx, remotes)
	if err != nil {
		return nil, nil, err
	}
	if opts.Selection != nil {
		candidates, err = r.restrictToSelection(ctx, candidates, *opts.Selection)
		if err != nil {
			return nil, nil, err
		}
	}
	// Ask every remote once which of the candidates it already holds, when the
	// backend can answer in bulk. Without this an unchanged chunked asset
	// would cost one existence round trip per chunk before uploading nothing.
	onRemote := r.batchPresence(ctx, remotes, candidates)
	// Everything the plan needs about every candidate, in three queries rather
	// than three per candidate: the sizes, who already holds them, and the
	// paths that refer to them.
	records, err := l.ObjectsMany(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	publishedOn, err := l.PublishedRemotesMany(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	referrers, err := l.ReferrerPathsMany(ctx, candidates)
	if err != nil {
		return nil, nil, err
	}
	var plan []PublishItem
	var confirmed []PublishOutcome
	var totalBytes int64
	for _, digest := range candidates {
		rec := records[digest]
		have := publishedOn[digest]
		paths := referrers[digest]
		for _, remote := range remotes {
			if have[remote] {
				continue
			}
			if onRemote[remote][digest] {
				// The remote already holds it; record the fact instead of
				// transferring, and count it as a skipped upload.
				confirmed = append(confirmed, PublishOutcome{Digest: digest, Remote: remote, Paths: paths})
				continue
			}
			plan = append(plan, PublishItem{Digest: digest, Remote: remote, Bytes: rec.Size, Paths: paths,
				knownAbsent: onRemote[remote] != nil})
			totalBytes += rec.Size
		}
	}
	emit(PublishEvent{Kind: PublishPlan, Plan: plan, TotalBytes: totalBytes})

	type result struct {
		item PublishItem
		n    int64
		skip bool
		err  error
	}
	results := make([]result, len(plan))
	sem := make(chan struct{}, opts.Concurrency)
	// A second, much narrower gate for objects big enough to be uploaded in
	// parts. Concurrency counts objects, but memory is bounded by bytes: a
	// multipart upload holds several part buffers at once, so the width that
	// is right for a flood of small chunks would let a handful of whole-blob
	// assets allocate gigabytes between them.
	large := make(chan struct{}, largeUploads)
	var wg sync.WaitGroup
	for i, item := range plan {
		if ctx.Err() != nil {
			results[i] = result{item: item, err: ctx.Err()}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item PublishItem) {
			defer wg.Done()
			defer func() { <-sem }()
			// Taken here rather than by the launcher: a launcher that blocked
			// on this while holding an object slot would stall every smaller
			// upload queued behind one big one.
			if item.isLarge() {
				large <- struct{}{}
				defer func() { <-large }()
			}
			emit(PublishEvent{Kind: PublishStarted, Item: item})
			n, skipped, uerr := r.uploadOne(ctx, item, opts.ProgressStep, func(done int64) {
				emit(PublishEvent{Kind: PublishProgressed, Item: item, Transferred: done})
			})
			results[i] = result{item: item, n: n, skip: skipped, err: uerr}
			if uerr != nil {
				emit(PublishEvent{Kind: PublishFailed, Item: item, Err: uerr})
			} else {
				emit(PublishEvent{Kind: PublishFinished, Item: item, Transferred: n})
			}
		}(i, item)
	}
	wg.Wait()

	// Ledger writes serialized after transfers; failures never abort the batch.
	//
	// They go in batches rather than one transaction per object. The database
	// is opened synchronous=FULL, so a row at a time meant an fsync per
	// object, and publishing a few thousand chunks spent minutes recording
	// work that was already durable in the store. Nothing depended on the
	// finer granularity: no row is written until every transfer has finished,
	// so a crash mid-publish already recorded nothing. Batches are bounded so
	// that a crash near the end still keeps most of what was proven.
	var uploaded int64
	skipped := len(confirmed)
	pending := map[string][]string{}
	npending := 0
	flush := func() error {
		if npending == 0 {
			return nil
		}
		err := l.MarkPublishedMany(ctx, pending)
		pending, npending = map[string][]string{}, 0
		return err
	}
	record := func(digest, remote string) error {
		pending[digest] = append(pending[digest], remote)
		npending++
		if npending >= publicationBatch {
			return flush()
		}
		return nil
	}
	for _, c := range confirmed {
		if err := record(c.Digest, c.Remote); err != nil {
			return published, failed, err
		}
		published = append(published, c)
	}
	for _, res := range results {
		if res.err != nil {
			failed = append(failed, PublishOutcome{Digest: res.item.Digest, Remote: res.item.Remote, Err: res.err, Paths: res.item.Paths})
			continue
		}
		if err := record(res.item.Digest, res.item.Remote); err != nil {
			return published, failed, err
		}
		if res.skip {
			skipped++
		}
		uploaded += res.n
		published = append(published, PublishOutcome{Digest: res.item.Digest, Remote: res.item.Remote, Bytes: res.n, Paths: res.item.Paths})
	}
	if err := flush(); err != nil {
		return published, failed, err
	}
	// Settle pins whose required remotes are complete; unpin in the cache.
	settled, err := l.Settle(ctx, r.RequiredRemotes())
	if err != nil {
		return published, failed, err
	}
	for _, d := range settled {
		_ = r.Cache.Unpin(r.Domain, d)
	}
	emit(PublishEvent{Kind: PublishDone, TotalBytes: uploaded, Skipped: skipped})
	return published, failed, nil
}

// restrictToSelection keeps only the candidate objects that the selected
// managed paths depend on, preserving the ledger's order.
//
// An asset is not one object: a chunked file is a manifest plus a chunk per
// content-defined piece, and two assets routinely share chunks. So the filter
// is over the union of what the selection needs, and publishing one asset can
// legitimately upload an object another asset also uses — content addressing
// means the object belongs to neither of them exclusively.
//
// A selected path whose manifest cannot be resolved contributes nothing rather
// than failing the operation: the same selection's other assets are still
// publishable, and the unresolvable one is reported by verify and doctor, which
// are the commands whose job that is.
func (r *Repo) restrictToSelection(ctx context.Context, candidates []string, sel Selection) ([]string, error) {
	entries, _, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, e := range entries {
		if _, ok := pointerEntryUsable(e); !ok {
			continue
		}
		c, err := r.Content(ctx, e.Pointer)
		if err != nil {
			continue
		}
		for _, o := range c.Objects {
			want[o.Digest] = true
		}
	}
	out := make([]string, 0, len(candidates))
	for _, d := range candidates {
		if want[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// defaultPublishConcurrency picks how many uploads run at once when the
// caller did not say.
//
// A filesystem remote is bound by disk, where more concurrency mostly adds
// seeking. A network remote is bound by round trips: a chunked asset is many
// small objects, and at a typical 50 ms round trip a handful at a time leaves
// the link idle. The figure is matched by the backend's idle-connection pool,
// so the workers reuse connections instead of renegotiating TLS; raising it
// past that would trade round trips for handshakes. Callers that know better
// pass Concurrency explicitly.
func defaultPublishConcurrency(r *Repo, remotes []string) int {
	const local, network = 4, 24
	for _, v := range r.Remotes() {
		for _, name := range remotes {
			if v.Name == name && v.Type != "filesystem" {
				return network
			}
		}
	}
	return local
}

// batchPresence asks each remote, in one call where the backend supports it,
// which candidate digests it already holds. A backend without bulk existence
// (or a remote that cannot be opened) simply contributes nothing and the
// per-object check in uploadOne still applies.
func (r *Repo) batchPresence(ctx context.Context, remotes, candidates []string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(candidates) < 2 {
		return out
	}
	for _, name := range remotes {
		s, _, err := r.Store(name)
		if err != nil {
			continue
		}
		bh, ok := s.(objectstore.BatchHaser)
		if !ok {
			continue
		}
		have, err := bh.BatchHas(ctx, r.Domain, candidates)
		if err != nil {
			continue // fall back to per-object checks during upload
		}
		out[name] = have
	}
	return out
}

// countingReader reports byte deltas as they are read.
type countingReader struct {
	r      io.Reader
	n      *int64
	report func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		*c.n += int64(n)
		c.report(int64(n))
	}
	return n, err
}

// progressReader reports cumulative bytes read at ProgressStep intervals.
type progressReader struct {
	r    io.Reader
	n    int64
	next int64
	step int64
	fn   func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	// Do not let a caller's large buffer jump over an entire reporting
	// interval. Ingest may ask for several MiB at once; without this cap a
	// multi-megabyte item still reports only 0 then complete.
	if p.fn != nil && p.step > 0 && p.next > p.n {
		remaining := p.next - p.n
		if int64(len(b)) > remaining {
			b = b[:remaining]
		}
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.fn != nil && p.n >= p.next {
		p.fn(p.n)
		p.next = p.n + p.step
	}
	return n, err
}

func (r *Repo) uploadOne(ctx context.Context, item PublishItem, step int64, onProgress func(int64)) (n int64, skipped bool, err error) {
	s, _, err := r.Store(item.Remote)
	if err != nil {
		return 0, false, err
	}
	// The planner's bulk existence call already answered for this remote when
	// knownAbsent is set. The store's own Put still checks before writing, so
	// dropping this one leaves exactly one existence probe per object instead
	// of three, and never uploads over something already there.
	if !item.knownAbsent {
		if ok, err := s.Has(ctx, r.Domain, item.Digest); err != nil {
			return 0, false, err
		} else if ok {
			return 0, true, nil
		}
	}
	rd, err := r.Cache.Open(r.Domain, item.Digest)
	if err != nil {
		// Settled objects may have been evicted from the cache; bring the
		// bytes back from any remote that already holds them.
		if ferr := r.FetchObject(ctx, item.Digest); ferr != nil {
			return 0, false, fmt.Errorf("object %s is not in the local cache and no bound remote has it: %w", item.Digest, ferr)
		}
		rd, err = r.Cache.Open(r.Domain, item.Digest)
		if err != nil {
			return 0, false, err
		}
	}
	defer rd.Close()
	size := rd.Size()
	pr := &progressReader{r: rd, step: step, next: step, fn: onProgress}
	if err := s.Put(ctx, r.Domain, item.Digest, size, pr); err != nil {
		return 0, false, err
	}
	return size, false, nil
}

// FetchObject brings an object into the local cache from the first remote in
// read order that has it, verifying the digest while streaming. Returns
// objectstore.ErrNotFound when no configured remote has it.
func (r *Repo) FetchObject(ctx context.Context, digest string) error {
	return r.fetchObject(ctx, digest, nil)
}

// fetchObject is FetchObject with byte-level reporting. onBytes receives
// deltas as the object streams in, which is what makes a per-file bar move
// smoothly instead of jumping once per object: a chunk is up to a megabyte,
// and on a slow link that is many seconds of an apparently frozen row.
//
// An attempt that fails after delivering bytes reports the negative of what
// it delivered, so a retry against another remote cannot double-count.
func (r *Repo) fetchObject(ctx context.Context, digest string, onBytes func(int64)) error {
	if r.Cache.Has(r.Domain, digest) {
		if err := r.Cache.Verify(r.Domain, digest); err == nil {
			return nil
		}
		// Damaged. It is not deleted here: the replacement is written over it
		// once it has actually arrived, so a fetch that fails leaves the
		// object — and its pin — as they were rather than leaving nothing.
	}
	lastErr := error(objectstore.ErrNotFound)
	for _, name := range r.ReadOrder() {
		s, _, err := r.Store(name)
		if err != nil {
			lastErr = err
			continue
		}
		rc, err := s.Get(ctx, r.Domain, digest)
		if err != nil {
			if !errors.Is(err, objectstore.ErrNotFound) {
				lastErr = err
			}
			continue
		}
		counted := int64(0)
		var body io.Reader = rc
		if onBytes != nil {
			body = &countingReader{r: rc, n: &counted, report: onBytes}
		}
		// The expected digest goes in: content that is not this object is
		// discarded while still temporary and never reaches the cache. The
		// old shape published whatever arrived under its own digest and then
		// deleted that entry on mismatch — which deleted a perfectly good,
		// possibly pinned, possibly sole copy of an object nobody had asked
		// about, on the word of the remote that misbehaved.
		_, perr := r.Cache.PutReplacing(r.Domain, digest, body)
		_ = rc.Close()
		undo := func() {
			if onBytes != nil && counted > 0 {
				onBytes(-counted)
			}
		}
		if perr != nil {
			undo()
			if errors.Is(perr, cache.ErrCorrupt) {
				lastErr = fmt.Errorf("remote %s did not return %s: %w", name, digest, perr)
			} else {
				lastErr = perr
			}
			continue
		}
		return nil
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// CacheSummary describes cache usage for this repository's storage domain.
type CacheSummary struct {
	Dir            string
	TotalBytes     int64
	PinnedBytes    int64
	EvictableBytes int64
	Objects        int
	Pinned         int
	// LimitBytes is the automatic-trim budget this clone configured (0 = none).
	LimitBytes int64
}

// CacheStatus summarizes cache usage across all domains and pins.
func (r *Repo) CacheStatus(ctx context.Context) (CacheSummary, error) {
	infos, err := r.Cache.List()
	if err != nil {
		return CacheSummary{}, err
	}
	s := CacheSummary{Dir: r.Cache.Root(), LimitBytes: r.CacheLimit()}
	for _, in := range infos {
		s.Objects++
		s.TotalBytes += in.Size
		if in.Pinned {
			s.Pinned++
			s.PinnedBytes += in.Size
		} else {
			s.EvictableBytes += in.Size
		}
	}
	return s, nil
}

// CachePrune evicts least-recently-used unpinned objects until the cache is
// at or below targetBytes (0 = evict everything evictable). Pinned (pending
// publication) objects are never removed. dryRun only plans.
func (r *Repo) CachePrune(ctx context.Context, targetBytes int64, dryRun bool) ([]CacheEviction, int, error) {
	h, err := r.Lock(ctx, journal.LockCachePrune, "cache")
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = h.Release() }()
	plan, kept, err := r.Cache.PlanPrune(targetBytes)
	if err != nil {
		return nil, 0, err
	}
	out := make([]CacheEviction, 0, len(plan))
	for _, in := range plan {
		ev := CacheEviction{Domain: in.Domain, Digest: in.Digest, Bytes: in.Size}
		if !dryRun {
			if err := r.Cache.Remove(in.Domain, in.Digest, false); err != nil {
				ev.Err = err
			}
		}
		out = append(out, ev)
	}
	return out, kept, nil
}

// CacheEviction is one planned or executed cache removal.
type CacheEviction struct {
	Domain string
	Digest string
	Bytes  int64
	Err    error
}
