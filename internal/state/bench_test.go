package state

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkMarkPublished contrasts recording publications one transaction at a
// time with recording them in batches.
//
// The ledger is opened synchronous=FULL because pending pins are
// authoritative, so every committed transaction is an fsync. That makes the
// number of transactions, not the number of rows, the thing that costs: a
// publish of a few thousand chunks recorded work that was already durable in
// the object store, and paid an fsync per row to do it.
func BenchmarkMarkPublished(b *testing.B) {
	const objects = 500
	for _, mode := range []string{"one-at-a-time", "batched"} {
		b.Run(mode, func(b *testing.B) {
			ctx := context.Background()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				l := benchLedger(b)
				digests := benchObjects(b, l, objects)
				b.StartTimer()
				switch mode {
				case "one-at-a-time":
					for _, d := range digests {
						if err := l.MarkPublished(ctx, d, "primary"); err != nil {
							b.Fatal(err)
						}
					}
				case "batched":
					byDigest := make(map[string][]string, len(digests))
					for _, d := range digests {
						byDigest[d] = []string{"primary"}
					}
					if err := l.MarkPublishedMany(ctx, byDigest); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				_ = l.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkSettle measures clearing the pins of every published object, which
// closes a publish and is the other place the ledger touched one row at a
// time.
func BenchmarkSettle(b *testing.B) {
	const objects = 500
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		l := benchLedger(b)
		digests := benchObjects(b, l, objects)
		byDigest := make(map[string][]string, len(digests))
		for _, d := range digests {
			byDigest[d] = []string{"primary"}
		}
		if err := l.MarkPublishedMany(ctx, byDigest); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := l.Settle(ctx, []string{"primary"}); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = l.Close()
		b.StartTimer()
	}
}

// BenchmarkPublicationSummary measures the always-visible repository query at
// a size large enough to expose accidental per-object database round trips.
func BenchmarkPublicationSummary(b *testing.B) {
	const objects = 10_000
	ctx := context.Background()
	l := benchLedger(b)
	digests := benchObjects(b, l, objects)
	byDigest := make(map[string][]string, len(digests)/2)
	for i, d := range digests {
		var remotes []string
		if i%2 == 0 {
			remotes = append(remotes, "primary")
		}
		if i%3 == 0 {
			remotes = append(remotes, "secondary")
		}
		if len(remotes) > 0 {
			byDigest[d] = remotes
		}
	}
	if err := l.MarkPublishedMany(ctx, byDigest); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = l.Close() })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.Summary(ctx, []string{"primary"}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchLedger(b *testing.B) *Ledger {
	b.Helper()
	l, err := OpenLedger(context.Background(), b.TempDir()+"/publication.db", Options{
		Create: true, InstalledVersion: "0.0.0-dev", StorageDomainID: "domain_bench_01",
	})
	if err != nil {
		b.Fatal(err)
	}
	return l
}

// benchObjects records n objects in one transaction, so the setup cost does
// not swamp what is being measured.
func benchObjects(b *testing.B, l *Ledger, n int) []string {
	b.Helper()
	ctx := context.Background()
	digests := make([]string, n)
	for i := 0; i < n; i++ {
		d := fmt.Sprintf("sha256:%064x", i+1)
		digests[i] = d
		if err := l.RecordIngest(ctx, ObjectRecord{
			Digest: d, StorageDomainID: "domain_bench_01", Size: 1024, Encoding: "whole",
		}); err != nil {
			b.Fatal(err)
		}
	}
	return digests
}
