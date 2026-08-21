// Package live is the interactive progress view: a bounded, in-place display
// of an operation that touches many files.
//
// It exists because the obvious rendering — one line per item, redrawn — stops
// working at scale. A repository load can select hundreds of assets, and a
// region taller than the terminal cannot be redrawn in place: it scrolls, so
// the operation appears to emit an endless waterfall of "pending" rows and the
// one thing the reader wants, how far along it is, is never on screen.
//
// The view is therefore bounded by construction: a summary line, a bar, and at
// most a handful of in-flight rows, never more than the terminal can hold.
// Finished items leave the region and become numbers in the summary; failures
// are printed above it, where they stay.
//
// # Why not a TUI framework
//
// The obvious answer is Bubble Tea, and the design here is deliberately its
// shape: messages in, immutable state, a pure View. What it does not take is
// the runtime, because importing it costs more than it gives us:
//
//   - bubbletea's package init calls lipgloss.HasDarkBackground(), which asks
//     the terminal for its background colour (OSC 11) and blocks for
//     termenv.OSCTimeout — five seconds — when nothing answers. That is paid
//     by *every* invocation of the binary, including `fxvcs filter-process`,
//     which Git spawns on every `git add` of a managed file.
//   - The query reads from /dev/tty directly, so pipes on stdin and stdout do
//     not shield a subprocess from it, and a reply that never comes is
//     indistinguishable from a keystroke it consumed.
//
// Lip Gloss on its own has neither problem as long as the colour profile is
// pinned instead of detected, which the CLI can do because it already decided
// about colour. So the styling and width handling are Lip Gloss's, and the
// hundred lines of cursor arithmetic below are ours.
package live

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Layout constants. The live region never exceeds
// summaryLines + maxActiveRows + 1 overflow line.
const (
	summaryLines  = 2 // summary + bar
	maxActiveRows = 24
	minActiveRows = 1
	// reservedLines keeps the shell prompt and the command's own output from
	// being overwritten on a short terminal.
	reservedLines = 4
	defaultWidth  = 80
	defaultHeight = 24
)

// spinnerFrames is the in-flight indicator; frames advance with wall clock so
// the model stays a pure function of its messages.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const spinnerRate = 80 * time.Millisecond

// Item is one unit of work in the operation.
type Item struct {
	// Key identifies the item across messages.
	Key string
	// Label is what the reader recognises: a repository path, or an object
	// digest when nothing refers to it.
	Label string
	// Note is secondary context, e.g. the destination storage remote.
	Note string
	// Bytes and Total track transfer for this item; Total 0 means unknown.
	Bytes, Total int64
	// Transfer marks an item whose data is crossing the network, as opposed
	// to being written into the working tree. Only transfer moves the
	// operation's byte total, and only transfer draws a per-item bar.
	Transfer bool
	// Chunks and ChunksDone describe an item transferred in pieces.
	Chunks, ChunksDone int
}

// Msg is one state change. The adapter translates SDK events into these.
type Msg any

// Message types.
type (
	// PlanMsg announces the whole operation before any item starts. Count or
	// TotalBytes may be zero when the operation cannot know them up front.
	PlanMsg struct {
		Verb       string
		Count      int
		TotalBytes int64
		// ReusedItems and ReusedBytes are the assets that need no transfer
		// because their content is already stored or is shared with another
		// asset in the same operation.
		ReusedItems int
		ReusedBytes int64
	}
	// SizeMsg reports the terminal dimensions.
	SizeMsg struct{ Width, Height int }
	// StageMsg announces which part of the operation is running. Index/Count
	// position it when it can count itself.
	StageMsg struct {
		Name         string
		Index, Count int
	}
	// StartMsg marks an item in flight.
	StartMsg struct{ Item Item }
	// ProgressMsg updates an in-flight item's transferred bytes.
	ProgressMsg struct{ Item Item }
	// FinishMsg retires an item successfully.
	FinishMsg struct{ Item Item }
	// FailMsg retires an item with a reason.
	FailMsg struct {
		Item   Item
		Reason string
	}
	// FetchedMsg reports transfer progress for one item, which for a load
	// happens before the file itself is produced. It moves that item's bar
	// and the operation's byte total, but never the item count: data landing
	// is not the same as a file existing.
	FetchedMsg struct{ Item Item }
	// SnapshotMsg replaces locally accumulated progress with the SDK's
	// canonical snapshot. Interactive CLI rendering uses this path so overall
	// progress, transfer totals, rate, and ETA have exactly the same semantics
	// as every other SDK consumer.
	SnapshotMsg struct {
		Verb        string
		Stage       string
		Planned     int
		Active      int
		Completed   int
		Skipped     int
		Failed      int
		Cancelled   int
		Determinate bool
		Progress    float64
		WorkBytes   int64
		WorkTotal   int64
		Transfer    TransferSnapshot
		Items       []SnapshotItem
	}
	TransferSnapshot struct {
		Determinate    bool
		Bytes          int64
		Total          int64
		WireBytes      int64
		BytesPerSecond float64
		ETAKnown       bool
		ETA            time.Duration
		ReusedItems    int
		ReusedBytes    int64
	}
	SnapshotItem struct {
		Item
		Status  string
		Message string
	}
)

