package sdk

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ProgressStatus is the lifecycle state of an operation or one of its items.
type ProgressStatus string

const (
	ProgressPending   ProgressStatus = "pending"
	ProgressActive    ProgressStatus = "active"
	ProgressCompleted ProgressStatus = "completed"
	ProgressSkipped   ProgressStatus = "skipped"
	ProgressFailed    ProgressStatus = "failed"
	ProgressCancelled ProgressStatus = "cancelled"
)

// OperationProgress is the canonical overall view of an operation. Progress
// weights the byte totals of every known phase (for example transfer plus
// write), falling back to terminal items divided by Planned when no byte plan
// exists. Transfer remains separate so consumers can explain the network part.
type OperationProgress struct {
	Status             ProgressStatus `json:"status"`
	Stage              Stage          `json:"stage,omitempty"`
	Determinate        bool           `json:"determinate"`
	Progress           float64        `json:"progress"`
	Planned            int            `json:"planned"`
	Active             int            `json:"active"`
	Completed          int            `json:"completed"`
	Skipped            int            `json:"skipped"`
	Failed             int            `json:"failed"`
	Cancelled          int            `json:"cancelled"`
	Terminal           int            `json:"terminal"`
	CompletedWorkBytes int64          `json:"completedWorkBytes"`
	TotalWorkBytes     int64          `json:"totalWorkBytes"`
	// WorkBytesPerSecond and WorkETA describe all planned work, not only
	// network transfer. They therefore remain meaningful for local operations
	// such as dehydrate verification and placeholder replacement.
	WorkBytesPerSecond        float64       `json:"workBytesPerSecond"`
	AverageWorkBytesPerSecond float64       `json:"averageWorkBytesPerSecond"`
	WorkETA                   time.Duration `json:"workETA,omitempty"`
	WorkETAKnown              bool          `json:"workETAKnown"`
	FailureReason             string        `json:"failureReason,omitempty"`
}

// TransferProgress is the canonical transfer telemetry for an operation.
// TotalBytes counts only deduplicated bytes that really need to cross the
// storage boundary. WireBytes includes retries; CompletedBytes does not.
type TransferProgress struct {
	Determinate    bool  `json:"determinate"`
	TotalBytes     int64 `json:"totalBytes"`
	CompletedBytes int64 `json:"completedBytes"`
	WireBytes      int64 `json:"wireBytes"`
	ReusedItems    int   `json:"reusedItems"`
	ReusedBytes    int64 `json:"reusedBytes"`
	// BytesPerSecond is the recent rate at which useful bytes advance toward
	// TotalBytes. WireBytesPerSecond is the physical I/O rate, which can be
	// higher when a transport retries. ETA deliberately uses the useful rate.
	BytesPerSecond            float64       `json:"bytesPerSecond"`
	WireBytesPerSecond        float64       `json:"wireBytesPerSecond"`
	AverageBytesPerSecond     float64       `json:"averageBytesPerSecond"`
	AverageWireBytesPerSecond float64       `json:"averageWireBytesPerSecond"`
	ETA                       time.Duration `json:"eta,omitempty"`
	ETAKnown                  bool          `json:"etaKnown"`
	ActiveItems               int           `json:"activeItems"`
}

// ProgressPart is one independently measurable part of an item, normally a
// manifest or content chunk.
type ProgressPart struct {
	ID        string         `json:"id"`
	Status    ProgressStatus `json:"status"`
	Index     int            `json:"index,omitempty"`
	Count     int            `json:"count,omitempty"`
	Bytes     int64          `json:"bytes,omitempty"`
	Total     int64          `json:"total,omitempty"`
	WireBytes int64          `json:"wireBytes,omitempty"`
}

// ProgressItem is the canonical state of one planned item.
type ProgressItem struct {
	Path       string         `json:"path,omitempty"`
	Digest     string         `json:"digest,omitempty"`
	Remote     string         `json:"remote,omitempty"`
	Phase      Phase          `json:"phase,omitempty"`
	Status     ProgressStatus `json:"status"`
	Bytes      int64          `json:"bytes,omitempty"`
	Total      int64          `json:"total,omitempty"`
	WireBytes  int64          `json:"wireBytes,omitempty"`
	Chunks     int            `json:"chunks,omitempty"`
	ChunksDone int            `json:"chunksDone,omitempty"`
	Message    string         `json:"message,omitempty"`
	Parts      []ProgressPart `json:"parts,omitempty"`
}

