package live

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// apply folds messages into the model.
func apply(m Model, msgs ...Msg) Model {
	for _, msg := range msgs {
		m, _ = m.Apply(msg)
	}
	return m
}

func fixed(t time.Time) func() time.Time { return func() time.Time { return t } }

func newTestModel() Model {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	m := New(io.Discard, "loading", false).WithClock(fixed(base))
	return apply(m, SizeMsg{Width: 80, Height: 24})
}

func item(key string, bytes, total int64) Item {
	return Item{Key: key, Label: key, Bytes: bytes, Total: total}
}

// TestViewIsBoundedRegardlessOfItemCount is the whole point of this package:
// the region must fit on screen whatever the operation's size, because a
// region taller than the terminal cannot be redrawn in place and scrolls.
func TestViewIsBoundedRegardlessOfItemCount(t *testing.T) {
	for _, tc := range []struct{ height, want int }{
		{60, summaryLines + maxActiveRows + 1}, // capped
		{40, summaryLines + 20 + 1},            // half the window
		{24, summaryLines + 12 + 1},
		{10, summaryLines + 3 + 1},
		{6, summaryLines + minActiveRows + 1},
		{3, summaryLines + minActiveRows + 1},
	} {
		t.Run(fmt.Sprintf("height %d", tc.height), func(t *testing.T) {
			m := New(io.Discard, "loading", false).WithClock(fixed(time.Now()))
			m = apply(m, SizeMsg{Width: 80, Height: tc.height}, PlanMsg{Verb: "loading", Count: 600, TotalBytes: 6 << 30})
			for i := 0; i < 200; i++ {
				m = apply(m, StartMsg{Item: item(fmt.Sprintf("asset%03d.ytd", i), 0, 1<<20)})
			}
			lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
			if len(lines) > tc.want {
				t.Fatalf("view is %d lines on a %d-line terminal (max %d):\n%s", len(lines), tc.height, tc.want, m.View())
			}
			for i, l := range lines {
				if width := len([]rune(l)); width > 80 {
					t.Fatalf("line %d is %d columns wide:\n%s", i, width, l)
				}
			}
		})
	}
}

// TestSummaryTracksProgress checks the one line that must always be right.
func TestSummaryTracksProgress(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 600, TotalBytes: 600 << 20})
	if got := m.View(); !strings.Contains(got, "loading · 0 of 600 assets") {
		t.Fatalf("initial summary:\n%s", got)
	}
	if !strings.Contains(m.View(), "0%") {
		t.Fatalf("bar missing at the start:\n%s", m.View())
	}
	for i := 0; i < 300; i++ {
		k := fmt.Sprintf("a%03d", i)
		m = apply(m, StartMsg{Item: item(k, 0, 1<<20)}, FinishMsg{Item: item(k, 1<<20, 1<<20)})
	}
	got := m.View()
	if !strings.Contains(got, "300 of 600 assets") {
		t.Fatalf("summary did not advance:\n%s", got)
	}
	if !strings.Contains(got, "300.0 MiB of 600.0 MiB") {
		t.Fatalf("byte totals missing:\n%s", got)
	}
	if !strings.Contains(got, "50%") {
		t.Fatalf("bar did not reach 50%%:\n%s", got)
	}
}

func TestCanonicalSnapshotDrivesOverallTransferRateAndETA(t *testing.T) {
	m := newTestModel()
	m = apply(m, SizeMsg{Width: 160, Height: 24}, SnapshotMsg{
		Verb: "loading", Stage: "downloading", Planned: 10, Completed: 0,
		Determinate: true, Progress: 60, WorkBytes: 600, WorkTotal: 1000,
		Transfer: TransferSnapshot{
			Determinate: true, Bytes: 600, Total: 1000, WireBytes: 700,
			BytesPerSecond: 100, ETAKnown: true, ETA: 4 * time.Second,
		},
	})
	got := m.View()
	for _, want := range []string{"60%", "0/10 assets", "600 B/1000 B", "100 B/s", "700 B wire", "ETA 4s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("canonical %q missing:\n%s", want, got)
		}
	}
	if strings.Contains(got, "  0%") {
		t.Fatalf("bar fell back to completed-item percentage:\n%s", got)
	}
}

