package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/internal/cli/ui/live"
	"fxvcs.dev/fxvcs/sdk"
)

// progressView renders SDK progress events.
//
// There are two renderings, and the difference is not cosmetic:
//
//   - On a terminal, a bounded live region (internal/cli/ui/live) shows the
//     summary, a bar, and the handful of items currently in flight, repainted
//     in place. Its height is bounded by the model, which is what the previous
//     one-row-per-item table got wrong: a region taller than the screen cannot
//     be repainted and scrolls forever.
//   - Off a terminal, one plain line per finished or failed item. This is what
//     CI logs, pipes, and `git push` hook output get, and it must stay
//     append-only and free of control sequences.
//
// --quiet and --json create no view at all.
type progressView struct {
	w     io.Writer
	tty   bool
	color bool

	mu    sync.Mutex
	verb  string
	stage string
	// Line mode state.
	rows  map[string]*progressRow
	order []string
	// Live mode state.
	model    live.Model
	renderer *live.Renderer
	started  bool
	stream   *sdk.ProgressStream
	liveDone chan struct{}
	failWake chan struct{}
	failures []sdk.Event
	lastSize time.Time
	sizeW    int
	sizeH    int
}

// sizePoll is how often the terminal is re-measured. There is no portable
// resize signal (SIGWINCH does not exist on Windows), and an ioctl four times
// a second costs nothing next to the redraw it accompanies.
const sizePoll = 250 * time.Millisecond

type progressRow struct {
	path   string
	remote string
	total  int64
}

func newProgressView(w io.Writer, tty, color bool) *progressView {
	v := &progressView{w: w, tty: tty, color: color, rows: map[string]*progressRow{}}
	if tty {
		v.stream = sdk.NewProgressStream(sdk.ProgressStreamOptions{})
		v.liveDone = make(chan struct{})
		v.failWake = make(chan struct{}, 1)
		go v.runLive()
	}
	return v
}

func verbFor(op string) string {
	switch op {
	case "objects publish":
		return "uploading"
	case "object fetch":
		return "downloading"
	case "hydrate":
		return "loading"
	case "dehydrate":
		return "unloading"
	case "discard":
		return "restoring"
	case "migrate":
		return "moving"
	case "cache verify":
		return "checking"
	}
	return op
}

func pastTense(verb string) string {
	switch verb {
	case "uploading":
		return "uploaded"
	case "loading":
		return "loaded"
	case "unloading":
		return "unloaded"
	case "restoring":
		return "restored"
	case "moving":
		return "moved"
	case "checking":
		return "checked"
	}
	return verb
}

// rowKey identifies an item across its events.
func rowKey(ev sdk.Event) string {
	if ev.Path != "" {
		return ev.Path + "\x00" + ev.Remote
	}
	return ev.Digest + "\x00" + ev.Remote
}

// itemOf projects an event onto a live item.
func itemOf(ev sdk.Event) live.Item {
	label := ev.Path
	if label == "" {
		label = ui.ShortID(ev.Digest)
	}
	note := ""
	if ev.Remote != "" {
		note = "→ storage " + ev.Remote
	}
	return live.Item{Key: rowKey(ev), Label: label, Note: note, Bytes: ev.Bytes, Total: ev.Total,
		Chunks: ev.Chunks, ChunksDone: ev.ChunksDone}
}

// handle is the sdk.ProgressFunc. Delivery is serialized by the SDK, but it
// may arrive from worker goroutines, so it must be cheap: in live mode it
// only forwards a message, and Bubble Tea does the rendering on its own
// schedule.
func (v *progressView) handle(ev sdk.Event) {
	v.mu.Lock()
	if v.verb == "" && ev.Operation != "" {
		v.verb = verbFor(ev.Operation)
	}
	v.mu.Unlock()

	if v.tty {
		v.stream.Progress(ev)
		if ev.Kind == sdk.EventItemFailed {
			v.mu.Lock()
			v.failures = append(v.failures, ev)
			v.mu.Unlock()
			select {
			case v.failWake <- struct{}{}:
			default:
			}
		}
		return
	}
	v.handleLines(ev)
}

