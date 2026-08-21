// Package rollback constructs the safe realization input tuple for automatic
// rollback. It never blindly reactivates retained rendered files: only the
// previous immutable Release identity is reused, while all mutable authorized
// configuration comes from the current desired tuple.
package rollback

import (
	"errors"
	"fmt"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/integrity"
)

var ErrUnsafeInput = errors.New("rollback: unsafe input")

type Plan struct {
	Inputs        domain.RealizationInputs `json:"inputs"`
	RealizationID string                   `json:"realizationID"`
	PriorRelease  string                   `json:"priorRelease"`
	FailedID      string                   `json:"failedRealizationID"`
}

// Build creates a rollback tuple from the previous LKG's immutable Release
// fields and the failed desired tuple's currently authorized environment,
// target document, hooks, verifier bindings, and secret versions.
func Build(previousLKG, failedDesired domain.Deployment) (Plan, error) {
	if previousLKG.RealizationID == "" || failedDesired.RealizationID == "" || previousLKG.RealizationID == failedDesired.RealizationID {
		return Plan{}, fmt.Errorf("%w: distinct previous and failed realizations required", ErrUnsafeInput)
	}
	if previousLKG.Inputs.ReleaseName == "" || previousLKG.Inputs.ReleaseDigest == "" || previousLKG.Inputs.SourceSetDigest == "" {
		return Plan{}, fmt.Errorf("%w: previous realization lacks immutable release identity", ErrUnsafeInput)
	}
	current := failedDesired.Inputs
	inputs := domain.RealizationInputs{
		ReleaseName:         previousLKG.Inputs.ReleaseName,
		ReleaseDigest:       previousLKG.Inputs.ReleaseDigest,
		SourceSetDigest:     previousLKG.Inputs.SourceSetDigest,
		EnvironmentName:     current.EnvironmentName,
		EnvironmentRevision: current.EnvironmentRevision,
		TargetContentDigest: current.TargetContentDigest,
		HookDigests:         clone(current.HookDigests),
		VerifierBindings:    clone(current.VerifierBindings),
		SecretVersions:      clone(current.SecretVersions),
	}
	id, err := integrity.RealizationID(inputs)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Inputs: inputs, RealizationID: id, PriorRelease: inputs.ReleaseName, FailedID: failedDesired.RealizationID}, nil
}

func clone(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
