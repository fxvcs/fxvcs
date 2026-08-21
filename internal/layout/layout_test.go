package layout

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/pathsafe"
)

func abs(p string) string {
	if runtime.GOOS == "windows" {
		return filepath.Clean(`C:\` + filepath.FromSlash(p))
	}
	return filepath.Clean("/" + p)
}

func TestRootsValidate(t *testing.T) {
	ok := Roots{DeployRoot: abs("srv/fxvcs/prd"), ServerData: abs("srv/server-data"), TxData: abs("srv/txData")}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Roots{
		{DeployRoot: "relative", ServerData: abs("x")},
		{DeployRoot: abs("a"), ServerData: abs("a/inside")},
		{DeployRoot: abs("a"), ServerData: abs("b"), TxData: abs("a/txData")},
		{DeployRoot: abs("a") + string(filepath.Separator) + "..", ServerData: abs("b")},
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if err := DistinctDeployRoots(abs("a"), abs("b"), abs("a/nested")); !errors.Is(err, ErrOverlap) {
		t.Fatalf("nested roots accepted: %v", err)
	}
	if err := DistinctDeployRoots(abs("a"), abs("a")); !errors.Is(err, ErrOverlap) {
		t.Fatalf("duplicate roots accepted: %v", err)
	}
	if err := DistinctDeployRoots(abs("a"), abs("a2")); err != nil {
		t.Fatal(err)
	}
}

func TestRealizationPathsAndContainment(t *testing.T) {
	root := abs("srv/fxvcs/prd")
	rz, err := Realization(root, "rz_01ABC")
	if err != nil {
		t.Fatal(err)
	}
	if !pathsafe.Within(root, rz) {
		t.Fatal("realization outside root")
	}
	for _, id := range []string{"", "rz_../x", "release-1", "rz_a/b", "rz_" + strings.Repeat("x", 70)} {
		if _, err := Realization(root, id); !errors.Is(err, ErrInvalidRealizationID) {
			t.Errorf("id %q accepted", id)
		}
	}
	mp, err := MaterializedPath(root, "sha256:"+strings.Repeat("ab", 32))
	if err != nil || !pathsafe.Within(filepath.Join(root, MaterializedDir), mp) {
		t.Fatalf("materialized: %v %s", err, mp)
	}
	if _, err := MaterializedPath(root, "sha256:../../etc"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatal("bad digest accepted")
	}

	// Component files stay inside the realization regardless of hostile paths.
	good, err := ResourceFile(rz, "resources/[maps]", "city-map/stream/x.ytd")
	if err != nil || !pathsafe.Within(rz, good) {
		t.Fatalf("resource file: %v", err)
	}
	for _, c := range [][2]string{
		{"resources/[maps]", "../../../etc/passwd"},
		{"resources/[maps]", "/abs"},
		{"../resources", "x"},
		{"runtime", "x"},
		{"resources/[maps]/../../..", "x"},
		{"resources", "x"},
	} {
		if _, err := ResourceFile(rz, c[0], c[1]); err == nil {
			t.Errorf("mount %q rel %q escaped", c[0], c[1])
		}
	}
	if _, err := RuntimeFile(rz, "config/server-values.cfg"); err != nil {
		t.Fatal(err)
	}
	if _, err := RuntimeFile(rz, "../resources/x"); err == nil {
		t.Fatal("runtime escape accepted")
	}
}

func TestHookDestination(t *testing.T) {
	rz := abs("srv/fxvcs/prd/releases/rz_1")
	if _, err := HookDestination(rz, "runtime/config", "runtime/config/server-values.cfg"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"resources/[core]/secret.cfg", "runtime/fxvcs.cfg", "../rz_0/runtime/config/x", "runtime/config/../../x", "/etc/x"} {
		if _, err := HookDestination(rz, "runtime/config", d); err == nil {
			t.Errorf("destination %q accepted", d)
		}
	}
}

func TestPersistentPath(t *testing.T) {
	root := abs("srv/fxvcs/prd")
	p, err := PersistentPath(root, "repo_inv_01", "inventory", "data/db.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if !pathsafe.Within(filepath.Join(root, SharedDir, SharedPersist), p) {
		t.Fatal("persistent path outside shared/persistent")
	}
	for _, c := range [][3]string{
		{"repo_inv_01", "inventory", "../../escape"},
		{"../x", "inventory", "data"},
		{"repo_inv_01", "../inventory", "data"},
		{"repo_inv_01", "inventory", "/abs"},
	} {
		if _, err := PersistentPath(root, c[0], c[1], c[2]); err == nil {
			t.Errorf("%v escaped", c)
		}
	}
}

func TestServerDataLinks(t *testing.T) {
	r := Roots{DeployRoot: abs("srv/fxvcs/prd"), ServerData: abs("srv/server-data")}
	links := ServerDataLinks(r)
	for name, target := range links {
		if !pathsafe.Within(r.DeployRoot, target) {
			t.Errorf("%s -> %s outside deployRoot", name, target)
		}
	}
	if _, ok := links["fxvcs.cfg"]; !ok {
		t.Fatal("missing fxvcs.cfg link")
	}
}
