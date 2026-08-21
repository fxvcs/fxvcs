// Package wire connects the Git process boundary (internal/filter) to the
// engine (internal/app) so that the clean filter ingests through the same
// path as every SDK operation, smudge honours the hydration intent recorded
// in state.db, and the pre-push barrier consults the publication ledger.
// It exists so internal/app never imports internal/filter's process code and
// internal/filter never imports internal/app.
package wire

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// Connector returns the filter.ProcessOptions.Connect function that opens
// the engine for the resolved working tree.
func Connector(installed format.Version, cacheDir, operation string) func(ctx context.Context, root string) (filter.Backend, error) {
	return func(ctx context.Context, root string) (filter.Backend, error) {
		// The filter runs inside someone else's Git command, and the paths it
		// touches are exactly the ones a concurrent load or unload holds, so
		// it waits like every other operator-facing entry point rather than
		// failing the Git command outright.
		r, err := app.Open(ctx, root, app.Options{Installed: installed, Operation: operation, CacheDir: cacheDir, LockTimeout: app.DefaultLockTimeout})
		if err != nil {
			return nil, err
		}
		return &backend{r: r}, nil
	}
}

// Backend adapts an opened app.Repo to filter.Backend.
func Backend(r *app.Repo) filter.Backend { return &backend{r: r} }

type backend struct{ r *app.Repo }

// relative maps a pathname Git handed to the filter onto the repository-
// relative form the engine expects. Git normally sends worktree-relative
// names, but some entry points (`git hash-object --path`, third-party tools)
// send absolute paths; those are accepted only when they lie inside the
// working tree. Anything else is returned unchanged and rejected downstream.
func (b *backend) relative(path string) string {
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(b.r.Root, path)
	if err != nil {
		if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
			rel, err = filepath.Rel(b.r.Root, resolved)
		}
	}
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

func (b *backend) Ingest(ctx context.Context, content io.Reader, path string) (pointer.Pointer, error) {
	ing, err := b.r.Ingest(ctx, content, b.relative(path))
	if err != nil {
		return pointer.Pointer{}, err
	}
	return ing.Pointer, nil
}

func (b *backend) HydrationIntent(ctx context.Context, path string) (bool, bool, error) {
	return b.r.HydrationIntent(ctx, b.relative(path))
}

func (b *backend) RecordHydrated(ctx context.Context, path, digest string, size int64) error {
	return b.r.RecordHydrated(ctx, b.relative(path), digest, size)
}

func (b *backend) FetchObject(ctx context.Context, digest string) error {
	return b.r.FetchObject(ctx, digest)
}

func (b *backend) ContentObjects(ctx context.Context, ptr pointer.Pointer) ([]string, error) {
	return b.r.ContentDigests(ctx, ptr)
}

func (b *backend) OpenContent(ctx context.Context, ptr pointer.Pointer) (io.ReadCloser, error) {
	if _, err := b.r.EnsureContent(ctx, ptr, nil); err != nil {
		return nil, err
	}
	return b.r.OpenContent(ctx, ptr)
}

func (b *backend) MissingRemotes(ctx context.Context, ptr pointer.Pointer) ([]string, error) {
	return b.r.PublicationMissing(ctx, ptr)
}

func (b *backend) MissingRemotesMany(ctx context.Context, ptrs []pointer.Pointer) (map[string][]string, error) {
	return b.r.PublicationMissingMany(ctx, ptrs)
}

func (b *backend) RequiredRemotes() []string { return b.r.RequiredRemotes() }

func (b *backend) Publish(ctx context.Context, digests []string, log io.Writer) []filter.PublishFailure {
	want := map[string]bool{}
	for _, d := range digests {
		want[d] = true
	}
	var failures []filter.PublishFailure
	_, failed, err := b.r.ObjectsPublish(ctx, nil, func(digest, remote string, n int64, done bool, perr error) {
		if !want[digest] || log == nil {
			return
		}
		switch {
		case perr != nil:
			fmt.Fprintf(log, "fxvcs pre-push: %s -> %s: %v\n", short(digest), remote, perr)
		case done:
			fmt.Fprintf(log, "fxvcs pre-push: published %s -> %s (%d bytes)\n", short(digest), remote, n)
		}
	})
	for _, f := range failed {
		if want[f.Digest] {
			failures = append(failures, filter.PublishFailure{Digest: f.Digest, Remote: f.Remote, Err: f.Err})
		}
	}
	if err != nil {
		for d := range want {
			failures = append(failures, filter.PublishFailure{Digest: d, Remote: "*", Err: err})
		}
	}
	return failures
}

func short(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func (b *backend) Close() error { return b.r.Close() }
