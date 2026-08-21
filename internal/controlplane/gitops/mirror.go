package gitops

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/schema"
)

// Component repositories are fetched into bare mirrors under the coordination
// repository's $GIT_DIR, never into its working tree:
//
//	$GIT_DIR/fxvcs/components/<repositoryID>.git
//
// The mirror is derived state. It can be deleted and re-fetched at any time,
// it is never committed, and it holds only what the component's Git remote
// already served. Reading a pinned tree therefore needs no checkout and cannot
// disturb the coordination working tree.

// sha1RE and sha256RE match full object names; releases pin full SHAs only.
var (
	sha1RE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// IsCommitID reports whether s is a full SHA-1 or SHA-256 object name.
func IsCommitID(s string) bool { return sha1RE.MatchString(s) || sha256RE.MatchString(s) }

// mirrorSafe rejects a repositoryID that could escape the mirror directory.
// Repository IDs come from committed component configuration, which a
// component author controls; they must never become a path traversal.
func mirrorSafe(repositoryID string) error {
	if repositoryID == "" || strings.ContainsAny(repositoryID, `/\:`) || strings.Contains(repositoryID, "..") {
		return fmt.Errorf("%w: repositoryID %q is not usable as a directory name", controlplane.ErrInvalid, repositoryID)
	}
	return nil
}

// MirrorPath returns the bare mirror directory for a component.
func (r *Repo) MirrorPath(repositoryID string) (string, error) {
	if err := mirrorSafe(repositoryID); err != nil {
		return "", err
	}
	return filepath.Join(r.GitDir, "fxvcs", "components", repositoryID+".git"), nil
}

// HasMirror reports whether a component has been fetched at least once.
func (r *Repo) HasMirror(repositoryID string) bool {
	dir, err := r.MirrorPath(repositoryID)
	if err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && !st.IsDir()
}

// SyncComponent creates the mirror if needed and fetches all branches and tags
// from the component's remote. It is the only operation in this package that
// touches the network.
func (r *Repo) SyncComponent(ctx context.Context, comp domain.StackComponent) error {
	dir, err := r.MirrorPath(comp.RepositoryID)
	if err != nil {
		return err
	}
	if !r.HasMirror(comp.RepositoryID) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, err := r.Git.Run(ctx, "init", "--bare", "--quiet", dir); err != nil {
			return fmt.Errorf("create mirror for %s: %w", comp.RepositoryID, err)
		}
	}
	mirror := r.Git.WithDir(dir)
	if _, err := mirror.Run(ctx, "fetch", "--prune", "--tags", "--quiet", comp.Remote,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return fmt.Errorf("fetch %s from %s: %w", comp.RepositoryID, comp.Remote, err)
	}
	return nil
}

// mirrorGit returns a Git client rooted at a component's mirror.
func (r *Repo) mirrorGit(repositoryID string) (*gitclient.Client, error) {
	dir, err := r.MirrorPath(repositoryID)
	if err != nil {
		return nil, err
	}
	if !r.HasMirror(repositoryID) {
		return nil, fmt.Errorf("%w: component %s has not been fetched (run: fxvcs stack sync)",
			controlplane.ErrNotFound, repositoryID)
	}
	return r.Git.WithDir(dir), nil
}

// ResolveRef resolves a branch, tag, or SHA in a component mirror to a full
// commit ID. A ref that does not exist locally is not silently fetched: the
// caller decides when the network is allowed.
func (r *Repo) ResolveRef(ctx context.Context, repositoryID, ref string) (string, error) {
	git, err := r.mirrorGit(repositoryID)
	if err != nil {
		return "", err
	}
	if ref == "" {
		ref = "HEAD"
	}
	out, err := git.RevParse(ctx, ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %s in component %s: %v", controlplane.ErrNotFound, ref, repositoryID, err)
	}
	if !IsCommitID(out) {
		return "", fmt.Errorf("%w: %s in component %s resolved to %q", controlplane.ErrInvalid, ref, repositoryID, out)
	}
	return out, nil
}

// RemoteHasRef reports whether the component's Git remote still advertises a
// ref pointing at commit. Release creation uses it to warn that a pin is not
// reachable from a durable advertised ref.
func (r *Repo) RemoteHasRef(ctx context.Context, comp domain.StackComponent, commit string) (bool, error) {
	git, err := r.mirrorGit(comp.RepositoryID)
	if err != nil {
		return false, err
	}
	out, err := git.RunString(ctx, "ls-remote", comp.Remote)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		id, _, ok := strings.Cut(line, "\t")
		if ok && id == commit {
			return true, nil
		}
	}
	return false, nil
}

