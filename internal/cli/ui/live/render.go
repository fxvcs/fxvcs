package live

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Renderer paints a bounded region at the bottom of the terminal and keeps it
// there while permanent lines scroll above it.
//
// The whole trick is that the region's height is known and small: to repaint,
// move the cursor up by exactly the number of lines drawn last time, rewrite
// each line, and erase any that are no longer needed. That only works while
// the region fits on screen, which is what the Model guarantees — the previous
// implementation redrew one line per item and broke the moment an operation
// had more items than the terminal had rows.
type Renderer struct {
	w    io.Writer
	rate time.Duration
	now  func() time.Time

	mu       sync.Mutex
	drawn    int
	lastDraw time.Time
	hidden   bool
	stopped  bool
}

// RedrawInterval bounds how often the region is repainted. Faster looks no
// better and costs the terminal real work when hundreds of events arrive per
// second.
const RedrawInterval = 80 * time.Millisecond

// NewRenderer returns a renderer writing to w.
func NewRenderer(w io.Writer) *Renderer {
	return &Renderer{w: w, rate: RedrawInterval, now: time.Now}
}

// WithClock injects a clock (tests).
func (r *Renderer) WithClock(now func() time.Time) *Renderer {
	r.now = now
	return r
}

// Draw repaints the region, at most once per RedrawInterval unless forced.
func (r *Renderer) Draw(view string, force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if !force && r.now().Sub(r.lastDraw) < r.rate {
		return
	}
	r.lastDraw = r.now()
	r.paint(view)
}

// Print writes a permanent line above the region: it scrolls with the rest of
// the terminal history and is not erased when the region is.
func (r *Renderer) Print(line string) {
	if line == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.erase()
	fmt.Fprintln(r.w, line)
}

// Stop erases the region and restores the cursor. It is safe to call twice.
func (r *Renderer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	r.erase()
	if r.hidden {
		fmt.Fprint(r.w, showCursor)
		r.hidden = false
	}
}

// ANSI sequences. Only these five are used, and every one of them is undone
// by Stop.
const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	clearLine  = "\x1b[2K"
	cursorUp   = "\x1b[%dA"
	carriage   = "\r"
)

// paint rewrites the region in place. Callers hold mu.
func (r *Renderer) paint(view string) {
	lines := splitLines(view)
	var b strings.Builder
	if !r.hidden {
		b.WriteString(hideCursor)
		r.hidden = true
	}
	if r.drawn > 0 {
		fmt.Fprintf(&b, cursorUp, r.drawn)
	}
	b.WriteString(carriage)
	for _, l := range lines {
		b.WriteString(clearLine)
		b.WriteString(l)
		b.WriteString("\n")
	}
	// Erase what the previous, taller frame left behind, then come back.
	extra := r.drawn - len(lines)
	for i := 0; i < extra; i++ {
		b.WriteString(clearLine)
		b.WriteString("\n")
	}
	if extra > 0 {
		fmt.Fprintf(&b, cursorUp, extra)
	}
	r.drawn = len(lines)
	fmt.Fprint(r.w, b.String())
}

// erase removes the region without restoring the cursor. Callers hold mu.
func (r *Renderer) erase() {
	if r.drawn == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, cursorUp, r.drawn)
	b.WriteString(carriage)
	for i := 0; i < r.drawn; i++ {
		b.WriteString(clearLine)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, cursorUp, r.drawn)
	r.drawn = 0
	fmt.Fprint(r.w, b.String())
}

func splitLines(view string) []string {
	trimmed := strings.TrimRight(view, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
