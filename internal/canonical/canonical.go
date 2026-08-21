// Package canonical implements the deterministic JSON encoding used for every
// FXVCS content identity: object manifests, asset indexes, the semantic release
// digest, and realization identity.
//
// The encoding is a strict subset of JSON modelled on RFC 8785 (JSON
// Canonicalization Scheme):
//
//   - object members are sorted by the UTF-16 code units of their names;
//   - no insignificant whitespace is emitted;
//   - strings use the shortest escaping (only ", \, and control characters are
//     escaped, using \uXXXX for controls other than \b \f \n \r \t);
//   - numbers must be integers within ±2^53; floats are rejected because FXVCS
//     formats never need them and their canonical printing is a common source
//     of cross-language drift;
//   - null members are omitted only when the caller removes them; the encoder
//     itself never drops data.
//
// FXVCS never hashes YAML or Go-struct field order. Callers convert a document
// to a generic value (map[string]any / []any / string / int64 / bool / nil),
// hand it to Marshal, and hash the bytes.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrUnsupportedValue is returned for values the canonical form cannot express.
var ErrUnsupportedValue = errors.New("canonical: unsupported value")

// Marshal encodes v in canonical JSON. v may be any combination of
// map[string]any, []any, string, bool, nil, and integer types, or a value that
// json.Marshal can convert to such a tree (structs are round-tripped through
// encoding/json first so that `json` tags apply, then canonicalized).
func Marshal(v any) ([]byte, error) {
	generic, err := toGeneric(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := encode(&buf, generic); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest returns "sha256:<hex>" of the canonical encoding of v.
func Digest(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DigestBytes returns "sha256:<hex>" of raw bytes.
func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Recanonicalize parses arbitrary JSON and re-emits it canonically. It rejects
// non-integer numbers and duplicate object keys.
func Recanonicalize(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, errEOF) && err == nil {
		return nil, errors.New("canonical: trailing data after JSON value")
	}
	return Marshal(v)
}

var errEOF = errors.New("EOF")

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		if err.Error() == "EOF" {
			return nil, errEOF
		}
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := map[string]any{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, errors.New("canonical: object key is not a string")
				}
				if _, dup := m[k]; dup {
					return nil, fmt.Errorf("canonical: duplicate object key %q", k)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				m[k] = val
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return m, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("canonical: unexpected delimiter %v", t)
		}
	case json.Number:
		i, err := strconv.ParseInt(t.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: non-integer number %s", ErrUnsupportedValue, t)
		}
		return i, nil
	default:
		return tok, nil
	}
}

func toGeneric(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return uint64ToInt64(uint64(x))
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return uint64ToInt64(x)
	case json.Number:
		i, err := strconv.ParseInt(x.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: non-integer number %s", ErrUnsupportedValue, x)
		}
		return i, nil
	case float32, float64:
		return nil, fmt.Errorf("%w: floating point numbers are not canonical", ErrUnsupportedValue)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer, reflect.Interface:
		// Round-trip through encoding/json so struct tags, omitempty, and
		// custom marshalers apply. Numbers stay exact through UseNumber.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		val, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		return val, nil
	}
	return nil, fmt.Errorf("%w: %T", ErrUnsupportedValue, v)
}

func uint64ToInt64(u uint64) (any, error) {
	if u > math.MaxInt64 {
		return nil, fmt.Errorf("%w: integer overflow", ErrUnsupportedValue)
	}
	return int64(u), nil
}

func encode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case int64:
		if x > 1<<53 || x < -(1<<53) {
			return fmt.Errorf("%w: integer %d outside ±2^53", ErrUnsupportedValue, x)
		}
		buf.WriteString(strconv.FormatInt(x, 10))
	case string:
		if !utf8.ValidString(x) {
			return fmt.Errorf("%w: string is not valid UTF-8", ErrUnsupportedValue)
		}
		encodeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if !utf8.ValidString(k) {
				return fmt.Errorf("%w: object key is not valid UTF-8", ErrUnsupportedValue)
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeString(buf, k)
			buf.WriteByte(':')
			if err := encode(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedValue, v)
	}
	return nil
}

// lessUTF16 orders strings by UTF-16 code units as required by RFC 8785.
func lessUTF16(a, b string) bool {
	ua := utf16.Encode([]rune(a))
	ub := utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

const hexDigits = "0123456789abcdef"

func encodeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				buf.WriteString(`\"`)
			case '\\':
				buf.WriteString(`\\`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			default:
				if c < 0x20 {
					buf.WriteString(`\u00`)
					buf.WriteByte(hexDigits[c>>4])
					buf.WriteByte(hexDigits[c&0xF])
				} else {
					buf.WriteByte(c)
				}
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
}
