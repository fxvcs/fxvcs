package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

// s3Server is a minimal S3-compatible provider over HTTP: enough of the
// protocol for the real AWS SDK client to sign against and for the engine to
// publish, list, and fetch through. It exists so the Stage 2 path is exercised
// end to end — signing, path-style addressing, key layout, transfer — rather
// than only against an in-process fake of the SDK.
type s3Server struct {
	mu      sync.Mutex
	objects map[string]s3Object
	// failFrom makes every request fail once it is set, modelling a remote
	// that goes away mid-session.
	failFrom bool
	requests int
}

func newS3Server(t *testing.T) (*s3Server, string) {
	t.Helper()
	srv := &s3Server{objects: map[string]s3Object{}}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts.URL
}

func (s *s3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	fail := s.failFrom
	s.mu.Unlock()
	if fail {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	// Path style: /<bucket>/<key…>
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	if bucket == "" {
		http.Error(w, "no bucket", http.StatusBadRequest)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case r.Method == http.MethodHead:
		s.head(w, key)
	case r.Method == http.MethodGet:
		s.get(w, r, key)
	case r.Method == http.MethodPut:
		s.put(w, r, key)
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}
}

// s3Object is a stored body plus its user metadata, which real S3 returns on
// GET and HEAD and which the storage codec depends on.
type s3Object struct {
	data []byte
	meta map[string]string
}

func (s *s3Server) head(w http.ResponseWriter, key string) {
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeMeta(w, obj.meta)
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.WriteHeader(http.StatusOK)
}

func writeMeta(w http.ResponseWriter, meta map[string]string) {
	for k, v := range meta {
		w.Header().Set("x-amz-meta-"+k, v)
	}
}

func (s *s3Server) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	data := obj.data
	if !ok {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`)
		return
	}
	writeMeta(w, obj.meta)
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		var start int
		if _, err := fmtSscan(strings.TrimPrefix(rng, "bytes="), &start); err == nil && start <= len(data) {
			data = data[start:]
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data)
			return
		}
	}
	_, _ = w.Write(data)
}

func (s *s3Server) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta := map[string]string{}
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			meta[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
		}
	}
	s.mu.Lock()
	s.objects[key] = s3Object{data: data, meta: meta}
	s.mu.Unlock()
	sum := sha256.Sum256(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	w.WriteHeader(http.StatusOK)
}

type listResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	Contents    []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
}

func (s *s3Server) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var res listResult
	s.mu.Lock()
	for k, v := range s.objects {
		if strings.HasPrefix(k, prefix) {
			res.Contents = append(res.Contents, struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			}{Key: k, Size: int64(len(v.data))})
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			break
		}
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(s[i]-'0')
	}
	*out = n
	return 1, nil
}

// TestS3EndToEndPublishAndHydrate drives the real S3 client through the whole
// Stage 2 path: a chunked asset is ingested, published to an S3-compatible
// bucket, and rebuilt byte for byte on a machine whose cache is empty.
func TestS3EndToEndPublishAndHydrate(t *testing.T) {
	srv, endpoint := newS3Server(t)
	r, cacheDir := testRepo(t)
	configDir := t.TempDir()
	repo := openTestWithConfig(t, r, cacheDir, configDir)
	ctx := context.Background()

	if _, err := repo.StorageAdd(ctx, StorageAddInput{
		Name: "cdn", Type: "s3", Bucket: "assets", Endpoint: endpoint,
		Prefix: "team", Region: "auto", Addressing: "path", RequiredForPublication: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StorageLogin(ctx, StorageLoginInput{Remote: "cdn", AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}); err != nil {
		t.Fatal(err)
	}

	content := asset(11, 5<<20)
	ing, err := repo.Ingest(ctx, bytes.NewReader(content), "assets/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(ing.Objects) < 3 {
		t.Fatalf("expected a chunked asset, got %d objects", len(ing.Objects))
	}
	published, failed, err := repo.ObjectsPublish(ctx, []string{"cdn"}, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("publish: %v %v", failed, err)
	}
	if len(published) != len(ing.Objects) {
		t.Fatalf("published %d of %d objects", len(published), len(ing.Objects))
	}
	// Keys are repository-scoped and carry the prefix.
	srv.mu.Lock()
	for key := range srv.objects {
		if !strings.HasPrefix(key, "team/"+repo.Domain+"/objects/") {
			srv.mu.Unlock()
			t.Fatalf("unexpected key layout: %s", key)
		}
	}
	stored := len(srv.objects)
	srv.mu.Unlock()
	if stored != len(ing.Objects) {
		t.Fatalf("bucket holds %d objects, published %d", stored, len(ing.Objects))
	}

	// Publishing again transfers nothing: the ledger already knows, and the
	// batch existence check confirms without uploading.
	published, failed, err = repo.ObjectsPublish(ctx, []string{"cdn"}, nil)
	if err != nil || len(failed) != 0 || len(published) != 0 {
		t.Fatalf("second publish moved data: %v %v %v", published, failed, err)
	}

	// A second machine: empty cache, same bucket, keys from the environment.
	t.Setenv("FXVCS_S3_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("FXVCS_S3_SECRET_ACCESS_KEY", "secret")
	fresh := openTestWithConfig(t, r, t.TempDir(), t.TempDir())
	if fresh.HasContent(ctx, ing.Pointer) {
		t.Fatal("a fresh cache should hold nothing")
	}
	if _, err := fresh.EnsureContent(ctx, ing.Pointer, nil); err != nil {
		t.Fatal(err)
	}
	rc, err := fresh.OpenContent(ctx, ing.Pointer)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("fetched asset differs (%d vs %d bytes)", len(got), len(content))
	}

	// Verification against the remote passes...
	if _, issues, err := repo.Verify(ctx, Selection{}, VerifyOptions{Remote: true}); err != nil || len(issues) != 0 {
		t.Fatalf("verify --remote: %v %v", issues, err)
	}
	// ...and an unreachable remote is reported as unavailable, not as a
	// missing object: "publish it" and "the remote is down" are different
	// problems with different remedies.
	srv.mu.Lock()
	srv.failFrom = true
	srv.mu.Unlock()
	empty := openTestWithConfig(t, r, t.TempDir(), t.TempDir())
	// One attempt, so the assertions are about the outcome rather than about
	// how long the SDK's backoff takes.
	for i := range empty.Local.Spec.Remotes {
		empty.Local.Spec.Remotes[i].MaxAttempts = 1
	}
	store, _, err := empty.Store("cdn")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Has(ctx, empty.Domain, ing.Digest); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable from a remote that is down", err)
	}
	if _, err := empty.EnsureContent(ctx, ing.Pointer, nil); err == nil {
		t.Fatal("fetching from a remote that is down must fail, not silently produce nothing")
	}
}
