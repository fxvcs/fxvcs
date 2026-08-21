package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
)

// parseSize reads a human byte size: a plain number of bytes, or a number with
// a unit. Both conventions are accepted because both are in common use and
// arguing about them helps nobody:
//
//	500      500 bytes
//	20K 20KB 20 000 bytes
//	20KiB    20 480 bytes
//
// Suffixes are case-insensitive and a space before the unit is allowed.
func parseSize(s string) (int64, error) {
	text := strings.TrimSpace(strings.ToLower(s))
	if text == "" {
		return 0, fmt.Errorf("empty size")
	}
	units := []struct {
		suffix string
		mult   int64
	}{
		{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"tb", 1e12}, {"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10},
		{"b", 1},
	}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(text, u.suffix) {
			mult = u.mult
			text = strings.TrimSpace(strings.TrimSuffix(text, u.suffix))
			break
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size", s)
	}
	if value < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return int64(value * float64(mult)), nil
}

// optionalSize reads a size flag that may be unset.
func optionalSize(cmd *cli.Command, name string) (int64, error) {
	raw := cmd.String(name)
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	n, err := parseSize(raw)
	if err != nil {
		return 0, usageError{err.Error(), "sizes look like 500MB, 20GiB, or a plain number of bytes"}
	}
	return n, nil
}
