package diskspace

import (
	"path/filepath"
	"testing"
)

func TestAvailableUsesNearestExistingAncestor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "created", "yet")
	bytes, err := Available(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes <= 0 {
		t.Fatalf("available bytes = %d", bytes)
	}
}