// Model is the state of one operation's display. Apply and View are pure;
// nothing here writes to a terminal.
type Model struct {
	verb           string
	width          int
	height         int
	color          bool
	styles         styles
	started        time.Time
	quit           bool
	planned        int
	pBytes         int64
	done           int
	failed         int
	skipped        int
	cancelled      int
	activeTotal    int
	doneByte       int64
	bytesFromFetch bool
	// sawChunked latches once any asset arrives in pieces. The chunk column
	// is then reserved for the rest of the operation: deciding per frame
	// makes every other row shift sideways as chunked and whole-blob assets
	// come and go, which is exactly the jitter a fixed layout is for.
	sawChunked           bool
	reusedItems          int
	reusedBytes          int64
	canonical            bool
	determinate          bool
	progressPct          float64
	workBytes, workTotal int64
	transferDeterminate  bool
	wireBytes            int64
	bytesPerSecond       float64
	etaKnown             bool
	eta                  time.Duration
	stage                string
	stageAt              int
	stageOf              int
	seen                 map[string]int64 // cumulative transferred bytes per item

	active map[string]Item
	// touched orders rows by activity, not arrival. Arrival order is the
	// wrong order: a load resolves every manifest first, so every file gets a
	// row at nearly zero progress before any of their chunks are fetched, and
	// showing the first N by arrival pins the display to files that will not
	// move again for minutes while the work happens out of sight.
	touched map[string]int64
	seq     int64
	// recent holds the labels of the last few completed items, newest first.
	// Individual items often start and finish between two repaints, so
	// without this the rows are empty and the display looks stalled even
	// while it races through hundreds of files.
	recent       []string
	terminalSeen map[string]bool

	nowFunc func() time.Time
}

type styles struct {
	summary lipgloss.Style
	bar     lipgloss.Style
	dim     lipgloss.Style
	done    lipgloss.Style
	fail    lipgloss.Style
}

// New returns a model for an operation rendering to w. color is the
// already-resolved decision from --color, NO_COLOR, and terminal detection.
//
// The colour profile is pinned rather than detected: see the package comment
// for why nothing may interrogate the terminal.
func New(w io.Writer, verb string, color bool) Model {
	profile := termenv.Ascii
	if color {
		profile = termenv.ANSI256
	}
	r := lipgloss.NewRenderer(w, termenv.WithProfile(profile))
	r.SetHasDarkBackground(true)
	return Model{
		verb:   verb,
		width:  defaultWidth,
		height: defaultHeight,
		color:  color,
		styles: styles{
			summary: r.NewStyle().Bold(true),
			bar:     r.NewStyle().Foreground(lipgloss.Color("6")),
			dim:     r.NewStyle().Faint(true),
			done:    r.NewStyle().Foreground(lipgloss.Color("2")),
			fail:    r.NewStyle().Foreground(lipgloss.Color("1")),
		},
		active:       map[string]Item{},
		touched:      map[string]int64{},
		seen:         map[string]int64{},
		terminalSeen: map[string]bool{},
		nowFunc:      time.Now,
	}
}

// WithClock injects a clock (tests).
func (m Model) WithClock(now func() time.Time) Model {
	m.nowFunc = now
	return m
}

