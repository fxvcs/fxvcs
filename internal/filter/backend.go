package filter

import (
	"context"
	"io"

	"fxvcs.dev/fxvcs/internal/pointer"
)

// Backend is the engine the Git process boundary delegates to for everything
// that must agree with the SDK operations: ingestion (cache pin + publication
// ledger row through the single ingestion path), hydration intent recorded in
// state.db, remote fetches, and the publication ledger consulted by the
// pre-push barrier. It is implemented over app.Repo by internal/filter/wire;
// this package never imports the engine so the dependency stays one-way.
//
// Every method is bounded local work except FetchObject and MissingRemotes,
// which may talk to configured remotes; the caller holds no lock around them.
type Backend interface {
	// Ingest records content as pending publication and returns the
	// deterministic pointer (identical bytes → identical pointer).
	Ingest(ctx context.Context, content io.Reader, path string) (pointer.Pointer, error)
	// HydrationIntent reports the intent recorded for path. known is false
	// when no row (or no state database) exists: the unhydrated default.
	HydrationIntent(ctx context.Context, path string) (hydrated, known bool, err error)
	// RecordHydrated notes, best effort, that the working file at path holds
	// the real bytes of digest (the clean filter just ingested them from it).
	RecordHydrated(ctx context.Context, path, digest string, size int64) error
	// FetchObject brings digest into the local cache from a configured
	// remote, verifying while streaming. objectstore.ErrNotFound when none
	// has it.
	FetchObject(ctx context.Context, digest string) error
	// ContentObjects returns every object digest the pointer depends on: the
	// file itself for whole/1, the manifest and its chunks for a chunked
	// encoding. It may need the manifest, so it can touch a remote.
	ContentObjects(ctx context.Context, ptr pointer.Pointer) ([]string, error)
	// OpenContent returns the whole file behind a pointer, fetching whatever
	// is missing and verifying every byte against the manifest and the
	// pointer. Any error means "no valid content"; the caller must not write
	// a partial result.
	OpenContent(ctx context.Context, ptr pointer.Pointer) (io.ReadCloser, error)
	// MissingRemotes returns the requiredForPublication remotes that hold no
	// publication record for the pointer's object.
	MissingRemotes(ctx context.Context, ptr pointer.Pointer) ([]string, error)
	// MissingRemotesMany is MissingRemotes for many pointers at once, keyed by
	// pointer oid. The barrier uses it instead of a loop: against a network
	// object store, one query per asset is one sequential round trip per
	// asset, paid before any upload starts.
	MissingRemotesMany(ctx context.Context, ptrs []pointer.Pointer) (map[string][]string, error)
	// RequiredRemotes lists the requiredForPublication remote names.
	RequiredRemotes() []string
	// Publish uploads the given pending objects (by digest) to every
	// requiredForPublication remote bound on this machine and records the
	// result in the ledger. It returns per-object failures instead of
	// stopping at the first, and never pushes Git refs. log receives
	// human-readable progress (Git shows hook stderr to the user).
	Publish(ctx context.Context, digests []string, log io.Writer) []PublishFailure
	// Close releases the backend (databases, locks).
	Close() error
}

// PublishFailure reports one object×remote upload that did not succeed.
type PublishFailure struct {
	Digest string
	Remote string
	Err    error
}
