package measure

import (
	"os"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/chunk"
)

// TestHarnessProducesAComparableReport keeps the measurement spike runnable:
// the go/no-go in docs/stage-2-measurement.md is only re-checkable against a
// real corpus if this harness still works.
func TestHarnessProducesAComparableReport(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a multi-megabyte corpus")
	}
	dir := t.TempDir()
	pairs, err := Corpus{Dir: dir, Scale: 1}.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) < 4 {
		t.Fatalf("corpus has %d pairs", len(pairs))
	}
	for _, p := range pairs {
		v1, err := os.Stat(p.V1)
		if err != nil {
			t.Fatal(err)
		}
		v2, err := os.Stat(p.V2)
		if err != nil {
			t.Fatal(err)
		}
		if v1.Size() == 0 || v2.Size() == 0 {
			t.Fatalf("%s: empty version (%d, %d)", p.Name, v1.Size(), v2.Size())
		}
	}

	results, err := Run(pairs, Strategies())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(Strategies())*4 && len(results) < len(Strategies()) {
		t.Fatalf("got %d results", len(results))
	}
	byKey := map[string]Result{}
	for _, r := range results {
		byKey[r.Format+"/"+r.Strategy] = r
		if r.V1Bytes == 0 || r.V2Bytes == 0 {
			t.Fatalf("%s/%s measured nothing", r.Format, r.Strategy)
		}
		if r.TransferV2 > r.StoredBoth {
			t.Fatalf("%s/%s transfers more than it stores", r.Format, r.Strategy)
		}
	}
	// The whole-blob baseline must always transfer the entire second version:
	// that is what chunking is being compared against.
	whole := byKey["ymap/whole"]
	if whole.TransferV2 != whole.V2Bytes {
		t.Fatalf("whole-blob baseline transferred %d of %d bytes", whole.TransferV2, whole.V2Bytes)
	}
	// And the shipped parameter set must beat it on the format that motivated
	// chunking in the first place.
	shipped := byKey["ymap/"+Shipped]
	if shipped.TransferV2 >= whole.TransferV2/2 {
		t.Fatalf("%s transferred %d bytes where whole blobs transferred %d; chunking is not paying off",
			Shipped, shipped.TransferV2, whole.TransferV2)
	}

	report := Markdown(results, "test corpus")
	for _, want := range []string{"cdc-fastcdc/1", "Summary by format", "v2 transfer"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
}

// TestShippedStrategyMatchesTheEncoding guards the link between the report and
// the code: the row the report marks as shipped must be the parameter set
// cdc-fastcdc/1 actually uses.
func TestShippedStrategyMatchesTheEncoding(t *testing.T) {
	var found bool
	for _, s := range Strategies() {
		if s.Name != Shipped {
			continue
		}
		found = true
		p := chunk.FastCDCv1
		if s.Params.Min != p.Min || s.Params.Avg != p.Avg || s.Params.Max != p.Max || s.Params.Normalization != p.Normalization {
			t.Fatalf("the report's %q row is %+v but cdc-fastcdc/1 is %+v", Shipped, s.Params, p)
		}
	}
	if !found {
		t.Fatalf("no strategy named %q", Shipped)
	}
}
