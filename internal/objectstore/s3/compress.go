package s3

import (
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// The storage codec.
//
// Compression here is a property of the *stored copy*, never of object
// identity. Digests, manifests, and pointers are computed over raw bytes, so
// two clients with different zstd builds still produce the same pointer for
// the same file — an identity that depended on a compressor version could not
// promise that. The store therefore records how it encoded each object in
// object metadata and decodes on the way out.
//
// The corpus measurement (docs/stage-2-measurement.md) found zstd removes ~87%
// of uncompressed map data and *nothing* from RSC7 and audio containers, whose
// payloads are already compressed. Compressing those would burn CPU for no
// saving, so an object is stored compressed only when the compressed form is
// meaningfully smaller.
const (
	metaCodec   = "fxvcs-codec"
	metaRawSize = "fxvcs-raw-size"

	codecNone = ""
	codecZstd = "zstd/1"

	// compressionGain is the fraction an object must shrink before the store
	// keeps the compressed form. Below it the decode cost on every read is
	// not worth the bytes.
	compressionGain = 0.05
)

var (
	// One encoder per goroutine that needs one, not one shared by all of them.
	//
	// EncodeAll takes an encoder state from a channel sized by the encoder's
	// configured concurrency, so a single WithEncoderConcurrency(1) encoder
	// shared across uploads serialised every compression in the process: the
	// uploads ran concurrently and then queued, one at a time, to be encoded.
	// Pooling concurrency-1 encoders keeps the reason that setting was chosen
	// — predictable memory per encoder — while letting the workers proceed.
	encoderPool = sync.Pool{New: func() any {
		// SpeedDefault (level 3) is the ratio/throughput knee for asset data.
		e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return err
		}
		return e
	}}
	decoderPool = sync.Pool{New: func() any {
		d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return err
		}
		return d
	}}
)

// encodeBody returns the bytes to store and the metadata describing them.
func (s *Store) encodeBody(raw []byte) ([]byte, map[string]string) {
	if !s.cfg.Compress || len(raw) == 0 {
		return raw, nil
	}
	v := encoderPool.Get()
	enc, ok := v.(*zstd.Encoder)
	if !ok {
		return raw, nil // compression unavailable; store raw
	}
	packed := enc.EncodeAll(raw, nil)
	encoderPool.Put(enc)
	if float64(len(packed)) > float64(len(raw))*(1-compressionGain) {
		return raw, nil // not worth a decode on every read
	}
	return packed, map[string]string{
		metaCodec:   codecZstd,
		metaRawSize: fmt.Sprint(len(raw)),
	}
}

// codecOf reads the storage codec from object metadata. Metadata keys come
// back from providers with inconsistent capitalisation, so match case
// insensitively.
func codecOf(meta map[string]string) string {
	for k, v := range meta {
		if equalFold(k, metaCodec) {
			return v
		}
	}
	return codecNone
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// decodeBody wraps a compressed body so the caller reads raw bytes.
func decodeBody(codec string, body io.ReadCloser) (io.ReadCloser, error) {
	if codec != codecZstd {
		_ = body.Close()
		return nil, fmt.Errorf("s3 store: object uses unknown storage codec %q; run: fxvcs upgrade --check", codec)
	}
	v := decoderPool.Get()
	dec, ok := v.(*zstd.Decoder)
	if !ok {
		_ = body.Close()
		return nil, fmt.Errorf("s3 store: zstd decoder unavailable: %v", v)
	}
	if err := dec.Reset(body); err != nil {
		_ = body.Close()
		decoderPool.Put(dec)
		return nil, err
	}
	return &decodedBody{dec: dec, src: body}, nil
}

type decodedBody struct {
	dec *zstd.Decoder
	src io.ReadCloser
}

func (d *decodedBody) Read(p []byte) (int, error) { return d.dec.Read(p) }

func (d *decodedBody) Close() error {
	// Reset with no source releases the decoder's hold on the body without
	// freeing its buffers, so it can go back in the pool.
	_ = d.dec.Reset(nil)
	decoderPool.Put(d.dec)
	return d.src.Close()
}
