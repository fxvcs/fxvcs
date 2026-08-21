package live

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func newTestRenderer(buf *bytes.Buffer, clock *time.Time) *Renderer {
	return NewRenderer(buf).WithClock(func() time.Time { return *clock })
}

// TestRepaintRewindsExactlyWhatItDrew is the arithmetic the whole region
// depends on: the cursor must go up by the number of lines drawn last time,
// no more and no less, or output above the region gets overwritten.
func TestRepaintRewindsExactlyWhatItDrew(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	r := newTestRenderer(&buf, &now)

	r.Draw("one\ntwo\nthree\n", true)
	first := buf.String()
	if strings.Contains(first, "\x1b[0A") {
		t.Fatal("first frame must not move the cursor up: nothing was drawn yet")
	}
	if !strings.HasPrefix(first, hideCursor) {
		t.Fatalf("cursor not hidden on the first frame: %q", first)
	}

	buf.Reset()
	now = now.Add(time.Second)
	r.Draw("one\ntwo\nthree\n", true)
	if !strings.Contains(buf.String(), "\x1b[3A") {
		t.Fatalf("repaint did not rewind 3 lines: %q", buf.String())
	}

	// A shorter frame must erase the lines it no longer uses.
	buf.Reset()
	now = now.Add(time.Second)
	r.Draw("only\n", true)
	out := buf.String()
	if !strings.Contains(out, "\x1b[3A") {
		t.Fatalf("shrinking frame did not rewind the old height: %q", out)
	}
	if strings.Count(out, clearLine) < 3 {
		t.Fatalf("shrinking frame left stale lines behind: %q", out)
	}
}

// TestThrottleLimitsRepaints keeps a flood of events from making the terminal
// do the work instead of the operation.
func TestThrottleLimitsRepaints(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	r := newTestRenderer(&buf, &now)

	r.Draw("frame\n", true)
	buf.Reset()
	for i := 0; i < 100; i++ {
		now = now.Add(time.Millisecond)
		r.Draw("frame\n", false)
	}
	repaints := strings.Count(buf.String(), clearLine)
	if repaints == 0 {
		t.Fatal("throttling suppressed every repaint")
	}
	if repaints > 3 {
		t.Fatalf("%d repaints in 100ms; the interval is %s", repaints, RedrawInterval)
	}
	// force always paints.
	buf.Reset()
	r.Draw("frame\n", true)
	if !strings.Contains(buf.String(), clearLine) {
		t.Fatal("a forced draw was throttled")
	}
}

// TestPrintScrollsAboveTheRegion: a permanent line must not be erased by the
// next repaint, which means the region has to be taken down first.
func TestPrintScrollsAboveTheRegion(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	r := newTestRenderer(&buf, &now)
	r.Draw("region line\n", true)
	buf.Reset()

	r.Print("failed  a.ytd")
	out := buf.String()
	if !strings.Contains(out, "\x1b[1A") {
		t.Fatalf("region was not taken down before printing: %q", out)
	}
	if !strings.Contains(out, "failed  a.ytd\n") {
		t.Fatalf("permanent line missing: %q", out)
	}
	// The next frame starts from scratch, so it must not rewind over the
	// line just printed.
	buf.Reset()
	now = now.Add(time.Second)
	r.Draw("region line\n", true)
	if strings.Contains(buf.String(), "\x1b[1A") {
		t.Fatalf("repaint rewound over a permanent line: %q", buf.String())
	}
}

// TestStopRestoresTheTerminal: whatever happens, the cursor comes back and no
// region text is left for the shell prompt to collide with.
func TestStopRestoresTheTerminal(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	r := newTestRenderer(&buf, &now)
	r.Draw("a\nb\n", true)
	buf.Reset()

	r.Stop()
	out := buf.String()
	if !strings.Contains(out, showCursor) {
		t.Fatalf("cursor not restored: %q", out)
	}
	if !strings.Contains(out, "\x1b[2A") {
		t.Fatalf("region not erased: %q", out)
	}
	// Idempotent, and inert afterwards.
	buf.Reset()
	r.Stop()
	r.Draw("more\n", true)
	r.Print("late")
	if buf.String() != "" {
		t.Fatalf("renderer wrote after Stop: %q", buf.String())
	}
}

// TestStopWithoutDrawWritesNothing: a command that reports no progress must
// not emit a single control sequence.
func TestStopWithoutDrawWritesNothing(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	newTestRenderer(&buf, &now).Stop()
	if buf.String() != "" {
		t.Fatalf("renderer wrote without drawing: %q", buf.String())
	}
}

// TestModelAndRendererTogetherStayBounded is the end-to-end shape of the fix:
// hundreds of items, and the region never grows.
func TestModelAndRendererTogetherStayBounded(t *testing.T) {
	var buf bytes.Buffer
	now := time.Now()
	r := newTestRenderer(&buf, &now)
	m := New(&buf, "loading", false).WithClock(func() time.Time { return now })
	m, _ = m.Apply(SizeMsg{Width: 80, Height: 24})
	m, _ = m.Apply(PlanMsg{Verb: "loading", Count: 600, TotalBytes: 600 << 20})

	maxLines := 0
	for i := 0; i < 600; i++ {
		key := "asset" + string(rune('a'+i%26))
		m, _ = m.Apply(StartMsg{Item: Item{Key: key, Label: key, Total: 1 << 20}})
		m, _ = m.Apply(FinishMsg{Item: Item{Key: key, Bytes: 1 << 20}})
		now = now.Add(10 * time.Millisecond)
		r.Draw(m.View(), false)
		if n := len(splitLines(m.View())); n > maxLines {
			maxLines = n
		}
	}
	if maxLines > summaryLines+maxActiveRows+1 {
		t.Fatalf("region grew to %d lines", maxLines)
	}
	r.Stop()
	// Every cursor move must be within the region's height.
	for _, move := range cursorMoves(buf.String()) {
		if move > summaryLines+maxActiveRows+1 {
			t.Fatalf("cursor moved %d lines, beyond the region", move)
		}
	}
}

// cursorMoves extracts the N of every "\x1b[NA" in the stream.
func cursorMoves(s string) []int {
	var out []int
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			return out
		}
		s = s[i+2:]
		j := strings.IndexByte(s, 'A')
		if j < 0 || j > 4 {
			continue
		}
		n := 0
		ok := true
		for _, c := range s[:j] {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if ok {
			out = append(out, n)
		}
	}
}
