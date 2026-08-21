package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

func storageCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("objects", catSharing, "See and upload asset data that is not yet in shared storage",
			command(env, cmdSpec{
				Name:     "status",
				Usage:    "List asset data recorded on this computer and whether it is uploaded",
				Examples: []string{"fxvcs objects status", "fxvcs --verbose objects status   # with asset ids"},
				Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
					return c.ObjectsStatus(ctx, sdk.ObjectsStatusRequest{Path: dir})
				},
				Render: renderObjectsStatus,
			}),
			command(env, cmdSpec{
				Name:      "publish",
				Usage:     "Upload asset data to shared storage (required before git push; pushes no Git commits)",
				ArgsUsage: "[path...]",
				Examples: []string{
					"fxvcs objects publish",
					"fxvcs objects publish stream/city.ytd   # only what this asset needs",
					"fxvcs objects publish --remote mirror",
					"fxvcs objects publish --concurrency 8",
				},
				Flags: []cli.Flag{
					globFlag(),
					&cli.StringSliceFlag{Name: "remote", Usage: "upload only to this storage (repeatable); default: every storage required before push"},
					&cli.IntFlag{Name: "concurrency", Value: 4, Usage: "how many uploads run at the same time"},
				},
				Partial: true,
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					res, err := c.ObjectsPublish(ctx, sdk.ObjectsPublishRequest{Path: dir, Selector: ckSelector(cmd),
						Remotes: cmd.StringSlice("remote"), Concurrency: cmd.Int("concurrency")})
					if err != nil {
						return nil, err
					}
					if len(res.Failed) > 0 {
						remotes := remoteNames(res.Failed)
						if name, ok := allUnbound(res.Failed); ok {
							return nil, sdk.NewError(sdk.CodeNotFound, fmt.Sprintf("storage %q is not set up on this computer", name)).
								WithHint(fmt.Sprintf("fxvcs storage add --name %s --path <folder shared with your team>", name))
						}
						e := sdk.NewError(sdk.CodeStorage, fmt.Sprintf("%s could not be uploaded to storage %s", ui.Count(len(res.Failed), "asset"), strings.Join(remotes, ", ")))
						e.Hint = "fix the cause shown above (or choose another folder with fxvcs storage add), then run: fxvcs objects publish"
						err = e
					}
					return res, err
				},
				Render: renderPublish,
			}),
		),
		group("cache", catSharing, "Local copy of asset data on this computer",
			command(env, cmdSpec{
				Name:     "status",
				Usage:    "Show how much disk the local asset cache uses and how much can be freed",
				Examples: []string{"fxvcs cache status"},
				Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
					return c.CacheStatus(ctx, sdk.CacheStatusRequest{Path: dir})
				},
				Render: renderCacheStatus,
			}),
			command(env, cmdSpec{
				Name:  "prune",
				Usage: "Free disk by removing cached asset data that is safely uploaded (plan by default)",
				Examples: []string{
					"fxvcs cache prune                    # show what would be removed",
					"fxvcs cache prune --apply            # remove it",
					"fxvcs cache prune --keep 20GiB --apply   # trim down to 20 GiB, keeping the most recently used",
				},
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "apply", Usage: "actually remove the files instead of only listing them"},
					&cli.StringFlag{Name: "keep", Usage: "how much cached data to keep, e.g. 20GiB (default: keep nothing that is safely uploaded)"},
				},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					keep, err := optionalSize(cmd, "keep")
					if err != nil {
						return nil, err
					}
					return c.CachePrune(ctx, sdk.CachePruneRequest{Path: dir, TargetBytes: keep, DryRun: !cmd.Bool("apply")})
				},
				Render: renderCachePrune,
			}),
			command(env, cmdSpec{
				Name:      "limit",
				Usage:     "Set how much disk the local asset cache may use before it trims itself",
				ArgsUsage: "<size|none>",
				Examples: []string{
					"fxvcs cache limit 50GiB",
					"fxvcs cache limit none    # let it grow until fxvcs cache prune",
				},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					if cmd.Args().Len() != 1 {
						return nil, usageError{"cache limit needs a size", "fxvcs cache limit 50GiB   (or: fxvcs cache limit none)"}
					}
					arg := cmd.Args().First()
					var bytes int64
					if arg != "none" && arg != "0" {
						n, err := parseSize(arg)
						if err != nil {
							return nil, usageError{err.Error(), "sizes look like 500MB, 20GiB, or a plain number of bytes"}
						}
						bytes = n
					}
					return c.CacheLimit(ctx, sdk.CacheLimitRequest{Path: dir, LimitBytes: bytes})
				},
				Render: renderCacheLimit,
			}),
			command(env, cmdSpec{
				Name:  "verify",
				Usage: "Re-read cached asset data and check it against its checksums",
				Examples: []string{
					"fxvcs cache verify",
					"fxvcs cache verify --repair   # delete damaged copies so they are downloaded again",
				},
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "repair", Usage: "delete damaged copies (never ones that are not yet uploaded)"},
					&cli.BoolFlag{Name: "all", Usage: "check every repository's cached data, not just this one's"},
				},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					return c.CacheVerify(ctx, sdk.CacheVerifyRequest{Path: dir, Repair: cmd.Bool("repair"), AllDomains: cmd.Bool("all")})
				},
				Render: renderCacheVerify,
			}),
		),
		group("storage", catSetup, "Where asset data is kept and shared with your team",
			command(env, cmdSpec{
				Name:  "add",
				Usage: "Add shared storage (a folder or S3 bucket your team can reach), or connect this computer to one",
				Examples: []string{
					"fxvcs storage add --name primary --path /srv/fxvcs-storage",
					"fxvcs storage add --name primary --path 'Z:\\fxvcs-storage'      # connect this computer to an existing one",
					"fxvcs storage add --name primary --bucket fxvcs-assets           # Amazon S3",
					"fxvcs storage add --name primary --bucket assets --endpoint https://s3.example.com   # S3-compatible (MinIO, R2, B2, Wasabi)",
					"fxvcs storage add --name mirror --path /mnt/backup --local       # only on this computer, never committed",
				},
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Required: true, Usage: "storage name, e.g. primary (declared in .fxvcs/repository.yaml when new; review and commit)"},
					&cli.StringFlag{Name: "type", Usage: "storage type: filesystem or s3 (guessed from --path or --bucket)"},
					&cli.StringFlag{Name: "path", Usage: "absolute folder of the storage on this computer (filesystem)"},
					&cli.StringFlag{Name: "bucket", Usage: "bucket name (s3)"},
					&cli.StringFlag{Name: "endpoint", Usage: "service URL for an S3-compatible provider; omit for Amazon S3"},
					&cli.StringFlag{Name: "prefix", Usage: "key prefix inside the bucket, so one bucket can hold several repositories"},
					&cli.StringFlag{Name: "region", Usage: "bucket region; use auto for providers that ignore it"},
					&cli.StringFlag{Name: "addressing", Usage: "path or virtual-host; leave unset unless your provider needs one"},
					&cli.BoolFlag{Name: "local", Usage: "keep this storage private to this clone (never committed)"},
					&cli.BoolFlag{Name: "required", Value: true, Usage: "asset data must be uploaded here before git push"},
				},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					spec := sdk.StorageRemoteSpec{
						Name: cmd.String("name"), Type: storageType(cmd), LocalPath: cmd.String("path"),
						Bucket: cmd.String("bucket"), Endpoint: cmd.String("endpoint"), Prefix: cmd.String("prefix"),
						Region: cmd.String("region"), Addressing: cmd.String("addressing"),
						Local: cmd.Bool("local"), RequiredForPublication: cmd.Bool("required"),
					}
					if spec.Type == "" {
						return nil, usageError{"storage add needs --path (a shared folder) or --bucket (an S3 bucket)",
							"fxvcs storage add --name primary --path <folder shared with your team>"}
					}
					return c.StorageAdd(ctx, sdk.StorageAddRequest{Path: dir, Remote: spec})
				},
				Render: renderStorageAdd,
			}),
			command(env, cmdSpec{
				Name:      "login",
				Usage:     "Save the access keys this computer uses for an S3 storage (never committed)",
				ArgsUsage: "<name>",
				Examples: []string{
					"fxvcs storage login primary --access-key-id AKIA...        # secret key is typed, not echoed",
					"cat secret.txt | fxvcs storage login primary --access-key-id AKIA...",
					"fxvcs storage login primary --forget                       # remove the saved keys",
				},
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "access-key-id", Usage: "access key id (the non-secret half)"},
					&cli.StringFlag{Name: "profile", Usage: "name the saved entry, so several repositories can share one login"},
					&cli.BoolFlag{Name: "forget", Usage: "remove the saved keys for this storage"},
				},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					if cmd.Args().Len() != 1 {
						return nil, usageError{"storage login needs the storage name", "fxvcs storage login <name> --access-key-id <id>   (see fxvcs storage list)"}
					}
					req := sdk.StorageLoginRequest{Path: dir, Remote: cmd.Args().First(), Profile: cmd.String("profile"), Forget: cmd.Bool("forget")}
					if !req.Forget {
						req.AccessKeyID = cmd.String("access-key-id")
						if strings.TrimSpace(req.AccessKeyID) == "" {
							return nil, usageError{"storage login needs --access-key-id", "fxvcs storage login " + req.Remote + " --access-key-id <id>"}
						}
						secret, err := readSecret(env, "Secret access key for storage "+req.Remote+": ")
						if err != nil {
							return nil, err
						}
						req.SecretAccessKey = secret
					}
					return c.StorageLogin(ctx, req)
				},
				Render: renderStorageLogin,
			}),
			command(env, cmdSpec{
				Name:     "list",
				Usage:    "List the storage this repository knows about and whether this computer can reach it",
				Examples: []string{"fxvcs storage list"},
				Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
					return c.StorageList(ctx, sdk.StorageListRequest{Path: dir})
				},
				Render: renderStorageList,
			}),
			command(env, cmdSpec{
				Name:      "test",
				Usage:     "Check that a storage can be read and written from this computer",
				ArgsUsage: "<name>",
				Examples:  []string{"fxvcs storage test primary"},
				Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
					if cmd.Args().Len() != 1 {
						return nil, usageError{"storage test needs the storage name", "fxvcs storage test <name>   (see fxvcs storage list)"}
					}
					return c.StorageTest(ctx, sdk.StorageTestRequest{Path: dir, Remote: cmd.Args().First()})
				},
				Render: renderStorageTest,
			}),
		),
	}
}

