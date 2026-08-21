// Package pointer encodes and decodes the FXVCS pointer stored in Git in place
// of a large asset.
//
// Format version 1 is a short, deterministic, line-oriented UTF-8 text:
//
//	version https://fxvcs.dev/pointer/v1
//	domain domain_maps_01
//	encoding whole/1
//	manifest sha256:<64 hex>
//	oid sha256:<64 hex>
//	size 1048576
//
// Rules:
//
//   - the first line is always the version line;
//   - the remaining keys appear exactly once each, in ascending byte order;
//   - key and value are separated by one space, lines end with "\n", the file
//     ends with "\n", there is no CRLF, BOM, comment, or trailing whitespace;
//   - Decode is strict: any deviation is ErrMalformed, so encode(decode(p)) == p
//     for every accepted p and pointer bytes are stable identity input for the
//     asset index;
//   - a pointer never exceeds MaxSize bytes, which lets clean/smudge sniff cheaply.
//
// `-text` in .gitattributes is required so Git never rewrites the "\n" bytes.
package pointer

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// FormatVersion is the pointer format this package writes.
	FormatVersion = 1
	// VersionLine is the exact first line of a v1 pointer (without newline).
	VersionLine = "version https://fxvcs.dev/pointer/v1"
	// Prefix is what every supported pointer starts with; used for sniffing.
	Prefix = "version https://fxvcs.dev/pointer/"
	// MaxSize bounds a pointer so filters can read a prefix and decide.
	MaxSize = 1024
)

// Errors.
var (
	ErrNotPointer         = errors.New("pointer: not an fxvcs pointer")
	ErrMalformed          = errors.New("pointer: malformed")
	ErrUnsupportedVersion = errors.New("pointer: unsupported pointer format version")
	ErrInvalidField       = errors.New("pointer: invalid field")
)

var (
	digestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	domainRE   = regexp.MustCompile(`^domain_[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
	encodingRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}/[1-9][0-9]{0,3}$`)
	versionRE  = regexp.MustCompile(`^version https://fxvcs\.dev/pointer/v([1-9][0-9]*)$`)
)

// Encoding identifiers.
const (
	EncodingWhole = "whole/1" // single object == whole file, manifest has one chunk
)

// Pointer is the decoded form.
type Pointer struct {
	StorageDomainID string // domain_...
	Encoding        string // e.g. "whole/1", "cdc-fastcdc/1"
	ManifestDigest  string // sha256:... of the canonical object manifest
	OID             string // sha256:... of the whole uncompressed file
	Size            int64  // uncompressed size in bytes
}

// Validate checks every field against the v1 grammar.
func (p Pointer) Validate() error {
	if !domainRE.MatchString(p.StorageDomainID) {
		return fmt.Errorf("%w: domain %q", ErrInvalidField, p.StorageDomainID)
	}
	if !encodingRE.MatchString(p.Encoding) {
		return fmt.Errorf("%w: encoding %q", ErrInvalidField, p.Encoding)
	}
	if !digestRE.MatchString(p.ManifestDigest) {
		return fmt.Errorf("%w: manifest %q", ErrInvalidField, p.ManifestDigest)
	}
	if !digestRE.MatchString(p.OID) {
		return fmt.Errorf("%w: oid %q", ErrInvalidField, p.OID)
	}
	if p.Size < 0 {
		return fmt.Errorf("%w: size %d", ErrInvalidField, p.Size)
	}
	return nil
}

// Encode returns the exact pointer bytes. It fails on invalid fields so a
// malformed pointer can never be written into Git.
func (p Pointer) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(VersionLine)
	b.WriteByte('\n')
	// keys in ascending byte order: domain < encoding < manifest < oid < size
	fmt.Fprintf(&b, "domain %s\n", p.StorageDomainID)
	fmt.Fprintf(&b, "encoding %s\n", p.Encoding)
	fmt.Fprintf(&b, "manifest %s\n", p.ManifestDigest)
	fmt.Fprintf(&b, "oid %s\n", p.OID)
	fmt.Fprintf(&b, "size %d\n", p.Size)
	if b.Len() > MaxSize {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrMalformed, MaxSize)
	}
	return b.Bytes(), nil
}

// MustEncode panics on invalid fields; for tests and fixtures only.
func (p Pointer) MustEncode() []byte {
	b, err := p.Encode()
	if err != nil {
		panic(err)
	}
	return b
}

// Sniff reports whether data (or a prefix of it, at least len(Prefix) bytes)
// looks like a pointer of any version. It does not validate.
func Sniff(data []byte) bool {
	return bytes.HasPrefix(data, []byte(Prefix))
}

// Decode strictly parses pointer bytes.
func Decode(data []byte) (Pointer, error) {
	var p Pointer
	if !Sniff(data) {
		return p, ErrNotPointer
	}
	if len(data) > MaxSize {
		return p, fmt.Errorf("%w: exceeds %d bytes", ErrMalformed, MaxSize)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return p, fmt.Errorf("%w: missing trailing newline", ErrMalformed)
	}
	if bytes.Contains(data, []byte{'\r'}) {
		return p, fmt.Errorf("%w: CRLF line ending", ErrMalformed)
	}
	lines := strings.Split(string(data[:len(data)-1]), "\n")
	m := versionRE.FindStringSubmatch(lines[0])
	if m == nil {
		return p, fmt.Errorf("%w: bad version line", ErrMalformed)
	}
	if v, _ := strconv.Atoi(m[1]); v != FormatVersion {
		return p, fmt.Errorf("%w: v%d (this binary supports v%d)", ErrUnsupportedVersion, v, FormatVersion)
	}
	wantKeys := []string{"domain", "encoding", "manifest", "oid", "size"}
	if len(lines)-1 != len(wantKeys) {
		return p, fmt.Errorf("%w: expected %d fields, got %d", ErrMalformed, len(wantKeys), len(lines)-1)
	}
	for i, k := range wantKeys {
		line := lines[i+1]
		key, val, ok := strings.Cut(line, " ")
		if !ok || key != k || val == "" || strings.ContainsAny(val, " \t") {
			return p, fmt.Errorf("%w: line %d %q (expected key %s)", ErrMalformed, i+2, line, k)
		}
		switch k {
		case "domain":
			p.StorageDomainID = val
		case "encoding":
			p.Encoding = val
		case "manifest":
			p.ManifestDigest = val
		case "oid":
			p.OID = val
		case "size":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil || strconv.FormatInt(n, 10) != val {
				return p, fmt.Errorf("%w: size %q", ErrInvalidField, val)
			}
			p.Size = n
		}
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	// Prove determinism: the accepted bytes must equal our encoding.
	enc, err := p.Encode()
	if err != nil {
		return p, err
	}
	if !bytes.Equal(enc, data) {
		return p, fmt.Errorf("%w: non-canonical bytes", ErrMalformed)
	}
	return p, nil
}
