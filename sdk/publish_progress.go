package sdk

import (
	"sort"
	"sync"

	"fxvcs.dev/fxvcs/internal/app"
)

// publishAggregator turns per-object upload events into per-asset progress.
//
// Publication works on stored objects, but a chunked asset is one manifest
// plus many chunks, and a consumer watching a push wants one row per file with
// its total size. The aggregator therefore collapses every object that belongs
// to the same (asset, remote) pair into a single planned item, sums transfer
// across them, and reports the asset finished only when its last object is.
// Objects that no path refers to keep their own row, keyed by digest.
type publishAggregator struct {
	// mu guards the aggregate: uploads run concurrently, so events for
	// different assets arrive from different goroutines.
	mu    sync.Mutex
	emit  func(Event)
	index map[string]int    // asset key -> 1-based plan position
	rows  map[string]*asset // asset key -> aggregate
	count int
}

type asset struct {
	path        string
	paths       []string
	firstDigest string
	remote      string
	total       int64
	objects     int
	partIndex   map[string]int
	partSize    map[string]int64
	transferred map[string]int64
	settled     map[string]bool
	started     bool
	failed      bool
}

func newPublishAggregator(emit func(Event)) *publishAggregator {
	return &publishAggregator{emit: emit, index: map[string]int{}, rows: map[string]*asset{}}
}

// assetOf names the row an upload belongs to: its first referring path, or the
// object digest when nothing refers to it.
func assetOf(it app.PublishItem) (key, path string) {
	if len(it.Paths) > 0 {
		return it.Paths[0] + "\x00" + it.Remote, it.Paths[0]
	}
	return it.Digest + "\x00" + it.Remote, ""
}

func (a *publishAggregator) handle(pe app.PublishEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key, _ := assetOf(pe.Item)
	switch pe.Kind {
	case app.PublishPlan:
		var order []string
		for _, it := range pe.Plan {
			k, p := assetOf(it)
			row, ok := a.rows[k]
			if !ok {
				row = &asset{path: p, paths: it.Paths, firstDigest: it.Digest, remote: it.Remote,
					partIndex: map[string]int{}, partSize: map[string]int64{},
					transferred: map[string]int64{}, settled: map[string]bool{}}
				a.rows[k] = row
				order = append(order, k)
			}
			row.total += it.Bytes
			row.objects++
			row.partIndex[it.Digest] = row.objects
			row.partSize[it.Digest] = it.Bytes
		}
		sort.SliceStable(order, func(i, j int) bool { return a.rows[order[i]].path < a.rows[order[j]].path })
		items := make([]PlanItem, 0, len(order))
		for i, k := range order {
			a.index[k] = i + 1
			row := a.rows[k]
			items = append(items, PlanItem{Path: row.path, Paths: row.paths, Digest: assetDigest(row), Bytes: row.total, Remote: row.remote})
		}
		a.count = len(items)
		a.emit(Event{Kind: EventPlan, Phase: PhaseTransfer, Items: items, Total: pe.TotalBytes, Count: a.count})
	case app.PublishStarted:
		row := a.rows[key]
		if row == nil || row.started {
			return
		}
		row.started = true
		a.emit(Event{Kind: EventItemStarted, Phase: PhaseTransfer, Path: row.path, Digest: assetDigest(row), Total: row.total, Remote: row.remote, Index: a.index[key], Count: a.count})
	case app.PublishProgressed:
		row := a.rows[key]
		if row == nil {
			return
		}
		row.transferred[pe.Item.Digest] = pe.Transferred
		a.emit(Event{Kind: EventItemProgress, Phase: PhaseTransfer, Path: row.path, Digest: assetDigest(row),
			Bytes: row.sum(), Total: row.total, WireBytes: row.sum(), Remote: row.remote, Index: a.index[key], Count: a.count,
			PartID: pe.Item.Digest, PartBytes: pe.Transferred, PartTotal: row.partSize[pe.Item.Digest],
			PartWireBytes: pe.Transferred, PartIndex: row.partIndex[pe.Item.Digest], PartCount: row.objects})
	case app.PublishFinished:
		row := a.rows[key]
		if row == nil {
			return
		}
		row.transferred[pe.Item.Digest] = pe.Transferred
		row.settled[pe.Item.Digest] = true
		if len(row.settled) < row.objects || row.failed {
			return
		}
		a.emit(Event{Kind: EventItemFinished, Phase: PhaseTransfer, Path: row.path, Digest: assetDigest(row),
			Bytes: row.sum(), Total: row.total, WireBytes: row.sum(), Remote: row.remote, Index: a.index[key], Count: a.count,
			PartID: pe.Item.Digest, PartBytes: pe.Transferred, PartTotal: row.partSize[pe.Item.Digest],
			PartWireBytes: pe.Transferred, PartIndex: row.partIndex[pe.Item.Digest], PartCount: row.objects})
	case app.PublishFailed:
		row := a.rows[key]
		if row == nil {
			return
		}
		row.settled[pe.Item.Digest] = true
		if row.failed {
			return // one failure per asset is enough to explain it
		}
		row.failed = true
		a.emit(Event{Kind: EventItemFailed, Phase: PhaseTransfer, Path: row.path, Digest: assetDigest(row), Total: row.total, Remote: row.remote, Index: a.index[key], Count: a.count, Code: CodeStorage, Message: pe.Err.Error()})
	case app.PublishDone:
		a.emit(Event{Kind: EventInfo, Bytes: pe.TotalBytes, Count: a.count, Message: "uploads complete"})
	}
}

func (r *asset) sum() int64 {
	var n int64
	for _, v := range r.transferred {
		n += v
	}
	return n
}

// assetDigest names the row's object when there is exactly one — a whole/1
// file, or an object nothing refers to. A chunked asset has no single digest
// to show, and printing one of its chunks would be misleading, so it has none.
func assetDigest(r *asset) string {
	if r.objects == 1 {
		return r.firstDigest
	}
	return ""
}
