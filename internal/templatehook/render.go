// Package templatehook implements the built-in, in-process template.v1 hook.
// It has no command, file, URL, or network primitives and writes only through
// the realization containment boundary.
package templatehook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"

	"fxvcs.dev/fxvcs/internal/durablefs"
	"fxvcs.dev/fxvcs/internal/fileprotect"
	"fxvcs.dev/fxvcs/internal/layout"
)

var (
	ErrTemplateTooLarge = errors.New("templatehook: template exceeds limit")
	ErrOutputTooLarge   = errors.New("templatehook: output exceeds limit")
	ErrUnsafeMode       = errors.New("templatehook: unsafe output mode")
	ErrDestination      = errors.New("templatehook: destination already exists")
	ErrSecretEncoding   = errors.New("templatehook: secret input requires a context encoder")
	ErrCfgValue         = errors.New("templatehook: invalid cfg value")
	ErrFunction         = errors.New("templatehook: function is not allowlisted")
)

const DefaultMaxTemplateBytes int64 = 1 << 20

// Renderer bounds both template source and output. Inputs are plain strings;
// callers resolve typed Target/Release/literal/secret sources before rendering.
type Renderer struct {
	MaxTemplateBytes int64
	MaxOutputBytes   int64
}

// Input retains sensitivity until template validation. Secret values cannot be
// stringified directly; cfgQuote is the only v1 context encoder that may read
// their plaintext.
type Input struct {
	value  string
	secret bool
}

func PublicInput(value string) Input { return Input{value: value} }

func SecretInput(value string) Input { return Input{value: value, secret: true} }

func (i Input) String() string {
	if i.secret {
		return "[secret requires cfgQuote]"
	}
	return i.value
}

type renderData struct {
	Inputs map[string]Input
}

// Render executes template.v1 with missing-key failure and a fixed function
// allowlist. It exposes only .Inputs; there are no I/O, environment, network,
// subprocess, or dynamic-include primitives.
func (r Renderer) Render(name string, source []byte, inputs map[string]Input) ([]byte, error) {
	maxTemplate := r.MaxTemplateBytes
	if maxTemplate <= 0 {
		maxTemplate = DefaultMaxTemplateBytes
	}
	if int64(len(source)) > maxTemplate {
		return nil, ErrTemplateTooLarge
	}
	if r.MaxOutputBytes <= 0 {
		return nil, errors.New("templatehook: positive output limit required")
	}
	tmpl, err := template.New(name).Funcs(template.FuncMap{"cfgQuote": cfgQuote}).Option("missingkey=error").Parse(string(source))
	if err != nil {
		return nil, fmt.Errorf("templatehook: parse: %w", err)
	}
	if err := validateSecretUses(tmpl.Root, inputs); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	bounded := &limitWriter{writer: &output, remaining: r.MaxOutputBytes}
	if err := tmpl.Execute(bounded, renderData{Inputs: inputs}); err != nil {
		return nil, fmt.Errorf("templatehook: execute: %w", err)
	}
	return output.Bytes(), nil
}

func cfgQuote(input Input) (string, error) {
	if strings.ContainsAny(input.value, "\x00\r\n") {
		return "", ErrCfgValue
	}
	escaped := strings.ReplaceAll(input.value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`, nil
}

func validateSecretUses(node parse.Node, inputs map[string]Input) error {
	if node == nil {
		return nil
	}
	switch n := node.(type) {
	case *parse.ListNode:
		for _, child := range n.Nodes {
			if err := validateSecretUses(child, inputs); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return validateSecretPipe(n.Pipe, inputs)
	case *parse.IfNode:
		if err := validateSecretPipe(n.Pipe, inputs); err != nil {
			return err
		}
		if err := validateSecretUses(n.List, inputs); err != nil {
			return err
		}
		return validateSecretUses(n.ElseList, inputs)
	case *parse.RangeNode:
		if err := validateSecretPipe(n.Pipe, inputs); err != nil {
			return err
		}
		if err := validateSecretUses(n.List, inputs); err != nil {
			return err
		}
		return validateSecretUses(n.ElseList, inputs)
	case *parse.WithNode:
		if err := validateSecretPipe(n.Pipe, inputs); err != nil {
			return err
		}
		if err := validateSecretUses(n.List, inputs); err != nil {
			return err
		}
		return validateSecretUses(n.ElseList, inputs)
	case *parse.TemplateNode:
		return validateSecretPipe(n.Pipe, inputs)
	}
	return nil
}

func validateSecretPipe(pipe *parse.PipeNode, inputs map[string]Input) error {
	if pipe == nil {
		return nil
	}
	for _, command := range pipe.Cmds {
		cfgCommand := len(command.Args) == 2
		identifier, hasFunction := command.Args[0].(*parse.IdentifierNode)
		if hasFunction && identifier.Ident != "cfgQuote" {
			return fmt.Errorf("%w: %s", ErrFunction, identifier.Ident)
		}
		if !hasFunction || identifier.Ident != "cfgQuote" {
			cfgCommand = false
		}
		for index, argument := range command.Args {
			field, ok := argument.(*parse.FieldNode)
			if !ok || len(field.Ident) != 2 || field.Ident[0] != "Inputs" {
				continue
			}
			name := field.Ident[1]
			if input, exists := inputs[name]; exists && input.secret && (!cfgCommand || index != 1) {
				return fmt.Errorf("%w: .Inputs.%s must be passed directly to cfgQuote", ErrSecretEncoding, name)
			}
		}
	}
	return nil
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		if w.remaining > 0 {
			n, _ := w.writer.Write(p[:int(w.remaining)])
			w.remaining = 0
			return n, ErrOutputTooLarge
		}
		return 0, ErrOutputTooLarge
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type WriteOptions struct {
	RealizationDir         string
	AllowedDestinationRoot string
	Destination            string
	Mode                   string
	ContainsSecret         bool
}

// Write atomically publishes rendered bytes without replacement. Executable
// or group/other-writable output is always refused; secret-bearing output must
// be owner-only.
func Write(options WriteOptions, rendered []byte) (string, error) {
	destination, err := layout.HookDestination(options.RealizationDir, options.AllowedDestinationRoot, options.Destination)
	if err != nil {
		return "", err
	}
	mode64, err := strconv.ParseUint(options.Mode, 8, 32)
	if err != nil || mode64 > 0o777 {
		return "", fmt.Errorf("%w: %q", ErrUnsafeMode, options.Mode)
	}
	mode := os.FileMode(mode64)
	if mode&0o111 != 0 || mode&0o022 != 0 || (options.ContainsSecret && mode&0o077 != 0) {
		return "", fmt.Errorf("%w: %s", ErrUnsafeMode, mode)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("%w: %s", ErrDestination, destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	tmp, err := os.CreateTemp(parent, ".render-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(rendered); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if options.ContainsSecret {
		if err := fileprotect.Protect(tmpPath); err != nil {
			return "", err
		}
	}
	if err := os.Link(tmpPath, destination); err != nil {
		if _, statErr := os.Lstat(destination); statErr == nil {
			return "", fmt.Errorf("%w: %s", ErrDestination, destination)
		}
		return "", err
	}
	if err := durablefs.SyncDir(parent); err != nil {
		return "", err
	}
	return destination, nil
}
