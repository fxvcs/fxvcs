// Package format owns FXVCS format-version negotiation: the committed
// document envelope (apiVersion/kind), semantic version comparison for
// minimumFXVCSVersion, pointer/object format identifiers, Git integration
// generations, and local SQLite schema versions.
//
// The rules are normative (docs/spec/version-negotiation.md):
//
//   - Every persisted document starts with a format version.
//   - Readers accept documented older versions and reject unknown or newer
//     versions before performing any write.
//   - A repository whose minimumFXVCSVersion exceeds the running binary refuses
//     every normal operation; only the compatibility diagnostic may run.
package format

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Group is the API group for every committed FXVCS document.
const Group = "fxvcs.dev"

// CurrentAPIVersion is the committed-document schema version this binary writes.
const CurrentAPIVersion = Group + "/v1alpha1"

// SupportedAPIVersions lists every committed-document version this binary can
// read, oldest first. Currently exactly one; upgrades append here and add an
// ordered migration in internal/migrate.
var SupportedAPIVersions = []string{CurrentAPIVersion}

// Format identifiers that version themselves independently of apiVersion.
const (
	PointerFormatVersion       = 1 // internal/pointer
	ObjectManifestVersion      = 1 // canonical JSON object manifest
	AssetIndexVersion          = 1 // canonical JSON asset index
	GitIntegrationGeneration   = 1 // filter+diff+merge+-text
	HydrationStateSchema       = 2 // $GIT_DIR/fxvcs/state.db
	PublicationLedgerSchema    = 1 // $GIT_COMMON_DIR/fxvcs/publication.db
	AgentStateSchema           = 1 // agent observed-state database
	TemplateRendererVersion    = "template.v1"
	SDKMajor                   = 0
	FilterProtocolVersion      = 2 // Git long-running filter protocol version
	SDKAPIVersion              = "fxvcs.dev/sdk/v0"
	MaximumSupportedIntegrator = GitIntegrationGeneration
)

// Kinds of committed documents.
const (
	KindRepository             = "Repository"
	KindResourceCatalog        = "ResourceCatalog"
	KindRuntimeResourceCatalog = "RuntimeResourceCatalog"
	KindResourceOverrides      = "ResourceOverrides"
	KindStack                  = "Stack"
	KindRelease                = "Release"
	KindEnvironment            = "Environment"
	KindTarget                 = "Target"
	KindLocalConfig            = "LocalConfig"
	KindAgentConfig            = "AgentConfig"
)

// KnownKinds is the closed set of committed/local document kinds.
var KnownKinds = map[string]bool{
	KindRepository: true, KindResourceCatalog: true, KindRuntimeResourceCatalog: true,
	KindResourceOverrides: true, KindStack: true, KindRelease: true,
	KindEnvironment: true, KindTarget: true, KindLocalConfig: true, KindAgentConfig: true,
}

// Errors returned by negotiation. Callers must check with errors.Is / errors.As
// and must not write anything after receiving them.
var (
	ErrUnknownAPIVersion   = errors.New("format: unknown apiVersion")
	ErrNewerAPIVersion     = errors.New("format: document apiVersion is newer than this binary supports")
	ErrUnknownKind         = errors.New("format: unknown kind")
	ErrInvalidSemver       = errors.New("format: invalid semantic version")
	ErrUnsupportedGen      = errors.New("format: unsupported Git integration generation")
	ErrUnsupportedPointer  = errors.New("format: unsupported pointer format version")
	ErrUnsupportedSchemaDB = errors.New("format: unsupported local database schema version")
)

// UpgradeRequiredError is the only result a below-minimum client may report. It
// carries the installed and required versions so CLI, filter, agent, and desktop
// can print an actionable message. It is deliberately not wrapped in other
// operation results: a client that receives it must stop.
type UpgradeRequiredError struct {
	Installed  Version
	Required   Version
	Repository string // repository path or coordination record, for diagnostics
	Reason     string // e.g. "minimumFXVCSVersion", "gitIntegrationVersion"
}

func (e *UpgradeRequiredError) Error() string {
	return fmt.Sprintf("fxvcs upgrade required: installed %s, required %s (%s%s); run: fxvcs upgrade --check",
		e.Installed, e.Required, e.Reason, repoSuffix(e.Repository))
}

func repoSuffix(r string) string {
	if r == "" {
		return ""
	}
	return " in " + r
}

// Version is a semantic version without build metadata; prerelease is compared
// per SemVer 2.0 §11.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

var semverRE = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// ParseVersion parses "1.2.3", "v1.2.3", "1.2.3-rc.1", ignoring build metadata.
func ParseVersion(s string) (Version, error) {
	m := semverRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("%w: %q", ErrInvalidSemver, s)
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return Version{Major: maj, Minor: mnr, Patch: pat, Pre: m[4]}, nil
}

