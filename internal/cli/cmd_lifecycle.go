package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func lifecycleCommands(env Env) []*cli.Command {
	return []*cli.Command{
		command(env, cmdSpec{
			Name:     "init",
			Category: catSetup,
			Usage:    "Set up this Git repository for fxvcs (writes .fxvcs/ and .gitattributes; commits nothing)",
			Examples: []string{
				"fxvcs init --profile fivem",
				"fxvcs init --profile fivem --source-root resources --layout collection",
				"fxvcs init --profile fivem --remote-name primary --remote-path 'D:\\fxvcs-storage'",
			},
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "name", Usage: "display name for the repository (default: the folder name)"},
				&cli.StringFlag{Name: "profile", Value: "generic", Usage: "'fivem' for a FiveM server or resource repository, 'generic' otherwise"},
				&cli.StringFlag{Name: "source-root", Usage: "FiveM: folder that holds your resources (default: found automatically)"},
				&cli.StringFlag{Name: "layout", Usage: "FiveM: 'single' (this repository is one resource) or 'collection' (many resources); required with --source-root"},
				&cli.StringFlag{Name: "resource-name", Usage: "FiveM: the resource name when --layout is single"},
				&cli.StringFlag{Name: "remote-name", Usage: "also declare a shared storage with this name (required before push)"},
				&cli.StringFlag{Name: "remote-path", Usage: "absolute folder of the storage named by --remote-name on this computer"},
				&cli.StringFlag{Name: "launcher", Usage: "absolute path where the fxvcs launcher for Git is installed (default: per-user location)"},
			},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				return c.Init(ctx, sdk.InitRequest{
					Path: dir, Name: cmd.String("name"), Profile: cmd.String("profile"),
					SourceRoot: cmd.String("source-root"), Layout: cmd.String("layout"), ResourceName: cmd.String("resource-name"),
					RemoteName: cmd.String("remote-name"), RemotePath: cmd.String("remote-path"), LauncherPath: cmd.String("launcher"),
				})
			},
			Render: renderInit,
		}),
		command(env, cmdSpec{
			Name:      "clone",
			Category:  catSetup,
			Usage:     "Clone a repository and set this computer up for it (assets stay placeholders until fxvcs load)",
			ArgsUsage: "<url> [directory]",
			Examples: []string{
				"fxvcs clone https://github.com/you/server-resources.git",
				"fxvcs clone --load git@github.com:you/maps.git my-maps",
			},
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "load", Usage: "download every tracked asset right after cloning"},
				&cli.BoolFlag{Name: "blobless", Usage: "faster clone of big histories (git clone --filter=blob:none)"},
				&cli.StringFlag{Name: "launcher", Usage: "absolute path where the fxvcs launcher for Git is installed"},
			},
			Partial: true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				args := cmd.Args().Slice()
				if len(args) == 0 {
					return nil, usageError{"clone requires a URL", "fxvcs clone <url> [directory]"}
				}
				req := sdk.CloneRequest{URL: args[0], Cwd: dir, Hydrate: cmd.Bool("load"), Blobless: cmd.Bool("blobless"), LauncherPath: cmd.String("launcher")}
				if len(args) > 1 {
					req.Directory = args[1]
				}
				return c.Clone(ctx, req)
			},
			Render: renderClone,
		}),
		command(env, cmdSpec{
			Name:     "setup",
			Category: catSetup,
			Usage:    "Prepare an existing clone on this computer (Git driver, pre-push check, local records)",
			Examples: []string{"fxvcs setup"},
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "launcher", Usage: "absolute path where the fxvcs launcher for Git is installed (default: the recorded or per-user location)"},
				&cli.BoolFlag{Name: "repair-launcher", Usage: "replace equal-version launcher bytes with this trusted build"},
			},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				return c.Setup(ctx, sdk.SetupRequest{Path: dir, LauncherPath: cmd.String("launcher"), RepairLauncher: cmd.Bool("repair-launcher")})
			},
			Render: renderSetup,
		}),
		command(env, cmdSpec{
			Name:     "upgrade",
			Category: catFix,
			Usage:    "Bring this repository and clone up to date with the installed fxvcs (plan by default)",
			Examples: []string{
				"fxvcs upgrade            # show what would change",
				"fxvcs upgrade --apply    # do it (nothing is committed)",
				"fxvcs upgrade --check    # for CI: exit 1 when something is pending",
			},
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "check", Usage: "read-only: exit 1 when an upgrade is pending"},
				&cli.BoolFlag{Name: "apply", Usage: "apply the plan (files are written; nothing is committed)"},
				&cli.BoolFlag{Name: "repair-launcher", Usage: "replace equal-version launcher bytes with this trusted build"},
			},
			Partial: true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				mode := sdk.UpgradePlan
				if cmd.Bool("check") {
					mode = sdk.UpgradeCheck
				}
				if cmd.Bool("apply") {
					mode = sdk.UpgradeApply
				}
				res, err := c.Upgrade(ctx, sdk.UpgradeRequest{Path: dir, Mode: mode, RepairLauncher: cmd.Bool("repair-launcher")})
				if err != nil {
					return res, err
				}
				if mode == sdk.UpgradeCheck && !res.UpToDate {
					return res, upgradePending{}
				}
				return res, nil
			},
			Render: renderUpgrade,
		}),
		command(env, cmdSpec{
			Name:      "track",
			Category:  catSetup,
			Usage:     "Start keeping the given files (or a pattern) in fxvcs storage instead of Git",
			ArgsUsage: "<path|pattern>...",
			Examples: []string{
				"fxvcs track stream/city.ytd",
				"fxvcs track --glob '*.ytd'",
				"fxvcs track --glob '[maps]/**/*.ymap'   # quote bracket folders",
			},
			Flags: []cli.Flag{
				&cli.BoolFlag{Name: "glob", Usage: "treat the arguments as patterns like '*.ytd' instead of file paths"},
				&cli.BoolFlag{Name: "force", Usage: "allow a pattern that would also catch source files such as *.lua"},
			},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				if cmd.Args().Len() == 0 {
					return nil, usageError{"track needs at least one path or --glob pattern", "fxvcs track --help"}
				}
				return c.Track(ctx, sdk.TrackRequest{Path: dir, Selector: sdk.PathSelector{Paths: cmd.Args().Slice(), Glob: cmd.Bool("glob")}, Force: cmd.Bool("force")})
			},
			Render: renderTrack,
		}),
		command(env, cmdSpec{
			Name:      "untrack",
			Category:  catSetup,
			Usage:     "Stop keeping the given files (or a pattern) in fxvcs storage; they go back to plain Git",
			ArgsUsage: "<path|pattern>...",
			Examples:  []string{"fxvcs untrack --glob '*.ytd'", "fxvcs untrack stream/city.ytd"},
			Flags:     []cli.Flag{&cli.BoolFlag{Name: "glob", Usage: "treat the arguments as patterns like '*.ytd' instead of file paths"}},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				if cmd.Args().Len() == 0 {
					return nil, usageError{"untrack needs at least one path or --glob pattern", "fxvcs untrack --help"}
				}
				return c.Untrack(ctx, sdk.UntrackRequest{Path: dir, Selector: sdk.PathSelector{Paths: cmd.Args().Slice(), Glob: cmd.Bool("glob")}})
			},
			Render: renderUntrack,
		}),
		group("tracking", catSetup, "Explain which tracking rule applies to a file",
			command(env, cmdSpec{
				Name:      "check",
				Usage:     "Say whether a file is tracked by fxvcs and which rule decides that",
				ArgsUsage: "<path>",
				Examples:  []string{"fxvcs tracking check stream/city.ytd"},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					if cmd.Args().Len() != 1 {
						return nil, usageError{"tracking check needs exactly one path", "fxvcs tracking check <path>"}
					}
					return c.TrackingCheck(ctx, sdk.TrackingCheckRequest{Path: dir, File: cmd.Args().First()})
				},
				Render: renderTrackingCheck,
			}),
		),
		command(env, cmdSpec{
			Name:     "scan",
			Category: catSetup,
			Usage:    "List large files in this repository that are not tracked by fxvcs yet",
			Examples: []string{"fxvcs scan", "fxvcs scan --min-bytes 512000"},
			Flags:    []cli.Flag{&cli.Int64Flag{Name: "min-bytes", Value: scanThreshold, Usage: "only list files at least this many bytes (default 1 MiB)"}},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				return c.Scan(ctx, sdk.ScanRequest{Path: dir, MinimumBytes: cmd.Int64("min-bytes")})
			},
			Render: renderScan,
		}),
		command(env, cmdSpec{
			Name:      "migrate",
			Category:  catSetup,
			Usage:     "Move files that are already committed in Git into fxvcs storage (history is unchanged)",
			ArgsUsage: "[path|pattern...]",
			Examples:  []string{"fxvcs migrate --glob '*.ytd'", "fxvcs migrate stream/city.ytd"},
			Flags:     []cli.Flag{&cli.BoolFlag{Name: "glob", Usage: "treat the arguments as patterns like '*.ytd' instead of file paths"}},
			Partial:   true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				if cmd.Args().Len() == 0 {
					return nil, usageError{"migrate needs at least one path or --glob pattern", "fxvcs migrate --glob '*.ytd'   (fxvcs scan lists large committed files)"}
				}
				res, err := c.Migrate(ctx, sdk.MigrateRequest{Path: dir, Selector: sdk.PathSelector{Paths: cmd.Args().Slice(), Glob: cmd.Bool("glob")}})
				if err != nil && (res == nil || len(res.Items) == 0) {
					return nil, err
				}
				return res, err
			},
			Render: renderMigrate,
		}),
	}
}

