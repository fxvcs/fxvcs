package app

import (
	"io"
	"strings"
	"testing"
)

func TestProgressReaderDoesNotSkipIntervalsForLargeReadBuffers(t *testing.T) {
	var reports []int64
	r := &progressReader{
		r:    strings.NewReader(strings.Repeat("x", 20)),
		step: 8, next: 8,
		fn: func(n int64) { reports = append(reports, n) },
	}
	if n, err := io.Copy(io.Discard, r); err != nil || n != 20 {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	if len(reports) != 2 || reports[0] != 8 || reports[1] != 16 {
		t.Fatalf("reports = %v, want [8 16]", reports)
	}
}
