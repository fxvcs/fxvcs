package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"fxvcs.dev/fxvcs/internal/controlplane/gitops"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/repository"
)

// RepositoryKind identifies which FXVCS repository model owns a Git worktree.
type RepositoryKind string

const (
	RepositoryKindComponent RepositoryKind = "component"
	RepositoryKindStack     RepositoryKind = "stack"
)

// RepositoryIdentifyRequest selects a path in or below a Git worktree.
type RepositoryIdentifyRequest struct {
	Path string `json:"path"`
}

// RepositoryIdentifyResult is the minimal information needed to route to the
// component- or stack-specific SDK operations.
type RepositoryIdentifyResult struct {
	RepositoryRoot string         `json:"repositoryRoot"`
	Kind           RepositoryKind `json:"kind"`
}

// RepositoryIdentify classifies an FXVCS Git worktree without opening local
// databases, caches, filters, manifests, object stores, or storage backends.
func (c *Client) RepositoryIdentify(ctx context.Context, req RepositoryIdentifyRequest) (*RepositoryIdentifyResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, WrapError(CodeCancelled, "cancelled", err)
	}
	git := gitclient.New(req.Path)
	if c.opts.GitExecutable != "" {
		git.Executable = c.opts.GitExecutable
	}
	root, err := git.TopLevel(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, WrapError(CodeCancelled, "cancelled", ctxErr)
		}
		return nil, WrapError(CodeNotRepository, fmt.Sprintf("%s is not inside a Git worktree", req.Path), err)
	}

	component, err := markerExists(filepath.Join(root, repository.ConfigPath))
	if err != nil {
		return nil, WrapError(CodeIO, err.Error(), err)
	}
	stack, err := markerExists(filepath.Join(root, gitops.StackFile))
	if err != nil {
		return nil, WrapError(CodeIO, err.Error(), err)
	}
	if component && stack {
		return nil, NewError(CodeConflict, fmt.Sprintf(
			"%s contains both %s and %s; an FXVCS repository cannot be both a component and a stack",
			root, repository.ConfigPath, gitops.StackFile))
	}
	if !component && !stack {
		return nil, NewError(CodeNotRepository, fmt.Sprintf(
			"%s is a Git worktree but not an FXVCS repository", root))
	}
	if component {
		if _, err := repository.LoadConfig(root); err != nil {
			return nil, translateFormatError(root, err)
		}
		return &RepositoryIdentifyResult{RepositoryRoot: root, Kind: RepositoryKindComponent}, nil
	}
	if _, err := gitops.LoadStack(root); err != nil {
		return nil, translateCoordError(root, err)
	}
	return &RepositoryIdentifyResult{RepositoryRoot: root, Kind: RepositoryKindStack}, nil
}

func markerExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: %w", path, err)
	}
}
