// Package materialize owns the verified digest-keyed file store beneath a
// target deployment root and safe placement into out-of-place realizations.
package materialize

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/integrity"
	"fxvcs.dev/fxvcs/internal/layout"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

var (
	ErrCorrupt      = errors.New("materialize: stored file is corrupt")
	ErrSizeMismatch = errors.New("materialize: size mismatch")
	ErrDestination  = errors.New("materialize: destination already exists")
)

type Store struct{ DeployRoot string }

type PersistentKind string

const (
	PersistentFile      PersistentKind = "file"
	PersistentDirectory PersistentKind = "directory"
)

type Result struct {
	Path    string
	Bytes   int64
	Existed bool
}

func (s Store) Path(digest string) (string, error) {
	return layout.MaterializedPath(s.DeployRoot, digest)
}

// Verify reads the entire stored ordinary file. Existence or a matching file
// name is never accepted as proof of content identity.
func (s Store) Verify(digest string, size int64) error {
	if !integrity.IsDigest(digest) {
		return fmt.Errorf("materialize: invalid digest %q", digest)
	}
	path, err := s.Path(digest)
	if err != nil {
		return err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not an ordinary file", ErrCorrupt, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(linked, info) {
		return fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, path)
	}
	if size >= 0 && info.Size() != size {
		return fmt.Errorf("%w: %s has %d bytes, want %d", ErrSizeMismatch, path, info.Size(), size)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("%w: %s hashes to %s, want %s", ErrCorrupt, path, got, digest)
	}
	if size >= 0 && n != size {
		return fmt.Errorf("%w: read %d bytes, want %d", ErrSizeMismatch, n, size)
	}
	return nil
}

// Put streams exactly size bytes into a same-directory temporary file, hashes
// while writing, fsyncs, makes it read-only, publishes without replacement,
// then fsyncs the parent directory. A concurrent winner is accepted only after
// its full bytes are verified.
func (s Store) Put(ctx context.Context, digest string, size int64, src io.Reader) (Result, error) {
	if !integrity.IsDigest(digest) || size < 0 {
		return Result{}, fmt.Errorf("materialize: invalid digest or size")
	}
	final, err := s.Path(digest)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Lstat(final); err == nil {
		if err := s.Verify(digest, size); err != nil {
			return Result{}, err
		}
		return Result{Path: final, Bytes: size, Existed: true}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Result{}, err
	}
	tmp, err := os.CreateTemp(parent, ".materialize-*")
	if err != nil {
		return Result{}, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	h := sha256.New()
	limited := io.LimitReader(contextReader{ctx: ctx, r: src}, size+1)
	n, err := io.Copy(io.MultiWriter(tmp, h), limited)
	if err != nil {
		return Result{}, err
	}
	if n != size {
		return Result{}, fmt.Errorf("%w: received %d bytes, want %d", ErrSizeMismatch, n, size)
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return Result{}, fmt.Errorf("%w: received %s, want %s", ErrCorrupt, got, digest)
	}
	if err := tmp.Sync(); err != nil {
		return Result{}, err
	}
	if err := tmp.Chmod(0o444); err != nil {
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	// Hard-link publication is atomic and fails when final already exists; it
	// never replaces a digest-named file whose bytes have not been proved.
	if err := os.Link(tmpPath, final); err != nil {
		if _, statErr := os.Lstat(final); statErr == nil {
			if verifyErr := s.Verify(digest, size); verifyErr != nil {
				return Result{}, verifyErr
			}
			return Result{Path: final, Bytes: size, Existed: true}, nil
		}
		return Result{}, fmt.Errorf("materialize: publish: %w", err)
	}
	if err := syncDir(parent); err != nil {
		return Result{}, err
	}
	return Result{Path: final, Bytes: size}, nil
}

// PlaceImmutable verifies the store object then hard-links it into a new
// realization. It refuses every pre-existing destination.
func (s Store) PlaceImmutable(digest string, size int64, realizationDir, relativePath string) error {
	if err := s.Verify(digest, size); err != nil {
		return err
	}
	source, _ := s.Path(digest)
	destination, err := pathsafe.Join(realizationDir, relativePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := os.Link(source, destination); err != nil {
		if _, statErr := os.Lstat(destination); statErr == nil {
			return fmt.Errorf("%w: %s", ErrDestination, destination)
		}
		return err
	}
	return syncDir(filepath.Dir(destination))
}

// BindPersistent creates a target-scoped persistent directory and installs a
// relative symlink at an exact realization-relative path. Both paths are
// containment checked and a collision is refused.
func (s Store) BindPersistent(realizationDir, repositoryID, resourcePath, declared, realizationRelative string, kind PersistentKind) error {
	return s.BindPersistentForPublication(realizationDir, realizationDir, repositoryID, resourcePath, declared, realizationRelative, kind)
}

// BindPersistentForPublication creates the symlink in stagingDir but computes
// its relative target from publishedDir. Atomic directory publication may
// change path depth, so a link made relative to staging itself would escape or
// point at the wrong shared root after rename.
func (s Store) BindPersistentForPublication(stagingDir, publishedDir, repositoryID, resourcePath, declared, realizationRelative string, kind PersistentKind) error {
	persistent, err := layout.PersistentPath(s.DeployRoot, repositoryID, resourcePath, declared)
	if err != nil {
		return err
	}
	destination, err := pathsafe.Join(stagingDir, realizationRelative)
	if err != nil {
		return err
	}
	publishedDestination, err := pathsafe.Join(publishedDir, realizationRelative)
	if err != nil {
		return err
	}
	switch kind {
	case PersistentFile:
		if err := os.MkdirAll(filepath.Dir(persistent), 0o750); err != nil {
			return err
		}
	case PersistentDirectory:
		if err := os.MkdirAll(persistent, 0o750); err != nil {
			return err
		}
	default:
		return fmt.Errorf("materialize: invalid persistent kind %q", kind)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("%w: %s", ErrDestination, destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(publishedDestination), persistent)
	if err != nil {
		return err
	}
	if filepath.IsAbs(rel) || rel == "." || strings.TrimSpace(rel) == "" {
		return errors.New("materialize: invalid persistent symlink target")
	}
	if err := os.Symlink(rel, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("materialize: fsync directory %s: %w", path, err)
	}
	return nil
}