// upgradePending makes `upgrade --check` exit non-zero without an SDK error.
type upgradePending struct{}

func (upgradePending) Error() string { return "upgrade pending (run: fxvcs upgrade --apply)" }

func renderSetupBody(p *ui.Printer, s *sdk.SetupResult) {
	if s == nil {
		return
	}
	var items []ui.Item
	launcherNote := "already installed"
	if s.LauncherInstalled {
		launcherNote = "installed"
	}
	if p.Verbose() {
		launcherNote += " at " + s.LauncherPath
	}
	items = append(items, ui.Item{State: ui.OK, Label: "ready", Path: "Git driver (fxvcs launcher)", Note: launcherNote})
	switch {
	case s.HookForeign:
		items = append(items, ui.Item{State: ui.Warn, Label: "manual", Path: "pre-push check", Note: "another pre-push hook exists and was left alone; see the note below"})
	case s.HookInstalled:
		items = append(items, ui.Item{State: ui.OK, Label: "ready", Path: "pre-push check", Note: "git push is refused while assets are not yet uploaded"})
	}
	if s.StateCreated || s.LedgerCreated {
		items = append(items, ui.Item{State: ui.OK, Label: "ready", Path: "local records", Note: "created"})
	} else {
		items = append(items, ui.Item{State: ui.OK, Label: "ready", Path: "local records"})
	}
	p.Items(items)
	if p.Verbose() && len(s.ConfigKeys) > 0 {
		p.Verbosef("  git config: %s", strings.Join(s.ConfigKeys, ", "))
	}
	if s.HookForeign {
		p.Blank()
		p.Line("%s", strings.TrimSpace(s.HookInstructions))
	}
}