func TestCanonicalSnapshotKeepsHiddenActiveAndWaitingCountsExact(t *testing.T) {
	msg := SnapshotMsg{Verb: "loading", Planned: 100, Active: 80, Determinate: true, Progress: 10}
	for i := 0; i < 64; i++ {
		key := fmt.Sprintf("active%02d", i)
		msg.Items = append(msg.Items, SnapshotItem{Item: item(key, 1, 10), Status: "active"})
	}
	m := apply(newTestModel(), msg)
	got := m.View()
	if !strings.Contains(got, "68 more in flight, 20 waiting") {
		t.Fatalf("bounded SDK item window distorted operation counts:\n%s", got)
	}
}

// TestOverflowNamesWhatIsNotShown: a bounded view must not imply that the rows
// on screen are all the work there is.
func TestOverflowNamesWhatIsNotShown(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 100})
	for i := 0; i < 20; i++ {
		m = apply(m, StartMsg{Item: item(fmt.Sprintf("a%02d", i), 0, 1<<20)})
	}
	got := m.View()
	if !strings.Contains(got, "more in flight") {
		t.Fatalf("hidden in-flight rows not reported:\n%s", got)
	}
	if !strings.Contains(got, "waiting") {
		t.Fatalf("queued work not reported:\n%s", got)
	}
	// With everything visible there is nothing to disclaim.
	m2 := newTestModel()
	m2 = apply(m2, PlanMsg{Verb: "loading", Count: 2},
		StartMsg{Item: item("a", 0, 10)}, StartMsg{Item: item("b", 0, 10)})
	for _, unwanted := range []string{"more in flight", "waiting"} {
		if strings.Contains(m2.View(), unwanted) {
			t.Fatalf("overflow line shown when everything fits:\n%s", m2.View())
		}
	}
}

// TestFailuresArePrintedAboveTheRegion: a failure must survive; the live
// region is transient and gets cleared.
func TestFailuresArePrintedAboveTheRegion(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 3}, StartMsg{Item: item("bad.ytd", 0, 10)})
	after, above := m.Apply(FailMsg{Item: item("bad.ytd", 0, 10), Reason: "storage is unreachable"})
	if above == "" {
		t.Fatal("a failure must produce a line to print above the region")
	}
	if !strings.Contains(above, "bad.ytd") || !strings.Contains(above, "storage is unreachable") {
		t.Fatalf("printed line lacks the path or the reason: %q", above)
	}
	if !strings.Contains(after.View(), "1 failed") {
		t.Fatalf("failure not counted in the summary:\n%s", after.View())
	}
	if strings.Contains(after.View(), "bad.ytd") {
		t.Fatal("a failed item must leave the in-flight rows")
	}
}

// TestQuitClearsTheRegion: after the operation the command prints its own
// result, which must not collide with leftover progress.
func TestQuitClearsTheRegion(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 2}, StartMsg{Item: item("a", 0, 10)})
	if m.View() == "" {
		t.Fatal("nothing rendered while running")
	}
	m = m.Quit()
	if got := m.View(); got != "" {
		t.Fatalf("region not cleared on quit: %q", got)
	}
}

// TestUnknownTotalsStillRender: an operation that cannot count its work up
// front must still show movement, without a bar that can never fill.
func TestUnknownTotalsStillRender(t *testing.T) {
	m := newTestModel()
	m = apply(m, StartMsg{Item: item("a.ytd", 0, 0)}, FinishMsg{Item: item("a.ytd", 0, 0)})
	got := m.View()
	if !strings.Contains(got, "1 asset") {
		t.Fatalf("count missing:\n%s", got)
	}
	if strings.Contains(got, "%") {
		t.Fatalf("a bar was drawn without a denominator:\n%s", got)
	}
}

