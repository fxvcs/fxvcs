package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/controlplane"
	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/sdk"
)

// VerbatimError marks an error whose message must reach the user unchanged
// (Git-facing driver output, whose wording other tooling may match).
type VerbatimError struct{ Err error }

// Error returns the wrapped message.
func (e VerbatimError) Error() string { return e.Err.Error() }

// Unwrap exposes the wrapped error for errors.Is/As.
func (e VerbatimError) Unwrap() error { return e.Err }

// Verbatim wraps err so Run prints it word for word (nil stays nil).
func Verbatim(err error) error {
	if err == nil {
		return nil
	}
	return VerbatimError{Err: err}
}

// describeError turns any error into a plain-English cause and an exact next
// command. Under verbose the original message is kept and the SDK code is
// printed by the caller.
func describeError(err error, verbose bool) (cause, hint string) {
	var ue usageError
	if errors.As(err, &ue) {
		return ue.msg, ue.hint
	}
	var up upgradePending
	if errors.As(err, &up) {
		return "this repository or clone needs an upgrade", "fxvcs upgrade --apply (nothing is committed; review the plan first with: fxvcs upgrade)"
	}
	var ve VerbatimError
	if errors.As(err, &ve) {
		return ve.Error(), sdk.HintOf(err)
	}
	var se *sdk.Error
	if !errors.As(err, &se) {
		return humanize(err.Error()), ""
	}
	cause, hint = plainCause(se)
	if hint == "" {
		hint = se.Hint // the SDK's own next step when the CLI has nothing more specific
	}
	if verbose {
		cause = err.Error()
	}
	return cause, hint
}

var (
	unboundRemoteRE = regexp.MustCompile(`on this machine: ([^ (]+)`)
	notTrackedRE    = regexp.MustCompile(`\): (.+)$`)
	profileRE       = regexp.MustCompile(`profile is "([^"]*)"`)
	noSuchRemoteRE  = regexp.MustCompile(`storage remote: "([^"]*)"`)
)

