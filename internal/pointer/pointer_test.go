package pointer

import (
	"errors"
	"strings"
	"testing"
)

var sample = Pointer{
	StorageDomainID: "domain_maps_01",
	Encoding:        EncodingWhole,
	ManifestDigest:  "sha256:" + strings.Repeat("ab", 32),
	OID:             "sha256:" + strings.Repeat("cd", 32),
	Size:            1048576,
}

const sampleText = "version https://fxvcs.dev/pointer/v1\n" +
	"domain domain_maps_01\n" +
	"encoding whole/1\n" +
	"manifest sha256:abababababababababababababababababababababababababababababababab\n" +
	"oid sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd\n" +
	"size 1048576\n"

func TestEncodeDecodeRoundTrip(t *testing.T) {
	b, err := sample.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != sampleText {
		t.Fatalf("encoding differs:\n%s", b)
	}
	p, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if p != sample {
		t.Fatalf("round trip mismatch: %+v", p)
	}
}

func TestDecodeRejects(t *testing.T) {
	cases := map[string]struct {
		in   string
		want error
	}{
		"not pointer":       {"hello world\n", ErrNotPointer},
		"empty":             {"", ErrNotPointer},
		"crlf":              {strings.ReplaceAll(sampleText, "\n", "\r\n"), ErrMalformed},
		"no trailing nl":    {strings.TrimSuffix(sampleText, "\n"), ErrMalformed},
		"reordered":         {strings.Replace(strings.Replace(sampleText, "domain domain_maps_01\n", "", 1), "size 1048576\n", "size 1048576\ndomain domain_maps_01\n", 1), ErrMalformed},
		"extra field":       {sampleText + "extra x\n", ErrMalformed},
		"missing field":     {strings.Replace(sampleText, "size 1048576\n", "", 1), ErrMalformed},
		"double space":      {strings.Replace(sampleText, "size 1048576", "size  1048576", 1), ErrMalformed},
		"trailing space":    {strings.Replace(sampleText, "size 1048576\n", "size 1048576 \n", 1), ErrMalformed},
		"leading zero size": {strings.Replace(sampleText, "size 1048576", "size 01048576", 1), ErrInvalidField},
		"negative size":     {strings.Replace(sampleText, "size 1048576", "size -1", 1), ErrInvalidField},
		"bad digest":        {strings.Replace(sampleText, "cdcdcdcd", "CDCDCDCD", 1), ErrInvalidField},
		"short digest":      {strings.Replace(sampleText, "oid sha256:cd", "oid sha256:", 1), ErrInvalidField},
		"md5 digest":        {strings.Replace(sampleText, "oid sha256:", "oid md5:", 1), ErrInvalidField},
		"bad domain":        {strings.Replace(sampleText, "domain_maps_01", "repo_maps_01", 1), ErrInvalidField},
		"bad encoding":      {strings.Replace(sampleText, "whole/1", "whole", 1), ErrInvalidField},
		"future version":    {strings.Replace(sampleText, "pointer/v1", "pointer/v2", 1), ErrUnsupportedVersion},
		"bom":               {"\ufeff" + sampleText, ErrNotPointer},
		"too large":         {sampleText + strings.Repeat("x", MaxSize), ErrMalformed},
	}
	for name, c := range cases {
		if _, err := Decode([]byte(c.in)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
}

func TestEncodeRejectsInvalid(t *testing.T) {
	bad := sample
	bad.OID = "sha256:xyz"
	if _, err := bad.Encode(); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("got %v", err)
	}
	if !Sniff([]byte(sampleText)) || Sniff([]byte("version https://example.com/")) {
		t.Fatal("sniff wrong")
	}
}
