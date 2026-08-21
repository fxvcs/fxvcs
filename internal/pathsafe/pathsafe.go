// Package pathsafe normalizes repository-relative paths and proves that a
// materialization destination stays inside its root. Every path read from a
// repository, catalog, release, hook request, or persistent-path declaration
// passes through here before it touches the filesystem.
//
// Rules (docs/security/threat-model.md, "path traversal"):
//
//   - repository paths use "/" separators, are relative, contain no empty, ".",
//     or ".." segments, no NUL, no backslash, no drive letters, and no leading
//     "/";
//   - segments are literal; "[maps]" is a directory name, never a glob;
//   - Windows reserved device names and trailing dots/spaces are rejected so a
//     path cannot resolve differently across platforms;
//   - Join refuses to produce a filesystem path outside root even when a segment
//     is later replaced by a symlink: callers must open with O_NOFOLLOW /
//     verify realpath, and Materializer must never follow symlinks inside root.
package pathsafe

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Errors.
var (
	ErrEmpty         = errors.New("pathsafe: empty path")
	ErrAbsolute      = errors.New("pathsafe: absolute path not allowed")
	ErrTraversal     = errors.New("pathsafe: path escapes root")
	ErrBadSegment    = errors.New("pathsafe: invalid path segment")
	ErrBadCharacter  = errors.New("pathsafe: invalid character in path")
	ErrReservedName  = errors.New("pathsafe: reserved or platform-ambiguous name")
	ErrNotNormalized = errors.New("pathsafe: path is not normalized")
)

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Normalize validates a repository-relative path and returns its normalized
// form. It rejects rather than repairs: "a//b", "./a", "a/../b" and "a\b" are
// errors so that stored paths are always byte-identical to their canonical form
// and can be compared without re-normalization.
func Normalize(p string) (string, error) {
	if p == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrBadCharacter)
	}
	if strings.ContainsAny(p, "\x00\\") {
		return "", fmt.Errorf("%w: NUL or backslash in %q", ErrBadCharacter, p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w: %q", ErrAbsolute, p)
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("%w: drive letter in %q", ErrAbsolute, p)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if err := checkSegment(s); err != nil {
			return "", fmt.Errorf("%w in %q", err, p)
		}
	}
	return p, nil
}

func checkSegment(s string) error {
	switch s {
	case "":
		return fmt.Errorf("%w: empty segment", ErrBadSegment)
	case ".", "..":
		return fmt.Errorf("%w: %q", ErrTraversal, s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrBadCharacter)
		}
		switch r {
		case '<', '>', ':', '"', '|', '?', '*':
			return fmt.Errorf("%w: %q", ErrBadCharacter, r)
		}
	}
	if strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return fmt.Errorf("%w: trailing dot or space", ErrReservedName)
	}
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		return fmt.Errorf("%w: %q", ErrReservedName, s)
	}
	if strings.EqualFold(s, ".git") {
		return fmt.Errorf("%w: %q", ErrReservedName, s)
	}
	return nil
}

// Join returns filepath.Join(root, rel) after proving the result stays inside
// root lexically. rel must already be normalized (Normalize is applied). root
// must be absolute and clean; the caller is responsible for resolving symlinks
// in root itself (filepath.EvalSymlinks) before calling Join.
func Join(root, rel string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("%w: root %q is not absolute", ErrAbsolute, root)
	}
	if filepath.Clean(root) != root {
		return "", fmt.Errorf("%w: root %q is not clean", ErrNotNormalized, root)
	}
	n, err := Normalize(rel)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(n))
	if !Within(root, full) {
		return "", fmt.Errorf("%w: %q", ErrTraversal, rel)
	}
	return full, nil
}

// Within reports whether path is root or a lexical descendant of root. Both are
// expected to be absolute cleaned OS paths.
func Within(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// Overlaps reports whether two normalized repository paths are equal or one is
// an ancestor of the other. Used for mount and persistent-path validation.
func Overlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// IsBracketCategory reports whether the final segment is a FiveM bracketed
// category such as "[maps]". Brackets are literal directory names, never globs.
func IsBracketCategory(seg string) bool {
	return len(seg) >= 3 && seg[0] == '[' && seg[len(seg)-1] == ']'
}
