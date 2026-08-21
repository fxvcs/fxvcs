package integrity

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/domain"
)

// RealizationIdentity returns the canonical digest of the non-secret
// realization input tuple. It rejects anything that looks like secret
// plaintext or an unkeyed hash of a secret being smuggled through the version
// map: values must be opaque version identifiers ("v:<id>") or domain-separated
// HMAC fingerprints ("hmac-sha256:<hex>").
func RealizationIdentity(in domain.RealizationInputs) (string, error) {
	if !IsDigest(in.ReleaseDigest) || !IsDigest(in.SourceSetDigest) || !IsDigest(in.TargetContentDigest) {
		return "", fmt.Errorf("%w: realization inputs require release, source-set, and target digests", ErrInvalid)
	}
	for k, v := range in.SecretVersions {
		if !strings.HasPrefix(v, "v:") && !strings.HasPrefix(v, "hmac-sha256:") {
			return "", fmt.Errorf("%w: secret version for %q must be an opaque version ID or keyed fingerprint", ErrInvalid, k)
		}
	}
	for k, v := range in.HookDigests {
		if !IsDigest(v) {
			return "", fmt.Errorf("%w: hook digest for %q", ErrInvalid, k)
		}
	}
	for k, v := range in.VerifierBindings {
		if !IsDigest(v) {
			return "", fmt.Errorf("%w: verifier binding digest for %q", ErrInvalid, k)
		}
	}
	return canonical.Digest(map[string]any{
		"realizationVersion": 1,
		"inputs":             in,
	})
}

// RealizationID converts the canonical realization identity digest into the
// filesystem-safe ID used under releases/. Lowercase unpadded base32 retains
// all 256 digest bits while fitting layout's bounded rz_ identifier format.
func RealizationID(in domain.RealizationInputs) (string, error) {
	digest, err := RealizationIdentity(in)
	if err != nil {
		return "", err
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil {
		return "", fmt.Errorf("%w: realization digest: %v", ErrInvalid, err)
	}
	return "rz_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}

// VerifierBindingDigest canonicalizes the safe, non-secret part of a
// target-local verifier binding: implementation ID and version only. Endpoint
// references, credentials, and paths are excluded by construction.
func VerifierBindingDigest(contract string, b domain.VerifierBinding) (string, error) {
	if contract == "" || b.Implementation == "" || b.ImplementationVersion <= 0 {
		return "", fmt.Errorf("%w: verifier binding for %q", ErrInvalid, contract)
	}
	return canonical.Digest(map[string]any{
		"contract":              contract,
		"implementation":        b.Implementation,
		"implementationVersion": b.ImplementationVersion,
	})
}
