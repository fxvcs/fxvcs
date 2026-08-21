// Package domain defines the Go representation of every FXVCS persisted format.
// YAML is the human/committed presentation; canonical JSON (internal/canonical)
// is the identity encoding. All types carry both `json` and `yaml` tags with
// identical names so the same document parses from either.
//
// Nothing here performs I/O. Validation beyond schema shape lives with the
// owning package (internal/schema for structural rules, release/stack
// packages in later stages for cross-document rules).
package domain

import "fxvcs.dev/fxvcs/internal/format"

// Envelope is embedded in every committed document.
type Envelope = format.Envelope

// ---------------------------------------------------------------------------
// Repository (.fxvcs/repository.yaml)
// ---------------------------------------------------------------------------

// Repository is the committed component/repository configuration.
type Repository struct {
	Envelope `yaml:",inline"`
	Metadata RepositoryMetadata `json:"metadata" yaml:"metadata"`
	Spec     RepositorySpec     `json:"spec" yaml:"spec"`
}

type RepositoryMetadata struct {
	RepositoryID    string `json:"repositoryID" yaml:"repositoryID"`
	StorageDomainID string `json:"storageDomainID" yaml:"storageDomainID"`
	Name            string `json:"name" yaml:"name"`
}

type RepositorySpec struct {
	Profile               string          `json:"profile" yaml:"profile"` // "generic" | "fivem"
	MinimumFXVCSVersion   string          `json:"minimumFXVCSVersion" yaml:"minimumFXVCSVersion"`
	GitIntegrationVersion int             `json:"gitIntegrationVersion" yaml:"gitIntegrationVersion"`
	SourceRoot            string          `json:"sourceRoot,omitempty" yaml:"sourceRoot,omitempty"`     // fivem
	Layout                string          `json:"layout,omitempty" yaml:"layout,omitempty"`             // fivem: single | collection
	ResourceName          *string         `json:"resourceName,omitempty" yaml:"resourceName,omitempty"` // fivem single only
	Storage               RepositoryStore `json:"storage" yaml:"storage"`
	Tracking              *TrackingPolicy `json:"tracking,omitempty" yaml:"tracking,omitempty"`
}

// TrackingPolicy holds committed, reviewable tracking preferences that
// `track`/`untrack` maintain and `upgrade` honours.
type TrackingPolicy struct {
	// DisabledPresets lists profile preset patterns the repository has opted
	// out of (`fxvcs untrack --glob <preset>`). Upgrade never re-adds them.
	DisabledPresets []string `json:"disabledPresets,omitempty" yaml:"disabledPresets,omitempty"`
}

type RepositoryStore struct {
	Remotes []StorageRemote `json:"remotes" yaml:"remotes"`
}