// Apply folds one message into the model. The second result is a line to
// print *above* the live region, which is how a failure survives: the region
// itself is transient and is erased when the operation ends.
func (m Model) Apply(msg Msg) (Model, string) {
	switch msg := msg.(type) {
	case SizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}

	case StageMsg:
		m.startClock()
		m.stage, m.stageAt, m.stageOf = msg.Name, msg.Index, msg.Count

	case PlanMsg:
		if msg.Verb != "" {
			m.verb = msg.Verb
		}
		m.planned, m.pBytes = msg.Count, msg.TotalBytes
		m.reusedItems, m.reusedBytes = msg.ReusedItems, msg.ReusedBytes
		m.startClock()

	case StartMsg:
		m.startClock()
		m.track(msg.Item)

	case ProgressMsg:
		m.startClock()
		if !m.bytesFromFetch {
			m.doneByte += msg.Item.Bytes - m.seen[msg.Item.Key]
			m.seen[msg.Item.Key] = msg.Item.Bytes
		}
		if cur, ok := m.active[msg.Item.Key]; ok {
			cur.Bytes = msg.Item.Bytes
			if msg.Item.Total > 0 {
				cur.Total = msg.Item.Total
			}
			m.active[msg.Item.Key] = cur
		} else {
			// Work can be reported before its start when it runs
			// concurrently; the row must not be dropped.
			m.track(msg.Item)
		}

	case FetchedMsg:
		m.startClock()
		// Once an operation reports transfer separately, it is the authority
		// on bytes moved; counting write progress too would double the same
		// data. Totals are cumulative per item, so only the delta is added.
		m.bytesFromFetch = true
		it := msg.Item
		it.Transfer = true
		if it.Chunks > 1 {
			m.sawChunked = true
		}
		m.doneByte += it.Bytes - m.seen[it.Key]
		m.seen[it.Key] = it.Bytes
		if it.Total > 0 && it.Bytes >= it.Total {
			// Fully downloaded: it is no longer moving, and its row would
			// otherwise crowd out items that are.
			m.retire(it.Key)
			m.remember(it.Label)
		} else {
			m.track(it)
		}

	case FinishMsg:
		m.retire(msg.Item.Key)
		m.remember(msg.Item.Label)
		m.done++
		if !m.bytesFromFetch {
			b := msg.Item.Bytes
			if b == 0 {
				b = msg.Item.Total
			}
			m.doneByte += b - m.seen[msg.Item.Key]
			delete(m.seen, msg.Item.Key)
		}

	case FailMsg:
		m.retire(msg.Item.Key)
		m.failed++
		return m, m.failureLine(msg)

	case SnapshotMsg:
		m.startClock()
		m.canonical = true
		if msg.Verb != "" {
			m.verb = msg.Verb
		}
		m.stage = msg.Stage
		m.stageAt, m.stageOf = 0, 0
		m.planned, m.activeTotal, m.done = msg.Planned, msg.Active, msg.Completed
		m.skipped, m.failed, m.cancelled = msg.Skipped, msg.Failed, msg.Cancelled
		m.determinate, m.progressPct = msg.Determinate, msg.Progress
		m.workBytes, m.workTotal = msg.WorkBytes, msg.WorkTotal
		m.transferDeterminate = msg.Transfer.Determinate
		m.doneByte, m.pBytes = msg.Transfer.Bytes, msg.Transfer.Total
		m.wireBytes, m.bytesPerSecond = msg.Transfer.WireBytes, msg.Transfer.BytesPerSecond
		m.etaKnown, m.eta = msg.Transfer.ETAKnown, msg.Transfer.ETA
		m.reusedItems, m.reusedBytes = msg.Transfer.ReusedItems, msg.Transfer.ReusedBytes
		m.bytesFromFetch = msg.Transfer.Determinate || msg.Transfer.Bytes > 0 || msg.Transfer.WireBytes > 0
		m.active = map[string]Item{}
		m.touched = map[string]int64{}
		// SDK items are most-recent first; fold them backwards so track's
		// increasing sequence preserves that order.
		for i := len(msg.Items) - 1; i >= 0; i-- {
			it := msg.Items[i]
			if it.Transfer && it.Chunks > 1 {
				m.sawChunked = true
			}
			switch it.Status {
			case "active":
				m.track(it.Item)
			case "completed":
				if !m.terminalSeen[it.Key] {
					m.terminalSeen[it.Key] = true
					m.remember(it.Label)
				}
			}
		}
	}
	return m, ""
}

