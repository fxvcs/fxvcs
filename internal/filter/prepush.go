package filter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/gitclient"
	"fxvcs.dev/fxvcs/internal/pointer"
)

// zeroOID matches the all-zero object id Git uses for "no such ref".
func isZeroOID(s string) bool {
	if len(s) < 40 {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// PushRef is one line of the pre-push stdin protocol:
//
//	<local ref> SP <local oid> SP <remote ref> SP <remote oid> LF
type PushRef struct {
	LocalRef, LocalOID, RemoteRef, RemoteOID string
}

// ParsePushRefs parses the pre-push stdin lines.
func ParsePushRefs(r io.Reader) ([]PushRef, error) {
	var refs []PushRef
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 4 {
			return nil, fmt.Errorf("filter: malformed pre-push line %q", line)
		}
		refs = append(refs, PushRef{LocalRef: f[0], LocalOID: f[1], RemoteRef: f[2], RemoteOID: f[3]})
	}
	return refs, sc.Err()
}

// PendingObject is a pointer reachable from a pushed commit whose object is
// not published to every required remote.
type PendingObject struct {
	Digest  string
	Domain  string
	Path    string // first path where it was seen
	Commit  string
	Missing []string // required remotes without a publication record
	Reason  string   // why automatic publication did not succeed, when known
	// Pointer is the index pointer the object belongs to; a chunked encoding
	// expands to a manifest object plus its chunks, all of which must be
	// published before the push may proceed.
	Pointer pointer.Pointer
}

// PublicationBarrierError is returned when a push must be refused.
type PublicationBarrierError struct {
	Pending []PendingObject
}

func (e *PublicationBarrierError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fxvcs pre-push: refusing to push %d object(s) that could not be published to every required remote:\n", len(e.Pending))
	for _, p := range e.Pending {
		fmt.Fprintf(&b, "  %s  %s  (commit %.12s; missing on: %s)", p.Digest, p.Path, p.Commit, strings.Join(p.Missing, ", "))
		if p.Reason != "" {
			fmt.Fprintf(&b, "\n      %s", p.Reason)
		}
		b.WriteString("\n")
	}
	b.WriteString("Fix the storage remote (fxvcs storage list / storage add / storage test), or retry with fxvcs objects publish, then push again. Bypassing with --no-verify leaves commits that reference unavailable assets.\n")
	return b.String()
}

// ErrNoPublicationLedger is returned when the hook runs without an engine
// Backend: publication cannot be judged, so the push is refused rather than
// waved through.
var ErrNoPublicationLedger = errors.New("filter: publication ledger unavailable; cannot verify that pushed objects are published")

// PrePush implements the publication barrier for one push. remoteName is the
// first hook argument (used to select commits not yet on that remote when
// the remote ref is new). It scans the added/modified paths of every commit
// being pushed, keeps those whose filter attribute is fxvcs, decodes each
// pointer, publishes every object that is not yet recorded on a
// requiredForPublication remote (Backend.Publish uploads to bound remotes and
// records the result), and fails only when an object still lacks a
// publication record afterwards — because a required remote is unbound or
// unreachable, or the object is no longer available locally. Git refs
// therefore never become remotely reachable before their assets are durable,
// and the common path needs no separate `fxvcs objects publish`.
func PrePush(ctx context.Context, co *Checkout, remoteName string, refs []PushRef, log io.Writer) error {
	if log == nil {
		log = io.Discard
	}
	if co.Backend == nil {
		return ErrNoPublicationLedger
	}
	required := co.Backend.RequiredRemotes()
	if len(required) == 0 {
		fmt.Fprintln(log, "fxvcs pre-push: no storage remote is requiredForPublication; nothing to enforce")
		return nil
	}
	seen := map[string]bool{}
	var candidates []PendingObject
	for _, ref := range refs {
		if isZeroOID(ref.LocalOID) {
			continue // deletion
		}
		var commits []string
		var err error
		if isZeroOID(ref.RemoteOID) {
			commits, err = co.Git.RevList(ctx, ref.LocalOID, "--not", "--remotes="+remoteName)
		} else {
			commits, err = co.Git.RevList(ctx, ref.RemoteOID+".."+ref.LocalOID)
		}
		if err != nil {
			return fmt.Errorf("filter: enumerate pushed commits: %w", err)
		}
		for _, c := range commits {
			paths, err := co.Git.DiffTreePaths(ctx, c)
			if err != nil {
				return err
			}
			attrs, err := co.Git.CheckAttrs(ctx, paths)
			if err != nil {
				return err
			}
			for _, p := range paths {
				if !attrs[p].IsManaged() {
					continue
				}
				blob, err := co.Git.CatFileBlob(ctx, c+":"+p)
				if err != nil {
					return err
				}
				if !pointer.Sniff(blob) {
					continue // historical raw blob: never re-ingested, nothing to publish
				}
				ptr, err := pointer.Decode(blob)
				if err != nil {
					if errors.Is(err, pointer.ErrUnsupportedVersion) {
						return fmt.Errorf("%w: %s at %.12s: %v; installed fxvcs %s supports pointer format v%d; run: fxvcs upgrade --check",
							ErrUnsupportedPointer, p, c, err, co.installedString(), pointer.FormatVersion)
					}
					return fmt.Errorf("%w: %s at %.12s: %v", ErrMalformedPointer, p, c, err)
				}
				key := ptr.StorageDomainID + "/" + ptr.OID
				if seen[key] {
					continue
				}
				seen[key] = true
				candidates = append(candidates, PendingObject{Digest: ptr.OID, Domain: ptr.StorageDomainID, Path: p, Commit: c, Pointer: ptr})
			}
		}
	}
	// Ask about every asset at once. One question per asset would be one
	// sequential round trip per asset against a network object store: a few
	// hundred assets then spend a minute deciding what to upload before
	// uploading anything.
	pending, err := co.refuseUnpublished(ctx, candidates)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		// Publish first: this is the normal path for a developer pushing new
		// assets. Anything that cannot be published stays pending and blocks.
		digests := make([]string, 0, len(pending))
		for _, po := range pending {
			objs, err := co.Backend.ContentObjects(ctx, po.Pointer)
			if err != nil {
				// The manifest could not be resolved; publish what is named by
				// the pointer and let the re-check below refuse the push.
				digests = append(digests, po.Digest)
				continue
			}
			digests = append(digests, objs...)
		}
		fmt.Fprintf(log, "fxvcs pre-push: publishing %d object(s) to required storage remotes\n", len(digests))
		failures := co.Backend.Publish(ctx, digests, log)
		failed := map[string]string{}
		for _, f := range failures {
			failed[f.Digest] = f.Remote + ": " + f.Err.Error()
		}
		still, err := co.refuseUnpublished(ctx, pending)
		if err != nil {
			return err
		}
		for i := range still {
			if why, ok := failed[still[i].Digest]; ok {
				still[i].Reason = why
			}
		}
		pending = still
	}
	if len(pending) > 0 {
		sort.Slice(pending, func(i, j int) bool { return pending[i].Digest < pending[j].Digest })
		return &PublicationBarrierError{Pending: pending}
	}
	return nil
}

