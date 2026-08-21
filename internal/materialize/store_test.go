package materialize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func bytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPutVerifyAndPlaceImmutable(t *testing.T) {
	root := t.TempDir()
	store := Store{DeployRoot: root}
	data := []byte("verified materialized bytes")
	digest := bytesDigest(data)
	result, err := store.Put(context.Background(), digest, int64(len(data)), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if result.Existed || result.Path == "" {
		t.Fatalf("unexpected put result: %#v", result)
	}
	if err := store.Verify(digest, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	again, err := store.Put(context.Background(), digest, int64(len(data)), bytes.NewReader([]byte("ignored because existing bytes are verified")))
	if err != nil || !again.Existed {
		t.Fatalf("existing put: %#v, %v", again, err)
	}
	realization := filepath.Join(root, "releases", "rz_test")
	if err := store.PlaceImmutable(digest, int64(len(data)), realization, "resources/core/file.bin"); err != nil {
		t.Fatal(err)
	}
	sourceInfo, _ := os.Stat(result.Path)
	destInfo, _ := os.Stat(filepath.Join(realization, "resources", "core", "file.bin"))
	if !os.SameFile(sourceInfo, destInfo) {
		t.Fatal("immutable placement was not a hard link")
	}
	if err := store.PlaceImmutable(digest, int64(len(data)), realization, "resources/core/file.bin"); !errors.Is(err, ErrDestination) {
		t.Fatalf("want destination collision, got %v", err)
	}
}

func TestPutRejectsWrongSizeAndDigestWithoutPublishing(t *testing.T) {
	store := Store{DeployRoot: t.TempDir()}
	data := []byte("content")
	digest := bytesDigest(data)
	if _, err := store.Put(context.Background(), digest, int64(len(data)-1), bytes.NewReader(data)); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("want size mismatch, got %v", err)
	}
	path, _ := store.Path(digest)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong-size content was published: %v", err)
	}
	wrong := bytesDigest([]byte("different"))
	if _, err := store.Put(context.Background(), wrong, int64(len(data)), bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want corrupt error, got %v", err)
	}
}

func TestExistingCorruptionAndSymlinkFailClosed(t *testing.T) {
	store := Store{DeployRoot: t.TempDir()}
	data := []byte("expected")
	digest := bytesDigest(data)
	path, _ := store.Path(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("damaged!"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), digest, int64(len(data)), bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("existing corruption accepted: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(digest, int64(len(data))); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("symlink accepted as stored object: %v", err)
	}
}

func TestBindPersistentUsesContainedRelativeSymlink(t *testing.T) {
	root := t.TempDir()
	store := Store{DeployRoot: root}
	realization := filepath.Join(root, "releases", "rz_test")
	destination := filepath.Join(realization, "resources", "core", "data", "state.db")
	if err := store.BindPersistent(realization, "repo_core", "data", "state.db", "resources/core/data/state.db", PersistentFile); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(destination)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) {
		t.Fatalf("persistent link is absolute: %q", target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(destination), target))
	want := filepath.Join(root, "shared", "persistent", "repo_core", "data", "state.db")
	if resolved != want {
		t.Fatalf("link resolves to %q, want %q", resolved, want)
	}
	if _, err := os.Stat(filepath.Dir(want)); err != nil {
		t.Fatalf("persistent parent missing: %v", err)
	}
	if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file binding should not invent persistent file content: %v", err)
	}
}