// Quit marks the operation finished; the view then renders nothing so the
// region can be erased.
func (m Model) Quit() Model {
	m.quit = true
	return m
}

func (m *Model) startClock() {
	if m.started.IsZero() {
		m.started = m.nowFunc()
	}
}

func (m *Model) track(it Item) {
	m.seq++
	m.touched[it.Key] = m.seq
	m.active[it.Key] = it
}

// remember records a completed label, newest first, bounded.
func (m *Model) remember(label string) {
	if label == "" {
		return
	}
	next := make([]string, 0, maxActiveRows)
	next = append(next, label)
	for _, l := range m.recent {
		if len(next) == maxActiveRows {
			break
		}
		if l != label {
			next = append(next, l)
		}
	}
	m.recent = next
}

func (m *Model) retire(key string) {
	delete(m.active, key)
	delete(m.touched, key)
}

// liveKeys returns the active keys most-recently-changed first, so the rows on
// screen are the ones actually moving.
func (m Model) liveKeys() []string {
	keys := make([]string, 0, len(m.active))
	for k := range m.active {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m.touched[keys[i]] != m.touched[keys[j]] {
			return m.touched[keys[i]] > m.touched[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// View renders the bounded live region.
func (m Model) View() string {
	if m.quit {
		return ""
	}
	var b strings.Builder
	b.WriteString(m.summary())
	b.WriteByte('\n')
	if bar := m.progressBar(); bar != "" {
		b.WriteString(bar)
		b.WriteByte('\n')
	}
	rows := m.activeRows()
	for _, r := range rows {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	if extra := m.overflow(len(rows)); extra != "" {
		b.WriteString(extra)
		b.WriteByte('\n')
	}
	return b.String()
}

// summary is the one line that must always be visible.
func (m Model) summary() string {
	parts := []string{m.verb}
	// Name the stage: the three parts of a load are bound by different things
	// and fail for different reasons, and "resolving" sitting still means
	// something quite different from "writing" sitting still.
	if m.stage != "" {
		stage := m.stage
		if m.stageOf > 0 && m.stageAt > 0 && m.stageAt < m.stageOf {
			stage += fmt.Sprintf(" %d/%d", m.stageAt, m.stageOf)
		}
		parts = append(parts, stage)
	}
	if m.canonical && m.planned > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d %s", m.done, m.planned, plural(m.planned, "asset")))
	} else if m.planned > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d %s", m.done, m.planned, plural(m.planned, "asset")))
	} else {
		parts = append(parts, fmt.Sprintf("%d %s", m.done, plural(m.done, "asset")))
	}
	switch {
	case m.canonical && m.transferDeterminate && (m.pBytes > 0 || m.doneByte > 0):
		parts = append(parts, fmt.Sprintf("%s/%s", Bytes(m.doneByte), Bytes(m.pBytes)))
	case m.canonical && !m.transferDeterminate && m.workTotal > 0:
		parts = append(parts, fmt.Sprintf("%s/%s processed", Bytes(m.workBytes), Bytes(m.workTotal)))
	case m.pBytes > 0:
		bytePart := fmt.Sprintf("%s of %s", Bytes(m.doneByte), Bytes(m.pBytes))
		if m.bytesFromFetch && m.done < m.planned {
			bytePart += " " + m.transferPastTense()
		}
		parts = append(parts, bytePart)
	case m.doneByte > 0:
		parts = append(parts, Bytes(m.doneByte))
	}
	if rate := m.rate(); rate != "" {
		parts = append(parts, rate)
	}
	if m.canonical && m.etaKnown {
		parts = append(parts, "ETA "+formatETA(m.eta))
	}
	if m.canonical && m.wireBytes > m.doneByte {
		parts = append(parts, Bytes(m.wireBytes)+" wire")
	}
	if m.reusedItems > 0 {
		// Content that never had to move is the storage model paying for
		// itself; an operation that quietly skips half its bytes should say so.
		parts = append(parts, fmt.Sprintf("%s reused", Bytes(m.reusedBytes)))
	}
	if m.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", m.failed))
	}
	if m.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", m.skipped))
	}
	if m.cancelled > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled", m.cancelled))
	}
	return m.styles.summary.Render(truncate(strings.Join(parts, " · "), m.width))
}

