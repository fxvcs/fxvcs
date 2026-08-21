package gitclient

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestBatchPathsRespectsCommandLineBudget covers the splitting rule: every
// batch stays inside the byte and count budgets, and the concatenation of the
// batches is the original list, in order.
func TestBatchPathsRespectsCommandLineBudget(t *testing.T) {
	long := strings.Repeat("resources/[category]/some_resource/stream/", 4) + "asset.ytd"
	many := make([]string, 5000)
	for i := range many {
		many[i] = fmt.Sprintf("resources/pack%04d/asset%05d.ytd", i/100, i)
	}
	longPaths := make([]string, 400)
	for i := range longPaths {
		longPaths[i] = fmt.Sprintf("%s.%d", long, i)
	}
	cases := map[string][]string{
		"empty":     {},
		"one":       {"a.bin"},
		"few":       {"a.bin", "b.bin", "c.bin"},
		"many":      many,
		"long path": longPaths,
	}
	for name, paths := range cases {
		t.Run(name, func(t *testing.T) {
			var got []string
			batches := 0
			err := BatchPaths(paths, false, func(batch []string) error {
				batches++
				size := 0
				for _, p := range batch {
					size += len(p) + 1
				}
				if len(batch) > maxPathArgCount {
					t.Fatalf("batch of %d paths exceeds the count budget %d", len(batch), maxPathArgCount)
				}
				if size > maxPathArgBytes && len(batch) > 1 {
					t.Fatalf("batch of %d bytes exceeds the byte budget %d", size, maxPathArgBytes)
				}
				got = append(got, batch...)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) == 0 && batches != 0 {
				t.Fatalf("an empty list called fn %d times with callEmpty false", batches)
			}
			if !slices.Equal(got, paths) {
				t.Fatalf("batches do not reassemble the input: %d of %d paths", len(got), len(paths))
			}
		})
	}
	// callEmpty is what "no pathspec means everything" commands need.
	calls := 0
	if err := BatchPaths(nil, true, func([]string) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("callEmpty: %d calls, %v", calls, err)
	}
	// An error stops the walk at the first batch.
	sentinel := errors.New("stop")
	seen := 0
	if err := BatchPaths(many, false, func([]string) error { seen++; return sentinel }); !errors.Is(err, sentinel) || seen != 1 {
		t.Fatalf("got %v after %d batches", err, seen)
	}
}