// allUnbound reports that every failed upload targets the same storage that
// is not set up on this computer, so one plain error replaces N identical rows.
func allUnbound(failed []sdk.ItemOutcome) (string, bool) {
	name := ""
	for _, f := range failed {
		if !strings.Contains(f.Reason, "no local binding on this machine") {
			return "", false
		}
		if name == "" {
			name = f.Path
		} else if name != f.Path {
			return "", false
		}
	}
	return name, name != ""
}

func remoteNames(items []sdk.ItemOutcome) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it.Path] {
			seen[it.Path] = true
			out = append(out, fmt.Sprintf("%q", it.Path))
		}
	}
	sort.Strings(out)
	return out
}

// assetRow aggregates the stored objects that belong to one asset. A large
// file is stored as a manifest plus many chunks, but people track files, so
// the listing is per file and mentions the pieces only when there is more
// than one.
type assetRow struct {
	path    string
	bytes   int64
	pieces  int
	pending int
	missing map[string]bool
	digest  string // only when the asset is a single object
}

func renderObjectsStatus(p *ui.Printer, v any, _ renderMeta) {
	res := v.(*sdk.ObjectsStatusResult)
	if len(res.Objects) == 0 {
		p.Header("objects status", "no asset data recorded on this computer yet")
		p.Hint("git add a tracked file to record it; fxvcs status shows tracked assets")
		return
	}
	rows := map[string]*assetRow{}
	var order []string
	for _, o := range res.Objects {
		path := firstReferrer(o.Referrers)
		if path == "" {
			path = "(no committed file refers to it)"
		}
		row := rows[path]
		if row == nil {
			row = &assetRow{path: path, missing: map[string]bool{}, digest: o.Digest}
			rows[path] = row
			order = append(order, path)
		} else {
			row.digest = ""
		}
		row.bytes += o.Bytes
		row.pieces++
		if o.Pending {
			row.pending++
		}
		for n, published := range o.Remotes {
			if !published {
				row.missing[n] = true
			}
		}
	}
	sort.Strings(order)
	uploaded, pending := 0, 0
	for _, path := range order {
		if rows[path].pending > 0 {
			pending++
		} else {
			uploaded++
		}
	}
	p.Header("objects status", fmt.Sprintf("%s recorded on this computer: %d uploaded, %d not yet uploaded", ui.Count(len(order), "asset"), uploaded, pending))
	p.Blank()
	items := make([]ui.Item, 0, len(order))
	for _, path := range order {
		row := rows[path]
		it := ui.Item{Path: row.path, Size: ui.Bytes(row.bytes)}
		var notes []string
		if row.pending > 0 {
			it.State, it.Label = ui.Pending, "not yet uploaded"
			if len(row.missing) > 0 {
				names := make([]string, 0, len(row.missing))
				for n := range row.missing {
					names = append(names, n)
				}
				sort.Strings(names)
				notes = append(notes, "missing from storage "+strings.Join(names, ", "))
			}
		} else {
			it.State, it.Label = ui.OK, "uploaded"
		}
		if p.Verbose() {
			switch {
			case row.digest != "":
				notes = append([]string{ui.ShortID(row.digest)}, notes...)
			default:
				notes = append([]string{fmt.Sprintf("%d pieces", row.pieces)}, notes...)
			}
		} else if row.pieces > 1 && row.pending > 0 {
			notes = append(notes, fmt.Sprintf("%d of %d pieces still to upload", row.pending, row.pieces))
		}
		it.Note = strings.Join(notes, " ")
		items = append(items, it)
	}
	p.Items(items)
	if pending > 0 {
		p.Footer("upload with: fxvcs objects publish   (git push is refused until then)")
	} else {
		p.Footer("everything is uploaded; git push is not blocked by fxvcs.")
	}
}

