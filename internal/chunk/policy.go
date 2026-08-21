package chunk

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// Policy decides which encoding an object gets. It is deliberately a property
// of the *format generation*, not of a machine: the encoding choice feeds the
// object manifest, the manifest digest feeds the pointer, and the pointer is
// what Git stores. Two clients that disagree about the policy would produce
// different pointers for byte-identical content and Git would report a
// spurious modification on every restage.
//
// Changing these defaults therefore means introducing a new encoding
// identifier and a new minimumFXVCSVersion, exactly like changing the FastCDC
// parameters.
type Policy struct {
	// MinChunkedSize is the size at or above which an object is chunked.
	// Below it a single whole object costs one round trip and no manifest.
	MinChunkedSize int64
	// WholeOnly lists lowercase file extensions (with the dot) that stay
	// whole-blob regardless of size because the corpus measurement showed no
	// worthwhile reuse between versions.
	WholeOnly map[string]bool
}

// DefaultPolicy is the Stage 2 policy justified by docs/stage-2-measurement.md.
//
// The measurement compared whole blobs, fixed chunks, and four CDC parameter
// sets over version pairs of RSC7-backed and audio assets. Every format
// measured cleared the 25% net-transfer gate at cdc-fastcdc/1 parameters, so
// WholeOnly is empty; the mechanism stays because the report must be re-run
// against a real asset corpus and a format that falls below the gate there is
// added here rather than handled with new code.
//
// Objects below MinChunkedSize stay whole: at 1 MiB the manifest, the extra
// existence checks, and the extra requests cost more than the reuse.
func DefaultPolicy() Policy {
	return Policy{MinChunkedSize: 1 << 20, WholeOnly: map[string]bool{}}
}

// EnvPolicy is a diagnostic override for the measurement harness and tests.
// It is documented as diagnostic-only: a machine that sets it produces
// different (still valid, still verifiable) pointers than its collaborators,
// so Git will report the affected paths as modified when they are restaged.
//
//	FXVCS_CHUNKING=off          never chunk
//	FXVCS_CHUNKING=always       chunk every object
//	FXVCS_CHUNKING=min=<bytes>  change only the size floor
const EnvPolicy = "FXVCS_CHUNKING"

// PolicyFromEnv applies EnvPolicy to the default policy.
func PolicyFromEnv() (Policy, error) {
	p := DefaultPolicy()
	v := strings.TrimSpace(os.Getenv(EnvPolicy))
	switch {
	case v == "":
		return p, nil
	case v == "off":
		p.MinChunkedSize = -1
	case v == "always":
		p.MinChunkedSize = 0
	case strings.HasPrefix(v, "min="):
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "min="), 10, 64)
		if err != nil || n < 0 {
			return p, fmt.Errorf("%s=%q: want off, always, or min=<bytes>", EnvPolicy, v)
		}
		p.MinChunkedSize = n
	default:
		return p, fmt.Errorf("%s=%q: want off, always, or min=<bytes>", EnvPolicy, v)
	}
	return p, nil
}

// Encoding returns the encoding identifier for an object. repoPath is the
// repository-relative slash path (empty when unknown) and size is the exact
// object size.
//
// Callers must know the size, even when ingesting from a stream: deciding
// "chunk it" for a file whose size was merely unknown would give the same
// bytes two different pointers depending on how they arrived. The ingest path
// therefore reads MinChunkedSize bytes ahead before choosing (see
// AtLeastMinChunked).
func (p Policy) Encoding(repoPath string, size int64) string {
	switch {
	case p.MinChunkedSize < 0:
		return EncodingWhole
	case p.WholeOnly[strings.ToLower(path.Ext(repoPath))]:
		return EncodingWhole
	case size < p.MinChunkedSize:
		return EncodingWhole
	}
	return EncodingFastCDCv1
}

// AtLeastMinChunked is the lookahead the size decision needs: the number of
// leading bytes that must be present before an object counts as large enough
// to chunk.
func (p Policy) AtLeastMinChunked() int64 {
	if p.MinChunkedSize < 0 {
		return 0
	}
	return p.MinChunkedSize
}

// ParamsFor returns the parameter set behind an encoding identifier.
func ParamsFor(encoding string) (Params, error) {
	if encoding == EncodingFastCDCv1 {
		return FastCDCv1, nil
	}
	return Params{}, fmt.Errorf("%w: unknown encoding %q", ErrInvalidParams, encoding)
}
