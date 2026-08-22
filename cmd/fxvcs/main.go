// Command fxvcs is the FXVCS developer CLI, Git filter/diff/merge driver entry
// point, and target agent. It is a thin adapter over the
// public SDK (fxvcs.dev/fxvcs/sdk).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/cli/drivers"
)

func main() {
	prepareSubprocessConsole()
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Run(ctx, cli.Env{Extra: drivers.Commands()}, os.Args)
}
