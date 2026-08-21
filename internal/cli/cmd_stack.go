package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

// catStack is the command category for the coordination repository: the
// place where several resource repositories become one deployable stack,
// which may then be deployed to any number of servers.
const catStack = "Stacks"

func stackCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("stack", catStack, "Your stack: the resource repositories bundled into one deployable unit",
			stackInitCommand(env),
			stackAddCommand(env),
			stackSyncCommand(env),
			stackStatusCommand(env),
			stackValidateCommand(env),
			stackRescanCommand(env),
		),
		group("runtime-resources", catStack, "Resources a server provides itself, or that someone installs outside fxvcs",
			runtimeResourcesSyncCommand(env),
		),
	}
}

func stackInitCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:  "init",
		Usage: "Set this Git repository up as a stack (commits nothing)",
		Examples: []string{
			"fxvcs stack init",
			"fxvcs stack init --name my-server",
			"fxvcs stack init --adopt        # keep an fxvcs.yaml that is already here",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "a name for this stack (default: the folder name)"},
			&cli.StringFlag{Name: "profile", Value: "fivem", Usage: "fivem or generic"},
			&cli.BoolFlag{Name: "adopt", Usage: "use the fxvcs.yaml already in this folder instead of failing"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.StackInit(ctx, sdk.StackInitRequest{
				Path: dir, Name: cmd.String("name"), Profile: cmd.String("profile"), Adopt: cmd.Bool("adopt"),
			})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackInitResult)
			what := "created"
			if res.Adopted {
				what = "adopted"
			}
			p.Header("stack init", fmt.Sprintf("%s the stack %q (%s)", what, res.Name, res.File))
			p.Verbosef("stack id: %s, profile: %s", res.StackID, res.Profile)
			p.Footer("add your first resource repository: fxvcs stack add <git-url>")
		},
	})
}

func stackAddCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "add",
		Usage:     "Add a resource repository to this stack and choose where it installs (commits nothing)",
		ArgsUsage: "<git-url>",
		Examples: []string{
			"fxvcs stack add git@github.com:my-team/maps.git",
			"fxvcs stack add https://github.com/my-team/tools.git --ref main",
			"fxvcs stack add ... --alias community-tools   # when two repositories share a name",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "ref", Usage: "branch or tag to follow (default: the repository's default branch)"},
			&cli.StringFlag{Name: "mount", Usage: "where its resources install, for example resources/[maps] (default: proposed for you)"},
			&cli.StringFlag{Name: "alias", Usage: "a different display name, when another repository already uses this one"},
			&cli.BoolFlag{Name: "optional", Usage: "releases may be created without this repository"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"stack add needs exactly one Git URL", "fxvcs stack add <git-url>"}
			}
			return c.StackAdd(ctx, sdk.StackAddRequest{
				Path: dir, Remote: cmd.Args().First(), Ref: cmd.String("ref"),
				Mount: cmd.String("mount"), Alias: cmd.String("alias"), Optional: cmd.Bool("optional"),
			})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackAddResult)
			c := res.Component
			p.Header("stack add", fmt.Sprintf("added %s — installs at %s", c.Name, c.Mount))
			p.Blank()
			p.Line("  %s", describeLayout(c.Descriptor))
			if c.Resources > 0 {
				p.Line("  %s in its resource list", ui.Count(c.Resources, "resource"))
			}
			if !c.Required {
				p.Line("  optional: releases can be created without it")
			}
			p.Verbosef("repository %s at %s (%s)", c.RepositoryID, ui.ShortID(c.Commit), c.Remote)
			printWarnings(p, res.Warnings)
			p.Footer("review " + res.File + " and commit it, then: fxvcs stack validate")
		},
	})
}

func describeLayout(d sdk.ComponentDescriptor) string {
	where := d.SourceRoot
	if where == "" || where == "." {
		where = "the repository root"
	}
	if d.Layout == "single" {
		return fmt.Sprintf("one resource named %q in %s", d.ResourceName, where)
	}
	return "a folder of resources in " + where
}

func stackSyncCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "sync",
		Usage:     "Download the latest history of every resource repository in this stack",
		ArgsUsage: "[repository-id...]",
		Examples:  []string{"fxvcs stack sync", "fxvcs stack sync repo_maps_01"},
		Partial:   true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.StackSync(ctx, sdk.StackSyncRequest{Path: dir, Components: cmd.Args().Slice()})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackSyncResult)
			p.Header("stack sync", fmt.Sprintf("%s checked", ui.Count(len(res.Components), "repository")))
			var items []ui.Item
			for _, c := range res.Components {
				switch {
				case c.Error != "":
					items = append(items, ui.Item{State: ui.Fail, Label: "failed", Path: c.RepositoryID, Note: c.Error})
				case c.Updated:
					items = append(items, ui.Item{State: ui.OK, Label: "updated", Path: c.RepositoryID, Note: ui.ShortID(c.Commit)})
				default:
					items = append(items, ui.Item{State: ui.Plain, Label: "up to date", Path: c.RepositoryID, Note: ui.ShortID(c.Commit)})
				}
			}
			if len(items) > 0 {
				p.Blank()
				p.Items(items)
			}
			p.Footer(fmt.Sprintf("%d updated, %d unchanged, %d failed.", res.Updated, res.Fetched-res.Updated, res.Failed))
		},
	})
}

func stackStatusCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "status",
		Usage:    "Show the repositories in this stack, what they are pinned to, and what has moved on",
		Examples: []string{"fxvcs stack status"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			return c.StackStatus(ctx, sdk.StackStatusRequest{Path: dir})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackStatusResult)
			p.Header("stack status", fmt.Sprintf("stack %q — %s", res.Name, ui.Count(len(res.Components), "resource repository")))
			p.Blank()
			var items []ui.Item
			drift := 0
			for _, c := range res.Components {
				name := c.Name
				if name == "" {
					name = c.RepositoryID
				}
				switch {
				case !c.Fetched:
					items = append(items, ui.Item{State: ui.Warn, Label: "not downloaded", Path: name, Note: "fxvcs stack sync"})
				case c.Error != "":
					items = append(items, ui.Item{State: ui.Fail, Label: "problem", Path: name, Note: c.Error})
				case c.Drifted:
					drift++
					items = append(items, ui.Item{State: ui.Warn, Label: "newer than release", Path: name,
						Note: fmt.Sprintf("%s now, %s in %s", ui.ShortID(c.Commit), ui.ShortID(c.ReleasedCommit), res.LatestRelease)})
				default:
					items = append(items, ui.Item{State: ui.OK, Label: "ready", Path: name, Note: c.Mount})
				}
			}
			p.Items(items)
			p.Blank()
			p.Line("  %s, %s, %s",
				ui.Count(res.Releases, "release"), ui.Count(len(res.Environments), "environment"), ui.Count(len(res.Targets), "server"))
			if res.LatestRelease != "" {
				p.Line("  newest release: %s", res.LatestRelease)
			}
			p.Verbosef("stack id: %s (%s)", res.StackID, res.Root)
			switch {
			case drift > 0:
				p.Footer(fmt.Sprintf("%s changed since the last release: fxvcs release create <name>", ui.Plural(drift, "repository has", "repositories have")))
			default:
				p.Footer("check everything fits together: fxvcs stack validate")
			}
		},
	})
}

func stackValidateCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "validate",
		Usage:    "Check every repository fits together and show exactly which resources would start (exit 1 if not)",
		Examples: []string{"fxvcs stack validate", "fxvcs --json stack validate"},
		Partial:  true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			res, verr := c.StackValidate(ctx, sdk.StackValidateRequest{Path: dir})
			if res == nil {
				return nil, verr
			}
			if verr != nil {
				e := sdk.NewError(sdk.CodeValidation, fmt.Sprintf("this stack is not ready for a release (%s)", ui.Count(len(res.Problems), "problem")))
				e.Hint = "fix the problems above, then run: fxvcs stack validate"
				return res, e
			}
			return res, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackValidateResult)
			if res.Valid {
				p.Header("stack validate", fmt.Sprintf("ready for a release — %s would start, %s would not",
					ui.Count(res.Enabled, "resource"), ui.Count(res.Disabled, "resource")))
				renderPlan(p, res.Resources, false)
				printWarnings(p, res.Warnings)
				p.Footer("create the release: fxvcs release create <name>")
				return
			}
			p.Header("stack validate", fmt.Sprintf("NOT READY: %s", ui.Count(len(res.Problems), "problem")))
			p.Blank()
			renderProblems(p, res.Problems)
			printWarnings(p, res.Warnings)
			p.Footer(ui.Count(len(res.Problems), "problem") + " to fix before a release can be created.")
		},
	})
}

// renderPlan prints the start order. Only enabled resources are listed by
// default: "what starts" is the question people actually ask, and a long list
// of disabled entries buries the answer.
func renderPlan(p *ui.Printer, plan []sdk.PlannedResource, all bool) {
	if len(plan) == 0 {
		return
	}
	p.Blank()
	rows := [][]string{}
	for _, r := range plan {
		if !all && r.Activation != "enabled" {
			continue
		}
		where := r.Mount
		if where == "" {
			where = sourceWord(r.Source)
		}
		state := "starts"
		if r.Activation != "enabled" {
			state = "off"
		}
		rows = append(rows, []string{fmt.Sprintf("%2d.", r.EnsureOrder+1), state, r.Name, where, provenanceWord(r.ActivationSource)})
	}
	if len(rows) == 0 {
		p.Line("  nothing would start")
		return
	}
	p.Table(rows)
}

