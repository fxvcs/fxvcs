package drivers

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	fxcli "fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/sdk"
)

// mergeConflictError makes cli.Run exit 1 (Git records the path as unmerged)
// with an explanatory message on stderr.
type mergeConflictError struct{ path string }

func (e mergeConflictError) Error() string {
	return fmt.Sprintf("merge-driver: %s: both sides changed the asset; both pointer candidates are kept in the working file (see git ls-files -u)", e.path)
}

func init() {
	Register(&cli.Command{
		Name:      "diff-driver",
		Usage:     "Git external diff for fxvcs pointers (configured as diff.fxvcs.command)",
		Hidden:    true,
		ArgsUsage: "<path> <old-file> <old-hex> <old-mode> <new-file> <new-hex> <new-mode>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			v, err := installedVersion()
			if err != nil {
				return err
			}
			return mapDriverError(filter.RunDiffDriver(cmd.Args().Slice(), v, cmd.Root().Writer))
		},
	})
	Register(&cli.Command{
		Name:      "merge-driver",
		Usage:     "Git merge driver for fxvcs pointers (configured as merge.fxvcs.driver %O %A %B %P)",
		Hidden:    true,
		ArgsUsage: "<base-file> <ours-file> <theirs-file> <path>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			v, err := installedVersion()
			if err != nil {
				return err
			}
			args := cmd.Args().Slice()
			if len(args) < 3 {
				return sdk.NewError(sdk.CodeInvalidArgument, "merge-driver expects %O %A %B [%P]")
			}
			pathname := ""
			if len(args) > 3 {
				pathname = args[3]
			}
			res, err := filter.RunMergeDriver(args[0], args[1], args[2], pathname, v)
			if err != nil {
				return mapDriverError(err)
			}
			if res.Outcome == filter.MergeConflict {
				return fxcli.Verbatim(sdk.WrapError(sdk.CodeConflict, mergeConflictError{path: pathname}.Error(), mergeConflictError{path: pathname}))
			}
			return nil
		},
	})
}
