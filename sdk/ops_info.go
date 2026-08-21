package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/filter"
)

// ---- info ------------------------------------------------------------------

// InfoRequest identifies a repository checkout.
type InfoRequest struct {
	Path string `json:"path"`
}

// InfoResult is the read-only identity and setup summary of a repository and
// this clone: the place identifiers and internal names are meant to appear.
type InfoResult struct {
	RepositoryRoot        string              `json:"repositoryRoot"`
	Name                  string              `json:"name"`
	Profile               string              `json:"profile"`
	RepositoryID          string              `json:"repositoryID"`
	StorageDomainID       string              `json:"storageDomainID"`
	MinimumFXVCSVersion   string              `json:"minimumFXVCSVersion"`
	GitIntegrationVersion int                 `json:"gitIntegrationVersion"`
	InstalledVersion      string              `json:"installedVersion"`
	Descriptor            *FiveMDescriptor    `json:"descriptor,omitempty"`
	Remotes               []StorageRemoteSpec `json:"remotes"`
	// LauncherPath is the driver launcher recorded by setup ("" when this
	// clone has not been set up).
	LauncherPath string `json:"launcherPath,omitempty"`
	// GitDriverConfigured reports that every repository-local Git
	// configuration entry fxvcs needs points at LauncherPath.
	GitDriverConfigured  bool `json:"gitDriverConfigured"`
	PrePushHookInstalled bool `json:"prePushHookInstalled"`
	// PrePushHookForeign reports a pre-push hook that fxvcs did not write.
	PrePushHookForeign bool `json:"prePushHookForeign,omitempty"`
}

// Info reports repository identity, storage, and the Git driver setup of
// this clone. It is read-only and never creates local databases.
func (c *Client) Info(ctx context.Context, req InfoRequest) (*InfoResult, error) {
	r, err := c.openRepo(ctx, req.Path, "info")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cfg := r.Config
	res := &InfoResult{
		RepositoryRoot: r.Root, Name: cfg.Metadata.Name, Profile: cfg.Spec.Profile,
		RepositoryID: cfg.Metadata.RepositoryID, StorageDomainID: cfg.Metadata.StorageDomainID,
		MinimumFXVCSVersion: cfg.Spec.MinimumFXVCSVersion, GitIntegrationVersion: cfg.Spec.GitIntegrationVersion,
		InstalledVersion: c.installed.String(), Remotes: []StorageRemoteSpec{},
	}
	if cfg.Spec.Layout != "" || cfg.Spec.SourceRoot != "" {
		d := &FiveMDescriptor{SourceRoot: cfg.Spec.SourceRoot, Layout: cfg.Spec.Layout, Resources: []string{}}
		if cfg.Spec.ResourceName != nil {
			d.ResourceName = *cfg.Spec.ResourceName
		}
		res.Descriptor = d
	}
	for _, v := range r.Remotes() {
		res.Remotes = append(res.Remotes, viewToSpec(v))
	}
	if r.Local != nil {
		res.LauncherPath = r.Local.Spec.LauncherPath
	}
	launcher := res.LauncherPath
	if launcher == "" {
		if def, err := filter.DefaultLauncherPath(); err == nil {
			launcher = def
		}
	}
	if launcher != "" {
		res.GitDriverConfigured = true
		for _, want := range filter.GitConfigEntries(launcher) {
			got, ok, err := r.Git.ConfigGet(ctx, want.Key)
			if err != nil {
				return nil, translateError(r.Root, err)
			}
			if !ok || got != want.Value {
				res.GitDriverConfigured = false
				break
			}
		}
	}
	hooksDir, err := r.Git.GitPath(ctx, "hooks")
	if err != nil {
		return nil, translateError(r.Root, err)
	}
	if b, err := os.ReadFile(filepath.Join(hooksDir, "pre-push")); err == nil { //nolint:gosec // hooks dir resolved by git
		if strings.Contains(string(b), filter.HookMarker) {
			res.PrePushHookInstalled = true
		} else {
			res.PrePushHookForeign = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, translateError(r.Root, err)
	}
	return res, nil
}