// StorageRemote is a non-secret shared remote descriptor.
//
//   - filesystem: a directory bound per clone (local config); nothing else
//     committed.
//   - s3: bring-your-own S3-compatible bucket; endpoint/bucket/prefix are
//     committed, credentials stay local.
//   - fxvcs-cloud: hosted storage. Nothing provider-specific is committed; the
//     client resolves the organization bucket and short-lived credentials
//     from the control plane by storageDomainID, and the login session is the
//     binding. Reserved in this build (not yet implemented).
type StorageRemote struct {
	Name       string `json:"name" yaml:"name"`
	Type       string `json:"type" yaml:"type"` // filesystem | s3 | fxvcs-cloud
	Endpoint   string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Bucket     string `json:"bucket,omitempty" yaml:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Region     string `json:"region,omitempty" yaml:"region,omitempty"`         // s3; "auto" for providers that ignore it
	Addressing string `json:"addressing,omitempty" yaml:"addressing,omitempty"` // path | virtual-host (s3)

	RequiredForPublication bool `json:"requiredForPublication" yaml:"requiredForPublication"`
}

// ---------------------------------------------------------------------------
// FiveM component resource catalog (.fxvcs/resources.yaml)
// ---------------------------------------------------------------------------

type ResourceCatalog struct {
	Envelope `yaml:",inline"`
	Metadata ResourceCatalogMetadata `json:"metadata" yaml:"metadata"`
	Spec     ResourceCatalogSpec     `json:"spec" yaml:"spec"`
}

type ResourceCatalogMetadata struct {
	RepositoryID string `json:"repositoryID" yaml:"repositoryID"`
}

type ResourceCatalogSpec struct {
	Resources []CatalogResource `json:"resources" yaml:"resources"`
}

// Activation is the explicit tri-state. Never null/missing.
type Activation string

const (
	ActivationEnabled  Activation = "enabled"
	ActivationDisabled Activation = "disabled"
	ActivationPending  Activation = "pending"
)

// CatalogResource is one component resource; identity is (repositoryID, Path).
type CatalogResource struct {
	Path         string     `json:"path" yaml:"path"`
	Name         string     `json:"name" yaml:"name"`
	Activation   Activation `json:"activation" yaml:"activation"`
	After        []string   `json:"after" yaml:"after"`
	Before       []string   `json:"before" yaml:"before"`
	Provides     []string   `json:"provides" yaml:"provides"`
	Dependencies []string   `json:"dependencies" yaml:"dependencies"`
	// RuntimeRequirements are the `/`-prefixed manifest dependency entries
	// (/server:NNNN, /onesync, /gameBuild:xx, /policy:xx, /native:0x..):
	// FXServer capability requirements, never resource names.
	RuntimeRequirements []string `json:"runtimeRequirements,omitempty" yaml:"runtimeRequirements,omitempty"`
	Games               []string `json:"games" yaml:"games"`
	ServerOnly          bool     `json:"serverOnly" yaml:"serverOnly"`
	PersistentPaths     []string `json:"persistentPaths" yaml:"persistentPaths"`
	// PersistentPathKinds declares whether each persistentPaths entry is a
	// dynamically-created file or directory. V1 never guesses from extensions
	// and does not seed source-controlled bytes into shared mutable storage.
	PersistentPathKinds map[string]string `json:"persistentPathKinds,omitempty" yaml:"persistentPathKinds,omitempty"`
	MutableMode         string            `json:"mutableMode" yaml:"mutableMode"` // immutable | copy
	Status              string            `json:"status" yaml:"status"`           // present | missing
}

// ---------------------------------------------------------------------------
// Coordination-owned runtime/external resources and sparse overrides
// ---------------------------------------------------------------------------

type RuntimeResourceCatalog struct {
	Envelope `yaml:",inline"`
	Spec     RuntimeResourceCatalogSpec `json:"spec" yaml:"spec"`
}

type RuntimeResourceCatalogSpec struct {
	Resources []RuntimeResource `json:"resources" yaml:"resources"`
}

// RuntimeResource has ID "runtime:<provider>/<path>" or "external:<owner>/<name>".
type RuntimeResource struct {
	ID         string             `json:"id" yaml:"id"`
	Name       string             `json:"name" yaml:"name"`
	Source     string             `json:"source" yaml:"source"` // runtime | external
	Activation Activation         `json:"activation" yaml:"activation"`
	After      []string           `json:"after,omitempty" yaml:"after,omitempty"`
	Before     []string           `json:"before,omitempty" yaml:"before,omitempty"`
	Verifier   *VerifierContract  `json:"verifier,omitempty" yaml:"verifier,omitempty"`   // external only
	Lifecycle  *LifecycleContract `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"` // external only
}

// VerifierContract is the portable requirement; it enters sourceSetDigest.
type VerifierContract struct {
	Contract string `json:"contract" yaml:"contract"` // e.g. resource-present
	Version  int    `json:"version" yaml:"version"`
}

type LifecycleContract struct {
	Ownership string `json:"ownership" yaml:"ownership"` // external
	Actions   string `json:"actions" yaml:"actions"`     // observe-only
}

