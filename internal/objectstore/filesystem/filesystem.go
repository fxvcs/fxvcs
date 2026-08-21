// Package filesystem is the filesystem-backed object store: a directory tree
// (local disk, NFS/SMB share, or a synced folder) laid out as
//
//	<root>/<storageDomainID>/objects/<aa>/<sha256-hex>
//
// Writes go to <root>/<domain>/tmp/ then rename atomically; the digest is
// verified while streaming so a corrupt or truncated upload never becomes
// visible under its claimed name.
package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

var (
	digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	domainRE = regexp.MustCompile(`^domain_[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
)

// Store implements objectstore.Store over a directory.
type Store struct{ root string }

// New opens (creating if needed) a filesystem store at root. root must be
// absolute.
func New(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("filesystem store: root %q must be absolute", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: filepath.Clean(root)}, nil
}

// Root returns the store directory.
func (s *Store) Root() string { return s.root }

func split(domain, digest string) (string, error) {
	if !domainRE.MatchString(domain) {
		return "", fmt.Errorf("%w: %q", objectstore.ErrInvalidDomain, domain)
	}
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("%w: %q", objectstore.ErrInvalidDigest, digest)
	}
	return m[1], nil
}

func (s *Store) objectPath(domain, hexPart string) string {
	return filepath.Join(s.root, domain, "objects", hexPart[:2], hexPart)
}

// Has reports whether the object exists.
func (s *Store) Has(_ context.Context, domain, digest string) (bool, error) {
	h, err := split(domain, digest)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(s.objectPath(domain, h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
	}
	return st.Mode().IsRegular(), nil
}

// Put streams r into a temp file, verifies the digest/size, and renames.
func (s *Store) Put(ctx context.Context, domain, digest string, size int64, r io.Reader) error {
	h, err := split(domain, digest)
	if err != nil {
		return err
	}
	dst := s.objectPath(domain, h)
	if ok, err := s.Has(ctx, domain, digest); err != nil {
		return err
	} else if ok {
		// Immutable: identical identity means identical content; drain nothing.
		return nil
	}
	tmpDir := filepath.Join(s.root, domain, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
	}
	tmp, err := os.CreateTemp(tmpDir, h[:8]+"-*.part")
	if err != nil {
		return fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), r)
	if err != nil {
		cleanup()
		return err
	}
	if size >= 0 && n != size {
		cleanup()
		return fmt.Errorf("filesystem store: declared size %d, got %d bytes", size, n)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != h {
		cleanup()
		return fmt.Errorf("filesystem store: content digest sha256:%s does not match %s", got, digest)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// A concurrent identical Put may have won; that is success.
		if ok, _ := s.Has(ctx, domain, digest); ok {
			_ = os.Remove(tmpName)
			return nil
		}
		_ = os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Get opens the stored object.
func (s *Store) Get(_ context.Context, domain, digest string) (io.ReadCloser, error) {
	h, err := split(domain, digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(s.objectPath(domain, h))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, objectstore.ErrNotFound
		}
		return nil, fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
	}
	return f, nil
}

// Test probes the directory for the capabilities `storage test` reports.
func (s *Store) Test(ctx context.Context, domain string) ([]objectstore.Capability, error) {
	if !domainRE.MatchString(domain) {
		return nil, fmt.Errorf("%w: %q", objectstore.ErrInvalidDomain, domain)
	}
	caps := []objectstore.Capability{}
	add := func(name string, ok bool, detail string) {
		caps = append(caps, objectstore.Capability{Name: name, Supported: ok, Detail: detail})
	}
	st, err := os.Stat(s.root)
	if err != nil || !st.IsDir() {
		add("reachable", false, fmt.Sprint(err))
		return caps, nil
	}
	add("reachable", true, s.root)
	probeDir := filepath.Join(s.root, domain, "tmp")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		add("writable", false, err.Error())
		return caps, nil
	}
	f, err := os.CreateTemp(probeDir, "probe-*.part")
	if err != nil {
		add("writable", false, err.Error())
		return caps, nil
	}
	name := f.Name()
	_, werr := f.WriteString("probe")
	_ = f.Close()
	target := filepath.Join(probeDir, filepath.Base(name)+".renamed")
	rerr := os.Rename(name, target)
	_ = os.Remove(target)
	_ = os.Remove(name)
	add("writable", werr == nil, "")
	add("atomic-rename", rerr == nil, "")
	// Read-back through Put/Get of a tiny object under a probe digest.
	payload := []byte("fxvcs-storage-test")
	sum := sha256.Sum256(payload)
	dg := "sha256:" + hex.EncodeToString(sum[:])
	perr := s.Put(ctx, domain, dg, int64(len(payload)), bytesReader(payload))
	ok := perr == nil
	if ok {
		rc, gerr := s.Get(ctx, domain, dg)
		if gerr == nil {
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			ok = string(b) == string(payload)
		} else {
			ok = false
		}
	}
	add("put-get-roundtrip", ok, "")
	return caps, nil
}
