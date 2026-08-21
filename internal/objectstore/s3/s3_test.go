package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

const testDomain = "domain_test_01"

// fakeS3 is an in-process S3-compatible provider: enough of the API to
// exercise the store's own behaviour (keys, integrity, multipart, ranges,
// metadata) without a network or a container. Faults are injected explicitly
// so failure paths are tested as deliberately as success ones.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	uploads map[string]map[int32][]byte
	nextID  int

	// injected behaviour
	failGetAfter  int   // bytes to deliver before breaking the body (0 = never)
	getFailures   int   // how many times to break before succeeding
	failPut       error // returned by PutObject
	failParts     map[int32]error
	countHead     int
	countList     int
	countGet      int
	dropMetadata  bool
	ignoreRange   bool
	failMultipart error
}

type fakeObject struct {
	data []byte
	meta map[string]string
}

func newFake() *fakeS3 {
	return &fakeS3{objects: map[string]fakeObject{}, uploads: map[string]map[int32][]byte{}, failParts: map[int32]error{}}
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countHead++
	obj, ok := f.objects[*in.Key]
	if !ok {
		return nil, &types.NotFound{}
	}
	meta := obj.meta
	if f.dropMetadata {
		meta = nil
	}
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(obj.data))), Metadata: meta}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	obj, ok := f.objects[*in.Key]
	if !ok {
		f.mu.Unlock()
		return nil, &types.NoSuchKey{}
	}
	f.countGet++
	data := obj.data
	offset := 0
	if in.Range != nil && !f.ignoreRange {
		var start int
		if _, err := fmt.Sscanf(*in.Range, "bytes=%d-", &start); err == nil && start <= len(data) {
			offset = start
			data = data[start:]
		}
	}
	breakAfter := 0
	if f.failGetAfter > 0 && f.getFailures > 0 {
		f.getFailures--
		breakAfter = f.failGetAfter
	}
	meta := obj.meta
	if f.dropMetadata {
		meta = nil
	}
	f.mu.Unlock()
	body := io.ReadCloser(io.NopCloser(bytes.NewReader(data)))
	if breakAfter > 0 {
		body = &brokenBody{r: bytes.NewReader(data), limit: breakAfter}
	}
	_ = offset
	return &s3.GetObjectOutput{Body: body, ContentLength: aws.Int64(int64(len(data))), Metadata: meta}, nil
}

// brokenBody delivers limit bytes and then fails, like a connection reset
// mid-download.
type brokenBody struct {
	r     *bytes.Reader
	limit int
	n     int
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.n >= b.limit {
		return 0, errors.New("connection reset by peer")
	}
	if len(p) > b.limit-b.n {
		p = p[:b.limit-b.n]
	}
	n, err := b.r.Read(p)
	b.n += n
	return n, err
}

func (b *brokenBody) Close() error { return nil }

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return nil, f.failPut
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.objects[*in.Key] = fakeObject{data: data, meta: in.Metadata}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, *in.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.countList++
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	for k := range f.objects {
		if in.Prefix == nil || strings.HasPrefix(k, *in.Prefix) {
			key := k
			out.Contents = append(out.Contents, types.Object{Key: &key})
		}
	}
	return out, nil
}

func (f *fakeS3) CreateMultipartUpload(_ context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMultipart != nil {
		return nil, f.failMultipart
	}
	f.nextID++
	id := "upload-" + strconv.Itoa(f.nextID)
	f.uploads[id] = map[int32][]byte{}
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(id), Key: in.Key}, nil
}

func (f *fakeS3) UploadPart(_ context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if perr := f.failParts[*in.PartNumber]; perr != nil {
		return nil, perr
	}
	parts, ok := f.uploads[*in.UploadId]
	if !ok {
		return nil, errors.New("no such upload")
	}
	parts[*in.PartNumber] = data
	etag := fmt.Sprintf("\"part-%d\"", *in.PartNumber)
	return &s3.UploadPartOutput{ETag: &etag}, nil
}

