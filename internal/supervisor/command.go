package supervisor

import (
	"context"
	"os/exec"
)

// CommandRunner executes an exact argv without a shell. It is the production
// process boundary used by the systemd adapter; tests can replace Runner with
// a deterministic fake.
type CommandRunner struct{}

func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