// MustParseVersion panics on invalid input; for compile-time constants only.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Compare returns -1, 0, or 1.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpInt(v.Minor, o.Minor)
	case v.Patch != o.Patch:
		return cmpInt(v.Patch, o.Patch)
	}
	return comparePre(v.Pre, o.Pre)
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1 // release > prerelease
	}
	if b == "" {
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if c := cmpInt(ai, bi); c != 0 {
				return c
			}
		case aerr == nil:
			return -1 // numeric < alphanumeric
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(as), len(bs))
}

// Envelope is the leading portion of every committed document.
type Envelope struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// ParseAPIVersion splits "fxvcs.dev/v1alpha1" into group and version and
// classifies it as supported, newer, or unknown. A version is "newer" when it
// has the correct group and a lexically/numerically later version token than
// any supported version, so that a downgraded binary reports the right error.
func ParseAPIVersion(apiVersion string) (group, version string, err error) {
	i := strings.LastIndex(apiVersion, "/")
	if i <= 0 || i == len(apiVersion)-1 {
		return "", "", fmt.Errorf("%w: %q", ErrUnknownAPIVersion, apiVersion)
	}
	group, version = apiVersion[:i], apiVersion[i+1:]
	if group != Group {
		return group, version, fmt.Errorf("%w: group %q", ErrUnknownAPIVersion, group)
	}
	if slices.Contains(SupportedAPIVersions, apiVersion) {
		return group, version, nil
	}
	if newerThanSupported(version) {
		return group, version, fmt.Errorf("%w: %q (supported: %s)", ErrNewerAPIVersion, apiVersion, strings.Join(SupportedAPIVersions, ", "))
	}
	return group, version, fmt.Errorf("%w: %q", ErrUnknownAPIVersion, apiVersion)
}

var apiVersionTokenRE = regexp.MustCompile(`^v(\d+)(?:(alpha|beta)(\d+))?$`)

// apiVersionRank orders v1alpha1 < v1beta1 < v1 < v2alpha1 ...
func apiVersionRank(tok string) (major, stage, n int, ok bool) {
	m := apiVersionTokenRE.FindStringSubmatch(tok)
	if m == nil {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(m[1])
	switch m[2] {
	case "alpha":
		stage = 0
	case "beta":
		stage = 1
	default:
		stage = 2
	}
	if m[3] != "" {
		n, _ = strconv.Atoi(m[3])
	}
	return major, stage, n, true
}

func newerThanSupported(tok string) bool {
	maj, st, n, ok := apiVersionRank(tok)
	if !ok {
		return false
	}
	for _, s := range SupportedAPIVersions {
		_, sv, _ := strings.Cut(s, "/")
		smaj, sst, sn, _ := apiVersionRank(sv)
		if maj > smaj || (maj == smaj && st > sst) || (maj == smaj && st == sst && n > sn) {
			continue
		}
		return false
	}
	return true
}

// ValidateEnvelope checks the apiVersion and kind of a committed document.
// It returns nil only when the document may proceed to schema validation.
func ValidateEnvelope(env Envelope, expectKind string) error {
	if _, _, err := ParseAPIVersion(env.APIVersion); err != nil {
		return err
	}
	if !KnownKinds[env.Kind] {
		return fmt.Errorf("%w: %q", ErrUnknownKind, env.Kind)
	}
	if expectKind != "" && env.Kind != expectKind {
		return fmt.Errorf("%w: expected %s, got %s", ErrUnknownKind, expectKind, env.Kind)
	}
	return nil
}

// CheckMinimumVersion implements the client gate. installed is the running
// binary; minimum is the repository/coordination declaration ("" means none).
// A non-nil error is always *UpgradeRequiredError (or ErrInvalidSemver).
func CheckMinimumVersion(installed Version, minimum string, repository string) error {
	if strings.TrimSpace(minimum) == "" {
		return nil
	}
	req, err := ParseVersion(minimum)
	if err != nil {
		return err
	}
	if installed.Compare(req) < 0 {
		return &UpgradeRequiredError{Installed: installed, Required: req, Repository: repository, Reason: "minimumFXVCSVersion"}
	}
	return nil
}

// CheckGitIntegrationGeneration gates filter/diff/merge entry points.
func CheckGitIntegrationGeneration(required int, repository string, installed Version) error {
	if required <= 0 {
		return fmt.Errorf("%w: %d", ErrUnsupportedGen, required)
	}
	if required > MaximumSupportedIntegrator {
		return &UpgradeRequiredError{Installed: installed, Required: Version{Major: 0, Minor: 0, Patch: 0, Pre: fmt.Sprintf("git-integration.%d", required)}, Repository: repository, Reason: "gitIntegrationVersion"}
	}
	return nil
}
