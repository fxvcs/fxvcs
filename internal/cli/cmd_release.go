package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func releaseCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("release", catStack, "Releases: an exact, unchangeable snapshot of a stack, ready to deploy",
			releaseCreateCommand(env),
			releaseListCommand(env),
			releaseInspectCommand(env),
			group("notes", catStack, "Draft release notes", releaseNotesGenerateCommand(env)),
			releaseExportCommand(env),
			releaseImportCommand(env),
		),
	}
}

func releaseCreateCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "create",
		Usage:     "Pin every repository in this stack and write an unchangeable release (commits nothing)",
		ArgsUsage: "<name>",
		Examples: []string{
			"fxvcs release create v1.4.0",
			"fxvcs release create v1.4.0 --pin repo_maps_01=v2.1",
			"fxvcs release create v1.4.0-dev --disable map-editor --variant-of v1.4.0",
			"fxvcs release create v1.4.0 --tag-components   # also tag each repository at its pinned commit",
		},
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "pin", Usage: "use a specific branch, tag, or commit for one repository: <repository-id>=<ref>"},
			&cli.StringSliceFlag{Name: "enable", Usage: "start this resource in this release only (name, or <repository-id>:<path>)"},
			&cli.StringSliceFlag{Name: "disable", Usage: "do not start this resource in this release only"},
			&cli.StringFlag{Name: "variant-of", Usage: "group this release with another one for humans"},
			&cli.StringFlag{Name: "created-by", Usage: "who or what created this release (default: your Git user)"},
			&cli.BoolFlag{Name: "tag-components", Usage: "create a durable tag in every repository so its commit cannot be garbage-collected"},
			&cli.BoolFlag{Name: "skip-storage-check", Usage: "do not check that every asset is already uploaded"},
			&cli.BoolFlag{Name: "allow-unverified-objects", Usage: "continue when storage cannot be reached from this computer"},
		},
		Partial: true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"release create needs exactly one name", "fxvcs release create <name>"}
			}
			res, err := c.ReleaseCreate(ctx, sdk.ReleaseCreateRequest{
				Path: dir, Name: cmd.Args().First(), Pins: cmd.StringSlice("pin"),
				Enable: cmd.StringSlice("enable"), Disable: cmd.StringSlice("disable"),
				VariantOf: cmd.String("variant-of"), CreatedBy: cmd.String("created-by"),
				TagComponents: cmd.Bool("tag-components"), SkipObjectCheck: cmd.Bool("skip-storage-check"),
				AllowUnverifiedObjects: cmd.Bool("allow-unverified-objects"),
			})
			if err != nil {
				if res == nil || len(res.Problems) == 0 {
					// Nothing worth rendering: let the error speak alone.
					return nil, err
				}
				e := sdk.NewError(sdk.CodeValidation, fmt.Sprintf("this release cannot be created yet (%s)", ui.Count(len(res.Problems), "problem")))
				e.Hint = "fix the problems above, then run: fxvcs release create " + cmd.Args().First()
				return res, e
			}
			return res, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ReleaseCreateResult)
			if len(res.Problems) > 0 {
				p.Header("release create", fmt.Sprintf("NOT CREATED: %s", ui.Count(len(res.Problems), "problem")))
				p.Blank()
				renderProblems(p, res.Problems)
				printWarnings(p, res.Warnings)
				return
			}
			what := "written"
			if !res.Written {
				what = "already present, unchanged"
			}
			p.Header("release create", fmt.Sprintf("%s %s — %s start, from %s",
				res.Name, what, ui.Count(res.Enabled, "resource"), ui.Count(len(res.Components), "repository")))
			p.Blank()
			rows := [][]string{}
			for _, c := range res.Components {
				note := ""
				if c.DurablePin != nil && !*c.DurablePin {
					note = "not on any branch or tag"
				}
				rows = append(rows, []string{c.RepositoryName, ui.ShortID(c.Commit), c.Mount, ui.Count(c.Assets, "asset"), note})
			}
			p.Table(rows)
			if len(res.Tags) > 0 {
				p.Blank()
				p.Line("  tagged %s in each repository", res.Tags[0].Tag[:strings.LastIndex(res.Tags[0].Tag, "/")+1]+"…")
			}
			if res.Change != nil {
				renderChange(p, res.Change)
			}
			p.Verbosef("release digest %s, source set %s", ui.ShortID(res.Digest), ui.ShortID(res.SourceSetDigest))
			printWarnings(p, res.Warnings)
			if res.Written {
				p.Footer("review " + res.File + " and commit it, then point a server at it: fxvcs target set <server> --release " + res.Name)
			}
		},
	})
}