type ResourceOverrides struct {
	Envelope `yaml:",inline"`
	Spec     ResourceOverridesSpec `json:"spec" yaml:"spec"`
}

type ResourceOverridesSpec struct {
	Resources []ResourceOverride `json:"resources" yaml:"resources"`
}

// ResourceOverride may set only activation/after/before.
type ResourceOverride struct {
	RepositoryID string      `json:"repositoryID,omitempty" yaml:"repositoryID,omitempty"`
	Path         string      `json:"path,omitempty" yaml:"path,omitempty"`
	ResourceID   string      `json:"resourceID,omitempty" yaml:"resourceID,omitempty"` // runtime/external
	Activation   *Activation `json:"activation,omitempty" yaml:"activation,omitempty"`
	After        []string    `json:"after,omitempty" yaml:"after,omitempty"`
	Before       []string    `json:"before,omitempty" yaml:"before,omitempty"`
}

// ---------------------------------------------------------------------------
// Stack (coordination repository fxvcs.yaml + repositories/*.yaml)
// ---------------------------------------------------------------------------

type Stack struct {
	Envelope `yaml:",inline"`
	Metadata StackMetadata `json:"metadata" yaml:"metadata"`
	Spec     StackSpec     `json:"spec" yaml:"spec"`
}

type StackMetadata struct {
	StackID string `json:"stackID" yaml:"stackID"`
	Name    string `json:"name" yaml:"name"`
}

type StackSpec struct {
	Profile             string           `json:"profile" yaml:"profile"`
	MinimumFXVCSVersion string           `json:"minimumFXVCSVersion" yaml:"minimumFXVCSVersion"`
	Components          []StackComponent `json:"components" yaml:"components"`
}

// StackComponent registers a component repository and its mount.
type StackComponent struct {
	RepositoryID       string               `json:"repositoryID" yaml:"repositoryID"`
	Remote             string               `json:"remote" yaml:"remote"`
	Ref                string               `json:"ref,omitempty" yaml:"ref,omitempty"` // default branch/tag input; releases resolve to SHA
	Required           bool                 `json:"required" yaml:"required"`
	Mount              string               `json:"mount" yaml:"mount"`                     // e.g. resources/[maps]
	Alias              string               `json:"alias,omitempty" yaml:"alias,omitempty"` // resolved display name when metadata.name collides
	DescriptorOverride *ComponentDescriptor `json:"descriptorOverride,omitempty" yaml:"descriptorOverride,omitempty"`
}

// Target desired-state authorities. Exactly one owns a target at a time.
const (
	AuthorityGitOps = "gitops"
	AuthorityCloud  = "cloud"
)

// Resource sources, as recorded on a Release entry.
const (
	ResourceSourceComponent = "component"
	ResourceSourceRuntime   = "runtime"
	ResourceSourceExternal  = "external"
)

// Catalog entry status.
const (
	ResourceStatusPresent = "present"
	ResourceStatusMissing = "missing"
)

// Repository profiles.
const (
	ProfileGeneric = "generic"
	ProfileFiveM   = "fivem"
)

// Component layouts.
const (
	LayoutSingle     = "single"
	LayoutCollection = "collection"
)

// Descriptor provenance, recorded per component in a Release.
const (
	DescriptorFromComponent = "component"
	DescriptorFromStack     = "stack-override"
)

// ComponentDescriptor is the accepted layout, from the component (or override).
type ComponentDescriptor struct {
	SourceRoot   string  `json:"sourceRoot" yaml:"sourceRoot"`
	Layout       string  `json:"layout" yaml:"layout"` // single | collection
	ResourceName *string `json:"resourceName,omitempty" yaml:"resourceName,omitempty"`
}

// ---------------------------------------------------------------------------
// Release (releases/<name>.yaml)
// ---------------------------------------------------------------------------

type Release struct {
	Envelope `yaml:",inline"`
	Metadata ReleaseMetadata `json:"metadata" yaml:"metadata"`
	Spec     ReleaseSpec     `json:"spec" yaml:"spec"`
}

