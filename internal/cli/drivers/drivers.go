// Package drivers registers the Git process-boundary entry points
// (filter-process, diff-driver, merge-driver) and spike commands with
// the CLI. Their engines live in internal/filter and internal/profile; this
// package only adapts them to urfave/cli.
package drivers

import "github.com/urfave/cli/v3"

// registry is appended to by init functions in this package.
var registry []*cli.Command

// Register adds a top-level command.
func Register(c *cli.Command) { registry = append(registry, c) }

// Commands returns every registered driver/spike command.
func Commands() []*cli.Command { return registry }
