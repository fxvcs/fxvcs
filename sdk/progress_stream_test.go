package sdk

import (
	"context"
	"errors"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/app"
)

func latestSnapshot(t *testing.T, stream *ProgressStream) ProgressSnapshot {
	t.Helper()
	select {
	case snap, ok := <-stream.Snapshots():
		if !ok {
			t.Fatal("snapshot channel closed before its final value")
		}
		return snap
	default:
		t.Fatal("no progress snapshot available")
		return ProgressSnapshot{}
	}
}

func TestCheckoutOutcomesCloseSkippedAndFailedPlanItems(t *testing.T) {
	var events []Event
	c := &Client{opts: Options{Progress: func(ev Event) { events = append(events, ev) }}}
	c.checkoutTerminalOutcomes("hydrate", []app.PathOutcome{
		{Path: "written", Bytes: 10},
		{Path: "already", Skipped: true, Reason: "already hydrated"},
		{Path: "refused", Skipped: true, Reason: "working file is modified", Err: app.ErrConflict},
		{Path: "failed", Skipped: true, Reason: "read failed", Err: errors.New("read failed")},
	})
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Kind != EventItemSkipped || events[1].Kind != EventItemSkipped || events[2].Kind != EventItemFailed {
		t.Fatalf("terminal kinds = %q, %q, %q", events[0].Kind, events[1].Kind, events[2].Kind)
	}
}

func TestProgressStreamCountsEveryTerminalOutcome(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Phase: PhaseWrite, Count: 3, Total: 300})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemStarted, Phase: PhaseWrite, Path: "done", Total: 100})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemFinished, Phase: PhaseWrite, Path: "done", Bytes: 100, Total: 100})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemSkipped, Path: "already", Message: "already loaded"})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemFailed, Path: "bad", Message: "unavailable"})

	snap := latestSnapshot(t, s)
	if snap.Overall.Terminal != 3 || snap.Overall.Completed != 1 || snap.Overall.Skipped != 1 || snap.Overall.Failed != 1 {
		t.Fatalf("terminal counts = %+v", snap.Overall)
	}
	if snap.Overall.Progress != 100 {
		t.Fatalf("progress = %v, want 100", snap.Overall.Progress)
	}
}

func TestProgressStreamTracksUsefulWireRateAndETASeparately(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	s := newProgressStream(ProgressStreamOptions{
		RateWindow: 10 * time.Second, MinETASample: time.Second,
	}, func() time.Time { return now })
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Phase: PhaseTransfer, Count: 1, Total: 1000, ReusedBytes: 200})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemProgress, Phase: PhaseTransfer,
		Path: "asset", Bytes: 100, Total: 1000, WireBytes: 100})
	now = now.Add(time.Second)
	s.Progress(Event{Operation: "hydrate", Kind: EventItemProgress, Phase: PhaseTransfer,
		Path: "asset", Bytes: 500, Total: 1000, WireBytes: 700})

	got := latestSnapshot(t, s).Transfer
	if got.TotalBytes != 1000 || got.CompletedBytes != 500 || got.WireBytes != 700 || got.ReusedBytes != 200 {
		t.Fatalf("transfer counters = %+v", got)
	}
	if got.BytesPerSecond != 400 || got.WireBytesPerSecond != 600 || got.AverageBytesPerSecond != 500 || got.AverageWireBytesPerSecond != 700 {
		t.Fatalf("rates = useful %v wire %v average useful %v average wire %v",
			got.BytesPerSecond, got.WireBytesPerSecond, got.AverageBytesPerSecond, got.AverageWireBytesPerSecond)
	}
	if !got.ETAKnown || got.ETA != 1250*time.Millisecond {
		t.Fatalf("ETA = %v known=%v, want 1.25s", got.ETA, got.ETAKnown)
	}
}

func TestProgressStreamTracksOverallWorkRateAndETAWithoutTransfer(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	s := newProgressStream(ProgressStreamOptions{
		RateWindow: 10 * time.Second, MinETASample: time.Second,
	}, func() time.Time { return now })
	s.Progress(Event{Operation: "dehydrate", Kind: EventPlan, Phase: PhaseVerify, Count: 1, Total: 1000})
	s.Progress(Event{Operation: "dehydrate", Kind: EventItemProgress, Phase: PhaseVerify,
		Path: "asset", Bytes: 100, Total: 1000})
	now = now.Add(time.Second)
	s.Progress(Event{Operation: "dehydrate", Kind: EventItemProgress, Phase: PhaseVerify,
		Path: "asset", Bytes: 500, Total: 1000})

	snap := latestSnapshot(t, s)
	got := snap.Overall
	if got.CompletedWorkBytes != 500 || got.TotalWorkBytes != 1000 {
		t.Fatalf("work counters = %+v", got)
	}
	if got.WorkBytesPerSecond != 400 || got.AverageWorkBytesPerSecond != 500 {
		t.Fatalf("work rates = recent %v average %v", got.WorkBytesPerSecond, got.AverageWorkBytesPerSecond)
	}
	if !got.WorkETAKnown || got.WorkETA != 1250*time.Millisecond {
		t.Fatalf("work ETA = %v known=%v, want 1.25s", got.WorkETA, got.WorkETAKnown)
	}
	if got.WorkBytesPerSecond == 0 || snap.Transfer.BytesPerSecond != 0 {
		t.Fatal("local verification throughput was incorrectly treated as network transfer")
	}
}

