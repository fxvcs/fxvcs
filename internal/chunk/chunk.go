// Package chunk implements the versioned content-defined chunking (CDC) that
// FXVCS uses to store large assets as reusable pieces.
//
// The encoding identifier written into pointers and object manifests names one
// exact parameter set, never "whatever this binary happens to prefer":
//
//	whole/1        the object is one piece: the whole file
//	cdc-fastcdc/1  FastCDC with normalization level 2, min 64 KiB,
//	               average 256 KiB, max 1 MiB, and the gear table pinned by
//	               TestGearTableIsPinned
//
// Those numbers are the winning candidate of the Stage 2 corpus measurement
// (docs/stage-2-measurement.md), not a guess.
//
// That matters because the chunk list determines the object manifest, the
// manifest digest is inside the pointer, and the pointer bytes are what Git
// stores. Two clients that split the same file differently would produce
// different pointers for identical content and Git would report a spurious
// modification. Changing any parameter therefore requires a new identifier
// (cdc-fastcdc/2), never an edit here.
//
// Splitting is streaming and bounded: peak memory is one Params.Max window
// regardless of file size.
package chunk

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// Encoding identifiers. These are the values that appear in pointers and
// object manifests.
const (
	EncodingWhole     = "whole/1"
	EncodingFastCDCv1 = "cdc-fastcdc/1"
)

// Params is one pinned, versioned parameter set.
type Params struct {
	// Encoding is the identifier written into pointers and manifests.
	Encoding string
	// Min is the smallest chunk the splitter will emit (except the last).
	Min int
	// Avg is the target average chunk size; it must be a power of two.
	Avg int
	// Max is the largest chunk the splitter will emit.
	Max int
	// Normalization is the FastCDC normalized-chunking level in bits: the
	// cut mask is tightened by this many bits below Avg and loosened by the
	// same amount above it, which concentrates chunk sizes around Avg.
	Normalization uint
}

// FastCDCv1 is the parameter set named by EncodingFastCDCv1. It is a constant
// of the format, not a tunable: see the package comment.
var FastCDCv1 = Params{
	Encoding:      EncodingFastCDCv1,
	Min:           64 << 10,
	Avg:           256 << 10,
	Max:           1 << 20,
	Normalization: 2,
}

// ErrInvalidParams reports a parameter set that cannot be used.
var ErrInvalidParams = errors.New("chunk: invalid parameters")

// Validate checks the invariants the splitter relies on.
func (p Params) Validate() error {
	switch {
	case p.Min <= 0 || p.Avg <= 0 || p.Max <= 0:
		return fmt.Errorf("%w: sizes must be positive (min %d avg %d max %d)", ErrInvalidParams, p.Min, p.Avg, p.Max)
	case p.Min > p.Avg || p.Avg > p.Max:
		return fmt.Errorf("%w: need min <= avg <= max (min %d avg %d max %d)", ErrInvalidParams, p.Min, p.Avg, p.Max)
	case bits.OnesCount64(uint64(p.Avg)) != 1:
		return fmt.Errorf("%w: avg %d is not a power of two", ErrInvalidParams, p.Avg)
	}
	b := uint(bits.TrailingZeros64(uint64(p.Avg)))
	if p.Normalization >= b {
		return fmt.Errorf("%w: normalization %d must be below log2(avg)=%d", ErrInvalidParams, p.Normalization, b)
	}
	return nil
}

// masks returns the strict (below average) and loose (above average) cut masks.
func (p Params) masks() (small, large uint64) {
	b := uint(bits.TrailingZeros64(uint64(p.Avg)))
	small = 1<<(b+p.Normalization) - 1
	large = 1<<(b-p.Normalization) - 1
	return small, large
}

// Chunk is one emitted piece.
type Chunk struct {
	// Offset is the byte offset of the chunk in the input stream.
	Offset int64
	// Data is the chunk content. It aliases the splitter's window and is only
	// valid until the callback returns; copy it if it must outlive the call.
	Data []byte
}

// Split reads r to EOF and calls fn once per chunk, in order. Peak memory is
// p.Max bytes plus the reader's own buffering, whatever the input size. An
// error from fn stops the walk and is returned unchanged.
//
// An empty input produces exactly one empty chunk, so that every object has at
// least one chunk and the manifest schema's minItems holds.
func Split(r io.Reader, p Params, fn func(Chunk) error) error {
	if err := p.Validate(); err != nil {
		return err
	}
	maskS, maskL := p.masks()
	buf := make([]byte, p.Max)
	filled, offset := 0, int64(0)
	eof := false
	emitted := false
	for {
		if !eof && filled < p.Max {
			n, err := io.ReadFull(r, buf[filled:])
			filled += n
			switch {
			case err == nil:
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				eof = true
			default:
				return err
			}
		}
		if filled == 0 {
			if !emitted {
				return fn(Chunk{Offset: 0, Data: buf[:0]})
			}
			return nil
		}
		n := cutpoint(buf[:filled], p, maskS, maskL, eof)
		if err := fn(Chunk{Offset: offset, Data: buf[:n]}); err != nil {
			return err
		}
		emitted = true
		offset += int64(n)
		copy(buf, buf[n:filled])
		filled -= n
	}
}

// cutpoint returns the length of the next chunk in window. When the window is
// not yet full and more input may follow, the whole window is consumed only if
// no cut point exists inside it.
func cutpoint(window []byte, p Params, maskS, maskL uint64, eof bool) int {
	n := len(window)
	if n <= p.Min {
		return n
	}
	normal := p.Avg
	if n < normal {
		normal = n
	}
	var fp uint64
	i := p.Min
	for ; i < normal; i++ {
		fp = (fp << 1) + gear[window[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = (fp << 1) + gear[window[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	_ = eof
	return n
}

// SplitFixed is the fixed-size splitter used as the measurement baseline in
// docs/stage-2-measurement.md. It is never used for stored objects: no
// encoding identifier names it.
func SplitFixed(r io.Reader, size int, fn func(Chunk) error) error {
	if size <= 0 {
		return fmt.Errorf("%w: fixed chunk size %d", ErrInvalidParams, size)
	}
	buf := make([]byte, size)
	offset := int64(0)
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if ferr := fn(Chunk{Offset: offset, Data: buf[:n]}); ferr != nil {
				return ferr
			}
			offset += int64(n)
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			if offset == 0 {
				return fn(Chunk{Offset: 0, Data: buf[:0]})
			}
			return nil
		default:
			return err
		}
	}
}
