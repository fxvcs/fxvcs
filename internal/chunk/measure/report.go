package measure

import (
	"fmt"
	"sort"
	"strings"
)

// Markdown renders the comparison table the Stage 2 exit criterion asks for:
// whole blobs, fixed chunks, and candidate CDC parameters compared on transfer
// savings, stored bytes, CPU time, memory, and manifest overhead.
func Markdown(results []Result, corpus string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Corpus: %s\n\n", corpus)
	formats := map[string][]Result{}
	var order []string
	for _, r := range results {
		if _, ok := formats[r.Format]; !ok {
			order = append(order, r.Format)
		}
		formats[r.Format] = append(formats[r.Format], r)
	}
	sort.Strings(order)
	for _, f := range order {
		rows := formats[f]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Strategy < rows[j].Strategy })
		fmt.Fprintf(&b, "### %s (%d asset pairs, v2 total %s)\n\n", f, rows[0].Samples, bytesText(rows[0].V2Bytes))
		b.WriteString("| strategy | v2 transfer | saving | stored (v1+v2) | chunks v2 | new chunks | manifest | zstd saving | split | alloc |\n")
		b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
		for _, r := range rows {
			name := r.Strategy
			if name == Shipped {
				name = "**" + name + "** (cdc-fastcdc/1)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %s | %s | %s | %.0f MiB |\n",
				name,
				bytesText(r.TransferV2),
				percent(r.TransferSaving()),
				bytesText(r.StoredBoth),
				r.ChunksV2,
				r.NewChunksV2,
				bytesText(r.ManifestBytesV2),
				percent(r.CompressionSaving()),
				fmt.Sprintf("%.0f MiB/s", r.ThroughputMiBs()),
				r.PeakAllocMiB,
			)
		}
		b.WriteString("\n")
	}
	b.WriteString("### Summary by format (best CDC candidate vs whole blob)\n\n")
	b.WriteString("| format | whole transfer | best CDC | strategy | net saving | verdict |\n")
	b.WriteString("|---|--:|--:|---|--:|---|\n")
	for _, f := range order {
		var whole Result
		best := Result{}
		bestSet := false
		for _, r := range formats[f] {
			if r.Strategy == "whole" {
				whole = r
				continue
			}
			if !strings.HasPrefix(r.Strategy, "cdc-") {
				continue
			}
			cost := r.TransferV2 + r.ManifestBytesV2
			if !bestSet || cost < best.TransferV2+best.ManifestBytesV2 {
				best, bestSet = r, true
			}
		}
		if !bestSet {
			continue
		}
		saving := 1 - float64(best.TransferV2+best.ManifestBytesV2)/float64(whole.TransferV2+whole.ManifestBytesV2)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			f, bytesText(whole.TransferV2), bytesText(best.TransferV2), best.Strategy, percent(saving), verdict(saving))
	}
	return b.String()
}

// GateSaving is the net transfer saving a format must show before
// chunk.DefaultPolicy encodes it with CDC. Below it the manifest bytes, the
// extra round trips, and the CPU cost are not worth paying.
const GateSaving = 0.25

func verdict(saving float64) string {
	if saving >= GateSaving {
		return "**chunk**"
	}
	return "whole blob"
}

func percent(f float64) string {
	if f < 0 {
		return fmt.Sprintf("%.1f%%", f*100)
	}
	return fmt.Sprintf("%.1f%%", f*100)
}

func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
