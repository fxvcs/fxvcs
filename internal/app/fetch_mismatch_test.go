package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/objectstore"
)

// wrongObjectStore answers every read with the bytes of one other object,
// modelling a remote that is misconfigured, has a corrupted index, or is
// simply serving someone else's namespace.
type wrongObjectStore struct {
	inner objectstore.Store
	body  []byte
}

func (w *wrongObjectStore) Has(ctx context.Context, domain, digest string) (bool, error) {
	return true, nil
}

func (w *wrongObjectStore) Put(ctx context.Context, domain, digest string, size int64, r io.Reader) error {
	return w.inner.Put(ctx, domain, digest, size, r)
}

func (w *wrongObjectStore) Get(ctx context.Context, domain, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(w.body)), nil
}

// TestFetchingTheWrongObjectDoesNotTouchTheOneItReturned is the regression for
// a remote's mistake destroying local data.
//
// A fetch used to store whatever arrived under its own digest and then, on
// finding it was not what was asked for, delete that entry — force, so the pin
// went too. The bytes were valid content for some other object, which may be
// pending publication and may have no copy anywhere else. Nothing the remote
// says should be able to remove a local object nobody asked about.
func TestFetchingTheWrongObjectDoesNotTouchTheOneItReturned(t *testing.T) {
	t.Parallel()
	repo, _, path := setupUnloadable(t, 3<<20)
	ctx := context.Background()
	ptr := indexPointerFor(t, repo, path)
	c, err := repo.Content(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	var bystander ContentObject
	for _, o := range c.Objects {
		if o.Kind != ObjectKindManifest {
			bystander = o
			break
		}
	}
	// The bystander is pinned and unpublished: the cache is its only copy.
	forgetPublication(t, repo, bystander.Digest)
	if err := repo.Cache.Pin(repo.Domain, bystander.Digest); err != nil {
		t.Fatal(err)
	}
	rd, err := repo.Cache.Open(repo.Domain, bystander.Digest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	_ = rd.Close()

	inner, _, err := repo.Store("primary")
	if err != nil {
		t.Fatal(err)
	}
	repo.setStore("primary", &wrongObjectStore{inner: inner, body: body})

	// Ask for something else entirely.
	missing := "sha256:" + strings.Repeat("1", 64)
	ferr := repo.FetchObject(ctx, missing)
	if ferr == nil {
		t.Fatal("a remote serving the wrong object was accepted")
	}
	if repo.Cache.Verify(repo.Domain, bystander.Digest) != nil {
		t.Fatal("the object the remote wrongly returned was damaged or removed")
	}
	if pinned, err := repo.Cache.IsPinned(repo.Domain, bystander.Digest); err != nil || !pinned {
		t.Fatalf("the bystander lost its pin (pinned=%v err=%v)", pinned, err)
	}
	if repo.Cache.Has(repo.Domain, missing) {
		t.Fatal("content that was not the requested object was published anyway")
	}
	// And it is reported as what it is. A remote that answers with the wrong
	// content is broken; calling it "not found" points the operator at
	// publishing an object that is already there.
	if errors.Is(ferr, objectstore.ErrNotFound) {
		t.Fatalf("a wrong-content response was reported as a missing object: %v", ferr)
	}
	if !errors.Is(ferr, cache.ErrCorrupt) {
		t.Fatalf("the integrity failure was not preserved in the error chain: %v", ferr)
	}
	// The same must hold through the path callers actually use.
	cerr := repo.ensureCached(ctx, missing)
	if cerr == nil {
		t.Fatal("ensureCached accepted a remote serving the wrong object")
	}
	if !errors.Is(cerr, ErrIntegrity) {
		t.Fatalf("ensureCached classified a wrong-content response as %v, not an integrity failure", cerr)
	}
	if errors.Is(cerr, ErrObjectUnavailable) {
		t.Fatalf("ensureCached reported a broken remote as a missing object: %v", cerr)
	}
}
