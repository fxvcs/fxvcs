// Package trust implements the deterministic parts of
// docs/spec/trust-and-signing.md that need no Git or key material: promotion
// tag naming and monotonic sequence selection, coordination authority policy
// validation, and the detached-signature envelope for canonical Release
// digests. Signature verification itself shells out to git / ssh-keygen in a
// later stage; this package decides *what* would be verified.
package trust

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
)

// Errors.
var (
	ErrInvalidPolicy       = errors.New("trust: invalid coordination trust policy")
	ErrNotPromotionTag     = errors.New("trust: not a promotion tag for this target")
	ErrNonCanonicalSeq     = errors.New("trust: non-canonical promotion sequence")
	ErrDuplicateSeq        = errors.New("trust: duplicate promotion sequence")
	ErrNoValidPromotion    = errors.New("trust: no valid promotion tag")
	ErrDesiredStateRelaxes = errors.New("trust: desired state cannot relax agent-local trust")
)

const (
	AuthoritySignedCommitTip    = "signed-commit-tip"
	AuthoritySignedPromotionTag = "signed-promotion-tag"

	SignaturesRequired = "required"
	SignaturesOptional = "optional"
	SignaturesOff      = "off"

	// PromotionTagRoot is the fixed namespace under which promotion tags live.
	PromotionTagRoot = "refs/tags/fxvcs/promote/"
)

var (
	targetNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	seqRE        = regexp.MustCompile(`^(0|[1-9][0-9]{0,17})$`)
)

// ValidatePolicy checks an agent-local coordination trust policy.
func ValidatePolicy(p domain.CoordinationTrust) error {
	switch p.Authority {
	case AuthoritySignedCommitTip:
		if p.PromotionTagPrefix != "" {
			return fmt.Errorf("%w: promotionTagPrefix is only valid for %s", ErrInvalidPolicy, AuthoritySignedPromotionTag)
		}
	case AuthoritySignedPromotionTag:
		if p.PromotionTagPrefix == "" {
			return fmt.Errorf("%w: promotionTagPrefix required for %s", ErrInvalidPolicy, AuthoritySignedPromotionTag)
		}
		if _, err := TargetFromPrefix(p.PromotionTagPrefix); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: authority %q", ErrInvalidPolicy, p.Authority)
	}
	switch p.Signatures {
	case SignaturesRequired, SignaturesOptional, SignaturesOff:
	default:
		return fmt.Errorf("%w: signatures %q", ErrInvalidPolicy, p.Signatures)
	}
	if p.Signatures != SignaturesOff && p.AllowedSignersFile == "" {
		return fmt.Errorf("%w: allowedSignersFile required when signatures are %s", ErrInvalidPolicy, p.Signatures)
	}
	return nil
}

// Floor enforces "desired state cannot relax agent-local policy": the
// effective policy is the agent-local one; a Git/Cloud-supplied policy may only
// tighten. Returns ErrDesiredStateRelaxes if desired is weaker in any respect.
func Floor(local, desired domain.CoordinationTrust) (domain.CoordinationTrust, error) {
	if err := ValidatePolicy(local); err != nil {
		return local, err
	}
	if desired.Authority != "" && desired.Authority != local.Authority {
		return local, fmt.Errorf("%w: authority %q != agent-local %q", ErrDesiredStateRelaxes, desired.Authority, local.Authority)
	}
	if desired.Signatures != "" && sigRank(desired.Signatures) < sigRank(local.Signatures) {
		return local, fmt.Errorf("%w: signatures %q weaker than agent-local %q", ErrDesiredStateRelaxes, desired.Signatures, local.Signatures)
	}
	if desired.AllowedSignersFile != "" && desired.AllowedSignersFile != local.AllowedSignersFile {
		return local, fmt.Errorf("%w: allowedSignersFile is agent-local", ErrDesiredStateRelaxes)
	}
	eff := local
	if desired.Signatures != "" && sigRank(desired.Signatures) > sigRank(local.Signatures) {
		eff.Signatures = desired.Signatures
	}
	return eff, nil
}

func sigRank(s string) int {
	switch s {
	case SignaturesOff:
		return 0
	case SignaturesOptional:
		return 1
	case SignaturesRequired:
		return 2
	}
	return -1
}

// PromotionTagPrefix returns "refs/tags/fxvcs/promote/<target>/".
func PromotionTagPrefix(target string) (string, error) {
	if !targetNameRE.MatchString(target) {
		return "", fmt.Errorf("%w: target %q", ErrInvalidPolicy, target)
	}
	return PromotionTagRoot + target + "/", nil
}

// TargetFromPrefix validates a configured prefix and extracts the target.
func TargetFromPrefix(prefix string) (string, error) {
	if !strings.HasPrefix(prefix, PromotionTagRoot) || !strings.HasSuffix(prefix, "/") {
		return "", fmt.Errorf("%w: prefix %q must be %s<target>/", ErrInvalidPolicy, prefix, PromotionTagRoot)
	}
	t := strings.TrimSuffix(strings.TrimPrefix(prefix, PromotionTagRoot), "/")
	if !targetNameRE.MatchString(t) {
		return "", fmt.Errorf("%w: target %q in prefix", ErrInvalidPolicy, t)
	}
	return t, nil
}

