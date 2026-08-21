package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/format"
)

// TestPerformanceFloor measures what a caller pays per Status call and per
// asset, on whatever platform it runs on. It is skipped unless FXVCS_PERF is
// set; run it through .github/workflows/perf.yml to compare platforms.
//
// It reports two numbers because they decide different things:
//
//   - the floor: what one call costs regardless of how little is asked for.
//     It is dominated by process spawns (Open plus ls-files, check-attr and two
//     cat-file passes), so it varies by platform far more than the rest, and it
//     is what an interactive caller pays on a cache miss.
//   - the marginal cost per asset, which sets how much a caller can afford to
//     ask for in one call. It is file I/O and SQLite rather than spawns, and a
//     chunked asset costs per stored object, not per asset.
//
// Timings are best-of-N: on a shared or virtualized machine the minimum is the
// closest estimate of the work, and the tail is the machine.
func TestPerformanceFloor(t *testing.T) {
	if os.Getenv("FXVCS_PERF") == "" {
		t.Skip("set FXVCS_PERF=1 to run the performance measurement")
	}
	t.Logf("platform: %s/%s  go %s  cpus %d", runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU())

	whole := measure(t, perfShape{name: "whole/1", assets: 1000, size: 4 << 10, plain: 2000})
	chunked := measure(t, perfShape{name: "cdc-fastcdc/1", assets: 150, size: 2 << 20})

	t.Log("")
	t.Log("=== summary ===============================================")
	t.Logf("platform                    %s/%s", runtime.GOOS, runtime.GOARCH)
	t.Logf("call floor (open + 1 path)  %7.1f ms", whole.floor)
	t.Logf("marginal, whole/1           %7.3f ms/asset  (%.0f objects/asset)", whole.perAsset, whole.perAssetObjects)
	t.Logf("marginal, chunked           %7.3f ms/asset  (%.1f objects/asset)", chunked.perAsset, chunked.perAssetObjects)
	t.Logf("marginal, per stored object %7.4f ms", (chunked.perAsset-whole.perAsset)/(chunked.perAssetObjects-whole.perAssetObjects))
	t.Log("===========================================================")
}

type perfShape struct {
	name   string
	assets int
	size   int
	plain  int // untracked-by-fxvcs files, so the index is bigger than the asset set
}

type perfResult struct {
	floor           float64
	perAsset        float64
	perAssetObjects float64
}

func measure(t *testing.T, sh perfShape) perfResult {
	t.Helper()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	var all []string
	for i := 0; i < sh.assets; i++ {
		p := fmt.Sprintf("assets/d%02d/f%05d.bin", i%30, i)
		// Distinct content per asset: identical content would share stored
		// objects and hide the per-object cost.
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(i)+7, sh.size)), p)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
		all = append(all, p)
	}
	for i := 0; i < sh.plain; i++ {
		r.WriteFile(fmt.Sprintf("src/m%03d/f%05d.lua", i%50, i), []byte("-- x\n"))
	}
	r.Git("add", ".")
	r.Commit("fixture")
	if _, _, err := repo.Status(ctx, Selection{}); err != nil { // warm the stat cache
		t.Fatal(err)
	}
	entries, _, err := repo.ManagedEntries(ctx, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	objects := 0
	for _, e := range entries {
		c, err := repo.ContentLocal(ctx, e.Pointer)
		if err != nil {
			t.Fatal(err)
		}
		objects += len(c.Objects)
	}

	opts := Options{Installed: format.MustParseVersion("0.0.0-dev"), Operation: "perf", CacheDir: cacheDir}
	open := best(t, sh.name, "open repository", func() {
		o, err := Open(ctx, r.Root, opts)
		if err != nil {
			t.Fatal(err)
		}
		_ = o.Close()
	})
	one := best(t, sh.name, "Status, 1 path", func() {
		if _, _, err := repo.Status(ctx, Selection{Paths: all[:1]}); err != nil {
			t.Fatal(err)
		}
	})
	whole := best(t, sh.name, fmt.Sprintf("Status, all %d", sh.assets), func() {
		if _, _, err := repo.Status(ctx, Selection{}); err != nil {
			t.Fatal(err)
		}
	})
	res := perfResult{
		floor:           open + one,
		perAsset:        (whole - one) / float64(sh.assets-1),
		perAssetObjects: float64(objects) / float64(sh.assets),
	}
	t.Logf("%-14s %-24s %7.1f ms floor, %.3f ms/asset, %.1f objects/asset",
		sh.name, "derived", res.floor, res.perAsset, res.perAssetObjects)
	return res
}

// best runs f repeatedly and reports the fastest run.
func best(t *testing.T, group, name string, f func()) float64 {
	t.Helper()
	const reps = 7
	lo := 0.0
	for i := 0; i < reps; i++ {
		start := time.Now()
		f()
		ms := float64(time.Since(start).Microseconds()) / 1000
		if i == 0 || ms < lo {
			lo = ms
		}
	}
	t.Logf("%-14s %-24s %7.1f ms", group, name, lo)
	return lo
}
