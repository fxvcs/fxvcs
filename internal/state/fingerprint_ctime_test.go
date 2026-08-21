package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestFingerprintNoticesAnInPlaceRewriteThatRestoresMtime is the reason the
// fingerprint carries a change time.
//
// The stat cache decides whether a working file still holds the content it was
// last seen with, and unload acts on that answer destructively. A writer can
// set mtime back to whatever it likes; it cannot set ctime. Without ctime, a
// same-length in-place rewrite that restores the timestamp is indistinguishable
// from no change at all.
func TestFingerprintNoticesAnInPlaceRewriteThatRestoresMtime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows exposes no change time; the fingerprint is advisory there")
	}
	p := filepath.Join(t.TempDir(), "asset.bin")
	original := []byte("the original content")
	if err := os.WriteFile(p, original, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := StatFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if before.CtimeNS == 0 {
		t.Fatal("no change time was recorded on a platform that has one")
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := fi.ModTime()

	// Rewrite in place at the same length, then put the timestamp back —
	// what a timestamp-preserving build or asset tool does.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(p, []byte("the modified content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	after, err := StatFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != before.Size || after.MtimeNS != before.MtimeNS || after.FileID != before.FileID {
		t.Skip("the rewrite moved size, mtime or file identity; this platform catches it without ctime")
	}
	if after == before {
		t.Fatal("a rewritten file was reported as unchanged: size, mtime and file id all match, so only the change time can tell them apart")
	}
}

// TestFingerprintEncodingSurvivesRowsWrittenBeforeChangeTime keeps journal rows
// from an earlier schema readable: they parse with a zero change time, which
// simply reads as "changed".
func TestFingerprintEncodingSurvivesRowsWrittenBeforeChangeTime(t *testing.T) {
	old := "4096:1700000000000000000:1:100"
	f, err := ParseFingerprint(old)
	if err != nil {
		t.Fatalf("an old journal row no longer parses: %v", err)
	}
	if f.Size != 4096 || f.MtimeNS != 1700000000000000000 || f.FileID != "1:100" || f.CtimeNS != 0 {
		t.Fatalf("old row decoded as %+v", f)
	}
	// And the current encoding round-trips.
	want := Fingerprint{Size: 12, MtimeNS: 34, CtimeNS: 56, FileID: "7:8"}
	got, err := ParseFingerprint(want.String())
	if err != nil || got != want {
		t.Fatalf("round trip: %+v %v (encoded %q)", got, err, want.String())
	}
}
