package filter

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// ---------------------------------------------------------------------------
// diff driver
// ---------------------------------------------------------------------------

// Side describes one side of a diff: absent, a pointer, or raw bytes (a
// hydrated working file or a historical blob).
type Side struct {
	Absent  bool
	Pointer *pointer.Pointer
	// For raw sides: computed identity so pointer and hydrated file compare.
	OID  string
	Size int64
	Mode string
}

// ErrNotPointerInput is returned by drivers when they need pointer bytes and
// receive something else.
var ErrNotPointerInput = errors.New("filter: input is not an fxvcs pointer")

// ReadSide classifies a file handed to the diff driver. path=="", "/dev/null",
// "nul", or mode "." marks an absent side. The hex is not used for that
// decision because Git reports an all-zero hex for a working-tree file that
// does exist. Raw sides are hashed while streaming so memory stays bounded.
func ReadSide(path, hexOID, mode string, installed format.Version) (Side, error) {
	if path == "" || path == "/dev/null" || strings.EqualFold(path, "nul") || mode == "." {
		return Side{Absent: true}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Side{Absent: true}, nil
		}
		return Side{}, err
	}
	defer f.Close()
	head, err := readHead(f)
	if err != nil {
		return Side{}, err
	}
	whole := len(head) <= pointer.MaxSize
	if whole && pointer.Sniff(head) {
		p, err := pointer.Decode(head)
		if err != nil {
			if errors.Is(err, pointer.ErrUnsupportedVersion) {
				return Side{}, fmt.Errorf("%w: %v; installed fxvcs %s supports pointer format v%d; run: fxvcs upgrade --check",
					ErrUnsupportedPointer, err, installed, pointer.FormatVersion)
			}
			return Side{}, fmt.Errorf("%w: %v", ErrMalformedPointer, err)
		}
		return Side{Pointer: &p, OID: p.OID, Size: p.Size, Mode: mode}, nil
	}
	h := sha256.New()
	h.Write(head)
	n, err := io.Copy(h, f)
	if err != nil {
		return Side{}, err
	}
	return Side{OID: "sha256:" + fmt.Sprintf("%x", h.Sum(nil)), Size: int64(len(head)) + n, Mode: mode}, nil
}