// TestProgressWithoutStartIsTracked: events can arrive out of order when work
// is concurrent; the view must not drop an item because it never saw a start.
func TestProgressWithoutStartIsTracked(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 1}, ProgressMsg{Item: item("late.ytd", 5, 10)})
	if !strings.Contains(m.View(), "late.ytd") {
		t.Fatalf("item known only from a progress event is missing:\n%s", m.View())
	}
}

// TestRateAppearsOnlyWhenMeaningful avoids a wildly wrong number in the first
// instants of an operation.
func TestRateAppearsOnlyWhenMeaningful(t *testing.T) {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	now := base
	m := New(io.Discard, "loading", false).WithClock(func() time.Time { return now })
	m = apply(m, SizeMsg{Width: 80, Height: 24}, PlanMsg{Verb: "loading", Count: 10, TotalBytes: 100 << 20})
	m = apply(m, StartMsg{Item: item("a", 0, 10<<20)}, FinishMsg{Item: item("a", 10<<20, 10<<20)})
	if strings.Contains(m.View(), "/s") {
		t.Fatalf("a rate was shown before enough time passed:\n%s", m.View())
	}
	now = base.Add(10 * time.Second)
	if got := m.View(); !strings.Contains(got, "1.0 MiB/s") {
		t.Fatalf("rate missing or wrong:\n%s", got)
	}
}

func lipglossWidth(s string) int { return len([]rune(s)) }

// runeIndexAny is the display column of the first rune from set.
func runeIndexAny(s, set string) int {
	for i, r := range []rune(s) {
		if strings.ContainsRune(set, r) {
			return i
		}
	}
	return -1
}

// runeIndex is the display column where sub begins.
func runeIndex(s, sub string) int {
	b := strings.Index(s, sub)
	if b < 0 {
		return -1
	}
	return len([]rune(s[:b]))
}

func TestBytesFormatting(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{{0, "0 B"}, {512, "512 B"}, {1024, "1.0 KiB"}, {1536, "1.5 KiB"}, {1 << 20, "1.0 MiB"}, {1 << 30, "1.0 GiB"}, {1 << 40, "1.0 TiB"}} {
		if got := Bytes(tc.n); got != tc.want {
			t.Errorf("Bytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestTruncateKeepsWidth(t *testing.T) {
	long := strings.Repeat("résources/", 20)
	got := truncate(long, 30)
	if len([]rune(got)) > 30 {
		t.Fatalf("truncate produced %d runes: %q", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated text should say so: %q", got)
	}
	if short := truncate("fits", 30); short != "fits" {
		t.Fatalf("short text was altered: %q", short)
	}
}

// TestSpareRowsShowRecentlyFinishedItems: individual files often start and
// finish between two repaints, so an "in flight only" view looks stalled while
// racing through hundreds of them. The spare rows must name what just
// completed, and must never push the region over its budget.
func TestSpareRowsShowRecentlyFinishedItems(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 100})
	for i := 0; i < 4; i++ {
		k := fmt.Sprintf("done%02d.bin", i)
		m = apply(m, StartMsg{Item: item(k, 0, 1<<20)}, FinishMsg{Item: item(k, 1<<20, 1<<20)})
	}
	got := m.View()
	if !strings.Contains(got, "done03.bin") {
		t.Fatalf("the most recent completion is not shown:\n%s", got)
	}
	if !strings.Contains(got, doneMark) {
		t.Fatalf("completed rows are not marked:\n%s", got)
	}

	// In-flight work always takes precedence over history.
	for i := 0; i < maxActiveRows+3; i++ {
		m = apply(m, StartMsg{Item: item(fmt.Sprintf("live%02d.bin", i), 0, 1<<20)})
	}
	got = m.View()
	if strings.Contains(got, "done03.bin") {
		t.Fatalf("history displaced in-flight work:\n%s", got)
	}
	if lines := strings.Split(strings.TrimRight(got, "\n"), "\n"); len(lines) > summaryLines+maxActiveRows+1 {
		t.Fatalf("region grew past its budget: %d lines\n%s", len(lines), got)
	}
}

// TestBackgroundBytesDoNotClaimCompletedItems: data arriving ahead of the
// files it belongs to must move the transferred total, not the item count.
func TestBackgroundBytesDoNotClaimCompletedItems(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 10, TotalBytes: 10 << 20})
	m = apply(m, FetchedMsg{Item: item("a", 5<<20, 10<<20)})
	got := m.View()
	if !strings.Contains(got, "0 of 10 assets") {
		t.Fatalf("downloaded bytes were counted as finished items:\n%s", got)
	}
	if !strings.Contains(got, "5.0 MiB of 10.0 MiB downloaded") {
		t.Fatalf("transferred total not shown:\n%s", got)
	}
	if !strings.Contains(got, "  0%") {
		t.Fatalf("the bar tracks items, so it must still be at zero:\n%s", got)
	}
	// And a later finish must not add those bytes a second time.
	m = apply(m, StartMsg{Item: item("a", 0, 5<<20)}, FinishMsg{Item: item("a", 5<<20, 5<<20)})
	if !strings.Contains(m.View(), "5.0 MiB of 10.0 MiB") {
		t.Fatalf("bytes were double-counted:\n%s", m.View())
	}
}

