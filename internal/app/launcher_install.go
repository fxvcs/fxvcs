package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/lock"
)

// LauncherOutcome is the result of comparing or installing a launcher.
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

// ErrLauncherVersionConflict means two different launcher binaries report the
// same semantic version. There is no safe newest-wins ordering; a trusted
// caller must explicitly request repair to replace the installed bytes.
var ErrLauncherVersionConflict = errors.New("app: launcher has the same version but different content")

// LauncherEnsureInput identifies a trusted candidate and stable destination.
type LauncherEnsureInput struct {
	SourcePath         string
	TargetPath         string
	CandidateVersion   format.Version
	LockTimeout        time.Duration
	RepairEqualVersion bool
}

// LauncherEnsureResult describes the installed and candidate launchers.
type LauncherEnsureResult struct {
	Path             string
	Outcome          LauncherOutcome
	Changed          bool
	CandidateVersion string
	InstalledVersion string
	CandidateSHA256  string
	InstalledSHA256  string
}

// InspectLauncher compares a trusted candidate with an installed launcher
// without writing. The candidate version is supplied by the embedding host;
// installed versions are always probed from the installed executable itself.
func InspectLauncher(ctx context.Context, in LauncherEnsureInput) (LauncherEnsureResult, error) {
	if !filepath.IsAbs(in.SourcePath) || !filepath.IsAbs(in.TargetPath) {
		return LauncherEnsureResult{}, fmt.Errorf("%w: launcher source and target paths must be absolute", ErrInvalidArgument)
	}
	res := LauncherEnsureResult{Path: in.TargetPath, CandidateVersion: in.CandidateVersion.String()}
	fi, err := os.Stat(in.SourcePath)
	if err != nil {
		return res, fmt.Errorf("launcher source %s: %w", in.SourcePath, err)
	}
	if !fi.Mode().IsRegular() {
		return res, fmt.Errorf("%w: launcher source %s is not a regular file", ErrInvalidArgument, in.SourcePath)
	}
	res.CandidateSHA256, err = fileDigest(in.SourcePath)
	if err != nil {
		return res, err
	}
	tfi, err := os.Lstat(in.TargetPath)
	if errors.Is(err, os.ErrNotExist) {
		res.Outcome = LauncherInstallRequired
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if tfi.Mode()&os.ModeSymlink != 0 || !tfi.Mode().IsRegular() {
		return res, fmt.Errorf("%w: launcher path %s is not a regular file", ErrInvalidArgument, in.TargetPath)
	}
	res.InstalledSHA256, err = fileDigest(in.TargetPath)
	if err != nil {
		return res, err
	}
	if res.InstalledSHA256 == res.CandidateSHA256 {
		res.InstalledVersion = res.CandidateVersion
		res.Outcome = LauncherAlreadyCurrent
		return res, nil
	}
	v, err := probeLauncherVersion(ctx, in.TargetPath)
	if err != nil {
		if in.RepairEqualVersion {
			res.Outcome = LauncherUpdateRequired
			return res, nil
		}
		return res, fmt.Errorf("probe installed launcher %s: %w", in.TargetPath, err)
	}
	installed, err := format.ParseVersion(v)
	if err != nil {
		return res, err
	}
	res.InstalledVersion = installed.String()
	switch installed.Compare(in.CandidateVersion) {
	case 1:
		res.Outcome = LauncherNewerPresent
	case -1:
		res.Outcome = LauncherUpdateRequired
	default:
		if in.RepairEqualVersion {
			res.Outcome = LauncherUpdateRequired
		} else {
			res.Outcome = LauncherVersionConflict
		}
	}
	return res, nil
}

// EnsureLauncher applies newest-wins launcher ownership under a durable,
// cross-process lock. It never downgrades an installed launcher.
func EnsureLauncher(ctx context.Context, in LauncherEnsureInput) (LauncherEnsureResult, error) {
	if !filepath.IsAbs(in.TargetPath) {
		return LauncherEnsureResult{}, fmt.Errorf("%w: launcher target path must be absolute", ErrInvalidArgument)
	}
	lockRoot := filepath.Join(filepath.Dir(filepath.Dir(in.TargetPath)), "locks")
	locks := lock.NewSet(lockRoot, "launcher-install")
	h, err := locks.Acquire(ctx, journal.LockPublication, "launcher", in.LockTimeout)
	if err != nil {
		return LauncherEnsureResult{}, err
	}
	defer func() { _ = h.Release() }()

	res, err := InspectLauncher(ctx, in)
	if err != nil {
		return res, err
	}
	switch res.Outcome {
	case LauncherAlreadyCurrent, LauncherNewerPresent:
		return res, nil
	case LauncherVersionConflict:
		return res, fmt.Errorf("%w: %s reports %s; candidate sha256 %s, installed sha256 %s",
			ErrLauncherVersionConflict, in.TargetPath, res.InstalledVersion, res.CandidateSHA256, res.InstalledSHA256)
	case LauncherInstallRequired, LauncherUpdateRequired:
		if err := installLauncherFrom(in.SourcePath, in.TargetPath); err != nil {
			return res, err
		}
		res.Changed = true
		res.InstalledVersion = res.CandidateVersion
		res.InstalledSHA256 = res.CandidateSHA256
		if res.Outcome == LauncherInstallRequired {
			res.Outcome = LauncherInstalled
		} else {
			res.Outcome = LauncherUpdated
		}
		return res, nil
	default:
		return res, fmt.Errorf("%w: unknown launcher outcome %q", ErrInvalidArgument, res.Outcome)
	}
}