// firstReferrer turns "path@commit" referrers into the first path.
func firstReferrer(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	r := refs[0]
	if i := strings.LastIndex(r, "@"); i > 0 {
		r = r[:i]
	}
	return r
}

func renderPublish(p *ui.Printer, v any, m renderMeta) {
	res := v.(*sdk.ObjectsPublishResult)
	if len(res.Published) == 0 && len(res.Failed) == 0 {
		p.Header("objects publish", "nothing to upload; storage already has every asset")
		return
	}
	// Uploads are per stored object, but a chunked asset is one file split
	// into a manifest and many chunks. Report files, and mention the pieces
	// only when there is more than one.
	type remoteTotals struct {
		objects int
		bytes   int64
		assets  map[string]bool
	}
	perRemote := map[string]*remoteTotals{}
	var remotes []string
	for _, it := range res.Published {
		e := perRemote[it.Path]
		if e == nil {
			e = &remoteTotals{assets: map[string]bool{}}
			perRemote[it.Path] = e
			remotes = append(remotes, it.Path)
		}
		e.objects++
		e.bytes += it.Bytes
		for _, p := range it.Paths {
			e.assets[p] = true
		}
	}
	sort.Strings(remotes)
	var parts []string
	for _, r := range remotes {
		e := perRemote[r]
		n := len(e.assets)
		if n == 0 {
			n = e.objects
		}
		part := fmt.Sprintf("%s (%s)", ui.Count(n, "asset"), ui.Bytes(e.bytes))
		if e.objects > n {
			part += fmt.Sprintf(" in %d pieces", e.objects)
		}
		parts = append(parts, part+" to storage "+r)
	}
	summary := "uploaded " + strings.Join(parts, "; ")
	if len(res.Published) == 0 {
		summary = "uploaded nothing"
	}
	summary += " in " + elapsedText(m.Elapsed)
	p.Header("objects publish", summary)
	if len(res.Failed) > 0 {
		p.Blank()
		byRemote := map[string][]sdk.ItemOutcome{}
		var names []string
		for _, f := range res.Failed {
			if _, ok := byRemote[f.Path]; !ok {
				names = append(names, f.Path)
			}
			byRemote[f.Path] = append(byRemote[f.Path], f)
		}
		sort.Strings(names)
		for _, n := range names {
			p.Line("%s could not be uploaded to storage %s:", ui.Count(len(byRemote[n]), "asset"), n)
			items := make([]ui.Item, 0, len(byRemote[n]))
			for _, f := range byRemote[n] {
				path := "asset"
				if p.Verbose() {
					path = ui.ShortID(f.Digest)
				}
				items = append(items, ui.Item{State: ui.Fail, Label: "failed", Path: path, Note: humanReason(p.Verbose(), f.Reason)})
			}
			p.Items(items)
		}
		p.Footer(fmt.Sprintf("%d uploaded, %d failed.", len(res.Published), len(res.Failed)))
		return
	}
	p.Footer("git push is no longer blocked by fxvcs for these assets.")
}

