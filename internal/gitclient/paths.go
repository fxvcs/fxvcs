package gitclient

import (
	"path/filepath"
	"strings"
)

// isAbs treats both native absolute paths and Git-for-Windows style
// forward-slash absolute paths ("C:/x", "/c/x") as absolute.
func isAbs(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	if len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\') {
		return true
	}
	return strings.HasPrefix(p, "/")
}

func joinAbs(dir, rel string) string {
	return filepath.Clean(filepath.Join(dir, filepath.FromSlash(rel)))
}
