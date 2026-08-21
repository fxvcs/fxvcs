// Package s3 is the S3-compatible object store backend (build plan §9, Stage
// 2). It targets AWS S3 and the compatible providers FiveM teams actually use
// — MinIO, Cloudflare R2, Backblaze B2, Wasabi, Ceph — through the AWS SDK for
// Go v2, with custom endpoints, path-style addressing, and Signature V4.
//
// Keys are repository-scoped, so one bucket can serve several storage domains
// without any of them being able to guess another's layout:
//
//	<prefix><storageDomainID>/objects/<aa>/<bb>/<sha256-hex>
//
// Integrity is FXVCS's, never the provider's: the digest is verified while
// bytes stream in both directions and an ETag is never treated as a checksum
// (multipart ETags are not content hashes, and providers differ). Objects are
// immutable, so an upload whose key already exists is a no-op.
//
// Transfers are bounded: a single-part upload buffers at most PartSize bytes,
// a multipart upload holds at most Concurrency part buffers at once, and
// downloads stream. A download interrupted mid-body resumes with a ranged
// request from the offset already verified rather than starting over.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

var (
	digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	domainRE = regexp.MustCompile(`^domain_[0-9A-Za-z][0-9A-Za-z_-]{0,62}$`)
)

// Defaults for transfer tuning.
const (
	DefaultPartSize    = 16 << 20
	DefaultConcurrency = 8
	DefaultMaxAttempts = 5
	// MinPartSize is the S3 minimum for every part but the last.
	MinPartSize = 5 << 20
	// idleConnsPerHost sizes the transport's keep-alive pool. It has to be at
	// least as large as the number of requests in flight, or the workers past
	// that number close their connection after each request and pay a fresh
	// TLS handshake on the next one — which for thousands of small objects
	// costs more than the transfers. It covers the publisher's own upload
	// concurrency, which is higher than this package's.
	idleConnsPerHost = 64
)

// Credentials are static keys for one remote. An empty AccessKeyID selects the
// AWS default credential chain (environment, shared config, instance role).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Config describes one S3-compatible remote.
type Config struct {
	// Endpoint is a custom service endpoint (empty for AWS S3 itself).
	Endpoint string
	Bucket   string
	// Prefix is the key namespace inside the bucket; a trailing slash is
	// added when missing.
	Prefix string
	Region string
	// Addressing is "path", "virtual-host", or empty for the provider
	// default. Custom endpoints usually need path style.
	Addressing string

	Credentials Credentials

	PartSize    int64
	Concurrency int
	MaxAttempts int
	// Compress enables the zstd storage codec (see compress.go). Objects are
	// stored compressed only when that actually makes them smaller.
	Compress bool

	// HTTPClient overrides the transport (tests, proxies, custom TLS).
	HTTPClient aws.HTTPClient
	// Clock is injected by tests; nil means time.Now.
	Clock func() time.Time
}

// Addressing modes.
const (
	AddressingPath        = "path"
	AddressingVirtualHost = "virtual-host"
)

// Store implements objectstore.Store over an S3-compatible bucket.
type Store struct {
	api    api
	cfg    Config
	prefix string
}

// api is the subset of the S3 client the store uses; tests substitute it.
type api interface {
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, in *s3.UploadPartInput, opts ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, opts ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, opts ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// New builds a store from a configuration.
func New(cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("%w: s3 remote needs a bucket", objectstore.ErrInvalidDomain)
	}
	cfg = withDefaults(cfg)
	client := s3.New(clientOptions(cfg))
	return newWithAPI(client, cfg), nil
}

func newWithAPI(a api, cfg Config) *Store {
	cfg = withDefaults(cfg)
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &Store{api: a, cfg: cfg, prefix: prefix}
}