func elapsedText(d time.Duration) string {
	d = d.Round(100 * time.Millisecond)
	if d == 0 {
		return "under 0.1s"
	}
	return d.String()
}

func renderCacheStatus(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.CacheStatusResult)
	p.Header("cache status", fmt.Sprintf("%s using %s on this computer", ui.Count(r.Objects, "asset"), ui.Bytes(r.TotalBytes)))
	p.Blank()
	p.Table([][]string{
		{"can be freed", ui.Bytes(r.EvictableBytes), fmt.Sprintf("(%s already uploaded)", ui.Count(r.Objects-r.Pinned, "asset"))},
		{"protected", ui.Bytes(r.PinnedBytes), fmt.Sprintf("(%s not yet uploaded)", ui.Count(r.Pinned, "asset"))},
	})
	p.Verbosef("  location: %s", r.CacheDir)
	switch {
	case r.LimitBytes > 0:
		p.Line("  trims itself back to %s", ui.Bytes(r.LimitBytes))
	default:
		p.Verbosef("  no size limit set (fxvcs cache limit 50GiB)")
	}
	if r.EvictableBytes > 0 {
		p.Footer("free space with: fxvcs cache prune --apply")
	}
	if r.Pinned > 0 {
		p.Hint("upload the protected assets first: fxvcs objects publish")
	}
}

