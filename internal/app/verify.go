package app

import (
	"context"
	"errors"
	"fmt"

	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/state"
)

// IssueKind classifies a verification finding; the SDK maps kinds to codes.
type IssueKind string

const (
	IssueIntegrity IssueKind = "integrity" // malformed pointer, manifest/domain mismatch, corrupt or mis-sized object
	IssueNotFound  IssueKind = "not-found" // object neither cached nor available where required
	IssueStorage   IssueKind = "storage"   // a remote could not be queried
)

// VerifyIssue is one verification finding.
type VerifyIssue struct {
	Path    string
	Digest  string
	Kind    IssueKind
	Message string
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Remote also checks that every requiredForPublication remote holds the
	// object (bounded Has() probes; no lock is held).
	Remote bool
}

// Verify checks, for every selected managed index entry: pointer syntax and
// whole/1 manifest derivation, storage domain, the cached object's size and
// digest (full read), and — with Remote — presence on every required remote.
// It writes nothing. Checked counts index entries examined.
func (r *Repo) Verify(ctx context.Context, sel Selection, opts VerifyOptions) (checked int, issues []VerifyIssue, err error) {
	entries, missing, err := r.ManagedEntries(ctx, sel)
	if err != nil {
		return 0, nil, err
	}
	for _, m := range missing {
		issues = append(issues, VerifyIssue{Path: m, Kind: IssueNotFound, Message: "not in the index or not managed by fxvcs (filter=fxvcs)"})
	}
	ledger, err := r.LedgerIfExists(ctx)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return 0, nil, err
	}
	required := r.RequiredRemotes()
	seen := map[string]bool{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return checked, issues, err
		}
		checked++
		switch {
		case e.Unmerged:
			continue
		case e.PointerErr != nil:
			issues = append(issues, VerifyIssue{Path: e.Path, Kind: IssueIntegrity, Message: e.PointerErr.Error()})
			continue
		case !e.HasPointer:
			continue // historical raw blob: nothing to verify
		}
		p := e.Pointer
		if seen[p.OID] {
			continue
		}
		seen[p.OID] = true
		// Resolving the content proves the pointer's manifest derivation (or,
		// for a chunked encoding, reads and validates the manifest object) and
		// yields every object the file depends on.
		// Reporting, never repairing: verify writes nothing.
		content, err := r.Content(withoutHealing(ctx), p)
		if err != nil {
			issues = append(issues, VerifyIssue{Path: e.Path, Digest: p.OID, Kind: issueKindFor(err), Message: err.Error()})
			continue
		}
		for _, o := range content.Objects {
			issues = append(issues, r.verifyObject(ctx, e.Path, p, o, ledger, required, opts)...)
		}
	}
	return checked, issues, nil
}

// issueKindFor maps a resolution failure to the reported kind.
func issueKindFor(err error) IssueKind {
	switch {
	case errors.Is(err, ErrIntegrity):
		return IssueIntegrity
	case errors.Is(err, ErrObjectUnavailable):
		return IssueNotFound
	default:
		return IssueStorage
	}
}

// verifyObject checks one stored object: its cached bytes, its publication
// record, and — with Remote — its presence on every required remote.
func (r *Repo) verifyObject(ctx context.Context, path string, p pointer.Pointer, o ContentObject, ledger *state.Ledger, required []string, opts VerifyOptions) []VerifyIssue {
	var issues []VerifyIssue
	label := o.Digest
	if o.Kind != ObjectKindWhole {
		label = o.Kind + " " + o.Digest
	}
	cached := r.Cache.Has(r.Domain, o.Digest)
	if cached {
		if info, err := r.Cache.Stat(r.Domain, o.Digest); err == nil && info.Size != o.Size {
			issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueIntegrity, Message: fmt.Sprintf("cached %s size %d does not match %d", o.Kind, info.Size, o.Size)})
		} else if err := r.Cache.Verify(r.Domain, o.Digest); err != nil {
			issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueIntegrity, Message: "cached object failed verification: " + err.Error()})
		}
	}
	published := false
	if ledger != nil {
		_, published = publicationFlags(ctx, ledger, o.Digest, required)
	}
	if !cached && !published {
		issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueNotFound, Message: "object " + label + " is not in the local cache and has no publication record for every required remote"})
	}
	if !opts.Remote {
		return issues
	}
	for _, name := range required {
		s, _, err := r.Store(name)
		if err != nil {
			issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueStorage, Message: name + ": " + err.Error()})
			continue
		}
		ok, err := s.Has(ctx, r.Domain, o.Digest)
		switch {
		case err != nil:
			issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueStorage, Message: name + ": " + err.Error()})
		case !ok:
			issues = append(issues, VerifyIssue{Path: path, Digest: o.Digest, Kind: IssueNotFound, Message: "object " + label + " is missing on required remote " + name + "; run fxvcs objects publish"})
		}
	}
	return issues
}
