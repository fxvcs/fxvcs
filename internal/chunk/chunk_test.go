package chunk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
)

// TestGearTableIsPinned guards the chunk boundaries of cdc-fastcdc/1. A
// different gear table would move every boundary, so every pointer produced by
// this encoding would change: that needs a new encoding identifier, not an
// edit to gear.go.
func TestGearTableIsPinned(t *testing.T) {
	var b bytes.Buffer
	for _, v := range gear {
		_ = binary.Write(&b, binary.BigEndian, v)
	}
	sum := sha256.Sum256(b.Bytes())
	const want = "b296813089ce788f48e4dde0bd831ae0bbdc7a6a779408b39b6fd54ea88ed043"
	got := hex.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("gear table digest changed\n got: %s\nwant: %s\n\nIf this change is intentional it is a new encoding identifier (cdc-fastcdc/2), not an edit to cdc-fastcdc/1.", got, want)
	}
}

// TestParamsPinned guards the other half of the encoding identity.
func TestParamsPinned(t *testing.T) {
	want := Params{Encoding: "cdc-fastcdc/1", Min: 65536, Avg: 262144, Max: 1048576, Normalization: 2}
	if FastCDCv1 != want {
		t.Fatalf("cdc-fastcdc/1 parameters changed: %+v (want %+v); a new parameter set needs a new identifier", FastCDCv1, want)
	}
	if err := FastCDCv1.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParamsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Params
	}{
		{"zero", Params{}},
		{"min above avg", Params{Min: 4, Avg: 2, Max: 8}},
		{"avg above max", Params{Min: 2, Avg: 16, Max: 8}},
		{"avg not power of two", Params{Min: 2, Avg: 3, Max: 8}},
		{"normalization too large", Params{Min: 2, Avg: 8, Max: 16, Normalization: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

// deterministic pseudo-random content with a compressible texture, so chunk
// boundaries depend on content rather than on pure noise.
func content(seed uint64, n int) []byte {
	src := rand.New(rand.NewPCG(seed, 0x5EED))
	b := make([]byte, n)
	motif := make([]byte, 48)
	fill := func() {
		for i := 0; i < len(motif); i += 8 {
			binary.LittleEndian.PutUint64(motif[i:], src.Uint64())
		}
	}
	fill()
	for off := 0; off < n; off += len(motif) {
		copy(b[off:], motif)
		if src.IntN(6) == 0 {
			fill()
		}
	}
	return b
}

func split(t *testing.T, data []byte, p Params) [][]byte {
	t.Helper()
	var out [][]byte
	var offset int64
	err := Split(bytes.NewReader(data), p, func(c Chunk) error {
		if c.Offset != offset {
			t.Fatalf("chunk offset %d, want %d", c.Offset, offset)
		}
		offset += int64(len(c.Data))
		out = append(out, bytes.Clone(c.Data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSplitCoversInputExactlyAndRespectsBounds(t *testing.T) {
	data := content(1, 12<<20)
	chunks := split(t, data, FastCDCv1)
	var joined bytes.Buffer
	for i, c := range chunks {
		joined.Write(c)
		last := i == len(chunks)-1
		if len(c) > FastCDCv1.Max {
			t.Fatalf("chunk %d is %d bytes, above max %d", i, len(c), FastCDCv1.Max)
		}
		if !last && len(c) < FastCDCv1.Min {
			t.Fatalf("chunk %d is %d bytes, below min %d", i, len(c), FastCDCv1.Min)
		}
	}
	if !bytes.Equal(joined.Bytes(), data) {
		t.Fatal("concatenated chunks do not reproduce the input")
	}
	if len(chunks) < 8 {
		t.Fatalf("expected content-defined splitting to produce many chunks, got %d", len(chunks))
	}
}

// TestSplitIsIndependentOfReadSizes is the cross-platform determinism guard:
// the boundaries must depend only on content, never on how the bytes arrive.
func TestSplitIsIndependentOfReadSizes(t *testing.T) {
	data := content(2, 5<<20)
	want := split(t, data, FastCDCv1)
	for _, readSize := range []int{1, 7, 4096, 1 << 20, 3 << 20} {
		var got [][]byte
		r := iotest{r: bytes.NewReader(data), max: readSize}
		err := Split(&r, FastCDCv1, func(c Chunk) error {
			got = append(got, bytes.Clone(c.Data))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("read size %d: %d chunks, want %d", readSize, len(got), len(want))
		}
		for i := range got {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("read size %d: chunk %d differs", readSize, i)
			}
		}
	}
}

// iotest caps how many bytes each Read returns.
type iotest struct {
	r   io.Reader
	max int
}

func (t *iotest) Read(p []byte) (int, error) {
	if len(p) > t.max {
		p = p[:t.max]
	}
	return t.r.Read(p)
}

// TestSplitLocalizesAnEdit is the property the whole stage rests on: changing
// a small region must leave the surrounding chunks untouched.
func TestSplitLocalizesAnEdit(t *testing.T) {
	v1 := content(3, 8<<20)
	v2 := bytes.Clone(v1)
	for i := 4 << 20; i < 4<<20+512; i++ {
		v2[i] ^= 0xA5
	}
	set := map[string]bool{}
	for _, c := range split(t, v1, FastCDCv1) {
		set[digest(c)] = true
	}
	var newBytes int
	c2 := split(t, v2, FastCDCv1)
	for _, c := range c2 {
		if !set[digest(c)] {
			newBytes += len(c)
		}
	}
	if newBytes > 4*FastCDCv1.Max {
		t.Fatalf("a 512-byte edit made %d new bytes (%d chunks total); expected the change to stay local", newBytes, len(c2))
	}
	if newBytes == 0 {
		t.Fatal("edited content produced no new chunks")
	}
}

// TestSplitInsertionShiftsNothingDownstream distinguishes CDC from fixed
// chunking: an insertion must not rewrite every following chunk.
func TestSplitInsertionShiftsNothingDownstream(t *testing.T) {
	v1 := content(4, 8<<20)
	v2 := append(bytes.Clone(v1[:3<<20]), append([]byte(strings.Repeat("INSERTED", 1024)), v1[3<<20:]...)...)
	set := map[string]bool{}
	for _, c := range split(t, v1, FastCDCv1) {
		set[digest(c)] = true
	}
	reused := 0
	for _, c := range split(t, v2, FastCDCv1) {
		if set[digest(c)] {
			reused += len(c)
		}
	}
	if reused < len(v1)/2 {
		t.Fatalf("only %d of %d bytes were reused after an insertion", reused, len(v1))
	}
}

func TestSplitEmptyInputEmitsOneEmptyChunk(t *testing.T) {
	chunks := split(t, nil, FastCDCv1)
	if len(chunks) != 1 || len(chunks[0]) != 0 {
		t.Fatalf("got %d chunks (first %d bytes), want exactly one empty chunk", len(chunks), len(chunks[0]))
	}
}

func TestSplitSmallInputIsOneChunk(t *testing.T) {
	data := content(5, 1000)
	chunks := split(t, data, FastCDCv1)
	if len(chunks) != 1 || !bytes.Equal(chunks[0], data) {
		t.Fatalf("got %d chunks, want the whole 1000-byte input as one", len(chunks))
	}
}

func TestSplitPropagatesCallbackError(t *testing.T) {
	sentinel := fmt.Errorf("stop here")
	err := Split(bytes.NewReader(content(6, 4<<20)), FastCDCv1, func(Chunk) error { return sentinel })
	if err != sentinel { //nolint:errorlint // identity is the contract
		t.Fatalf("got %v, want the callback error unchanged", err)
	}
}

func TestSplitFixed(t *testing.T) {
	data := content(7, 300)
	var got [][]byte
	if err := SplitFixed(bytes.NewReader(data), 128, func(c Chunk) error {
		got = append(got, bytes.Clone(c.Data))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(got[0]) != 128 || len(got[2]) != 44 {
		t.Fatalf("unexpected fixed split: %d chunks (%d, %d, %d)", len(got), len(got[0]), len(got[1]), len(got[2]))
	}
}

func TestPolicy(t *testing.T) {
	p := DefaultPolicy()
	if got := p.Encoding("resources/x.ytd", p.MinChunkedSize-1); got != EncodingWhole {
		t.Fatalf("small object encoding %q, want %q", got, EncodingWhole)
	}
	if got := p.Encoding("resources/x.ytd", p.MinChunkedSize); got != EncodingFastCDCv1 {
		t.Fatalf("large object encoding %q, want %q", got, EncodingFastCDCv1)
	}
	p.WholeOnly = map[string]bool{".rpf": true}
	if got := p.Encoding("resources/big.RPF", 1<<30); got != EncodingWhole {
		t.Fatalf("measured-no-benefit extension encoding %q, want %q", got, EncodingWhole)
	}
}

func TestPolicyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		size int64
		want string
	}{
		{"", 1 << 20, EncodingFastCDCv1},
		{"off", 1 << 30, EncodingWhole},
		{"always", 1, EncodingFastCDCv1},
		{"min=4194304", 1 << 20, EncodingWhole},
	} {
		t.Setenv(EnvPolicy, tc.env)
		p, err := PolicyFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Encoding("x.bin", tc.size); got != tc.want {
			t.Fatalf("%s=%q size %d: encoding %q, want %q", EnvPolicy, tc.env, tc.size, got, tc.want)
		}
	}
	t.Setenv(EnvPolicy, "nonsense")
	if _, err := PolicyFromEnv(); err == nil {
		t.Fatal("want an error for an unparseable override")
	}
}

func TestParamsFor(t *testing.T) {
	if p, err := ParamsFor(EncodingFastCDCv1); err != nil || p != FastCDCv1 {
		t.Fatalf("ParamsFor(%q) = %+v, %v", EncodingFastCDCv1, p, err)
	}
	if _, err := ParamsFor("cdc-fastcdc/2"); err == nil {
		t.Fatal("want an error for an unsupported encoding")
	}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestChunkBoundariesArePinned is the cross-platform reconstruction guard.
// Splitting is pure arithmetic over bytes, so the same input must produce the
// same boundaries on every platform and in every future build; if it does not,
// the same file gets different pointers on different machines. The expected
// values were produced by this implementation and are now part of the format.
func TestChunkBoundariesArePinned(t *testing.T) {
	sizes := []int{}
	for _, c := range split(t, content(42, 4<<20), FastCDCv1) {
		sizes = append(sizes, len(c))
	}
	want := pinnedBoundaries
	if len(sizes) != len(want) {
		t.Fatalf("got %d chunks %v, want %d %v", len(sizes), sizes, len(want), want)
	}
	for i := range sizes {
		if sizes[i] != want[i] {
			t.Fatalf("chunk %d is %d bytes, want %d (full: %v)\n\nA change here changes every cdc-fastcdc/1 pointer and needs a new encoding identifier.", i, sizes[i], want[i], sizes)
		}
	}
	// The whole-file digest of the concatenation is equally pinned.
	if got := digest(content(42, 4<<20)); got != pinnedDigest {
		t.Fatalf("fixture content digest %s, want %s", got, pinnedDigest)
	}
}

// pinnedBoundaries and pinnedDigest are the recorded output of cdc-fastcdc/1
// for content(42, 4 MiB). They are format constants, not test conveniences.
var pinnedBoundaries = []int{497436, 408755, 450460, 693761, 389152, 1029715, 556229, 168796}

const pinnedDigest = "4c4f94fe190e9826771619d5051e2775bf6227b033c267c211b7b5f716e738c9"
