// Package driver exposes the Git process-boundary entry points
// (filter-process, clean-filter, smudge-filter, diff-driver, merge-driver,
// pre-push) as an in-process function, so a downstream test binary can serve
// as its own driver launcher without building or re-execing cmd/fxvcs.
//
// Repo setup installs the *running executable* as Git's filter/diff/merge
// driver, and the launcher must stay byte-identical to it (see
// docs/spec/git-integration.md). Under `go test` the running executable is the
// test binary, whose only behaviour is to run the suite — so Git would run the
// suite again, recursively. Delegating driver invocations back into the CLI
// from TestMain resolves that:
//
//	func TestMain(m *testing.M) {
//		if code, ok := driver.RunFromEnv(context.Background()); ok {
//			os.Exit(code)
//		}
//		os.Exit(m.Run())
//	}
//
// Each test then exports driver.EnvVar so the drivers Git spawns take the
// delegating path:
//
//	t.Setenv(driver.EnvVar, "1")
//
// Git subprocesses inherit the process environment, so one Setenv per test
// covers every driver invocation the SDK triggers.
//
// This package is a testing-support surface, not part of the pinned
// fxvcs.dev/fxvcs/sdk API. It follows the same additive-only v0 rules
// (docs/sdk-stability.md); it is not covered by TestAPISurfaceIsPinned.
package driver

import (
	"context"
	"fmt"
	"io"
	"os"

	"fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/cli/drivers"
)

// EnvVar is the environment variable a test binary sets to declare that it
// delegates driver invocations to Run. Setting it is what makes RunFromEnv
// take the driver path instead of returning to the test suite.
const EnvVar = "FXVCS_TEST_DRIVER"

// Run executes one driver invocation against the given streams and returns the
// process exit status. args excludes the program name, so a Git invocation of
// `<launcher> filter-process` is Run(ctx, []string{"filter-process"}, ...).
//
// Run never panics on an SDK error, never prompts, and never calls os.Exit;
// the caller owns the exit. Exit codes are the documented CLI contract
// (docs/sdk-stability.md §1): 0 OK, 1 failure, 2 usage, 3 upgrade-required,
// 4 not-implemented, 5 busy.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	env := cli.Env{Stdout: stdout, Stderr: stderr, Stdin: stdin, Extra: drivers.Commands()}
	return cli.Run(ctx, env, append([]string{"fxvcs"}, args...))
}

// RunFromEnv is the TestMain adapter for Run. It reports handled=true when the
// process was started as a driver rather than as a test run, in which case the
// caller must exit with the returned status instead of running the suite.
//
// It handles two cases:
//
//   - EnvVar is set to "1": the invocation is delegated to Run over os.Args[1:]
//     and the real standard streams.
//   - EnvVar is unset but os.Args[1:] names a driver command: Git launched this
//     binary as a driver and the delegation was never wired up. Falling through
//     to the suite here is what recursively re-runs it, so this fails loudly
//     with a non-zero status instead.
//
// Anything else returns (0, false) and the caller proceeds to m.Run().
func RunFromEnv(ctx context.Context) (code int, handled bool) {
	args := os.Args[1:]
	if os.Getenv(EnvVar) == "1" {
		return Run(ctx, args, os.Stdin, os.Stdout, os.Stderr), true
	}
	if len(args) > 0 && IsDriverCommand(args[0]) {
		fmt.Fprintf(os.Stderr,
			"fxvcs: this binary was invoked as the %q driver but %s is not set.\n"+
				"Git is running a Go test binary as the driver launcher; without delegation it would re-run the test suite.\n"+
				"Set %s=1 for the process under test (t.Setenv) and delegate from TestMain via sdk/driver.RunFromEnv.\n",
			args[0], EnvVar, EnvVar)
		return 1, true
	}
	return 0, false
}

// IsDriverCommand reports whether name is a top-level driver command, i.e. one
// Git or a hook may invoke the launcher with.
func IsDriverCommand(name string) bool {
	for _, c := range drivers.Commands() {
		if c.Name == name {
			return true
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return true
			}
		}
	}
	return false
}

// Commands returns the names of every driver command Run accepts, in
// registration order. Consumers use it for diagnostics; the set is additive.
func Commands() []string {
	cmds := drivers.Commands()
	names := make([]string, 0, len(cmds))
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	return names
}
