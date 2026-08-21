// Package controlplane defines the backend-neutral coordination seam.
//
// A control plane answers "which release does this target want, and what is in
// that release". The Stage 3 implementation is Git-backed
// (internal/controlplane/gitops); a future hosted backend implements the same
// interface over the same schemas, which is why every type crossing it comes
// from internal/domain rather than from a backend package.
//
// Nothing here commits, pushes, or deploys. Writes produce reviewable files or
// records; making them authoritative is a separate, human-authorized step.
package controlplane

import (
	"context"
	"errors"

	"fxvcs.dev/fxvcs/internal/domain"
)

// Errors every backend shares.
var (
	// ErrNotFound: no such release, target, environment, or component.
	ErrNotFound = errors.New("controlplane: not found")
	// ErrImmutable: a release record already exists with different content.
	// A release name is permanently bound to its content digest; changing a
	// component creates a new release rather than editing the old one.
	ErrImmutable = errors.New("controlplane: releases are immutable")
	// ErrExists: a record that must be created fresh already exists.
	ErrExists = errors.New("controlplane: already exists")
	// ErrInvalid: the request cannot be satisfied as written.
	ErrInvalid = errors.New("controlplane: invalid request")
)

// ControlPlane is the build-plan §6 contract.
type ControlPlane interface {
	GetRelease(ctx context.Context, name string) (domain.Release, error)
	GetTarget(ctx context.Context, name string) (domain.Target, error)
	SetTargetRelease(ctx context.Context, target, release string) error
}
