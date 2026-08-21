package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

func dg(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

func TestPutGetHasAtomicAndVerified(t *testing.T) {
	ctx := context.Background()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("abc"), 1000)
	d := dg(payload)
	if ok, _ := s.Has(ctx, "domain_x", d); ok {
		t.Fatal("has before put")
	}
	if err := s.Put(ctx, "domain_x", d, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Has(ctx, "domain_x", d); !ok {
		t.Fatal("missing after put")
	}
	rc, err := s.Get(ctx, "domain_x", d)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("content mismatch")
	}
	// Idempotent re-put with a different (wrong) reader is a no-op because identity already exists.
	if err := s.Put(ctx, "domain_x", d, -1, bytes.NewReader([]byte("ignored"))); err != nil {
		t.Fatal(err)
	}
	// Wrong digest / wrong size / truncated stream never become visible.
	bad := dg([]byte("other"))
	if err := s.Put(ctx, "domain_x", bad, -1, bytes.NewReader(payload)); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	if ok, _ := s.Has(ctx, "domain_x", bad); ok {
		t.Fatal("bad object visible")
	}
	if err := s.Put(ctx, "domain_x", dg([]byte("sized")), 99, bytes.NewReader([]byte("sized"))); err == nil {
		t.Fatal("size mismatch accepted")
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root(), "domain_x", "tmp"))
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if _, err := s.Get(ctx, "domain_x", bad); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Get(ctx, "domain_x", "sha256:zz"); !errors.Is(err, objectstore.ErrInvalidDigest) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Has(ctx, "repo_x", d); !errors.Is(err, objectstore.ErrInvalidDomain) {
		t.Fatalf("got %v", err)
	}
	// Domain isolation.
	if ok, _ := s.Has(ctx, "domain_y", d); ok {
		t.Fatal("cross-domain visibility")
	}
}

func TestTestCapabilities(t *testing.T) {
	s, _ := New(t.TempDir())
	caps, err := s.Test(context.Background(), "domain_x")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"reachable": true, "writable": true, "atomic-rename": true, "put-get-roundtrip": true}
	for _, c := range caps {
		if want[c.Name] != c.Supported {
			t.Fatalf("%s: %v (%s)", c.Name, c.Supported, c.Detail)
		}
		delete(want, c.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing capabilities %v", want)
	}
	if _, err := New("relative"); err == nil {
		t.Fatal("relative root accepted")
	}
}