func renderCacheLimit(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.CacheLimitResult)
	if r.LimitBytes == 0 {
		p.Header("cache limit", "the local asset cache has no size limit on this computer")
		p.Hint("free space when you want to: fxvcs cache prune --apply")
		return
	}
	p.Header("cache limit", fmt.Sprintf("the local asset cache will trim itself back to %s", ui.Bytes(r.LimitBytes)))
	p.Line("  it currently holds %s", ui.Bytes(r.CurrentBytes))
	p.Line("  asset data that is not yet uploaded is never removed")
}

func renderCacheVerify(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.CacheVerifyResult)
	if len(r.Corrupt) == 0 {
		p.Header("cache verify", fmt.Sprintf("%s checked (%s); everything matches its checksum",
			ui.Count(r.Checked, "cached item"), ui.Bytes(r.Bytes)))
		return
	}
	p.Header("cache verify", fmt.Sprintf("%s checked (%s); %s damaged",
		ui.Count(r.Checked, "cached item"), ui.Bytes(r.Bytes), ui.Count(len(r.Corrupt), "item")))
	p.Blank()
	items := make([]ui.Item, 0, len(r.Corrupt))
	for _, c := range r.Corrupt {
		it := ui.Item{State: ui.Fail, Label: "damaged", Path: ui.ShortID(c.Digest), Size: ui.Bytes(c.Bytes)}
		if c.Pinned {
			it.Note = "not yet uploaded — kept, because this is the only copy"
		}
		items = append(items, it)
	}
	p.Items(items)
	switch {
	case r.Removed > 0:
		p.Footer(fmt.Sprintf("%s removed; they will be downloaded again when needed.", ui.Count(r.Removed, "damaged copy")))
	default:
		p.Footer("nothing was removed. Delete the damaged copies with: fxvcs cache verify --repair")
	}
	if len(r.Kept) > 0 {
		p.Hint("damaged data that was never uploaded cannot be recovered by fxvcs; restore the file and run git add again")
	}
}

func renderCachePrune(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.CachePruneResult)
	var total int64
	failed := 0
	for _, e := range r.Evicted {
		if e.Skipped {
			failed++
			continue
		}
		total += e.Bytes
	}
	verb := "removed"
	if r.DryRun {
		verb = "would remove"
	}
	p.Header("cache prune", fmt.Sprintf("%s %s (%s); %s kept", verb, ui.Count(len(r.Evicted)-failed, "cached asset"), ui.Bytes(total), ui.Count(r.Kept, "protected asset")))
	if p.Verbose() {
		p.Blank()
		items := make([]ui.Item, 0, len(r.Evicted))
		for _, e := range r.Evicted {
			it := ui.Item{State: ui.OK, Label: verb, Path: ui.ShortID(e.Digest), Size: ui.Bytes(e.Bytes)}
			if e.Skipped {
				it.State, it.Label, it.Note = ui.Fail, "failed", e.Reason
			}
			items = append(items, it)
		}
		p.Items(items)
	}
	if failed > 0 {
		p.Footer(fmt.Sprintf("%s could not be removed; run again or check the cache folder permissions.", ui.Count(failed, "item")))
	}
	if r.DryRun && len(r.Evicted) > 0 {
		p.Footer("nothing was removed. Apply with: fxvcs cache prune --apply")
	}
}

