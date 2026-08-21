package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"fxvcs.dev/fxvcs/internal/objectstore"
)

// Test probes what `fxvcs storage test` reports for an S3-compatible remote.
//
// It answers the questions that actually decide whether a provider can back a
// storage domain, in the order they fail: can we reach and authenticate
// against the bucket, may we list our own prefix, can we write and read back
// an object byte for byte, does the provider keep the metadata the storage
// codec depends on, and does a multipart upload complete. Each probe is
// scoped to the domain's own key namespace, and the probe object is removed
// afterwards when deletion is permitted — a read-only agent identity that
// cannot delete is reported, not treated as a failure.
func (s *Store) Test(ctx context.Context, domain string) ([]objectstore.Capability, error) {
	if !domainRE.MatchString(domain) {
		return nil, fmt.Errorf("%w: %q", objectstore.ErrInvalidDomain, domain)
	}
	var caps []objectstore.Capability
	add := func(name string, ok bool, detail string) {
		caps = append(caps, objectstore.Capability{Name: name, Supported: ok, Detail: detail})
	}

	where := s.cfg.Bucket + "/" + s.prefix + domain
	if s.cfg.Endpoint != "" {
		where += " @ " + s.cfg.Endpoint
	}
	if _, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.cfg.Bucket),
		Prefix:  aws.String(s.prefix + domain + "/"),
		MaxKeys: aws.Int32(1),
	}); err != nil {
		add("reachable", false, reason(err))
		return caps, nil
	}
	add("reachable", true, where)
	add("list-prefix", true, "")

	payload := []byte("fxvcs-storage-test\n" + domain)
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	key, err := s.Key(domain, digest)
	if err != nil {
		return nil, err
	}
	if err := s.Put(ctx, domain, digest, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		add("write", false, reason(err))
		return caps, nil
	}
	add("write", true, "")

	rc, err := s.Get(ctx, domain, digest)
	switch {
	case err != nil:
		add("read-back", false, reason(err))
	default:
		got, rerr := io.ReadAll(rc)
		_ = rc.Close()
		switch {
		case rerr != nil:
			add("read-back", false, reason(rerr))
		case !bytes.Equal(got, payload):
			add("read-back", false, "the object read back differs from what was written")
		default:
			add("read-back", true, "")
		}
	}

	// Range requests are what makes an interrupted download resumable.
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), Range: aws.String("bytes=5-"),
	})
	if err != nil {
		add("range-requests", false, reason(err))
	} else {
		got, rerr := io.ReadAll(out.Body)
		_ = out.Body.Close()
		add("range-requests", rerr == nil && bytes.Equal(got, payload[5:]), rangeDetail(rerr, got, payload))
	}

	// User metadata carries the storage codec, so a provider that silently
	// drops it would make compressed objects unreadable. Write a probe object
	// that carries metadata and read it back.
	if s.cfg.Compress {
		mok, mdetail := s.probeMetadata(ctx, domain)
		add("object-metadata", mok, mdetail)
	}

	ok, detail := s.probeMultipart(ctx, domain)
	add("multipart-upload", ok, detail)

	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)}); err != nil {
		// Not a failure: contributors need write access, agents are read-only,
		// and only a garbage collector needs delete.
		caps = append(caps, objectstore.Capability{Name: "delete", Supported: false,
			Detail: "this identity cannot delete objects; that is expected for readers and agents, but object garbage collection needs a separately authorized identity"})
	} else {
		add("delete", true, "")
	}
	return caps, nil
}

// probeMultipart creates and immediately aborts a multipart upload: it proves
// the provider implements the API without moving a 5 MiB part over the wire.
func (s *Store) probeMultipart(ctx context.Context, domain string) (bool, string) {
	key := s.prefix + domain + "/probe/multipart"
	create, err := s.api.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key),
	})
	if err != nil {
		return false, reason(err)
	}
	if _, err := s.api.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), UploadId: create.UploadId,
	}); err != nil {
		return true, "created, but the upload could not be aborted: " + reason(err) +
			" (incomplete uploads may accumulate; configure a lifecycle rule)"
	}
	return true, ""
}

func rangeDetail(err error, got, want []byte) string {
	switch {
	case err != nil:
		return reason(err)
	case !bytes.Equal(got, want[5:]):
		return "the provider ignored the Range header; interrupted downloads will restart from the beginning"
	}
	return ""
}

// reason trims an SDK error to something a person can act on.
func reason(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "https response error "); i >= 0 {
		msg = msg[i:]
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// probeMetadata checks that provider-side user metadata survives a round
// trip, which the storage codec depends on.
func (s *Store) probeMetadata(ctx context.Context, domain string) (bool, string) {
	key := s.prefix + domain + "/probe/metadata"
	if _, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   aws.String(s.cfg.Bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader([]byte("probe")),
		Metadata: map[string]string{metaCodec: codecZstd, metaRawSize: "5"},
	}); err != nil {
		return false, reason(err)
	}
	defer func() {
		_, _ = s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	}()
	head, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	if err != nil {
		return false, reason(err)
	}
	if codecOf(head.Metadata) != codecZstd {
		return false, "the provider did not return the user metadata that was written; compression must stay off for this remote"
	}
	return true, ""
}