// ProgressSnapshot is an immutable, fully reduced view. Consumers render it;
// they do not need to correlate or aggregate raw Event values themselves.
type ProgressSnapshot struct {
	Time      time.Time         `json:"time"`
	Operation string            `json:"operation,omitempty"`
	Overall   OperationProgress `json:"overall"`
	Transfer  TransferProgress  `json:"transfer"`
	// Items is a bounded, most-recently-updated window. Complete per-item
	// results remain on the operation's result; keeping every planned item in
	// every byte tick would make progress delivery quadratic.
	Items []ProgressItem `json:"items,omitempty"`
}

// ProgressStreamOptions controls telemetry sampling.
type ProgressStreamOptions struct {
	RateWindow   time.Duration
	MinETASample time.Duration
	// ItemLimit bounds the live item window. Values <= 0 use 64.
	ItemLimit int
}

// ProgressStream reduces SDK events and publishes the latest canonical
// snapshot on a channel. The channel has latest-value semantics: a slow UI may
// miss intermediate byte ticks, but never loses state because every snapshot
// is complete. Call Complete exactly once when the SDK operation returns.
type ProgressStream struct {
	mu       sync.Mutex
	out      chan ProgressSnapshot
	closed   bool
	tracker  progressTracker
	complete sync.Once
}

type transferSample struct {
	at        time.Time
	completed int64
	wire      int64
}

type workSample struct {
	at        time.Time
	completed int64
}

type trackedItem struct {
	ProgressItem
	parts   map[string]*ProgressPart
	updated uint64
}

type progressTracker struct {
	now           func() time.Time
	rateWindow    time.Duration
	minETASample  time.Duration
	operation     string
	stage         Stage
	status        ProgressStatus
	planned       int
	items         map[string]*trackedItem
	order         []string
	transferSeen  map[string]int64
	wireSeen      map[string]int64
	phaseTotals   map[Phase]int64
	phaseSeen     map[Phase]map[string]int64
	transfer      TransferProgress
	samples       []transferSample
	workSamples   []workSample
	workCompleted int64
	workRate      float64
	workAverage   float64
	workETA       time.Duration
	workETAKnown  bool
	started       time.Time
	failure       string
	itemLimit     int
	sequence      uint64
}

// NewProgressStream creates a stream suitable for sdk.Options.Progress.
func NewProgressStream(opts ProgressStreamOptions) *ProgressStream {
	return newProgressStream(opts, time.Now)
}

func newProgressStream(opts ProgressStreamOptions, clock func() time.Time) *ProgressStream {
	if opts.RateWindow <= 0 {
		opts.RateWindow = 10 * time.Second
	}
	if opts.MinETASample <= 0 {
		opts.MinETASample = time.Second
	}
	if opts.ItemLimit <= 0 {
		opts.ItemLimit = 64
	}
	s := &ProgressStream{out: make(chan ProgressSnapshot, 1)}
	s.tracker = progressTracker{
		now: clock, rateWindow: opts.RateWindow, minETASample: opts.MinETASample, itemLimit: opts.ItemLimit,
		status: ProgressPending, items: map[string]*trackedItem{},
		transferSeen: map[string]int64{}, wireSeen: map[string]int64{},
		phaseTotals: map[Phase]int64{}, phaseSeen: map[Phase]map[string]int64{},
	}
	return s
}

// Snapshots returns the stream's latest-value channel.
func (s *ProgressStream) Snapshots() <-chan ProgressSnapshot { return s.out }

// Progress implements ProgressFunc.
func (s *ProgressStream) Progress(ev Event) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.tracker.apply(ev)
	snap := s.tracker.snapshot()
	s.publishLocked(snap)
	s.mu.Unlock()
}

// Snapshot returns the current state without consuming the channel.
func (s *ProgressStream) Snapshot() ProgressSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracker.snapshot()
}

// Complete publishes and closes the final snapshot. A nil error marks the
// operation completed; context cancellation is distinguished from failure.
func (s *ProgressStream) Complete(err error) {
	s.complete.Do(func() {
		s.mu.Lock()
		s.tracker.finish(err)
		snap := s.tracker.snapshot()
		s.publishLocked(snap)
		s.closed = true
		close(s.out)
		s.mu.Unlock()
	})
}

