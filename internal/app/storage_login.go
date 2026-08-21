package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fxvcs.dev/fxvcs/internal/credentials"
	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/journal"
)

// StorageLoginInput records an object-store identity for one remote on this
// machine. The secret arrives as a value from the caller: no operation in this
// package reads stdin or prompts, so the CLI collects it (without echo) and
// hands it over.
type StorageLoginInput struct {
	Remote          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Profile overrides the credential-store entry name. Empty means the
	// default, "<storageDomainID>/<remote>", which lets several clones of the
	// same storage domain share one login.
	Profile string
	// Forget removes the stored entry instead of writing one.
	Forget bool
}

// StorageLoginResult reports where the identity was stored. It never contains
// the secret, and the access key id is returned only so a person can confirm
// they stored the one they meant to.
type StorageLoginResult struct {
	Remote          string
	Profile         string
	CredentialsFile string
	AccessKeyID     string
	Removed         bool
}

// StorageLogin stores (or forgets) the credentials for an S3-compatible
// remote.
//
// The secret goes to the user configuration directory with 0600 permissions,
// never to the repository: committed configuration is shared with everyone who
// can clone, and Git's administrative directory travels with copies of the
// clone. Nothing is written to the working tree and nothing is committed.
func (r *Repo) StorageLogin(ctx context.Context, in StorageLoginInput) (StorageLoginResult, error) {
	var res StorageLoginResult
	if !remoteNameRE.MatchString(in.Remote) {
		return res, fmt.Errorf("%w: remote name %q", ErrInvalidArgument, in.Remote)
	}
	view, err := r.remoteView(in.Remote)
	if err != nil {
		return res, err
	}
	if view.Type != "s3" {
		return res, fmt.Errorf("%w: storage %q is type %s and needs no credentials", ErrInvalidArgument, in.Remote, view.Type)
	}
	profile := in.Profile
	if profile == "" {
		profile = view.CredentialProfile
	}
	if profile == "" {
		profile = credentials.ProfileFor(r.Domain, in.Remote)
	}
	store, err := credentials.Open(r.opts.ConfigDir)
	if err != nil {
		return res, err
	}
	res = StorageLoginResult{Remote: in.Remote, Profile: profile, CredentialsFile: store.Path()}

	if in.Forget {
		if err := store.Remove(profile); err != nil {
			return res, err
		}
		res.Removed = true
	} else {
		entry := credentials.Entry{
			Type:            "s3",
			AccessKeyID:     strings.TrimSpace(in.AccessKeyID),
			SecretAccessKey: strings.TrimSpace(in.SecretAccessKey),
			SessionToken:    strings.TrimSpace(in.SessionToken),
			Endpoint:        view.Endpoint,
			Region:          view.Region,
		}
		if err := entry.Validate(); err != nil {
			return res, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
		if err := store.Set(profile, entry); err != nil {
			return res, err
		}
		res.AccessKeyID = entry.AccessKeyID
	}

	// Record the non-default profile in local configuration so later
	// operations resolve the same entry. This is a machine-local pointer to a
	// credential, never the credential.
	if in.Profile != "" && in.Profile != credentials.ProfileFor(r.Domain, in.Remote) {
		if err := r.bindCredentialProfile(ctx, in.Remote, in.Profile); err != nil {
			return res, err
		}
	}
	// Drop any cached store and the memoised profile list so the next
	// operation picks up the new identity.
	r.storesMu.Lock()
	delete(r.stores, in.Remote)
	r.storesMu.Unlock()
	r.forgetCredentialProfiles()
	return res, nil
}

// remoteView finds a known remote by name.
func (r *Repo) remoteView(name string) (RemoteView, error) {
	for _, v := range r.Remotes() {
		if v.Name == name {
			return v, nil
		}
	}
	return RemoteView{}, fmt.Errorf("%w: %q (see fxvcs storage list)", ErrNoSuchRemote, name)
}

// bindCredentialProfile records which credential-store entry a remote uses on
// this machine.
func (r *Repo) bindCredentialProfile(ctx context.Context, remote, profile string) error {
	h, err := r.Lock(ctx, journal.LockPublication, "local-config")
	if err != nil {
		return err
	}
	defer func() { _ = h.Release() }()
	lc, err := LoadLocalConfig(r.Paths.ConfigYAML)
	if err != nil {
		return err
	}
	found := false
	for i, l := range lc.Spec.Remotes {
		if l.Name == remote {
			lc.Spec.Remotes[i].CredentialProfile = profile
			found = true
		}
	}
	if !found {
		lc.Spec.Remotes = append(lc.Spec.Remotes, domain.LocalRemote{Name: remote, Type: "s3", CredentialProfile: profile})
	}
	if err := SaveLocalConfig(r.Paths.ConfigYAML, lc); err != nil {
		return err
	}
	r.Local = lc
	return nil
}

// StorageProfiles lists the credential-store entries this machine holds. It
// returns names only; secrets never leave the store.
func (r *Repo) StorageProfiles() ([]string, string, error) {
	store, err := credentials.Open(r.opts.ConfigDir)
	if err != nil {
		return nil, "", err
	}
	names, err := store.Profiles()
	if err != nil && !errors.Is(err, credentials.ErrNotFound) {
		return nil, store.Path(), err
	}
	return names, store.Path(), nil
}
