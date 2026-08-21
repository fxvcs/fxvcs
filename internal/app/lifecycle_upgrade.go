package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"fxvcs.dev/fxvcs/internal/domain"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"fxvcs.dev/fxvcs/internal/filter"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/migrate"
	"fxvcs.dev/fxvcs/internal/profile/fivem"
	"fxvcs.dev/fxvcs/internal/repository"
	"fxvcs.dev/fxvcs/internal/state"
)

// Upgrade modes.
const (
	UpgradeCheck = "check"
	UpgradePlan  = "plan"
	UpgradeApply = "apply"
)

// Upgrade step kinds.
const (
	StepCommittedFile = "committed-file"
	StepGitConfig     = "git-config"
	StepHook          = "hook"
	StepLauncher      = "launcher"
	StepLocalState    = "local-state"
)

// ErrUpgradeConflict: managed content changed between plan and apply, or a
// user edit sits inside FXVCS-owned content. Nothing further is written.
var ErrUpgradeConflict = errors.New("app: managed content differs from the plan; re-run fxvcs upgrade and review the working tree")

// UpgradeStep is one planned change. ExpectedBefore is the sha256 of the
// managed content the step will replace ("" when nothing exists yet); apply
// re-reads and refuses when it no longer matches.
type UpgradeStep struct {
	Kind           string
	Target         string
	FromVersion    string
	ToVersion      string
	ExpectedBefore string
	Description    string
}

// UpgradeResult is the plan (and, for apply, the outcome).
type UpgradeResult struct {
	UpToDate            bool
	Steps               []UpgradeStep
	Applied             bool
	AppliedSteps        []UpgradeStep
	Notes               []string
	InstalledVersion    string
	MinimumFXVCSVersion string
	LauncherPath        string
}

// upgradeStepHook runs before each applied step; tests inject failures to
// prove interruption safety. nil in production.
var upgradeStepHook func(index int, step UpgradeStep) error

// probeLauncherVersion asks the launcher binary for its version. Overridable
// in tests.
var probeLauncherVersion = func(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--json", "version") //nolint:gosec // the launcher path is configured by setup
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	if _, err := format.ParseVersion(v.Version); err != nil {
		return "", err
	}
	return v.Version, nil
}