func remoteLocation(r sdk.StorageRemoteSpec) string {
	switch {
	case r.LocalPath != "":
		return r.LocalPath
	case r.Bucket != "":
		loc := r.Bucket
		if r.Prefix != "" {
			loc += "/" + r.Prefix
		}
		if r.Endpoint != "" {
			loc += " @ " + r.Endpoint
		}
		return loc
	}
	return "-"
}

// storageType guesses the backend from what the operator supplied, so the
// common cases need no --type at all.
func storageType(cmd *cli.Command) string {
	if t := cmd.String("type"); t != "" {
		return t
	}
	switch {
	case cmd.String("bucket") != "":
		return "s3"
	case cmd.String("path") != "":
		return "filesystem"
	}
	return ""
}

func renderStorageLogin(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.StorageLoginResult)
	if r.Removed {
		p.Header("storage login", fmt.Sprintf("forgot the access keys for storage %q on this computer", r.Remote))
		p.Verbosef("  entry %s removed from %s", r.Profile, r.CredentialsFile)
		return
	}
	p.Header("storage login", fmt.Sprintf("saved the access keys for storage %q on this computer", r.Remote))
	p.Line("  access key id %s", r.AccessKeyID)
	p.Line("  stored in %s (readable only by you; never committed)", r.CredentialsFile)
	p.Verbosef("  entry %s", r.Profile)
	p.Hint("check it works: fxvcs storage test " + r.Remote)
}

func renderStorageAdd(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.StorageAddResult).Remote
	scope := "shared with your team (declared in .fxvcs/repository.yaml)"
	if r.Local {
		scope = "private to this clone"
	}
	p.Header("storage add", fmt.Sprintf("storage %q is set up on this computer at %s", r.Name, remoteLocation(r)))
	p.Line("  %s", scope)
	if r.RequiredForPublication {
		p.Line("  asset data must be uploaded here before git push")
	}
	if r.Committed && !r.Local {
		p.Footer("if .fxvcs/repository.yaml changed, commit it so your team gets the same storage: git add .fxvcs && git commit")
	}
	p.Hint("check it works: fxvcs storage test " + r.Name)
}

func renderStorageList(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.StorageListResult)
	if len(r.Remotes) == 0 {
		p.Header("storage list", "no storage configured yet")
		p.Hint("fxvcs storage add --name primary --path <folder shared with your team>")
		return
	}
	p.Header("storage list", ui.Plural(len(r.Remotes), "storage location", "storage locations"))
	p.Blank()
	rows := [][]string{{"NAME", "TYPE", "REQUIRED BEFORE PUSH", "THIS COMPUTER", "LOCATION"}}
	var unbound []sdk.StorageRemoteSpec
	for _, rem := range r.Remotes {
		req := "no"
		if rem.RequiredForPublication {
			req = "yes"
		}
		here := "set up"
		switch {
		case !rem.Bound:
			here = "not set up"
			unbound = append(unbound, rem)
		case rem.Type == "s3" && !rem.HasCredentials:
			// The bucket is known; whether this machine can sign for it is a
			// separate question, and one worth saying out loud.
			here = "set up, no saved keys"
		}
		if rem.Local {
			here += " (private to this clone)"
		}
		rows = append(rows, []string{rem.Name, rem.Type, req, here, remoteLocation(rem)})
	}
	p.Table(rows)
	for _, rem := range r.Remotes {
		if rem.Bound && rem.Type == "s3" && !rem.HasCredentials {
			p.Hint(fmt.Sprintf("storage %q has no saved keys on this computer; if it does not use an instance role or environment keys, run: fxvcs storage login %s --access-key-id <id>", rem.Name, rem.Name))
		}
	}
	if len(unbound) > 0 {
		p.Footer(fmt.Sprintf("%s not set up on this computer.", ui.Plural(len(unbound), "storage location is", "storage locations are")))
		for _, rem := range unbound {
			p.Hint(setupHint(rem))
		}
	}
}

