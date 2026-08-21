package cli

import (
	"context"

	cli "github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func launcherCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("launcher", catFix, "Install or inspect the stable fxvcs helper used by Git",
			command(env, cmdSpec{
				Name:     "ensure",
				Usage:    "Install the current fxvcs build when it is newer than the shared Git launcher",
				Examples: []string{"fxvcs launcher ensure", "fxvcs launcher ensure --repair-equal-version"},
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "path", Usage: "custom absolute launcher destination (default: the per-user platform location)"},
					&cli.BoolFlag{Name: "repair-equal-version", Usage: "replace different equal-version bytes with this trusted build"},
				},
				Op: func(ctx context.Context, c *sdk.Client, _ string, cmd *cli.Command) (any, error) {
					return c.LauncherEnsure(ctx, sdk.LauncherEnsureRequest{TargetPath: cmd.String("path"), RepairEqualVersion: cmd.Bool("repair-equal-version")})
				},
				Render: renderLauncherEnsure,
			}),
		),
	}
}

func renderLauncherEnsure(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.LauncherEnsureResult)
	if res.Launcher == nil {
		return
	}
	note := string(res.Launcher.Outcome)
	if p.Verbose() {
		note += " at " + res.Launcher.Path
	}
	p.Items([]ui.Item{{State: ui.OK, Label: "ready", Path: "Git driver (fxvcs launcher)", Note: note}})
	if res.Legacy != nil && p.Verbose() {
		p.Verbosef("  legacy compatibility: %s at %s", res.Legacy.Outcome, res.Legacy.Path)
	}
}
