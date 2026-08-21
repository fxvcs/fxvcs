package filter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/repository"
)

// ProcessOptions configures the filter process and one-shot filters.
type ProcessOptions struct {
	// Installed is the running binary version (required).
	Installed format.Version
	// Cwd is the directory Git started the process in ("" = os.Getwd).
	Cwd string
	// Git overrides the git client (tests). nil = gitclient.New(Cwd).
	Git *gitclient.Client
	// CacheDir overrides the object cache root ("" = cache.DefaultDir()).
	CacheDir string
	// Connect opens the engine Backend for the resolved working tree
	// (internal/filter/wire supplies it). nil runs the cache-only engine:
	// clean pins in the cache but records no ledger row or intent, smudge
	// always emits pointers, pre-push cannot judge publication.
	Connect func(ctx context.Context, root string) (Backend, error)
	// Log receives diagnostics that are not errors (nil = discard).
	Log io.Writer
}

// Checkout is the resolved Git/FXVCS context of a filter or driver process.
type Checkout struct {
	Root   string // working-tree top level
	Repo   *domain.Repository
	Git    *gitclient.Client
	Cache  *cache.Cache
	Domain string
	// Backend is the engine (nil in cache-only mode, see ProcessOptions).
	Backend Backend
	// ConfigSource says where repository.yaml came from: "worktree",
	// "index" or "HEAD".
	ConfigSource string

	installed format.Version
}

// Close releases the backend.
func (co *Checkout) Close() error {
	if co != nil && co.Backend != nil {
		return co.Backend.Close()
	}
	return nil
}

// OpenCheckout resolves the working tree through Git, loads
// .fxvcs/repository.yaml (working tree first, then the index, then HEAD so a
// clone whose checkout is still in progress can be served), and runs the
// mandatory preflight. On *format.UpgradeRequiredError the caller must stop
// before answering any Git command.
func OpenCheckout(ctx context.Context, opts ProcessOptions) (*Checkout, error) {
	cwd := opts.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		cwd = wd
	}
	git := opts.Git
	if git == nil {
		git = gitclient.New(cwd)
	}
	root, err := git.TopLevel(ctx)
	if err != nil {
		return nil, fmt.Errorf("filter: locate working tree: %w", err)
	}
	git = git.WithDir(root)
	repo, source, err := loadRepositoryConfig(ctx, git, root)
	if err != nil {
		return nil, err
	}
	if err := repository.Preflight(root, repo, opts.Installed); err != nil {
		return nil, err
	}
	c, err := cache.Open(opts.CacheDir)
	if err != nil {
		return nil, err
	}
	co := &Checkout{Root: root, Repo: repo, Git: git, Cache: c, Domain: repo.Metadata.StorageDomainID, ConfigSource: source, installed: opts.Installed}
	if opts.Connect != nil {
		b, err := opts.Connect(ctx, root)
		switch {
		case err == nil:
			co.Backend = b
		case source != "worktree" && errors.Is(err, repository.ErrNotRepository):
			// The checkout is still being written (clone in progress): the
			// engine cannot open yet. Serve in cache-only mode; nothing is
			// hydrated (no state rows exist) and clean is not expected.
			if opts.Log != nil {
				fmt.Fprintf(opts.Log, "fxvcs: repository configuration not in the working tree yet (%s); serving pointers only\n", source)
			}
		default:
			return nil, err
		}
	}
	return co, nil
}

func loadRepositoryConfig(ctx context.Context, git *gitclient.Client, root string) (*domain.Repository, string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(repository.ConfigPath)))
	if err == nil {
		r, err := repository.LoadConfigBytes(data)
		return r, "worktree", err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	for _, name := range []string{":" + repository.ConfigPath, "HEAD:" + repository.ConfigPath} {
		if b, err := git.CatFileBlob(ctx, name); err == nil {
			r, err := repository.LoadConfigBytes(b)
			src := "index"
			if strings.HasPrefix(name, "HEAD") {
				src = "HEAD"
			}
			return r, src, err
		}
	}
	return nil, "", repository.ErrNotRepository
}

// FormatUpgradeRequired renders the multi-line stderr block every Git entry
// point prints when the repository requires a newer client.
func FormatUpgradeRequired(e *format.UpgradeRequiredError) string {
	var b strings.Builder
	b.WriteString("fxvcs: this repository requires a newer fxvcs; refusing to run the Git integration\n")
	fmt.Fprintf(&b, "  installed version: %s\n", e.Installed)
	fmt.Fprintf(&b, "  required version:  %s (%s)\n", e.Required, e.Reason)
	if e.Repository != "" {
		fmt.Fprintf(&b, "  repository:        %s\n", e.Repository)
	}
	b.WriteString("  next step:         fxvcs upgrade --check\n")
	return b.String()
}

// RunProcess is the `fxvcs filter-process` entry point. It preflights the
// checkout, then serves the long-running filter protocol on stdin/stdout.
// Diagnostics go to stderr, which Git forwards to the user. It returns
// *format.UpgradeRequiredError (unwrapped) when the repository requires a
// newer client so the CLI can choose the upgrade-required exit code.
func RunProcess(ctx context.Context, opts ProcessOptions, stdin io.Reader, stdout, stderr io.Writer) error {
	if opts.Log == nil {
		opts.Log = stderr
	}
	co, err := OpenCheckout(ctx, opts)
	if err != nil {
		var ue *format.UpgradeRequiredError
		if errors.As(err, &ue) {
			io.WriteString(stderr, FormatUpgradeRequired(ue))
			return ue
		}
		fmt.Fprintf(stderr, "fxvcs filter-process: %v\n", err)
		return err
	}
	defer func() { _ = co.Close() }()
	eng := &Engine{Cache: co.Cache, Domain: co.Domain, Installed: opts.Installed, Backend: co.Backend}
	return Serve(ctx, stdin, stdout, eng, stderr)
}

// RunOneShot is the `fxvcs clean-filter <path>` / `smudge-filter <path>`
// entry point for debugging and `git cat-file --filters`-style use: it reads
// the entire content from stdin and writes the result to stdout.
func RunOneShot(ctx context.Context, opts ProcessOptions, command, pathname string, stdin io.Reader, stdout, stderr io.Writer) error {
	if opts.Log == nil {
		opts.Log = stderr
	}
	co, err := OpenCheckout(ctx, opts)
	if err != nil {
		var ue *format.UpgradeRequiredError
		if errors.As(err, &ue) {
			io.WriteString(stderr, FormatUpgradeRequired(ue))
			return ue
		}
		return err
	}
	defer func() { _ = co.Close() }()
	eng := &Engine{Cache: co.Cache, Domain: co.Domain, Installed: opts.Installed, Backend: co.Backend}
	req := Request{Command: command, Pathname: pathname}
	switch command {
	case commandClean:
		return eng.Clean(ctx, req, stdin, stdout)
	case commandSmudge:
		return eng.Smudge(ctx, req, stdin, stdout)
	}
	return fmt.Errorf("filter: unknown one-shot command %q", command)
}
