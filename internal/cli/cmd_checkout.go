package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

// scanThreshold is the "large file" size used by the status footer (matches
// the default of fxvcs scan --min-bytes).
const scanThreshold = 1 << 20

func ckSelector(cmd *cli.Command) sdk.PathSelector {
	return sdk.PathSelector{Paths: cmd.Args().Slice(), Glob: cmd.Bool("glob")}
}

func globFlag() cli.Flag {
	return &cli.BoolFlag{Name: "glob", Usage: "treat the arguments as patterns like '*.ytd' instead of literal paths"}
}

func launcherFlag() cli.Flag {
	return &cli.StringFlag{Name: "launcher", Usage: "absolute path of the fxvcs launcher the Git driver configuration must use (default: the per-user launcher)"}
}

func checkoutCommands(env Env) []*cli.Command {
	load := cmdSpec{
		Name:      "load",
		Aliases:   []string{"download"},
		Category:  catDaily,
		Usage:     "Download assets into your checkout (replace placeholders with the real files)",
		ArgsUsage: "[path...]",
		Examples: []string{
			"fxvcs load                              # every tracked asset",
			"fxvcs load stream/city.ytd              # one asset",
			"fxvcs load --glob '[maps]/**/*.ymap'    # a pattern (quote bracket paths)",
		},
		Flags:   []cli.Flag{globFlag()},
		Partial: true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			res, err := c.Hydrate(ctx, sdk.HydrateRequest{Path: dir, Selector: ckSelector(cmd)})
			return res, err
		},
		Render: renderItems("load", "loaded", "loaded"),
	}
	unload := cmdSpec{
		Name:      "unload",
		Aliases:   []string{"undownload"},
		Category:  catDaily,
		Usage:     "Free disk space: turn downloaded assets back into placeholders (never touches changed files)",
		ArgsUsage: "[path...]",
		Examples: []string{
			"fxvcs unload                            # every loaded asset",
			"fxvcs unload stream/city.ytd",
			"fxvcs unload --glob 'stream/*.ytd'",
		},
		Flags:   []cli.Flag{globFlag()},
		Partial: true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			res, err := c.Dehydrate(ctx, sdk.DehydrateRequest{Path: dir, Selector: ckSelector(cmd)})
			return res, err
		},
		Render: renderItems("unload", "unloaded", "unloaded"),
	}
	discard := cmdSpec{
		Name:      "discard",
		Category:  catFix,
		Usage:     "Throw away your local changes to an asset and put back the version fxvcs has recorded",
		ArgsUsage: "<path...>",
		Examples: []string{
			"fxvcs discard stream/city.ytd           # one asset",
			"fxvcs discard --glob 'stream/*.ytd'",
			"fxvcs discard --all                     # every asset you have changed",
		},
		Flags: []cli.Flag{
			globFlag(),
			&cli.BoolFlag{Name: "all", Usage: "discard your changes to every tracked asset"},
		},
		Partial: true,
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.Discard(ctx, sdk.DiscardRequest{Path: dir, Selector: ckSelector(cmd), All: cmd.Bool("all")})
		},
		Render: renderItems("discard", "restored", "restored"),
	}
	hydrate, dehydrate := load, unload
	hydrate.Name, hydrate.Aliases, hydrate.Hidden = "hydrate", nil, true
	dehydrate.Name, dehydrate.Aliases, dehydrate.Hidden = "dehydrate", nil, true

	return []*cli.Command{
		command(env, cmdSpec{
			Name:      "status",
			Category:  catDaily,
			Usage:     "Show which assets are loaded, placeholders, or changed locally",
			ArgsUsage: "[path...]",
			Examples:  []string{"fxvcs status", "fxvcs status stream/", "fxvcs status --glob '*.ytd'"},
			Flags:     []cli.Flag{globFlag()},
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				return c.Status(ctx, sdk.StatusRequest{Path: dir, Selector: ckSelector(cmd)})
			},
			Render: renderStatus,
			After:  statusScanFooter,
		}),
		command(env, load),
		command(env, unload),
		command(env, discard),
		command(env, hydrate),
		command(env, dehydrate),
		command(env, cmdSpec{
			Name:      "verify",
			Category:  catDaily,
			Usage:     "Check that every tracked asset is intact and, with --remote, present in storage",
			ArgsUsage: "[path...]",
			Examples:  []string{"fxvcs verify", "fxvcs verify --remote           # also probe every storage required before push"},
			Flags:     []cli.Flag{globFlag(), &cli.BoolFlag{Name: "remote", Usage: "also check that every storage required before push has the asset data"}},
			Partial:   true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				res, err := c.Verify(ctx, sdk.VerifyRequest{Path: dir, Selector: ckSelector(cmd), Remote: cmd.Bool("remote")})
				if err == nil && len(res.Issues) > 0 {
					err = sdk.NewError(res.Issues[0].Code, fmt.Sprintf("%s found by verify", ui.Count(len(res.Issues), "problem")))
				}
				return res, err
			},
			Render: renderVerify,
		}),
		command(env, cmdSpec{
			Name:     "doctor",
			Category: catFix,
			Usage:    "Check your checkout for problems and say which ones fxvcs repair can fix",
			Examples: []string{"fxvcs doctor"},
			Flags:    []cli.Flag{launcherFlag()},
			Partial:  true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				res, err := c.Doctor(ctx, sdk.DoctorRequest{Path: dir, Launcher: cmd.String("launcher")})
				if err == nil && len(res.Findings) > 0 {
					fixable := 0
					for _, f := range res.Findings {
						if f.Repairable {
							fixable++
						}
					}
					e := sdk.NewError(sdk.CodeConflict, fmt.Sprintf("%s found by doctor", ui.Count(len(res.Findings), "problem")))
					if fixable > 0 {
						e.Hint = "fxvcs repair   (fixes the " + ui.Count(fixable, "fixable item") + ")"
					}
					err = e
				}
				return res, err
			},
			Render: renderDoctor,
		}),
		command(env, cmdSpec{
			Name:     "repair",
			Category: catFix,
			Usage:    "Fix the problems doctor marked fixable (never overwrites files you changed)",
			Examples: []string{"fxvcs repair"},
			Flags:    []cli.Flag{launcherFlag()},
			Partial:  true,
			Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
				res, err := c.Repair(ctx, sdk.RepairRequest{Path: dir, Launcher: cmd.String("launcher")})
				if err == nil && len(res.Refused) > 0 {
					e := sdk.NewError(sdk.CodeConflict, fmt.Sprintf("%s need your decision", ui.Plural(len(res.Refused), "file", "files")))
					e.Hint = "commit or discard the local changes with git, then run: fxvcs repair"
					err = e
				}
				return res, err
			},
			Render: renderRepair,
		}),
	}
}

