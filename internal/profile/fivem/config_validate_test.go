package fivem

import (
	"errors"
	"testing"
)

func TestValidateConfigFragmentAllowsBoundedOwnedDirectives(t *testing.T) {
	data := []byte("# generated\nsv_hostname \"Example # One\"\nset inventory_token \"quote\\\" and \\\\ slash\"\nsets tags \"roleplay\"\n")
	if err := ValidateConfigFragment(data); err != nil {
		t.Fatal(err)
	}
}

func TestValidateConfigFragmentRefusesForbiddenAndAmbiguousLines(t *testing.T) {
	cases := []string{
		"ensure chat\n",
		"exec other.cfg\n",
		"+set onesync on\n",
		"set txAdmin-menuEnabled true\n",
		"sets sv_appearAllowlisted yes\n",
		"set fxvcs_health_token \"shadow\"\n",
		"set token one two\n",
		"set token \"one\"\nset TOKEN \"two\"\n",
		"sv_hostname \"unterminated\n",
		"add_ace group.admin command allow\n",
	}
	for _, data := range cases {
		if err := ValidateConfigFragment([]byte(data)); !errors.Is(err, ErrRuntimeConfig) {
			t.Fatalf("accepted %q: %v", data, err)
		}
	}
}
