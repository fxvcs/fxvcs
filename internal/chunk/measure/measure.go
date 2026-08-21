// Package measure is the Stage 2 chunking measurement spike (build plan §9,
// Stage 2): it compares whole-blob, fixed-size, and content-defined chunking
// over *version pairs* of representative assets and reports the numbers the
// go/no-go decision needs — transfer savings, stored bytes, CPU time, memory,
// and manifest overhead.
//
// It is a measurement tool, not part of the storage path. Nothing here decides
// how an object is stored; chunk.DefaultPolicy does, and its defaults are
// justified by the report this package produces
// (docs/stage-2-measurement.md).
//
// The model is deliberately simple and explicit:
//
//	stored(v1)     unique chunk bytes after ingesting version 1
//	transfer(v2)   chunk bytes that version 2 adds, i.e. what an upload of the
//	               second version actually costs once the first is published
//	stored(v1+v2)  unique chunk bytes holding both versions
//
// A strategy is worth its complexity only when transfer(v2) is much smaller
// than the whole file and the extra manifest bytes and CPU do not eat the
// saving.
package measure

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"

	"fxvcs.dev/fxvcs/internal/canonical"
	"fxvcs.dev/fxvcs/internal/chunk"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
)

// Strategy is one candidate encoding under test.
type Strategy struct {
	Name string
	// Fixed > 0 selects the fixed-size splitter; otherwise Params selects CDC.
	Fixed  int
	Params chunk.Params
	// Whole marks the no-chunking baseline.
	Whole bool
}

// Strategies is the candidate set the report compares. The candidates are
// spelled out here rather than derived from chunk.FastCDCv1 so that changing
// the shipped parameter set does not silently change what the report compared.
func Strategies() []Strategy {
	return []Strategy{
		{Name: "whole", Whole: true},
		{Name: "fixed-256K", Fixed: 256 << 10},
		{Name: "fixed-1M", Fixed: 1 << 20},
		{Name: "cdc-64K", Params: chunk.Params{Encoding: "cdc-measure/64k", Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10, Normalization: 2}},
		{Name: "cdc-256K", Params: chunk.Params{Encoding: "cdc-measure/256k", Min: 64 << 10, Avg: 256 << 10, Max: 1 << 20, Normalization: 2}},
		{Name: "cdc-1M", Params: chunk.Params{Encoding: "cdc-measure/1m", Min: 256 << 10, Avg: 1 << 20, Max: 4 << 20, Normalization: 2}},
		{Name: "cdc-4M", Params: chunk.Params{Encoding: "cdc-measure/4m", Min: 1 << 20, Avg: 4 << 20, Max: 16 << 20, Normalization: 2}},
	}
}

// Shipped names the candidate that chunk.FastCDCv1 implements, so the report
// can mark it.
const Shipped = "cdc-256K"

// Pair is one asset observed at two consecutive versions.
type Pair struct {
	// Format groups results in the report, e.g. "ytd" or "awc".
	Format string
	// Name identifies the asset within its format.
	Name string
	// V1 and V2 are file paths (see Corpus for the synthetic generator).
	V1, V2 string
}

// Result is one (format, strategy) measurement.
type Result struct {
	Format   string
	Strategy string
	Samples  int

	V1Bytes int64 // total size of the first versions
	V2Bytes int64 // total size of the second versions

	StoredV1   int64 // unique chunk bytes after v1
	TransferV2 int64 // chunk bytes v2 adds on top of v1
	StoredBoth int64 // unique chunk bytes holding both versions

	ChunksV1, ChunksV2 int
	NewChunksV2        int

	ManifestBytesV2 int64 // canonical object-manifest bytes for the v2 objects

	ZstdStoredBoth int64 // zstd level 3 size of the unique chunk set
	ZstdSeconds    float64

	SplitSeconds float64
	PeakAllocMiB float64
}

// TransferSaving is the fraction of a whole-file upload avoided for v2.
func (r Result) TransferSaving() float64 {
	if r.V2Bytes == 0 {
		return 0
	}
	return 1 - float64(r.TransferV2+r.ManifestBytesV2)/float64(r.V2Bytes)
}

// ThroughputMiBs is split+hash throughput over both versions.
func (r Result) ThroughputMiBs() float64 {
	if r.SplitSeconds <= 0 {
		return 0
	}
	return float64(r.V1Bytes+r.V2Bytes) / (1 << 20) / r.SplitSeconds
}

// CompressionSaving is the fraction of stored bytes zstd removes.
func (r Result) CompressionSaving() float64 {
	if r.StoredBoth == 0 {
		return 0
	}
	return 1 - float64(r.ZstdStoredBoth)/float64(r.StoredBoth)
}

// Run measures every strategy over every pair and aggregates by format.
func Run(pairs []Pair, strategies []Strategy) ([]Result, error) {
	agg := map[string]*Result{}
	var order []string
	for _, st := range strategies {
		for _, p := range pairs {
			key := p.Format + "\x00" + st.Name
			r, ok := agg[key]
			if !ok {
				r = &Result{Format: p.Format, Strategy: st.Name}
				agg[key] = r
				order = append(order, key)
			}
			if err := measurePair(p, st, r); err != nil {
				return nil, err
			}
		}
	}
	out := make([]Result, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Format != out[j].Format {
			return out[i].Format < out[j].Format
		}
		return out[i].Strategy < out[j].Strategy
	})
	return out, nil
}

