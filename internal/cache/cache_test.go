package cache

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dom = "domain_test_01"

func openTemp(t *testing.T) *Cache {
	t.Helper()
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPutGetVerify(t *testing.T) {
	c := openTemp(t)
	data := bytes.Repeat([]byte("fxvcs object "), 10000)
	sum := sha256.Sum256(data)
	want := Digest(sum[:])

	res, err := c.Put(dom, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != want || res.Size != int64(len(data)) || res.Existed {
		t.Fatalf("put result %+v", res)
	}
	if !c.Has(dom, want) {
		t.Fatal("Has = false")
	}
	// Two-level fan-out layout.
	hexPart := strings.TrimPrefix(want, "sha256:")
	p := filepath.Join(c.Root(), "objects", dom, hexPart[:2], hexPart[2:4], hexPart)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("object not at fan-out path: %v", err)
	}

	res2, err := c.Put(dom, bytes.NewReader(data))
	if err != nil || !res2.Existed || res2.Digest != want {
		t.Fatalf("second put %+v %v", res2, err)
	}

	r, err := c.Open(dom, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back mismatch: %v", err)
	}
	if err := c.Verify(dom, want); err != nil {
		t.Fatal(err)
	}
	info, err := c.Stat(dom, want)
	if err != nil || info.Size != int64(len(data)) || info.Pinned {
		t.Fatalf("stat %+v %v", info, err)
	}
	if _, err := c.Stat(dom, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat missing: %v", err)
	}
	if _, err := c.Open(dom, "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	// Temp dir is empty after successful puts.
	ents, _ := os.ReadDir(filepath.Join(c.Root(), "tmp"))
	if len(ents) != 0 {
		t.Fatalf("tmp not empty: %d entries", len(ents))
	}
}

func TestCorruptDetection(t *testing.T) {
	c := openTemp(t)
	data := []byte("some object content that will be corrupted")
	res, err := c.Put(dom, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	hexPart := strings.TrimPrefix(res.Digest, "sha256:")
	p := filepath.Join(c.Root(), "objects", dom, hexPart[:2], hexPart[2:4], hexPart)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[5] ^= 0x01
	if err := os.WriteFile(p, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(dom, res.Digest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify on flipped byte = %v, want ErrCorrupt", err)
	}
	r, err := c.Open(dom, res.Digest)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	r.Close()
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("ReadAll on corrupt = %v", err)
	}
	// Truncation is also detected.
	if err := os.WriteFile(p, data[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Verify(dom, res.Digest); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify on truncated = %v", err)
	}
}

type failingReader struct {
	data []byte
	off  int
}

var errAbort = errors.New("simulated interruption")

func (f *failingReader) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		return 0, errAbort
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

func TestInterruptedWriteLeavesNoObject(t *testing.T) {
	c := openTemp(t)
	data := bytes.Repeat([]byte("x"), 100000)
	_, err := c.Put(dom, &failingReader{data: data})
	if !errors.Is(err, errAbort) {
		t.Fatalf("expected abort error, got %v", err)
	}
	// No object visible for either the partial or the full digest, and no
	// leftover temp file.
	sum := sha256.Sum256(data)
	if c.Has(dom, Digest(sum[:])) {
		t.Fatal("interrupted put made an object visible")
	}
	n := 0
	filepath.WalkDir(filepath.Join(c.Root(), "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	if n != 0 {
		t.Fatalf("%d objects visible after interrupted write", n)
	}
	ents, _ := os.ReadDir(filepath.Join(c.Root(), "tmp"))
	if len(ents) != 0 {
		t.Fatalf("temp file left behind: %d", len(ents))
	}
}

func TestPinsPublicationAndPrune(t *testing.T) {
	c := openTemp(t)
	var digests []string
	for i := 0; i < 4; i++ {
		data := bytes.Repeat([]byte{byte('a' + i)}, 1000)
		res, err := c.Put(dom, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, res.Digest)
		// Make LRU order deterministic: object 0 oldest.
		hexPart := strings.TrimPrefix(res.Digest, "sha256:")
		p := filepath.Join(c.Root(), "objects", dom, hexPart[:2], hexPart[2:4], hexPart)
		ts := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	// Pin the oldest: it must survive.
	if err := c.Pin(dom, digests[0]); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := c.IsPinned(dom, digests[0]); !pinned {
		t.Fatal("IsPinned false after Pin")
	}
	if err := c.Remove(dom, digests[0], false); err == nil {
		t.Fatal("Remove of pinned object without force succeeded")
	}
	res, err := c.Prune(2000)
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesBefore != 4000 || res.BytesAfter != 2000 || res.Removed != 2 || res.SkippedPins != 1 {
		t.Fatalf("prune %+v", res)
	}
	if !c.Has(dom, digests[0]) {
		t.Fatal("pinned object was evicted")
	}
	if c.Has(dom, digests[1]) || c.Has(dom, digests[2]) {
		t.Fatal("LRU objects not evicted")
	}
	if !c.Has(dom, digests[3]) {
		t.Fatal("newest object evicted")
	}

	if err := c.Unpin(dom, digests[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Unpin(dom, digests[0]); err != nil {
		t.Fatalf("second unpin: %v", err)
	}
	res, err = c.Prune(0)
	if err != nil || res.BytesAfter != 0 || res.SkippedPins != 0 {
		t.Fatalf("prune to zero: %+v %v", res, err)
	}
}

func TestValidation(t *testing.T) {
	c := openTemp(t)
	if _, err := c.Put("../escape", strings.NewReader("x")); !errors.Is(err, ErrInvalidDomain) {
		t.Fatalf("bad domain: %v", err)
	}
	if _, err := c.Open(dom, "sha256:zz"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("bad digest: %v", err)
	}
	if c.Has(dom, "nope") {
		t.Fatal("Has accepted bad digest")
	}
	t.Setenv(EnvDir, filepath.Join(t.TempDir(), "custom"))
	d, err := DefaultDir()
	if err != nil || filepath.Base(d) != "custom" {
		t.Fatalf("DefaultDir env override: %q %v", d, err)
	}
	c2, err := Open("")
	if err != nil || c2.Root() != d {
		t.Fatalf("Open default: %q %v", c2.Root(), err)
	}
}