func (v *progressView) runLive() {
	defer close(v.liveDone)
	ticker := time.NewTicker(live.RedrawInterval)
	defer ticker.Stop()
	var latest sdk.ProgressSnapshot
	have := false
	for {
		select {
		case snap, ok := <-v.stream.Snapshots():
			if !ok {
				v.flushFailures()
				return
			}
			force := !have || snap.Overall.Stage != latest.Overall.Stage ||
				snap.Overall.Planned != latest.Overall.Planned || snap.Overall.Determinate != latest.Overall.Determinate
			latest, have = snap, true
			v.flushFailures()
			if snapshotVisible(snap) {
				v.apply(snapshotMessage(snap, v.verbNow()), force)
			}
		case <-ticker.C:
			if have && snapshotVisible(latest) {
				latest = v.stream.Snapshot()
				v.apply(snapshotMessage(latest, v.verbNow()), true)
			}
		case <-v.failWake:
			v.flushFailures()
		}
	}
}

func snapshotVisible(s sdk.ProgressSnapshot) bool {
	return s.Overall.Stage != "" || s.Overall.Planned > 0 || s.Overall.Determinate || len(s.Items) > 0
}

func snapshotMessage(s sdk.ProgressSnapshot, verb string) live.SnapshotMsg {
	msg := live.SnapshotMsg{
		Verb: verb, Stage: string(s.Overall.Stage), Planned: s.Overall.Planned,
		Active: s.Overall.Active, Completed: s.Overall.Completed, Skipped: s.Overall.Skipped,
		Failed: s.Overall.Failed, Cancelled: s.Overall.Cancelled,
		Determinate: s.Overall.Determinate, Progress: s.Overall.Progress,
		WorkBytes: s.Overall.CompletedWorkBytes, WorkTotal: s.Overall.TotalWorkBytes,
		Transfer: live.TransferSnapshot{
			Determinate: s.Transfer.Determinate, Bytes: s.Transfer.CompletedBytes, Total: s.Transfer.TotalBytes,
			WireBytes: s.Transfer.WireBytes, BytesPerSecond: s.Transfer.BytesPerSecond,
			ETAKnown: s.Transfer.ETAKnown, ETA: s.Transfer.ETA,
			ReusedItems: s.Transfer.ReusedItems, ReusedBytes: s.Transfer.ReusedBytes,
		},
	}
	for _, it := range s.Items {
		label := it.Path
		if label == "" {
			label = ui.ShortID(it.Digest)
		}
		note := ""
		if it.Remote != "" {
			note = "→ storage " + it.Remote
		}
		key := it.Path + "\x00" + it.Remote
		if it.Path == "" {
			key = it.Digest + "\x00" + it.Remote
		}
		msg.Items = append(msg.Items, live.SnapshotItem{Item: live.Item{
			Key: key, Label: label, Note: note, Bytes: it.Bytes, Total: it.Total,
			Transfer: it.Phase == sdk.PhaseTransfer, Chunks: it.Chunks, ChunksDone: it.ChunksDone,
		}, Status: string(it.Status), Message: it.Message})
	}
	return msg
}

func (v *progressView) flushFailures() {
	v.mu.Lock()
	if len(v.failures) == 0 {
		v.mu.Unlock()
		return
	}
	failures := append([]sdk.Event(nil), v.failures...)
	v.failures = nil
	v.ensureStarted()
	r := v.renderer
	v.mu.Unlock()
	for _, ev := range failures {
		it := itemOf(ev)
		line := "failed  " + it.Label
		if reason := humanize(ev.Message); reason != "" {
			line += "  (" + reason + ")"
		}
		r.Print(line)
	}
}

// apply folds a message into the model and repaints. The renderer throttles
// itself, so this stays cheap even when events arrive in a flood.
func (v *progressView) apply(msg live.Msg, force bool) {
	v.mu.Lock()
	v.ensureStarted()
	v.pollSize()
	model, above := v.model.Apply(msg)
	v.model = model
	view := model.View()
	r := v.renderer
	v.mu.Unlock()

	if above != "" {
		r.Print(above)
	}
	r.Draw(view, force)
}

func (v *progressView) verbNow() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.verb
}