func renderSetup(p *ui.Printer, v any, _ renderMeta) {
	s := v.(*sdk.SetupResult)
	p.Header("setup", "this clone is ready")
	p.Blank()
	renderSetupBody(p, s)
	p.Footer("next: fxvcs load downloads the assets; fxvcs status shows what you have.")
}

func renderInit(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.InitResult)
	kind := "generic repository"
	if r.Profile == "fivem" {
		kind = "FiveM repository"
	}
	name := r.Name
	if name == "" {
		name = r.RepositoryRoot
	}
	p.Header("init", fmt.Sprintf("set up %s as a %s", name, kind))
	p.Verbosef("repository id %s, storage id %s", r.RepositoryID, r.StorageDomainID)
	p.Blank()
	if r.Descriptor != nil {
		d := r.Descriptor
		layout := "one resource"
		if d.Layout == "collection" {
			layout = ui.Count(len(d.Resources), "resource")
		}
		where := d.SourceRoot
		if where == "." || where == "" {
			where = "this folder"
		}
		line := fmt.Sprintf("  found %s in %s", layout, where)
		if d.ResourceName != "" {
			line += " (resource name " + d.ResourceName
			if d.ResourceNameProposed {
				line += ", taken from the folder name; check it"
			}
			line += ")"
		}
		p.Line("%s", line)
		for _, ig := range d.Ignored {
			p.Line("  ignored %s (FiveM would not load it from here either)", ig)
		}
	}
	if len(r.FilesWritten) > 0 {
		p.Line("  wrote %s", strings.Join(r.FilesWritten, ", "))
	}
	if r.Track != nil && len(r.Track.RulesAdded) > 0 {
		shown := r.Track.RulesAdded
		more := ""
		if len(shown) > 8 && !p.Verbose() {
			more = fmt.Sprintf(" and %d more", len(shown)-8)
			shown = shown[:8]
		}
		p.Line("  tracking %s of FiveM asset types (%s%s); see .gitattributes", ui.Count(len(r.Track.RulesAdded), "pattern"), strings.Join(shown, " "), more)
	}
	renderSetupBody(p, r.Setup)
	p.Footer("review the new files, then: git add .fxvcs .gitattributes && git commit -m \"set up fxvcs\"")
	p.Hint("add shared storage before pushing assets: fxvcs storage add --name primary --path <folder shared with your team>")
}