type ReleaseMetadata struct {
	Name      string `json:"name" yaml:"name"`
	CreatedAt string `json:"createdAt" yaml:"createdAt"` // RFC 3339 UTC
	CreatedBy string `json:"createdBy" yaml:"createdBy"`
}

type ReleaseSpec struct {
	StackID             string             `json:"stackID" yaml:"stackID"`
	MinimumFXVCSVersion string             `json:"minimumFXVCSVersion,omitempty" yaml:"minimumFXVCSVersion,omitempty"`
	SourceSetDigest     string             `json:"sourceSetDigest" yaml:"sourceSetDigest"`
	VariantOf           string             `json:"variantOf,omitempty" yaml:"variantOf,omitempty"`
	Components          []ReleaseComponent `json:"components" yaml:"components"`
	RuntimeResources    []RuntimeResource  `json:"runtimeResources" yaml:"runtimeResources"` // snapshotted portable requirements
	Resources           []ReleaseResource  `json:"resources" yaml:"resources"`               // fully resolved plan in ensure order
	Signature           *ReleaseSignature  `json:"signature,omitempty" yaml:"signature,omitempty"`
}

type ReleaseComponent struct {
	RepositoryID    string              `json:"repositoryID" yaml:"repositoryID"`
	RepositoryName  string              `json:"repositoryName" yaml:"repositoryName"` // display; excluded from sourceSetDigest
	Remote          string              `json:"remote" yaml:"remote"`
	Commit          string              `json:"commit" yaml:"commit"`       // 40-hex SHA-1 or 64-hex SHA-256
	AssetRoot       string              `json:"assetRoot" yaml:"assetRoot"` // sha256:...
	StorageDomainID string              `json:"storageDomainID" yaml:"storageDomainID"`
	Descriptor      ComponentDescriptor `json:"descriptor" yaml:"descriptor"`
	DescriptorFrom  string              `json:"descriptorFrom" yaml:"descriptorFrom"` // component | stack-override
	Mount           string              `json:"mount" yaml:"mount"`
}

