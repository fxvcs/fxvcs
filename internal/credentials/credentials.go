// Package credentials stores the object-store secrets `fxvcs storage login`
// records, outside the repository and outside Git's administrative
// directories.
//
// Where and why:
//
//	<os.UserConfigDir()>/fxvcs/credentials.json   0600, in a 0700 directory
//
// Not in the working tree, because committed configuration must never carry
// secrets. Not under $GIT_COMMON_DIR either: a clone is copied, archived, and
// handed to colleagues far more casually than a user configuration directory,
// and one storage identity usually serves several clones of the same domain.
//
// Entries are keyed by a credential *profile* name, which a clone's local
// configuration references. The default profile for a remote is
// "<storageDomainID>/<remote>", so two repositories that share a storage
// domain share one login and two that do not, do not.
//
// This file holds long-lived provider keys, so it is deliberately boring:
// no caching in environment variables, no copying into logs, restrictive
// permissions checked on every read, and a refusal to load anything
// world-readable.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// EnvDir overrides the credential directory (tests, and operators who keep
// configuration on an encrypted volume).
const EnvDir = "FXVCS_CONFIG_DIR"

// FileName is the credential file inside the configuration directory.
const FileName = "credentials.json"

// Errors.
var (
	ErrNotFound    = errors.New("credentials: no entry for this profile")
	ErrPermissions = errors.New("credentials: file permissions are too open")
	ErrInvalid     = errors.New("credentials: invalid entry")
)

// Entry is one stored object-store identity.
type Entry struct {
	// Type is the backend the entry is for; only "s3" exists today.
	Type string `json:"type"`
	// AccessKeyID and SecretAccessKey are the provider keys.
	AccessKeyID     string `json:"accessKeyID"`
	SecretAccessKey string `json:"secretAccessKey"`
	// SessionToken is set for temporary credentials.
	SessionToken string `json:"sessionToken,omitempty"`
	// Endpoint and Region are recorded only as a convenience for diagnostics;
	// the committed remote descriptor remains authoritative.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
}

// Validate rejects an entry that could not be used.
func (e Entry) Validate() error {
	switch {
	case e.Type != "s3":
		return fmt.Errorf("%w: unsupported type %q", ErrInvalid, e.Type)
	case strings.TrimSpace(e.AccessKeyID) == "":
		return fmt.Errorf("%w: access key id is empty", ErrInvalid)
	case strings.TrimSpace(e.SecretAccessKey) == "":
		return fmt.Errorf("%w: secret access key is empty", ErrInvalid)
	}
	return nil
}

// file is the on-disk document.
type file struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

const currentVersion = 1

// Store is a handle on one credential file.
type Store struct{ path string }

// DefaultDir returns FXVCS_CONFIG_DIR if set, else <UserConfigDir>/fxvcs.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		return filepath.Clean(d), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("credentials: locate user config dir: %w", err)
	}
	return filepath.Join(base, "fxvcs"), nil
}

// Open returns the store at dir (empty selects DefaultDir). It creates
// nothing: reading a missing file is an empty store.
func Open(dir string) (*Store, error) {
	if dir == "" {
		d, err := DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(abs, FileName)}, nil
}

// Path is the credential file location, for diagnostics.
func (s *Store) Path() string { return s.path }

// ProfileFor is the default profile name for a remote in a storage domain.
func ProfileFor(storageDomainID, remote string) string { return storageDomainID + "/" + remote }

// Get returns the entry for a profile.
func (s *Store) Get(profile string) (Entry, error) {
	doc, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	e, ok := doc.Entries[profile]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, profile)
	}
	return e, nil
}

// Profiles lists the stored profile names (never their secrets).
func (s *Store) Profiles() ([]string, error) {
	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(doc.Entries))
	for name := range doc.Entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Set writes (or replaces) one profile. The file is written through a
// temporary file in the same directory with 0600 and renamed, so a crash
// never leaves a half-written credential file or widens permissions.
func (s *Store) Set(profile string, e Entry) error {
	if strings.TrimSpace(profile) == "" {
		return fmt.Errorf("%w: profile name is empty", ErrInvalid)
	}
	if err := e.Validate(); err != nil {
		return err
	}
	doc, err := s.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if doc.Entries == nil {
		doc.Entries = map[string]Entry{}
	}
	doc.Version = currentVersion
	doc.Entries[profile] = e
	return s.save(doc)
}

// Remove deletes a profile; a missing profile is not an error.
func (s *Store) Remove(profile string) error {
	doc, err := s.load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, ok := doc.Entries[profile]; !ok {
		return nil
	}
	delete(doc.Entries, profile)
	return s.save(doc)
}

func (s *Store) load() (file, error) {
	doc := file{Version: currentVersion, Entries: map[string]Entry{}}
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return doc, nil
		}
		return doc, err
	}
	// Windows permission bits do not mean what they mean elsewhere, so the
	// check applies where it is meaningful.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return doc, fmt.Errorf("%w: %s is %#o; run: chmod 600 %s", ErrPermissions, s.path, info.Mode().Perm(), s.path)
	}
	data, err := os.ReadFile(s.path) //nolint:gosec // the credential file this package owns
	if err != nil {
		return doc, err
	}
	if len(data) == 0 {
		return doc, nil
	}
	var parsed file
	if err := json.Unmarshal(data, &parsed); err != nil {
		return doc, fmt.Errorf("credentials: parse %s: %w", s.path, err)
	}
	if parsed.Version > currentVersion {
		return doc, fmt.Errorf("credentials: %s has version %d, this fxvcs understands %d; run: fxvcs upgrade --check", s.path, parsed.Version, currentVersion)
	}
	if parsed.Entries == nil {
		parsed.Entries = map[string]Entry{}
	}
	return parsed, nil
}

func (s *Store) save(doc file) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(name) }
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, s.path)
}
