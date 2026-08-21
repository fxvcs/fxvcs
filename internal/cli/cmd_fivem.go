package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func fivemCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("resources", catFiveM, "Your resource list (.fxvcs/resources.yaml): which resources exist and which start",
			resourcesSyncCommand(env),
			resourcesSetCommand(env),
			resourcesValidateCommand(env),
		),
	}
}

func resourcesSyncCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:  "sync",
		Usage: "Find every resource in this repository and update the resource list (commits nothing)",
		Examples: []string{
			"fxvcs resources sync",
			"fxvcs resources sync --from-cfg server.cfg     # enable the resources your server.cfg starts",
			"fxvcs resources sync --new enabled",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "new", Value: "pending", Usage: "what newly found resources become: pending (decide later), enabled, or disabled"},
			&cli.StringFlag{Name: "from-cfg", Usage: "read this server.cfg (and the files it execs) and enable the resources it starts"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.ResourcesSync(ctx, sdk.ResourcesSyncRequest{Path: dir, NewActivation: cmd.String("new"), FromCfg: cmd.String("from-cfg")})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ResourcesSyncResult)
			state := "unchanged"
			if res.Written {
				state = "updated"
			}
			p.Header("resources sync", fmt.Sprintf("resource list %s (%s)", state, res.CatalogPath))
			var items []ui.Item
			for _, n := range res.Added {
				items = append(items, ui.Item{State: ui.OK, Label: "added", Path: n})
			}
			for _, n := range res.Enabled {
				items = append(items, ui.Item{State: ui.OK, Label: "enabled", Path: n, Note: "started by your server.cfg"})
			}
			for _, n := range res.Updated {
				items = append(items, ui.Item{State: ui.OK, Label: "updated", Path: n})
			}
			for _, n := range res.Missing {
				items = append(items, ui.Item{State: ui.Warn, Label: "missing", Path: n, Note: "folder is gone; restore it or remove the entry"})
			}
			if len(items) > 0 {
				p.Blank()
				p.Items(items)
			}
			if res.Unchanged > 0 {
				p.Line("  %s unchanged", ui.Count(res.Unchanged, "resource"))
			}
			printWarnings(p, res.Warnings)
			if len(res.Pending) > 0 {
				p.Footer(fmt.Sprintf("%s undecided (%s): enable or disable each with fxvcs resources set <path> --activation enabled|disabled", ui.Plural(len(res.Pending), "resource is", "resources are"), strings.Join(res.Pending, ", ")))
			} else if res.Written {
				p.Footer("review .fxvcs/resources.yaml and commit it.")
			}
		},
	})
}

func resourcesSetCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "set",
		Usage:     "Enable or disable a resource and set its start order (commits nothing)",
		ArgsUsage: "<resource-path>",
		Examples: []string{
			"fxvcs resources set chat --activation enabled",
			"fxvcs resources set '[maps]/city-map' --activation enabled --after mapmanager",
			"fxvcs resources set old-hud --activation disabled",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "activation", Usage: "enabled, disabled, or pending (undecided)"},
			&cli.StringFlag{Name: "after", Usage: "comma-separated resource names this one starts after (empty clears)"},
			&cli.StringFlag{Name: "before", Usage: "comma-separated resource names this one starts before (empty clears)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"resources set needs exactly one resource path", "fxvcs resources set <resource-path> --activation enabled|disabled"}
			}
			req := sdk.ResourcesSetRequest{Path: dir, ResourcePath: cmd.Args().First(), Activation: cmd.String("activation")}
			if cmd.IsSet("after") {
				req.After = splitNames(cmd.String("after"))
			}
			if cmd.IsSet("before") {
				req.Before = splitNames(cmd.String("before"))
			}
			return c.ResourcesSet(ctx, req)
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ResourcesSetResult)
			state := "unchanged"
			if res.Written {
				state = "saved"
			}
			p.Header("resources set", fmt.Sprintf("%s is %s (%s)", res.ResourcePath, activationWord(res.Activation), state))
			if r := res.Resource; r != nil {
				if len(r.After) > 0 {
					p.Line("  starts after: %s", strings.Join(r.After, ", "))
				}
				if len(r.Before) > 0 {
					p.Line("  starts before: %s", strings.Join(r.Before, ", "))
				}
				if r.Status != "" && r.Status != "ok" {
					p.Line("  status: %s", r.Status)
				}
			}
			printWarnings(p, res.Warnings)
			if res.Written {
				p.Footer("commit .fxvcs/resources.yaml when you are done.")
			}
		},
	})
}

