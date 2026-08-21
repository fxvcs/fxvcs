package app

import (
	"bytes"
	"context"
	"testing"
)

// TestPublishSelectionMakesThePerAssetActionActionable.
//
// Status reports the publish affordance per asset. Until publication could be
// scoped the same way, that was a promise the SDK could not keep: an interface
// was told this particular asset was publishable and had only a
// whole-repository upload to offer for it, which is why a control labelled with
// one file's name was not worth building.
//
// So the property under test is the one an interface depends on: publishing a
// selection uploads what those assets need and leaves the rest pending, and the
// action for each asset afterwards says so.
func TestPublishSelectionMakesThePerAssetActionActionable(t *testing.T) {
	r, cacheDir := testRepo(t)
	repo := openTest(t, r, cacheDir)
	ctx := context.Background()
	if _, err := repo.StorageAdd(ctx, StorageAddInput{Name: "primary", Type: "filesystem", Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r.WriteFile(".gitattributes", []byte("*.bin filter=fxvcs diff=fxvcs merge=fxvcs -text\n"))

	// Two assets with unrelated content, so neither can be published as a side
	// effect of the other sharing its objects.
	for i, p := range []string{"chosen.bin", "untouched.bin"} {
		ing, err := repo.Ingest(ctx, bytes.NewReader(asset(uint64(940+i), 2<<20)), p)
		if err != nil {
			t.Fatal(err)
		}
		r.WriteFile(p, ing.Pointer.MustEncode())
	}
	r.Git("add", ".")
	r.Commit("assets")

	for _, p := range []string{"chosen.bin", "untouched.bin"} {
		if av := actionOf(t, statusOf(t, repo, p), ActionPublish); !av.Available {
			t.Fatalf("%s: publish not offered before uploading: %+v", p, av)
		}
	}

	sel := Selection{Paths: []string{"chosen.bin"}}
	published, failed, err := repo.Publish(ctx, PublishOptions{Selection: &sel})
	if err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %+v", err, failed)
	}
	if len(published) == 0 {
		t.Fatal("a scoped publish uploaded nothing at all")
	}
	for _, o := range published {
		for _, path := range o.Paths {
			if path != "chosen.bin" {
				t.Fatalf("publishing chosen.bin also uploaded objects for %s", path)
			}
		}
	}

	if av := actionOf(t, statusOf(t, repo, "chosen.bin"), ActionPublish); av.Available || !av.Done {
		t.Fatalf("publish still offered for the asset just uploaded: %+v", av)
	}
	if av := actionOf(t, statusOf(t, repo, "untouched.bin"), ActionPublish); !av.Available {
		t.Fatalf("a scoped publish silently uploaded the asset it was not given: %+v", av)
	}

	// The whole-repository publish is still the way to be sure a push will
	// pass: it finishes what the scoped one deliberately left behind.
	if _, failed, err := repo.Publish(ctx, PublishOptions{}); err != nil || len(failed) != 0 {
		t.Fatalf("repository publish: %v %+v", err, failed)
	}
	if av := actionOf(t, statusOf(t, repo, "untouched.bin"), ActionPublish); av.Available || !av.Done {
		t.Fatalf("publish still pending after a repository-wide upload: %+v", av)
	}
}
