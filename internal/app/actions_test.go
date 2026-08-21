package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"fxvcs.dev/fxvcs/internal/state"
)

// actionOf finds one action's availability in a status entry.
func actionOf(t *testing.T, ps PathStatus, want Action) Availability {
	t.Helper()
	for _, a := range ps.Actions {
		if a.Action == want {
			return a
		}
	}
	t.Fatalf("%s reports no %q action: %+v", ps.Path, want, ps.Actions)
	return Availability{}
}

func statusOf(t *testing.T, repo *Repo, path string) PathStatus {
	t.Helper()
	list, _, err := repo.Status(context.Background(), Selection{Paths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("status for %s returned %d entries", path, len(list))
	}
	return list[0]
}

// TestAdvertisedActionsMatchWhatTheOperationsDo is the reason the availability
// rules live in one place.
//
// An interface asks what it may offer, then offers it. If the listing and the
// operation disagree, either a control is greyed out for something that would
// have worked, or — the case that actually matters — a control is offered for
// replacing a file whose only copy is the one on disk. So for every asset
// state, what Status advertises has to be exactly what the operation then does.
func TestAdvertisedActionsMatchWhatTheOperationsDo(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: store}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	content := asset(900, 2<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "loaded.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"loaded.bin", "unloaded.bin", "modified.bin"} {
		if _, err := repo.Ingest(ctx, bytes.NewReader(content), p); err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	// loaded.bin holds the real bytes; unloaded.bin stays a placeholder;
	// modified.bin holds bytes that are in neither Git nor the object store.
	r.WriteFile("loaded.bin", content)
	r.WriteFile("modified.bin", append(bytes.Clone(content), []byte("LOCAL EDIT")...))

	for _, tc := range []struct {
		path            string
		load, unload    bool
		loadDone        bool
		unloadDone      bool
		unloadRefusedBy error
	}{
		{path: "loaded.bin", load: false, loadDone: true, unload: true},
		{path: "unloaded.bin", load: true, unload: false, unloadDone: true},
		{path: "modified.bin", load: false, unload: false, unloadRefusedBy: ErrConflict},
	} {
		t.Run(tc.path, func(t *testing.T) {
			ps := statusOf(t, repo, tc.path)
			load := actionOf(t, ps, ActionLoad)
			unload := actionOf(t, ps, ActionUnload)
			if load.Available != tc.load || load.Done != tc.loadDone {
				t.Fatalf("load advertised as %+v", load)
			}
			if unload.Available != tc.unload || unload.Done != tc.unloadDone {
				t.Fatalf("unload advertised as %+v", unload)
			}
			if tc.unloadRefusedBy != nil && unload.Err != tc.unloadRefusedBy {
				t.Fatalf("unload refusal is %v, want %v", unload.Err, tc.unloadRefusedBy)
			}

			// Now do it, and require the operation to agree.
			outs, err := repo.Dehydrate(ctx, Selection{Paths: []string{tc.path}}, nil)
			if err != nil || len(outs) != 1 {
				t.Fatalf("unload: %v %+v", err, outs)
			}
			acted := !outs[0].Skipped
			if acted != unload.Available {
				t.Fatalf("status advertised unload available=%v but the operation %s (%s)",
					unload.Available, actedWord(acted), outs[0].Reason)
			}
			if !acted && outs[0].Reason != unload.Reason {
				t.Fatalf("status and operation give different reasons:\n status: %s\n   op:   %s", unload.Reason, outs[0].Reason)
			}
		})
	}

	// The refusal really did leave the file alone.
	got, err := os.ReadFile(filepath.Join(repo.Root, "modified.bin")) //nolint:gosec // test tree
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte("LOCAL EDIT")) {
		t.Fatal("a refused unload modified the working file")
	}
}

func actedWord(acted bool) string {
	if acted {
		return "acted"
	}
	return "refused"
}

// TestPublishActionTracksTheLedger: an interface should offer "upload" only
// when there is something to upload, and say why when there is not.
func TestPublishActionTracksTheLedger(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	ing, err := repo.Ingest(ctx, bytes.NewReader(asset(901, 1<<20)), "a.bin")
	if err != nil {
		t.Fatal(err)
	}
	r.WriteFile("a.bin", ing.Pointer.MustEncode())
	r.Git("add", ".")
	r.Commit("asset")

	// The fixture repository declares a required remote that is not bound
	// here, so publication is pending and the action is offered.
	if pub := actionOf(t, statusOf(t, repo, "a.bin"), ActionPublish); !pub.Available {
		t.Fatalf("publish not offered for an unpublished asset: %+v", pub)
	}
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	pub := actionOf(t, statusOf(t, repo, "a.bin"), ActionPublish)
	if pub.Available || !pub.Done {
		t.Fatalf("publish still offered after uploading: %+v", pub)
	}
	if pub.Reason != "already uploaded" {
		t.Fatalf("unhelpful reason: %q", pub.Reason)
	}
}

