package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/materialize"
	"fxvcs.dev/fxvcs/internal/objectstore"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/release"
)

type MaterializedContent struct {
	Digest  string
	Bytes   int64
	Existed bool
	Remote  string
}

// MaterializeReleaseContent reconstructs every pointer-backed file required by
// a pinned Release into the target's verified materialized store. It does not
// assemble or activate a realization.
func (c *Coord) MaterializeReleaseContent(ctx context.Context, rel domain.Release, deployRoot string) ([]MaterializedContent, error) {
	paths := coordLocalPaths(c.GitDir)
	local, err := LoadLocalConfig(paths)
	if err != nil {
		return nil, err
	}
	checker := &coordObjectChecker{local: local, configDir: c.opts.ConfigDir}
	target := materialize.Store{DeployRoot: deployRoot}
	byDigest := map[string]MaterializedContent{}

	for _, component := range rel.Spec.Components {
		tree, err := c.Tree(ctx, component.RepositoryID, component.Commit)
		if err != nil {
			return nil, err
		}
		root, _, err := release.AssetRoot(ctx, tree, component.StorageDomainID)
		if err != nil {
			return nil, err
		}
		if root != component.AssetRoot {
			return nil, fmt.Errorf("reconcile: component %s asset root changed before materialization", component.RepositoryID)
		}
		pointers, err := release.Pointers(ctx, tree, component.StorageDomainID)
		if err != nil {
			return nil, err
		}
		cfg, err := tree.RepositoryConfig(ctx)
		if err != nil {
			return nil, err
		}
		pointerPaths := make([]string, 0, len(pointers))
		for path := range pointers {
			pointerPaths = append(pointerPaths, path)
		}
		sort.Strings(pointerPaths)
		for _, pointerPath := range pointerPaths {
			ptr := pointers[pointerPath]
			if previous, exists := byDigest[ptr.OID]; exists {
				if previous.Bytes != ptr.Size {
					return nil, fmt.Errorf("reconcile: digest %s has conflicting sizes", ptr.OID)
				}
				continue
			}
			if err := target.Verify(ptr.OID, ptr.Size); err == nil {
				byDigest[ptr.OID] = MaterializedContent{Digest: ptr.OID, Bytes: ptr.Size, Existed: true}
				continue
			}

			var failures []error
			materialized := false
			for _, remote := range cfg.Spec.Storage.Remotes {
				if !remote.RequiredForPublication {
					continue
				}
				store, err := checker.store(component.StorageDomainID, remote)
				if err != nil {
					failures = append(failures, fmt.Errorf("remote %s: %w", remote.Name, err))
					continue
				}
				result, err := materializePointerFrom(ctx, store, component.StorageDomainID, ptr, target)
				if err != nil {
					failures = append(failures, fmt.Errorf("remote %s: %w", remote.Name, err))
					continue
				}
				byDigest[ptr.OID] = MaterializedContent{Digest: ptr.OID, Bytes: ptr.Size, Existed: result.Existed, Remote: remote.Name}
				materialized = true
				break
			}
			if !materialized {
				if len(failures) == 0 {
					return nil, fmt.Errorf("reconcile: %s has no required publication remote for %s", component.RepositoryID, pointerPath)
				}
				return nil, fmt.Errorf("reconcile: materialize %s from %s: %w", pointerPath, component.RepositoryID, errors.Join(failures...))
			}
		}
	}

	out := make([]MaterializedContent, 0, len(byDigest))
	for _, item := range byDigest {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Digest < out[j].Digest })
	return out, nil
}

func materializePointerFrom(ctx context.Context, source objectstore.Store, domainID string, ptr pointer.Pointer, target materialize.Store) (materialize.Result, error) {
	if ptr.Encoding == pointer.EncodingWhole {
		rc, err := source.Get(ctx, domainID, ptr.OID)
		if err != nil {
			return materialize.Result{}, err
		}
		defer func() { _ = rc.Close() }()
		return target.Put(ctx, ptr.OID, ptr.Size, rc)
	}
	manifest, err := readManifestFrom(ctx, source, domainID, ptr)
	if err != nil {
		return materialize.Result{}, err
	}
	reader := &verifiedChunkReader{ctx: ctx, source: source, domainID: domainID, chunks: manifest.Chunks}
	result, putErr := target.Put(ctx, ptr.OID, ptr.Size, reader)
	closeErr := reader.Close()
	if putErr != nil {
		return materialize.Result{}, putErr
	}
	if closeErr != nil {
		return materialize.Result{}, closeErr
	}
	return result, nil
}

type verifiedChunkReader struct {
	ctx      context.Context
	source   objectstore.Store
	domainID string
	chunks   []domain.ManifestChunk
	index    int
	current  io.ReadCloser
	hash     hashState
	failed   error
}

type hashState struct {
	h      hashWriter
	read   int64
	digest string
	size   int64
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (r *verifiedChunkReader) Read(buf []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	for {
		if r.current == nil {
			if r.index >= len(r.chunks) {
				return 0, io.EOF
			}
			chunk := r.chunks[r.index]
			rc, err := r.source.Get(r.ctx, r.domainID, chunk.Digest)
			if err != nil {
				return 0, r.fail(err)
			}
			r.current = rc
			r.hash = hashState{h: sha256.New(), digest: chunk.Digest, size: chunk.RawSize}
		}
		n, err := r.current.Read(buf)
		if n > 0 {
			r.hash.read += int64(n)
			_, _ = r.hash.h.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			if closeErr := r.current.Close(); closeErr != nil {
				return n, r.fail(closeErr)
			}
			r.current = nil
			got := "sha256:" + hex.EncodeToString(r.hash.h.Sum(nil))
			if r.hash.read != r.hash.size || got != r.hash.digest {
				return n, r.fail(fmt.Errorf("%w: chunk %s failed verification: read %d bytes with digest %s", ErrIntegrity, r.hash.digest, r.hash.read, got))
			}
			r.index++
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			return n, r.fail(err)
		}
		return n, nil
	}
}

func (r *verifiedChunkReader) fail(err error) error {
	r.failed = err
	if r.current != nil {
		_ = r.current.Close()
		r.current = nil
	}
	return err
}

func (r *verifiedChunkReader) Close() error {
	if r.current != nil {
		err := r.current.Close()
		r.current = nil
		if err != nil {
			return err
		}
	}
	return r.failed
}
