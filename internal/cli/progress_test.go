package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"fxvcs.dev/fxvcs/internal/cli/ui/live"
	"fxvcs.dev/fxvcs/sdk"
)

// A burst inside the renderer's throttle interval must still get a trailing
// frame. Without it the terminal can sit at an arbitrary old percentage until
// the operation ends and the region disappears.
func TestProgressViewDrawsTrailingCanonicalSnapshot(t *testing.T) {
	var out bytes.Buffer
	v := newProgressView(&out, true, false)
	v.handle(sdk.Event{Operation: "object fetch", Kind: sdk.EventOperationStarted})
	v.handle(sdk.Event{Operation: "object fetch", Kind: sdk.EventPlan, Phase: sdk.PhaseTransfer, Count: 1, Total: 100})
	v.handle(sdk.Event{Operation: "object fetch", Kind: sdk.EventItemProgress, Phase: sdk.PhaseTransfer,
		Digest: "sha256:asset", Bytes: 50, Total: 100, WireBytes: 50})
	time.Sleep(2 * live.RedrawInterval)
	v.finish(errors.New("test stop before completion"))

	if got := out.String(); !strings.Contains(got, " 50%") {
		t.Fatalf("trailing 50%% snapshot was never drawn: %q", got)
	}
}
