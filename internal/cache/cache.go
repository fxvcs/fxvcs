// Package cache is the host-wide, immutable, content-addressed object cache.
//
// Layout (root defaults to <os.UserCacheDir()>/fxvcs, override with the
// FXVCS_CACHE_DIR environment variable or Open):
//
//	<root>/objects/<storageDomainID>/<hh>/<hh>/<64 hex>   object bytes
//	<root>/pins/<storageDomainID>/<64 hex>                pending-publication marker
//	<root>/published/<storageDomainID>/<64 hex>/<remote>  publication marker per remote
//	<root>/tmp/                                           in-flight writes
//
// Objects are addressed by the sha256 of their bytes; a stored object is
// therefore immutable and can be shared by every clone and worktree on the
// host. Concurrency across processes relies only on atomic rename: a writer
// streams into tmp/, fsyncs, then renames into place. Two writers racing on the
// same digest both produce identical bytes, so whichever rename wins is
// correct and the loser discards its temp file. No lock files are needed.
//
// Readers verify: Open returns a reader that recomputes the digest and fails
// at EOF on mismatch, so a corrupt cache entry can never be handed out as
// valid content.
//
// Pins mark objects that were ingested locally (git add) but not yet published
// to every required remote. Prune never evicts a pinned object. Publication
// markers are the temporary stand-in for the publication ledger
// ($GIT_COMMON_DIR/fxvcs/publication.db); the pre-push hook consults them.
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// EnvDir is the environment variable that overrides the cache location.
const EnvDir = "FXVCS_CACHE_DIR"

// touchInterval is how stale an object's mtime must be before Open refreshes
// it. Prune's LRU order only has to be approximately right; an hour of
// granularity keeps it meaningful while making the touch a rare event rather
// than one metadata write per object read.
const touchInterval = time.Hour

// Errors.
var (
	ErrNotFound      = errors.New("cache: object not found")
	ErrCorrupt       = errors.New("cache: object bytes do not match digest")
	ErrInvalidDigest = errors.New("cache: invalid digest")
	ErrInvalidDomain = errors.New("cache: invalid storage domain id")
)

