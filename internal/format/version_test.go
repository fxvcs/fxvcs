package format

import (
	"errors"
	"testing"
)

func TestParseVersionAndCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0}, {"0.1.0", "0.2.0", -1}, {"1.0.0", "0.9.9", 1},
		{"1.0.0-rc.1", "1.0.0", -1}, {"1.0.0-rc.1", "1.0.0-rc.2", -1}, {"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-1", "1.0.0-a", -1}, {"v1.2.3+build", "1.2.3", 0},
	}
	for _, c := range cases {
		a, b := MustParseVersion(c.a), MustParseVersion(c.b)
		if got := a.Compare(b); got != c.want {
			t.Errorf("%s vs %s: got %d want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "01.2.3", "1.2.3-", "x.y.z"} {
		if _, err := ParseVersion(bad); !errors.Is(err, ErrInvalidSemver) {
			t.Errorf("%q: expected ErrInvalidSemver, got %v", bad, err)
		}
	}
}

func TestParseAPIVersion(t *testing.T) {
	if _, _, err := ParseAPIVersion("fxvcs.dev/v1alpha1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   string
		want error
	}{
		{"fxvcs.dev/v1alpha2", ErrNewerAPIVersion},
		{"fxvcs.dev/v1beta1", ErrNewerAPIVersion},
		{"fxvcs.dev/v1", ErrNewerAPIVersion},
		{"fxvcs.dev/v2alpha1", ErrNewerAPIVersion},
		{"fxvcs.dev/v0alpha1", ErrUnknownAPIVersion},
		{"example.com/v1alpha1", ErrUnknownAPIVersion},
		{"v1alpha1", ErrUnknownAPIVersion},
		{"fxvcs.dev/", ErrUnknownAPIVersion},
		{"fxvcs.dev/banana", ErrUnknownAPIVersion},
	} {
		if _, _, err := ParseAPIVersion(c.in); !errors.Is(err, c.want) {
			t.Errorf("%q: got %v want %v", c.in, err, c.want)
		}
	}
}

func TestValidateEnvelope(t *testing.T) {
	if err := ValidateEnvelope(Envelope{CurrentAPIVersion, "Repository"}, "Repository"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvelope(Envelope{CurrentAPIVersion, "Repository"}, "Release"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("got %v", err)
	}
	if err := ValidateEnvelope(Envelope{CurrentAPIVersion, "Nope"}, ""); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("got %v", err)
	}
}

func TestCheckMinimumVersion(t *testing.T) {
	installed := MustParseVersion("0.1.0")
	if err := CheckMinimumVersion(installed, "", "r"); err != nil {
		t.Fatal(err)
	}
	if err := CheckMinimumVersion(installed, "0.1.0", "r"); err != nil {
		t.Fatal(err)
	}
	err := CheckMinimumVersion(installed, "0.2.0", "/repo")
	var ur *UpgradeRequiredError
	if !errors.As(err, &ur) {
		t.Fatalf("expected UpgradeRequiredError, got %v", err)
	}
	if ur.Required.String() != "0.2.0" || ur.Installed.String() != "0.1.0" || ur.Repository != "/repo" {
		t.Fatalf("bad error %+v", ur)
	}
	if err := CheckGitIntegrationGeneration(2, "/repo", installed); !errors.As(err, &ur) {
		t.Fatalf("expected UpgradeRequiredError, got %v", err)
	}
	if err := CheckGitIntegrationGeneration(1, "/repo", installed); err != nil {
		t.Fatal(err)
	}
	if err := CheckGitIntegrationGeneration(0, "/repo", installed); !errors.Is(err, ErrUnsupportedGen) {
		t.Fatal(err)
	}
}
