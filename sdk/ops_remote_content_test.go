package sdk

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"os/exec"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/app"
	"fxvcs.dev/fxvcs/internal/format"
)

const remoteContentConfig = `apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_test_01
  storageDomainID: domain_test_01
  name: test
spec:
  profile: generic
  minimumFXVCSVersion: "0.1.0"
  gitIntegrationVersion: 1
  storage:
    remotes:
      - name: primary
        type: filesystem
        requiredForPublication: true
`

// A deploy tool holds the committed configuration and pointer bytes from a Git
// archive, nothing else. It reads verified content straight from storage.
func TestOpenRemoteContentReadsPublishedContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := writeRepo(t, "0.1.0", 1)
	repo, err := app.Open(ctx, dir, app.Options{Installed: format.MustParseVersion("0.1.0"), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	if _, err := repo.StorageAdd(ctx, app.StorageAddInput{Name: "primary", Type: "filesystem", Path: store, Local: true, RequiredForPublication: true}); err != nil {
		t.Fatal(err)
	}
	content := make([]byte, 3<<20)
	src := rand.New(rand.NewPCG(1, 2))
	for i := range content {
		content[i] = byte(src.Uint32())
	}
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "stream/big.ydr")
	if err != nil {
		t.Fatal(err)
	}
	if _, failed, err := repo.ObjectsPublish(ctx, nil, nil); err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	_ = repo.Close()
	ptrBytes, err := ing.Pointer.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if !IsPointer(ptrBytes) || IsPointer(content[:100]) {
		t.Fatal("IsPointer misclassified")
	}
	ptr, err := ParsePointer(ptrBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ptr.Size != int64(len(content)) || !strings.HasPrefix(ptr.OID, "sha256:") {
		t.Fatalf("pointer = %+v", ptr)
	}

	c, _ := New(Options{InstalledVersion: "0.1.0"})
	rc, err := c.OpenRemoteContent(ctx, RemoteContentRequest{Repository: []byte(remoteContentConfig),
		Paths: map[string]string{"primary": store}})
	if err != nil {
		t.Fatal(err)
	}
	if rc.StorageDomainID != "domain_test_01" || rc.RepositoryID != "repo_test_01" || len(rc.Remotes) != 1 {
		t.Fatalf("remote content = %+v", rc)
	}
	res, err := rc.Read(ctx, ptr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Content)
	_ = res.Content.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) || res.Size != int64(len(content)) || res.Digest != ptr.OID {
		t.Fatal("content differs")
	}

	// An object nobody published is not-found, not content.
	missing := ptr
	missing.OID = "sha256:" + strings.Repeat("0", 64)
	missing.Encoding = "whole/1"
	if _, err := rc.Read(ctx, missing); CodeOf(err) != CodeNotFound {
		t.Fatalf("missing: got %v (%s), want not-found", err, CodeOf(err))
	}
}

func TestOpenRemoteContentRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	c, _ := New(Options{InstalledVersion: "0.1.0"})
	if _, err := c.OpenRemoteContent(ctx, RemoteContentRequest{}); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("empty config: %v", err)
	}
	newer := strings.Replace(remoteContentConfig, `"0.1.0"`, `"9.0.0"`, 1)
	if _, err := c.OpenRemoteContent(ctx, RemoteContentRequest{Repository: []byte(newer)}); CodeOf(err) != CodeUpgradeRequired {
		t.Fatalf("newer minimum: %v (%s)", err, CodeOf(err))
	}
	if _, err := ParsePointer([]byte("hello")); CodeOf(err) != CodeInvalidArgument {
		t.Fatalf("not a pointer: %v", err)
	}
}
