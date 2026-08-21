package s3

import (
	"math/rand"
	"sync"
	"testing"
)

// BenchmarkEncodeBodyParallel measures compression throughput with several
// uploads in flight, which is the only shape that matters: publication runs
// many concurrent transfers, and each one compresses before it sends.
//
// It is a regression guard as much as a measurement. A single shared encoder
// configured with concurrency 1 is safe to call from many goroutines but
// serialises them — EncodeAll takes its encoder state from a channel sized by
// that setting — so every upload in the process queued behind one compressor
// while appearing to run in parallel.
func BenchmarkEncodeBodyParallel(b *testing.B) {
	s := newWithAPI(nil, Config{Bucket: "b", Compress: true})
	// Compressible content: incompressible input returns early and would
	// measure the decision, not the compressor.
	payload := compressibleBytes(1 << 20)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if out, _ := s.encodeBody(payload); len(out) == 0 {
				b.Fatal("empty encoding")
			}
		}
	})
}

// BenchmarkEncodeBodyConcurrentFixed is the same work at a fixed width, which
// makes the serialisation visible as wall clock rather than as a per-op
// average.
func BenchmarkEncodeBodyConcurrentFixed(b *testing.B) {
	const workers = 8
	s := newWithAPI(nil, Config{Bucket: "b", Compress: true})
	payload := compressibleBytes(1 << 20)
	b.SetBytes(int64(len(payload)) * workers)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.encodeBody(payload)
			}()
		}
		wg.Wait()
	}
}

// compressibleBytes builds content that zstd can actually shrink, drawn from a
// small alphabet so the result resembles the uncompressed map data the corpus
// measurement found compresses well.
func compressibleBytes(n int) []byte {
	rng := rand.New(rand.NewSource(1))
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('a' + rng.Intn(6))
	}
	return out
}