// TestTransferringItemsShowTheirOwnProgress is what a large load needs: a row
// per file that is moving, with its own bar, the way a container image shows
// each layer. Cumulative per-item totals must not be double-counted.
func TestTransferringItemsShowTheirOwnProgress(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 3, TotalBytes: 24 << 20})
	m = apply(m, FetchedMsg{Item: item("big.ytd", 2<<20, 8<<20)})
	got := m.View()
	if !strings.Contains(got, "big.ytd") {
		t.Fatalf("a downloading file has no row:\n%s", got)
	}
	if !strings.Contains(got, "2.0 MiB / 8.0 MiB") {
		t.Fatalf("per-file figures missing:\n%s", got)
	}
	if !strings.Contains(got, "█") || !strings.Contains(got, "░") {
		t.Fatalf("per-file bar missing:\n%s", got)
	}
	// Cumulative updates advance by their delta only.
	m = apply(m, FetchedMsg{Item: item("big.ytd", 6<<20, 8<<20)})
	if !strings.Contains(m.View(), "6.0 MiB of 24.0 MiB downloaded") {
		t.Fatalf("cumulative transfer was double-counted:\n%s", m.View())
	}
	// A fully downloaded file stops occupying an in-flight row.
	m = apply(m, FetchedMsg{Item: item("big.ytd", 8<<20, 8<<20)})
	got = m.View()
	if strings.Contains(got, "8.0 MiB / 8.0 MiB") {
		t.Fatalf("a finished download still holds an in-flight row:\n%s", got)
	}
	if !strings.Contains(got, "8.0 MiB of 24.0 MiB downloaded") {
		t.Fatalf("completed transfer not counted:\n%s", got)
	}
}

// TestChunkCountsAreShownForSplitAssets: a large asset arrives in pieces, and
// "12 of 47 chunks" is often more legible than a byte figure. A whole-blob
// asset has one piece and must not be annotated.
func TestChunkCountsAreShownForSplitAssets(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 2, TotalBytes: 16 << 20})
	m = apply(m, FetchedMsg{Item: Item{Key: "big", Label: "big.ytd", Bytes: 4 << 20, Total: 12 << 20, Chunks: 47, ChunksDone: 12}})
	if got := m.View(); !strings.Contains(got, "(12/47)") {
		t.Fatalf("chunk counts missing:\n%s", got)
	}
	m = apply(m, FetchedMsg{Item: Item{Key: "small", Label: "small.ytd", Bytes: 1 << 20, Total: 4 << 20, Chunks: 1, ChunksDone: 1}})
	if got := m.View(); strings.Contains(got, "(1/1)") {
		t.Fatalf("a whole-blob asset should not be annotated with chunk counts:\n%s", got)
	}
}