// TestDiscardIsTheOnlyWayBackAndSaysSoFirst covers the affordance and the
// operation together, because a discard control that disagrees with the
// engine is worse than no control at all: the bytes it throws away are in no
// commit and no object store, and there is nothing to undo it with.
func TestDiscardIsTheOnlyWayBackAndSaysSoFirst(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	content := asset(920, 2<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "clean.bin")
	if err != nil {
		t.Fatal(err)
	}
	// A second asset, so "conflict" can be a working file holding a valid
	// pointer to something other than what the index records.
	other := asset(921, 1<<20)
	otherIng, err := repo.Ingest(ctx, bytes.NewReader(other), "conflicted.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"clean.bin", "modified.bin", "conflicted.bin"} {
		if _, err := repo.Ingest(ctx, bytes.NewReader(content), p); err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	// clean.bin holds the recorded bytes; modified.bin holds an edit that
	// exists nowhere else; conflicted.bin points at the other asset.
	r.WriteFile("clean.bin", content)
	edit := append(bytes.Clone(content), []byte("LOCAL EDIT")...)
	r.WriteFile("modified.bin", edit)
	r.WriteFile("conflicted.bin", otherIng.Pointer.MustEncode())

	for _, tc := range []struct {
		path      string
		available bool
		done      bool
		// want is the file content the operation must leave behind.
		want []byte
	}{
		{path: "clean.bin", done: true, want: content},
		{path: "modified.bin", available: true, want: content},
		{path: "conflicted.bin", available: true, want: ing.Pointer.MustEncode()},
	} {
		t.Run(tc.path, func(t *testing.T) {
			av := actionOf(t, statusOf(t, repo, tc.path), ActionDiscard)
			if av.Available != tc.available || av.Done != tc.done {
				t.Fatalf("discard advertised as %+v", av)
			}
			outs, err := repo.Discard(ctx, Selection{Paths: []string{tc.path}}, nil)
			if err != nil || len(outs) != 1 {
				t.Fatalf("discard: %v %+v", err, outs)
			}
			if acted := !outs[0].Skipped; acted != av.Available {
				t.Fatalf("status advertised discard available=%v but the operation %s (%s)",
					av.Available, actedWord(acted), outs[0].Reason)
			}
			got, err := os.ReadFile(filepath.Join(repo.Root, tc.path)) //nolint:gosec // test tree
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("working file is %d bytes, want the recorded %d", len(got), len(tc.want))
			}
			// Having discarded, there is nothing left to discard.
			if again := actionOf(t, statusOf(t, repo, tc.path), ActionDiscard); again.Available || !again.Done {
				t.Fatalf("discard still offered after discarding: %+v", again)
			}
		})
	}

	// A discarded asset stays loaded. Restoring it as a placeholder would be a
	// second, unasked-for decision about the user's disk.
	if ps := statusOf(t, repo, "modified.bin"); ps.State != state.HydratedClean {
		t.Fatalf("discard left modified.bin in state %q", ps.State)
	}
}