// renderItems renders load/unload outcomes: header with the request count,
// one row per item, footer with counts and the next step.
func renderItems(command, doneLabel, doneWord string) renderFunc {
	// unload reports placeholder sizes, which mean nothing to the user; load and
	// discard both report the asset's own bytes.
	showSize := command == "load" || command == "discard"
	return func(p *ui.Printer, v any, _ renderMeta) {
		res := v.(*sdk.HydrateResult)
		if len(res.Items) == 0 {
			p.Header(command, "nothing to do; no tracked assets matched")
			p.Hint("fxvcs status   (lists tracked assets) or fxvcs track <path>")
			return
		}
		p.Header(command, ui.Count(len(res.Items), "asset")+" requested")
		p.Blank()
		done, skipped, failed := 0, 0, 0
		items := make([]ui.Item, 0, len(res.Items))
		var reusedShared, reusedCached int
		var reusedBytes int64
		for _, it := range res.Items {
			switch {
			case it.Skipped && isRefusal(it.Reason):
				skipped++
				items = append(items, ui.Item{State: ui.Skip, Label: "skipped", Path: it.Path, Note: skipNote(p, command, it)})
			case it.Skipped:
				failed++
				items = append(items, ui.Item{State: ui.Fail, Label: "failed", Path: it.Path, Note: humanReason(p.Verbose(), it.Reason)})
			default:
				done++
				note := ""
				if p.Verbose() {
					note = ui.ShortID(it.Digest)
				}
				// Content that did not have to be transferred is worth naming:
				// it is the whole point of storing an asset once and using it
				// in many resources.
				switch it.Reuse {
				case sdk.ReuseShared:
					reusedShared++
					reusedBytes += it.Bytes
					note = strings.TrimSpace(note + " shares content with another file")
				case sdk.ReuseCached:
					reusedCached++
					reusedBytes += it.Bytes
					note = strings.TrimSpace(note + " already stored on this computer")
				}
				size := ""
				if showSize {
					size = ui.Bytes(it.Bytes)
				}
				items = append(items, ui.Item{State: ui.OK, Label: doneLabel, Path: it.Path, Size: size, Note: note})
			}
		}
		p.Items(items)
		parts := []string{fmt.Sprintf("%d %s", done, doneWord)}
		if skipped > 0 {
			parts = append(parts, fmt.Sprintf("%d skipped", skipped))
		}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", failed))
		}
		p.Footer(strings.Join(parts, ", ") + ".")
		if reused := reusedShared + reusedCached; reused > 0 && reusedBytes > 0 {
			var why []string
			if reusedShared > 0 {
				why = append(why, fmt.Sprintf("%s share content with other files", ui.Count(reusedShared, "file")))
			}
			if reusedCached > 0 {
				why = append(why, fmt.Sprintf("%s already stored here", ui.Count(reusedCached, "file")))
			}
			p.Footer(fmt.Sprintf("%s did not need downloading: %s.", ui.Bytes(reusedBytes), strings.Join(why, "; ")))
		}
	}
}