// ReleaseResource is one entry of the exact activation plan.
type ReleaseResource struct {
	Source           string     `json:"source" yaml:"source"` // component | runtime | external
	RepositoryID     string     `json:"repositoryID,omitempty" yaml:"repositoryID,omitempty"`
	RepositoryName   string     `json:"repositoryName,omitempty" yaml:"repositoryName,omitempty"`
	Path             string     `json:"path,omitempty" yaml:"path,omitempty"`
	ResourceID       string     `json:"resourceID,omitempty" yaml:"resourceID,omitempty"`
	Name             string     `json:"name" yaml:"name"`
	Activation       Activation `json:"activation" yaml:"activation"` // enabled | disabled (never pending)
	ActivationSource string     `json:"activationSource" yaml:"activationSource"`
	After            []string   `json:"after,omitempty" yaml:"after,omitempty"`
	Before           []string   `json:"before,omitempty" yaml:"before,omitempty"`
	Dependencies     []string   `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Provides         []string   `json:"provides,omitempty" yaml:"provides,omitempty"`
	EnsureOrder      int        `json:"ensureOrder" yaml:"ensureOrder"`
}

// Activation sources.
const (
	ActivationSourceComponent    = "component"
	ActivationSourceRuntime      = "runtime-catalog"
	ActivationSourceCoordination = "coordination-override"
	ActivationSourceRelease      = "release-override"
)

type ReleaseSignature struct {
	Profile    string   `json:"profile" yaml:"profile"` // ssh-ed25519-v1
	Signatures []string `json:"signatures" yaml:"signatures"`
}

// ---------------------------------------------------------------------------
// Environment and Target
// ---------------------------------------------------------------------------

type Environment struct {
	Envelope `yaml:",inline"`
	Metadata NamedMetadata   `json:"metadata" yaml:"metadata"`
	Spec     EnvironmentSpec `json:"spec" yaml:"spec"`
}

type NamedMetadata struct {
	Name string `json:"name" yaml:"name"`
}

type EnvironmentSpec struct {
	Revision    int               `json:"revision" yaml:"revision"`
	Inputs      map[string]string `json:"inputs" yaml:"inputs"`
	SecretScope string            `json:"secretScope" yaml:"secretScope"`
}

type Target struct {
	Envelope `yaml:",inline"`
	Metadata NamedMetadata `json:"metadata" yaml:"metadata"`
	Spec     TargetSpec    `json:"spec" yaml:"spec"`
}

type TargetSpec struct {
	Release     string                     `json:"release" yaml:"release"`
	Environment string                     `json:"environment" yaml:"environment"`
	AgentID     string                     `json:"agentID" yaml:"agentID"`
	Authority   string                     `json:"authority" yaml:"authority"` // gitops | cloud
	RuntimeRef  string                     `json:"runtimeRef" yaml:"runtimeRef"`
	Retention   RetentionPolicy            `json:"retention" yaml:"retention"`
	Secrets     SecretPolicy               `json:"secrets" yaml:"secrets"`
	Health      *HealthPolicy              `json:"health,omitempty" yaml:"health,omitempty"`
	Hooks       *HookRequests              `json:"hooks,omitempty" yaml:"hooks,omitempty"`
	Verifiers   map[string]VerifierBinding `json:"verifiers,omitempty" yaml:"verifiers,omitempty"`
	Trust       *TrustPolicy               `json:"trust,omitempty" yaml:"trust,omitempty"`
}

type RetentionPolicy struct {
	Keep int `json:"keep" yaml:"keep"`
}

type SecretPolicy struct {
	DefaultProvider string                       `json:"defaultProvider" yaml:"defaultProvider"`
	Namespaces      map[string]NamespaceProvider `json:"namespaces,omitempty" yaml:"namespaces,omitempty"`
}

type NamespaceProvider struct {
	Provider string `json:"provider" yaml:"provider"`
}

// HealthPolicy — docs/spec/health-and-rollback.md.
type HealthPolicy struct {
	StartupGrace          string        `json:"startupGrace" yaml:"startupGrace"`
	Interval              string        `json:"interval" yaml:"interval"`
	Timeout               string        `json:"timeout" yaml:"timeout"`
	SuccessThreshold      int           `json:"successThreshold" yaml:"successThreshold"`
	FailureThreshold      int           `json:"failureThreshold" yaml:"failureThreshold"`
	SoakDuration          string        `json:"soakDuration" yaml:"soakDuration"`
	OnFailure             string        `json:"onFailure" yaml:"onFailure"` // rollback | keep | none
	MaxAutomaticRollbacks int           `json:"maxAutomaticRollbacks" yaml:"maxAutomaticRollbacks"`
	RollbackWindow        string        `json:"rollbackWindow" yaml:"rollbackWindow"`
	ActivationWindow      *string       `json:"activationWindow" yaml:"activationWindow"`
	MaxPlayersForRestart  *int          `json:"maxPlayersForRestart" yaml:"maxPlayersForRestart"`
	Checks                []HealthCheck `json:"checks" yaml:"checks"`
}

type HealthCheck struct {
	Type      string   `json:"type" yaml:"type"` // tcp | fivem-http | rcon-status | fxvcs-resource-state
	Endpoints []string `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
}