func withDefaults(cfg Config) Config {
	if cfg.PartSize < MinPartSize {
		cfg.PartSize = DefaultPartSize
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Region == "" {
		// Providers that ignore the region still require a syntactically valid
		// one for SigV4; "auto" is what R2 and MinIO document.
		cfg.Region = "auto"
	}
	return cfg
}

// clientOptions maps Config onto the AWS SDK. Retries with exponential
// backoff and jitter come from the SDK's standard retryer, which already
// knows which S3 and HTTP conditions are retryable.
func clientOptions(cfg Config) s3.Options {
	o := s3.Options{
		Region:           cfg.Region,
		Credentials:      credentialsProvider(cfg.Credentials),
		RetryMaxAttempts: cfg.MaxAttempts,
	}
	if cfg.Endpoint != "" {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	switch cfg.Addressing {
	case AddressingPath:
		o.UsePathStyle = true
	case AddressingVirtualHost:
		o.UsePathStyle = false
	default:
		// A custom endpoint is nearly always a bucket-in-path deployment
		// (MinIO, Ceph); AWS itself prefers virtual host style.
		o.UsePathStyle = cfg.Endpoint != ""
	}
	if cfg.HTTPClient != nil {
		o.HTTPClient = cfg.HTTPClient
	} else {
		o.HTTPClient = defaultHTTPClient()
	}
	return o
}

// defaultHTTPClient is the SDK's transport with a keep-alive pool wide enough
// for concurrent uploads. The SDK's own default caps idle connections per host
// well below the concurrency the publisher uses.
func defaultHTTPClient() aws.HTTPClient {
	return awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.MaxIdleConns = idleConnsPerHost * 2
		t.MaxIdleConnsPerHost = idleConnsPerHost
	})
}

func credentialsProvider(c Credentials) aws.CredentialsProvider {
	if c.AccessKeyID == "" {
		return &defaultChain{}
	}
	return credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
}

// Key returns the object key for a digest, for diagnostics and tests.
func (s *Store) Key(domain, digest string) (string, error) {
	h, err := split(domain, digest)
	if err != nil {
		return "", err
	}
	return s.key(domain, h), nil
}

func (s *Store) key(domain, hexPart string) string {
	return s.prefix + domain + "/objects/" + hexPart[:2] + "/" + hexPart[2:4] + "/" + hexPart
}

func split(domain, digest string) (string, error) {
	if !domainRE.MatchString(domain) {
		return "", fmt.Errorf("%w: %q", objectstore.ErrInvalidDomain, domain)
	}
	m := digestRE.FindStringSubmatch(digest)
	if m == nil {
		return "", fmt.Errorf("%w: %q", objectstore.ErrInvalidDigest, digest)
	}
	return m[1], nil
}

// Has reports whether the object exists.
func (s *Store) Has(ctx context.Context, domain, digest string) (bool, error) {
	h, err := split(domain, digest)
	if err != nil {
		return false, err
	}
	_, err = s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.key(domain, h))})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, unavailable(err)
}

// BatchHas answers many existence questions in one call. Publication planning
// for a chunked asset asks about hundreds of digests at once, and sequential
// round trips would dominate the wall clock.
//
// Two things make it cheaper than a loop over Has: the questions are issued
// with bounded concurrency, and digests that crowd into the same two-hex
// fan-out — which happens once an asset has thousands of chunks — are answered
// by one ListObjectsV2 over that prefix instead of one HeadObject each. Below
// that crowding a list would return more keys than were asked about, so plain
// concurrent HEADs win.
func (s *Store) BatchHas(ctx context.Context, domain string, digests []string) (map[string]bool, error) {
	out := make(map[string]bool, len(digests))
	groups := map[string][]string{}
	for _, d := range digests {
		h, err := split(domain, d)
		if err != nil {
			return nil, err
		}
		out[d] = false
		groups[h[:2]] = append(groups[h[:2]], d)
	}
	// listThreshold is where one list beats N heads; below it the list would
	// return more keys than we asked about.
	const listThreshold = 4
	var mu sync.Mutex
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}
	for fanout, members := range groups {
		wg.Add(1)
		sem <- struct{}{}
		go func(fanout string, members []string) {
			defer wg.Done()
			defer func() { <-sem }()
			if len(members) < listThreshold {
				for _, d := range members {
					ok, err := s.Has(ctx, domain, d)
					if err != nil {
						fail(err)
						return
					}
					mu.Lock()
					out[d] = ok
					mu.Unlock()
				}
				return
			}
			present, err := s.listFanout(ctx, domain, fanout)
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range members {
				h := strings.TrimPrefix(d, "sha256:")
				out[d] = present[h]
			}
		}(fanout, members)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// listFanout enumerates the hex names present under one two-hex prefix.
func (s *Store) listFanout(ctx context.Context, domain, fanout string) (map[string]bool, error) {
	present := map[string]bool{}
	prefix := s.prefix + domain + "/objects/" + fanout + "/"
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.cfg.Bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, unavailable(err)
		}
		for _, obj := range out.Contents {
			if obj.Key == nil {
				continue
			}
			name := (*obj.Key)[strings.LastIndex(*obj.Key, "/")+1:]
			present[name] = true
		}
		if out.IsTruncated == nil || !*out.IsTruncated {
			return present, nil
		}
		token = out.NextContinuationToken
	}
}

