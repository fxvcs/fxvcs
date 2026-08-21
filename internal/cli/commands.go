package cli

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func coreCommands(env Env) []*cli.Command {
	cmds := []*cli.Command{}
	cmds = append(cmds, checkoutCommands(env)...)
	cmds = append(cmds, lifecycleCommands(env)...)
	cmds = append(cmds, storageCommands(env)...)
	cmds = append(cmds, fivemCommands(env)...)
	cmds = append(cmds, stackCommands(env)...)
	cmds = append(cmds, releaseCommands(env)...)
	cmds = append(cmds, desiredStateCommands(env)...)
	cmds = append(cmds, launcherCommands(env)...)
	cmds = append(cmds, deploymentCommands(env)...)
	cmds = append(cmds, infoCommand(env), statsCommand(env), versionCommand(env), compatCommand(env))
	return cmds
}

// rootHelpTemplate groups commands into the plan's categories in a fixed
// order (urfave/cli sorts categories alphabetically by default).
const rootHelpTemplate = `NAME:
   {{template "helpNameTemplate" .}}

USAGE:
   fxvcs [global options] <command> [command options] [arguments]

VERSION:
   {{.Version}}

COMMANDS:{{range .VisibleCategories}}{{if eq .Name "Daily"}}

   Daily:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "Set up"}}

   Set up:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "Fix"}}

   Fix:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "Sharing"}}

   Sharing:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "FiveM"}}

   FiveM:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "Stacks"}}

   Stacks (repositories → a release → your servers):{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name "More"}}

   More:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}{{range .VisibleCategories}}{{if eq .Name ""}}

   Other:{{range .VisibleCommands}}
     {{join .Names ", "}}{{"\t"}}{{.Usage}}{{end}}{{end}}{{end}}

GLOBAL OPTIONS:{{template "visibleFlagTemplate" .}}

Run "fxvcs <command> --help" for examples. New here? See docs/guide/quickstart.md.
`

type opFunc func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error)

// renderMeta carries invocation facts a renderer may mention.
type renderMeta struct {
	Elapsed time.Duration
	Cmd     *cli.Command
}

type renderFunc func(p *ui.Printer, v any, m renderMeta)

// cmdSpec declares one SDK-backed command. Render is required: a missing
// renderer is a build error, never a struct dump.
type cmdSpec struct {
	Name      string
	Aliases   []string
	Hidden    bool
	Category  string
	Usage     string
	ArgsUsage string
	Examples  []string
	Flags     []cli.Flag
	Op        opFunc
	Render    renderFunc
	// Partial renders a non-nil result even when Op also returned an error
	// (multi-item operations report every outcome); the error still decides
	// the exit code.
	Partial bool
	// After runs after rendering (human mode only) with the same client, for
	// cheap advisory footers such as the untracked-files line of status.
	After func(ctx context.Context, p *ui.Printer, c *sdk.Client, dir string, v any)
}

func examples(lines ...string) string {
	if len(lines) == 0 {
		return ""
	}
	return "Examples:\n   " + strings.Join(lines, "\n   ")
}

// command builds a cli.Command from a spec.
func command(env Env, s cmdSpec) *cli.Command {
	if s.Render == nil {
		panic("cli: command " + s.Name + " has no renderer")
	}
	return &cli.Command{
		Name:         s.Name,
		Aliases:      s.Aliases,
		Hidden:       s.Hidden,
		Category:     s.Category,
		Usage:        s.Usage,
		ArgsUsage:    s.ArgsUsage,
		Description:  examples(s.Examples...),
		Flags:        s.Flags,
		OnUsageError: onUsageError,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			dir, err := cwd(env, cmd)
			if err != nil {
				return err
			}
			c, view, err := client(env, cmd)
			if err != nil {
				return err
			}
			started := time.Now()
			res, err := s.Op(ctx, c, dir, cmd)
			if view != nil {
				view.finish(err)
			}
			if err != nil && (!s.Partial || isNil(res)) {
				return err
			}
			if !isNil(res) {
				meta := renderMeta{Elapsed: time.Since(started), Cmd: cmd}
				if rerr := render(env, cmd, res, func(p *ui.Printer) {
					s.Render(p, res, meta)
					if s.After != nil {
						s.After(ctx, p, c, dir, res)
					}
				}); rerr != nil {
					return rerr
				}
			}
			return err
		},
	}
}

// group builds a command that only holds subcommands.
func group(name, category, usage string, subs ...*cli.Command) *cli.Command {
	return &cli.Command{
		Name:         name,
		Category:     category,
		Usage:        usage,
		Commands:     subs,
		Action:       groupAction,
		OnUsageError: onUsageError,
	}
}