func sha256Hex(b []byte) string {
	if b == nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Upgrade computes the plan that brings FXVCS-owned content of this
// repository and clone to the running binary's generation and, in apply
// mode, executes it step by step. Check and plan modes are read-only (apart
// from asking the launcher binary for its version). Apply changes only
// FXVCS-owned content guarded by ExpectedBefore digests, never commits,
// stops at the first conflict, and can be re-run after an interruption: the
// remaining steps are recomputed from the current state.
func (r *Repo) Upgrade(ctx context.Context, mode string, repairLauncher ...bool) (UpgradeResult, error) {
	switch mode {
	case UpgradeCheck, UpgradePlan, UpgradeApply:
	default:
		return UpgradeResult{}, fmt.Errorf("%w: upgrade mode %q", ErrInvalidArgument, mode)
	}
	repair := len(repairLauncher) > 0 && repairLauncher[0]
	res, launcher, err := r.upgradePlan(ctx, repair)
	if err != nil {
		return UpgradeResult{}, err
	}
	if mode != UpgradeApply || res.UpToDate {
		return res, nil
	}
	for i, st := range res.Steps {
		if upgradeStepHook != nil {
			if err := upgradeStepHook(i, st); err != nil {
				return res, err
			}
		}
		if err := r.applyStep(ctx, st, launcher, repair); err != nil {
			return res, err
		}
		res.AppliedSteps = append(res.AppliedSteps, st)
	}
	res.Applied = true
	// Reload local config so a caller holding this Repo sees the recorded
	// launcher.
	if lc, err := LoadLocalConfig(r.Paths.ConfigYAML); err == nil {
		r.Local = lc
	}
	return res, nil
}

// upgradePlan inspects every managed surface without writing.
func (r *Repo) upgradePlan(ctx context.Context, repairLauncher bool) (UpgradeResult, string, error) {
	res := UpgradeResult{Steps: []UpgradeStep{}, Notes: []string{}, InstalledVersion: r.Installed().String(), MinimumFXVCSVersion: r.Config.Spec.MinimumFXVCSVersion}
	launcher, useDefault, err := r.resolveLauncherPath("")
	if err != nil {
		return res, "", err
	}
	if r.Local.Spec.LauncherPath == "" {
		res.Notes = append(res.Notes, "no launcher recorded in local config; using the default "+launcher)
	} else if useDefault && r.Local.Spec.LauncherPath != launcher {
		res.Notes = append(res.Notes, "migrating the former default launcher path "+r.Local.Spec.LauncherPath+" to "+launcher)
	}
	res.LauncherPath = launcher

	// Launcher binary.
	source, err := r.launcherSourcePath()
	if err != nil {
		return res, "", err
	}
	launcherState, err := InspectLauncher(ctx, LauncherEnsureInput{SourcePath: source, TargetPath: launcher, CandidateVersion: r.Installed(), RepairEqualVersion: repairLauncher})
	if err != nil {
		return res, "", err
	}
	switch launcherState.Outcome {
	case LauncherInstallRequired:
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepLauncher, Target: launcher, FromVersion: "", ToVersion: res.InstalledVersion, Description: "install the trusted fxvcs launcher candidate"})
	case LauncherUpdateRequired:
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepLauncher, Target: launcher, FromVersion: launcherState.InstalledVersion, ToVersion: res.InstalledVersion, ExpectedBefore: launcherState.InstalledSHA256, Description: "update the driver launcher to the newer trusted candidate"})
	case LauncherVersionConflict:
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepLauncher, Target: launcher, FromVersion: launcherState.InstalledVersion + " (different sha256)", ToVersion: res.InstalledVersion, ExpectedBefore: launcherState.InstalledSHA256, Description: "repair equal-version launcher content (requires --repair-launcher)"})
	case LauncherNewerPresent:
		res.Notes = append(res.Notes, "a newer launcher "+launcherState.InstalledVersion+" is already installed; leaving it unchanged")
	}
	// During the macOS migration window, keep the exact former default usable
	// for repositories that have not been opened and rewired yet. Never create
	// the legacy path and never apply this policy to arbitrary custom paths.
	if useDefault {
		legacy, ok, lerr := filter.DefaultLegacyLauncherPath()
		if lerr != nil {
			return res, "", lerr
		}
		if ok && legacy != launcher {
			if _, statErr := os.Lstat(legacy); statErr == nil {
				compat, inspectErr := InspectLauncher(ctx, LauncherEnsureInput{SourcePath: source, TargetPath: legacy, CandidateVersion: r.Installed(), RepairEqualVersion: repairLauncher})
				if inspectErr != nil {
					return res, "", inspectErr
				}
				switch compat.Outcome {
				case LauncherUpdateRequired:
					res.Steps = append(res.Steps, UpgradeStep{Kind: StepLauncher, Target: legacy, FromVersion: compat.InstalledVersion, ToVersion: res.InstalledVersion, ExpectedBefore: compat.InstalledSHA256, Description: "refresh the former macOS launcher path during migration"})
				case LauncherVersionConflict:
					res.Steps = append(res.Steps, UpgradeStep{Kind: StepLauncher, Target: legacy, FromVersion: compat.InstalledVersion + " (different sha256)", ToVersion: res.InstalledVersion, ExpectedBefore: compat.InstalledSHA256, Description: "repair equal-version content at the former macOS launcher path (requires --repair-launcher)"})
				case LauncherNewerPresent:
					res.Notes = append(res.Notes, "the former macOS launcher path contains newer version "+compat.InstalledVersion+"; leaving it unchanged")
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return res, "", statErr
			}
		}
	}

	// Repository-local git config.
	for _, e := range filter.GitConfigEntries(launcher) {
		cur, ok, err := r.Git.ConfigGet(ctx, e.Key)
		if err != nil {
			return res, "", err
		}
		if !ok || cur != e.Value {
			st := UpgradeStep{Kind: StepGitConfig, Target: e.Key, FromVersion: cur, ToVersion: e.Value, Description: "set repository-local " + e.Key}
			if ok {
				st.ExpectedBefore = sha256Hex([]byte(cur))
			}
			res.Steps = append(res.Steps, st)
		}
	}

	// Pre-push hook.
	hookDir, err := r.Git.GitPath(ctx, "hooks")
	if err != nil {
		return res, "", err
	}
	hookPath := filepath.Join(hookDir, "pre-push")
	existing, err := os.ReadFile(hookPath) //nolint:gosec // hooks directory reported by git
	switch {
	case errors.Is(err, os.ErrNotExist):
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepHook, Target: hookPath, ToVersion: "managed", Description: "install the fxvcs pre-push hook"})
	case err != nil:
		return res, "", err
	case !strings.Contains(string(existing), "# fxvcs-managed pre-push hook"):
		res.Notes = append(res.Notes, "pre-push hook is not managed by fxvcs; chain it manually: "+strings.TrimSpace(filter.PrePushRefusalMessage(filter.HookInstallResult{Path: hookPath, HooksPath: hookDir}, launcher)))
	case string(existing) != filter.HookScript(launcher):
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepHook, Target: hookPath, FromVersion: "managed", ToVersion: "managed", ExpectedBefore: sha256Hex(existing), Description: "refresh the fxvcs pre-push hook"})
	}

	// Committed configuration.
	cfgPath := filepath.Join(r.Root, filepath.FromSlash(repository.ConfigPath))
	cfgBytes, err := os.ReadFile(cfgPath) //nolint:gosec // repository root file
	if err != nil {
		return res, "", err
	}
	// Re-read from disk: an earlier apply step in this process may have
	// rewritten the file after Open loaded r.Config.
	cfg, err := repository.LoadConfigBytes(cfgBytes)
	if err != nil {
		return res, "", err
	}
	r.Config = cfg
	res.MinimumFXVCSVersion = cfg.Spec.MinimumFXVCSVersion
	if cfg.APIVersion != format.CurrentAPIVersion {
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepCommittedFile, Target: repository.ConfigPath, FromVersion: cfg.APIVersion, ToVersion: format.CurrentAPIVersion, ExpectedBefore: sha256Hex(cfgBytes), Description: "migrate the committed document apiVersion"})
	}
	if cfg.Spec.GitIntegrationVersion < format.GitIntegrationGeneration {
		res.Steps = append(res.Steps, UpgradeStep{Kind: StepCommittedFile, Target: repository.ConfigPath, FromVersion: "gitIntegrationVersion " + strconv.Itoa(cfg.Spec.GitIntegrationVersion), ToVersion: "gitIntegrationVersion " + strconv.Itoa(format.GitIntegrationGeneration), ExpectedBefore: sha256Hex(cfgBytes), Description: "declare the current Git integration generation (review and commit)"})
	}
	if floor := strings.TrimSpace(cfg.Spec.MinimumFXVCSVersion); floor != "" {
		res.Notes = append(res.Notes, "minimumFXVCSVersion "+floor+" (installed "+res.InstalledVersion+"); raise it in a reviewed change once every client is upgraded")
	}

	// Managed .gitattributes block: rendering drift, plus — for the fivem
	// profile — preset patterns introduced by a newer fxvcs that are absent
	// from the block. The step is additive (patterns the user added by hand
	// stay; nothing is removed) and, like every committed change, lands in the
	// working tree for review.
	attrs := readAttributesOrEmpty(r.attributesFile())
	if mb := parseManagedBlock(attrs); mb.Found {
		if len(mb.Foreign) > 0 {
			res.Steps = append(res.Steps, UpgradeStep{Kind: StepCommittedFile, Target: AttributesPath, FromVersion: "managed block", ToVersion: "managed block", ExpectedBefore: sha256Hex(mb.Raw), Description: "the FXVCS-managed .gitattributes block contains lines not written by fxvcs (" + strings.Join(mb.Foreign, "; ") + "); move them outside the block, then re-run"})
		} else {
			patterns, added := withMissingPresets(cfg.Spec.Profile, mb.Patterns, disabledPresets(cfg))
			want := fivem.AttributesBlock(patterns)
			switch {
			case len(added) > 0:
				res.Steps = append(res.Steps, UpgradeStep{Kind: StepCommittedFile, Target: AttributesPath, FromVersion: "managed block", ToVersion: "generation " + strconv.Itoa(format.GitIntegrationGeneration), ExpectedBefore: sha256Hex(mb.Raw), Description: "add " + strconv.Itoa(len(added)) + " preset pattern(s) new in this fxvcs to the managed .gitattributes block (" + strings.Join(added, " ") + "); review and commit"})
			case !bytes.Equal(want, mb.Raw):
				res.Steps = append(res.Steps, UpgradeStep{Kind: StepCommittedFile, Target: AttributesPath, FromVersion: "managed block", ToVersion: "generation " + strconv.Itoa(format.GitIntegrationGeneration), ExpectedBefore: sha256Hex(mb.Raw), Description: "re-render the FXVCS-managed .gitattributes block (review and commit)"})
			}
		}
	}

	// Local databases.
	for _, db := range []struct {
		path string
		reg  *migrate.Registry
		kind string
	}{
		{r.Paths.StateDB, state.HydrationMigrations(), "hydration state"},
		{r.Paths.PublicationDB, state.PublicationMigrations(), "publication ledger"},
	} {
		st, err := probeDatabase(ctx, db.path)
		if err != nil {
			return res, "", err
		}
		if !st.exists {
			continue
		}
		if st.version > db.reg.Latest() {
			return res, "", &format.UpgradeRequiredError{Installed: r.Installed(), Required: format.Version{Pre: fmt.Sprintf("%s-schema.%d", strings.ReplaceAll(db.kind, " ", "-"), st.version)}, Repository: r.Root, Reason: db.kind + " schema"}
		}
		if st.version < db.reg.Latest() || st.resume {
			plan, err := db.reg.Plan(st.version, db.reg.Latest())
			if err != nil {
				return res, "", err
			}
			var descs []string
			for _, s := range plan.Steps {
				descs = append(descs, s.Description)
			}
			d := "migrate " + db.kind + " " + filepath.Base(db.path) + ": " + strings.Join(descs, "; ")
			if st.resume {
				d = "resume interrupted migration of " + db.kind + " " + filepath.Base(db.path)
			}
			res.Steps = append(res.Steps, UpgradeStep{Kind: StepLocalState, Target: db.path, FromVersion: strconv.Itoa(st.version), ToVersion: strconv.Itoa(db.reg.Latest()), Description: d})
		}
	}
	res.UpToDate = len(res.Steps) == 0
	return res, launcher, nil
}

