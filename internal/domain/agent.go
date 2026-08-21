package domain

// ---------------------------------------------------------------------------
// Agent-local configuration and deployment evidence. These are never committed
// to the coordination repository; they answer "how can this host implement the
// desired target?" (docs/spec/runtime-layout.md, docs/spec/trust-and-signing.md).
// ---------------------------------------------------------------------------

// AgentConfig is the host-local file consumed by the target agent.
type AgentConfig struct {
	Envelope `yaml:",inline"`
	Spec     AgentConfigSpec `json:"spec" yaml:"spec"`
}

type AgentConfigSpec struct {
	AgentID string               `json:"agentID" yaml:"agentID"`
	Git     AgentGitConfig       `json:"git" yaml:"git"`
	Targets []AgentTargetBinding `json:"targets" yaml:"targets"`
	// Safety floors cannot be relaxed by GitOps or cloud desired state.
	SafetyFloors AgentSafetyFloors `json:"safetyFloors" yaml:"safetyFloors"`
}

type AgentGitConfig struct {
	RemoteRewrite []RemoteRewrite `json:"remoteRewrite,omitempty" yaml:"remoteRewrite,omitempty"`
	// Blobless enables --filter=blob:none fetches (default true).
	Blobless *bool `json:"blobless,omitempty" yaml:"blobless,omitempty"`
	// Shallow is only honoured after provider capability checks.
	Shallow bool `json:"shallow,omitempty" yaml:"shallow,omitempty"`
}

type RemoteRewrite struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// AgentTargetBinding binds one portable Target to physical host resources.
type AgentTargetBinding struct {
	Target     string                     `json:"target" yaml:"target"`
	DeployRoot string                     `json:"deployRoot" yaml:"deployRoot"` // absolute
	ServerData string                     `json:"serverData" yaml:"serverData"` // absolute
	Runtime    RuntimeBinding             `json:"runtime" yaml:"runtime"`
	Verifiers  map[string]VerifierBinding `json:"verifiers,omitempty" yaml:"verifiers,omitempty"`
	Secrets    map[string]SecretProvider  `json:"secrets,omitempty" yaml:"secrets,omitempty"` // provider alias -> definition
	Trust      TrustPolicy                `json:"trust" yaml:"trust"`
	Hooks      map[string]HookDefinition  `json:"hooks,omitempty" yaml:"hooks,omitempty"` // allowlisted hook ID -> definition
}

type RuntimeBinding struct {
	Supervisor          string                    `json:"supervisor" yaml:"supervisor"` // txadmin | systemd | manual
	SystemdUnit         string                    `json:"systemdUnit,omitempty" yaml:"systemdUnit,omitempty"`
	RCON                *RCONEndpointRef          `json:"rcon,omitempty" yaml:"rcon,omitempty"`
	ResourceState       *ResourceStateEndpointRef `json:"resourceState,omitempty" yaml:"resourceState,omitempty"`
	TxData              string                    `json:"txData,omitempty" yaml:"txData,omitempty"`                           // absolute, outside deployRoot
	SystemResourcesRoot string                    `json:"systemResourcesRoot,omitempty" yaml:"systemResourcesRoot,omitempty"` // installed citizen/system_resources root
}

// ResourceStateEndpointRef binds the optional FXVCS-shipped loopback health
// resource. Its token is realization identity because it is rendered into the
// immutable runtime configuration without persisting plaintext in metadata.
type ResourceStateEndpointRef struct {
	Host        string    `json:"host" yaml:"host"`
	Port        int       `json:"port" yaml:"port"`
	TokenSecret SecretRef `json:"tokenSecret" yaml:"tokenSecret"`
}

// RCONEndpointRef names an endpoint; the password comes from a secret provider.
type RCONEndpointRef struct {
	Host           string    `json:"host" yaml:"host"`
	Port           int       `json:"port" yaml:"port"`
	PasswordSecret SecretRef `json:"passwordSecret" yaml:"passwordSecret"`
}

// SecretProvider defines what a provider alias means on this host.
type SecretProvider struct {
	Type string `json:"type" yaml:"type"` // env | envfile | fxvcs-cloud
	// Envfile path (envfile type). Must not be inside source, coordination, or
	// deployment roots; symlinks and insecure permissions are rejected.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// Aliases maps "namespace/key" to an exact variable name.
	Aliases map[string]string `json:"aliases,omitempty" yaml:"aliases,omitempty"`
}