type HookRequests struct {
	PostMaterialize []HookRequest `json:"postMaterialize,omitempty" yaml:"postMaterialize,omitempty"`
	PreActivate     []HookRequest `json:"preActivate,omitempty" yaml:"preActivate,omitempty"`
	PostActivate    []HookRequest `json:"postActivate,omitempty" yaml:"postActivate,omitempty"`
	OnFailure       []HookRequest `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
}

// HookRequest names an allowlisted hook; it never contains executable text.
type HookRequest struct {
	ID          string                 `json:"id" yaml:"id"`
	Type        string                 `json:"type" yaml:"type"` // template.v1
	Template    *TemplateRef           `json:"template,omitempty" yaml:"template,omitempty"`
	Destination string                 `json:"destination,omitempty" yaml:"destination,omitempty"`
	Mode        string                 `json:"mode,omitempty" yaml:"mode,omitempty"` // "0600"
	Inputs      map[string]InputSource `json:"inputs,omitempty" yaml:"inputs,omitempty"`
}

type TemplateRef struct {
	Repository string `json:"repository" yaml:"repository"`
	Path       string `json:"path" yaml:"path"`
	Digest     string `json:"digest" yaml:"digest"`
}

// InputSource is the discriminated union: exactly one field set.
type InputSource struct {
	Secret  *SecretRef `json:"secret,omitempty" yaml:"secret,omitempty"`
	Target  *string    `json:"target,omitempty" yaml:"target,omitempty"`   // schema-checked target path e.g. server.hostname
	Release *string    `json:"release,omitempty" yaml:"release,omitempty"` // schema-checked release path e.g. id
	Literal *string    `json:"literal,omitempty" yaml:"literal,omitempty"`
}

type SecretRef struct {
	Namespace string `json:"namespace" yaml:"namespace"` // lowercase kebab-case
	Key       string `json:"key" yaml:"key"`
}

// VerifierBinding maps a portable contract to a target-local implementation.
type VerifierBinding struct {
	Implementation        string `json:"implementation" yaml:"implementation"`
	ImplementationVersion int    `json:"implementationVersion" yaml:"implementationVersion"`
	EndpointRef           string `json:"endpointRef,omitempty" yaml:"endpointRef,omitempty"`
}

// TrustPolicy — docs/spec/trust-and-signing.md (agent-local, shown for schema).
type TrustPolicy struct {
	Coordination CoordinationTrust `json:"coordination" yaml:"coordination"`
}

type CoordinationTrust struct {
	Authority          string `json:"authority" yaml:"authority"`   // signed-commit-tip | signed-promotion-tag
	Signatures         string `json:"signatures" yaml:"signatures"` // required | optional | off
	AllowedSignersFile string `json:"allowedSignersFile,omitempty" yaml:"allowedSignersFile,omitempty"`
	PromotionTagPrefix string `json:"promotionTagPrefix,omitempty" yaml:"promotionTagPrefix,omitempty"`
}

// ---------------------------------------------------------------------------
// Low-level canonical JSON documents (never YAML)
// ---------------------------------------------------------------------------

// ObjectManifest describes how an object's bytes are stored. Its canonical
// digest is the pointer's manifest field.
type ObjectManifest struct {
	FormatVersion   int             `json:"formatVersion"`
	StorageDomainID string          `json:"storageDomainID"`
	Encoding        string          `json:"encoding"`    // whole/1 | cdc-fastcdc/1
	Compression     string          `json:"compression"` // none | zstd/1
	OID             string          `json:"oid"`         // whole-file sha256
	Size            int64           `json:"size"`        // uncompressed
	Chunks          []ManifestChunk `json:"chunks"`
}

type ManifestChunk struct {
	Digest      string `json:"digest"` // sha256 of raw (uncompressed) chunk bytes
	RawSize     int64  `json:"rawSize"`
	EncodedSize int64  `json:"encodedSize"` // bytes as stored (after compression)
}

// AssetIndex is derived from a pinned Git tree; its canonical digest is the
// component's assetRoot.
type AssetIndex struct {
	FormatVersion   int               `json:"formatVersion"`
	StorageDomainID string            `json:"storageDomainID"`
	Entries         []AssetIndexEntry `json:"entries"` // sorted by Path (byte order)
}

type AssetIndexEntry struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`          // Git mode e.g. "100644"
	PointerDigest string `json:"pointerDigest"` // sha256 of exact pointer bytes
}