// isRefusal reports a per-item skip that is not a failure: already in the
// wanted state, not tracked, changed locally, or a conflict the user must
// resolve.
func isRefusal(reason string) bool {
	for _, s := range []string{"already hydrated", "already a pointer", "not managed by fxvcs", "not in the index", "hydrated-modified", "conflict", "unmerged", "not migrated", "no local changes to discard", "no change to discard"} {
		if strings.Contains(reason, s) {
			return true
		}
	}
	return false
}

func skipNote(p *ui.Printer, command string, it sdk.ItemOutcome) string {
	reason := humanReason(p.Verbose(), it.Reason)
	switch {
	case p.Verbose():
		return reason
	case strings.Contains(it.Reason, "not managed by fxvcs"):
		return fmt.Sprintf("not tracked by fxvcs (fxvcs track %s to start tracking it)", quoteIfNeeded(it.Path))
	case strings.Contains(it.Reason, "hydrated-modified"):
		return "changed locally (commit it with git add/commit, or discard the change, then run fxvcs " + command + ")"
	}
	return reason
}

// renderStatus renders the checkout table.
func renderStatus(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.StatusResult)
	if len(res.Entries) == 0 && len(res.Skipped) == 0 {
		p.Header("status", "no tracked assets in this repository")
		p.Hint("fxvcs track <path> or fxvcs track --glob '*.ytd' starts tracking; fxvcs scan lists large files")
		return
	}
	p.Header("status", ui.Count(len(res.Entries), "tracked asset")+" in this repository")
	p.Blank()
	var items []ui.Item
	loaded, placeholders, changed, attention, missing, pending := 0, 0, 0, 0, 0, 0
	for _, e := range res.Entries {
		it := ui.Item{Path: e.Path}
		if e.Size > 0 {
			it.Size = ui.Bytes(e.Size)
		}
		var notes []string
		switch e.State {
		case sdk.AssetHydratedClean:
			loaded++
			it.State, it.Label = ui.OK, "loaded"
		case sdk.AssetHydratedDirty:
			changed++
			it.State, it.Label = ui.Warn, "loaded"
			note := "changed locally"
			if e.SizeDelta != nil {
				note += " (" + ui.SignedBytes(*e.SizeDelta) + ")"
			}
			notes = append(notes, note)
		case sdk.AssetUnhydratedClean:
			placeholders++
			it.State, it.Label = ui.Pending, "placeholder"
			notes = append(notes, "(fxvcs load "+quoteIfNeeded(e.Path)+")")
		case sdk.AssetConflict:
			attention++
			it.State, it.Label = ui.Warn, "needs attention"
			if e.Detail != "" {
				notes = append(notes, humanReason(p.Verbose(), e.Detail))
			}
		case sdk.AssetMissing:
			missing++
			it.State, it.Label = ui.Warn, "missing"
			notes = append(notes, "file is gone (fxvcs repair rewrites the placeholder)")
		default:
			it.State, it.Label = ui.Plain, string(e.State)
		}
		if e.Pending {
			pending++
			if e.State != sdk.AssetUnhydratedClean {
				notes = append(notes, "not yet uploaded")
			}
		}
		if p.Verbose() {
			extra := []string{ui.ShortID(e.IndexDigest)}
			if e.Cached {
				extra = append(extra, "cached")
			}
			if e.Published {
				extra = append(extra, "published")
			}
			notes = append(notes, strings.Join(extra, " "))
		}
		it.Note = strings.Join(notes, "; ")
		items = append(items, it)
	}
	for _, s := range res.Skipped {
		items = append(items, ui.Item{State: ui.Skip, Label: "not tracked", Path: s.Path, Note: fmt.Sprintf("(fxvcs track %s to start tracking it)", quoteIfNeeded(s.Path))})
	}
	p.Items(items)
	var parts []string
	if changed > 0 {
		parts = append(parts, ui.Count(changed, "asset")+" changed locally: commit with git add/commit")
	}
	if placeholders > 0 {
		parts = append(parts, fmt.Sprintf("%d not loaded (fxvcs load)", placeholders))
	}
	if attention > 0 {
		parts = append(parts, ui.Plural(attention, "asset needs", "assets need")+" attention (fxvcs doctor)")
	}
	if missing > 0 {
		parts = append(parts, fmt.Sprintf("%d missing (fxvcs repair)", missing))
	}
	if pending > 0 {
		parts = append(parts, ui.Count(pending, "asset")+" not yet uploaded (fxvcs objects publish before git push)")
	}
	if len(parts) == 0 && loaded > 0 {
		parts = append(parts, "everything is loaded and unchanged")
	}
	p.Footer(strings.Join(parts, "; ") + ".")
}