// TestStatusMeasuresHowMuchAnAssetChanged: Git cannot answer this. It diffs
// the placeholders, so a texture that grew by 40 MB is one line added and one
// removed. The index-side size makes the real number available.
func TestStatusMeasuresHowMuchAnAssetChanged(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	content := asset(930, 1<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "grown.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"grown.bin", "shrunk.bin", "same.bin", "placeholder.bin"} {
		if _, err := repo.Ingest(ctx, bytes.NewReader(content), p); err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")

	const grewBy = 4096
	r.WriteFile("grown.bin", append(bytes.Clone(content), make([]byte, grewBy)...))
	r.WriteFile("shrunk.bin", content[:len(content)-grewBy])
	same := bytes.Clone(content)
	copy(same, []byte("a different first line, identical length"))
	r.WriteFile("same.bin", same)

	for _, tc := range []struct {
		path string
		// delta is the expected SizeDelta; nil means "not comparable".
		delta *int64
	}{
		{path: "grown.bin", delta: ptrTo(int64(grewBy))},
		{path: "shrunk.bin", delta: ptrTo(int64(-grewBy))},
		{path: "same.bin", delta: ptrTo(int64(0))},
		{path: "placeholder.bin", delta: nil}, // a pointer's length is not the asset's
	} {
		t.Run(tc.path, func(t *testing.T) {
			ps := statusOf(t, repo, tc.path)
			if ps.Size != int64(len(content)) {
				t.Fatalf("index size is %d, want the recorded %d", ps.Size, len(content))
			}
			switch {
			case tc.delta == nil && ps.SizeDelta != nil:
				t.Fatalf("a placeholder reported a %d byte change", *ps.SizeDelta)
			case tc.delta == nil:
				if ps.WorkingSize == 0 || ps.WorkingSize >= ps.Size {
					t.Fatalf("placeholder working size %d is not a pointer's", ps.WorkingSize)
				}
			case ps.SizeDelta == nil:
				t.Fatal("a changed asset reported no measurable change")
			case *ps.SizeDelta != *tc.delta:
				t.Fatalf("change is %+d bytes, want %+d", *ps.SizeDelta, *tc.delta)
			}
		})
	}
}

func ptrTo(v int64) *int64 { return &v }

// TestScopedStatusAgreesWithWholeRepositoryStatus guards the rule that makes
// scoping usable: a listing restricted to some paths must say exactly what the
// whole-repository listing says about those paths.
//
// The two now read the recorded rows by different routes — one query for
// everything, or one query per batch of selected paths — and a scoped listing
// that silently lost a row would not look broken. It would look like an asset
// with no local state: a placeholder reported as missing, or a modified file
// reported as clean, which is the reading that gets bytes overwritten.
func TestScopedStatusAgreesWithWholeRepositoryStatus(t *testing.T) {
	t.Parallel()
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))
	content := asset(950, 64<<10)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "seed.bin")
	if err != nil {
		t.Fatal(err)
	}
	// Enough paths to cross the query batch size, spread over directories.
	var paths []string
	for i := 0; i < 1200; i++ {
		p := fmt.Sprintf("assets/d%02d/f%04d.bin", i%12, i)
		if _, err := repo.Ingest(ctx, bytes.NewReader(content), p); err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
		paths = append(paths, p)
	}
	r.Git("add", ".")
	r.Commit("assets")
	// A mix of states, so an agreement is not just an agreement about
	// placeholders: some loaded, some changed locally, one deleted.
	for i := 0; i < 1200; i += 3 {
		r.WriteFile(paths[i], content)
	}
	for i := 1; i < 1200; i += 7 {
		r.WriteFile(paths[i], append(bytes.Clone(content), byte(i)))
	}
	if err := os.Remove(filepath.Join(repo.Root, paths[5])); err != nil {
		t.Fatal(err)
	}

	whole, _, err := repo.Status(ctx, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]PathStatus{}
	for _, ps := range whole {
		byPath[ps.Path] = ps
	}
	if len(byPath) != len(paths) {
		t.Fatalf("fixture reports %d assets", len(byPath))
	}

	// Selections either side of the batch size, and one directory.
	for _, sel := range []Selection{
		{Paths: paths[:1]},
		{Paths: paths[:499]},
		{Paths: paths[:500]},
		{Paths: paths[:501]},
		{Paths: paths[600:1100]},
		{Paths: []string{"assets/d03"}},
	} {
		scoped, _, err := repo.Status(ctx, sel)
		if err != nil {
			t.Fatal(err)
		}
		if len(scoped) == 0 {
			t.Fatalf("selection of %d paths reported nothing", len(sel.Paths))
		}
		for _, got := range scoped {
			want, ok := byPath[got.Path]
			if !ok {
				t.Fatalf("scoped listing invented %s", got.Path)
			}
			if got.State != want.State || got.IndexDigest != want.IndexDigest ||
				got.Size != want.Size || got.WorkingSize != want.WorkingSize ||
				got.Cached != want.Cached || got.Pending != want.Pending {
				t.Fatalf("%s scoped %+v, whole-repository %+v", got.Path, got, want)
			}
			if (got.SizeDelta == nil) != (want.SizeDelta == nil) ||
				(got.SizeDelta != nil && *got.SizeDelta != *want.SizeDelta) {
				t.Fatalf("%s scoped delta %v, whole-repository %v", got.Path, got.SizeDelta, want.SizeDelta)
			}
			for i, a := range got.Actions {
				if a != want.Actions[i] {
					t.Fatalf("%s scoped action %+v, whole-repository %+v", got.Path, a, want.Actions[i])
				}
			}
		}
	}
}