// TagComponent creates an annotated tag in the component's remote naming the
// pinned commit. It is only reached through `release create --tag-components`,
// which plans every tag first and never accepts partial tagging as success.
func (r *Repo) TagComponent(ctx context.Context, comp domain.StackComponent, tag, commit, message string) error {
	git, err := r.mirrorGit(comp.RepositoryID)
	if err != nil {
		return err
	}
	if _, err := git.Run(ctx, "tag", "-a", "-m", message, "--force", tag, commit); err != nil {
		return err
	}
	if _, err := git.Run(ctx, "push", comp.Remote, "refs/tags/"+tag); err != nil {
		return fmt.Errorf("push %s to %s: %w", tag, comp.Remote, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pinned trees
// ---------------------------------------------------------------------------

// TreeEntry is one blob of a pinned component tree.
type TreeEntry struct {
	// Path is the normalized repository-relative path.
	Path string
	// Mode is the Git file mode ("100644", "100755", "120000", "160000").
	Mode string
	// OID is the blob object name.
	OID string
	// Size is the blob size in bytes.
	Size int64
}

// IsRegular reports a mode an asset index may contain.
func (e TreeEntry) IsRegular() bool { return e.Mode == "100644" || e.Mode == "100755" }

// Tree reads one pinned component commit.
type Tree struct {
	RepositoryID string
	Commit       string
	git          *gitclient.Client
}

// Tree opens a pinned commit in a component mirror.
func (r *Repo) Tree(ctx context.Context, repositoryID, commit string) (*Tree, error) {
	if !IsCommitID(commit) {
		return nil, fmt.Errorf("%w: %q is not a full commit id", controlplane.ErrInvalid, commit)
	}
	git, err := r.mirrorGit(repositoryID)
	if err != nil {
		return nil, err
	}
	if _, err := git.RunString(ctx, "rev-parse", "--quiet", "--verify", commit+"^{commit}"); err != nil {
		return nil, fmt.Errorf("%w: commit %s is not in the %s mirror (run: fxvcs stack sync)",
			controlplane.ErrNotFound, commit[:min(len(commit), 12)], repositoryID)
	}
	return &Tree{RepositoryID: repositoryID, Commit: commit, git: git}, nil
}

var lsTreeRE = regexp.MustCompile(`^(\d{6}) (blob|tree|commit) ([0-9a-f]+)\s+(-|\d+)\t(.*)$`)

// Entries lists every blob in the pinned tree, sorted by path.
func (t *Tree) Entries(ctx context.Context) ([]TreeEntry, error) {
	out, err := t.git.RunString(ctx, "ls-tree", "-r", "--long", "--full-tree", t.Commit)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []TreeEntry{}, nil
	}
	lines := strings.Split(out, "\n")
	entries := make([]TreeEntry, 0, len(lines))
	for _, line := range lines {
		m := lsTreeRE.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("gitops: cannot parse ls-tree output %q", line)
		}
		size := int64(-1)
		if m[4] != "-" {
			if size, err = strconv.ParseInt(m[4], 10, 64); err != nil {
				return nil, err
			}
		}
		path, err := pathsafe.Normalize(unquotePath(m[5]))
		if err != nil {
			return nil, fmt.Errorf("gitops: component %s commit %s: %w", t.RepositoryID, t.Commit, err)
		}
		entries = append(entries, TreeEntry{Path: path, Mode: m[1], OID: m[3], Size: size})
	}
	return entries, nil
}

// unquotePath undoes Git's C-style quoting of paths containing unusual bytes.
func unquotePath(p string) string {
	if !strings.HasPrefix(p, `"`) {
		return p
	}
	if s, err := strconv.Unquote(p); err == nil {
		return s
	}
	return p
}

// ReadFile returns the bytes of one path in the pinned tree. A missing path
// returns controlplane.ErrNotFound.
func (t *Tree) ReadFile(ctx context.Context, path string) ([]byte, error) {
	data, err := t.git.CatFileBlob(ctx, t.Commit+":"+path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s in component %s at %s", controlplane.ErrNotFound, path, t.RepositoryID, t.Commit[:min(len(t.Commit), 12)])
	}
	return data, nil
}

// ReadBlob returns the bytes of one object name from the pinned tree.
func (t *Tree) ReadBlob(ctx context.Context, oid string) ([]byte, error) {
	return t.git.CatFileBlob(ctx, oid)
}

// CopyBlob streams one pinned blob without buffering it in memory.
func (t *Tree) CopyBlob(ctx context.Context, oid string, w io.Writer) error {
	return t.git.CatFileBlobTo(ctx, oid, w)
}

// RepositoryConfig loads the component's committed .fxvcs/repository.yaml from
// the pinned tree. This is what makes a component self-describing: its
// accepted source root and layout travel with the commit, so composition never
// has to guess them.
func (t *Tree) RepositoryConfig(ctx context.Context) (*domain.Repository, error) {
	data, err := t.ReadFile(ctx, ".fxvcs/repository.yaml")
	if err != nil {
		return nil, fmt.Errorf("%w (component %s at %s is not an fxvcs repository)", err, t.RepositoryID, shortID(t.Commit))
	}
	cfg, err := schema.LoadRepository(data)
	if err != nil {
		return nil, &DocumentError{File: t.RepositoryID + "@" + shortID(t.Commit) + ":.fxvcs/repository.yaml", Err: err}
	}
	return cfg, nil
}

// ResourceCatalog loads the component's committed .fxvcs/resources.yaml.
// A component with no catalog returns controlplane.ErrNotFound so the caller
// can say which repository needs `fxvcs resources sync`.
func (t *Tree) ResourceCatalog(ctx context.Context) (*domain.ResourceCatalog, error) {
	data, err := t.ReadFile(ctx, ".fxvcs/resources.yaml")
	if err != nil {
		return nil, err
	}
	cat, err := schema.LoadResourceCatalog(data)
	if err != nil {
		return nil, &DocumentError{File: t.RepositoryID + "@" + shortID(t.Commit) + ":.fxvcs/resources.yaml", Err: err}
	}
	return cat, nil
}

func shortID(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