// statusScanFooter appends the large-untracked-files line (human mode only;
// best effort, silent on error).
func statusScanFooter(ctx context.Context, p *ui.Printer, c *sdk.Client, dir string, _ any) {
	if p.Quiet() {
		return
	}
	res, err := c.Scan(ctx, sdk.ScanRequest{Path: dir, MinimumBytes: scanThreshold})
	if err != nil {
		return
	}
	n := 0
	for _, cd := range res.Candidates {
		if !cd.Tracked {
			n++
		}
	}
	if n > 0 {
		p.Line("%s not tracked by fxvcs — see fxvcs scan", ui.Plural(n, "large file is", "large files are"))
	}
}

func renderVerify(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.VerifyResult)
	if len(res.Issues) == 0 {
		p.Header("verify", fmt.Sprintf("checked %s; everything is consistent", ui.Count(res.Checked, "asset")))
		return
	}
	p.Header("verify", fmt.Sprintf("checked %s; %s", ui.Count(res.Checked, "asset"), ui.Count(len(res.Issues), "problem")))
	p.Blank()
	groups := map[sdk.Code][]sdk.VerifyIssue{}
	order := []sdk.Code{}
	for _, is := range res.Issues {
		if _, ok := groups[is.Code]; !ok {
			order = append(order, is.Code)
		}
		groups[is.Code] = append(groups[is.Code], is)
	}
	for _, code := range order {
		title, next := verifyGroup(code)
		p.Line("%s:", title)
		items := make([]ui.Item, 0, len(groups[code]))
		for _, is := range groups[code] {
			note := humanReason(p.Verbose(), is.Message)
			if p.Verbose() && is.Digest != "" {
				note += " (" + ui.ShortID(is.Digest) + ")"
			}
			items = append(items, ui.Item{State: ui.Fail, Label: "problem", Path: is.Path, Note: note})
		}
		p.Items(items)
		p.Hint(next)
	}
	p.Footer(ui.Count(len(res.Issues), "problem") + " found.")
}

