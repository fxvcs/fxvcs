package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/profile/fivem"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/schema"
)

// Lifecycle errors. They are sentinels so the SDK can map them to stable codes.
var (
	// ErrAlreadyInitialized: .fxvcs/repository.yaml already exists.
	ErrAlreadyInitialized = errors.New("app: repository is already initialized (.fxvcs/repository.yaml exists)")
	// ErrDiscovery wraps a fail-closed FiveM layout detection; the cause is a
	// *discovery.Error carrying candidates.
	ErrDiscovery = errors.New("app: fivem layout could not be determined")
)

// Profiles.
const (
	ProfileGeneric = "generic"
	ProfileFiveM   = "fivem"
)

// MinimumVersionFloor is the minimumFXVCSVersion written by a prerelease
// (development) binary. A prerelease cannot claim a meaningful floor and
// SemVer orders every prerelease of 0.0.0 above "0.0.0-0", so this value
// admits every build. Release binaries write their own version instead.
const MinimumVersionFloor = "0.0.0-0"

var displayNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// InitInput configures Init.
type InitInput struct {
	// Path is any directory inside the Git working tree to initialize.
	Path string
	// Name is metadata.name; empty selects the working-tree basename.
	Name string
	// Profile is generic (default) or fivem.
	Profile string
	// StorageDomainID overrides the generated storage domain (rare: a
	// repository that intentionally shares a domain with another).
	StorageDomainID string
	// FiveM descriptor overrides; when all are empty, discovery proposes them.
	SourceRoot   string
	Layout       string
	ResourceName string
	// RemoteName/RemotePath register one committed filesystem remote
	// (requiredForPublication) and bind it locally.
	RemoteName string
	RemotePath string
	// LauncherPath is passed to Setup.
	LauncherPath string
}

// InitResult reports what Init created.
type InitResult struct {
	Root            string
	RepositoryID    string
	StorageDomainID string
	Name            string
	Profile         string
	// Detection is the accepted FiveM descriptor (fivem profile only).
	Detection *discovery.Detection
	// FilesWritten lists repository-relative paths written or edited.
	FilesWritten []string
	Setup        SetupResult
	// Track is the preset tracking result (fivem profile only).
	Track *TrackResult
}

// newID returns prefix + a 26-character lowercase Crockford base32 identifier
// (48-bit millisecond timestamp followed by 80 random bits, ULID layout).
func newID(prefix string, now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli()) //nolint:gosec // millisecond timestamps fit in 48 bits until year 10889
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[:6], ts[2:])
	if _, err := rand.Read(b[6:]); err != nil {
		return "", err
	}
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	n := new(big.Int).SetBytes(b[:])
	out := make([]byte, 26)
	mask := big.NewInt(31)
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return prefix + string(out), nil
}

// minimumVersionFor returns the minimumFXVCSVersion a repository created by
// installed declares.
func minimumVersionFor(installed format.Version) string {
	if installed.Pre != "" {
		return MinimumVersionFloor
	}
	return installed.String()
}