func TestProgressStreamRollsBackRejectedUsefulBytesButKeepsWireBytes(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{})
	s.Progress(Event{Operation: "object fetch", Kind: EventPlan, Phase: PhaseTransfer, Count: 1, Total: 1000})
	s.Progress(Event{Operation: "object fetch", Kind: EventItemProgress, Phase: PhaseTransfer,
		Digest: "sha256:asset", Bytes: 600, Total: 1000, WireBytes: 600})
	s.Progress(Event{Operation: "object fetch", Kind: EventItemProgress, Phase: PhaseTransfer,
		Digest: "sha256:asset", Bytes: 0, Total: 1000, WireBytes: 600})

	snap := latestSnapshot(t, s)
	if snap.Transfer.CompletedBytes != 0 || snap.Transfer.WireBytes != 600 {
		t.Fatalf("transfer after rejected body = %+v", snap.Transfer)
	}
	if snap.Overall.CompletedWorkBytes != 0 || snap.Overall.Progress != 0 {
		t.Fatalf("overall work retained rejected bytes: %+v", snap.Overall)
	}
}

func TestItemProgressPreservesTransferDetailsOnFinish(t *testing.T) {
	var got Event
	c := &Client{opts: Options{Progress: func(ev Event) { got = ev }}}
	c.itemProgress("object fetch")(app.ItemProgress{Digest: "sha256:asset", Phase: app.ProgressPhaseTransfer,
		Bytes: 100, Total: 100, Done: true, Chunks: 2, ChunksDone: 2, WireBytes: 120,
		PartID: "sha256:chunk", PartBytes: 60, PartTotal: 60, PartWireBytes: 80, PartIndex: 2, PartCount: 2})
	if got.Kind != EventItemFinished || got.Phase != PhaseTransfer || got.WireBytes != 120 || got.ChunksDone != 2 ||
		got.PartID != "sha256:chunk" || got.PartWireBytes != 80 || got.PartIndex != 2 {
		t.Fatalf("finished event lost transfer details: %+v", got)
	}
}

func TestProgressStreamWeightsKnownPhaseWorkWithoutDesktopAveraging(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Phase: PhaseWrite, Count: 10, Total: 1000})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Phase: PhaseTransfer, Count: 10, Total: 1000})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemProgress, Phase: PhaseTransfer, Path: "asset", Bytes: 600, Total: 1000})
	if got := latestSnapshot(t, s).Overall.Progress; got != 30 {
		t.Fatalf("transfer-only overall progress = %v, want 30", got)
	}
	s.Progress(Event{Operation: "hydrate", Kind: EventItemProgress, Phase: PhaseWrite, Path: "asset", Bytes: 900, Total: 1000})
	snap := latestSnapshot(t, s)
	if snap.Overall.Progress != 75 || snap.Overall.CompletedWorkBytes != 1500 || snap.Overall.TotalWorkBytes != 2000 {
		t.Fatalf("phase-weighted overall progress = %+v", snap.Overall)
	}
}

func TestProgressStreamTracksItemParts(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Phase: PhaseTransfer, Count: 1, Total: 100})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemProgress, Phase: PhaseTransfer,
		Path: "asset", Bytes: 40, Total: 100, WireBytes: 50,
		PartID: "sha256:chunk", PartBytes: 40, PartTotal: 40, PartWireBytes: 50, PartIndex: 1, PartCount: 2})

	snap := latestSnapshot(t, s)
	if len(snap.Items) != 1 || len(snap.Items[0].Parts) != 1 {
		t.Fatalf("items = %+v", snap.Items)
	}
	part := snap.Items[0].Parts[0]
	if part.ID != "sha256:chunk" || part.Status != ProgressCompleted || part.Bytes != 40 || part.WireBytes != 50 || part.Index != 1 || part.Count != 2 {
		t.Fatalf("part = %+v", part)
	}
}

func TestProgressStreamBoundsItemsToMostRecentlyUpdated(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{ItemLimit: 3})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Count: 10})
	for i := 0; i < 10; i++ {
		s.Progress(Event{Operation: "hydrate", Kind: EventItemStarted, Path: string(rune('a' + i))})
	}
	snap := latestSnapshot(t, s)
	if len(snap.Items) != 3 || snap.Items[0].Path != "j" || snap.Items[2].Path != "h" {
		t.Fatalf("recent item window = %+v", snap.Items)
	}
	if snap.Overall.Active != 10 {
		t.Fatalf("active count = %d, want all 10 despite bounded items", snap.Overall.Active)
	}
}

func TestProgressStreamPublishesFinalSnapshotAndCloses(t *testing.T) {
	s := NewProgressStream(ProgressStreamOptions{})
	s.Progress(Event{Operation: "hydrate", Kind: EventPlan, Count: 1})
	s.Progress(Event{Operation: "hydrate", Kind: EventItemCancelled, Path: "asset"})
	s.Complete(context.Canceled)

	final := latestSnapshot(t, s)
	if final.Overall.Status != ProgressCancelled {
		t.Fatalf("status = %q", final.Overall.Status)
	}
	if _, ok := <-s.Snapshots(); ok {
		t.Fatal("snapshot channel remained open after final snapshot")
	}
	// Idempotent and inert after completion.
	s.Complete(nil)
	s.Progress(Event{Kind: EventItemFinished, Path: "late"})
}