func (f *fakeS3) CompleteMultipartUpload(_ context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts, ok := f.uploads[*in.UploadId]
	if !ok {
		return nil, errors.New("no such upload")
	}
	var joined []byte
	for _, p := range in.MultipartUpload.Parts {
		joined = append(joined, parts[*p.PartNumber]...)
	}
	f.objects[*in.Key] = fakeObject{data: joined}
	delete(f.uploads, *in.UploadId)
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, *in.UploadId)
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func newTestStore(t *testing.T, cfg Config) (*Store, *fakeS3) {
	t.Helper()
	fake := newFake()
	if cfg.Bucket == "" {
		cfg.Bucket = "assets"
	}
	return newWithAPI(fake, cfg), fake
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestKeysAreRepositoryScoped(t *testing.T) {
	s, _ := newTestStore(t, Config{Prefix: "repos"})
	d := digestOf([]byte("x"))
	key, err := s.Key(testDomain, d)
	if err != nil {
		t.Fatal(err)
	}
	h := strings.TrimPrefix(d, "sha256:")
	want := "repos/" + testDomain + "/objects/" + h[:2] + "/" + h[2:4] + "/" + h
	if key != want {
		t.Fatalf("key %q, want %q", key, want)
	}
	// A different domain never shares a prefix with this one.
	other, err := s.Key("domain_other", d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(other, "repos/"+testDomain+"/") {
		t.Fatalf("domains share a key namespace: %q and %q", key, other)
	}
	if _, err := s.Key("not a domain", d); !errors.Is(err, objectstore.ErrInvalidDomain) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Key(testDomain, "sha1:abc"); !errors.Is(err, objectstore.ErrInvalidDigest) {
		t.Fatalf("got %v", err)
	}
}

func TestPutGetRoundTripAndImmutability(t *testing.T) {
	s, fake := newTestStore(t, Config{})
	ctx := context.Background()
	payload := bytes.Repeat([]byte("fxvcs"), 5000)
	d := digestOf(payload)

	if ok, err := s.Has(ctx, testDomain, d); err != nil || ok {
		t.Fatalf("Has before Put: %v %v", ok, err)
	}
	if err := s.Put(ctx, testDomain, d, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Has(ctx, testDomain, d); err != nil || !ok {
		t.Fatalf("Has after Put: %v %v", ok, err)
	}
	rc, err := s.Get(ctx, testDomain, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("read back %d bytes (%v)", len(got), err)
	}
	// An object that already exists is not re-uploaded: the reader is not
	// even drained.
	before := len(fake.objects)
	unread := &countingReader{r: bytes.NewReader(payload)}
	if err := s.Put(ctx, testDomain, d, int64(len(payload)), unread); err != nil {
		t.Fatal(err)
	}
	if unread.n != 0 {
		t.Fatalf("re-Put transferred %d bytes for an object the remote already has", unread.n)
	}
	if len(fake.objects) != before {
		t.Fatal("re-Put created another object")
	}
	if _, err := s.Get(ctx, testDomain, digestOf([]byte("absent"))); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestPutRejectsContentThatDoesNotMatchTheDigest(t *testing.T) {
	s, fake := newTestStore(t, Config{})
	ctx := context.Background()
	payload := []byte("the real content")
	claimed := digestOf([]byte("something else"))
	err := s.Put(ctx, testDomain, claimed, int64(len(payload)), bytes.NewReader(payload))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("got %v, want a digest mismatch", err)
	}
	if len(fake.objects) != 0 {
		t.Fatal("a mismatching object was stored anyway")
	}
	// A declared size that disagrees with the stream is also refused.
	good := digestOf(payload)
	if err := s.Put(ctx, testDomain, good, int64(len(payload))+1, bytes.NewReader(payload)); err == nil {
		t.Fatal("want a size mismatch error")
	}
	if len(fake.objects) != 0 {
		t.Fatal("a mis-sized object was stored anyway")
	}
}

func TestMultipartUploadForLargeObjects(t *testing.T) {
	s, fake := newTestStore(t, Config{PartSize: MinPartSize, Concurrency: 3})
	ctx := context.Background()
	// Three and a bit parts.
	payload := bytes.Repeat([]byte("abcdefgh"), (MinPartSize*3+1000)/8)
	d := digestOf(payload)
	if err := s.Put(ctx, testDomain, d, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	key, _ := s.Key(testDomain, d)
	if !fake.has(key) {
		t.Fatal("multipart upload did not complete")
	}
	rc, err := s.Get(ctx, testDomain, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("multipart round trip differs (%d vs %d bytes, %v)", len(got), len(payload), err)
	}
}

func TestMultipartAbortsOnPartFailureAndDigestMismatch(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("z"), MinPartSize*2+16)

	t.Run("part failure", func(t *testing.T) {
		s, fake := newTestStore(t, Config{PartSize: MinPartSize})
		fake.failParts[2] = errors.New("503 slow down")
		err := s.Put(ctx, testDomain, digestOf(payload), int64(len(payload)), bytes.NewReader(payload))
		if err == nil {
			t.Fatal("want an error when a part fails")
		}
		if len(fake.objects) != 0 || len(fake.uploads) != 0 {
			t.Fatalf("failed multipart left state behind: %d objects, %d uploads", len(fake.objects), len(fake.uploads))
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		s, fake := newTestStore(t, Config{PartSize: MinPartSize})
		err := s.Put(ctx, testDomain, digestOf([]byte("other")), int64(len(payload)), bytes.NewReader(payload))
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("got %v, want a digest mismatch", err)
		}
		if len(fake.objects) != 0 || len(fake.uploads) != 0 {
			t.Fatal("a multipart object with the wrong content became visible")
		}
	})
}

func TestGetResumesAnInterruptedBody(t *testing.T) {
	s, fake := newTestStore(t, Config{MaxAttempts: 4})
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789"), 4096)
	d := digestOf(payload)
	if err := s.Put(ctx, testDomain, d, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	// Break the body twice, 1000 bytes in each time.
	fake.failGetAfter, fake.getFailures = 1000, 2
	rc, err := s.Get(ctx, testDomain, d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("resumed read failed: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed download produced %d bytes, want %d", len(got), len(payload))
	}
	if fake.countGet < 3 {
		t.Fatalf("expected the download to be resumed, only %d GETs were made", fake.countGet)
	}
}

func TestGetGivesUpAfterMaxAttempts(t *testing.T) {
	s, fake := newTestStore(t, Config{MaxAttempts: 2})
	ctx := context.Background()
	payload := bytes.Repeat([]byte("x"), 10000)
	d := digestOf(payload)
	if err := s.Put(ctx, testDomain, d, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	fake.failGetAfter, fake.getFailures = 100, 50
	rc, err := s.Get(ctx, testDomain, d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if err == nil {
		t.Fatal("a permanently broken body must surface an error, never a short read reported as success")
	}
}

func TestBatchHasAnswersEveryDigest(t *testing.T) {
	s, _ := newTestStore(t, Config{})
	ctx := context.Background()
	var digests []string
	present := map[string]bool{}
	for i := 0; i < 60; i++ {
		body := []byte("object-" + strconv.Itoa(i))
		d := digestOf(body)
		digests = append(digests, d)
		if i%2 == 0 {
			if err := s.Put(ctx, testDomain, d, int64(len(body)), bytes.NewReader(body)); err != nil {
				t.Fatal(err)
			}
			present[d] = true
		}
	}
	have, err := s.BatchHas(ctx, testDomain, digests)
	if err != nil {
		t.Fatal(err)
	}
	if len(have) != len(digests) {
		t.Fatalf("BatchHas answered %d of %d digests", len(have), len(digests))
	}
	for _, d := range digests {
		if have[d] != present[d] {
			t.Fatalf("BatchHas said %v for %s, want %v", have[d], d, present[d])
		}
	}
	if _, err := s.BatchHas(ctx, testDomain, []string{"not-a-digest"}); !errors.Is(err, objectstore.ErrInvalidDigest) {
		t.Fatalf("got %v", err)
	}
}

// TestBatchHasListsCrowdedFanouts is the request-count half of the contract:
// once enough digests land in the same two-hex fan-out — what a very large
// chunked asset looks like — they cost one list instead of one HEAD each.
func TestBatchHasListsCrowdedFanouts(t *testing.T) {
	s, fake := newTestStore(t, Config{})
	ctx := context.Background()
	crowd := digestsWithPrefix(t, "ab", 8)
	for i, d := range crowd {
		if i%2 == 0 {
			continue // leave half absent
		}
		fake.mu.Lock()
		key, _ := s.Key(testDomain, d)
		fake.objects[key] = fakeObject{data: []byte("x")}
		fake.mu.Unlock()
	}
	fake.mu.Lock()
	fake.countHead, fake.countList = 0, 0
	fake.mu.Unlock()

	have, err := s.BatchHas(ctx, testDomain, crowd)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range crowd {
		if want := i%2 == 1; have[d] != want {
			t.Fatalf("BatchHas said %v for %s, want %v", have[d], d, want)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.countList != 1 || fake.countHead != 0 {
		t.Fatalf("%d digests in one fan-out cost %d lists and %d heads; want exactly one list", len(crowd), fake.countList, fake.countHead)
	}
}

// digestsWithPrefix finds n real sha256 digests whose hex begins with prefix,
// so a test can build the crowded fan-out a huge asset would produce.
func digestsWithPrefix(t *testing.T, prefix string, n int) []string {
	t.Helper()
	var out []string
	for i := 0; len(out) < n; i++ {
		if i > 1_000_000 {
			t.Fatalf("could not find %d digests starting with %q", n, prefix)
		}
		d := digestOf([]byte("seed-" + strconv.Itoa(i)))
		if strings.HasPrefix(strings.TrimPrefix(d, "sha256:"), prefix) {
			out = append(out, d)
		}
	}
	return out
}

func TestCompressionIsTransparentAndOptional(t *testing.T) {
	ctx := context.Background()
	compressible := bytes.Repeat([]byte("a very repetitive payload "), 4000)
	d := digestOf(compressible)

	t.Run("compressed when smaller", func(t *testing.T) {
		s, fake := newTestStore(t, Config{Compress: true})
		if err := s.Put(ctx, testDomain, d, int64(len(compressible)), bytes.NewReader(compressible)); err != nil {
			t.Fatal(err)
		}
		key, _ := s.Key(testDomain, d)
		fake.mu.Lock()
		stored := fake.objects[key]
		fake.mu.Unlock()
		if len(stored.data) >= len(compressible) {
			t.Fatalf("compressible payload stored as %d bytes, raw is %d", len(stored.data), len(compressible))
		}
		if codecOf(stored.meta) != codecZstd {
			t.Fatalf("stored object does not record its codec: %v", stored.meta)
		}
		// The digest is over raw bytes, so the key is unchanged by
		// compression and the read is transparent.
		rc, err := s.Get(ctx, testDomain, d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || !bytes.Equal(got, compressible) {
			t.Fatalf("compressed round trip differs (%d bytes, %v)", len(got), err)
		}
	})

	t.Run("stored raw when compression would not help", func(t *testing.T) {
		s, fake := newTestStore(t, Config{Compress: true})
		// Random-ish bytes: zstd cannot shrink them.
		incompressible := make([]byte, 64<<10)
		for i := range incompressible {
			incompressible[i] = byte(i*i*31 + i*7)
		}
		sum := sha256.Sum256(incompressible)
		for i := 0; i < len(incompressible); i += 32 {
			copy(incompressible[i:], sum[:])
			sum = sha256.Sum256(sum[:])
		}
		id := digestOf(incompressible)
		if err := s.Put(ctx, testDomain, id, int64(len(incompressible)), bytes.NewReader(incompressible)); err != nil {
			t.Fatal(err)
		}
		key, _ := s.Key(testDomain, id)
		fake.mu.Lock()
		stored := fake.objects[key]
		fake.mu.Unlock()
		if codecOf(stored.meta) != codecNone || !bytes.Equal(stored.data, incompressible) {
			t.Fatalf("incompressible payload was stored compressed anyway (%d bytes, codec %q)", len(stored.data), codecOf(stored.meta))
		}
	})

	t.Run("disabled", func(t *testing.T) {
		s, fake := newTestStore(t, Config{Compress: false})
		if err := s.Put(ctx, testDomain, d, int64(len(compressible)), bytes.NewReader(compressible)); err != nil {
			t.Fatal(err)
		}
		key, _ := s.Key(testDomain, d)
		fake.mu.Lock()
		stored := fake.objects[key]
		fake.mu.Unlock()
		if !bytes.Equal(stored.data, compressible) {
			t.Fatal("compression happened with Compress: false")
		}
	})
}

func TestUnavailableRemoteIsDistinguishedFromMissingObject(t *testing.T) {
	s, fake := newTestStore(t, Config{})
	ctx := context.Background()
	fake.failPut = errors.New("dial tcp: connection refused")
	payload := []byte("content")
	err := s.Put(ctx, testDomain, digestOf(payload), int64(len(payload)), bytes.NewReader(payload))
	if !errors.Is(err, objectstore.ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	// A missing object is ErrNotFound, not ErrUnavailable: hydrate must be
	// able to tell "publish it" from "the remote is down".
	if _, err := s.Get(ctx, testDomain, digestOf([]byte("nope"))); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCapabilityProbe(t *testing.T) {
	s, fake := newTestStore(t, Config{Compress: true, Endpoint: "https://objects.example.com"})
	caps, err := s.Test(context.Background(), testDomain)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]objectstore.Capability{}
	for _, c := range caps {
		byName[c.Name] = c
	}
	for _, want := range []string{"reachable", "write", "read-back", "range-requests", "object-metadata", "multipart-upload", "delete"} {
		c, ok := byName[want]
		if !ok {
			t.Fatalf("probe did not report %q: %+v", want, caps)
		}
		if !c.Supported {
			t.Fatalf("probe reported %q unsupported: %s", want, c.Detail)
		}
	}
	if !strings.Contains(byName["reachable"].Detail, "objects.example.com") {
		t.Fatalf("reachable detail should name the endpoint: %q", byName["reachable"].Detail)
	}
	// The probe cleans up after itself.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for k := range fake.objects {
		t.Fatalf("probe left %s behind", k)
	}
}

func TestCapabilityProbeReportsProviderGaps(t *testing.T) {
	s, fake := newTestStore(t, Config{Compress: true})
	fake.dropMetadata = true
	fake.ignoreRange = true
	caps, err := s.Test(context.Background(), testDomain)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]objectstore.Capability{}
	for _, c := range caps {
		byName[c.Name] = c
	}
	if byName["object-metadata"].Supported {
		t.Fatal("a provider that drops user metadata must be reported")
	}
	if !strings.Contains(byName["object-metadata"].Detail, "compression must stay off") {
		t.Fatalf("the metadata failure should say what to do: %q", byName["object-metadata"].Detail)
	}
	if byName["range-requests"].Supported {
		t.Fatal("a provider that ignores Range must be reported")
	}
}

func TestAddressingDefaults(t *testing.T) {
	// A custom endpoint means a bucket-in-path deployment unless told
	// otherwise; AWS itself uses virtual-host style.
	if o := clientOptions(withDefaults(Config{Bucket: "b", Endpoint: "https://minio.local"})); !o.UsePathStyle {
		t.Fatal("custom endpoint should default to path-style addressing")
	}
	if o := clientOptions(withDefaults(Config{Bucket: "b"})); o.UsePathStyle {
		t.Fatal("AWS S3 should default to virtual-host addressing")
	}
	if o := clientOptions(withDefaults(Config{Bucket: "b", Endpoint: "https://x", Addressing: AddressingVirtualHost})); o.UsePathStyle {
		t.Fatal("explicit virtual-host addressing was ignored")
	}
	if o := clientOptions(withDefaults(Config{Bucket: "b"})); o.Region != "auto" {
		t.Fatalf("region %q, want the documented default for providers that ignore it", o.Region)
	}
}

func TestNewRequiresABucket(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, objectstore.ErrInvalidDomain) {
		t.Fatalf("got %v", err)
	}
}

// The store must satisfy the contracts the engine depends on.
var (
	_ objectstore.Store      = (*Store)(nil)
	_ objectstore.Tester     = (*Store)(nil)
	_ objectstore.BatchHaser = (*Store)(nil)
)