// Put stores exactly the bytes read from r under the digest, verifying the
// content while it streams. Objects are immutable, so an existing key is
// success without transferring anything.
func (s *Store) Put(ctx context.Context, domain, digest string, size int64, r io.Reader) error {
	h, err := split(domain, digest)
	if err != nil {
		return err
	}
	if ok, err := s.Has(ctx, domain, digest); err != nil {
		return err
	} else if ok {
		return nil
	}
	key := s.key(domain, h)
	// Buffer up to one part. Almost every FXVCS object is a chunk of at most
	// 1 MiB or a small whole file, so this is the normal path; the multipart
	// path exists for whole-blob objects above the part size.
	buf := make([]byte, 0, min64(s.cfg.PartSize, maxInitialBuffer(size)))
	first, full, err := readUpTo(r, buf[:cap(buf)])
	if err != nil {
		return err
	}
	if full {
		return s.putMultipart(ctx, key, digest, size, first, r)
	}
	if size >= 0 && int64(len(first)) != size {
		return fmt.Errorf("s3 store: declared size %d, got %d bytes", size, len(first))
	}
	if got := sha256Hex(first); got != h {
		return fmt.Errorf("s3 store: content digest sha256:%s does not match %s", got, digest)
	}
	body, meta := s.encodeBody(first)
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(s.cfg.Bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader(body),
		Metadata: meta,
	})
	if err != nil {
		return unavailable(err)
	}
	return nil
}

// maxInitialBuffer keeps the first-read buffer proportional to a known size so
// small objects do not allocate a whole part.
func maxInitialBuffer(size int64) int64 {
	if size < 0 {
		return DefaultPartSize
	}
	return size + 1
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// readUpTo fills buf; full reports that buf was filled, meaning more input may
// follow.
func readUpTo(r io.Reader, buf []byte) (data []byte, full bool, err error) {
	n, err := io.ReadFull(r, buf)
	switch {
	case err == nil:
		return buf[:n], true, nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:n], false, nil
	default:
		return nil, false, err
	}
}

// putMultipart uploads an object larger than one part. Memory stays bounded:
// at most Concurrency part buffers exist at any moment, parts are read
// sequentially from the source, and the digest is verified across the whole
// stream before the upload is completed.
//
// Multipart objects are stored uncompressed. They only occur for whole-blob
// encodings above the part size, which by policy are large already-compressed
// assets; streaming compression would also make the "is it smaller?" decision
// impossible to answer before committing to it.
func (s *Store) putMultipart(ctx context.Context, key, digest string, size int64, first []byte, rest io.Reader) error {
	create, err := s.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return unavailable(err)
	}
	uploadID := create.UploadId
	abort := func() {
		// Best effort: a dangling multipart upload costs storage, so providers
		// are usually configured to expire them, but try to be tidy.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = s.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), UploadId: uploadID,
		})
	}

	hash := sha256.New()
	hash.Write(first)
	total := int64(len(first))

	type partResult struct {
		number int32
		etag   *string
		err    error
	}
	var (
		mu      sync.Mutex
		results []partResult
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, s.cfg.Concurrency)
	upload := func(number int32, data []byte) {
		defer wg.Done()
		defer func() { <-sem }()
		out, err := s.api.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String(s.cfg.Bucket),
			Key:        aws.String(key),
			UploadId:   uploadID,
			PartNumber: aws.Int32(number),
			Body:       bytes.NewReader(data),
		})
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			results = append(results, partResult{number: number, err: unavailable(err)})
			return
		}
		results = append(results, partResult{number: number, etag: out.ETag})
	}

	part := first
	number := int32(1)
	for {
		wg.Add(1)
		sem <- struct{}{}
		go upload(number, part)
		next := make([]byte, s.cfg.PartSize)
		data, _, err := readUpTo(rest, next)
		if err != nil {
			wg.Wait()
			abort()
			return err
		}
		if len(data) == 0 {
			break
		}
		hash.Write(data)
		total += int64(len(data))
		part = data
		number++
	}
	wg.Wait()

	for _, res := range results {
		if res.err != nil {
			abort()
			return res.err
		}
	}
	if got := hex.EncodeToString(hash.Sum(nil)); "sha256:"+got != digest {
		abort()
		return fmt.Errorf("s3 store: content digest sha256:%s does not match %s", got, digest)
	}
	if size >= 0 && total != size {
		abort()
		return fmt.Errorf("s3 store: declared size %d, got %d bytes", size, total)
	}
	completed := make([]types.CompletedPart, 0, len(results))
	for _, res := range results {
		completed = append(completed, types.CompletedPart{ETag: res.etag, PartNumber: aws.Int32(res.number)})
	}
	sortParts(completed)
	_, err = s.api.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.cfg.Bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		abort()
		return unavailable(err)
	}
	return nil
}