func (m Model) transferPastTense() string {
	if m.verb == "uploading" {
		return "uploaded"
	}
	return "downloaded"
}

// progressBar is drawn only when the denominator is known; a bar that cannot
// reach the end is worse than no bar.
func (m Model) progressBar() string {
	if m.canonical {
		if !m.determinate {
			return ""
		}
		pct := clampFloat(m.progressPct / 100)
		width := clampInt(m.width-8, 10, 60)
		filled := int(pct*float64(width) + 0.5)
		bar := m.styles.bar.Render(strings.Repeat("█", filled)) + m.styles.dim.Render(strings.Repeat("░", width-filled))
		return fmt.Sprintf("%s %3.0f%%", bar, pct*100)
	}
	if m.planned <= 0 && m.pBytes <= 0 {
		return ""
	}
	// The bar tracks items, not bytes: "how many of my files are done" is the
	// question, and a byte bar jumps unevenly when file sizes differ by two
	// orders of magnitude, which they do.
	var pct float64
	if m.planned > 0 {
		pct = float64(m.done+m.failed) / float64(m.planned)
	} else {
		pct = float64(m.doneByte) / float64(m.pBytes)
	}
	pct = clampFloat(pct)
	width := clampInt(m.width-8, 10, 60)
	filled := int(pct*float64(width) + 0.5)
	bar := m.styles.bar.Render(strings.Repeat("█", filled)) + m.styles.dim.Render(strings.Repeat("░", width-filled))
	return fmt.Sprintf("%s %3.0f%%", bar, pct*100)
}

// activeRows renders the in-flight items, bounded by the space available.
func (m Model) activeRows() []string {
	limit := m.rowBudget()
	keys := m.liveKeys()
	if len(keys) > limit {
		keys = keys[:limit]
	}
	frame := spinnerFrames[0]
	if !m.started.IsZero() {
		elapsed := m.nowFunc().Sub(m.started)
		frame = spinnerFrames[int(elapsed/spinnerRate)%len(spinnerFrames)]
	}
	// Columns are computed once for the whole frame so every bar and every
	// figure starts in the same place. Ragged rows are hard to scan, and the
	// eye is looking down the bars, not along one row.
	figures := m.figureWidth()
	labelCol := m.width - lipgloss.Width("  ") - 2 - miniBarWidth - 1 - figures
	labelCol = clampInt(labelCol, 12, m.width)

	rows := make([]string, 0, limit)
	for _, k := range keys {
		it := m.active[k]
		right := ""
		switch {
		case it.Transfer && it.Total > 0:
			// A transferring item gets its own bar, the way a container image
			// shows each layer: the reader can see which file is moving and
			// how far along it is, not just that something is.
			right = miniBar(m, float64(it.Bytes)/float64(it.Total)) + " " + m.figures(it)
		case it.Total > 0 && it.Bytes > 0:
			right = strings.Repeat(" ", miniBarWidth+1) + m.figures(it)
		case it.Total > 0:
			right = strings.Repeat(" ", miniBarWidth+1) + padLeft(Bytes(it.Total), figureCol)
		}
		label := it.Label
		if it.Note != "" {
			label += "  " + it.Note
		}
		rows = append(rows, strings.TrimRight("  "+frame+" "+padRight(truncate(label, labelCol), labelCol)+" "+right, " "))
	}
	// Fill the unused rows with what just finished, so the reader can see
	// which files are moving even when each one takes less time than a
	// repaint.
	for _, label := range m.recent {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, "  "+m.styles.done.Render(doneMark)+" "+m.styles.dim.Render(truncate(label, labelCol)))
	}
	return rows
}

// figureCol is the width of one byte figure ("999.9 KiB"), and chunkCol the
// width of the "(12/47)" annotation. Fixing them is what lets a column of
// numbers be read down the page instead of hunted for on each line.
const (
	figureCol = len("999.9 KiB")
	chunkCol  = len(" (999/999)")
)