func (s *ProgressStream) publishLocked(snap ProgressSnapshot) {
	select {
	case s.out <- snap:
		return
	default:
	}
	// Replace a stale buffered snapshot. A concurrent receiver may take it
	// between the failed send and this receive, in which case the default keeps
	// the callback non-blocking and the following send publishes the new one.
	select {
	case <-s.out:
	default:
	}
	select {
	case s.out <- snap:
	default:
	}
}

func (t *progressTracker) apply(ev Event) {
	now := t.now()
	if t.started.IsZero() {
		t.started = now
	}
	if ev.Operation != "" {
		t.operation = ev.Operation
	}
	if t.status == ProgressPending {
		t.status = ProgressActive
	}
	switch ev.Kind {
	case EventPlan:
		if ev.Count > 0 {
			t.planned = ev.Count
		}
		for _, p := range ev.Items {
			it := t.item(Event{Path: p.Path, Digest: p.Digest, Remote: p.Remote})
			it.Status, it.Total = ProgressPending, p.Bytes
		}
		if ev.Phase != "" && ev.Total > 0 {
			t.phaseTotals[ev.Phase] = ev.Total
		}
		if ev.Phase == PhaseTransfer {
			t.transfer.Determinate = true
			t.transfer.TotalBytes = maxInt64(ev.Total, 0)
			t.transfer.ReusedItems = max(ev.ReusedItems, 0)
			t.transfer.ReusedBytes = maxInt64(ev.ReusedBytes, 0)
		}
	case EventStage:
		t.stage = ev.Stage
	case EventItemStarted:
		it := t.item(ev)
		it.Status, it.Phase = ProgressActive, ev.Phase
		it.Bytes, it.Total = ev.Bytes, ev.Total
		t.touch(it)
	case EventItemProgress:
		it := t.item(ev)
		it.Status, it.Phase = ProgressActive, ev.Phase
		it.Bytes, it.Total, it.WireBytes = ev.Bytes, ev.Total, ev.WireBytes
		it.Chunks, it.ChunksDone = ev.Chunks, ev.ChunksDone
		t.applyPart(it, ev)
		t.touch(it)
		t.updateWork(now, ev)
		if ev.Phase == PhaseTransfer {
			t.updateTransfer(now, progressKey(ev), ev.Bytes, ev.WireBytes)
		}
	case EventItemFinished:
		it := t.item(ev)
		it.Status, it.Phase = ProgressCompleted, ev.Phase
		it.Bytes, it.Total, it.WireBytes = ev.Bytes, ev.Total, ev.WireBytes
		t.applyPart(it, ev)
		t.touch(it)
		t.updateWork(now, ev)
		if ev.Phase == PhaseTransfer {
			t.updateTransfer(now, progressKey(ev), ev.Bytes, ev.WireBytes)
		}
	case EventItemSkipped:
		it := t.item(ev)
		it.Status, it.Message = ProgressSkipped, ev.Message
		t.touch(it)
	case EventItemFailed:
		it := t.item(ev)
		it.Status, it.Message = ProgressFailed, ev.Message
		t.touch(it)
	case EventItemCancelled:
		it := t.item(ev)
		it.Status, it.Message = ProgressCancelled, ev.Message
		t.touch(it)
	case EventOperationFinished:
		// The SDK method may still return an error after this event. Complete is
		// the authoritative terminal signal; keep the stream active until then.
	}
}

func (t *progressTracker) updateWork(now time.Time, ev Event) {
	if ev.Phase == "" || t.phaseTotals[ev.Phase] <= 0 {
		return
	}
	seen := t.phaseSeen[ev.Phase]
	if seen == nil {
		seen = map[string]int64{}
		t.phaseSeen[ev.Phase] = seen
	}
	value := maxInt64(ev.Bytes, 0)
	// Useful progress can legitimately move backwards when bytes from a bad
	// remote are rejected before another remote is tried. Wire progress remains
	// monotonic, but phase work must reflect the current accepted byte count.
	key := progressKey(ev)
	old := seen[key]
	seen[key] = value
	t.workCompleted += value - old
	if t.workCompleted < 0 {
		t.workCompleted = 0
	}
	t.workSamples = append(t.workSamples, workSample{at: now, completed: t.workCompleted})
	t.refreshWorkRate(now)
}

