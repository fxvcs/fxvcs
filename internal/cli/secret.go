package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// readSecret collects a secret from the operator.
//
// Secrets are never accepted as command-line flags: arguments are visible in
// the process list and land in shell history. They come from stdin instead —
// typed without echo at an interactive terminal, or piped from a secret
// manager or CI variable when stdin is not a terminal:
//
//	fxvcs storage login primary --access-key-id AKIA…      # prompts
//	op read op://vault/s3/secret | fxvcs storage login …   # piped
//
// The prompt is written to stderr so stdout stays parseable.
func readSecret(env Env, prompt string) (string, error) {
	if f, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(env.Stderr, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := readLine(env.Stdin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(line) == "" {
		return "", usageError{
			"no secret was provided on standard input",
			"pipe it in, e.g. printf '%s' \"$SECRET_ACCESS_KEY\" | fxvcs storage login <name> --access-key-id <id>",
		}
	}
	return strings.TrimSpace(line), nil
}

// readLine reads one line (or everything up to EOF) from a non-terminal
// stdin, bounded so a mistaken redirect of a large file cannot be buffered
// whole.
func readLine(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("no standard input to read the secret from")
	}
	br := bufio.NewReader(io.LimitReader(r, 64<<10))
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return line, nil
}