// RenderDiff writes the side-by-side pointer metadata view and a one-line
// summary. Output is deterministic and contains no host paths.
func RenderDiff(w io.Writer, path string, oldSide, newSide Side) error {
	row := func(label, a, b string) {
		fmt.Fprintf(w, "  %-9s %-72s %s\n", label, a, b)
	}
	cell := func(s Side, f func(p *pointer.Pointer) string, raw func(s Side) string) string {
		switch {
		case s.Absent:
			return "-"
		case s.Pointer != nil:
			return f(s.Pointer)
		default:
			return raw(s)
		}
	}
	fmt.Fprintf(w, "fxvcs pointer diff: %s\n", path)
	row("", "old", "new")
	row("kind",
		cell(oldSide, func(*pointer.Pointer) string { return "pointer" }, func(Side) string { return "file (not a pointer)" }),
		cell(newSide, func(*pointer.Pointer) string { return "pointer" }, func(Side) string { return "file (not a pointer)" }))
	row("domain",
		cell(oldSide, func(p *pointer.Pointer) string { return p.StorageDomainID }, func(Side) string { return "-" }),
		cell(newSide, func(p *pointer.Pointer) string { return p.StorageDomainID }, func(Side) string { return "-" }))
	row("encoding",
		cell(oldSide, func(p *pointer.Pointer) string { return p.Encoding }, func(Side) string { return "-" }),
		cell(newSide, func(p *pointer.Pointer) string { return p.Encoding }, func(Side) string { return "-" }))
	row("manifest",
		cell(oldSide, func(p *pointer.Pointer) string { return p.ManifestDigest }, func(Side) string { return "-" }),
		cell(newSide, func(p *pointer.Pointer) string { return p.ManifestDigest }, func(Side) string { return "-" }))
	row("oid",
		cell(oldSide, func(p *pointer.Pointer) string { return p.OID }, func(s Side) string { return s.OID }),
		cell(newSide, func(p *pointer.Pointer) string { return p.OID }, func(s Side) string { return s.OID }))
	row("size",
		cell(oldSide, func(p *pointer.Pointer) string { return fmt.Sprint(p.Size) }, func(s Side) string { return fmt.Sprint(s.Size) }),
		cell(newSide, func(p *pointer.Pointer) string { return fmt.Sprint(p.Size) }, func(s Side) string { return fmt.Sprint(s.Size) }))
	if oldSide.Mode != "" || newSide.Mode != "" {
		row("mode", orDash(oldSide.Mode), orDash(newSide.Mode))
	}
	fmt.Fprintf(w, "summary: %s\n", diffSummary(oldSide, newSide))
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func diffSummary(oldSide, newSide Side) string {
	switch {
	case oldSide.Absent && newSide.Absent:
		return "no content on either side"
	case oldSide.Absent:
		return fmt.Sprintf("added %s (%d bytes)", newSide.OID, newSide.Size)
	case newSide.Absent:
		return fmt.Sprintf("deleted %s (%d bytes)", oldSide.OID, oldSide.Size)
	case oldSide.OID == newSide.OID && oldSide.Size == newSide.Size:
		if oldSide.Mode != newSide.Mode && oldSide.Mode != "" && newSide.Mode != "" {
			return fmt.Sprintf("content unchanged %s; mode %s -> %s", oldSide.OID, oldSide.Mode, newSide.Mode)
		}
		return fmt.Sprintf("content unchanged %s (%d bytes)", oldSide.OID, oldSide.Size)
	default:
		return fmt.Sprintf("content changed %s -> %s (%d -> %d bytes)", oldSide.OID, newSide.OID, oldSide.Size, newSide.Size)
	}
}

// RunDiffDriver is the `fxvcs diff-driver` entry point (GIT_EXTERNAL_DIFF
// argument convention: path oldSide-file oldSide-hex oldSide-mode newSide-file newSide-hex
// newSide-mode). With a single argument (unmerged path) it prints a note.
func RunDiffDriver(args []string, installed format.Version, stdout io.Writer) error {
	switch len(args) {
	case 1:
		fmt.Fprintf(stdout, "fxvcs pointer diff: %s\nsummary: unmerged; use git ls-files -u to see the candidate pointers\n", args[0])
		return nil
	case 7, 9:
		// 9 = with rename source/similarity, ignored.
	default:
		return fmt.Errorf("filter: diff-driver expects 1 or 7 arguments, got %d", len(args))
	}
	oldSide, err := ReadSide(args[1], args[2], args[3], installed)
	if err != nil {
		return err
	}
	newSide, err := ReadSide(args[4], args[5], args[6], installed)
	if err != nil {
		return err
	}
	return RenderDiff(stdout, args[0], oldSide, newSide)
}

// ---------------------------------------------------------------------------
// merge driver
// ---------------------------------------------------------------------------

// MergeOutcome classifies a merge.
type MergeOutcome int

const (
	// MergeClean means result holds the merged pointer (exit 0).
	MergeClean MergeOutcome = iota
	// MergeConflict means result holds the conflict layout (exit 1).
	MergeConflict
)

// Conflict layout markers.
const (
	ConflictOurs   = "<<<<<<< ours\n"
	ConflictSep    = "=======\n"
	ConflictTheirs = ">>>>>>> theirs\n"
)

// Merge performs the pointer-aware three-way merge. Rules:
//
//  1. ours == theirs (bytes)                -> ours, clean
//  2. ours or theirs is not a valid pointer -> ErrNotPointerInput
//  3. base absent or not a pointer          -> conflict (both sides added)
//  4. ours == base                          -> theirs, clean
//  5. theirs == base                        -> ours, clean
//  6. otherwise                             -> conflict layout, both pointers kept
//
// It never picks by timestamp or size.
func Merge(base, ours, theirs []byte, installed format.Version) ([]byte, MergeOutcome, error) {
	if bytes.Equal(ours, theirs) {
		if _, err := decodeStrict(ours, installed); err != nil {
			return nil, MergeConflict, err
		}
		return ours, MergeClean, nil
	}
	if _, err := decodeStrict(ours, installed); err != nil {
		return nil, MergeConflict, fmt.Errorf("ours: %w", err)
	}
	if _, err := decodeStrict(theirs, installed); err != nil {
		return nil, MergeConflict, fmt.Errorf("theirs: %w", err)
	}
	baseIsPointer := len(base) > 0
	if baseIsPointer {
		if _, err := decodeStrict(base, installed); err != nil {
			baseIsPointer = false
		}
	}
	if baseIsPointer {
		if bytes.Equal(ours, base) {
			return theirs, MergeClean, nil
		}
		if bytes.Equal(theirs, base) {
			return ours, MergeClean, nil
		}
	}
	var b bytes.Buffer
	b.WriteString(ConflictOurs)
	b.Write(ours)
	b.WriteString(ConflictSep)
	b.Write(theirs)
	b.WriteString(ConflictTheirs)
	return b.Bytes(), MergeConflict, nil
}

func decodeStrict(data []byte, installed format.Version) (pointer.Pointer, error) {
	p, err := pointer.Decode(data)
	if err == nil {
		return p, nil
	}
	if errors.Is(err, pointer.ErrNotPointer) {
		return p, fmt.Errorf("%w (%d bytes)", ErrNotPointerInput, len(data))
	}
	if errors.Is(err, pointer.ErrUnsupportedVersion) {
		return p, fmt.Errorf("%w: %v; installed fxvcs %s supports pointer format v%d; run: fxvcs upgrade --check",
			ErrUnsupportedPointer, err, installed, pointer.FormatVersion)
	}
	return p, fmt.Errorf("%w: %v", ErrMalformedPointer, err)
}

// MergeResult is what RunMergeDriver did.
type MergeResult struct {
	Outcome MergeOutcome
	Path    string
}

// RunMergeDriver is the `fxvcs merge-driver %O %A %B %P` entry point. It
// rewrites the %A file with the result (or the conflict layout) and returns
// the outcome. On ErrNotPointerInput (or any error) %A is left untouched and
// the caller must exit non-zero so Git records the path as unmerged.
func RunMergeDriver(basePath, oursPath, theirsPath, pathname string, installed format.Version) (MergeResult, error) {
	res := MergeResult{Outcome: MergeConflict, Path: pathname}
	base, err := os.ReadFile(basePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	ours, err := os.ReadFile(oursPath)
	if err != nil {
		return res, err
	}
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		return res, err
	}
	out, outcome, err := Merge(base, ours, theirs, installed)
	if err != nil {
		return res, err
	}
	res.Outcome = outcome
	if bytes.Equal(out, ours) {
		return res, nil
	}
	// oursPath is Git's %A temp file, supplied by Git itself, not repository content.
	if err := os.WriteFile(oursPath, out, 0o644); err != nil { //nolint:gosec // G703: path comes from Git, see above
		return res, err
	}
	return res, nil
}
