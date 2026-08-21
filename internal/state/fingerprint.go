package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Fingerprint is the stat-cache identity of a working file. Two fingerprints
// that are equal mean "probably unchanged"; any difference means the cached
// digest must be recomputed. It is an optimization only, never an integrity
// source (docs/spec/checkout-consistency.md).
type Fingerprint struct {
	Size    int64
	MtimeNS int64
	// CtimeNS is the inode change time, where the platform exposes it (Unix;
	// zero on Windows, which has no equivalent — its creation time does not
	// move when a file is written).
	//
	// It is here because mtime can be set by whoever wrote the file and ctime
	// cannot: utimes moves mtime, and nothing an unprivileged writer can do
	// moves ctime backwards. A tool that rewrites a file in place at the same
	// length and restores its timestamps therefore still changes ctime, and a
	// stat cache that ignored it would call that file unchanged. Git's index
	// records ctime for the same reason.
	CtimeNS int64
	// FileID is the platform file identity: "dev:ino" on Unix, empty on
	// Windows where the cache relies on size+mtime alone.
	FileID string
}

// IsZero reports an empty fingerprint (file absent / unknown).
func (f Fingerprint) IsZero() bool { return f == Fingerprint{} }

// String encodes the fingerprint for the operation journal.
//
// The form is "size:mtime:c<ctime>:fileID". The marked ctime field keeps the
// encoding readable by a parser that predates it: an older journal row has no
// "c" field where this one does, and parses with a zero ctime, which simply
// reads as "changed" and costs one re-hash.
func (f Fingerprint) String() string {
	return fmt.Sprintf("%d:%d:c%d:%s", f.Size, f.MtimeNS, f.CtimeNS, f.FileID)
}

// ParseFingerprint decodes String(), including rows written before the
// fingerprint carried a change time.
func ParseFingerprint(s string) (Fingerprint, error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return Fingerprint{}, fmt.Errorf("state: malformed fingerprint %q", s)
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("state: malformed fingerprint size %q", parts[0])
	}
	mt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("state: malformed fingerprint mtime %q", parts[1])
	}
	f := Fingerprint{Size: size, MtimeNS: mt, FileID: parts[2]}
	rest := parts[2]
	if marked, ok := strings.CutPrefix(rest, "c"); ok {
		ct, id, found := strings.Cut(marked, ":")
		if n, cerr := strconv.ParseInt(ct, 10, 64); cerr == nil && found {
			f.CtimeNS, f.FileID = n, id
		}
	}
	return f, nil
}

// FingerprintOf builds the fingerprint from an os.FileInfo (size, mtime and,
// where the platform exposes it, device+inode).
func FingerprintOf(fi os.FileInfo) Fingerprint {
	return Fingerprint{Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano(),
		CtimeNS: ctimeNS(fi), FileID: fileID(fi)}
}

// StatFingerprint stats path and returns its fingerprint (zero, nil when the
// file does not exist).
func StatFingerprint(path string) (Fingerprint, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return Fingerprint{}, nil
	}
	if err != nil {
		return Fingerprint{}, err
	}
	return FingerprintOf(fi), nil
}
