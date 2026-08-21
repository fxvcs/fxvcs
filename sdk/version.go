package sdk

import "fxvcs.dev/fxvcs/internal/format"

// Version is the version of the running FXVCS binary. Release builds override
// it with -ldflags "-X fxvcs.dev/fxvcs/sdk.Version=0.1.0". It must parse as a
// semantic version; New fails otherwise.
var Version = "0.0.0-dev"

// APIVersion identifies the SDK contract generation.
const APIVersion = format.SDKAPIVersion

// FormatVersions reports the persisted-format generations this binary
// understands. Consumers show these in diagnostics.
type FormatVersions struct {
	CommittedAPIVersions     []string `json:"committedAPIVersions"`
	PointerFormat            int      `json:"pointerFormat"`
	ObjectManifest           int      `json:"objectManifest"`
	AssetIndex               int      `json:"assetIndex"`
	GitIntegrationGeneration int      `json:"gitIntegrationGeneration"`
	HydrationStateSchema     int      `json:"hydrationStateSchema"`
	PublicationLedgerSchema  int      `json:"publicationLedgerSchema"`
}

// SupportedFormats returns the format generations of this binary.
func SupportedFormats() FormatVersions {
	return FormatVersions{
		CommittedAPIVersions:     append([]string(nil), format.SupportedAPIVersions...),
		PointerFormat:            format.PointerFormatVersion,
		ObjectManifest:           format.ObjectManifestVersion,
		AssetIndex:               format.AssetIndexVersion,
		GitIntegrationGeneration: format.GitIntegrationGeneration,
		HydrationStateSchema:     format.HydrationStateSchema,
		PublicationLedgerSchema:  format.PublicationLedgerSchema,
	}
}