type dbProbe struct {
	exists  bool
	version int
	resume  bool
}

// probeDatabase reads user_version and the migration journal without
// creating or changing the file.
func probeDatabase(ctx context.Context, path string) (dbProbe, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dbProbe{}, nil
		}
		return dbProbe{}, err
	}
	db, err := sql.Open(migrate.DriverName, path)
	if err != nil {
		return dbProbe{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	uv, err := migrate.UserVersion(db)
	if err != nil {
		return dbProbe{}, fmt.Errorf("%w: %s: %v", state.ErrCorrupt, path, err)
	}
	p := dbProbe{exists: true, version: uv}
	if st, verr := migrate.Verify(ctx, db); verr != nil && st.Resumable() {
		p.resume = true
	}
	return p, nil
}

// applyStep executes one planned step after re-validating ExpectedBefore.
func (r *Repo) applyStep(ctx context.Context, st UpgradeStep, launcher string, repairLauncher bool) error {
	switch st.Kind {
	case StepLauncher:
		source, err := r.launcherSourcePath()
		if err != nil {
			return err
		}
		ensured, err := EnsureLauncher(ctx, LauncherEnsureInput{SourcePath: source, TargetPath: st.Target, CandidateVersion: r.Installed(), LockTimeout: r.opts.LockTimeout, RepairEqualVersion: repairLauncher})
		if err != nil {
			return err
		}
		if ensured.Outcome == LauncherVersionConflict {
			return ErrLauncherVersionConflict
		}
		return r.recordLauncher(ctx, launcher)
	case StepGitConfig:
		cur, ok, err := r.Git.ConfigGet(ctx, st.Target)
		if err != nil {
			return err
		}
		if (ok && sha256Hex([]byte(cur)) != st.ExpectedBefore) || (!ok && st.ExpectedBefore != "") {
			return fmt.Errorf("%w: git config %s", ErrUpgradeConflict, st.Target)
		}
		if err := r.Git.ConfigSet(ctx, st.Target, st.ToVersion); err != nil {
			return err
		}
		return r.recordLauncher(ctx, launcher)
	case StepHook:
		existing, err := os.ReadFile(st.Target) //nolint:gosec // hooks directory reported by git
		switch {
		case errors.Is(err, os.ErrNotExist):
			if st.ExpectedBefore != "" {
				return fmt.Errorf("%w: hook %s", ErrUpgradeConflict, st.Target)
			}
		case err != nil:
			return err
		default:
			if sha256Hex(existing) != st.ExpectedBefore {
				return fmt.Errorf("%w: hook %s", ErrUpgradeConflict, st.Target)
			}
		}
		_, err = filter.InstallPrePushHook(ctx, r.Git, launcher)
		return err
	case StepCommittedFile:
		switch st.Target {
		case repository.ConfigPath:
			return r.applyRepositoryConfigStep(st)
		case AttributesPath:
			return r.applyAttributesStep(ctx, st)
		}
		return fmt.Errorf("%w: unknown committed file %s", ErrInvalidArgument, st.Target)
	case StepLocalState:
		return r.applyLocalStateStep(ctx, st)
	}
	return fmt.Errorf("%w: unknown upgrade step kind %q", ErrInvalidArgument, st.Kind)
}

// applyRepositoryConfigStep edits the single FXVCS-owned scalar in
// .fxvcs/repository.yaml through the YAML node tree so comments and layout
// survive; the user reviews and commits the diff.
func (r *Repo) applyRepositoryConfigStep(st UpgradeStep) error {
	path := filepath.Join(r.Root, filepath.FromSlash(repository.ConfigPath))
	data, err := os.ReadFile(path) //nolint:gosec // repository root file
	if err != nil {
		return err
	}
	if sha256Hex(data) != st.ExpectedBefore {
		return fmt.Errorf("%w: %s", ErrUpgradeConflict, repository.ConfigPath)
	}
	if !strings.HasPrefix(st.FromVersion, "gitIntegrationVersion ") {
		return fmt.Errorf("%w: no document migration chain for %s -> %s", ErrInvalidArgument, st.FromVersion, st.ToVersion)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%w: empty repository.yaml", ErrInvalidArgument)
	}
	spec := mappingValue(doc.Content[0], "spec")
	if spec == nil {
		return fmt.Errorf("%w: repository.yaml has no spec", ErrInvalidArgument)
	}
	gen := strconv.Itoa(format.GitIntegrationGeneration)
	if v := mappingValue(spec, "gitIntegrationVersion"); v != nil {
		v.Value, v.Tag, v.Style = gen, "!!int", 0
	} else {
		spec.Content = append(spec.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "gitIntegrationVersion"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: gen})
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, err := repository.LoadConfigBytes(b.Bytes()); err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b.Bytes(), fi.Mode().Perm())
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// applyAttributesStep re-renders the managed block after confirming it is
// byte-identical to what the plan saw.
func (r *Repo) applyAttributesStep(ctx context.Context, st UpgradeStep) error {
	h, err := r.Lock(ctx, journal.LockCheckoutWorktree, "gitattributes")
	if err != nil {
		return err
	}
	defer func() { _ = h.Release() }()
	path := r.attributesFile()
	data, perm, err := readAttributes(path)
	if err != nil {
		return err
	}
	mb := parseManagedBlock(data)
	if !mb.Found || sha256Hex(mb.Raw) != st.ExpectedBefore {
		return fmt.Errorf("%w: %s managed block", ErrUpgradeConflict, AttributesPath)
	}
	if len(mb.Foreign) > 0 {
		return fmt.Errorf("%w: %s managed block contains lines not written by fxvcs: %s", ErrUpgradeConflict, AttributesPath, strings.Join(mb.Foreign, "; "))
	}
	patterns, _ := withMissingPresets(r.Config.Spec.Profile, mb.Patterns, disabledPresets(r.Config))
	out := fivem.MergeAttributes(data, fivem.AttributesBlock(patterns))
	if bytes.Equal(out, data) {
		return nil
	}
	return writeFileAtomic(path, out, perm)
}

// withMissingPresets returns the block patterns plus any profile preset
// patterns not yet present (fivem profile only), and the list of additions.
// Existing order is preserved; new presets are appended in preset order.
func withMissingPresets(profile string, existing, disabled []string) (patterns, added []string) {
	patterns = append([]string(nil), existing...)
	if profile != ProfileFiveM {
		return patterns, nil
	}
	have := map[string]bool{}
	for _, p := range existing {
		have[p] = true
	}
	skip := map[string]bool{}
	for _, p := range disabled {
		skip[p] = true
	}
	for _, p := range fivem.DefaultBinaryPatterns {
		if !have[p] && !skip[p] {
			patterns = append(patterns, p)
			added = append(added, p)
		}
	}
	return patterns, added
}

// applyLocalStateStep migrates one SQLite database with the transactional
// swap: state.db (derived) under the checkout-worktree lock without keeping
// a backup, publication.db (authoritative) under the publication lock with
// <db>.bak-<from> retained.
func (r *Repo) applyLocalStateStep(ctx context.Context, st UpgradeStep) error {
	if r.stateDB != nil || r.ledger != nil {
		return fmt.Errorf("%w: local databases are open in this process", ErrInvalidArgument)
	}
	var reg *migrate.Registry
	var level journal.Level
	var key string
	keep := false
	switch st.Target {
	case r.Paths.StateDB:
		reg, level, key = state.HydrationMigrations(), journal.LockCheckoutWorktree, "state-db"
	case r.Paths.PublicationDB:
		reg, level, key, keep = state.PublicationMigrations(), journal.LockPublication, "ledger", true
	default:
		return fmt.Errorf("%w: unknown local database %s", ErrInvalidArgument, st.Target)
	}
	h, err := r.Lock(ctx, level, key)
	if err != nil {
		return err
	}
	defer func() { _ = h.Release() }()
	_, err = migrate.MigrateFile(ctx, st.Target, reg, migrate.FileOptions{KeepBackup: keep, Run: migrate.RunOptions{Now: r.opts.Now}})
	return err
}

// disabledPresets returns the committed preset opt-outs.
func disabledPresets(cfg *domain.Repository) []string {
	if cfg == nil || cfg.Spec.Tracking == nil {
		return nil
	}
	return cfg.Spec.Tracking.DisabledPresets
}
