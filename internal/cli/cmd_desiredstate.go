package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func desiredStateCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("target", catStack, "Your servers: which release each one should be running",
			targetSetCommand(env),
			targetShowCommand(env),
			targetListCommand(env),
		),
		group("environment", catStack, "Environments: non-secret settings shared by servers (dev, prd, …)",
			environmentSetCommand(env),
			environmentShowCommand(env),
			environmentListCommand(env),
		),
	}
}

func targetSetCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "set",
		Usage:     "Choose which release a server should run (commits nothing)",
		ArgsUsage: "<server-name>",
		Examples: []string{
			"fxvcs target set prd --release v1.4.0",
			"fxvcs target set prd --release v1.3.0        # rolling back is the same command",
			"fxvcs target set dev --release v1.4.0 --environment dev --agent agent-host-a --runtime fxserver-dev --secret-provider envfile",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "release", Usage: "the release this server should run"},
			&cli.StringFlag{Name: "environment", Usage: "the environment it uses (required when creating the server)"},
			&cli.StringFlag{Name: "agent", Usage: "which agent looks after it (required when creating the server)"},
			&cli.StringFlag{Name: "runtime", Usage: "which FXServer installation it uses (required when creating the server)"},
			&cli.StringFlag{Name: "secret-provider", Usage: "where its secrets come from: env, envfile, or fxvcs-cloud (required when creating the server)"},
			&cli.IntFlag{Name: "keep", Usage: "how many past releases to keep on the server for rollback"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"target set needs exactly one server name", "fxvcs target set <server-name> --release <release>"}
			}
			return c.TargetSet(ctx, sdk.TargetSetRequest{
				Path: dir, Name: cmd.Args().First(), Release: cmd.String("release"),
				Environment: cmd.String("environment"), AgentID: cmd.String("agent"),
				RuntimeRef: cmd.String("runtime"), Keep: cmd.Int("keep"), SecretProvider: cmd.String("secret-provider"),
			})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.Target)
			switch {
			case res.Created:
				p.Header("target set", fmt.Sprintf("created server %q, set to %s", res.Name, res.Release))
			case !res.Changed:
				p.Header("target set", fmt.Sprintf("%s was already set to %s", res.Name, res.Release))
			case res.PreviousRelease != "" && res.PreviousRelease != res.Release:
				p.Header("target set", fmt.Sprintf("%s: %s → %s", res.Name, res.PreviousRelease, res.Release))
			default:
				p.Header("target set", fmt.Sprintf("%s updated", res.Name))
			}
			renderTargetBody(p, res)
			printWarnings(p, res.Warnings)
			if res.Changed {
				p.Footer("review " + res.File + " and commit it — the agent picks it up from Git.")
			}
		},
	})
}

func renderTargetBody(p *ui.Printer, t *sdk.Target) {
	p.Blank()
	p.Line("  release:     %s", t.Release)
	if t.Enabled > 0 {
		p.Line("               starts %s", ui.Count(t.Enabled, "resource"))
	}
	p.Line("  environment: %s", t.Environment)
	p.Line("  agent:       %s", t.AgentID)
	p.Line("  keeps:       %s for rollback", ui.Count(t.Keep, "past release"))
	p.Verbosef("authority %s, runtime %s, secrets from %s (revision %d)", t.Authority, t.RuntimeRef, t.SecretProvider, t.EnvironmentRevision)
}

func targetShowCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "show",
		Usage:     "Show which release a server is set to run",
		ArgsUsage: "<server-name>",
		Examples:  []string{"fxvcs target show prd"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"target show needs exactly one server name", "fxvcs target show <server-name>"}
			}
			return c.TargetShow(ctx, sdk.TargetShowRequest{Path: dir, Name: cmd.Args().First()})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.Target)
			p.Header("target show", fmt.Sprintf("%s should be running %s", res.Name, res.Release))
			renderTargetBody(p, res)
			printWarnings(p, res.Warnings)
			p.Footer("change it with: fxvcs target set " + res.Name + " --release <release>")
		},
	})
}

func targetListCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "list",
		Usage:    "List your servers and the release each should run",
		Examples: []string{"fxvcs target list"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			return c.TargetList(ctx, sdk.TargetListRequest{Path: dir})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.TargetListResult)
			if len(res.Targets) == 0 {
				p.Header("target list", "no servers yet")
				p.Footer("add one: fxvcs target set <server-name> --release <release> --environment <env> --agent <agent> --runtime <runtime> --secret-provider envfile")
				return
			}
			p.Header("target list", ui.Count(len(res.Targets), "server"))
			p.Blank()
			rows := [][]string{}
			for _, t := range res.Targets {
				note := ""
				if !t.ReleaseExists {
					note = "release record missing"
				}
				rows = append(rows, []string{t.Name, t.Release, t.Environment, fmt.Sprintf("%d start", t.Enabled), note})
			}
			p.Table(rows)
		},
	})
}

func environmentSetCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "set",
		Usage:     "Create or update an environment's non-secret settings (commits nothing)",
		ArgsUsage: "<name>",
		Examples: []string{
			"fxvcs environment set prd --input hostname='Example Production'",
			"fxvcs environment set dev --input hostname=Dev --secret-scope dev",
			"fxvcs environment set dev --input hostname=      # remove a setting",
		},
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "input", Usage: "a non-secret setting, key=value (empty value removes it). Never put passwords or keys here."},
			&cli.StringFlag{Name: "secret-scope", Usage: "which secret scope this environment reads (default: its name)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"environment set needs exactly one name", "fxvcs environment set <name> --input key=value"}
			}
			return c.EnvironmentSet(ctx, sdk.EnvironmentSetRequest{
				Path: dir, Name: cmd.Args().First(), Inputs: cmd.StringSlice("input"), SecretScope: cmd.String("secret-scope"),
			})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.Environment)
			what := "unchanged"
			switch {
			case res.Created:
				what = "created"
			case res.Changed:
				what = "updated"
			}
			p.Header("environment set", fmt.Sprintf("%s %s (revision %d)", res.Name, what, res.Revision))
			renderEnvironmentBody(p, res)
			if res.Changed {
				p.Footer("review " + res.File + " and commit it.")
			}
		},
	})
}

func renderEnvironmentBody(p *ui.Printer, e *sdk.Environment) {
	if len(e.Inputs) > 0 {
		p.Blank()
		keys := make([]string, 0, len(e.Inputs))
		for k := range e.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows := [][]string{}
		for _, k := range keys {
			rows = append(rows, []string{k, e.Inputs[k]})
		}
		p.Table(rows)
	}
	p.Blank()
	p.Line("  secrets come from the %q scope; their values are never stored here", e.SecretScope)
	if len(e.UsedBy) > 0 {
		p.Line("  used by: %s", strings.Join(e.UsedBy, ", "))
	}
}

func environmentShowCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "show",
		Usage:     "Show an environment's non-secret settings",
		ArgsUsage: "<name>",
		Examples:  []string{"fxvcs environment show prd"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"environment show needs exactly one name", "fxvcs environment show <name>"}
			}
			return c.EnvironmentShow(ctx, sdk.EnvironmentShowRequest{Path: dir, Name: cmd.Args().First()})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.Environment)
			p.Header("environment show", fmt.Sprintf("%s (revision %d, %s)", res.Name, res.Revision, ui.Count(len(res.Inputs), "setting")))
			renderEnvironmentBody(p, res)
		},
	})
}

func environmentListCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "list",
		Usage:    "List the environments in this stack",
		Examples: []string{"fxvcs environment list"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			return c.EnvironmentList(ctx, sdk.EnvironmentListRequest{Path: dir})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.EnvironmentListResult)
			if len(res.Environments) == 0 {
				p.Header("environment list", "no environments yet")
				p.Footer("add one: fxvcs environment set <name>")
				return
			}
			p.Header("environment list", ui.Count(len(res.Environments), "environment"))
			p.Blank()
			rows := [][]string{}
			for _, e := range res.Environments {
				rows = append(rows, []string{e.Name, fmt.Sprintf("revision %d", e.Revision),
					ui.Count(len(e.Inputs), "setting"), strings.Join(e.UsedBy, ", ")})
			}
			p.Table(rows)
		},
	})
}