// figures renders "  1.0 MiB / 8.0 MiB (2/12)" with the slash, and the chunk
// annotation, in the same column on every row.
func (m Model) figures(it Item) string {
	out := padLeft(Bytes(it.Bytes), figureCol) + " / " + padRight(Bytes(it.Total), figureCol)
	if m.anyChunked() {
		chunks := ""
		if it.Chunks > 1 {
			chunks = fmt.Sprintf(" (%d/%d)", it.ChunksDone, it.Chunks)
		}
		out += padRight(chunks, chunkCol)
	}
	return out
}

// figureWidth is the fixed width of the right-hand column, so nothing shifts
// as the numbers grow.
func (m Model) figureWidth() int {
	w := miniBarWidth + 1 + figureCol + len(" / ") + figureCol
	if m.anyChunked() {
		w += chunkCol
	}
	return w
}

func (m Model) anyChunked() bool { return m.sawChunked }

func padRight(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func padLeft(s string, width int) string {
	if gap := width - lipgloss.Width(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// miniBarWidth is the per-item bar; wide enough to read at a glance, narrow
// enough to leave the path readable on an 80-column terminal.
const miniBarWidth = 12

func miniBar(m Model, pct float64) string {
	pct = clampFloat(pct)
	// Round rather than truncate so a complete transfer shows a full bar.
	filled := int(pct*miniBarWidth + 0.5)
	return m.styles.bar.Render(strings.Repeat("█", filled)) +
		m.styles.dim.Render(strings.Repeat("░", miniBarWidth-filled))
}

// doneMark is the completed indicator; it is one column wide like a spinner
// frame so rows do not shift as items move between the two groups.
const doneMark = "✓"

// overflow names the work that does not fit on screen, so the reader is never
// misled into thinking the visible rows are everything.
func (m Model) overflow(shown int) string {
	hidden := len(m.active) - shown
	if m.canonical {
		hidden = m.activeTotal - shown
		if hidden < 0 {
			hidden = 0
		}
	}
	waiting := 0
	if m.planned > 0 {
		terminal := m.done + m.failed
		active := len(m.active)
		if m.canonical {
			terminal += m.skipped + m.cancelled
			active = m.activeTotal
		}
		waiting = m.planned - terminal - active
		if waiting < 0 {
			waiting = 0
		}
	}
	var text string
	switch {
	case hidden > 0 && waiting > 0:
		text = fmt.Sprintf("  … %d more in flight, %d waiting", hidden, waiting)
	case hidden > 0:
		text = fmt.Sprintf("  … %d more in flight", hidden)
	case waiting > 0:
		text = fmt.Sprintf("  … %d waiting", waiting)
	default:
		return ""
	}
	return m.styles.dim.Render(truncate(text, m.width))
}

// rowBudget is how many in-flight rows to show: as many as the terminal can
// spare, but never more than half of it. A tall window should be used — the
// whole complaint about a six-row window on a fifty-line terminal is that it
// wastes the space that would have made the operation legible — while still
// leaving room for the command's own output and the shell prompt.
func (m Model) rowBudget() int {
	fits := m.height - summaryLines - reservedLines - 1 // -1 for the overflow line
	half := m.height / 2
	if half < fits {
		fits = half
	}
	return clampInt(fits, minActiveRows, maxActiveRows)
}

func (m Model) rate() string {
	if m.canonical {
		if m.bytesPerSecond <= 0 {
			return ""
		}
		return Bytes(int64(m.bytesPerSecond)) + "/s"
	}
	if m.started.IsZero() || m.doneByte <= 0 {
		return ""
	}
	elapsed := m.nowFunc().Sub(m.started).Seconds()
	if elapsed < 0.5 {
		return ""
	}
	return Bytes(int64(float64(m.doneByte)/elapsed)) + "/s"
}

func formatETA(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return d.String()
	}
	return d.Truncate(time.Second).String()
}

func (m Model) failureLine(msg FailMsg) string {
	line := "failed  " + msg.Item.Label
	if msg.Reason != "" {
		line += "  (" + msg.Reason + ")"
	}
	return m.styles.fail.Render(line)
}

// Counters reports what the operation achieved, for a caller that wants to
// print its own closing line.
func (m Model) Counters() (done, failed int, bytes int64) {
	return m.done, m.failed, m.doneByte
}

// ---------------------------------------------------------------------------
// formatting helpers
// ---------------------------------------------------------------------------

// Bytes formats a byte count in binary units.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	for len(runes) > 1 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

func clampFloat(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

func clampInt(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}
