package filter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"fxvcs.dev/fxvcs/internal/gitclient"
)

// Launcher path shapes per operating system.
//
//	linux:   $XDG_DATA_HOME/fxvcs/bin/fxvcs, else ~/.local/share/fxvcs/bin/fxvcs
//	darwin:  ~/Library/Application Support/fxvcs/bin/fxvcs
//	windows: %LOCALAPPDATA%\fxvcs\bin\fxvcs.exe
const (
	launcherDirName = "fxvcs"
	launcherBinName = "fxvcs"
)

// DefaultLauncherPath returns the stable per-user launcher path for this OS.
func DefaultLauncherPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return LauncherPath(runtime.GOOS, os.Getenv, home)
}

// LauncherPath computes the launcher path for goos using getenv and home. It
// is separated from DefaultLauncherPath so all three shapes can be tested on
// one machine.
func LauncherPath(goos string, getenv func(string) string, home string) (string, error) {
	switch goos {
	case "windows":
		base := getenv("LOCALAPPDATA")
		if base == "" {
			if home == "" {
				return "", errors.New("filter: neither LOCALAPPDATA nor the home directory is known")
			}
			base = home + `\AppData\Local`
		}
		return base + `\` + launcherDirName + `\bin\` + launcherBinName + ".exe", nil
	case "darwin":
		if home == "" {
			return "", errors.New("filter: the home directory is not known")
		}
		return home + "/Library/Application Support/" + launcherDirName + "/bin/" + launcherBinName, nil
	default:
		base := getenv("XDG_DATA_HOME")
		if base == "" {
			if home == "" {
				return "", errors.New("filter: neither XDG_DATA_HOME nor the home directory is known")
			}
			base = home + "/.local/share"
		}
		return base + "/" + launcherDirName + "/bin/" + launcherBinName, nil
	}
}

// LegacyLauncherPath returns the exact former default launcher path when this
// OS has one. It is used only to recognize fxvcs-owned defaults during
// migration; arbitrary recorded paths remain custom and are never moved.
func LegacyLauncherPath(goos string, getenv func(string) string, home string) (string, bool, error) {
	if goos != "darwin" {
		return "", false, nil
	}
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		if home == "" {
			return "", false, errors.New("filter: neither XDG_DATA_HOME nor the home directory is known")
		}
		base = home + "/.local/share"
	}
	return base + "/" + launcherDirName + "/bin/" + launcherBinName, true, nil
}

// DefaultLegacyLauncherPath is LegacyLauncherPath for the running OS.
func DefaultLegacyLauncherPath() (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	return LegacyLauncherPath(runtime.GOOS, os.Getenv, home)
}

// ConfigEntry is one repository-local git config key/value.
type ConfigEntry struct {
	Key   string
	Value string
}

// GitConfigEntries returns the exact repository-local configuration that
// routes Git's filter, diff, and merge drivers to launcher. The launcher path
// is quoted for Git's shell so spaces and (on Windows) backslashes survive.
func GitConfigEntries(launcher string) []ConfigEntry {
	return gitConfigEntries(launcher, runtime.GOOS)
}

func gitConfigEntries(launcher, goos string) []ConfigEntry {
	q := QuoteCommandPath(launcher, goos)
	return []ConfigEntry{
		{Key: "filter.fxvcs.process", Value: q + " filter-process"},
		{Key: "filter.fxvcs.required", Value: "true"},
		{Key: "diff.fxvcs.command", Value: q + " diff-driver"},
		{Key: "merge.fxvcs.name", Value: "fxvcs pointer merge"},
		{Key: "merge.fxvcs.driver", Value: q + " merge-driver %O %A %B %P"},
		// Virtual-ancestor merges (recursive strategy) must not run the
		// conflict-marking driver; Git's built-in binary merge picks ours.
		{Key: "merge.fxvcs.recursive", Value: "binary"},
	}
}

// QuoteCommandPath quotes an executable path for use inside a git config
// command value. Git runs these values through `sh -c` (Git for Windows ships
// its own sh), so:
//
//   - on Windows backslashes become forward slashes, which every Git for
//     Windows sh and the Windows loader accept, avoiding sh escaping rules;
//   - the path is wrapped in double quotes when it contains a space or any
//     shell-special character, and `"`, `$`, backtick, and backslash are
//     escaped inside the quotes.
func QuoteCommandPath(p, goos string) string {
	if goos == "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	if p != "" && !strings.ContainsAny(p, " \t\"'$`\\!&|;()<>*?[]{}~#") {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range p {
		switch r {
		case '"', '$', '`', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// Install writes GitConfigEntries(launcher) into the repository-local config
// of the checkout that git operates on. Values are set with `git config
// --local`, which is atomic per key; the set is idempotent.
func Install(ctx context.Context, git *gitclient.Client, launcher string) error {
	if launcher == "" {
		return errors.New("filter: launcher path is required")
	}
	if !filepath.IsAbs(launcher) && (runtime.GOOS == "windows" || !strings.HasPrefix(launcher, "/")) {
		return fmt.Errorf("filter: launcher path must be absolute: %q", launcher)
	}
	for _, e := range GitConfigEntries(launcher) {
		if err := git.ConfigSet(ctx, e.Key, e.Value); err != nil {
			return fmt.Errorf("filter: set %s: %w", e.Key, err)
		}
	}
	return nil
}

// Uninstall removes the repository-local driver configuration.
func Uninstall(ctx context.Context, git *gitclient.Client) error {
	for _, e := range GitConfigEntries("/x") {
		if err := git.ConfigUnset(ctx, e.Key); err != nil {
			return err
		}
	}
	return nil
}
