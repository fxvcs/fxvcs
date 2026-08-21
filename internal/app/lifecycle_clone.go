package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/gitclient"
)

// CloneInput configures Clone.
type CloneInput struct {
	URL string
	// Directory is the target directory (absolute or relative to Cwd); empty
	// derives it from the URL like git does.
	Directory string
	// Cwd resolves a relative Directory ("" = process cwd).
	Cwd string
	// Blobless passes --filter=blob:none so pre-migration binary history is
	// not downloaded.
	Blobless bool
	// LauncherPath is passed to Setup.
	LauncherPath string
}

// CloneResult reports the new checkout.
type CloneResult struct {
	Root  string
	Setup SetupResult
}

// Clone runs `git clone` (without any fxvcs driver configured, so Git checks
// out committed pointer bytes exactly as stored: an unhydrated checkout is
// always safe), then opens the checkout and runs Setup. Hydration is a
// separate, explicit step; the caller performs it when requested.
func Clone(ctx context.Context, in CloneInput, opts Options) (CloneResult, error) {
	if strings.TrimSpace(in.URL) == "" {
		return CloneResult{}, fmt.Errorf("%w: clone requires a URL", ErrInvalidArgument)
	}
	cwd := in.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return CloneResult{}, err
		}
		cwd = wd
	}
	dir := in.Directory
	if dir == "" {
		dir = deriveCloneDir(in.URL)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return CloneResult{}, fmt.Errorf("%w: destination %s exists and is not empty", ErrInvalidArgument, dir)
	}
	git := gitclient.New(cwd)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	args := []string{"clone", "--quiet"}
	if in.Blobless {
		args = append(args, "--filter=blob:none")
	}
	args = append(args, "--", in.URL, dir)
	if _, err := git.Run(ctx, args...); err != nil {
		return CloneResult{}, err
	}
	r, err := Open(ctx, dir, opts)
	if err != nil {
		return CloneResult{Root: dir}, err
	}
	defer r.Close()
	setup, err := r.Setup(ctx, SetupInput{LauncherPath: in.LauncherPath})
	if err != nil {
		return CloneResult{Root: r.Root}, err
	}
	return CloneResult{Root: r.Root, Setup: setup}, nil
}

// deriveCloneDir mimics git's default directory name for a URL.
func deriveCloneDir(url string) string {
	u := strings.TrimRight(url, "/\\")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimRight(u, "/\\")
	if i := strings.LastIndexAny(u, "/\\:"); i >= 0 {
		u = u[i+1:]
	}
	if u == "" {
		return "repository"
	}
	return u
}
