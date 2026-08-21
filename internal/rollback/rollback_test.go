package rollback

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func d(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }

func TestBuildUsesPriorReleaseAndCurrentAuthorization(t *testing.T) {
	previous := domain.Deployment{RealizationID: "rz_previous", Inputs: domain.RealizationInputs{
		ReleaseName: "v1", ReleaseDigest: d("1"), SourceSetDigest: d("2"),
		EnvironmentName: "old", EnvironmentRevision: 1, TargetContentDigest: d("3"),
		HookDigests: map[string]string{"old": d("4")}, SecretVersions: map[string]string{"x/y": "v:old"},
	}}
	failed := domain.Deployment{RealizationID: "rz_failed", Inputs: domain.RealizationInputs{
		ReleaseName: "v2", ReleaseDigest: d("5"), SourceSetDigest: d("6"),
		EnvironmentName: "prd", EnvironmentRevision: 9, TargetContentDigest: d("7"),
		HookDigests:      map[string]string{"current": d("8")},
		VerifierBindings: map[string]string{"resource-present": d("9")},
		SecretVersions:   map[string]string{"x/y": "hmac-sha256:" + strings.Repeat("a", 64)},
	}}
	plan, err := Build(previous, failed)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Inputs.ReleaseName != "v1" || plan.Inputs.EnvironmentName != "prd" || plan.Inputs.EnvironmentRevision != 9 {
		t.Fatalf("wrong source fields: %#v", plan.Inputs)
	}
	if plan.Inputs.HookDigests["current"] != d("8") || plan.Inputs.SecretVersions["x/y"] != failed.Inputs.SecretVersions["x/y"] {
		t.Fatalf("current authorization not used: %#v", plan.Inputs)
	}
	if plan.RealizationID == "" || plan.RealizationID == previous.RealizationID {
		t.Fatalf("unexpected rollback ID %q", plan.RealizationID)
	}
	failed.Inputs.HookDigests["current"] = d("a")
	if plan.Inputs.HookDigests["current"] != d("8") {
		t.Fatal("plan aliases caller maps")
	}
}

func TestBuildRejectsSameOrIncompleteRealization(t *testing.T) {
	dep := domain.Deployment{RealizationID: "rz_same"}
	if _, err := Build(dep, dep); err == nil {
		t.Fatal("same realization accepted")
	}
}
