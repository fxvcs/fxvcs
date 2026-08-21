package sdk

import (
	"context"

	"fxvcs.dev/fxvcs/internal/app"
)

// ---- objects ---------------------------------------------------------------

type ObjectsStatusRequest struct {
	Path string `json:"path"`
}

type ObjectPublicationState struct {
	Digest    string          `json:"digest"`
	Bytes     int64           `json:"bytes"`
	Pending   bool            `json:"pending"`
	Remotes   map[string]bool `json:"remotes"` // remote name -> published
	Referrers []string        `json:"referrers,omitempty"`
}

type ObjectsStatusResult struct {
	Objects []ObjectPublicationState `json:"objects"`
}

type PublicationSummaryRequest struct {
	Path string `json:"path"`
}

// PublicationSummaryResult reports unique objects known to the repository's
// publication ledger. Pending objects are missing from at least one currently
// required remote, regardless of whether their original cache pin was settled.
type PublicationSummaryResult struct {
	Objects        int                      `json:"objects"`
	Bytes          int64                    `json:"bytes"`
	PendingObjects int                      `json:"pendingObjects"`
	PendingBytes   int64                    `json:"pendingBytes"`
	Remotes        []PublicationRemoteUsage `json:"remotes"`
}

// PublicationRemoteUsage is the ledger-recorded unique-object footprint on a
// configured remote. The same object is counted once on every remote recorded
// as holding it, so remote Bytes are not additive across replicated backends.
// It is not a live measurement of physical backend usage.
type PublicationRemoteUsage struct {
	Name    string `json:"name"`
	Objects int    `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

// PublicationSummary returns cheap ledger-only totals. It does not inspect
// manifests, cache entries, working-tree files, or remote stores.
func (c *Client) PublicationSummary(ctx context.Context, req PublicationSummaryRequest) (*PublicationSummaryResult, error) {
	r, err := c.openRepo(ctx, req.Path, "publication summary")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	st, err := r.PublicationSummary(ctx)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	out := &PublicationSummaryResult{
		Objects: st.Objects, Bytes: st.Bytes,
		PendingObjects: st.PendingObjects, PendingBytes: st.PendingBytes,
		Remotes: make([]PublicationRemoteUsage, 0, len(st.Remotes)),
	}
	for _, remote := range st.Remotes {
		out.Remotes = append(out.Remotes, PublicationRemoteUsage{
			Name: remote.Name, Objects: remote.Objects, Bytes: remote.Bytes,
		})
	}
	return out, nil
}

// ObjectsStatus lists locally ingested objects and their per-remote
// publication state from the shared ledger.
func (c *Client) ObjectsStatus(ctx context.Context, req ObjectsStatusRequest) (*ObjectsStatusResult, error) {
	r, err := c.openRepo(ctx, req.Path, "objects status")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	states, err := r.ObjectsStatus(ctx)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &ObjectsStatusResult{Objects: make([]ObjectPublicationState, 0, len(states))}
	for _, s := range states {
		res.Objects = append(res.Objects, ObjectPublicationState{Digest: s.Digest, Bytes: s.Bytes, Pending: s.Pending, Remotes: s.Remotes, Referrers: s.Referrers})
	}
	return res, nil
}

type ObjectsPublishRequest struct {
	Path    string   `json:"path"`
	Remotes []string `json:"remotes,omitempty"` // empty = all requiredForPublication
	// Selector restricts the upload to the objects behind the named managed
	// paths. It is what makes the per-asset publish action in StatusEntry
	// actionable: an affordance reported per asset has to be performable per
	// asset, or a control labelled with one file's name would publish the
	// whole repository.
	//
	// Empty means every pending object, which is not the same as naming every
	// path: an object can still be pending with no index entry referring to it
	// — content that changed again before it was uploaded, or a path since
	// deleted — and a push that reaches it is refused until it is in storage.
	// So a whole-repository publish stays the way to be sure a push will pass.
	Selector PathSelector `json:"selector,omitempty"`
	// Concurrency bounds simultaneous uploads (0 = default 4).
	Concurrency int `json:"concurrency,omitempty"`
}

type ObjectsPublishResult struct {
	// Published and Failed are per stored object.
	Published []ItemOutcome `json:"published"`
	Failed    []ItemOutcome `json:"failed"`
	// Assets counts the distinct repository paths whose data was uploaded and
	// Objects the stored objects it took: a chunked file is one asset and
	// many objects.
	Assets  int `json:"assets"`
	Objects int `json:"objects"`
}

// ObjectsPublish uploads pending objects to the required (or named) remotes,
// records publication, and settles pins. It never pushes Git refs.
func (c *Client) ObjectsPublish(ctx context.Context, req ObjectsPublishRequest) (*ObjectsPublishResult, error) {
	r, err := c.openRepo(ctx, req.Path, "objects publish")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	const op = "objects publish"
	c.progress(Event{Operation: op, Kind: EventOperationStarted})
	// Progress is reported per *asset*, not per stored object: one chunked
	// file is a manifest plus dozens of chunks, and a user watching a push
	// wants one row per file. The transfer itself still runs per object.
	agg := newPublishAggregator(func(ev Event) { ev.Operation = op; c.progress(ev) })
	opts := app.PublishOptions{Remotes: req.Remotes, Concurrency: req.Concurrency, Events: agg.handle}
	if len(req.Selector.Paths) > 0 {
		sel := selection(req.Selector)
		opts.Selection = &sel
	}
	pub, failed, err := r.Publish(ctx, opts)
	res := &ObjectsPublishResult{Published: []ItemOutcome{}, Failed: []ItemOutcome{}}
	assets := map[string]bool{}
	for _, p := range pub {
		res.Published = append(res.Published, ItemOutcome{Path: p.Remote, Digest: p.Digest, Bytes: p.Bytes, Paths: p.Paths})
		for _, path := range p.Paths {
			assets[path] = true
		}
	}
	for _, f := range failed {
		res.Failed = append(res.Failed, ItemOutcome{Path: f.Remote, Digest: f.Digest, Reason: f.Err.Error(), Paths: f.Paths})
	}
	res.Assets, res.Objects = len(assets), len(pub)
	c.progress(Event{Operation: op, Kind: EventOperationFinished})
	if err != nil {
		return res, translateError(r.Root, err)
	}
	return res, nil
}

// ---- cache -----------------------------------------------------------------

type CacheStatusRequest struct {
	Path string `json:"path"`
}

type CacheStatusResult struct {
	CacheDir       string `json:"cacheDir"`
	TotalBytes     int64  `json:"totalBytes"`
	PinnedBytes    int64  `json:"pinnedBytes"` // pending publication; not evictable
	EvictableBytes int64  `json:"evictableBytes"`
	Objects        int    `json:"objects"`
	Pinned         int    `json:"pinned"`
	// LimitBytes is the size the cache is trimmed back to automatically on
	// this machine (0 = no automatic trimming).
	LimitBytes int64 `json:"limitBytes,omitempty"`
}

// CacheStatus reports cache usage and pinned (pending) bytes.
func (c *Client) CacheStatus(ctx context.Context, req CacheStatusRequest) (*CacheStatusResult, error) {
	r, err := c.openRepo(ctx, req.Path, "cache status")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	s, err := r.CacheStatus(ctx)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	return &CacheStatusResult{CacheDir: s.Dir, TotalBytes: s.TotalBytes, PinnedBytes: s.PinnedBytes,
		EvictableBytes: s.EvictableBytes, Objects: s.Objects, Pinned: s.Pinned, LimitBytes: s.LimitBytes}, nil
}

type CacheLimitRequest struct {
	Path string `json:"path"`
	// LimitBytes is the size the cache is trimmed back to after operations
	// that add objects. Zero removes the limit.
	LimitBytes int64 `json:"limitBytes"`
}

type CacheLimitResult struct {
	LimitBytes int64 `json:"limitBytes"`
	// CurrentBytes is what the cache holds right now.
	CurrentBytes int64 `json:"currentBytes"`
}

// CacheLimit sets how much disk this machine lets the shared object cache use.
// It is a clone-local preference and is never committed. Objects pending
// publication are pinned and are never evicted, so the cache can exceed the
// limit until they are published.
func (c *Client) CacheLimit(ctx context.Context, req CacheLimitRequest) (*CacheLimitResult, error) {
	r, err := c.openRepo(ctx, req.Path, "cache limit")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := r.SetCacheLimit(ctx, req.LimitBytes); err != nil {
		return nil, translateError(r.Root, err)
	}
	s, err := r.CacheStatus(ctx)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	return &CacheLimitResult{LimitBytes: s.LimitBytes, CurrentBytes: s.TotalBytes}, nil
}

type CacheVerifyRequest struct {
	Path string `json:"path"`
	// AllDomains verifies every storage domain in the cache, not only this
	// repository's.
	AllDomains bool `json:"allDomains,omitempty"`
	// Repair removes corrupt objects so the next operation refetches them.
	Repair bool `json:"repair,omitempty"`
}

type CacheCorruptObject struct {
	Domain string `json:"domain"`
	Digest string `json:"digest"`
	Bytes  int64  `json:"bytes"`
	Pinned bool   `json:"pinned"`
	Reason string `json:"reason"`
}

type CacheVerifyResult struct {
	Checked int   `json:"checked"`
	Bytes   int64 `json:"bytes"`
	// Corrupt lists every object whose bytes no longer match its digest.
	Corrupt []CacheCorruptObject `json:"corrupt"`
	// Removed counts corrupt objects Repair deleted; Kept lists corrupt
	// objects left in place, which happens when they are pending publication
	// and deleting them would destroy the only copy.
	Removed int                  `json:"removed"`
	Kept    []CacheCorruptObject `json:"kept,omitempty"`
}

// CacheVerify re-reads cached objects and checks them against their digests.
// Objects are normally verified when they are read, so this is what catches
// silent disk corruption in data nothing has needed for a while.
func (c *Client) CacheVerify(ctx context.Context, req CacheVerifyRequest) (*CacheVerifyResult, error) {
	r, err := c.openRepo(ctx, req.Path, "cache verify")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	const op = "cache verify"
	c.progress(Event{Operation: op, Kind: EventOperationStarted})
	rep, err := r.CacheVerify(ctx, app.CacheVerifyOptions{AllDomains: req.AllDomains, Repair: req.Repair}, func(p app.CacheVerifyProgress) {
		if p.Digest == "" {
			c.progress(Event{Operation: op, Kind: EventPlan, Phase: PhaseVerify, Count: p.Total})
			c.progress(Event{Operation: op, Kind: EventStage, Stage: StageVerifying, Count: p.Total})
			return
		}
		kind := EventItemFinished
		if p.Started {
			kind = EventItemStarted
		} else if p.Err != nil {
			kind = EventItemFailed
		}
		ev := Event{Operation: op, Kind: kind, Phase: PhaseVerify, Digest: p.Digest,
			Bytes: p.Bytes, Total: p.Bytes, Index: p.Done, Count: p.Total}
		if p.Err != nil {
			ev.Code, ev.Message = CodeIntegrity, p.Err.Error()
		}
		c.progress(ev)
	})
	c.progress(Event{Operation: op, Kind: EventOperationFinished})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &CacheVerifyResult{Checked: rep.Checked, Bytes: rep.Bytes, Removed: rep.Removed, Corrupt: []CacheCorruptObject{}}
	for _, c := range rep.Corrupt {
		res.Corrupt = append(res.Corrupt, CacheCorruptObject{Domain: c.Domain, Digest: c.Digest, Bytes: c.Bytes, Pinned: c.Pinned, Reason: c.Reason})
	}
	for _, c := range rep.Kept {
		res.Kept = append(res.Kept, CacheCorruptObject{Domain: c.Domain, Digest: c.Digest, Bytes: c.Bytes, Pinned: c.Pinned, Reason: c.Reason})
	}
	return res, nil
}

type CachePruneRequest struct {
	Path        string `json:"path"`
	TargetBytes int64  `json:"targetBytes,omitempty"`
	DryRun      bool   `json:"dryRun"`
}

type CachePruneResult struct {
	Evicted []ItemOutcome `json:"evicted"`
	Kept    int           `json:"kept"`
	DryRun  bool          `json:"dryRun"`
}

// CachePrune evicts LRU unpinned objects down to TargetBytes. Pinned objects
// (pending publication) are never evicted. DryRun plans only.
func (c *Client) CachePrune(ctx context.Context, req CachePruneRequest) (*CachePruneResult, error) {
	r, err := c.openRepo(ctx, req.Path, "cache prune")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	plan, kept, err := r.CachePrune(ctx, req.TargetBytes, req.DryRun)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &CachePruneResult{Evicted: []ItemOutcome{}, Kept: kept, DryRun: req.DryRun}
	for _, e := range plan {
		it := ItemOutcome{Path: e.Domain, Digest: e.Digest, Bytes: e.Bytes}
		if e.Err != nil {
			it.Skipped = true
			it.Reason = e.Err.Error()
		}
		res.Evicted = append(res.Evicted, it)
	}
	return res, nil
}

// ---- storage ---------------------------------------------------------------

type StorageRemoteSpec struct {
	Name                   string `json:"name"`
	Type                   string `json:"type"` // filesystem | s3
	Endpoint               string `json:"endpoint,omitempty"`
	Bucket                 string `json:"bucket,omitempty"`
	Prefix                 string `json:"prefix,omitempty"`
	Region                 string `json:"region,omitempty"`     // s3
	Addressing             string `json:"addressing,omitempty"` // s3: path | virtual-host
	LocalPath              string `json:"localPath,omitempty"`  // filesystem, clone-local binding
	RequiredForPublication bool   `json:"requiredForPublication"`
	Local                  bool   `json:"local"` // clone-local only, never committed
	Committed              bool   `json:"committed"`
	Bound                  bool   `json:"bound"` // usable on this machine
	// CredentialProfile names the stored login an s3 remote uses on this
	// machine; HasCredentials reports whether that entry exists. Neither ever
	// carries a secret.
	CredentialProfile string `json:"credentialProfile,omitempty"`
	HasCredentials    bool   `json:"hasCredentials,omitempty"`
}

type StorageAddRequest struct {
	Path   string            `json:"path"`
	Remote StorageRemoteSpec `json:"remote"`
}

type StorageAddResult struct {
	Remote StorageRemoteSpec `json:"remote"`
}

// StorageAdd registers a clone-local remote or binds a committed remote to a
// machine-specific location. It never edits committed configuration.
func (c *Client) StorageAdd(ctx context.Context, req StorageAddRequest) (*StorageAddResult, error) {
	r, err := c.openRepo(ctx, req.Path, "storage add")
	if err != nil {
		// Outside a component checkout this may still be a coordination
		// repository, which needs storage bindings of its own so release
		// creation can verify that assets are already uploaded.
		if CodeOf(err) == CodeNotRepository {
			if res, ok := c.storageBindCoordination(ctx, req); ok {
				return res, nil
			}
		}
		return nil, err
	}
	defer r.Close()
	v, err := r.StorageAdd(ctx, app.StorageAddInput{
		Name: req.Remote.Name, Type: req.Remote.Type, Path: req.Remote.LocalPath,
		Bucket: req.Remote.Bucket, Endpoint: req.Remote.Endpoint, Prefix: req.Remote.Prefix,
		Region: req.Remote.Region, Addressing: req.Remote.Addressing,
		Local: req.Remote.Local, RequiredForPublication: req.Remote.RequiredForPublication,
	})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	return &StorageAddResult{Remote: viewToSpec(v)}, nil
}

// storageBindCoordination records a local storage binding in a coordination
// repository. It reports false when this is not one, so the caller can return
// the original "not an fxvcs repository" error.
func (c *Client) storageBindCoordination(ctx context.Context, req StorageAddRequest) (*StorageAddResult, bool) {
	co, err := c.openCoord(ctx, req.Path, "storage add")
	if err != nil {
		return nil, false
	}
	v, err := co.StorageBind(app.StorageAddInput{
		Name: req.Remote.Name, Type: req.Remote.Type, Path: req.Remote.LocalPath,
		Bucket: req.Remote.Bucket, Endpoint: req.Remote.Endpoint, Prefix: req.Remote.Prefix,
		Region: req.Remote.Region, Addressing: req.Remote.Addressing,
	})
	if err != nil {
		return nil, false
	}
	return &StorageAddResult{Remote: viewToSpec(v)}, true
}

func viewToSpec(v app.RemoteView) StorageRemoteSpec {
	return StorageRemoteSpec{Name: v.Name, Type: v.Type, Endpoint: v.Endpoint, Bucket: v.Bucket, Prefix: v.Prefix,
		Region: v.Region, Addressing: v.Addressing, LocalPath: v.Path,
		RequiredForPublication: v.RequiredForPublication, Local: v.Local, Committed: v.Committed, Bound: v.Bound,
		CredentialProfile: v.CredentialProfile, HasCredentials: v.HasCredentials}
}

type StorageLoginRequest struct {
	Path   string `json:"path"`
	Remote string `json:"remote"`
	// AccessKeyID and SecretAccessKey are the provider keys. The caller is
	// responsible for collecting the secret safely; this package never reads
	// stdin and never prompts.
	AccessKeyID     string `json:"accessKeyID,omitempty"`
	SecretAccessKey string `json:"-"`
	SessionToken    string `json:"-"`
	// Profile overrides the credential-store entry name (default
	// "<storageDomainID>/<remote>").
	Profile string `json:"profile,omitempty"`
	// Forget removes the stored credentials instead of writing them.
	Forget bool `json:"forget,omitempty"`
}

type StorageLoginResult struct {
	Remote string `json:"remote"`
	// Profile is the credential-store entry that was written or removed.
	Profile string `json:"profile"`
	// CredentialsFile is where the identity is stored on this machine.
	CredentialsFile string `json:"credentialsFile"`
	// AccessKeyID echoes the non-secret half so a person can confirm it.
	AccessKeyID string `json:"accessKeyID,omitempty"`
	Removed     bool   `json:"removed,omitempty"`
}

// StorageLogin records (or forgets) the credentials for an S3-compatible
// remote on this machine. Secrets are written to the user configuration
// directory with restrictive permissions — never to the repository, never to
// committed configuration, and never to a result, a log, or an event.
func (c *Client) StorageLogin(ctx context.Context, req StorageLoginRequest) (*StorageLoginResult, error) {
	r, err := c.openRepo(ctx, req.Path, "storage login")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	res, err := r.StorageLogin(ctx, app.StorageLoginInput{
		Remote: req.Remote, AccessKeyID: req.AccessKeyID, SecretAccessKey: req.SecretAccessKey,
		SessionToken: req.SessionToken, Profile: req.Profile, Forget: req.Forget,
	})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	return &StorageLoginResult{Remote: res.Remote, Profile: res.Profile, CredentialsFile: res.CredentialsFile,
		AccessKeyID: res.AccessKeyID, Removed: res.Removed}, nil
}

type StorageListRequest struct {
	Path string `json:"path"`
}

type StorageListResult struct {
	Remotes []StorageRemoteSpec `json:"remotes"`
}

// StorageList merges committed and clone-local remotes.
func (c *Client) StorageList(ctx context.Context, req StorageListRequest) (*StorageListResult, error) {
	r, err := c.openRepo(ctx, req.Path, "storage list")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	res := &StorageListResult{Remotes: []StorageRemoteSpec{}}
	for _, v := range r.Remotes() {
		res.Remotes = append(res.Remotes, viewToSpec(v))
	}
	return res, nil
}

type StorageTestRequest struct {
	Path   string `json:"path"`
	Remote string `json:"remote"`
}

type StorageCapability struct {
	Name      string `json:"name"`
	Supported bool   `json:"supported"`
	Detail    string `json:"detail,omitempty"`
}

type StorageTestResult struct {
	Remote       string              `json:"remote"`
	Capabilities []StorageCapability `json:"capabilities"`
}

// StorageTest runs the backend capability and permission probe.
func (c *Client) StorageTest(ctx context.Context, req StorageTestRequest) (*StorageTestResult, error) {
	r, err := c.openRepo(ctx, req.Path, "storage test")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	caps, err := r.StorageTest(ctx, req.Remote)
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &StorageTestResult{Remote: req.Remote, Capabilities: []StorageCapability{}}
	for _, cp := range caps {
		res.Capabilities = append(res.Capabilities, StorageCapability{Name: cp.Name, Supported: cp.Supported, Detail: cp.Detail})
	}
	return res, nil
}

// ---- storage statistics -----------------------------------------------------

type StatsRequest struct {
	Path     string       `json:"path"`
	Selector PathSelector `json:"selector"`
	// Fetch allows resolving object manifests that are not stored locally,
	// at one request each. Off by default: a report should not quietly
	// download a repository's worth of metadata.
	Fetch bool `json:"fetch,omitempty"`
	// TopShared limits the most-reused objects returned (0 = none).
	TopShared int `json:"topShared,omitempty"`
}

// SharedObject is one stored object that more than one asset uses.
type SharedObject struct {
	Digest     string   `json:"digest"`
	Bytes      int64    `json:"bytes"`
	Users      int      `json:"users"`
	SavedBytes int64    `json:"savedBytes"`
	Paths      []string `json:"paths,omitempty"`
}

// StatsResult reports what content addressing saves in this repository,
// measured against storing every tracked file's bytes once per path.
//
// It is one repository's numbers. Savings across repositories are not
// reported because they do not exist: one repository is one storage domain
// and content is never deduplicated across access boundaries.
type StatsResult struct {
	Assets   int `json:"assets"`
	Contents int `json:"contents"`

	LogicalBytes int64   `json:"logicalBytes"`
	StoredBytes  int64   `json:"storedBytes"`
	SavedBytes   int64   `json:"savedBytes"`
	SavedPercent float64 `json:"savedPercent"`

	// The saving by mechanism: whole files that are byte-identical, and
	// pieces shared between files that only resemble each other.
	IdenticalFiles     int   `json:"identicalFiles"`
	IdenticalFileBytes int64 `json:"identicalFileBytes"`
	FilesSharingParts  int   `json:"filesSharingParts"`
	SharedPartBytes    int64 `json:"sharedPartBytes"`
	SharedObjects      int   `json:"sharedObjects"`

	ChunkedAssets int `json:"chunkedAssets"`
	WholeAssets   int `json:"wholeAssets"`

	// Unresolved assets could not be examined because their manifest is not
	// stored locally; while it is non-zero the savings are a lower bound.
	Unresolved int `json:"unresolved"`

	TopShared []SharedObject `json:"topShared,omitempty"`
}

// Stats measures deduplication across the tracked assets of one repository.
func (c *Client) Stats(ctx context.Context, req StatsRequest) (*StatsResult, error) {
	r, err := c.openRepo(ctx, req.Path, "stats")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	st, err := r.StorageStats(ctx, selection(req.Selector), app.StorageStatsOptions{Fetch: req.Fetch})
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	res := &StatsResult{
		Assets: st.Assets, Contents: st.Contents,
		LogicalBytes: st.LogicalBytes, StoredBytes: st.StoredBytes,
		SavedBytes: st.SavedBytes(), SavedPercent: st.SavedFraction() * 100,
		IdenticalFiles: st.IdenticalFiles, IdenticalFileBytes: st.IdenticalFileBytes,
		FilesSharingParts: st.FilesSharingParts, SharedPartBytes: st.SharedPartBytes,
		SharedObjects: st.SharedObjects, ChunkedAssets: st.ChunkedAssets,
		WholeAssets: st.WholeAssets, Unresolved: st.Unresolved,
	}
	if req.TopShared > 0 {
		top, err := r.TopSharedObjects(ctx, selection(req.Selector), req.TopShared)
		if err != nil {
			return nil, translateError(r.Root, err)
		}
		for _, s := range top {
			res.TopShared = append(res.TopShared, SharedObject{
				Digest: s.Digest, Bytes: s.Bytes, Users: s.Users, SavedBytes: s.SavedBytes, Paths: s.Paths,
			})
		}
	}
	return res, nil
}