func renderChange(p *ui.Printer, ch *sdk.ReleaseChange) {
	if ch == nil || ch.From == "" {
		return
	}
	p.Blank()
	if ch.SameSource {
		p.Line("  same files as %s — only which resources start has changed", ch.From)
	} else {
		p.Line("  changes since %s:", ch.From)
	}
	items := []ui.Item{}
	for _, c := range ch.Components {
		switch c.Change {
		case "updated":
			note := ui.ShortID(c.FromCommit) + " → " + ui.ShortID(c.ToCommit)
			if c.AssetsChanged {
				note += ", assets changed"
			}
			items = append(items, ui.Item{State: ui.OK, Label: "updated", Path: c.RepositoryName, Note: note})
		case "added":
			items = append(items, ui.Item{State: ui.OK, Label: "added", Path: c.RepositoryName})
		case "removed":
			items = append(items, ui.Item{State: ui.Warn, Label: "removed", Path: c.RepositoryName})
		}
	}
	for _, r := range ch.Resources {
		switch r.Change {
		case "added":
			items = append(items, ui.Item{State: ui.OK, Label: "new resource", Path: r.Name, Note: r.To})
		case "removed":
			items = append(items, ui.Item{State: ui.Warn, Label: "gone", Path: r.Name})
		case "activation":
			items = append(items, ui.Item{State: ui.Plain, Label: "now " + r.To, Path: r.Name, Note: "was " + r.From})
		}
	}
	if len(items) > 0 {
		p.Items(items)
	}
}

func releaseListCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "list",
		Usage:    "List every release of this stack and which servers run it",
		Examples: []string{"fxvcs release list"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			return c.ReleaseList(ctx, sdk.ReleaseListRequest{Path: dir})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ReleaseListResult)
			if len(res.Releases) == 0 {
				p.Header("release list", "no releases yet")
				p.Footer("create one: fxvcs release create <name>")
				return
			}
			p.Header("release list", ui.Count(len(res.Releases), "release"))
			p.Blank()
			rows := [][]string{}
			for _, r := range res.Releases {
				used := ""
				if len(r.UsedBy) > 0 {
					used = "running on " + strings.Join(r.UsedBy, ", ")
				}
				rows = append(rows, []string{r.Name, r.CreatedAt, ui.Count(r.Components, "repository"),
					fmt.Sprintf("%d start", r.Enabled), used})
			}
			p.Table(rows)
			p.Footer("see one in detail: fxvcs release inspect <name>")
		},
	})
}

func releaseInspectCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "inspect",
		Usage:     "Show exactly what a release pins and what it starts",
		ArgsUsage: "<name>",
		Examples: []string{
			"fxvcs release inspect v1.4.0",
			"fxvcs release inspect v1.4.0 --since v1.3.0",
			"fxvcs release inspect v1.4.0 --all   # include resources that do not start",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: "also show what changed since this release"},
			&cli.BoolFlag{Name: "all", Usage: "list resources that do not start as well"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"release inspect needs exactly one release name", "fxvcs release inspect <name>"}
			}
			return c.ReleaseInspect(ctx, sdk.ReleaseInspectRequest{Path: dir, Name: cmd.Args().First(), CompareTo: cmd.String("since")})
		},
		Render: func(p *ui.Printer, v any, m renderMeta) {
			res := v.(*sdk.ReleaseInspectResult)
			p.Header("release inspect", fmt.Sprintf("%s — %s start, %s do not, from %s",
				res.Name, ui.Count(res.Enabled, "resource"), ui.Count(res.Disabled, "resource"), ui.Count(len(res.Components), "repository")))
			p.Blank()
			rows := [][]string{}
			for _, c := range res.Components {
				rows = append(rows, []string{c.RepositoryName, ui.ShortID(c.Commit), c.Mount, descriptorNote(c)})
			}
			p.Table(rows)
			renderPlan(p, res.Resources, m.Cmd.Bool("all"))
			if len(res.SameSourceAs) > 0 {
				p.Blank()
				p.Line("  same files as: %s", strings.Join(res.SameSourceAs, ", "))
			}
			if res.VariantOf != "" {
				p.Line("  grouped with: %s", res.VariantOf)
			}
			renderChange(p, res.Change)
			p.Verbosef("created %s by %s; digest %s; source set %s", res.CreatedAt, res.CreatedBy, ui.ShortID(res.Digest), ui.ShortID(res.SourceSetDigest))
			if !m.Cmd.Bool("all") && res.Disabled > 0 {
				p.Footer(fmt.Sprintf("%s would not start; see them with --all.", ui.Count(res.Disabled, "resource")))
			}
		},
	})
}