// TestTransferRowsAlign: the eye scans down the bars, so every bar and every
// figure must start in the same column no matter how long the paths are.
func TestTransferRowsAlign(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 3, TotalBytes: 30 << 20})
	for _, it := range []Item{
		{Key: "a", Label: "a.ytd", Bytes: 1 << 20, Total: 8 << 20, Chunks: 12, ChunksDone: 2},
		{Key: "b", Label: "resources/[maps]/downtown/stream/very_long_name.ymap", Bytes: 900 << 10, Total: 2 << 20, Chunks: 4, ChunksDone: 1},
		{Key: "c", Label: "c.ydr", Bytes: 7 << 20, Total: 20 << 20, Chunks: 30, ChunksDone: 11},
	} {
		m = apply(m, FetchedMsg{Item: it})
	}
	var barCols, figCols []int
	for _, row := range m.activeRows() {
		if !strings.Contains(row, "█") && !strings.Contains(row, "░") {
			continue
		}
		// Columns are display positions, not byte offsets: a truncated path
		// contains multi-byte runes and would make byte indexes differ on rows
		// that line up perfectly on screen.
		barCols = append(barCols, runeIndexAny(row, "█░"))
		figCols = append(figCols, runeIndex(row, " / "))
	}
	if len(barCols) < 3 {
		t.Fatalf("expected three transferring rows:\n%s", strings.Join(m.activeRows(), "\n"))
	}
	for i := 1; i < len(barCols); i++ {
		if barCols[i] != barCols[0] {
			t.Fatalf("bars start at differing columns %v:\n%s", barCols, strings.Join(m.activeRows(), "\n"))
		}
		if figCols[i] != figCols[0] {
			t.Fatalf("figures start at differing columns %v:\n%s", figCols, strings.Join(m.activeRows(), "\n"))
		}
	}
	for _, row := range m.activeRows() {
		if strings.HasSuffix(row, " ") {
			t.Fatalf("row has trailing padding: %q", row)
		}
		if lipglossWidth(row) > 80 {
			t.Fatalf("row is %d columns on an 80-column terminal: %q", lipglossWidth(row), row)
		}
	}
}

// TestRowsFollowActivityNotArrival is the fix for a display that pinned itself
// to files which would not move again. A load resolves every manifest first,
// so every asset gets a row at nearly zero progress before any chunk arrives;
// showing the earliest arrivals means watching six frozen rows while the work
// happens out of sight.
func TestRowsFollowActivityNotArrival(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 100, TotalBytes: 100 << 20})
	// Every asset reports a sliver of manifest progress, in order.
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("early%02d.bin", i)
		m = apply(m, FetchedMsg{Item: Item{Key: k, Label: k, Bytes: 300, Total: 4 << 20, Chunks: 8, ChunksDone: 0}})
	}
	// Then real transfer happens on two of the later ones.
	for _, k := range []string{"early38.bin", "early39.bin"} {
		m = apply(m, FetchedMsg{Item: Item{Key: k, Label: k, Bytes: 2 << 20, Total: 4 << 20, Chunks: 8, ChunksDone: 4}})
	}
	rows := strings.Join(m.activeRows(), "\n")
	for _, want := range []string{"early38.bin", "early39.bin"} {
		if !strings.Contains(rows, want) {
			t.Fatalf("the moving files are not on screen:\n%s", rows)
		}
	}
	if strings.Contains(rows, "early00.bin") {
		t.Fatalf("a stalled arrival is holding a row:\n%s", rows)
	}
}