func verifyGroup(code sdk.Code) (title, next string) {
	switch code {
	case sdk.CodeNotFound:
		return "asset data is missing", "fxvcs load <path> fetches it from storage; if it was never uploaded, run fxvcs objects publish where the file was added"
	case sdk.CodeStorage:
		return "storage could not be checked", "fxvcs storage test <name>, then run fxvcs verify --remote again"
	}
	return "assets that are damaged or inconsistent", "fxvcs doctor explains each one; re-add the original file with git add if the data was damaged"
}

func renderDoctor(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.DoctorResult)
	if len(res.Findings) == 0 {
		p.Header("doctor", "checked your checkout; no problems found")
		return
	}
	var fixable, manual []sdk.DoctorFinding
	for _, f := range res.Findings {
		if f.Repairable {
			fixable = append(fixable, f)
		} else {
			manual = append(manual, f)
		}
	}
	p.Header("doctor", fmt.Sprintf("checked your checkout; %s", ui.Count(len(res.Findings), "problem")))
	if len(fixable) > 0 {
		p.Blank()
		p.Line("fxvcs repair can fix these:")
		p.Items(doctorItems(p, fixable, ui.Warn, "fixable"))
	}
	if len(manual) > 0 {
		p.Blank()
		p.Line("these need your decision:")
		p.Items(doctorItems(p, manual, ui.Fail, "attention"))
	}
	if len(fixable) > 0 {
		p.Footer("run fxvcs repair to fix the " + ui.Count(len(fixable), "fixable item") + ".")
	} else {
		p.Footer("nothing here is automatic; follow the notes above.")
	}
}

func doctorItems(p *ui.Printer, fs []sdk.DoctorFinding, st ui.State, label string) []ui.Item {
	items := make([]ui.Item, 0, len(fs))
	for _, f := range fs {
		path := f.Path
		if path == "" {
			path = doctorKindWord(f.Kind)
		}
		note := humanReason(p.Verbose(), f.Message)
		if p.Verbose() {
			note = "[" + f.Kind + "] " + note
		} else if f.Kind == "git-config" {
			note = gitConfigFinding(f.Message)
		}
		items = append(items, ui.Item{State: st, Label: label, Path: path, Note: note})
	}
	return items
}

// gitConfigFinding rewrites a git-config finding without config keys or
// launcher paths (those appear under --verbose).
func gitConfigFinding(msg string) string {
	switch {
	case strings.Contains(msg, "pre-push hook is not installed"):
		return "the pre-push check is not installed; git push would not be checked for assets that are not yet uploaded"
	case strings.Contains(msg, "is not set;"):
		return "a Git driver setting is missing; git add/checkout would not handle tracked assets"
	case strings.Contains(msg, "; expected"):
		return "a Git driver setting points at another fxvcs launcher than the one in use"
	}
	return humanize(msg)
}

func doctorKindWord(kind string) string {
	switch kind {
	case "git-config":
		return "git driver setup"
	case "publication":
		return "upload records"
	case "cache":
		return "local asset cache"
	case "state-derived":
		return "local checkout records"
	}
	return "checkout"
}

func renderRepair(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.RepairResult)
	if len(res.Rebuilt) == 0 && len(res.Refused) == 0 {
		p.Header("repair", "nothing needed fixing")
		return
	}
	p.Header("repair", fmt.Sprintf("fixed %s", ui.Count(len(res.Rebuilt), "item")))
	p.Blank()
	items := make([]ui.Item, 0, len(res.Rebuilt)+len(res.Refused))
	for _, s := range res.Rebuilt {
		items = append(items, ui.Item{State: ui.OK, Label: "fixed", Path: humanReason(p.Verbose(), s)})
	}
	for _, f := range res.Refused {
		path := f.Path
		if path == "" {
			path = doctorKindWord(f.Kind)
		}
		items = append(items, ui.Item{State: ui.Skip, Label: "left alone", Path: path, Note: humanReason(p.Verbose(), f.Message)})
	}
	p.Items(items)
	if len(res.Refused) > 0 {
		p.Footer(fmt.Sprintf("%d fixed, %d left alone (repair never overwrites files you changed).", len(res.Rebuilt), len(res.Refused)))
	} else {
		p.Footer(fmt.Sprintf("%d fixed.", len(res.Rebuilt)))
	}
}