// plainCause maps the engine sentinels and SDK codes a command can surface
// to the plan's causes and hints (docs/cli-ux.md §3, §4).
func plainCause(se *sdk.Error) (cause, hint string) {
	msg := se.Message
	switch {
	case errors.Is(se, gitops.ErrNotStack):
		// Coordination commands need the *other* kind of repository, so
		// sending the user to `fxvcs init` here would be exactly wrong.
		return "this folder is not a stack (no fxvcs.yaml here or above)",
			"run this inside your stack repository, or create one with: fxvcs stack init"
	case errors.Is(se, controlplane.ErrImmutable):
		return humanize(msg), "pick a new release name, or see what is already there: fxvcs release inspect <name>"
	case se.Code == sdk.CodeNotRepository, errors.Is(se, app.ErrNotRepository):
		return "this folder is not an fxvcs repository", "run this inside your server resource repository, or set one up with: fxvcs init --profile fivem"
	case errors.Is(se, app.ErrNotGitWorktree):
		return "this folder is not inside a Git repository", "git init, then: fxvcs init --profile fivem"
	case errors.Is(se, app.ErrRemoteUnbound):
		name := "<name>"
		if m := unboundRemoteRE.FindStringSubmatch(msg); m != nil {
			name = m[1]
		}
		return fmt.Sprintf("storage %q is not set up on this computer", name),
			fmt.Sprintf("fxvcs storage add --name %s --path <folder shared with your team>", name)
	case errors.Is(se, app.ErrNoSuchRemote):
		name := ""
		if m := noSuchRemoteRE.FindStringSubmatch(msg); m != nil {
			name = m[1]
		}
		if name == "" {
			return "no storage with that name is configured", "fxvcs storage list"
		}
		return fmt.Sprintf("no storage named %q is configured", name), "fxvcs storage list"
	case errors.Is(se, app.ErrNotTracked):
		path := "<path>"
		if m := notTrackedRE.FindStringSubmatch(msg); m != nil {
			path = m[1]
		}
		return fmt.Sprintf("%s is not tracked by fxvcs", path), fmt.Sprintf("fxvcs track %s   (or a pattern: fxvcs track --glob '*.ext')", path)
	case errors.Is(se, app.ErrProfileRequired):
		profile := ""
		if m := profileRE.FindStringSubmatch(msg); m != nil {
			profile = m[1]
		}
		if profile == "" {
			return "this is not a FiveM repository", "set one up with: fxvcs init --profile fivem"
		}
		return fmt.Sprintf("this is not a FiveM repository (its profile is %q)", profile), "set one up with: fxvcs init --profile fivem"
	case errors.Is(se, app.ErrDescriptorMissing):
		return "this FiveM repository does not record where its resources live", "fxvcs init --profile fivem --source-root <folder> --layout single|collection"
	case errors.Is(se, app.ErrAlreadyInitialized):
		return "this repository is already set up for fxvcs", "fxvcs setup   (to configure this clone) or fxvcs status"
	case errors.Is(se, app.ErrSetupRequired):
		return "this clone is not set up for fxvcs yet", "fxvcs setup"
	case errors.Is(se, app.ErrDirty):
		return "a selected file has local changes that are not committed", "commit or stash the change (git add/commit or git stash), then run the command again"
	case errors.Is(se, app.ErrRuleCovered):
		return humanize(msg), "fxvcs tracking check <path>   (shows which rule already covers it)"
	case errors.Is(se, app.ErrUpgradeConflict):
		return "the files fxvcs manages differ from what the upgrade plan expected", "fxvcs upgrade   (review the plan), then: fxvcs upgrade --apply"
	case errors.Is(se, app.ErrDiscovery):
		return "could not tell how your FiveM resources are laid out: " + humanize(strings.TrimPrefix(msg, "app: fivem layout could not be determined")),
			"fxvcs init --profile fivem --source-root <folder> --layout single|collection"
	case errors.Is(se, app.ErrTestBinaryLauncher):
		return humanize(msg), "delegate driver invocations from TestMain (fxvcs.dev/fxvcs/sdk/driver: driver.RunFromEnv) instead of installing the test binary as the launcher"
	case errors.Is(se, app.ErrInvalidArgument):
		plain := humanize(strings.TrimPrefix(strings.TrimPrefix(msg, "app: "), "invalid argument: "))
		if strings.Contains(msg, "no remotes are marked") {
			return "no shared storage is set up for this repository", "fxvcs storage add --name primary --path <folder shared with your team>   (or name one: fxvcs objects publish --remote <name>)"
		}
		return plain, ""
	}
	switch se.Code {
	case sdk.CodeUpgradeRequired:
		if d, ok := se.Details.(*sdk.UpgradeRequired); ok && d != nil {
			return fmt.Sprintf("this repository needs fxvcs %s or newer (you have %s)", d.RequiredVersion, d.InstalledVersion),
				fmt.Sprintf("install fxvcs %s or newer, then run: fxvcs upgrade", d.RequiredVersion)
		}
		return "this repository needs a newer fxvcs than the one installed", "install a newer fxvcs, then run: fxvcs upgrade"
	case sdk.CodeBusy:
		if o, ok := se.Details.(*sdk.BusyOwner); ok && o != nil {
			who := o.Operation
			if who == "" {
				who = "another fxvcs command"
			}
			where := ""
			if o.PID != 0 {
				where = fmt.Sprintf(" (pid %d", o.PID)
				if o.Host != "" {
					where += " on " + o.Host
				}
				where += ")"
			}
			return fmt.Sprintf("another fxvcs operation is using this repository right now: %s%s", who, where), "wait for it to finish, then run the command again"
		}
		return "another fxvcs operation is using this repository right now", "wait for it to finish, then run the command again"
	case sdk.CodeStorage:
		return humanize(msg), "check the storage folder (fxvcs storage test <name>) or choose another one with fxvcs storage add, then run the command again"
	case sdk.CodePathEscape:
		return humanize(msg), "use a path inside the repository"
	case sdk.CodeCancelled:
		return "cancelled", ""
	case sdk.CodeNotImplemented:
		return humanize(msg), ""
	}
	return humanize(msg), ""
}