func renderClone(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.CloneResult)
	p.Header("clone", "cloned into "+r.RepositoryRoot+" and set this computer up")
	p.Blank()
	renderSetupBody(p, r.Setup)
	if r.Hydrate != nil {
		loaded := 0
		for _, it := range r.Hydrate.Items {
			if !it.Skipped {
				loaded++
			}
		}
		p.Footer(fmt.Sprintf("loaded %s.", ui.Count(loaded, "asset")))
		return
	}
	p.Footer("assets are placeholders until you run: fxvcs load")
}

func renderUpgrade(p *ui.Printer, v any, m renderMeta) {
	r := v.(*sdk.UpgradeResult)
	if r.UpToDate {
		p.Header("upgrade", "this repository is up to date with fxvcs "+r.InstalledVersion)
	} else {
		verb := "needed"
		if r.Applied {
			verb = "applied"
		}
		p.Header("upgrade", fmt.Sprintf("%s %s", ui.Plural(len(r.Steps), "change", "changes"), verb))
		p.Blank()
		items := make([]ui.Item, 0, len(r.Steps))
		for i, s := range r.Steps {
			st, label := ui.Pending, "planned"
			if i < len(r.AppliedSteps) {
				st, label = ui.OK, "done"
			}
			note := s.Description
			if p.Verbose() {
				note = "[" + s.Kind + "] " + s.Target + ": " + s.Description
				if s.FromVersion != "" || s.ToVersion != "" {
					note += " (" + s.FromVersion + " -> " + s.ToVersion + ")"
				}
			}
			items = append(items, ui.Item{State: st, Label: label, Path: upgradeTarget(s), Note: note})
		}
		p.Items(items)
	}
	for _, n := range r.Notes {
		p.Line("  note: %s", n)
	}
	if !r.UpToDate && !r.Applied {
		if m.Cmd != nil && m.Cmd.Bool("check") {
			p.Footer("apply with fxvcs upgrade --apply (nothing is committed).")
		} else {
			p.Footer("dry run: nothing was changed. Apply with fxvcs upgrade --apply (nothing is committed).")
		}
	} else if r.Applied {
		p.Footer("done. Committed files that changed (.fxvcs/, .gitattributes) still need git add/commit.")
	}
}

func upgradeTarget(s sdk.UpgradeStep) string {
	switch s.Kind {
	case "hook":
		return "pre-push check"
	case "git-config":
		return "git driver setup"
	case "launcher":
		return "fxvcs launcher"
	case "local-state":
		return "local records"
	}
	return s.Target
}

func renderTrack(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.TrackResult)
	all := append(append([]string{}, r.RulesAdded...), r.RulesPresent...)
	p.Header("track", fmt.Sprintf("tracking %s from the next git add", strings.Join(all, ", ")))
	p.Blank()
	items := []ui.Item{}
	for _, s := range r.RulesAdded {
		items = append(items, ui.Item{State: ui.OK, Label: "added", Path: s})
	}
	for _, s := range r.RulesPresent {
		items = append(items, ui.Item{State: ui.OK, Label: "already present", Path: s})
	}
	p.Items(items)
	p.Footer("review .gitattributes and commit it: git add .gitattributes && git commit")
	p.Hint("files already committed in Git move to fxvcs storage with: fxvcs migrate")
}

