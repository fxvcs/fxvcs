package measure

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
)

// Corpus generates a deterministic *synthetic* corpus of version pairs whose
// internal structure mimics the FiveM formats Stage 2 must decide about. It
// exists so the measurement harness is reproducible in CI and on a laptop
// without shipping copyrighted game assets; the go/no-go in
// docs/stage-2-measurement.md states plainly which numbers came from here and
// which from a real corpus.
//
// What is modelled is the property that actually decides whether CDC pays off:
// how far a small logical edit propagates through the stored bytes.
//
//	ytd  RSC7 container whose payload is one deflate stream — an edit near the
//	     start rewrites everything after it (worst case for CDC)
//	ydr  RSC7 container with independently compressed segments — an edit
//	     rewrites one segment
//	yft  like ydr with more, smaller segments
//	ymap uncompressed structured placement records — an edit rewrites one
//	     record (best case)
//	awc  audio container of already-compressed blocks — an edit replaces one
//	     block and shifts the rest
type Corpus struct {
	Dir string
	// Scale multiplies the generated sizes (1 ≈ 8-40 MiB total).
	Scale int
}

// Generate writes the corpus and returns its version pairs.
func (c Corpus) Generate() ([]Pair, error) {
	scale := c.Scale
	if scale <= 0 {
		scale = 1
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return nil, err
	}
	specs := []struct {
		format   string
		name     string
		payload  int
		segments int
		compress bool
	}{
		{"ytd", "vehicle_paint", 6 << 20 * scale, 1, true},
		{"ytd", "props_atlas", 4 << 20 * scale, 1, true},
		{"ydr", "building_a", 6 << 20 * scale, 24, true},
		{"yft", "vehicle_body", 4 << 20 * scale, 64, true},
		{"ymap", "downtown", 3 << 20 * scale, 0, false},
		{"awc", "radio_stream", 8 << 20 * scale, 96, false},
	}
	var pairs []Pair
	for i, s := range specs {
		seed := uint64(i) + 1
		blocks := logicalBlocks(seed, s.payload, blockCount(s.segments))
		v1 := assemble(blocks, s.segments, s.compress)
		// v2 differs from v1 in exactly one logical block, which is the
		// realistic "one texture re-exported / one prop moved" change.
		v2 := assemble(editOne(blocks, seed), s.segments, s.compress)
		p1 := filepath.Join(c.Dir, fmt.Sprintf("%s_%s.v1.%s", s.format, s.name, s.format))
		p2 := filepath.Join(c.Dir, fmt.Sprintf("%s_%s.v2.%s", s.format, s.name, s.format))
		if err := os.WriteFile(p1, v1, 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p2, v2, 0o600); err != nil {
			return nil, err
		}
		pairs = append(pairs, Pair{Format: s.format, Name: s.name, V1: p1, V2: p2})
	}
	return pairs, nil
}

// logicalBlocks builds the uncompressed logical payload as a list of blocks.
// Blocks are "structured": a small varying header plus compressible body, so
// that deflate behaves like a real asset container rather than like noise.
func logicalBlocks(seed uint64, total, count int) [][]byte {
	if count <= 0 {
		count = 32
	}
	src := rand.New(rand.NewPCG(seed, 0xA55E7)) //nolint:gosec // reproducible corpus, not cryptography
	size := total / count
	blocks := make([][]byte, count)
	for i := range blocks {
		b := make([]byte, size)
		binary.LittleEndian.PutUint32(b[0:], uint32(i))
		binary.LittleEndian.PutUint32(b[4:], uint32(size))
		// Repeated motifs with occasional noise: compressible, but not trivial.
		motif := make([]byte, 64)
		fill(src, motif)
		for off := 16; off < size; off += len(motif) {
			copy(b[off:], motif)
			if src.IntN(8) == 0 {
				fill(src, motif)
			}
		}
		blocks[i] = b
	}
	return blocks
}

// fill writes deterministic pseudo-random bytes (math/rand/v2 has no Read).
func fill(src *rand.Rand, b []byte) {
	for i := 0; i < len(b); i += 8 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], src.Uint64())
		copy(b[i:], word[:])
	}
}

func deflateBytes(b []byte) []byte {
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.DefaultCompression)
	if err != nil {
		return b
	}
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// rsc7 wraps segments in an RSC7-like container: 16-byte header then each
// segment prefixed with its length.
func rsc7(segments [][]byte) []byte {
	var out bytes.Buffer
	out.WriteString("RSC7")
	_ = binary.Write(&out, binary.LittleEndian, uint32(162))
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(segments)))
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))
	for _, s := range segments {
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(s)))
		out.Write(s)
	}
	return out.Bytes()
}

func blockCount(segments int) int {
	if segments <= 1 {
		return 64
	}
	return segments
}

func assemble(blocks [][]byte, segments int, compress bool) []byte {
	switch {
	case segments == 0: // ymap: raw structured records
		var out bytes.Buffer
		for _, b := range blocks {
			out.Write(b)
		}
		return out.Bytes()
	case segments == 1 && compress: // ytd: one deflate stream over everything
		var joined bytes.Buffer
		for _, b := range blocks {
			joined.Write(b)
		}
		return rsc7([][]byte{deflateBytes(joined.Bytes())})
	case compress: // ydr/yft: per-segment compression
		segs := make([][]byte, 0, len(blocks))
		for _, b := range blocks {
			segs = append(segs, deflateBytes(b))
		}
		return rsc7(segs)
	default: // awc: already-compressed blocks concatenated with a small header
		segs := make([][]byte, 0, len(blocks))
		for _, b := range blocks {
			segs = append(segs, deflateBytes(b))
		}
		var out bytes.Buffer
		out.WriteString("AWC1")
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(segs)))
		for _, s := range segs {
			_ = binary.Write(&out, binary.LittleEndian, uint32(len(s)))
			out.Write(s)
		}
		return out.Bytes()
	}
}

// editOne returns the block list with exactly one block rewritten. The
// original slice is not modified.
func editOne(blocks [][]byte, seed uint64) [][]byte {
	out := make([][]byte, len(blocks))
	copy(out, blocks)
	idx := len(out) / 3
	edited := make([]byte, len(out[idx]))
	copy(edited, out[idx])
	src := rand.New(rand.NewPCG(seed^0xED17, 7)) //nolint:gosec // reproducible corpus, not cryptography
	for i := 16; i < len(edited); i += 97 {
		edited[i] ^= byte(src.UintN(251) + 1)
	}
	out[idx] = edited
	return out
}