// setupHint is the exact command that would connect this computer to a
// storage location that is declared but unusable here.
func setupHint(rem sdk.StorageRemoteSpec) string {
	if rem.Type == "s3" {
		return fmt.Sprintf("fxvcs storage add --name %s --bucket <bucket> [--endpoint <url>]", rem.Name)
	}
	return fmt.Sprintf("fxvcs storage add --name %s --path <folder shared with your team>", rem.Name)
}

func renderStorageTest(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.StorageTestResult)
	ok := 0
	for _, c := range r.Capabilities {
		if c.Supported {
			ok++
		}
	}
	if ok == len(r.Capabilities) {
		p.Header("storage test", fmt.Sprintf("storage %q works from this computer (%d checks passed)", r.Remote, ok))
	} else {
		p.Header("storage test", fmt.Sprintf("storage %q has problems (%d of %d checks failed)", r.Remote, len(r.Capabilities)-ok, len(r.Capabilities)))
	}
	p.Blank()
	items := make([]ui.Item, 0, len(r.Capabilities))
	for _, c := range r.Capabilities {
		it := ui.Item{State: ui.OK, Label: "ok", Path: c.Name, Note: c.Detail}
		if !c.Supported {
			it.State, it.Label = ui.Fail, "failed"
		}
		items = append(items, it)
	}
	p.Items(items)
	if ok != len(r.Capabilities) {
		p.Footer("fix the folder permissions, or point the storage elsewhere: fxvcs storage add --name " + r.Remote + " --path <folder>")
	}
}

// statsCommand reports what content addressing saves in this repository.
func statsCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "stats",
		Category: catMore,
		Usage:    "Show how much space this repository saves by storing each piece of content once",
		Examples: []string{
			"fxvcs stats",
			"fxvcs stats --top 10        # the most-reused content",
			"fxvcs stats --fetch         # examine assets whose details are not stored here yet",
		},
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "top", Usage: "also list the N most-reused pieces of content"},
			&cli.BoolFlag{Name: "fetch", Usage: "download the details of assets this computer has not seen (one request each)"},
		},
		Op: func(ctx context.Context, c *sdk.Client, dir string, cmd *cli.Command) (any, error) {
			return c.Stats(ctx, sdk.StatsRequest{Path: dir, Fetch: cmd.Bool("fetch"), TopShared: cmd.Int("top")})
		},
		Render: renderStats,
	})
}

func renderStats(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.StatsResult)
	if r.Assets == 0 {
		p.Header("stats", "no tracked assets yet")
		p.Hint("track some files first: fxvcs track --glob '**/*.ytd'")
		return
	}
	p.Header("stats", fmt.Sprintf("%s in %s of distinct content",
		ui.Count(r.Assets, "tracked asset"), ui.Bytes(r.StoredBytes)))
	p.Blank()
	rows := [][]string{
		{"as ordinary files", ui.Bytes(r.LogicalBytes), fmt.Sprintf("%s across %s", ui.Count(r.Assets, "file"), ui.Count(r.Contents, "distinct content"))},
		{"as stored", ui.Bytes(r.StoredBytes), ""},
	}
	switch {
	case r.SavedBytes > 0:
		rows = append(rows, []string{"saved", ui.Bytes(r.SavedBytes), fmt.Sprintf("%.0f%% smaller", r.SavedPercent)})
	case r.SavedBytes < 0:
		// Storing an asset in pieces costs a small index of those pieces. When
		// nothing in the repository repeats, that index is the whole result,
		// and saying so is more useful than omitting the row.
		rows = append(rows, []string{"overhead", ui.Bytes(-r.SavedBytes),
			fmt.Sprintf("%.2f%%: nothing here repeats yet, so the piece lists are all it costs", -r.SavedPercent)})
	}
	p.Table(rows)
	if r.IdenticalFiles > 0 || r.FilesSharingParts > 0 {
		p.Blank()
		p.Line("  where the saving comes from:")
		var why [][]string
		if r.IdenticalFiles > 0 {
			why = append(why, []string{"  identical files", ui.Bytes(r.IdenticalFileBytes),
				fmt.Sprintf("%s are a copy of another file", ui.Count(r.IdenticalFiles, "file"))})
		}
		if r.SharedPartBytes > 0 {
			why = append(why, []string{"  shared parts", ui.Bytes(r.SharedPartBytes),
				fmt.Sprintf("%s share pieces with another file", ui.Count(r.FilesSharingParts, "file"))})
		}
		p.Table(why)
	}
	p.Verbosef("  %d assets stored in pieces, %d stored whole, %d shared pieces", r.ChunkedAssets, r.WholeAssets, r.SharedObjects)
	if len(r.TopShared) > 0 {
		p.Blank()
		p.Line("  most reused content:")
		items := make([]ui.Item, 0, len(r.TopShared))
		for _, s := range r.TopShared {
			items = append(items, ui.Item{
				State: ui.OK, Label: "saves " + ui.Bytes(s.SavedBytes),
				Path: firstOf(s.Paths), Size: ui.Bytes(s.Bytes),
				Note: fmt.Sprintf("used by %s", ui.Count(s.Users, "file")),
			})
		}
		p.Items(items)
	}
	if r.Unresolved > 0 {
		p.Footer(fmt.Sprintf("%s could not be examined because their details are not stored on this computer; the saving above is a lower bound.",
			ui.Count(r.Unresolved, "asset")))
		p.Hint("include them with: fxvcs stats --fetch")
	}
}

