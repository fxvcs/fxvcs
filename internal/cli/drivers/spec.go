package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/schema"
	"fxvcs.dev/fxvcs/sdk"
)

func init() {
	Register(&cli.Command{
		Name:     "spec",
		Category: "More",
		Usage:    "Validate fxvcs documents against the built-in schemas",
		Commands: []*cli.Command{
			{
				Name:      "validate",
				Usage:     "Validate documents against the embedded FXVCS schemas and semantic rules",
				ArgsUsage: "<file>...",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "kind", Usage: "expected document `KIND` (default: detect from the envelope; required for schema-only kinds)"},
				},
				Action: specValidate,
			},
			{
				Name:  "kinds",
				Usage: "List every document kind the binary can validate",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					for _, k := range schema.KnownKinds() {
						file, _ := schema.SchemaFile(k)
						fmt.Fprintf(cmd.Root().Writer, "%-24s %s\n", k, file)
					}
					return nil
				},
			},
		},
	})
}

type fileResult struct {
	File   string        `json:"file"`
	Kind   string        `json:"kind,omitempty"`
	Valid  bool          `json:"valid"`
	Issues schema.Issues `json:"issues,omitempty"`
}

func specValidate(ctx context.Context, cmd *cli.Command) error {
	files := cmd.Args().Slice()
	if len(files) == 0 {
		return sdk.NewError(sdk.CodeInvalidArgument, "spec validate needs at least one file").WithHint("fxvcs spec validate <file>...")
	}
	kind := cmd.String("kind")
	jsonMode := cmd.Root().Bool("json")
	out := cmd.Root().Writer
	var results []fileResult
	failed := 0
	for _, f := range files {
		r := fileResult{File: f, Kind: kind}
		data, err := os.ReadFile(f)
		if err != nil {
			r.Issues = schema.Issues{{Message: err.Error()}}
		} else {
			if r.Kind == "" {
				if env, err := schema.DetectKind(data); err == nil {
					r.Kind = env.Kind
				}
			}
			r.Issues = schema.Validate(kind, data)
		}
		r.Valid = len(r.Issues) == 0
		if !r.Valid {
			failed++
		}
		results = append(results, r)
		if !jsonMode {
			if r.Valid {
				fmt.Fprintf(out, "ok   %s (%s)\n", f, r.Kind)
				continue
			}
			fmt.Fprintf(out, "FAIL %s (%s)\n", f, displayKind(r.Kind))
			for _, iss := range r.Issues {
				fmt.Fprintf(out, "     %s\n", iss.Error())
			}
			if errors.Is(r.Issues, format.ErrNewerAPIVersion) {
				fmt.Fprintf(out, "     hint: this document was written by a newer fxvcs; run: fxvcs upgrade --check\n")
			}
		}
	}
	if jsonMode {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("spec validate: %d of %d document(s) invalid", failed, len(files))
	}
	return nil
}

func displayKind(k string) string {
	if strings.TrimSpace(k) == "" {
		return "unknown kind"
	}
	return k
}
