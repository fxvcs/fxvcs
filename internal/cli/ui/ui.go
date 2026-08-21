// Package ui is the presentation layer of the fxvcs CLI: headers, footers,
// aligned tables, item rows with a state word (and an optional symbol on a
// terminal), hints, byte formatting, and plural helpers. It holds no business
// logic and knows nothing about repositories; commands hand it already
// computed facts.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// State classifies an item row. The word printed next to it is always
// present; the symbol and colour are decoration on a terminal only.
type State int

// Row states.
const (
	OK      State = iota // ✓ done, loaded, present
	Pending              // ○ placeholder, not yet done
	Warn                 // ! needs attention, changed locally
	Fail                 // ✗ failed
	Skip                 // – skipped, refused
	Plain                // no symbol
)

func (s State) symbol() string {
	switch s {
	case OK:
		return "✓"
	case Pending:
		return "○"
	case Warn:
		return "!"
	case Fail:
		return "✗"
	case Skip:
		return "–"
	}
	return " "
}

func (s State) color() string {
	switch s {
	case OK:
		return "\x1b[32m" // green
	case Pending:
		return "\x1b[36m" // cyan
	case Warn:
		return "\x1b[33m" // yellow
	case Fail:
		return "\x1b[31m" // red
	case Skip:
		return "\x1b[2m" // dim
	}
	return ""
}

const reset = "\x1b[0m"

// Item is one row of a command body: a state, its label word, the
// repository-relative path, an already formatted size (or ""), and a note.
type Item struct {
	State State
	Label string
	Path  string
	Size  string
	Note  string
}

// Options controls decoration and verbosity.
type Options struct {
	// Color enables ANSI colours and the state symbols. Callers decide from
	// the terminal, NO_COLOR, and --color; see Detect.
	Color bool
	// Quiet suppresses everything except errors; renderers check Quiet
	// before printing.
	Quiet bool
	// Verbose lets renderers add digests, codes, and internal paths.
	Verbose bool
}

// Printer writes formatted output to one writer.
type Printer struct {
	w    io.Writer
	opts Options
}

// New creates a Printer for w.
func New(w io.Writer, opts Options) *Printer { return &Printer{w: w, opts: opts} }

// Writer returns the underlying writer.
func (p *Printer) Writer() io.Writer { return p.w }

// Quiet reports --quiet.
func (p *Printer) Quiet() bool { return p.opts.Quiet }

// Verbose reports --verbose.
func (p *Printer) Verbose() bool { return p.opts.Verbose }

// Color reports whether decoration is on.
func (p *Printer) Color() bool { return p.opts.Color }

// Header prints the first line of a command's output:
// "fxvcs <command> — <summary>".
func (p *Printer) Header(command, summary string) {
	if p.opts.Quiet {
		return
	}
	if summary == "" {
		fmt.Fprintf(p.w, "fxvcs %s\n", command)
		return
	}
	fmt.Fprintf(p.w, "fxvcs %s — %s\n", command, summary)
}

// Line prints one plain line.
func (p *Printer) Line(format string, args ...any) {
	if p.opts.Quiet {
		return
	}
	fmt.Fprintf(p.w, format+"\n", args...)
}

// Blank prints an empty line.
func (p *Printer) Blank() {
	if p.opts.Quiet {
		return
	}
	fmt.Fprintln(p.w)
}

// Footer prints the closing summary preceded by a blank line.
func (p *Printer) Footer(text string) {
	if p.opts.Quiet || text == "" {
		return
	}
	fmt.Fprintf(p.w, "\n%s\n", text)
}

// Hint prints an indented "hint:" line with an exact next command.
func (p *Printer) Hint(command string) {
	if p.opts.Quiet || command == "" {
		return
	}
	fmt.Fprintf(p.w, "  hint: %s\n", command)
}

// Verbosef prints only under --verbose.
func (p *Printer) Verbosef(format string, args ...any) {
	if !p.opts.Verbose || p.opts.Quiet {
		return
	}
	fmt.Fprintf(p.w, format+"\n", args...)
}

// Items prints item rows as an aligned, two-space indented table:
// [symbol] label  path  size  note.
func (p *Printer) Items(items []Item) {
	if p.opts.Quiet || len(items) == 0 {
		return
	}
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		label := it.Label
		if p.opts.Color {
			label = it.State.symbol() + " " + label
		}
		rows = append(rows, []string{label, it.Path, it.Size, it.Note})
	}
	widths := columnWidths(rows)
	for i, it := range items {
		var b strings.Builder
		b.WriteString("  ")
		for c, cell := range rows[i] {
			last := c == len(rows[i])-1
			text := cell
			if c == 0 && p.opts.Color {
				text = it.State.color() + cell + reset
			}
			b.WriteString(text)
			if !last {
				b.WriteString(strings.Repeat(" ", widths[c]-utf8.RuneCountInString(cell)+2))
			}
		}
		fmt.Fprintln(p.w, strings.TrimRight(b.String(), " "))
	}
}

// Table prints rows as aligned columns, two-space indented; the first row
// may be a header (callers pass it like any other row).
func (p *Printer) Table(rows [][]string) {
	if p.opts.Quiet || len(rows) == 0 {
		return
	}
	widths := columnWidths(rows)
	for _, row := range rows {
		var b strings.Builder
		b.WriteString("  ")
		for c, cell := range row {
			b.WriteString(cell)
			if c != len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[c]-utf8.RuneCountInString(cell)+2))
			}
		}
		fmt.Fprintln(p.w, strings.TrimRight(b.String(), " "))
	}
}

// List prints one indented line per entry.
func (p *Printer) List(entries []string) {
	if p.opts.Quiet {
		return
	}
	for _, e := range entries {
		fmt.Fprintf(p.w, "  %s\n", e)
	}
}

func columnWidths(rows [][]string) []int {
	var widths []int
	for _, row := range rows {
		for c, cell := range row {
			for len(widths) <= c {
				widths = append(widths, 0)
			}
			if n := utf8.RuneCountInString(cell); n > widths[c] {
				widths[c] = n
			}
		}
	}
	return widths
}

// Bytes formats a byte count in human units (B, KiB, MiB, GiB, ...).
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// SignedBytes formats a size difference with its direction: "+2.4 MiB",
// "-1.1 MiB", or "same size" for a change that kept the byte count. It is what
// a managed asset has instead of a line count, because a diff of two
// placeholders says one line changed however many megabytes moved.
func SignedBytes(n int64) string {
	switch {
	case n > 0:
		return "+" + Bytes(n)
	case n < 0:
		return "-" + Bytes(-n)
	}
	return "same size"
}

// Plural returns "<n> <one>" or "<n> <many>".
func Plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Count returns "<n> <noun>" with a regular English plural (adds "s").
func Count(n int, noun string) string { return Plural(n, noun, noun+"s") }

// ShortID abbreviates a content digest for verbose output.
func ShortID(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// Detect decides whether decoration is on: mode is "always", "never", or
// "auto" (the default: on when w is a terminal and NO_COLOR is unset).
func Detect(mode string, w io.Writer, isTerminal func(io.Writer) bool) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if isTerminal == nil {
		isTerminal = IsTerminal
	}
	return isTerminal(w) && enableVirtualTerminal(w)
}

// IsTerminal reports whether w is a character device (an interactive
// terminal) without any platform-specific system call.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Size reports the terminal dimensions of w. ok is false when w is not a
// terminal, which is the caller's cue to use a fixed layout rather than
// guessing at a width.
func Size(w io.Writer) (width, height int, ok bool) {
	f, isFile := w.(*os.File)
	if !isFile {
		return 0, 0, false
	}
	width, height, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}
