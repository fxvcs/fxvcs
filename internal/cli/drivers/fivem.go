package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
	"fxvcs.dev/fxvcs/internal/profile/fivem/manifest"
	"fxvcs.dev/fxvcs/sdk"
)

// fivemScanResult is the --json shape of `fxvcs fivem scan`.
type fivemScanResult struct {
	Root       string                         `json:"root"`
	Detection  *discovery.Detection           `json:"detection,omitempty"`
	Skipped    []discovery.Skipped            `json:"skipped,omitempty"`
	Descriptor domain.ComponentDescriptor     `json:"descriptor"`
	Resources  []discovery.DiscoveredResource `json:"resources"`
}

func mapFivemError(err error) error {
	if err == nil {
		return nil
	}
	var de *discovery.Error
	if errors.As(err, &de) {
		code := sdk.CodeValidation
		switch de.Reason {
		case discovery.ReasonIO:
			code = sdk.CodeIO
		case discovery.ReasonNoManifests:
			code = sdk.CodeNotFound
		}
		return sdk.WrapError(code, err.Error(), err)
	}
	if errors.Is(err, manifest.ErrLimit) || errors.Is(err, manifest.ErrForbidden) ||
		errors.Is(err, manifest.ErrUnknownDirective) || errors.Is(err, manifest.ErrBadArgument) ||
		errors.Is(err, manifest.ErrInvalid) || errors.Is(err, manifest.ErrSyntax) || errors.Is(err, manifest.ErrRuntime) {
		return sdk.WrapError(sdk.CodeValidation, err.Error(), err)
	}
	return sdk.WrapError(sdk.CodeUnknown, err.Error(), err)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func init() {
	Register(&cli.Command{
		Name:     "fivem",
		Category: "FiveM",
		Usage:    "Look at a resource folder before init: find resources and read their manifests safely",
		Commands: []*cli.Command{
			{
				Name:        "scan",
				Usage:       "Find every resource under a folder and show what its manifest declares",
				Description: "Examples:\n   fxvcs fivem scan .\n   fxvcs fivem scan --source-root resources --layout collection ./server-data",
				ArgsUsage:   "<dir>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "source-root", Usage: "accepted sourceRoot (skips detection)"},
					&cli.StringFlag{Name: "layout", Usage: "accepted layout: single | collection (with --source-root)"},
					&cli.StringFlag{Name: "resource-name", Usage: "resource name for layout single"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					dir := cmd.Args().First()
					if dir == "" {
						dir = "."
					}
					if c := cmd.Root().String("C"); c != "" && !filepath.IsAbs(dir) {
						dir = filepath.Join(c, dir)
					}
					abs, err := filepath.Abs(dir)
					if err != nil {
						return sdk.WrapError(sdk.CodeIO, err.Error(), err)
					}
					res := fivemScanResult{Root: abs}
					if sr := cmd.String("source-root"); sr != "" || cmd.String("layout") != "" {
						if sr == "" {
							sr = "."
						}
						res.Descriptor = domain.ComponentDescriptor{SourceRoot: sr, Layout: cmd.String("layout")}
						if n := cmd.String("resource-name"); n != "" {
							res.Descriptor.ResourceName = &n
						}
					} else {
						det, err := discovery.DetectDir(abs)
						if err != nil {
							return mapFivemError(err)
						}
						res.Detection = &det
						res.Descriptor = det.Descriptor
					}
					scan, err := discovery.ScanTree(ctx, os.DirFS(abs), res.Descriptor, discovery.ScanOptions{})
					if err != nil {
						return mapFivemError(err)
					}
					list := scan.Resources
					res.Resources = list
					res.Skipped = scan.Skipped
					out := cmd.Root().Writer
					if cmd.Root().Bool("json") {
						return writeJSON(out, res)
					}
					name := ""
					if res.Descriptor.ResourceName != nil {
						name = " resourceName=" + *res.Descriptor.ResourceName
					}
					fmt.Fprintf(out, "descriptor: sourceRoot=%s layout=%s%s\n", res.Descriptor.SourceRoot, res.Descriptor.Layout, name)
					if res.Detection != nil && res.Detection.ResourceNameProposed {
						fmt.Fprintln(out, "note: resourceName is a proposal from the directory name; review before committing")
					}
					for _, sk := range res.Skipped {
						fmt.Fprintf(out, "ignored: %s/%s: %s\n", sk.Path, sk.File, sk.Detail)
					}
					for _, r := range list {
						m := r.Manifest
						extra := []string{}
						if len(m.Provides) > 0 {
							extra = append(extra, "provides="+strings.Join(m.Provides, ","))
						}
						if len(m.Dependencies) > 0 {
							extra = append(extra, "dependencies="+strings.Join(m.Dependencies, ","))
						}
						if m.ServerOnly {
							extra = append(extra, "server_only")
						}
						if m.LegacyFormat {
							extra = append(extra, "legacy")
						}
						fmt.Fprintf(out, "%-32s %-20s games=%s %s\n", r.Path, r.Name, strings.Join(m.Games, ","), strings.Join(extra, " "))
					}
					return nil
				},
			},
			{
				Name:        "manifest",
				Usage:       "Read one fxmanifest.lua/__resource.lua safely and print what it declares",
				Description: "Examples:\n   fxvcs fivem manifest '[maps]/city/fxmanifest.lua'",
				ArgsUsage:   "<file>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					file := cmd.Args().First()
					if file == "" {
						return sdk.WrapError(sdk.CodeInvalidArgument, "manifest file required", nil)
					}
					if c := cmd.Root().String("C"); c != "" && !filepath.IsAbs(file) {
						file = filepath.Join(c, file)
					}
					src, err := os.ReadFile(file)
					if err != nil {
						return sdk.WrapError(sdk.CodeIO, err.Error(), err)
					}
					base := filepath.Base(file)
					opts := manifest.Options{
						Legacy:      base == discovery.LegacyManifest,
						DefaultName: filepath.Base(filepath.Dir(file)),
						ChunkName:   base,
					}
					m, err := manifest.Evaluate(ctx, src, opts)
					if err != nil {
						return mapFivemError(err)
					}
					out := cmd.Root().Writer
					if cmd.Root().Bool("json") {
						return writeJSON(out, m)
					}
					fmt.Fprintf(out, "name: %s\nfx_version: %s\ngames: %s\nprovides: %s\ndependencies: %s\nserver_only: %v\nclient_scripts: %d\nserver_scripts: %d\nshared_scripts: %d\nfiles: %d\ndirectives: %d\n",
						m.Name, m.FxVersion, strings.Join(m.Games, ","), strings.Join(m.Provides, ","), strings.Join(m.Dependencies, ","), m.ServerOnly,
						len(m.ClientScripts), len(m.ServerScripts), len(m.SharedScripts), len(m.Files), m.Directives)
					if len(m.Ignored) > 0 {
						fmt.Fprintf(out, "ignored: %s\n", strings.Join(m.Ignored, ","))
					}
					return nil
				},
			},
		},
	})
}