func (t *progressTracker) totalWork() int64 {
	var total int64
	for _, phaseTotal := range t.phaseTotals {
		total += phaseTotal
	}
	return total
}

func (t *progressTracker) refreshWorkRate(now time.Time) {
	cut := now.Add(-t.rateWindow)
	first := 0
	for first+1 < len(t.workSamples) && t.workSamples[first+1].at.Before(cut) {
		first++
	}
	if first > 0 {
		t.workSamples = append([]workSample(nil), t.workSamples[first:]...)
	}
	t.workRate = 0
	t.workETAKnown = false
	if len(t.workSamples) >= 2 {
		a, b := t.workSamples[0], t.workSamples[len(t.workSamples)-1]
		elapsed := b.at.Sub(a.at)
		if elapsed > 0 {
			t.workRate = float64(b.completed-a.completed) / elapsed.Seconds()
			if t.workRate < 0 {
				t.workRate = 0
			}
			total := t.totalWork()
			if total > 0 && elapsed >= t.minETASample && t.workRate > 0 {
				remaining := total - t.workCompleted
				if remaining < 0 {
					remaining = 0
				}
				t.workETA = time.Duration(float64(time.Second) * float64(remaining) / t.workRate)
				t.workETAKnown = true
			}
		}
	}
	if elapsed := now.Sub(t.started); elapsed > 0 {
		t.workAverage = float64(t.workCompleted) / elapsed.Seconds()
	}
}

func (t *progressTracker) touch(it *trackedItem) {
	t.sequence++
	it.updated = t.sequence
}

func (t *progressTracker) applyPart(it *trackedItem, ev Event) {
	if ev.PartID == "" {
		return
	}
	p := it.parts[ev.PartID]
	if p == nil {
		p = &ProgressPart{ID: ev.PartID}
		it.parts[ev.PartID] = p
	}
	p.Status, p.Index, p.Count = ProgressActive, ev.PartIndex, ev.PartCount
	p.Bytes, p.Total, p.WireBytes = ev.PartBytes, ev.PartTotal, ev.PartWireBytes
	if p.Total > 0 && p.Bytes >= p.Total {
		p.Status = ProgressCompleted
	}
}

func (t *progressTracker) updateTransfer(now time.Time, key string, completed, wire int64) {
	completed = maxInt64(completed, 0)
	old := t.transferSeen[key]
	t.transfer.CompletedBytes += completed - old
	if t.transfer.CompletedBytes < 0 {
		t.transfer.CompletedBytes = 0
	}
	t.transferSeen[key] = completed

	oldWire := t.wireSeen[key]
	if wire <= 0 {
		wire = oldWire
		if completed > old {
			wire += completed - old
		}
	}
	if wire > oldWire {
		t.transfer.WireBytes += wire - oldWire
		t.wireSeen[key] = wire
	}
	t.samples = append(t.samples, transferSample{at: now, completed: t.transfer.CompletedBytes, wire: t.transfer.WireBytes})
	t.refreshRate(now)
}

func (t *progressTracker) refreshRate(now time.Time) {
	cut := now.Add(-t.rateWindow)
	first := 0
	for first+1 < len(t.samples) && t.samples[first+1].at.Before(cut) {
		first++
	}
	if first > 0 {
		t.samples = append([]transferSample(nil), t.samples[first:]...)
	}
	if len(t.samples) < 2 {
		t.transfer.BytesPerSecond = 0
		t.transfer.WireBytesPerSecond = 0
		t.transfer.ETAKnown = false
		return
	}
	a, b := t.samples[0], t.samples[len(t.samples)-1]
	elapsed := b.at.Sub(a.at)
	if elapsed <= 0 {
		return
	}
	t.transfer.BytesPerSecond = float64(b.completed-a.completed) / elapsed.Seconds()
	t.transfer.WireBytesPerSecond = float64(b.wire-a.wire) / elapsed.Seconds()
	totalElapsed := now.Sub(t.started)
	if totalElapsed > 0 {
		t.transfer.AverageBytesPerSecond = float64(t.transfer.CompletedBytes) / totalElapsed.Seconds()
		t.transfer.AverageWireBytesPerSecond = float64(t.transfer.WireBytes) / totalElapsed.Seconds()
	}
	t.transfer.ETAKnown = false
	if t.transfer.Determinate && elapsed >= t.minETASample && t.transfer.BytesPerSecond > 0 {
		remaining := t.transfer.TotalBytes - t.transfer.CompletedBytes
		if remaining < 0 {
			remaining = 0
		}
		t.transfer.ETA = time.Duration(float64(time.Second) * float64(remaining) / t.transfer.BytesPerSecond)
		t.transfer.ETAKnown = true
	}
}

