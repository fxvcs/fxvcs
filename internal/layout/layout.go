// Package layout computes the normative target runtime layout
// (docs/spec/runtime-layout.md §1) and proves every path stays inside its
// root. It contains no I/O; the deployment engine consumes the resolved paths.
//
//	<deployRoot>/
//	  current                            -> releases/<realizationID>
//	  releases/<realizationID>/resources/
//	  releases/<realizationID>/runtime/fxvcs.cfg
//	  releases/<realizationID>/runtime/config/
//	  materialized/<aa>/<sha256>
//	  shared/cache/
//	  shared/persistent/<repositoryID>/<resource-path>/
//	  state/
package layout

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"fxvcs.dev/fxvcs/internal/pathsafe"
)

// Errors.
var (
	ErrInvalidRoot          = errors.New("layout: root must be absolute and clean")
	ErrInvalidRealizationID = errors.New("layout: invalid realization ID")
	ErrOverlap              = errors.New("layout: roots overlap")
	ErrEscape               = errors.New("layout: path escapes its root")
	ErrInvalidDigest        = errors.New("layout: invalid digest")
)

var (
	realizationIDRE = regexp.MustCompile(`^rz_[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
	digestHexRE     = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
)

// Roots are the agent-local absolute directories for one target.
type Roots struct {
	DeployRoot string
	ServerData string
	TxData     string // may be empty for non-txAdmin
}

// Validate proves the roots are absolute, clean, and mutually non-overlapping.
// Two targets on one host must use distinct, non-nested deploy roots.
func (r Roots) Validate() error {
	for _, p := range []string{r.DeployRoot, r.ServerData} {
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("%w: %q", ErrInvalidRoot, p)
		}
	}
	if r.TxData != "" && (!filepath.IsAbs(r.TxData) || filepath.Clean(r.TxData) != r.TxData) {
		return fmt.Errorf("%w: %q", ErrInvalidRoot, r.TxData)
	}
	if pathsafe.Within(r.DeployRoot, r.ServerData) || pathsafe.Within(r.ServerData, r.DeployRoot) {
		return fmt.Errorf("%w: deployRoot and serverData", ErrOverlap)
	}
	if r.TxData != "" && (pathsafe.Within(r.DeployRoot, r.TxData) || pathsafe.Within(r.TxData, r.DeployRoot)) {
		return fmt.Errorf("%w: txData must be outside deployRoot", ErrOverlap)
	}
	return nil
}

// DistinctDeployRoots rejects duplicate or nested deploy roots across targets.
func DistinctDeployRoots(roots ...string) error {
	for i := range roots {
		for j := range roots {
			if i != j && pathsafe.Within(roots[i], roots[j]) {
				return fmt.Errorf("%w: %q and %q", ErrOverlap, roots[i], roots[j])
			}
		}
	}
	return nil
}

// Deploy-root relative segments (OS-independent names).
const (
	CurrentLink     = "current"
	ReleasesDir     = "releases"
	MaterializedDir = "materialized"
	SharedDir       = "shared"
	SharedCache     = "cache"
	SharedPersist   = "persistent"
	StateDir        = "state"
	ResourcesDir    = "resources"
	RuntimeDir      = "runtime"
	RuntimeCfg      = "fxvcs.cfg"
	RuntimeConfig   = "config"
)

// Current returns <deployRoot>/current.
func Current(deployRoot string) string { return filepath.Join(deployRoot, CurrentLink) }

// Releases returns <deployRoot>/releases.
func Releases(deployRoot string) string { return filepath.Join(deployRoot, ReleasesDir) }

// Realization returns <deployRoot>/releases/<realizationID> after validating the ID.
func Realization(deployRoot, realizationID string) (string, error) {
	if !realizationIDRE.MatchString(realizationID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRealizationID, realizationID)
	}
	return filepath.Join(deployRoot, ReleasesDir, realizationID), nil
}

// RealizationTarget is the relative symlink/junction target for `current`.
func RealizationTarget(realizationID string) (string, error) {
	if !realizationIDRE.MatchString(realizationID) {
		return "", fmt.Errorf("%w: %q", ErrInvalidRealizationID, realizationID)
	}
	return filepath.Join(ReleasesDir, realizationID), nil
}

// MaterializedPath returns <deployRoot>/materialized/<aa>/<hex> for a digest.
func MaterializedPath(deployRoot, digest string) (string, error) {
	m := digestHexRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidDigest, digest)
	}
	return filepath.Join(deployRoot, MaterializedDir, m[1][:2], m[1]), nil
}

// ResourceFile resolves a component-mounted file inside a realization:
// <realization>/<mount>/<relPath>. mount must be a normalized path under
// "resources/"; relPath must be normalized. Containment is proven.
func ResourceFile(realizationDir, mount, relPath string) (string, error) {
	if !strings.HasPrefix(mount, ResourcesDir+"/") {
		return "", fmt.Errorf("%w: mount %q must be under %s/", ErrEscape, mount, ResourcesDir)
	}
	base, err := pathsafe.Join(realizationDir, mount)
	if err != nil {
		return "", err
	}
	full, err := pathsafe.Join(base, relPath)
	if err != nil {
		return "", err
	}
	if !pathsafe.Within(realizationDir, full) {
		return "", ErrEscape
	}
	return full, nil
}

// RuntimeFile resolves <realization>/runtime/<relPath> (fxvcs.cfg, config/...).
func RuntimeFile(realizationDir, relPath string) (string, error) {
	base := filepath.Join(realizationDir, RuntimeDir)
	full, err := pathsafe.Join(base, relPath)
	if err != nil {
		return "", err
	}
	if !pathsafe.Within(realizationDir, full) {
		return "", ErrEscape
	}
	return full, nil
}

// HookDestination validates a hook's requested destination against the
// allowlisted destination root (both realization-relative) and resolves it.
// A destination inside resources/ is refused: secret-bearing output must stay
// outside the FTP/SFTP-exposed resource tree.
func HookDestination(realizationDir, allowedRoot, destination string) (string, error) {
	ar, err := pathsafe.Normalize(allowedRoot)
	if err != nil {
		return "", err
	}
	d, err := pathsafe.Normalize(destination)
	if err != nil {
		return "", err
	}
	if d == ResourcesDir || strings.HasPrefix(d, ResourcesDir+"/") {
		return "", fmt.Errorf("%w: hook destination %q is inside the resource tree", ErrEscape, destination)
	}
	if d != ar && !strings.HasPrefix(d, ar+"/") {
		return "", fmt.Errorf("%w: destination %q outside allowed root %q", ErrEscape, destination, allowedRoot)
	}
	full, err := pathsafe.Join(realizationDir, d)
	if err != nil {
		return "", err
	}
	if !pathsafe.Within(realizationDir, full) {
		return "", ErrEscape
	}
	return full, nil
}

// PersistentPath resolves <deployRoot>/shared/persistent/<repositoryID>/<resourcePath>/<declared>.
// The declared path must be relative, normalized, and is validated for
// containment; overlap between declared paths is the catalog validator's job.
func PersistentPath(deployRoot, repositoryID, resourcePath, declared string) (string, error) {
	base := filepath.Join(deployRoot, SharedDir, SharedPersist)
	p, err := pathsafe.Join(base, repositoryID)
	if err != nil {
		return "", err
	}
	p, err = pathsafe.Join(p, resourcePath)
	if err != nil {
		return "", err
	}
	full, err := pathsafe.Join(p, declared)
	if err != nil {
		return "", err
	}
	if !pathsafe.Within(base, full) {
		return "", ErrEscape
	}
	return full, nil
}

// ServerDataLinks lists the indirections installed under <serverData>: each
// entry maps a serverData-relative name to its target under deployRoot.
func ServerDataLinks(r Roots) map[string]string {
	return map[string]string{
		RuntimeCfg:   filepath.Join(r.DeployRoot, CurrentLink, RuntimeDir, RuntimeCfg),
		ResourcesDir: filepath.Join(r.DeployRoot, CurrentLink, ResourcesDir),
		SharedCache:  filepath.Join(r.DeployRoot, SharedDir, SharedCache),
	}
}
