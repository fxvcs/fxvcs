package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/durablefs"
	"fxvcs.dev/fxvcs/internal/layout"
)

// EnsureServerDataLinks installs the three FXVCS-owned stable entry points
// beneath serverData without editing operator-owned server.cfg or replacing
// any existing file/link. Existing links are accepted only when they resolve
// to the exact target-local path.
func EnsureServerDataLinks(roots layout.Roots) error {
	if err := roots.Validate(); err != nil {
		return err
	}
	if err := validateOrdinaryDirectoryPath(roots.ServerData); err != nil {
		return fmt.Errorf("activation: unsafe serverData: %w", err)
	}
	cache := filepath.Join(roots.DeployRoot, layout.SharedDir, layout.SharedCache)
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return err
	}
	links := layout.ServerDataLinks(roots)
	names := make([]string, 0, len(links))
	for name := range links {
		names = append(names, name)
	}
	sort.Strings(names)
	// Validate every destination before creating the first one so an operator
	// collision cannot leave a partially installed set.
	for _, name := range names {
		if err := validateServerDataLink(filepath.Join(roots.ServerData, name), links[name]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, name := range names {
		destination := filepath.Join(roots.ServerData, name)
		if err := validateServerDataLink(destination, links[name]); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		relative, err := filepath.Rel(roots.ServerData, links[name])
		if err != nil || filepath.IsAbs(relative) || relative == "." {
			return fmt.Errorf("activation: cannot construct stable link %s", name)
		}
		if err := os.Symlink(relative, destination); err != nil {
			// Resolve a same-target race only by exact revalidation.
			if validateErr := validateServerDataLink(destination, links[name]); validateErr == nil {
				continue
			}
			return err
		}
	}
	return durablefs.SyncDir(roots.ServerData)
}

func validateServerDataLink(destination, expected string) error {
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("activation: serverData destination %s is operator-owned and will not be replaced", destination)
	}
	target, err := os.Readlink(destination)
	if err != nil {
		return err
	}
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(destination), resolved)
	}
	if filepath.Clean(resolved) != filepath.Clean(expected) {
		return fmt.Errorf("activation: serverData link %s resolves to %s, want %s", destination, resolved, expected)
	}
	return nil
}

func validateOrdinaryDirectoryPath(path string) error {
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume+string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s is not an ordinary directory", current)
		}
	}
	return nil
}