func descriptorNote(c sdk.ReleaseComponentInfo) string {
	if c.DescriptorFrom == "stack-override" {
		return "layout set by this stack"
	}
	return ""
}

func releaseNotesGenerateCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "generate",
		Usage:     "Draft Markdown release notes from what actually changed",
		ArgsUsage: "<name>",
		Examples: []string{
			"fxvcs release notes generate v1.4.0",
			"fxvcs release notes generate v1.4.0 --since v1.3.0 > NOTES.md",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "since", Usage: "compare against this release (default: the one before it)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"release notes generate needs exactly one release name", "fxvcs release notes generate <name>"}
			}
			return c.ReleaseNotes(ctx, sdk.ReleaseNotesRequest{Path: dir, Name: cmd.Args().First(), Since: cmd.String("since")})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ReleaseNotesResult)
			// The draft is the output: it is meant to be redirected into a file
			// or pasted into a changelog, so it goes to stdout unadorned.
			fmt.Fprint(p.Writer(), res.Markdown)
		},
	})
}

func releaseExportCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "export",
		Usage:     "Write a release out as a portable file, to move it to another stack",
		ArgsUsage: "<name>",
		Examples: []string{
			"fxvcs release export v1.4.0 --output v1.4.0.yaml",
			"fxvcs release export v1.4.0 > v1.4.0.yaml",
		},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write to this file instead of standard output"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"release export needs exactly one release name", "fxvcs release export <name>"}
			}
			res, err := c.ReleaseExport(ctx, sdk.ReleaseExportRequest{Path: dir, Name: cmd.Args().First()})
			if err != nil {
				return nil, err
			}
			if out := cmd.String("output"); out != "" {
				if err := os.WriteFile(out, res.Document, 0o644); err != nil { //nolint:gosec // a release record is reviewable, non-secret YAML
					return nil, err
				}
				return exportView{Name: res.Name, Digest: res.Digest, Bytes: res.Bytes, File: out}, nil
			}
			if _, err := env.Stdout.Write(res.Document); err != nil {
				return nil, err
			}
			return exportView{Name: res.Name, Digest: res.Digest, Bytes: res.Bytes}, nil
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(exportView)
			if res.File == "" {
				return // the document itself already went to stdout
			}
			p.Header("release export", fmt.Sprintf("%s written to %s (%s)", res.Name, res.File, ui.Bytes(int64(res.Bytes))))
			p.Verbosef("release digest %s", res.Digest)
			p.Footer("import it elsewhere with: fxvcs release import " + res.File)
		},
	})
}

// exportView is what export reports; the document itself is not repeated in
// the result so `--json` stays small and stdout stays usable as a pipe.
type exportView struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
	File   string `json:"file,omitempty"`
}

func releaseImportCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "import",
		Usage:     "Read a portable release file into this stack (commits nothing)",
		ArgsUsage: "<file>",
		Examples:  []string{"fxvcs release import v1.4.0.yaml", "cat v1.4.0.yaml | fxvcs release import -"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"release import needs exactly one file (or - for standard input)", "fxvcs release import <file>"}
			}
			data, err := readImport(env, cmd.Args().First())
			if err != nil {
				return nil, err
			}
			return c.ReleaseImport(ctx, sdk.ReleaseImportRequest{Path: dir, Document: data})
		},
		Render: func(p *ui.Printer, v any, _ renderMeta) {
			res := v.(*sdk.ReleaseImportResult)
			what := "imported"
			if !res.Written {
				what = "already here, unchanged"
			}
			p.Header("release import", fmt.Sprintf("%s %s (%s)", res.Name, what, res.File))
			printWarnings(p, res.Warnings)
			p.Verbosef("release digest %s", res.Digest)
			if res.Written {
				p.Footer("review " + res.File + " and commit it.")
			}
		},
	})
}

func readImport(env Env, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(env.Stdin)
	}
	return os.ReadFile(path) //nolint:gosec // the user names the file to import
}
