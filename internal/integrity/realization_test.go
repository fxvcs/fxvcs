package integrity

import (
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

func TestRealizationIdentity(t *testing.T) {
	in := domain.RealizationInputs{
		ReleaseName: "v1.4.0", ReleaseDigest: d("1"), SourceSetDigest: d("2"), EnvironmentName: "prd", EnvironmentRevision: 3,
		TargetContentDigest: d("3"),
		HookDigests:         map[string]string{"render-server-values": d("4")},
		VerifierBindings:    map[string]string{"resource-present": d("5")},
		SecretVersions:      map[string]string{"fivem/rcon-password": "hmac-sha256:" + strings.Repeat("0", 64), "discord/token": "v:sv_01"},
	}
	a, err := RealizationIdentity(in)
	if err != nil {
		t.Fatal(err)
	}
	in2 := in
	in2.SecretVersions = map[string]string{"discord/token": "v:sv_01", "fivem/rcon-password": "hmac-sha256:" + strings.Repeat("0", 64)}
	if b, _ := RealizationIdentity(in2); b != a {
		t.Fatal("map order changed identity")
	}
	in3 := in
	in3.SecretVersions = map[string]string{"fivem/rcon-password": "hmac-sha256:" + strings.Repeat("1", 64)}
	if c, _ := RealizationIdentity(in3); c == a {
		t.Fatal("secret rotation did not change identity")
	}
	bad := in
	bad.SecretVersions = map[string]string{"fivem/rcon-password": "hunter2"}
	if _, err := RealizationIdentity(bad); err == nil {
		t.Fatal("plaintext-looking secret version accepted")
	}
	bad = in
	bad.SecretVersions = map[string]string{"x/y": "sha256:" + strings.Repeat("a", 64)}
	if _, err := RealizationIdentity(bad); err == nil {
		t.Fatal("unkeyed hash accepted as secret version")
	}
	id, err := RealizationID(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "rz_") || len(id) != 55 {
		t.Fatalf("unexpected realization ID %q", id)
	}
	if again, _ := RealizationID(in2); again != id {
		t.Fatal("map order changed realization ID")
	}
}

func TestVerifierBindingDigestExcludesEndpoint(t *testing.T) {
	a, err := VerifierBindingDigest("resource-present", domain.VerifierBinding{Implementation: "rcon-resource-state", ImplementationVersion: 1, EndpointRef: "local-rcon"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := VerifierBindingDigest("resource-present", domain.VerifierBinding{Implementation: "rcon-resource-state", ImplementationVersion: 1, EndpointRef: "other-endpoint"})
	if a != b {
		t.Fatal("endpoint leaked into binding digest")
	}
	c, _ := VerifierBindingDigest("resource-present", domain.VerifierBinding{Implementation: "rcon-resource-state", ImplementationVersion: 2})
	if c == a {
		t.Fatal("implementation version ignored")
	}
	if _, err := VerifierBindingDigest("resource-present", domain.VerifierBinding{}); err == nil {
		t.Fatal("empty binding accepted")
	}
}
