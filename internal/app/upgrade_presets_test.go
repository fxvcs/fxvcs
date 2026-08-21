package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/profile/fivem"
)

// A fivem repository whose managed block predates newer preset patterns gets
// them added by upgrade: additive, user-added patterns kept, lines outside the
// block untouched, uncommitted for review.
func TestUpgradeAddsNewPresetPatterns(t *testing.T) {
	// Never touch the developer's real launcher: redirect the per-user data
	// dirs so setup/upgrade install into a temp location.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "local"))
	r, repo := fivemRepo(t)
	ctx := context.Background()
	old := fivem.AttributesBlock([]string{"*.ytd", "*.ydr", "*.custom"}) // an older preset set plus a user pattern
	attrs := []byte("# user rule above\n*.lua text\n" + string(old) + "# user rule below\n")
	r.WriteFile(".gitattributes", attrs)
	r.Commit("old presets")

	plan, err := repo.Upgrade(ctx, UpgradePlan)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, st := range plan.Steps {
		if st.Target == AttributesPath && strings.Contains(st.Description, "*.ymf") && strings.Contains(st.Description, "preset pattern") {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan lacks preset step: %+v", plan.Steps)
	}
	if got := r.ReadFile(".gitattributes"); !bytes.Equal(got, attrs) {
		t.Fatal("plan modified .gitattributes")
	}
	if _, err := repo.Upgrade(ctx, UpgradeApply); err != nil {
		t.Fatal(err)
	}
	got := string(r.ReadFile(".gitattributes"))
	for _, want := range []string{"# user rule above\n*.lua text\n", "# user rule below\n", "*.custom filter=fxvcs", "*.ymf filter=fxvcs", "*.ytd filter=fxvcs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, p := range fivem.DefaultBinaryPatterns {
		if !strings.Contains(got, p+" filter=fxvcs") {
			t.Fatalf("preset %s not added", p)
		}
	}
	if committed := r.Git("show", "HEAD:.gitattributes"); committed == got {
		t.Fatal("expected an uncommitted .gitattributes change (upgrade must not commit)")
	}
	// Idempotent: a second plan has no attributes step.
	plan, _ = repo.Upgrade(ctx, UpgradePlan)
	for _, st := range plan.Steps {
		if st.Target == AttributesPath {
			t.Fatalf("attributes step still planned: %+v", st)
		}
	}

	// Opting out: untrack a preset records it in the committed tracking
	// policy and upgrade does not bring it back.
	if _, err := repo.Untrack(ctx, TrackInput{Patterns: []string{"*.png"}, Glob: true}); err != nil {
		t.Fatal(err)
	}
	if got := repo.Config.Spec.Tracking; got == nil || len(got.DisabledPresets) != 1 || got.DisabledPresets[0] != "*.png" {
		t.Fatalf("opt-out not recorded: %+v", got)
	}
	if cfg := string(r.ReadFile(".fxvcs/repository.yaml")); !strings.Contains(cfg, "disabledPresets") || !strings.Contains(cfg, "*.png") {
		t.Fatalf("repository.yaml missing opt-out:\n%s", cfg)
	}
	plan, _ = repo.Upgrade(ctx, UpgradePlan)
	for _, st := range plan.Steps {
		if st.Target == AttributesPath {
			t.Fatalf("upgrade wants to re-add an opted-out preset: %+v", st)
		}
	}
	if strings.Contains(string(r.ReadFile(".gitattributes")), "*.png filter=fxvcs") {
		t.Fatal("*.png still in the managed block after untrack")
	}
	// Re-enabling clears the opt-out.
	if _, err := repo.Track(ctx, TrackInput{Patterns: []string{"*.png"}, Glob: true}); err != nil {
		t.Fatal(err)
	}
	if repo.Config.Spec.Tracking != nil && len(repo.Config.Spec.Tracking.DisabledPresets) != 0 {
		t.Fatalf("opt-out not cleared: %+v", repo.Config.Spec.Tracking)
	}
	if strings.Contains(string(r.ReadFile(".fxvcs/repository.yaml")), "disabledPresets") {
		t.Fatal("empty tracking policy left in repository.yaml")
	}
	_ = os.Remove(filepath.Join(r.Root, ".gitattributes"))
}
