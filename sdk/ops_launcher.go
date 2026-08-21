package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/filter"
)

// LauncherOutcome is a stable newest-wins launcher state.
type LauncherOutcome string

const (
	LauncherAlreadyCurrent  LauncherOutcome = "already-current"
	LauncherInstallRequired LauncherOutcome = "install-required"
	LauncherUpdateRequired  LauncherOutcome = "update-required"
	LauncherInstalled       LauncherOutcome = "installed"
	LauncherUpdated         LauncherOutcome = "updated"
	LauncherNewerPresent    LauncherOutcome = "newer-present"
	LauncherVersionConflict LauncherOutcome = "equal-version-conflict"
)

// LauncherEnsureRequest installs or updates the shared per-user Git launcher.
// SourcePath overrides Options.LauncherSourcePath for this call. TargetPath is
// normally empty; custom destinations are supported but are never migrated.
type LauncherEnsureRequest struct {
	SourcePath         string `json:"sourcePath,omitempty"`
	TargetPath         string `json:"targetPath,omitempty"`
	RepairEqualVersion bool   `json:"repairEqualVersion,omitempty"`
}

// LauncherState is one newest-wins launcher comparison or installation.
type LauncherState struct {
	Path             string          `json:"path"`
	Outcome          LauncherOutcome `json:"outcome"`
	Changed          bool            `json:"changed"`
	CandidateVersion string          `json:"candidateVersion"`
	InstalledVersion string          `json:"installedVersion,omitempty"`
	CandidateSHA256  string          `json:"candidateSHA256"`
	InstalledSHA256  string          `json:"installedSHA256,omitempty"`
}

// LauncherEnsureResult reports the canonical launcher and optional legacy
// compatibility refresh.
type LauncherEnsureResult struct {
	Launcher *LauncherState `json:"launcher"`
	Legacy   *LauncherState `json:"legacy,omitempty"`
}

func launcherStateOf(in app.LauncherEnsureResult) *LauncherState {
	return &LauncherState{Path: in.Path, Outcome: LauncherOutcome(in.Outcome), Changed: in.Changed,
		CandidateVersion: in.CandidateVersion, InstalledVersion: in.InstalledVersion,
		CandidateSHA256: in.CandidateSHA256, InstalledSHA256: in.InstalledSHA256}
}

// LauncherEnsure lets fxvcs CLI and fxvcs Desktop safely share ownership of
// one launcher. It atomically installs only a newer trusted candidate, never
// downgrades, and rejects equal-version/different-content ambiguity unless the
// caller explicitly requests trusted repair.
func (c *Client) LauncherEnsure(ctx context.Context, req LauncherEnsureRequest) (*LauncherEnsureResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	source := req.SourcePath
	if source == "" {
		source = c.opts.LauncherSourcePath
	}
	if source == "" {
		var err error
		source, err = os.Executable()
		if err != nil {
			return nil, translateError("", err)
		}
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, translateError("", err)
	}
	canonical, err := filter.DefaultLauncherPath()
	if err != nil {
		return nil, translateError("", err)
	}
	target := req.TargetPath
	if target == "" {
		target = canonical
	}
	in := app.LauncherEnsureInput{SourcePath: source, TargetPath: target, CandidateVersion: c.installed,
		LockTimeout: c.opts.LockTimeout, RepairEqualVersion: req.RepairEqualVersion}
	ensured, err := app.EnsureLauncher(ctx, in)
	out := &LauncherEnsureResult{Launcher: launcherStateOf(ensured)}
	if err != nil {
		return out, translateLauncherError(err)
	}
	// Keep an existing exact legacy macOS default usable while repositories
	// are migrated. Never create it and never infer legacy from a custom path.
	if target == canonical {
		legacy, ok, lerr := filter.DefaultLegacyLauncherPath()
		if lerr != nil {
			return out, translateError("", lerr)
		}
		if ok && legacy != canonical {
			if _, statErr := os.Lstat(legacy); statErr == nil {
				in.TargetPath = legacy
				compat, compatErr := app.EnsureLauncher(ctx, in)
				out.Legacy = launcherStateOf(compat)
				if compatErr != nil {
					return out, translateLauncherError(compatErr)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return out, translateError("", statErr)
			}
		}
	}
	return out, nil
}

func translateLauncherError(err error) error {
	if errors.Is(err, app.ErrLauncherVersionConflict) {
		return WrapError(CodeConflict, err.Error(), err).WithHint("install a newer fxvcs build, or explicitly request trusted equal-version launcher repair")
	}
	return translateError("", err)
}