type chunkRef struct {
	digest string
	size   int64
}

func measurePair(p Pair, st Strategy, r *Result) error {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()

	v1, size1, err := split(p.V1, st)
	if err != nil {
		return err
	}
	v2, size2, err := split(p.V2, st)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	have := map[string]int64{}
	var storedV1 int64
	for _, c := range v1 {
		if _, ok := have[c.digest]; !ok {
			have[c.digest] = c.size
			storedV1 += c.size
		}
	}
	var transfer int64
	newChunks := 0
	both := map[string]int64{}
	for d, s := range have {
		both[d] = s
	}
	for _, c := range v2 {
		if _, ok := have[c.digest]; !ok {
			if _, dup := both[c.digest]; !dup {
				both[c.digest] = c.size
				transfer += c.size
				newChunks++
			}
		}
	}
	var storedBoth int64
	for _, s := range both {
		storedBoth += s
	}

	r.Samples++
	r.V1Bytes += size1
	r.V2Bytes += size2
	r.StoredV1 += storedV1
	r.TransferV2 += transfer
	r.StoredBoth += storedBoth
	r.ChunksV1 += len(v1)
	r.ChunksV2 += len(v2)
	r.NewChunksV2 += newChunks
	r.ManifestBytesV2 += manifestBytes(v2, size2, st)
	r.SplitSeconds += elapsed.Seconds()
	if mib := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20); mib > r.PeakAllocMiB {
		r.PeakAllocMiB = mib
	}

	zstdBytes, zstdSecs, err := compressUnique(p, st, both)
	if err != nil {
		return err
	}
	r.ZstdStoredBoth += zstdBytes
	r.ZstdSeconds += zstdSecs
	return nil
}

// split walks one file with the strategy and returns its chunk references.
func split(path string, st Strategy) ([]chunkRef, int64, error) {
	f, err := os.Open(path) //nolint:gosec // measurement tool over an operator-named corpus
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var refs []chunkRef
	var total int64
	collect := func(c chunk.Chunk) error {
		sum := sha256.Sum256(c.Data)
		refs = append(refs, chunkRef{digest: "sha256:" + hex.EncodeToString(sum[:]), size: int64(len(c.Data))})
		total += int64(len(c.Data))
		return nil
	}
	switch {
	case st.Whole:
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, 0, err
		}
		return []chunkRef{{digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), size: n}}, n, nil
	case st.Fixed > 0:
		err = chunk.SplitFixed(f, st.Fixed, collect)
	default:
		err = chunk.Split(f, st.Params, collect)
	}
	if err != nil {
		return nil, 0, err
	}
	return refs, total, nil
}

// manifestBytes is the canonical object-manifest size for one object: the
// per-version metadata a chunked encoding adds on top of the chunk bytes.
func manifestBytes(refs []chunkRef, size int64, st Strategy) int64 {
	enc := chunk.EncodingWhole
	if !st.Whole {
		enc = chunk.EncodingFastCDCv1
	}
	m := domain.ObjectManifest{
		FormatVersion:   format.ObjectManifestVersion,
		StorageDomainID: "domain_measurement",
		Encoding:        enc,
		Compression:     "none",
		OID:             "sha256:" + hex.EncodeToString(make([]byte, 32)),
		Size:            size,
	}
	for _, c := range refs {
		m.Chunks = append(m.Chunks, domain.ManifestChunk{Digest: c.digest, RawSize: c.size, EncodedSize: c.size})
	}
	b, err := canonical.Marshal(m)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// compressUnique zstd-compresses the unique chunk set of both versions, which
// is what a compressing object store would actually hold.
func compressUnique(p Pair, st Strategy, unique map[string]int64) (int64, float64, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return 0, 0, err
	}
	defer enc.Close()
	seen := map[string]bool{}
	var total int64
	var secs float64
	for _, path := range []string{p.V1, p.V2} {
		f, err := os.Open(path) //nolint:gosec // measurement tool over an operator-named corpus
		if err != nil {
			return 0, 0, err
		}
		visit := func(c chunk.Chunk) error {
			sum := sha256.Sum256(c.Data)
			d := "sha256:" + hex.EncodeToString(sum[:])
			if seen[d] {
				return nil
			}
			if _, ok := unique[d]; !ok {
				return nil
			}
			seen[d] = true
			start := time.Now()
			out := enc.EncodeAll(c.Data, nil)
			secs += time.Since(start).Seconds()
			// A store keeps whichever form is smaller.
			if len(out) < len(c.Data) {
				total += int64(len(out))
			} else {
				total += int64(len(c.Data))
			}
			return nil
		}
		switch {
		case st.Whole:
			data, rerr := io.ReadAll(f)
			if rerr == nil {
				err = visit(chunk.Chunk{Data: data})
			} else {
				err = rerr
			}
		case st.Fixed > 0:
			err = chunk.SplitFixed(f, st.Fixed, visit)
		default:
			err = chunk.Split(f, st.Params, visit)
		}
		f.Close()
		if err != nil {
			return 0, 0, fmt.Errorf("compress %s: %w", path, err)
		}
	}
	return total, secs, nil
}