// ensureStarted builds the model and renderer on first use, so a command that
// reports no progress never writes a control sequence. Callers hold mu.
func (v *progressView) ensureStarted() {
	if v.started {
		return
	}
	v.started = true
	v.model = live.New(v.w, v.verb, v.color)
	v.renderer = live.NewRenderer(v.w)
	v.lastSize = time.Time{}
	v.pollSize()
}

// pollSize re-measures the terminal so the region grows and shrinks with the
// window. Callers hold mu.
func (v *progressView) pollSize() {
	if time.Since(v.lastSize) < sizePoll {
		return
	}
	v.lastSize = time.Now()
	w, h, ok := ui.Size(v.w)
	if !ok || (w == v.sizeW && h == v.sizeH) {
		return
	}
	v.sizeW, v.sizeH = w, h
	v.model, _ = v.model.Apply(live.SizeMsg{Width: w, Height: h})
}

// ---------------------------------------------------------------------------
// line mode (not a terminal)
// ---------------------------------------------------------------------------

func (v *progressView) handleLines(ev sdk.Event) {
	switch ev.Kind {
	case sdk.EventStage:
		// One line per stage change, never per tick: this is a log, and a
		// reader scrolling it wants to know which part was slow.
		v.mu.Lock()
		changed := v.stage != string(ev.Stage)
		v.stage = string(ev.Stage)
		v.mu.Unlock()
		if changed {
			fmt.Fprintln(v.w, string(ev.Stage)+"…")
		}
	case sdk.EventPlan:
		v.mu.Lock()
		for i := range ev.Items {
			it := ev.Items[i]
			key := it.Path + "\x00" + it.Remote
			if it.Path == "" {
				key = it.Digest + "\x00" + it.Remote
			}
			if _, ok := v.rows[key]; !ok {
				v.rows[key] = &progressRow{path: it.Path, remote: it.Remote, total: it.Bytes}
				v.order = append(v.order, key)
			}
		}
		v.mu.Unlock()
	case sdk.EventItemStarted, sdk.EventItemProgress:
		v.mu.Lock()
		v.rowFor(ev)
		v.mu.Unlock()
	case sdk.EventItemFinished:
		if ev.Operation == "migrate" && ev.Message != "" {
			return
		}
		v.mu.Lock()
		r := v.rowFor(ev)
		if ev.Total > 0 {
			r.total = ev.Total
		}
		line := v.lineFor(*r, pastTense(v.verb), "")
		v.mu.Unlock()
		fmt.Fprintln(v.w, line)
	case sdk.EventItemFailed:
		v.mu.Lock()
		r := v.rowFor(ev)
		line := v.lineFor(*r, "failed", humanize(ev.Message))
		v.mu.Unlock()
		fmt.Fprintln(v.w, line)
	}
}

// rowFor finds or creates the line-mode row for an event. Callers hold mu.
func (v *progressView) rowFor(ev sdk.Event) *progressRow {
	key := rowKey(ev)
	if r, ok := v.rows[key]; ok {
		if ev.Total > 0 {
			r.total = ev.Total
		}
		return r
	}
	label := ev.Path
	if label == "" {
		label = ui.ShortID(ev.Digest)
	}
	r := &progressRow{path: label, remote: ev.Remote, total: ev.Total}
	v.rows[key] = r
	v.order = append(v.order, key)
	return r
}

// lineFor is the append-only rendering: one line, no control sequences.
func (v *progressView) lineFor(r progressRow, label, why string) string {
	var b strings.Builder
	b.WriteString(label)
	b.WriteString("  ")
	b.WriteString(r.path)
	if r.remote != "" {
		b.WriteString("  → storage " + r.remote)
	}
	if r.total > 0 && why == "" {
		b.WriteString("  " + ui.Bytes(r.total))
	}
	if why != "" {
		b.WriteString("  (" + why + ")")
	}
	return b.String()
}

// finish erases the live region and restores the terminal, so the command's
// own summary starts on a clean line.
func (v *progressView) finish(err error) {
	if v == nil {
		return
	}
	if v.stream != nil {
		v.stream.Complete(err)
		<-v.liveDone
	}
	v.mu.Lock()
	r := v.renderer
	if v.started {
		v.model = v.model.Quit()
	}
	v.mu.Unlock()
	if r != nil {
		r.Stop()
	}
}