func firstOf(paths []string) string {
	if len(paths) == 0 {
		return "(no path)"
	}
	if len(paths) == 1 {
		return paths[0]
	}
	return fmt.Sprintf("%s and %d more", paths[0], len(paths)-1)
}

// infoCommand shows repository identity and internals (the one place ids and
// internal names are meant to appear).
func infoCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:     "info",
		Category: catMore,
		Usage:    "Show repository name, profile, ids, storage, and Git driver setup",
		Examples: []string{"fxvcs info"},
		Op: func(ctx context.Context, c *sdk.Client, dir string, _ *cli.Command) (any, error) {
			return c.Info(ctx, sdk.InfoRequest{Path: dir})
		},
		Render: renderInfo,
	})
}

func renderInfo(p *ui.Printer, v any, _ renderMeta) {
	r := v.(*sdk.InfoResult)
	profile := r.Profile
	if profile == "fivem" {
		profile = "fivem (FiveM repository)"
	}
	p.Header("info", r.Name)
	p.Blank()
	rows := [][]string{
		{"repository", r.RepositoryRoot},
		{"name", r.Name},
		{"profile", profile},
		{"repository id", r.RepositoryID},
		{"storage domain id", r.StorageDomainID},
		{"needs fxvcs", r.MinimumFXVCSVersion + " or newer (installed " + r.InstalledVersion + ")"},
		{"git integration", fmt.Sprintf("generation %d", r.GitIntegrationVersion)},
	}
	if r.Descriptor != nil {
		rows = append(rows, []string{"resources", fmt.Sprintf("%s layout, source root %s", r.Descriptor.Layout, r.Descriptor.SourceRoot)})
	}
	launcher := r.LauncherPath
	if launcher == "" {
		launcher = "not recorded (run fxvcs setup)"
	}
	rows = append(rows, []string{"launcher", launcher})
	driver := "not configured (run fxvcs setup)"
	if r.GitDriverConfigured {
		driver = "configured"
	}
	rows = append(rows, []string{"git driver", driver})
	hook := "not installed (run fxvcs setup)"
	switch {
	case r.PrePushHookForeign:
		hook = "another pre-push hook is present; fxvcs is not chained (see fxvcs setup)"
	case r.PrePushHookInstalled:
		hook = "installed"
	}
	rows = append(rows, []string{"pre-push check", hook})
	p.Table(rows)
	p.Blank()
	if len(r.Remotes) == 0 {
		p.Line("  storage: none configured (fxvcs storage add)")
		return
	}
	p.Line("  storage:")
	srows := [][]string{}
	for _, rem := range r.Remotes {
		req := "optional"
		if rem.RequiredForPublication {
			req = "required before push"
		}
		here := "set up on this computer"
		if !rem.Bound {
			here = "not set up on this computer"
		}
		srows = append(srows, []string{"  " + rem.Name, rem.Type, req, here, remoteLocation(rem)})
	}
	p.Table(srows)
}