// Init creates .fxvcs/repository.yaml in the Git working tree containing
// in.Path, then runs Setup and (for the fivem profile) tracks the binary
// presets. It never commits. Validation (Git worktree, no existing
// configuration, name, profile, FiveM layout, schema) completes before the
// first write.
func Init(ctx context.Context, in InitInput, opts Options) (InitResult, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	git := gitclient.New(in.Path)
	if opts.GitExecutable != "" {
		git.Executable = opts.GitExecutable
	}
	root, err := git.TopLevel(ctx)
	if err != nil {
		return InitResult{}, fmt.Errorf("%w: %v", ErrNotGitWorktree, err)
	}
	cfgPath := filepath.Join(root, filepath.FromSlash(repository.ConfigPath))
	if _, err := os.Stat(cfgPath); err == nil {
		return InitResult{}, fmt.Errorf("%w: %s", ErrAlreadyInitialized, cfgPath)
	}
	// An enclosing repository (parent directory with .fxvcs) is also refused:
	// nested FXVCS repositories are not a supported shape.
	if existing, err := repository.Locate(root); err == nil {
		return InitResult{}, fmt.Errorf("%w: enclosing repository at %s", ErrAlreadyInitialized, existing)
	}
	name := in.Name
	if name == "" {
		name = filepath.Base(root)
	}
	if !displayNameRE.MatchString(name) {
		return InitResult{}, fmt.Errorf("%w: name %q must match %s (pass --name)", ErrInvalidArgument, name, displayNameRE)
	}
	profile := in.Profile
	if profile == "" {
		profile = ProfileGeneric
	}
	if profile != ProfileGeneric && profile != ProfileFiveM {
		return InitResult{}, fmt.Errorf("%w: profile %q (generic|fivem)", ErrInvalidArgument, profile)
	}
	if (in.RemoteName == "") != (in.RemotePath == "") {
		return InitResult{}, fmt.Errorf("%w: --remote-name and --remote-path must be given together", ErrInvalidArgument)
	}
	if in.RemoteName != "" && !remoteNameRE.MatchString(in.RemoteName) {
		return InitResult{}, fmt.Errorf("%w: remote name %q", ErrInvalidArgument, in.RemoteName)
	}
	if in.RemotePath != "" && !filepath.IsAbs(in.RemotePath) {
		return InitResult{}, fmt.Errorf("%w: --remote-path must be absolute", ErrInvalidArgument)
	}
	now := opts.Now()
	repoID, err := newID("repo_", now)
	if err != nil {
		return InitResult{}, err
	}
	domainID := in.StorageDomainID
	if domainID == "" {
		domainID, err = newID("domain_", now)
		if err != nil {
			return InitResult{}, err
		}
	}
	cfg := &domain.Repository{
		Envelope: format.Envelope{APIVersion: format.CurrentAPIVersion, Kind: format.KindRepository},
		Metadata: domain.RepositoryMetadata{RepositoryID: repoID, StorageDomainID: domainID, Name: name},
		Spec: domain.RepositorySpec{
			Profile:               profile,
			MinimumFXVCSVersion:   minimumVersionFor(opts.Installed),
			GitIntegrationVersion: format.GitIntegrationGeneration,
			Storage:               domain.RepositoryStore{Remotes: []domain.StorageRemote{}},
		},
	}
	if in.RemoteName != "" {
		cfg.Spec.Storage.Remotes = append(cfg.Spec.Storage.Remotes, domain.StorageRemote{Name: in.RemoteName, Type: "filesystem", RequiredForPublication: true})
	}
	res := InitResult{Root: root, RepositoryID: repoID, StorageDomainID: domainID, Name: name, Profile: profile}
	if profile == ProfileFiveM {
		det, err := detectLayout(ctx, root, in)
		if err != nil {
			return InitResult{}, err
		}
		res.Detection = &det
		cfg.Spec.SourceRoot = det.Descriptor.SourceRoot
		cfg.Spec.Layout = det.Descriptor.Layout
		cfg.Spec.ResourceName = det.Descriptor.ResourceName
	}
	data, err := marshalRepository(cfg)
	if err != nil {
		return InitResult{}, err
	}
	if issues := schema.Validate(format.KindRepository, data); len(issues) > 0 {
		return InitResult{}, fmt.Errorf("%w: generated repository.yaml is invalid: %v", ErrInvalidArgument, issues)
	}
	// Preflight the exact document this binary is about to write, so a floor
	// this binary itself would refuse can never be written.
	if _, err := repository.LoadConfigBytes(data); err != nil {
		return InitResult{}, err
	}
	if err := writeFileAtomic(cfgPath, data, 0o644); err != nil {
		return InitResult{}, err
	}
	res.FilesWritten = append(res.FilesWritten, repository.ConfigPath)

	r, err := Open(ctx, root, opts)
	if err != nil {
		return res, err
	}
	defer r.Close()
	if in.RemoteName != "" {
		if _, err := r.StorageAdd(ctx, StorageAddInput{Name: in.RemoteName, Type: "filesystem", Path: in.RemotePath}); err != nil {
			return res, err
		}
	}
	setup, err := r.Setup(ctx, SetupInput{LauncherPath: in.LauncherPath})
	if err != nil {
		return res, err
	}
	res.Setup = setup
	if profile == ProfileFiveM {
		tr, err := r.Track(ctx, TrackInput{Patterns: fivem.DefaultBinaryPatterns, Glob: true})
		if err != nil {
			return res, err
		}
		res.Track = &tr
		if tr.Changed {
			res.FilesWritten = append(res.FilesWritten, AttributesPath)
		}
	}
	return res, nil
}

// detectLayout proposes or validates the FiveM descriptor for root.
func detectLayout(ctx context.Context, root string, in InitInput) (discovery.Detection, error) {
	if in.SourceRoot == "" && in.Layout == "" && in.ResourceName == "" {
		det, err := discovery.DetectDir(root)
		if err != nil {
			return discovery.Detection{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
		}
		return det, nil
	}
	desc := domain.ComponentDescriptor{SourceRoot: in.SourceRoot, Layout: in.Layout}
	if desc.SourceRoot == "" {
		desc.SourceRoot = "."
	}
	if desc.Layout == "" {
		return discovery.Detection{}, fmt.Errorf("%w: --layout single|collection is required when overriding the FiveM descriptor", ErrInvalidArgument)
	}
	if in.ResourceName != "" {
		n := in.ResourceName
		desc.ResourceName = &n
	} else if desc.Layout == discovery.LayoutSingle {
		n := filepath.Base(root)
		desc.ResourceName = &n
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return discovery.Detection{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
	}
	found, err := discovery.ScanTree(ctx, os.DirFS(abs), desc, discovery.ScanOptions{})
	if err != nil {
		return discovery.Detection{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
	}
	det := discovery.Detection{Descriptor: desc, ResourceNameProposed: in.ResourceName == "" && desc.Layout == discovery.LayoutSingle, Skipped: found.Skipped}
	for _, f := range found.Resources {
		det.Resources = append(det.Resources, f.Path)
	}
	return det, nil
}

// marshalRepository renders the committed configuration with a short header.
func marshalRepository(cfg *domain.Repository) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# FXVCS repository configuration. Reviewed and committed like any other file;\n")
	b.WriteString("# never edited implicitly by fxvcs (fxvcs upgrade --apply changes only the\n")
	b.WriteString("# fields it owns and leaves the diff for review).\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// writeFileAtomic writes data to path via a temporary file and rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".fxvcs-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(name) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
