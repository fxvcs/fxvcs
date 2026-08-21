package sdk

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/format"
)

// A UI consuming publish events sees the full plan first, then each item's
// started/finished (interleaved across the concurrency window), then done.
func TestPublishStreamsPlanAndPerItemProgress(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := writeRepo(t, "0.1.0", 1)
	cacheDir := t.TempDir()
	repo, err := app.Open(context.Background(), dir, app.Options{Installed: format.MustParseVersion("0.1.0"), CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, app.StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir(), Local: true, RequiredForPublication: true}); err != nil {
		t.Fatal(err)
	}
	const n = 6
	for i := 0; i < n; i++ {
		if _, err := repo.Ingest(ctx, bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, 64*1024)), "res/"+string(rune('a'+i))+".bin"); err != nil {
			t.Fatal(err)
		}
	}
	_ = repo.Close()

	var events []Event
	c, _ := New(Options{InstalledVersion: "0.1.0", CacheDir: cacheDir, Progress: func(ev Event) { events = append(events, ev) }})
	res, err := c.ObjectsPublish(ctx, ObjectsPublishRequest{Path: dir, Concurrency: 3})
	if err != nil || len(res.Published) != n || len(res.Failed) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// Sequence: operation-started, plan, (started/finished)*n, info(done), operation-finished.
	if events[0].Kind != EventOperationStarted || events[1].Kind != EventPlan || events[len(events)-1].Kind != EventOperationFinished {
		t.Fatalf("unexpected framing: %v … %v", events[0].Kind, events[len(events)-1].Kind)
	}
	plan := events[1]
	if len(plan.Items) != n || plan.Count != n || plan.Total != int64(n*64*1024) {
		t.Fatalf("plan: %+v", plan)
	}
	for _, it := range plan.Items {
		if it.Path == "" || it.Digest == "" || it.Bytes != 64*1024 || it.Remote != "primary" {
			t.Fatalf("plan item incomplete: %+v", it)
		}
	}
	started, finished := map[string]bool{}, map[string]bool{}
	for _, ev := range events[2:] {
		switch ev.Kind {
		case EventItemStarted:
			if ev.Index < 1 || ev.Index > n || ev.Count != n || ev.Total != 64*1024 || ev.Remote != "primary" || ev.Path == "" {
				t.Fatalf("started event incomplete: %+v", ev)
			}
			started[ev.Digest] = true
		case EventItemFinished:
			if !started[ev.Digest] || ev.Bytes != 64*1024 {
				t.Fatalf("finished before started or wrong bytes: %+v", ev)
			}
			finished[ev.Digest] = true
		case EventItemFailed:
			t.Fatalf("unexpected failure: %+v", ev)
		}
	}
	if len(started) != n || len(finished) != n {
		t.Fatalf("started=%d finished=%d", len(started), len(finished))
	}
	// Second run: plan is empty and nothing starts.
	events = nil
	if _, err := c.ObjectsPublish(ctx, ObjectsPublishRequest{Path: dir}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == EventItemStarted {
			t.Fatal("re-uploaded on second run")
		}
		if ev.Kind == EventPlan && len(ev.Items) != 0 {
			t.Fatalf("non-empty plan on second run: %+v", ev.Items)
		}
	}
	_ = os.RemoveAll(filepath.Join(dir, ".git", "fxvcs", "tmp"))
}
