package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
)

// Benchmarks for the two operations whose cost is dominated by how much of the
// local object set they re-read: loading assets that are already cached, and
// unloading assets whose bytes must be proven recoverable first.
//
// Both are written against the public operations rather than the primitives
// underneath, so the same file measures the same thing across a change to how
// the check is implemented. Run:
//
//	go test ./internal/app -run '^$' -bench 'Load|Unload' -benchtime 3x

// benchAssets builds a repository holding n chunked assets of the given size,
// committed, published to a filesystem remote, and hydrated.
func benchAssets(b *testing.B, n, size int) (*Repo, func()) {
	b.Helper()
	r, cacheDir := testRepo(b)
	repo := openTest(b, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: b.TempDir()}); err != nil {
		b.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	contents := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("asset%03d.bin", i)
		content := asset(uint64(i)+1, size)
		ing, err := repo.Ingest(ctx, bytes.NewReader(content), name)
		if err != nil {
			b.Fatal(err)
		}
		r.WriteFile(name, ing.Pointer.MustEncode())
		contents[name] = content
	}
	r.Git("add", ".")
	r.Commit("assets")
	for name, content := range contents {
		r.WriteFile(name, content)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		b.Fatalf("publish: %v %v", failed, err)
	}
	// rehydrate restores the working tree to the loaded state between rounds.
	rehydrate := func() {
		if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
			b.Fatal(err)
		}
	}
	return repo, rehydrate
}

// BenchmarkUnloadCachedAssets measures unloading assets whose objects are all
// present locally — the ordinary case, and the one that decides how long
// reclaiming disk space takes.
func BenchmarkUnloadCachedAssets(b *testing.B) {
	for _, shape := range []struct {
		name string
		n    int
		size int
	}{
		{"20x1MiB", 20, 1 << 20},
		{"20x4MiB", 20, 4 << 20},
	} {
		b.Run(shape.name, func(b *testing.B) {
			repo, rehydrate := benchAssets(b, shape.n, shape.size)
			ctx := context.Background()
			b.SetBytes(int64(shape.n) * int64(shape.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				rehydrate()
				b.StartTimer()
				if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLoadCachedAssets measures loading assets that are already in the
// local cache, so nothing is transferred and the whole cost is deciding that
// the bytes are there and writing them out.
func BenchmarkLoadCachedAssets(b *testing.B) {
	for _, shape := range []struct {
		name string
		n    int
		size int
	}{
		{"20x1MiB", 20, 1 << 20},
		{"20x4MiB", 20, 4 << 20},
	} {
		b.Run(shape.name, func(b *testing.B) {
			repo, _ := benchAssets(b, shape.n, shape.size)
			ctx := context.Background()
			b.SetBytes(int64(shape.n) * int64(shape.size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLoadWithCacheLimit is the same load with an automatic-trim budget
// configured, which is what a machine with bounded disk actually runs.
//
// It is a separate benchmark because the budget changes the shape of the
// work, not just its size: trimming has to know how much the cache holds, and
// finding that out means walking it.
func BenchmarkLoadWithCacheLimit(b *testing.B) {
	repo, _ := benchAssets(b, 20, 1<<20)
	ctx := context.Background()
	// Far above what the repository needs, so nothing is ever evicted and the
	// benchmark measures deciding that, not the eviction itself.
	if err := repo.SetCacheLimit(ctx, 8<<30); err != nil {
		b.Fatal(err)
	}
	// A cache the size a real one reaches. The budget check has to establish
	// how much is stored, and the repository's own objects are a small part of
	// what a host-wide cache holds.
	fillCache(b, repo, 3000)
	b.SetBytes(20 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// fillCache adds n unrelated objects, so the cache is the size a shared
// host-wide one actually reaches rather than the size of one test fixture.
func fillCache(b *testing.B, repo *Repo, n int) {
	b.Helper()
	buf := make([]byte, 512)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(buf, uint64(i))
		if _, err := repo.Cache.PutBytes(repo.Domain, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIngestWithCacheLimit measures staging many files on a machine with
// an automatic-trim budget configured — what the Git clean filter does on a
// `git add` of a directory of assets.
func BenchmarkIngestWithCacheLimit(b *testing.B) {
	for _, limited := range []bool{false, true} {
		name := "no-limit"
		if limited {
			name = "limit-set"
		}
		b.Run(name, func(b *testing.B) {
			r, cacheDir := testRepo(b)
			repo := openTest(b, r, cacheDir)
			ctx := context.Background()
			if limited {
				if err := repo.SetCacheLimit(ctx, 8<<30); err != nil {
					b.Fatal(err)
				}
			}
			fillCache(b, repo, 3000)
			const files = 100
			payloads := make([][]byte, files)
			for i := range payloads {
				payloads[i] = asset(uint64(i)+500, 64<<10)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j, p := range payloads {
					if _, err := repo.Ingest(ctx, bytes.NewReader(p), fmt.Sprintf("f%03d.bin", j)); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// BenchmarkIngestOverCacheLimit stages files on a machine whose cache budget
// is already exceeded by content that cannot be evicted.
//
// Pending objects are pinned and pruning never takes them, so a repository
// staging more than its budget sits permanently over it. That is the state
// where a budget check that measures the cache is most expensive and least
// useful: the answer cannot change until the objects are published.
func BenchmarkIngestOverCacheLimit(b *testing.B) {
	r, cacheDir := testRepo(b)
	repo := openTest(b, r, cacheDir)
	ctx := context.Background()
	fillCache(b, repo, 3000)
	// A budget far below what is already pinned locally.
	if err := repo.SetCacheLimit(ctx, 1<<20); err != nil {
		b.Fatal(err)
	}
	const files = 50
	payloads := make([][]byte, files)
	for i := range payloads {
		payloads[i] = asset(uint64(i)+900, 64<<10)
	}
	// Pin more than the budget so pruning cannot get under it.
	for j, p := range payloads {
		if _, err := repo.Ingest(ctx, bytes.NewReader(p), fmt.Sprintf("p%03d.bin", j)); err != nil {
			b.Fatal(err)
		}
	}
	fresh := make([][]byte, files)
	for i := range fresh {
		fresh[i] = asset(uint64(i)+9000, 64<<10)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, p := range fresh {
			if _, err := repo.Ingest(ctx, bytes.NewReader(p), fmt.Sprintf("q%03d.bin", j)); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkLoadManyAssets is the shape a real repository has: many assets
// rather than a few large ones, so the per-path cost dominates the fixed cost
// of the Git reads that bracket the operation.
func BenchmarkLoadManyAssets(b *testing.B) {
	repo, _ := benchAssets(b, 200, 256<<10)
	ctx := context.Background()
	b.SetBytes(200 * 256 << 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := repo.Dehydrate(ctx, Selection{}, nil); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := repo.Hydrate(ctx, Selection{}, nil); err != nil {
			b.Fatal(err)
		}
	}
}