var (
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	domainRE = regexp.MustCompile(`^domain_[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
)

// CorruptError names the object whose bytes did not match, so a caller that
// can repair the cache knows what to evict. Verification happens while bytes
// stream, and by then the digest under suspicion is several call frames away
// from whoever can act on it; carrying it in the error is what makes an
// automatic evict-and-refetch possible instead of failing the whole operation.
//
// errors.Is(err, ErrCorrupt) matches it.
type CorruptError struct {
	Digest string // the object that was expected
	Got    string // the digest its bytes actually hash to ("" if not computed)
	Size   int64  // bytes actually read
	Want   int64  // bytes expected
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("cache: object %s does not match: got %s (%d bytes, want %d)", e.Digest, e.Got, e.Size, e.Want)
}

func (e *CorruptError) Unwrap() error { return ErrCorrupt }

// Cache is a handle on one cache root. It holds no open files or locks; a
// zero-cost value that can be created per process.
type Cache struct {
	root string
	// written counts object bytes this handle has stored. It lets a caller
	// with a size budget tell that it cannot possibly be over it without
	// measuring the cache, which means walking every object — a walk that
	// costs the size of the cache and was being paid once per ingested file.
	written atomic.Int64
}

// BytesWritten reports how many object bytes this handle has stored since it
// was opened. It is a lower bound on growth, not a measurement of the cache:
// other processes share it, so a caller must still measure eventually.
func (c *Cache) BytesWritten() int64 { return c.written.Load() }

// DefaultDir returns FXVCS_CACHE_DIR if set, else <UserCacheDir>/fxvcs.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		return filepath.Clean(d), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache: locate user cache dir: %w", err)
	}
	return filepath.Join(base, "fxvcs"), nil
}

// Open creates (if needed) and returns the cache at root. root=="" uses
// DefaultDir.
func Open(root string) (*Cache, error) {
	if root == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		root = d
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"objects", "pins", "published", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("cache: create %s: %w", sub, err)
		}
	}
	return &Cache{root: root}, nil
}

// Root returns the absolute cache directory.
func (c *Cache) Root() string { return c.root }

// Digest formats a raw sha256 sum as "sha256:<hex>".
func Digest(sum []byte) string { return "sha256:" + hex.EncodeToString(sum) }

func validate(domain, digest string) (hexPart string, err error) {
	if !domainRE.MatchString(domain) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	if !digestRE.MatchString(digest) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDigest, digest)
	}
	return strings.TrimPrefix(digest, "sha256:"), nil
}

func (c *Cache) objectPath(domain, hexPart string) string {
	return filepath.Join(c.root, "objects", domain, hexPart[:2], hexPart[2:4], hexPart)
}

func (c *Cache) pinPath(domain, hexPart string) string {
	return filepath.Join(c.root, "pins", domain, hexPart)
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// PutResult describes a stored object.
type PutResult struct {
	Digest string // sha256:<hex>
	Size   int64
	// Existed is true when an identical object was already present.
	Existed bool
}

// Put streams r into the cache under domain and returns its digest. Bytes are
// written to a temp file, fsynced, then atomically renamed to their final
// path; an interrupted Put leaves at most a temp file under tmp/ and never a
// visible object.
func (c *Cache) Put(domain string, r io.Reader) (PutResult, error) {
	if !domainRE.MatchString(domain) {
		return PutResult{}, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	return c.writeObject(domain, r, "", false)
}

// PutReplacing stores a stream the caller expects to be digest, overwriting a
// damaged copy of that object.
//
// It exists so that repairing an object never destroys the only copy first.
// Put treats an existing key as success and writes nothing, so a repair had to
// delete the damaged entry before fetching — and a fetch that then failed left
// nothing at all, along with a dropped pin. Writing with replacement keeps the
// damaged bytes until the good ones are in hand, and swaps them atomically.
//
// The expected digest is required, and content that does not match it is
// discarded rather than stored: a caller repairing one object must not be able
// to write, or replace, a different one on a remote's say-so. A mismatch is
// ErrCorrupt and the cache is untouched.
func (c *Cache) PutReplacing(domain, digest string, r io.Reader) (PutResult, error) {
	if !domainRE.MatchString(domain) {
		return PutResult{}, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	if _, err := validate(domain, digest); err != nil {
		return PutResult{}, err
	}
	return c.writeObject(domain, r, digest, true)
}

// PutBytes stores content already held in memory.
//
// It exists because Put cannot know an object's identity until it has read the
// whole stream, so it writes and fsyncs a temporary file before discovering
// that the object was already cached. That is the wrong shape for a caller
// that holds the bytes: re-ingesting an unchanged file then rewrote every
// chunk it already had. Hashing first costs nothing extra — Put hashes anyway
// — and turns the common "already present" case into a stat.
func (c *Cache) PutBytes(domain string, data []byte) (PutResult, error) {
	if !domainRE.MatchString(domain) {
		return PutResult{}, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	sum := sha256.Sum256(data)
	digest := Digest(sum[:])
	if c.present(domain, digest, int64(len(data))) {
		return PutResult{Digest: digest, Size: int64(len(data)), Existed: true}, nil
	}
	return c.writeObject(domain, bytes.NewReader(data), digest, false)
}

// PutBytesChecked is PutBytes for an object small enough that confirming the
// stored copy is cheaper than being wrong about it.
//
// PutBytes trusts identity and size, which is right for bulk content that is
// hashed again on the way out. A manifest is different: it is the map to
// everything else, it is a few kilobytes, and a damaged one makes its whole
// asset unreadable rather than one chunk. So the existing entry is read, and
// replaced when it does not match.
func (c *Cache) PutBytesChecked(domain string, data []byte) (PutResult, error) {
	if !domainRE.MatchString(domain) {
		return PutResult{}, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	sum := sha256.Sum256(data)
	digest := Digest(sum[:])
	if c.present(domain, digest, int64(len(data))) {
		if err := c.Verify(domain, digest); err == nil {
			return PutResult{Digest: digest, Size: int64(len(data)), Existed: true}, nil
		}
	}
	return c.writeObject(domain, bytes.NewReader(data), digest, true)
}

// PutExpecting stores a stream whose digest the caller already knows, and
// skips the write when the cache already holds that object at that size.
//
// The stream is still read and hashed in full: callers use this to prove that
// the bytes in hand are the bytes the cache claims to have — unload is about
// to destroy them — so skipping the read would defeat the purpose. What it
// skips is the temporary file, the fsync and the rename, which is the part
// that made re-deriving an unchanged file expensive.
//
// A stream that does not hash to expect is ErrCorrupt and nothing is stored.
func (c *Cache) PutExpecting(domain, expect string, size int64, r io.Reader) (PutResult, error) {
	if !domainRE.MatchString(domain) {
		return PutResult{}, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	if _, err := validate(domain, expect); err != nil {
		return PutResult{}, err
	}
	if !c.present(domain, expect, size) {
		// Absent, or there at the wrong size. Either way the entry that is
		// there is not the object, so it is replaced rather than kept.
		return c.writeObject(domain, r, expect, true)
	}
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return PutResult{}, err
	}
	if got := Digest(h.Sum(nil)); got != expect || n != size {
		return PutResult{}, &CorruptError{Digest: expect, Got: got, Size: n, Want: size}
	}
	return PutResult{Digest: expect, Size: n, Existed: true}, nil
}

// PutVerified stores bytes whose identity the caller has already established,
// replacing whatever is at that key.
//
// It exists for repair. Put treats an existing key as success without looking
// at it — objects are immutable, so identity implies content — but that
// reasoning only holds while the stored bytes are the bytes that were stored.
// A caller holding a proven copy of the content (unload, reading the working
// file it is about to destroy) needs to be able to put the cache right, not to
// be told the damaged entry is fine.
func (c *Cache) PutVerified(domain, digest string, data []byte) error {
	if !domainRE.MatchString(domain) {
		return fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	if _, err := validate(domain, digest); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if got := Digest(sum[:]); got != digest {
		return &CorruptError{Digest: digest, Got: got, Size: int64(len(data)), Want: int64(len(data))}
	}
	_, err := c.writeObject(domain, bytes.NewReader(data), digest, true)
	return err
}

// present reports that the object is cached at exactly the expected size. A
// negative size means "any size", which is only correct for callers with no
// recorded size to compare against.
func (c *Cache) present(domain, digest string, size int64) bool {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return false
	}
	st, err := os.Stat(c.objectPath(domain, hexPart))
	if err != nil || st.IsDir() {
		return false
	}
	return size < 0 || st.Size() == size
}

// HasSized reports that the object is cached and is exactly size bytes.
//
// This is the cheap presence predicate for hot paths. It is not a substitute
// for Verify: it catches a truncated or replaced entry, not bytes that rotted
// in place. Callers rely on it only where the bytes are hashed again before
// anything observable happens to them — see the package doc on reader
// verification.
func (c *Cache) HasSized(domain, digest string, size int64) bool {
	return c.present(domain, digest, size)
}

// writeObject streams r into a temporary file, then renames it into place
// under the digest of what was read.
//
// expect, when set, is the digest the caller claims the stream has. Content
// that hashes to anything else is discarded and never published: a caller who
// named an object is fetching or repairing that one, and bytes for a
// different object are not a lucky bonus — publishing them lets a
// misbehaving remote reach into an entry nobody asked about.
//
// replace overwrites an entry that is already there instead of keeping it,
// and is only honoured together with expect. You may replace the object you
// named; you may not replace one you happened to receive.
func (c *Cache) writeObject(domain string, r io.Reader, expect string, replace bool) (PutResult, error) {
	tmp, err := os.CreateTemp(filepath.Join(c.root, "tmp"), "put-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("cache: create temp: %w", err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return PutResult{}, fmt.Errorf("cache: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return PutResult{}, fmt.Errorf("cache: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return PutResult{}, fmt.Errorf("cache: close temp: %w", err)
	}
	digest := Digest(h.Sum(nil))
	if expect != "" && digest != expect {
		// Nothing is published; the temp file goes with the deferred cleanup.
		return PutResult{}, &CorruptError{Digest: expect, Got: digest, Size: n, Want: n}
	}
	replace = replace && expect != ""
	hexPart := digest[len("sha256:"):]
	final := c.objectPath(domain, hexPart)
	if st, err := os.Stat(final); err == nil && !st.IsDir() && !replace {
		os.Remove(tmpName)
		success = true
		return PutResult{Digest: digest, Size: n, Existed: true}, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return PutResult{}, fmt.Errorf("cache: create object dir: %w", err)
	}
	if replace && runtime.GOOS == "windows" {
		// Stored objects are mode 0444, and on Windows that is the read-only
		// attribute, which makes a replacing rename fail rather than
		// overwrite. Clear the way first, or a repair would report success
		// while leaving the damaged bytes exactly where they were.
		//
		// Only on Windows: elsewhere rename replaces atomically, and removing
		// first would open a window in which the object is absent to every
		// other process sharing this cache — turning a repair into a spurious
		// not-found, or into a missing object if the process dies mid-way.
		_ = os.Chmod(final, 0o644)
		if err := os.Remove(final); err != nil && !errors.Is(err, os.ErrNotExist) {
			return PutResult{}, fmt.Errorf("cache: replace object: %w", err)
		}
	}
	// Objects are read-only once visible.
	_ = os.Chmod(tmpName, 0o444)
	if err := os.Rename(tmpName, final); err != nil {
		// A concurrent writer may have won; that is fine because bytes are
		// identical. Anything else is a real error — and when the caller
		// asked for a replacement, finding something there is the failure,
		// not the success: it is what was supposed to be overwritten.
		if st, serr := os.Stat(final); serr == nil && !st.IsDir() && !replace {
			os.Remove(tmpName)
			success = true
			return PutResult{Digest: digest, Size: n, Existed: true}, nil
		}
		return PutResult{}, fmt.Errorf("cache: publish object: %w", err)
	}
	success = true
	syncDir(filepath.Dir(final))
	c.written.Add(n)
	return PutResult{Digest: digest, Size: n}, nil
}

// syncDir fsyncs a directory where the platform supports it (best effort).
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

// Has reports whether the object file exists (without verifying its bytes).
func (c *Cache) Has(domain, digest string) bool {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return false
	}
	st, err := os.Stat(c.objectPath(domain, hexPart))
	return err == nil && !st.IsDir()
}

// Info describes a cached object.
type Info struct {
	Digest  string
	Size    int64
	ModTime time.Time
	Pinned  bool
}

// Stat returns metadata for an object; ErrNotFound if absent.
func (c *Cache) Stat(domain, digest string) (Info, error) {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Stat(c.objectPath(domain, hexPart))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	pinned, _ := c.IsPinned(domain, digest)
	return Info{Digest: digest, Size: st.Size(), ModTime: st.ModTime(), Pinned: pinned}, nil
}

// Reader streams an object and verifies its digest as bytes pass through.
// Read returns ErrCorrupt (instead of io.EOF) if the full content does not
// hash to the expected digest, and also if the file is shorter or longer than
// recorded. Callers must treat any error as "no valid content".
type Reader struct {
	f       *os.File
	h       hash.Hash
	want    string
	n       int64
	size    int64
	done    bool
	failure error
}

// Read implements io.Reader with verify-at-EOF semantics.
func (r *Reader) Read(p []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	if r.done {
		return 0, io.EOF
	}
	n, err := r.f.Read(p)
	if n > 0 {
		r.h.Write(p[:n])
		r.n += int64(n)
	}
	if err == io.EOF {
		r.done = true
		if got := Digest(r.h.Sum(nil)); got != r.want || r.n != r.size {
			r.failure = &CorruptError{Digest: r.want, Got: got, Size: r.n, Want: r.size}
			return n, r.failure
		}
		return n, io.EOF
	}
	if err != nil {
		r.failure = err
	}
	return n, err
}

// Size returns the object size as recorded on disk.
func (r *Reader) Size() int64 { return r.size }

// Close closes the underlying file.
func (r *Reader) Close() error { return r.f.Close() }

// Open returns a verifying reader for the object; ErrNotFound if absent.
// Opening touches the object's mtime (best effort) so Prune treats it as
// recently used.
func (c *Cache) Open(domain, digest string) (*Reader, error) {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return nil, err
	}
	p := c.objectPath(domain, hexPart)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// Bump the recency mark only when it is already stale. Prune orders by
	// mtime, so the touch has to happen — but an operation that opens
	// thousands of objects does not need thousands of metadata writes to
	// record that they were all used at about the same time.
	if now := time.Now(); now.Sub(st.ModTime()) > touchInterval {
		_ = os.Chtimes(p, now, now)
	}
	return &Reader{f: f, h: sha256.New(), want: digest, size: st.Size()}, nil
}

// Verify reads the whole object and returns nil only if its bytes match.
func (c *Cache) Verify(domain, digest string) error {
	r, err := c.Open(domain, digest)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(io.Discard, r)
	return err
}

// Remove deletes an object and its pin/publication markers. It refuses to
// remove a pinned object unless force is set.
func (c *Cache) Remove(domain, digest string, force bool) error {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return err
	}
	if !force {
		if pinned, _ := c.IsPinned(domain, digest); pinned {
			return fmt.Errorf("cache: %s is pinned (pending publication)", digest)
		}
	}
	p := c.objectPath(domain, hexPart)
	_ = os.Chmod(p, 0o644)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(c.pinPath(domain, hexPart))
	return nil
}

// ---------------------------------------------------------------------------
// pins and publication markers
// ---------------------------------------------------------------------------

// Pin marks an object as pending publication. Pinned objects survive Prune.
func (c *Cache) Pin(domain, digest string) error {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return err
	}
	p := c.pinPath(domain, hexPart)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Unpin removes the pending-publication marker (no error if absent).
func (c *Cache) Unpin(domain, digest string) error {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return err
	}
	if err := os.Remove(c.pinPath(domain, hexPart)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// IsPinned reports whether the object carries a pending-publication marker.
func (c *Cache) IsPinned(domain, digest string) (bool, error) {
	hexPart, err := validate(domain, digest)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(c.pinPath(domain, hexPart))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// PruneResult reports what Prune did.
type PruneResult struct {
	BytesBefore int64
	BytesAfter  int64
	Removed     int
	SkippedPins int
}

type entry struct {
	domain, hexPart string
	size            int64
	mtime           time.Time
}

// Prune evicts least-recently-used unpinned objects until the total object
// bytes are at or below targetBytes. Pinned (pending publication) objects are
// never evicted, so the result may remain above target. Recency is the file
// mtime, which Open refreshes; atime is not portable enough to rely on.
func (c *Cache) Prune(targetBytes int64) (PruneResult, error) {
	var res PruneResult
	entries, err := c.list()
	if err != nil {
		return res, err
	}
	for _, e := range entries {
		res.BytesBefore += e.size
	}
	res.BytesAfter = res.BytesBefore
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	for _, e := range entries {
		if res.BytesAfter <= targetBytes {
			break
		}
		digest := "sha256:" + e.hexPart
		if pinned, _ := c.IsPinned(e.domain, digest); pinned {
			res.SkippedPins++
			continue
		}
		if err := c.Remove(e.domain, digest, false); err != nil {
			return res, err
		}
		res.BytesAfter -= e.size
		res.Removed++
	}
	return res, nil
}

// list enumerates all objects.
func (c *Cache) list() ([]entry, error) {
	var out []entry
	objRoot := filepath.Join(c.root, "objects")
	domains, err := os.ReadDir(objRoot)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		domain := d.Name()
		err := filepath.WalkDir(filepath.Join(objRoot, domain), func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return err
			}
			name := de.Name()
			if len(name) != 64 {
				return nil
			}
			info, err := de.Info()
			if err != nil {
				return nil
			}
			out = append(out, entry{domain: domain, hexPart: name, size: info.Size(), mtime: info.ModTime()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// TotalBytes sums the sizes of all stored objects.
func (c *Cache) TotalBytes() (int64, error) {
	entries, err := c.list()
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range entries {
		n += e.size
	}
	return n, nil
}

// Entry is one cached object as enumerated by List.
type Entry struct {
	Domain  string
	Digest  string
	Size    int64
	ModTime time.Time
	Pinned  bool
}

// List enumerates every cached object with its pin state, sorted by domain
// then digest. A missing objects directory is an empty cache.
func (c *Cache) List() ([]Entry, error) {
	entries, err := c.list()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Entry{}, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		digest := "sha256:" + e.hexPart
		pinned, _ := c.IsPinned(e.domain, digest)
		out = append(out, Entry{Domain: e.domain, Digest: digest, Size: e.size, ModTime: e.mtime, Pinned: pinned})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Domain != out[j].Domain {
			return out[i].Domain < out[j].Domain
		}
		return out[i].Digest < out[j].Digest
	})
	return out, nil
}

// PlanPrune returns the LRU eviction plan that Prune(targetBytes) would
// execute, without removing anything, plus the number of objects kept
// (pinned or within budget). Pinned objects never appear in the plan.
func (c *Cache) PlanPrune(targetBytes int64) (plan []Entry, kept int, err error) {
	all, err := c.List()
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for _, e := range all {
		total += e.Size
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].ModTime.Before(all[j].ModTime) })
	for _, e := range all {
		if total <= targetBytes || e.Pinned {
			kept++
			continue
		}
		plan = append(plan, e)
		total -= e.Size
	}
	return plan, kept, nil
}
