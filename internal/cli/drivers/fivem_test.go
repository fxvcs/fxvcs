package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cli"
)

const fivemFixtures = "../../../spec/fixtures/fivem"

func runFivem(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Extra: Commands()}
	code := cli.Run(context.Background(), env, append([]string{"fxvcs"}, args...))
	return code, out.String(), errb.String()
}

func TestFivemScanJSON(t *testing.T) {
	code, out, errs := runFivem(t, "--json", "fivem", "scan", filepath.Join(fivemFixtures, "collection"))
	if code != cli.ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var res fivemScanResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if res.Descriptor.Layout != "collection" || len(res.Resources) != 7 || res.Detection == nil {
		t.Fatalf("%+v", res)
	}
}

func TestFivemScanExplicitDescriptor(t *testing.T) {
	code, out, errs := runFivem(t, "fivem", "scan", "--source-root", "resources", "--layout", "collection", filepath.Join(fivemFixtures, "resources-wrapper"))
	if code != cli.ExitOK || !strings.Contains(out, "[core]/framework") {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
}

func TestFivemScanFailsClosed(t *testing.T) {
	code, _, errs := runFivem(t, "fivem", "scan", filepath.Join(fivemFixtures, "ambiguous-mixed"))
	if code != cli.ExitFailure || !strings.Contains(errs, "mixed-shape") {
		t.Fatalf("exit %d err=%s", code, errs)
	}
}

func TestFivemManifest(t *testing.T) {
	code, out, errs := runFivem(t, "fivem", "manifest", filepath.Join(fivemFixtures, "collection", "mapmanager-fork", "fxmanifest.lua"))
	if code != cli.ExitOK || !strings.Contains(out, "provides: mapmanager") {
		t.Fatalf("exit %d out=%s err=%s", code, out, errs)
	}
	code, _, errs = runFivem(t, "--json", "fivem", "manifest", filepath.Join(fivemFixtures, "limits", "forbidden-io.lua"))
	if code != cli.ExitFailure || !strings.Contains(errs, "forbidden") {
		t.Fatalf("exit %d err=%s", code, errs)
	}
	code, _, _ = runFivem(t, "fivem", "manifest")
	if code != cli.ExitUsage {
		t.Fatalf("missing arg exit %d", code)
	}
}