func sortParts(parts []types.CompletedPart) {
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && *parts[j-1].PartNumber > *parts[j].PartNumber; j-- {
			parts[j-1], parts[j] = parts[j], parts[j-1]
		}
	}
}

// Get returns the object bytes, transparently decoding the storage codec and
// resuming a body that breaks mid-stream.
func (s *Store) Get(ctx context.Context, domain, digest string) (io.ReadCloser, error) {
	h, err := split(domain, digest)
	if err != nil {
		return nil, err
	}
	key := s.key(domain, h)
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, objectstore.ErrNotFound
		}
		return nil, unavailable(err)
	}
	codec := codecOf(out.Metadata)
	if codec != codecNone {
		// A compressed object cannot be range-resumed at a raw offset, so it
		// is decoded as one stream; the caller still verifies the digest.
		return decodeBody(codec, out.Body)
	}
	return &resumingReader{ctx: ctx, store: s, key: key, body: out.Body, attempts: s.cfg.MaxAttempts}, nil
}

// resumingReader continues an interrupted download with a ranged request from
// the last byte delivered, instead of discarding verified progress. It never
// hides corruption: the caller (internal/cache) still hashes everything it
// receives, so a resumed stream that does not add up fails exactly as a broken
// one would.
type resumingReader struct {
	ctx      context.Context //nolint:containedctx // the reader outlives the Get call it belongs to
	store    *Store
	key      string
	body     io.ReadCloser
	offset   int64
	attempts int
	closed   bool
}

func (r *resumingReader) Read(p []byte) (int, error) {
	for {
		n, err := r.body.Read(p)
		r.offset += int64(n)
		if err == nil || errors.Is(err, io.EOF) {
			return n, err
		}
		if r.attempts <= 1 || r.closed {
			return n, err
		}
		r.attempts--
		_ = r.body.Close()
		out, rerr := r.store.api.GetObject(r.ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.store.cfg.Bucket),
			Key:    aws.String(r.key),
			Range:  aws.String("bytes=" + strconv.FormatInt(r.offset, 10) + "-"),
		})
		if rerr != nil {
			return n, err // report the original failure, not the retry's
		}
		r.body = out.Body
		if n > 0 {
			return n, nil
		}
	}
}

func (r *resumingReader) Close() error {
	r.closed = true
	return r.body.Close()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// isNotFound recognises the several ways providers say "no such key".
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// unavailable wraps a transport or provider failure as ErrUnavailable so the
// engine can tell "the remote is unreachable" from "the object is absent".
func unavailable(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", objectstore.ErrUnavailable, err)
}

// defaultChain defers to the AWS default credential chain (environment,
// shared config and profiles, container and instance roles). It resolves
// lazily and once, so a store built for a remote that is never used does not
// touch the filesystem or the instance metadata service.
type defaultChain struct {
	once     sync.Once
	provider aws.CredentialsProvider
	err      error
}

func (d *defaultChain) Retrieve(ctx context.Context) (aws.Credentials, error) {
	d.once.Do(func() {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			d.err = err
			return
		}
		d.provider = cfg.Credentials
	})
	if d.err != nil {
		return aws.Credentials{}, fmt.Errorf("s3 store: no credentials available (%v); run: fxvcs storage login <remote>", d.err)
	}
	if d.provider == nil {
		return aws.Credentials{}, errors.New("s3 store: no credentials available; run: fxvcs storage login <remote>")
	}
	creds, err := d.provider.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("s3 store: no credentials available (%v); run: fxvcs storage login <remote>", err)
	}
	return creds, nil
}
