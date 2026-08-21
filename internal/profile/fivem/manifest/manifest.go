// Package manifest evaluates FiveM resource manifests (fxmanifest.lua and the
// legacy __resource.lua) inside a deterministic, capability-free Lua sandbox
// and returns the recorded metadata as plain data.
//
// The manifest DSL is a set of pure recording functions installed as globals.
// The sandbox exposes only the base, string, table, and math libraries with
// every effectful or non-deterministic function removed. Every evaluation is
// bounded by instruction, allocation, recursion, output, string-length,
// source-size, and wall-clock limits; exceeding any of them returns a typed
// *ErrLimitExceeded. See docs/spec/manifest-sandbox.md.
package manifest

import "time"

// Options tunes one evaluation. Zero values select the defaults below.
type Options struct {
	// Legacy marks the source as __resource.lua rather than fxmanifest.lua.
	// It changes which declarations are mandatory (fx_version/game are not
	// required in the legacy format).
	Legacy bool
	// DefaultName is used for Manifest.Name when the manifest declares no
	// `name`; callers usually pass the resource directory name.
	DefaultName string
	// ChunkName is used in Lua error messages (defaults to "fxmanifest.lua").
	ChunkName string

	MaxInstructions uint64        // VM steps; default DefaultMaxInstructions
	MaxAllocBytes   uint64        // cumulative allocation delta; default DefaultMaxAllocBytes
	MaxCallDepth    int           // Lua call frames; default DefaultMaxCallDepth
	MaxEntries      int           // total recorded list entries; default DefaultMaxEntries
	MaxStringLength int           // bytes per recorded string / string.rep result; default DefaultMaxStringLength
	MaxSourceBytes  int           // manifest source size; default DefaultMaxSourceBytes
	WallClock       time.Duration // default DefaultWallClock
}

// Default limits. They are generous for real manifests (which need a few
// hundred instructions) and tight enough that a hostile manifest fails fast.
const (
	DefaultMaxInstructions uint64        = 1_000_000
	DefaultMaxAllocBytes   uint64        = 64 << 20
	DefaultMaxCallDepth    int           = 128
	DefaultMaxEntries      int           = 10_000
	DefaultMaxStringLength int           = 4096
	DefaultMaxSourceBytes  int           = 256 << 10
	DefaultWallClock       time.Duration = 2 * time.Second
)

func (o Options) withDefaults() Options {
	if o.MaxInstructions == 0 {
		o.MaxInstructions = DefaultMaxInstructions
	}
	if o.MaxAllocBytes == 0 {
		o.MaxAllocBytes = DefaultMaxAllocBytes
	}
	if o.MaxCallDepth == 0 {
		o.MaxCallDepth = DefaultMaxCallDepth
	}
	if o.MaxEntries == 0 {
		o.MaxEntries = DefaultMaxEntries
	}
	if o.MaxStringLength == 0 {
		o.MaxStringLength = DefaultMaxStringLength
	}
	if o.MaxSourceBytes == 0 {
		o.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if o.WallClock == 0 {
		o.WallClock = DefaultWallClock
	}
	if o.ChunkName == "" {
		if o.Legacy {
			o.ChunkName = "__resource.lua"
		} else {
			o.ChunkName = "fxmanifest.lua"
		}
	}
	return o
}

// DataFile is one `data_file 'TYPE' 'path'` declaration.
type DataFile struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

// Manifest is the recorded, ordered result of evaluating one manifest.
//
// Ordering: list fields that model sets (Games, Provides, Dependencies,
// RuntimeRequirements) are
// sorted and de-duplicated; every other list preserves declaration order.
type Manifest struct {
	FxVersion               string   `json:"fxVersion,omitempty"`
	ResourceManifestVersion string   `json:"resourceManifestVersion,omitempty"`
	Games                   []string `json:"games"`
	Name                    string   `json:"name"`
	Author                  string   `json:"author,omitempty"`
	Description             string   `json:"description,omitempty"`
	Version                 string   `json:"version,omitempty"`
	Provides                []string `json:"provides"`
	Dependencies            []string `json:"dependencies"`
	// RuntimeRequirements holds `/`-prefixed dependency entries such as
	// "/server:4500", "/onesync", "/gameBuild:h4", "/policy:...", or
	// "/native:0x...". They are FXServer capability requirements, not resource
	// names: they never enter the resource dependency graph and are recorded
	// separately so staging can later check them against observed runtime.
	RuntimeRequirements []string   `json:"runtimeRequirements"`
	ServerOnly          bool       `json:"serverOnly"`
	Lua54               bool       `json:"lua54"`
	ClientScripts       []string   `json:"clientScripts"`
	ServerScripts       []string   `json:"serverScripts"`
	SharedScripts       []string   `json:"sharedScripts"`
	Files               []string   `json:"files"`
	UIPage              string     `json:"uiPage,omitempty"`
	Loadscreen          string     `json:"loadscreen,omitempty"`
	Exports             []string   `json:"exports"`
	ServerExports       []string   `json:"serverExports"`
	DataFiles           []DataFile `json:"dataFiles"`
	IsMap               bool       `json:"isMap"`
	LegacyFormat        bool       `json:"legacyFormat"`
	// Directives counts every directive call, including tolerated cosmetic
	// ones, so callers can tell an empty manifest from one that recorded
	// nothing of interest.
	Directives int `json:"directives"`
	// Ignored lists tolerated cosmetic directives that were seen but not
	// recorded, sorted and de-duplicated.
	Ignored []string `json:"ignored"`
	// Metadata holds resource-defined metadata: manifest calls that are not
	// part of the directive allowlist. FiveM records them under their name and
	// scripts read them back with GetResourceMetadata, so they are declarations
	// rather than errors. Values keep declaration order; repeated declarations
	// of one key append.
	Metadata map[string][]string `json:"metadata"`
}