// HookDefinition is the allowlisted implementation of a hook ID.
type HookDefinition struct {
	Type             string   `json:"type" yaml:"type"`                                     // template.v1
	AllowedDestRoot  string   `json:"allowedDestinationRoot" yaml:"allowedDestinationRoot"` // realization-relative, e.g. runtime/config
	TimeoutSeconds   int      `json:"timeoutSeconds" yaml:"timeoutSeconds"`
	MaxOutputBytes   int64    `json:"maxOutputBytes" yaml:"maxOutputBytes"`
	RunAs            string   `json:"runAs,omitempty" yaml:"runAs,omitempty"`
	AllowedTemplates []string `json:"allowedTemplates,omitempty" yaml:"allowedTemplates,omitempty"` // repository:path pins
}

type AgentSafetyFloors struct {
	MinimumRetention         int  `json:"minimumRetention" yaml:"minimumRetention"`
	MaxDeletionsPerHour      int  `json:"maxDeletionsPerHour" yaml:"maxDeletionsPerHour"`
	MaxAutomaticRollbacks    int  `json:"maxAutomaticRollbacks" yaml:"maxAutomaticRollbacks"`
	DisableAutomaticRollback bool `json:"disableAutomaticRollback" yaml:"disableAutomaticRollback"`
}

// ---------------------------------------------------------------------------
// Realization identity and deployment evidence
// ---------------------------------------------------------------------------

// RealizationInputs is the non-secret tuple whose canonical digest identifies a
// realization (docs/spec/runtime-layout.md §5). It never contains secret
// plaintext or unkeyed hashes of secret values.
type RealizationInputs struct {
	ReleaseName         string            `json:"releaseName"`
	ReleaseDigest       string            `json:"releaseDigest"`
	SourceSetDigest     string            `json:"sourceSetDigest"`
	EnvironmentName     string            `json:"environmentName"`
	EnvironmentRevision int               `json:"environmentRevision"`
	TargetContentDigest string            `json:"targetContentDigest"`
	HookDigests         map[string]string `json:"hookDigests"`      // hook ID -> template/definition digest
	VerifierBindings    map[string]string `json:"verifierBindings"` // contract -> canonical binding digest
	SecretVersions      map[string]string `json:"secretVersions"`   // "namespace/key" -> opaque version ID or HMAC fingerprint
}

// Deployment is the agent-local observed record of one realization attempt.
type Deployment struct {
	RealizationID      string            `json:"realizationID"`
	Target             string            `json:"target"`
	Inputs             RealizationInputs `json:"inputs"`
	Phase              string            `json:"phase"` // health.Phase persisted as a string
	RetryHeld          bool              `json:"retryHeld"`
	AttemptInvalidated bool              `json:"attemptInvalidated"`
	StartedAt          string            `json:"startedAt"`
	FinishedAt         string            `json:"finishedAt,omitempty"`
	ObservedRuntime    *ObservedRuntime  `json:"observedRuntime,omitempty"`
	ProcessGeneration  string            `json:"processGeneration,omitempty"`
	Outcome            string            `json:"outcome,omitempty"`
}

// ObservedRuntime is best-effort evidence about the target-provided runtime.
type ObservedRuntime struct {
	FXServerBuild            int               `json:"fxserverBuild,omitempty" yaml:"fxserverBuild,omitempty"`
	FXServerVersionSource    string            `json:"fxserverVersionSource" yaml:"fxserverVersionSource"` // version-file | info-json | process | unknown
	TxAdminVersion           string            `json:"txadminVersion,omitempty" yaml:"txadminVersion,omitempty"`
	SystemResourcesDigest    string            `json:"systemResourcesDigest,omitempty" yaml:"systemResourcesDigest,omitempty"`
	ExternalVerifierEvidence map[string]string `json:"externalVerifierEvidence,omitempty" yaml:"externalVerifierEvidence,omitempty"`
	SourcesDisagree          bool              `json:"sourcesDisagree,omitempty" yaml:"sourcesDisagree,omitempty"`
}