func renderUntrack(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.UntrackResult)
	p.Header("untrack", fmt.Sprintf("%s removed from tracking", ui.Plural(len(r.RulesRemoved), "rule", "rules")))
	p.Blank()
	items := []ui.Item{}
	for _, s := range r.RulesRemoved {
		items = append(items, ui.Item{State: ui.OK, Label: "removed", Path: s})
	}
	for _, s := range r.RulesMissing {
		items = append(items, ui.Item{State: ui.Skip, Label: "not found", Path: s, Note: "no such rule in the fxvcs section of .gitattributes"})
	}
	p.Items(items)
	if len(r.PathsRequiringHydration) > 0 {
		p.Blank()
		p.Line("these committed assets need loading before they can be un-tracked:")
		p.List(r.PathsRequiringHydration)
		p.Hint("fxvcs load " + strings.Join(r.PathsRequiringHydration, " ") + ", then git add them")
	}
	p.Footer("review .gitattributes and commit it: git add .gitattributes && git commit")
}

func renderTrackingCheck(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.TrackingCheckResult)
	src := ""
	switch r.RuleSource {
	case "fxvcs-block":
		src = "from the fxvcs section of .gitattributes"
	case "user":
		src = "written by you in .gitattributes"
	case "other":
		src = "from another .gitattributes rule"
	}
	if r.Tracked {
		if r.MatchedRule != "" {
			p.Header("tracking check", fmt.Sprintf("%s is tracked by fxvcs (rule %s %s)", r.File, r.MatchedRule, src))
		} else {
			p.Header("tracking check", fmt.Sprintf("%s is tracked by fxvcs", r.File))
		}
	} else {
		p.Header("tracking check", fmt.Sprintf("%s is not tracked by fxvcs", r.File))
		p.Hint("fxvcs track " + r.File + "   (or a pattern: fxvcs track --glob '*.ext')")
	}
	p.Verbosef("attributes: %s (source %s)", r.AttributesRaw, r.RuleSource)
}

func renderScan(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.ScanResult)
	untracked := 0
	for _, cd := range r.Candidates {
		if !cd.Tracked {
			untracked++
		}
	}
	if len(r.Candidates) == 0 {
		p.Header("scan", "no large files found")
		return
	}
	p.Header("scan", fmt.Sprintf("%s not tracked by fxvcs (%d large files in total)", ui.Plural(untracked, "large file", "large files"), len(r.Candidates)))
	p.Blank()
	items := make([]ui.Item, 0, len(r.Candidates))
	for _, cd := range r.Candidates {
		it := ui.Item{Path: cd.Path, Size: ui.Bytes(cd.Bytes)}
		if cd.Tracked {
			it.State, it.Label = ui.OK, "tracked"
		} else {
			it.State, it.Label = ui.Pending, "not tracked"
		}
		items = append(items, it)
	}
	p.Items(items)
	if untracked > 0 {
		p.Footer("start tracking with fxvcs track <path>, or a pattern: fxvcs track --glob '*.ext'")
	}
}

func renderMigrate(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.MigrateResult)
	moved, skipped := 0, 0
	items := make([]ui.Item, 0, len(r.Items))
	for _, it := range r.Items {
		if it.Skipped {
			skipped++
			items = append(items, ui.Item{State: ui.Skip, Label: "skipped", Path: it.Path, Note: humanReason(p.Verbose(), it.Reason)})
			continue
		}
		moved++
		note := ""
		if p.Verbose() {
			note = ui.ShortID(it.Digest)
		}
		items = append(items, ui.Item{State: ui.OK, Label: "moved", Path: it.Path, Size: ui.Bytes(it.Bytes), Note: note})
	}
	if len(r.Items) == 0 {
		p.Header("migrate", "nothing to move; no committed files matched")
		p.Hint("fxvcs track --glob '*.ext' first, then fxvcs migrate")
		return
	}
	p.Header("migrate", fmt.Sprintf("moved %s into fxvcs storage", ui.Count(moved, "committed file")))
	p.Blank()
	p.Items(items)
	footer := fmt.Sprintf("%d moved, %d skipped. History is unchanged.", moved, skipped)
	if moved > 0 {
		footer += " Review with git diff --cached, commit the placeholders (git commit), then upload before pushing: fxvcs objects publish"
	}
	p.Footer(footer)
}