// PromotionTag is a parsed refs/tags/fxvcs/promote/<target>/<seq> ref.
type PromotionTag struct {
	Ref    string
	Target string
	Seq    uint64
}

// ParsePromotionTag parses a full ref name for the given target. The sequence
// must be canonical decimal (no leading zeros, no sign, no padding) so that
// "01" and "1" cannot both exist and be treated as distinct authorities.
func ParsePromotionTag(target, ref string) (PromotionTag, error) {
	prefix, err := PromotionTagPrefix(target)
	if err != nil {
		return PromotionTag{}, err
	}
	if !strings.HasPrefix(ref, prefix) {
		return PromotionTag{}, fmt.Errorf("%w: %q", ErrNotPromotionTag, ref)
	}
	rest := strings.TrimPrefix(ref, prefix)
	if !seqRE.MatchString(rest) {
		return PromotionTag{}, fmt.Errorf("%w: %q", ErrNonCanonicalSeq, rest)
	}
	n, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return PromotionTag{}, fmt.Errorf("%w: %q", ErrNonCanonicalSeq, rest)
	}
	return PromotionTag{Ref: ref, Target: target, Seq: n}, nil
}

// Candidate is a promotion tag together with the agent's verification result.
type Candidate struct {
	Tag PromotionTag
	// Commit the annotated tag points at (after peeling).
	Commit string
	// Valid is true only when the tag is annotated, its single signature
	// verifies against agent-local allowed signers (when required), and it was
	// not retargeted since first observation.
	Valid bool
	// Reason explains invalidity for audit.
	Reason string
}

// SelectDesired implements the promotion-tag authority: the highest sequence
// among valid candidates wins. Duplicate sequences (which can only occur when
// non-canonical encodings slipped past provider protection or the ref list was
// tampered with) fail closed. Branch commits newer than the selected tag are
// irrelevant; there is no fallback to branch-tip authority.
func SelectDesired(cands []Candidate) (Candidate, error) {
	seen := map[uint64]bool{}
	valid := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if seen[c.Tag.Seq] {
			return Candidate{}, fmt.Errorf("%w: %d", ErrDuplicateSeq, c.Tag.Seq)
		}
		seen[c.Tag.Seq] = true
		if c.Valid {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return Candidate{}, ErrNoValidPromotion
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Tag.Seq > valid[j].Tag.Seq })
	return valid[0], nil
}

// ReleaseSignatureEnvelope is the detached signature format over a canonical
// Release digest. Signatures cover exactly the ASCII bytes of
// SignedPayload(digest); the profile pins the algorithm and namespace so a
// signature cannot be replayed as another kind of statement.
type ReleaseSignatureEnvelope struct {
	Profile       string   `json:"profile"`       // "ssh-v1"
	Namespace     string   `json:"namespace"`     // "fxvcs-release"
	ReleaseDigest string   `json:"releaseDigest"` // sha256:...
	Signatures    []Signer `json:"signatures"`
}

type Signer struct {
	Role      string `json:"role"`      // creator | cloud | builder
	KeyID     string `json:"keyID"`     // SHA256:... fingerprint of the public key
	Signature string `json:"signature"` // armored ssh signature
}

const (
	ProfileSSHv1     = "ssh-v1"
	NamespaceRelease = "fxvcs-release"
)

// SignedPayload returns the exact bytes a signer signs for a release digest.
func SignedPayload(releaseDigest string) []byte {
	return []byte("fxvcs-release-v1\n" + releaseDigest + "\n")
}

// ThresholdPolicy is the agent-local requirement for portable Release signatures.
type ThresholdPolicy struct {
	RequiredRoles []string `json:"requiredRoles"` // every listed role must have >=1 valid signature
	Threshold     int      `json:"threshold"`     // total distinct valid signers required
	RevokedKeyIDs []string `json:"revokedKeyIDs"`
}

// Satisfied reports whether the set of verified (keyID, role) pairs meets policy.
// verified must contain only signatures already cryptographically checked.
func (p ThresholdPolicy) Satisfied(verified []Signer) (bool, string) {
	revoked := map[string]bool{}
	for _, k := range p.RevokedKeyIDs {
		revoked[k] = true
	}
	roles := map[string]bool{}
	keys := map[string]bool{}
	for _, s := range verified {
		if revoked[s.KeyID] {
			continue
		}
		roles[s.Role] = true
		keys[s.KeyID] = true
	}
	for _, r := range p.RequiredRoles {
		if !roles[r] {
			return false, "missing required role " + r
		}
	}
	if len(keys) < p.Threshold {
		return false, fmt.Sprintf("threshold %d not met (%d distinct signers)", p.Threshold, len(keys))
	}
	return true, ""
}