// TestRegionGrowsAndShrinksWithTheTerminal: a tall window should be used, and
// a window that shrinks must not leave the region taller than the screen.
func TestRegionGrowsAndShrinksWithTheTerminal(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 200})
	for i := 0; i < 60; i++ {
		k := fmt.Sprintf("a%02d.bin", i)
		m = apply(m, FetchedMsg{Item: Item{Key: k, Label: k, Bytes: 1, Total: 1 << 20}})
	}
	small := len(strings.Split(strings.TrimRight(m.View(), "\n"), "\n"))

	m = apply(m, SizeMsg{Width: 120, Height: 60})
	large := len(strings.Split(strings.TrimRight(m.View(), "\n"), "\n"))
	if large <= small {
		t.Fatalf("a taller terminal did not get more rows: %d then %d", small, large)
	}
	if large > summaryLines+maxActiveRows+1 {
		t.Fatalf("region exceeded its cap at %d lines", large)
	}

	m = apply(m, SizeMsg{Width: 60, Height: 8})
	tiny := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
	if len(tiny) > 8 {
		t.Fatalf("region is %d lines on an 8-line terminal:\n%s", len(tiny), strings.Join(tiny, "\n"))
	}
	for _, l := range tiny {
		if len([]rune(l)) > 60 {
			t.Fatalf("line wider than the narrowed terminal: %q", l)
		}
	}
}

// TestColumnsDoNotShiftBetweenFrames: a layout that is internally consistent
// but changes shape as chunked and whole-blob assets come and go is still
// jitter to the reader.
func TestColumnsDoNotShiftBetweenFrames(t *testing.T) {
	m := newTestModel()
	m = apply(m, PlanMsg{Verb: "loading", Count: 10, TotalBytes: 10 << 20})
	m = apply(m, FetchedMsg{Item: Item{Key: "big", Label: "big.ytd", Bytes: 1 << 20, Total: 4 << 20, Chunks: 9, ChunksDone: 2}})
	withChunked := runeIndexAny(m.activeRows()[0], "█░")

	// The chunked asset finishes; only whole-blob assets remain in flight.
	m = apply(m, FetchedMsg{Item: Item{Key: "big", Label: "big.ytd", Bytes: 4 << 20, Total: 4 << 20, Chunks: 9, ChunksDone: 9}})
	m = apply(m, FetchedMsg{Item: Item{Key: "small", Label: "small.ytd", Bytes: 1 << 19, Total: 1 << 20, Chunks: 1, ChunksDone: 0}})
	after := runeIndexAny(m.activeRows()[0], "█░")
	if after != withChunked {
		t.Fatalf("the bar column moved from %d to %d between frames:\n%s", withChunked, after, strings.Join(m.activeRows(), "\n"))
	}
}

// TestStageIsNamedInTheSummary: the parts of a load are bound by different
// things, so "resolving" sitting still means something different from
// "writing" sitting still, and the reader needs to know which they are
// watching.
func TestStageIsNamedInTheSummary(t *testing.T) {
	m := newTestModel()
	m = apply(m, StageMsg{Name: "resolving", Count: 754})
	if got := m.View(); !strings.Contains(got, "resolving") {
		t.Fatalf("stage not shown:\n%s", got)
	}
	m = apply(m, StageMsg{Name: "resolving", Index: 300, Count: 754})
	if got := m.View(); !strings.Contains(got, "resolving 300/754") {
		t.Fatalf("countable stage not positioned:\n%s", got)
	}
	// A finished stage stops counting rather than showing "754/754".
	m = apply(m, StageMsg{Name: "downloading", Count: 700})
	got := m.View()
	if !strings.Contains(got, "downloading") || strings.Contains(got, "resolving") {
		t.Fatalf("stage did not advance:\n%s", got)
	}
	m = apply(m, StageMsg{Name: "writing", Count: 754})
	if got := m.View(); !strings.Contains(got, "writing") {
		t.Fatalf("write stage not shown:\n%s", got)
	}
}
