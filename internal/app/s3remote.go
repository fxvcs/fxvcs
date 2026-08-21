package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"fxvcs.dev/fxvcs/internal/credentials"
	"fxvcs.dev/fxvcs/internal/objectstore/s3"
)

// ErrNoCredentials: an S3 remote has no usable identity on this machine.
var ErrNoCredentials = errors.New("app: no credentials for this storage remote")

// credentialEnvPrefix is the environment namespace for object-store
// credentials supplied by CI. It is a variable-name prefix, not a secret.
const credentialEnvPrefix = "FXVCS_S3_" //nolint:gosec // an env var prefix, not a credential

// s3Config assembles the effective configuration for an S3 remote from the
// committed descriptor, this clone's local overrides, and the machine's
// credentials.
//
// The committed descriptor says *where* the objects live and is shared by
// everyone; local configuration and credentials say *how this machine reaches
// it* and are never committed. A local value overrides the committed one only
// when it is set, so a machine can point at an in-office mirror endpoint
// without changing what its colleagues see.
func (r *Repo) s3Config(v RemoteView) (s3.Config, error) {
	local := r.localRemote(v.Name)
	cfg := s3.Config{
		Endpoint:   v.Endpoint,
		Bucket:     v.Bucket,
		Prefix:     v.Prefix,
		Region:     v.Region,
		Addressing: v.Addressing,
		Compress:   true,
	}
	if local != nil {
		if local.Endpoint != "" {
			cfg.Endpoint = local.Endpoint
		}
		if local.Region != "" {
			cfg.Region = local.Region
		}
		if local.Addressing != "" {
			cfg.Addressing = local.Addressing
		}
		if local.Bucket != "" {
			cfg.Bucket = local.Bucket
		}
		if local.Prefix != "" {
			cfg.Prefix = local.Prefix
		}
		if local.Compress != nil {
			cfg.Compress = *local.Compress
		}
		if local.PartSizeMiB > 0 {
			cfg.PartSize = int64(local.PartSizeMiB) << 20
		}
		if local.Concurrency > 0 {
			cfg.Concurrency = local.Concurrency
		}
		if local.MaxAttempts > 0 {
			cfg.MaxAttempts = local.MaxAttempts
		}
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return cfg, fmt.Errorf("%w: s3 remote %q has no bucket; declare it in .fxvcs/repository.yaml or bind one with fxvcs storage add", ErrInvalidArgument, v.Name)
	}
	creds, err := r.s3Credentials(v.Name, local)
	if err != nil {
		return cfg, err
	}
	cfg.Credentials = creds
	return cfg, nil
}

// s3Credentials resolves the identity for one remote, in this order:
//
//  1. environment variables scoped to the remote
//     (FXVCS_S3_<REMOTE>_ACCESS_KEY_ID / _SECRET_ACCESS_KEY / _SESSION_TOKEN);
//  2. unscoped FXVCS_S3_ACCESS_KEY_ID / _SECRET_ACCESS_KEY / _SESSION_TOKEN;
//  3. the credential store written by `fxvcs storage login`;
//  4. the AWS default chain (shared config, profiles, instance roles), which
//     the backend resolves lazily.
//
// The order puts the deliberately-set, most specific thing first: an operator
// exporting a key for one remote in CI means it, and it should not be
// shadowed by a stale desktop login. Nothing here reads committed files, and
// no secret is ever written back to one.
func (r *Repo) s3Credentials(remote string, local *localRemoteView) (s3.Credentials, error) {
	if c, ok := envCredentials(credentialEnvPrefix + envToken(remote) + "_"); ok {
		return c, nil
	}
	if c, ok := envCredentials(credentialEnvPrefix); ok {
		return c, nil
	}
	profile := credentials.ProfileFor(r.Domain, remote)
	if local != nil && local.CredentialProfile != "" {
		profile = local.CredentialProfile
	}
	store, err := credentials.Open(r.opts.ConfigDir)
	if err != nil {
		return s3.Credentials{}, err
	}
	entry, err := store.Get(profile)
	switch {
	case err == nil:
		return s3.Credentials{AccessKeyID: entry.AccessKeyID, SecretAccessKey: entry.SecretAccessKey, SessionToken: entry.SessionToken}, nil
	case errors.Is(err, credentials.ErrNotFound):
		// Empty credentials select the AWS default chain inside the backend,
		// which is what a CI runner with an instance role wants. If that has
		// nothing either, the first request fails with the login hint.
		return s3.Credentials{}, nil
	default:
		return s3.Credentials{}, err
	}
}

// envToken maps a remote name onto an environment-variable token:
// "eu-primary" becomes "EU_PRIMARY".
func envToken(remote string) string {
	return strings.ToUpper(strings.ReplaceAll(remote, "-", "_"))
}

func envCredentials(prefix string) (s3.Credentials, bool) {
	id := os.Getenv(prefix + "ACCESS_KEY_ID")
	secret := os.Getenv(prefix + "SECRET_ACCESS_KEY")
	if id == "" || secret == "" {
		return s3.Credentials{}, false
	}
	return s3.Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: os.Getenv(prefix + "SESSION_TOKEN")}, true
}

// localRemoteView is the clone-local binding for a remote, if any.
type localRemoteView struct {
	CredentialProfile          string
	Endpoint, Region, Bucket   string
	Prefix, Addressing         string
	Compress                   *bool
	PartSizeMiB, Concurrency   int
	MaxAttempts                int
	Path                       string
	Local, RequiredPublication bool
}

func (r *Repo) localRemote(name string) *localRemoteView {
	for _, l := range r.Local.Spec.Remotes {
		if l.Name != name {
			continue
		}
		return &localRemoteView{
			CredentialProfile: l.CredentialProfile, Endpoint: l.Endpoint, Region: l.Region,
			Bucket: l.Bucket, Prefix: l.Prefix, Addressing: l.Addressing, Compress: l.Compress,
			PartSizeMiB: l.PartSizeMiB, Concurrency: l.Concurrency, MaxAttempts: l.MaxAttempts, Path: l.Path,
			Local: l.Local, RequiredPublication: l.RequiredForPublication,
		}
	}
	return nil
}

// hasStoredLogin reports whether the credential store holds an entry for an
// s3 remote. It reads only the profile names, never a secret.
//
// The names are read once per opened repository: Remotes() is called inside
// publication planning, which asks about every object of an asset, and a
// chunked asset has hundreds. StorageLogin invalidates the memo.
func (r *Repo) hasStoredLogin(v RemoteView) bool {
	profile := v.CredentialProfile
	if profile == "" {
		profile = credentials.ProfileFor(r.Domain, v.Name)
	}
	for _, n := range r.credentialProfiles() {
		if n == profile {
			return true
		}
	}
	return false
}

func (r *Repo) credentialProfiles() []string {
	r.profilesMu.Lock()
	defer r.profilesMu.Unlock()
	if r.profiles != nil {
		return *r.profiles
	}
	names := []string{}
	if store, err := credentials.Open(r.opts.ConfigDir); err == nil {
		if got, err := store.Profiles(); err == nil {
			names = got
		}
	}
	r.profiles = &names
	return names
}

// forgetCredentialProfiles drops the memo after a login changed the store.
func (r *Repo) forgetCredentialProfiles() {
	r.profilesMu.Lock()
	r.profiles = nil
	r.profilesMu.Unlock()
}