func (co *Checkout) installedString() string {
	if co.installed == (format.Version{}) {
		return "unknown"
	}
	return co.installed.String()
}

// RunPrePush is the `fxvcs pre-push <remote> <url>` entry point.
func RunPrePush(ctx context.Context, opts ProcessOptions, remoteName string, stdin io.Reader, stderr io.Writer) error {
	co, err := OpenCheckout(ctx, opts)
	if err != nil {
		var ue *format.UpgradeRequiredError
		if errors.As(err, &ue) {
			io.WriteString(stderr, FormatUpgradeRequired(ue))
			return ue
		}
		return err
	}
	defer func() { _ = co.Close() }()
	refs, err := ParsePushRefs(stdin)
	if err != nil {
		return err
	}
	if err := PrePush(ctx, co, remoteName, refs, stderr); err != nil {
		var pb *PublicationBarrierError
		if errors.As(err, &pb) {
			io.WriteString(stderr, pb.Error())
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// hook installation
// ---------------------------------------------------------------------------

// HookMarker identifies a hook file written by fxvcs.
const HookMarker = "# fxvcs-managed pre-push hook"

// ErrHookExists is returned when a user hook already occupies pre-push.
var ErrHookExists = errors.New("filter: a pre-push hook that is not managed by fxvcs already exists")

// HookInstallResult reports where the hook went.
type HookInstallResult struct {
	Path      string
	HooksPath string // directory (honors core.hooksPath)
	Updated   bool   // an fxvcs-managed hook was refreshed
}

// HookScript renders the pre-push hook script that chains to the launcher.
func HookScript(launcher string) string {
	return "#!/bin/sh\n" + HookMarker + "\n" +
		"# Refreshed by fxvcs setup/upgrade. To chain your own hook, keep this line and\n" +
		"# add your commands before it (docs/spec/hook-integration.md).\n" +
		"exec " + QuoteCommandPath(launcher, hookGOOS()) + " pre-push \"$@\"\n"
}

func hookGOOS() string {
	// Hooks always run under sh, even on Windows, so forward slashes are safe.
	if filepath.Separator == '\\' {
		return "windows"
	}
	return "other"
}

// InstallPrePushHook writes the managed hook into the hooks directory Git
// reports (which honors core.hooksPath and linked worktrees). It never
// overwrites a hook it did not write: ErrHookExists is returned instead, and
// the caller should instruct the user to chain `fxvcs pre-push "$@"`.
func InstallPrePushHook(ctx context.Context, git *gitclient.Client, launcher string) (HookInstallResult, error) {
	dir, err := git.GitPath(ctx, "hooks")
	if err != nil {
		return HookInstallResult{}, err
	}
	res := HookInstallResult{HooksPath: dir, Path: filepath.Join(dir, "pre-push")}
	existing, err := os.ReadFile(res.Path)
	switch {
	case err == nil:
		if !strings.Contains(string(existing), HookMarker) {
			return res, fmt.Errorf("%w: %s", ErrHookExists, res.Path)
		}
		res.Updated = true
	case errors.Is(err, os.ErrNotExist):
	default:
		return res, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	tmp := res.Path + ".fxvcs-tmp"
	if err := os.WriteFile(tmp, []byte(HookScript(launcher)), 0o755); err != nil {
		return res, err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return res, err
	}
	if err := os.Rename(tmp, res.Path); err != nil {
		os.Remove(tmp)
		return res, err
	}
	return res, nil
}

// PrePushRefusalMessage is the stderr text printed when a hook must be
// chained manually.
func PrePushRefusalMessage(res HookInstallResult, launcher string) string {
	return fmt.Sprintf("fxvcs: %s already exists and is not managed by fxvcs.\nAdd this line to it so publication is verified before every push:\n\n    %s pre-push \"$@\" || exit 1\n\n",
		res.Path, QuoteCommandPath(launcher, hookGOOS()))
}

// refuseUnpublished keeps the candidates that are still missing from a
// required remote, filling in which remotes those are. It asks the engine
// once for the whole set rather than once per asset.
func (co *Checkout) refuseUnpublished(ctx context.Context, candidates []PendingObject) ([]PendingObject, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	ptrs := make([]pointer.Pointer, 0, len(candidates))
	for _, po := range candidates {
		ptrs = append(ptrs, po.Pointer)
	}
	missing, err := co.Backend.MissingRemotesMany(ctx, ptrs)
	if err != nil {
		return nil, err
	}
	var out []PendingObject
	for _, po := range candidates {
		if m := missing[po.Digest]; len(m) > 0 {
			po.Missing = m
			out = append(out, po)
		}
	}
	return out, nil
}