func activationWord(a string) string {
	switch a {
	case "enabled":
		return "enabled"
	case "disabled":
		return "disabled"
	case "pending":
		return "undecided"
	}
	return a
}

func resourcesValidateCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "validate",
		Usage:    "Check the resource list is complete and decided, as a release would (exit 1 if not)",
		Examples: []string{"fxvcs resources validate", "fxvcs resources validate --known mapmanager,chat"},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "known", Usage: "comma-separated resource names provided elsewhere (other repositories or the server) that start order may reference"},
		},
		Partial: true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			res, verr := c.ResourcesValidate(ctx, sdk.ResourcesValidateRequest{Path: dir, KnownNames: splitNames(cmd.String("known"))})
			if res == nil {
				return nil, verr
			}
			if verr != nil {
				// The problems are rendered from the result; keep the process
				// error short so stderr does not repeat them.
				e := sdk.NewError(sdk.CodeValidation, fmt.Sprintf("the resource list is not ready for release (%s)", ui.Count(len(res.Problems), "problem")))
				e.Hint = "fix the problems above, then run: fxvcs resources validate"
				return res, e
			}
			return res, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ResourcesValidateResult)
			if res.Valid {
				p.Header("resources validate", fmt.Sprintf("resource list is ready for release (%s)", ui.Count(res.Resources, "resource")))
				printWarnings(p, res.Warnings)
				return
			}
			p.Header("resources validate", fmt.Sprintf("NOT READY FOR RELEASE: %s in %s", ui.Count(len(res.Problems), "problem"), ui.Count(res.Resources, "resource")))
			p.Blank()
			byPath := map[string][]sdk.ResourceProblem{}
			var order []string
			for _, pr := range res.Problems {
				if _, ok := byPath[pr.Path]; !ok {
					order = append(order, pr.Path)
				}
				byPath[pr.Path] = append(byPath[pr.Path], pr)
			}
			for _, path := range order {
				title := path
				if title == "" {
					title = "resource list"
				}
				p.Line("%s:", title)
				items := []ui.Item{}
				for _, pr := range byPath[path] {
					items = append(items, ui.Item{State: ui.Fail, Label: "problem", Path: pr.Field, Note: pr.Message})
				}
				p.Items(items)
				if hint := validateHint(path, byPath[path]); hint != "" {
					p.Hint(hint)
				}
			}
			printWarnings(p, res.Warnings)
			p.Footer(ui.Count(len(res.Problems), "problem") + " to fix before this resource list can be released.")
		},
	})
}

// validateHint suggests the command that fixes a resource's problems.
func validateHint(path string, problems []sdk.ResourceProblem) string {
	for _, pr := range problems {
		msg := strings.ToLower(pr.Message + " " + pr.Field)
		switch {
		case strings.Contains(msg, "pending"):
			return fmt.Sprintf("fxvcs resources set %s --activation enabled|disabled", quoteIfNeeded(path))
		case strings.Contains(msg, "undeclared"), strings.Contains(msg, "stale"), strings.Contains(msg, "missing"), strings.Contains(msg, "drift"):
			return "fxvcs resources sync"
		}
	}
	if path == "" {
		return "fxvcs resources sync"
	}
	return ""
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, "[] ") {
		return "'" + s + "'"
	}
	return s
}

func splitNames(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printWarnings(p *ui.Printer, warnings []string) {
	for _, m := range warnings {
		p.Line("  warning: %s", m)
	}
}
