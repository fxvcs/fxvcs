// Command fxvcs-chunkbench is the Stage 2 chunking measurement harness. It is
// a development tool, not part of the shipped CLI: it produces the corpus
// report behind the go/no-go recorded in docs/stage-2-measurement.md.
//
//	# synthetic corpus (reproducible anywhere, no game assets required)
//	go run ./cmd/fxvcs-chunkbench -out docs/stage-2-measurement.md
//
//	# real corpus: a directory of <format>_<name>.v1.<ext> / .v2.<ext> pairs
//	go run ./cmd/fxvcs-chunkbench -corpus /data/rsc7-pairs -out report.md
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/chunk/measure"
)

func main() {
	corpus := flag.String("corpus", "", "directory of real version pairs (default: generate a synthetic corpus)")
	out := flag.String("out", "", "write the Markdown report here (default: stdout)")
	scale := flag.Int("scale", 1, "synthetic corpus size multiplier")
	flag.Parse()

	pairs, label, err := load(*corpus, *scale)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fxvcs-chunkbench:", err)
		os.Exit(1)
	}
	results, err := measure.Run(pairs, measure.Strategies())
	if err != nil {
		fmt.Fprintln(os.Stderr, "fxvcs-chunkbench:", err)
		os.Exit(1)
	}
	report := measure.Markdown(results, label)
	if *out == "" {
		fmt.Print(report)
		return
	}
	if err := os.WriteFile(*out, []byte(report), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fxvcs-chunkbench:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "wrote", *out)
}

func load(dir string, scale int) ([]measure.Pair, string, error) {
	if dir == "" {
		tmp, err := os.MkdirTemp("", "fxvcs-chunkbench-*")
		if err != nil {
			return nil, "", err
		}
		c := measure.Corpus{Dir: tmp, Scale: scale}
		pairs, err := c.Generate()
		if err != nil {
			return nil, "", err
		}
		return pairs, fmt.Sprintf("synthetic (internal/chunk/measure.Corpus, scale %d) — structural model, not real game assets", scale), nil
	}
	pairs, err := scan(dir)
	if err != nil {
		return nil, "", err
	}
	if len(pairs) == 0 {
		return nil, "", fmt.Errorf("no <format>_<name>.v1.<ext>/.v2.<ext> pairs under %s", dir)
	}
	return pairs, dir, nil
}

// scan pairs files named <format>_<name>.v1.<ext> with their .v2 sibling.
func scan(dir string) ([]measure.Pair, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pairs []measure.Pair
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".v1.") {
			continue
		}
		v1 := filepath.Join(dir, e.Name())
		v2 := filepath.Join(dir, strings.Replace(e.Name(), ".v1.", ".v2.", 1))
		if _, err := os.Stat(v2); err != nil {
			continue
		}
		base := strings.SplitN(e.Name(), ".v1.", 2)[0]
		format := strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Name()), "."))
		if i := strings.Index(base, "_"); i > 0 {
			format = base[:i]
		}
		pairs = append(pairs, measure.Pair{Format: format, Name: base, V1: v1, V2: v2})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Name < pairs[j].Name })
	return pairs, nil
}
