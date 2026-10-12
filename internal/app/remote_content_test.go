package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/repository"
)

// publishedStore ingests content, publishes it to a filesystem remote and
// returns that remote's directory with the pointers, as a deploy tool would
// find them: a store on one side, pointer bytes from Git on the other.
func publishedStore(t *testing.T, contents map[string][]byte) (string, map[string]pointer.Pointer) {
	t.Helper()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	ptrs := map[string]pointer.Pointer{}
	for path, content := range contents {
		ing, err := repo.Ingest(ctx, bytes.NewReader(content), path)
		if err != nil {
			t.Fatal(err)
		}
		ptrs[path] = ing.Pointer
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	return store, ptrs
}

func testConfig(t *testing.T, yaml string) *domain.Repository {
	t.Helper()
	cfg, err := repository.LoadConfigBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readRemote(src *RemoteSource, ptr pointer.Pointer) ([]byte, error) {
	rc, err := src.Open(context.Background(), ptr)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// TestRemoteSourceReadsWithoutCheckoutOrCache reads whole and chunked content
// with nothing but the committed configuration, a store binding and pointers.
func TestRemoteSourceReadsWithoutCheckoutOrCache(t *testing.T) {
	contents := map[string][]byte{
		"stream/small.ytd": asset(11, 200<<10),
		"stream/big.ydr":   asset(12, 5<<20),
	}
	store, ptrs := publishedStore(t, contents)
	if ptrs["stream/big.ydr"].Encoding == pointer.EncodingWhole {
		t.Fatal("test needs a chunked asset")
	}
	src, err := OpenRemoteSource(RemoteSourceInput{Config: testConfig(t, repoYAML), Paths: map[string]string{"primary": store}})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range contents {
		got, err := readRemote(src, ptrs[path])
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: content differs", path)
		}
	}
}

// TestRemoteSourceFailsClosed: a damaged object, a missing object and a
// pointer from another domain are errors, never content.
func TestRemoteSourceFailsClosed(t *testing.T) {
	ctx := context.Background()
	contents := map[string][]byte{"stream/big.ydr": asset(13, 4<<20)}
	store, ptrs := publishedStore(t, contents)
	ptr := ptrs["stream/big.ydr"]
	open := func() *RemoteSource {
		src, err := OpenRemoteSource(RemoteSourceInput{Config: testConfig(t, repoYAML), Paths: map[string]string{"primary": store}})
		if err != nil {
			t.Fatal(err)
		}
		return src
	}

	other := ptr
	other.StorageDomainID = "domain_other"
	if _, err := open().Open(ctx, other); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("foreign domain: got %v, want ErrIntegrity", err)
	}

	// Corrupt one chunk in place: same length, different bytes.
	var objects []string
	_ = filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, string(filepath.Separator)+"objects"+string(filepath.Separator)) {
			objects = append(objects, p)
		}
		return nil
	})
	var chunk string
	for _, p := range objects {
		if !strings.HasSuffix(p, strings.TrimPrefix(ptr.ManifestDigest, "sha256:")) {
			chunk = p
			break
		}
	}
	data, err := os.ReadFile(chunk)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(chunk, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemote(open(), ptr); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt chunk: got %v, want ErrIntegrity", err)
	}

	if err := os.Remove(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemote(open(), ptr); !errors.Is(err, ErrObjectUnavailable) {
		t.Fatalf("missing chunk: got %v, want ErrObjectUnavailable", err)
	}
}

// TestRemoteSourceFallsBackAcrossRemotes reads an object from the second
// remote when the publication remote does not have it, and skips filesystem
// remotes this machine has no directory for.
func TestRemoteSourceFallsBackAcrossRemotes(t *testing.T) {
	contents := map[string][]byte{"stream/a.ytd": asset(14, 300<<10)}
	store, ptrs := publishedStore(t, contents)
	cfg := testConfig(t, repoYAML)
	cfg.Spec.Storage.Remotes = []domain.StorageRemote{
		{Name: "empty", Type: "filesystem", RequiredForPublication: true},
		{Name: "unbound", Type: "filesystem"},
		{Name: "mirror", Type: "filesystem"},
	}
	src, err := OpenRemoteSource(RemoteSourceInput{Config: cfg,
		Paths: map[string]string{"empty": t.TempDir(), "mirror": store}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(src.Remotes(), ","); got != "empty,mirror" {
		t.Fatalf("remotes = %s, want empty,mirror", got)
	}
	got, err := readRemote(src, ptrs["stream/a.ytd"])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents["stream/a.ytd"]) {
		t.Fatal("content differs")
	}

	if _, err := OpenRemoteSource(RemoteSourceInput{Config: cfg}); !errors.Is(err, ErrRemoteUnbound) {
		t.Fatalf("no bindings: got %v, want ErrRemoteUnbound", err)
	}
}