// groupAction runs when a group (or the root) is invoked without a known
// subcommand: an unknown name is a usage error with a suggestion (exit 2);
// no name shows help.
func groupAction(ctx context.Context, cmd *cli.Command) error {
	name := cmd.Args().First()
	if name == "" || name == "help" || name == "h" {
		if name != "" && cmd.Args().Len() > 1 {
			// `fxvcs help <command>` shows that command's help.
			target := cmd.Args().Get(1)
			for _, c := range cmd.Commands {
				if c.HasName(target) {
					return cli.ShowSubcommandHelp(c)
				}
			}
		}
		if cmd.Root() == cmd {
			return cli.ShowRootCommandHelp(cmd)
		}
		return cli.ShowSubcommandHelp(cmd)
	}
	prefix := "fxvcs"
	if cmd.Root() != cmd {
		prefix = "fxvcs " + cmd.Name
	}
	msg := fmt.Sprintf("unknown command '%s'", name)
	if s := suggestCommand(cmd.Commands, name); s != "" {
		msg += fmt.Sprintf(". Did you mean '%s'?", s)
	}
	return usageError{msg: msg, hint: prefix + " --help"}
}

// onUsageError turns flag-parsing mistakes into a usage error (exit 2) with
// a help hint instead of printing the whole help text.
func onUsageError(_ context.Context, cmd *cli.Command, err error, _ bool) error {
	return usageError{msg: err.Error(), hint: cmd.FullName() + " --help"}
}

// suggestCommand returns the closest visible command name by edit distance,
// or "" when nothing is close enough.
func suggestCommand(cmds []*cli.Command, typed string) string {
	best, bestDist := "", 1<<30
	var names []string
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		names = append(names, c.Name)
		names = append(names, c.Aliases...)
	}
	sort.Strings(names)
	for _, n := range names {
		d := levenshtein(strings.ToLower(typed), n)
		if d < bestDist {
			best, bestDist = n, d
		}
	}
	limit := 2
	if len(typed) > 5 {
		limit = 3
	}
	if bestDist > limit {
		return ""
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// isNil reports a nil interface or a typed nil pointer.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

func versionCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "version",
		Category: catMore,
		Usage:    "Show the installed fxvcs version",
		Examples: []string{"fxvcs version", "fxvcs --verbose version   # also lists the file formats this build understands"},
		Op: func(context.Context, *sdk.Client, string, *cli.Command) (any, error) {
			return struct {
				Version    string             `json:"version"`
				APIVersion string             `json:"sdkAPIVersion"`
				Formats    sdk.FormatVersions `json:"formats"`
			}{sdk.Version, sdk.APIVersion, sdk.SupportedFormats()}, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			r := v.(struct {
				Version    string             `json:"version"`
				APIVersion string             `json:"sdkAPIVersion"`
				Formats    sdk.FormatVersions `json:"formats"`
			})
			p.Line("fxvcs %s", r.Version)
			p.Verbosef("sdk api %s", r.APIVersion)
			p.Verbosef("committed formats: %v", r.Formats.CommittedAPIVersions)
			p.Verbosef("placeholder v%d, manifest v%d, asset-index v%d, git-integration g%d, state-db v%d, publication-db v%d",
				r.Formats.PointerFormat, r.Formats.ObjectManifest, r.Formats.AssetIndex,
				r.Formats.GitIntegrationGeneration, r.Formats.HydrationStateSchema, r.Formats.PublicationLedgerSchema)
		},
	})
}

// compatCommand is the only command permitted against a repository whose
// minimumFXVCSVersion exceeds this binary. It exits ExitUpgradeRequired when
// an upgrade is required so CI and wrappers can detect it.
func compatCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "compat",
		Category: catMore,
		Usage:    "Check whether this fxvcs is new enough for this repository (read-only)",
		Examples: []string{"fxvcs compat"},
		Partial:  true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			res, err := c.Compatibility(ctx, sdk.CompatibilityRequest{Path: dir})
			if err != nil {
				return nil, err
			}
			if res.UpgradeRequired != nil {
				e := sdk.NewError(sdk.CodeUpgradeRequired, fmt.Sprintf("this repository needs fxvcs %s or newer (you have %s)", res.UpgradeRequired.RequiredVersion, res.UpgradeRequired.InstalledVersion))
				e.Details = res.UpgradeRequired
				e.Hint = "install a newer fxvcs, then run: fxvcs upgrade --check"
				return res, e
			}
			return res, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.CompatibilityResult)
			if res.UpgradeRequired != nil {
				p.Header("compat", fmt.Sprintf("UPGRADE REQUIRED: this repository needs fxvcs %s or newer, you have %s", res.UpgradeRequired.RequiredVersion, res.UpgradeRequired.InstalledVersion))
				p.Verbosef("reason: %s", res.UpgradeRequired.Reason)
				return
			}
			p.Header("compat", fmt.Sprintf("compatible (installed %s, repository needs %s or newer)", res.InstalledVersion, res.MinimumFXVCSVersion))
			p.Verbosef("repository: %s (%s), git integration g%d", res.RepositoryRoot, res.RepositoryID, res.GitIntegrationVersion)
		},
	})
}
