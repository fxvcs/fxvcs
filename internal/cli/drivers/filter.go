package drivers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cache"
	fxcli "fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/filter/wire"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/sdk"
)

// installedVersion parses the build version once for the Git entry points.
func installedVersion() (format.Version, error) {
	v, err := format.ParseVersion(sdk.Version)
	if err != nil {
		return format.Version{}, sdk.WrapError(sdk.CodeInvalidArgument,
			fmt.Sprintf("installed version %q is not a semantic version (major.minor.patch)", sdk.Version), err)
	}
	return v, nil
}

// processOptions builds filter.ProcessOptions from the process environment.
// The engine backend (wire.Connector) makes clean ingest through app.Repo,
// smudge honour recorded hydration intent, and pre-push consult the ledger.
func processOptions(cmd *cli.Command) (filter.ProcessOptions, error) {
	v, err := installedVersion()
	if err != nil {
		return filter.ProcessOptions{}, err
	}
	cacheDir := os.Getenv(cache.EnvDir)
	opts := filter.ProcessOptions{Installed: v, CacheDir: cacheDir, Connect: wire.Connector(v, cacheDir, cmd.Name)}
	if c := cmd.Root().String("C"); c != "" {
		opts.Cwd = c
	}
	return opts, nil
}

// stdin returns the root command's reader (os.Stdin unless a test injected one).
func stdin(cmd *cli.Command) io.Reader {
	if r := cmd.Root().Reader; r != nil {
		return r
	}
	return os.Stdin
}

// mapDriverError converts engine errors into typed SDK errors so cli.Run picks
// the documented exit code (3 for upgrade-required, 1 otherwise).
func mapDriverError(err error) error {
	return fxcli.Verbatim(mapDriverCode(err))
}

// mapDriverCode assigns the SDK code that selects the exit code.
func mapDriverCode(err error) error {
	if err == nil {
		return nil
	}
	var ue *format.UpgradeRequiredError
	if errors.As(err, &ue) {
		return sdk.WrapError(sdk.CodeUpgradeRequired, "Git integration refused to run: "+ue.Error(), err)
	}
	if errors.Is(err, filter.ErrUnsupportedPointer) {
		return sdk.WrapError(sdk.CodeUpgradeRequired, err.Error(), err)
	}
	if errors.Is(err, filter.ErrObjectUnavailable) {
		return sdk.WrapError(sdk.CodeNotFound, err.Error(), err)
	}
	var pb *filter.PublicationBarrierError
	if errors.As(err, &pb) {
		return sdk.WrapError(sdk.CodeConflict, fmt.Sprintf("%d object(s) pending publication", len(pb.Pending)), err)
	}
	return sdk.WrapError(sdk.CodeGit, err.Error(), err)
}

func init() {
	Register(&cli.Command{
		Name:   "filter-process",
		Usage:  "Git long-running clean/smudge filter (configured as filter.fxvcs.process)",
		Hidden: true,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := processOptions(cmd)
			if err != nil {
				return err
			}
			return mapDriverError(filter.RunProcess(ctx, opts, stdin(cmd), cmd.Root().Writer, cmd.Root().ErrWriter))
		},
	})
	Register(&cli.Command{
		Name:      "clean-filter",
		Usage:     "One-shot clean filter for debugging: reads content on stdin, writes the pointer",
		Hidden:    true,
		ArgsUsage: "<path>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := processOptions(cmd)
			if err != nil {
				return err
			}
			return mapDriverError(filter.RunOneShot(ctx, opts, "clean", cmd.Args().First(), stdin(cmd), cmd.Root().Writer, cmd.Root().ErrWriter))
		},
	})
	Register(&cli.Command{
		Name:      "smudge-filter",
		Usage:     "One-shot smudge filter for debugging: reads a pointer on stdin, writes the content",
		Hidden:    true,
		ArgsUsage: "<path>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := processOptions(cmd)
			if err != nil {
				return err
			}
			return mapDriverError(filter.RunOneShot(ctx, opts, "smudge", cmd.Args().First(), stdin(cmd), cmd.Root().Writer, cmd.Root().ErrWriter))
		},
	})
	Register(&cli.Command{
		Name:      "pre-push",
		Usage:     "Git pre-push hook: refuse to push commits whose objects are not published to every required remote",
		Hidden:    true,
		ArgsUsage: "<remote-name> <remote-url>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := processOptions(cmd)
			if err != nil {
				return err
			}
			return mapDriverError(filter.RunPrePush(ctx, opts, cmd.Args().First(), stdin(cmd), cmd.Root().ErrWriter))
		},
	})
}
