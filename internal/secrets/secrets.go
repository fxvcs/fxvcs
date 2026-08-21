// Package secrets resolves Stage 4 host-local env and envfile providers while
// exposing only opaque keyed versions for realization identity and evidence.
package secrets

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/pathsafe"
)

var (
	ErrUnavailable     = errors.New("secrets: value unavailable")
	ErrUnsafeProvider  = errors.New("secrets: unsafe provider configuration")
	ErrUnknownProvider = errors.New("secrets: unknown provider")
)

type Value struct {
	Plaintext []byte
	Version   string
}

type LookupEnv func(string) (string, bool)

type Resolver struct {
	providers map[string]domain.SecretProvider
	hmacKey   []byte
	lookupEnv LookupEnv
	target    string
}

// Close clears the copied HMAC key as soon as a bounded resolution phase is
// complete instead of retaining protected key material until garbage collection.
func (r *Resolver) Close() {
	if r != nil {
		clear(r.hmacKey)
	}
}

func NewResolver(providers map[string]domain.SecretProvider, hmacKey []byte, lookup LookupEnv) (*Resolver, error) {
	return NewTargetResolver("", providers, hmacKey, lookup)
}

// NewTargetResolver binds automatic env naming and keyed versions to one
// canonical target/secret scope. Stage 4 reconciliation always uses this
// constructor; NewResolver remains for callers that supply explicit aliases.
func NewTargetResolver(target string, providers map[string]domain.SecretProvider, hmacKey []byte, lookup LookupEnv) (*Resolver, error) {
	if len(hmacKey) < 32 {
		return nil, errors.New("secrets: HMAC key must contain at least 256 bits")
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &Resolver{providers: providers, hmacKey: append([]byte(nil), hmacKey...), lookupEnv: lookup, target: target}, nil
}

func reference(ref domain.SecretRef) string { return ref.Namespace + "/" + ref.Key }

// Resolve obtains plaintext just in time and derives a domain-separated HMAC
// version. Plaintext is returned to the caller but is never persisted here.
func (r *Resolver) Resolve(ctx context.Context, providerAlias string, ref domain.SecretRef) (Value, error) {
	select {
	case <-ctx.Done():
		return Value{}, ctx.Err()
	default:
	}
	provider, ok := r.providers[providerAlias]
	if !ok {
		return Value{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerAlias)
	}
	name := provider.Aliases[reference(ref)]
	if name == "" {
		switch provider.Type {
		case "envfile":
			name = EnvfileVariable(ref)
		case "env":
			if r.target == "" {
				return Value{}, fmt.Errorf("%w: provider %s requires a target scope or explicit alias for %s", ErrUnavailable, providerAlias, reference(ref))
			}
			name = "FXVCS_SECRET_" + variableSegment(r.target) + "__" + EnvfileVariable(ref)
		}
	}
	var plaintext string
	switch provider.Type {
	case "env":
		var present bool
		plaintext, present = r.lookupEnv(name)
		if !present {
			return Value{}, fmt.Errorf("%w: environment alias %s", ErrUnavailable, name)
		}
	case "envfile":
		values, err := readEnvFile(provider.Path)
		if err != nil {
			return Value{}, err
		}
		var present bool
		plaintext, present = values[name]
		if !present {
			return Value{}, fmt.Errorf("%w: envfile alias %s", ErrUnavailable, name)
		}
	default:
		return Value{}, fmt.Errorf("%w: type %s", ErrUnknownProvider, provider.Type)
	}
	bytes := []byte(plaintext)
	mac := hmac.New(sha256.New, r.hmacKey)
	_, _ = mac.Write([]byte("fxvcs-secret-version-v1\x00"))
	_, _ = mac.Write([]byte(r.target))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(reference(ref)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(bytes)
	return Value{Plaintext: bytes, Version: "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))}, nil
}

func readEnvFile(path string) (map[string]string, error) {
	if err := ValidateEnvfile(path, nil); err != nil {
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(linked, opened) {
		return nil, fmt.Errorf("%w: envfile changed while opening", ErrUnsafeProvider)
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	// Bound pathological lines while allowing typical certificates/tokens.
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("%w: invalid envfile assignment", ErrUnsafeProvider)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate envfile variable %s", ErrUnsafeProvider, name)
		}
		values[name] = value
	}
	return values, scanner.Err()
}

// ValidateEnvfile proves an absolute ordinary owner-only file has no symlink
// in its existing path and does not live beneath any forbidden root.
func ValidateEnvfile(path string, forbiddenRoots []string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%w: envfile path must be clean and absolute", ErrUnsafeProvider)
	}
	for _, root := range forbiddenRoots {
		if root == "" {
			continue
		}
		if pathsafe.Within(filepath.Clean(root), path) {
			return fmt.Errorf("%w: envfile %s is beneath forbidden root %s", ErrUnsafeProvider, path, root)
		}
	}
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume+string(filepath.Separator))
	current := volume + string(filepath.Separator)
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: envfile path traverses symlink %s", ErrUnsafeProvider, current)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: envfile must be an ordinary readable owner-only file owned by the agent user", ErrUnsafeProvider)
	}
	return nil
}

// LoadOrCreateHMACKey loads an owner-only 32-byte key or atomically creates
// one. Existing symlinks, wrong modes, and wrong lengths fail closed.
func LoadOrCreateHMACKey(path string) ([]byte, error) {
	if info, err := os.Lstat(path); err == nil {
		_ = info
		return LoadHMACKey(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateHMACKey(path)
		}
		return nil, err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(key); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return nil, err
	}
	ok = true
	return key, nil
}

// LoadHMACKey reads an existing protected target key without creating state.
// Dry-run reconciliation uses it so planning remains mutation-free.
func LoadHMACKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return nil, fmt.Errorf("%w: HMAC key must be an ordinary owner-only file owned by the agent user", ErrUnsafeProvider)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: HMAC key must be exactly 32 bytes", ErrUnsafeProvider)
	}
	return key, nil
}