func (t *progressTracker) finish(err error) {
	switch {
	case err == nil:
		t.status = ProgressCompleted
	case errors.Is(err, context.Canceled):
		t.status = ProgressCancelled
		t.failure = err.Error()
	default:
		t.status = ProgressFailed
		t.failure = err.Error()
	}
}

func (t *progressTracker) item(ev Event) *trackedItem {
	key := progressKey(ev)
	if it := t.items[key]; it != nil {
		return it
	}
	it := &trackedItem{ProgressItem: ProgressItem{
		Path: ev.Path, Digest: ev.Digest, Remote: ev.Remote, Status: ProgressPending,
	}, parts: map[string]*ProgressPart{}}
	t.items[key] = it
	t.order = append(t.order, key)
	return it
}

func progressKey(ev Event) string {
	if ev.Path != "" {
		return ev.Path + "\x00" + ev.Remote
	}
	return ev.Digest + "\x00" + ev.Remote
}

func (t *progressTracker) snapshot() ProgressSnapshot {
	t.refreshRate(t.now())
	t.refreshWorkRate(t.now())
	overall := OperationProgress{Status: t.status, Stage: t.stage, Planned: t.planned, FailureReason: t.failure}
	candidates := make([]*trackedItem, 0, min(len(t.items), t.itemLimit*2))
	for _, key := range t.order {
		tracked := t.items[key]
		switch tracked.Status {
		case ProgressActive:
			overall.Active++
		case ProgressCompleted:
			overall.Completed++
		case ProgressSkipped:
			overall.Skipped++
		case ProgressFailed:
			overall.Failed++
		case ProgressCancelled:
			overall.Cancelled++
		}
		if tracked.Status != ProgressPending {
			candidates = append(candidates, tracked)
		}
	}
	overall.Terminal = overall.Completed + overall.Skipped + overall.Failed + overall.Cancelled
	overall.TotalWorkBytes = t.totalWork()
	overall.CompletedWorkBytes = t.workCompleted
	if overall.CompletedWorkBytes > overall.TotalWorkBytes {
		overall.CompletedWorkBytes = overall.TotalWorkBytes
	}
	overall.WorkBytesPerSecond = t.workRate
	overall.AverageWorkBytesPerSecond = t.workAverage
	overall.WorkETA = t.workETA
	overall.WorkETAKnown = t.workETAKnown
	overall.Determinate = overall.TotalWorkBytes > 0 || overall.Planned > 0
	switch {
	case overall.Status == ProgressCompleted:
		overall.Progress = 100
	case overall.Planned > 0 && overall.Terminal >= overall.Planned:
		overall.Progress = 100
	case overall.TotalWorkBytes > 0:
		overall.Progress = float64(overall.CompletedWorkBytes) / float64(overall.TotalWorkBytes) * 100
	case overall.Planned > 0:
		overall.Progress = float64(overall.Terminal) / float64(overall.Planned) * 100
	}
	if overall.Progress > 100 {
		overall.Progress = 100
	}
	transfer := t.transfer
	for _, tracked := range t.items {
		if tracked.Status == ProgressActive && tracked.Phase == PhaseTransfer {
			transfer.ActiveItems++
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].updated > candidates[j].updated })
	if len(candidates) > t.itemLimit {
		candidates = candidates[:t.itemLimit]
	}
	items := make([]ProgressItem, 0, len(candidates))
	for _, tracked := range candidates {
		it := tracked.ProgressItem
		for _, p := range tracked.parts {
			it.Parts = append(it.Parts, *p)
		}
		sort.Slice(it.Parts, func(i, j int) bool { return it.Parts[i].ID < it.Parts[j].ID })
		items = append(items, it)
	}
	return ProgressSnapshot{Time: t.now(), Operation: t.operation, Overall: overall, Transfer: transfer, Items: items}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
