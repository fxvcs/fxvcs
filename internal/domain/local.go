package domain

// LocalConfig is the clone-local, uncommitted configuration stored at
// $GIT_COMMON_DIR/fxvcs/config.yaml (build plan §7 "Repository configuration").
// It holds machine-specific remote bindings — filesystem mirror paths,
// credential-profile names, TLS overrides, read order — and never secrets.
type LocalConfig struct {
	Envelope `yaml:",inline"`
	Spec     LocalConfigSpec `json:"spec" yaml:"spec"`
}

type LocalConfigSpec struct {
	// Remotes binds committed remote names (or purely local remotes) to
	// machine-specific settings.
	Remotes []LocalRemote `json:"remotes" yaml:"remotes"`
	// ReadOrder lists remote names to try first when fetching objects; unlisted
	// remotes follow in committed order.
	ReadOrder []string `json:"readOrder,omitempty" yaml:"readOrder,omitempty"`
	// LauncherPath is the absolute driver launcher installed by setup/upgrade.
	LauncherPath string `json:"launcherPath,omitempty" yaml:"launcherPath,omitempty"`
	// Cache tunes the host-wide object cache from this clone.
	Cache *CacheSettings `json:"cache,omitempty" yaml:"cache,omitempty"`
}

// CacheSettings bound the host object cache.
type CacheSettings struct {
	// LimitBytes is the size the cache is trimmed back to after an operation
	// adds objects to it. Zero means no automatic trimming: the cache grows
	// until `fxvcs cache prune` is run. Objects pending publication are never
	// evicted, so the cache may exceed the limit until they are published.
	LimitBytes int64 `json:"limitBytes,omitempty" yaml:"limitBytes,omitempty"`
}

// LocalRemote is one machine-specific remote binding.
type LocalRemote struct {
	Name string `json:"name" yaml:"name"`
	// Type is filesystem | s3. For a committed remote the type must match.
	Type string `json:"type" yaml:"type"`
	// Path is the absolute directory of a filesystem remote on this machine.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// CredentialProfile names an entry in the credential store written by
	// `fxvcs storage login` (s3). Empty means the default profile,
	// "<storageDomainID>/<remote>".
	CredentialProfile string `json:"credentialProfile,omitempty" yaml:"credentialProfile,omitempty"`
	// Endpoint, Region, and Addressing override the committed descriptor for
	// this machine only: an in-office mirror endpoint, a provider that needs
	// path-style addressing behind a proxy, a region the committed record
	// leaves open. Empty means "use the committed value".
	Endpoint   string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Region     string `json:"region,omitempty" yaml:"region,omitempty"`
	Addressing string `json:"addressing,omitempty" yaml:"addressing,omitempty"`
	// Bucket and Prefix exist for a purely local s3 remote (a personal
	// scratch bucket); a committed remote takes them from the repository.
	Bucket string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	// Compress selects the zstd storage codec for this remote. Nil means the
	// default (on): objects are stored compressed only where that is
	// actually smaller. Set it false for a provider that drops user metadata
	// (`fxvcs storage test` reports that as object-metadata).
	Compress *bool `json:"compress,omitempty" yaml:"compress,omitempty"`
	// PartSizeMiB, Concurrency, and MaxAttempts tune transfers on this
	// machine; 0 means the backend default. MaxAttempts bounds retries of a
	// failing request, which matters on a flaky link and in tests that expect
	// a fast, definite failure.
	PartSizeMiB int `json:"partSizeMiB,omitempty" yaml:"partSizeMiB,omitempty"`
	Concurrency int `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	MaxAttempts int `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
	// Local marks a remote that exists only in this clone (a personal mirror
	// or scratch store); it is never required for publication and never
	// committed.
	Local bool `json:"local,omitempty" yaml:"local,omitempty"`
	// RequiredForPublication applies only to Local remotes; committed remotes
	// carry this flag in repository.yaml.
	RequiredForPublication bool `json:"requiredForPublication,omitempty" yaml:"requiredForPublication,omitempty"`
}