func sourceWord(s string) string {
	switch s {
	case "runtime":
		return "from the server itself"
	case "external":
		return "installed outside fxvcs"
	}
	return ""
}

func provenanceWord(s string) string {
	switch s {
	case "component":
		return ""
	case "runtime-catalog":
		return ""
	case "coordination-override":
		return "(set by this stack)"
	case "release-override":
		return "(set for this release only)"
	}
	return s
}

func renderProblems(p *ui.Printer, problems []sdk.CompositionProblem) {
	byComponent := map[string][]sdk.CompositionProblem{}
	var order []string
	for _, pr := range problems {
		if _, ok := byComponent[pr.Component]; !ok {
			order = append(order, pr.Component)
		}
		byComponent[pr.Component] = append(byComponent[pr.Component], pr)
	}
	for _, comp := range order {
		title := comp
		if title == "" {
			title = "this stack"
		}
		p.Line("%s:", title)
		items := []ui.Item{}
		for _, pr := range byComponent[comp] {
			items = append(items, ui.Item{State: ui.Fail, Label: "problem", Path: pr.Field, Note: pr.Message})
		}
		p.Items(items)
	}
}

func stackRescanCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "rescan",
		Usage:     "Re-check how each repository is laid out and report anything that no longer fits (changes nothing)",
		ArgsUsage: "[repository-id...]",
		Examples: []string{
			"fxvcs stack rescan",
			"fxvcs stack rescan repo_maps_01 --accept   # record the new layout after reviewing it",
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "accept", Usage: "record the detected layout and install path (review the report first)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.StackRescan(ctx, sdk.StackRescanRequest{Path: dir, Components: cmd.Args().Slice(), Accept: cmd.Bool("accept")})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.StackRescanResult)
			disagree := 0
			for _, f := range res.Findings {
				if !f.Agrees {
					disagree++
				}
			}
			if disagree == 0 {
				p.Header("stack rescan", fmt.Sprintf("every repository still matches how it is set up (%s)", ui.Count(len(res.Findings), "repository")))
				return
			}
			p.Header("stack rescan", fmt.Sprintf("%s no longer match how they are set up", ui.Plural(disagree, "1 repository", fmt.Sprintf("%d repositories", disagree))))
			p.Blank()
			for _, f := range res.Findings {
				if f.Agrees {
					continue
				}
				p.Line("%s:", f.Name)
				items := []ui.Item{}
				for _, n := range f.Notes {
					state := ui.Warn
					if f.Updated {
						state = ui.OK
					}
					items = append(items, ui.Item{State: state, Label: "changed", Note: n})
				}
				if f.ProposedMount != "" && f.ProposedMount != f.Mount {
					items = append(items, ui.Item{State: ui.Plain, Label: "install path", Path: f.Mount, Note: "would become " + f.ProposedMount})
				}
				p.Items(items)
			}
			if res.Changed {
				p.Footer("review " + res.File + " and commit it — this changes where files land on your servers.")
			} else {
				p.Footer("if these changes are intended: fxvcs stack rescan --accept")
			}
		},
	})
}

func runtimeResourcesSyncCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "sync",
		Usage:    "Propose resources a server.cfg starts that no repository in this stack provides (commits nothing)",
		Examples: []string{"fxvcs runtime-resources sync --from-cfg /path/to/server.cfg"},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "from-cfg", Required: true, Usage: "the server.cfg to read (and the files it execs)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.RuntimeResourcesSync(ctx, sdk.RuntimeResourcesSyncRequest{Path: dir, FromCfg: cmd.String("from-cfg")})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.RuntimeResourcesSyncResult)
			if len(res.Proposed) == 0 {
				p.Header("runtime-resources sync", fmt.Sprintf("nothing new — every resource your cfg starts is already accounted for (%s read)", ui.Count(len(res.Files), "file")))
				printWarnings(p, res.Warnings)
				return
			}
			p.Header("runtime-resources sync", fmt.Sprintf("%s your cfg starts that no repository in this stack provides", ui.Count(len(res.Proposed), "resource")))
			p.Blank()
			items := []ui.Item{}
			for _, n := range res.Proposed {
				items = append(items, ui.Item{State: ui.Warn, Label: "undecided", Path: n, Note: "who installs this? edit " + res.File})
			}
			p.Items(items)
			if len(res.Matched) > 0 {
				p.Blank()
				p.Line("  already provided by your repositories: %s", strings.Join(res.Matched, ", "))
			}
			printWarnings(p, res.Warnings)
			p.Footer("edit " + res.File + ": set each one to enabled or disabled, then commit it.")
		},
	})
}
