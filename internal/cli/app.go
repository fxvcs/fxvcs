// Package cli builds the fxvcs command tree. Commands are presentation
// adapters: they parse flags, call a typed SDK operation, render the result
// (human or --json), and choose an exit code. No business logic lives here.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
)

// Exit codes are part of the automation contract.
const (
	ExitOK              = 0
	ExitFailure         = 1
	ExitUsage           = 2
	ExitUpgradeRequired = 3
	ExitNotImplemented  = 4
	ExitBusy            = 5
)

// Env carries process I/O so tests can drive the app without a real terminal.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// Getwd resolves the working directory (defaults to os.Getwd).
	Getwd func() (string, error)
	// IsTerminal decides whether a writer is an interactive terminal
	// (defaults to a character-device check). Tests set it to force the
	// live-progress and colour paths on.
	IsTerminal func(io.Writer) bool
	// Extra registers additional top-level commands (filter/diff/merge drivers,
	// spikes) supplied by other packages so this package has no dependency on
	// their engines.
	Extra []*cli.Command
}

func (env Env) withDefaults() Env {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.Getwd == nil {
		env.Getwd = os.Getwd
	}
	if env.IsTerminal == nil {
		env.IsTerminal = ui.IsTerminal
	}
	return env
}

// Command categories, rendered in this order by rootHelpTemplate.
const (
	catDaily   = "Daily"
	catSetup   = "Set up"
	catFix     = "Fix"
	catSharing = "Sharing"
	catFiveM   = "FiveM"
	catMore    = "More"
)

// New builds the root command.
func New(env Env) *cli.Command {
	env = env.withDefaults()
	root := &cli.Command{
		Name:                  "fxvcs",
		Usage:                 "Version FiveM assets alongside your Git repository",
		Version:               sdk.Version,
		EnableShellCompletion: true,
		Reader:                env.Stdin,
		Writer:                env.Stdout,
		ErrWriter:             env.Stderr,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "print machine-readable JSON instead of text (stable; for scripts)"},
			&cli.StringFlag{Name: "C", Usage: "run as if fxvcs was started in `DIR`"},
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "print errors only"},
			&cli.BoolFlag{Name: "verbose", Usage: "also show digests, error codes, and internal paths"},
			&cli.StringFlag{Name: "color", Value: "auto", Usage: "colour and symbols: auto (on a terminal), always, or never"},
			&cli.BoolFlag{Name: "no-color", Usage: "same as --color=never"},
		},
		Commands:                      append(coreCommands(env), env.Extra...),
		CustomRootCommandHelpTemplate: rootHelpTemplate,
		HideHelpCommand:               true,
	}
	root.Action = groupAction
	root.OnUsageError = onUsageError
	for _, c := range env.Extra {
		if c.Category == "" {
			c.Category = catMore
		}
	}
	return root
}

// globals are the presentation flags, readable before and after parsing.
type globals struct {
	JSON    bool
	Quiet   bool
	Verbose bool
	Color   string
}

func globalsFromArgs(args []string) globals {
	g := globals{Color: "auto"}
	for _, a := range args {
		switch {
		case a == "--json":
			g.JSON = true
		case a == "--quiet" || a == "-q":
			g.Quiet = true
		case a == "--verbose":
			g.Verbose = true
		case a == "--no-color":
			g.Color = "never"
		case strings.HasPrefix(a, "--color="):
			g.Color = strings.TrimPrefix(a, "--color=")
		}
	}
	return g
}

func globalsFromCommand(cmd *cli.Command) globals {
	root := cmd.Root()
	g := globals{JSON: root.Bool("json"), Quiet: root.Bool("quiet"), Verbose: root.Bool("verbose"), Color: root.String("color")}
	if root.Bool("no-color") {
		g.Color = "never"
	}
	return g
}

// printer builds the human printer for a command invocation.
func printer(env Env, cmd *cli.Command) *ui.Printer {
	g := globalsFromCommand(cmd)
	return ui.New(env.Stdout, ui.Options{Color: ui.Detect(g.Color, env.Stdout, env.IsTerminal), Quiet: g.Quiet, Verbose: g.Verbose})
}

// Run executes the app and maps errors to exit codes. It never panics on SDK
// errors and never prints secrets.
func Run(ctx context.Context, env Env, args []string) int {
	env = env.withDefaults()
	app := New(env)
	err := app.Run(ctx, args)
	if err == nil {
		return ExitOK
	}
	g := globalsFromArgs(args)
	code := sdk.CodeOf(err)
	if g.JSON {
		var se *sdk.Error
		payload := map[string]any{"error": map[string]any{"code": code, "message": err.Error()}}
		if errors.As(err, &se) {
			if se.Details != nil {
				payload["error"].(map[string]any)["details"] = se.Details
			}
			if se.Hint != "" {
				payload["error"].(map[string]any)["hint"] = se.Hint
			}
		}
		_ = json.NewEncoder(env.Stderr).Encode(payload)
	} else {
		printError(env.Stderr, err, g.Verbose)
	}
	return exitCode(err)
}

// exitCode maps an error to the documented process exit code.
func exitCode(err error) int {
	switch sdk.CodeOf(err) {
	case sdk.CodeUpgradeRequired:
		return ExitUpgradeRequired
	case sdk.CodeNotImplemented:
		return ExitNotImplemented
	case sdk.CodeBusy:
		return ExitBusy
	case sdk.CodeInvalidArgument:
		return ExitUsage
	}
	var ue usageError
	if errors.As(err, &ue) {
		return ExitUsage
	}
	return ExitFailure
}

// printError writes "error: <cause>" and "  hint: <command>" to stderr; the
// SDK code appears only under --verbose.
func printError(w io.Writer, err error, verbose bool) {
	cause, hint := describeError(err, verbose)
	fmt.Fprintf(w, "error: %s\n", cause)
	if hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", hint)
	}
	if verbose {
		if code := sdk.CodeOf(err); code != sdk.CodeUnknown {
			fmt.Fprintf(w, "  code: %s\n", code)
		}
	}
}

// usageError is a command-line usage mistake (exit 2).
type usageError struct {
	msg  string
	hint string
}

func (u usageError) Error() string { return u.msg }

// client constructs the SDK client for a command invocation, wiring progress
// rendering according to the presentation flags.
func client(env Env, cmd *cli.Command) (*sdk.Client, *progressView, error) {
	g := globalsFromCommand(cmd)
	var view *progressView
	opts := sdk.Options{}
	if !g.JSON && !g.Quiet {
		view = newProgressView(env.Stderr, env.IsTerminal(env.Stderr), ui.Detect(g.Color, env.Stderr, env.IsTerminal))
		opts.Progress = view.handle
	}
	c, err := sdk.New(opts)
	if err != nil {
		if view != nil {
			view.finish(err)
		}
		return nil, nil, err
	}
	return c, view, nil
}

// cwd resolves the working directory honoring -C.
func cwd(env Env, cmd *cli.Command) (string, error) {
	if c := cmd.Root().String("C"); c != "" {
		return c, nil
	}
	return env.Getwd()
}

// render writes a result as JSON when --json is set, otherwise via human.
func render(env Env, cmd *cli.Command, v any, human func(p *ui.Printer)) error {
	if cmd.Root().Bool("json") {
		enc := json.NewEncoder(env.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	p := printer(env, cmd)
	if p.Quiet() {
		return nil
	}
	human(p)
	return nil
}
