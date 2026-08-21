package filter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/internal/pointer"
)

const testDomain = "domain_test_01"

// pkt builds a pkt-line stream from text packets ("" = flush) and raw byte
// packets (prefixed with '\x00' marker in this helper via rawPkt).
type pktBuilder struct{ bytes.Buffer }

func (b *pktBuilder) text(lines ...string) *pktBuilder {
	for _, l := range lines {
		payload := l + "\n"
		fmt.Fprintf(&b.Buffer, "%04x%s", len(payload)+4, payload)
	}
	return b
}

func (b *pktBuilder) flush() *pktBuilder { b.WriteString("0000"); return b }

func (b *pktBuilder) content(data []byte) *pktBuilder {
	for len(data) > 0 {
		n := len(data)
		if n > MaxPacketPayload {
			n = MaxPacketPayload
		}
		fmt.Fprintf(&b.Buffer, "%04x", n+4)
		b.Write(data[:n])
		data = data[n:]
	}
	return b.flush()
}

// gitSession scripts what Git sends: handshake then commands.
func gitSession(cmds ...func(*pktBuilder)) []byte {
	var b pktBuilder
	b.text("git-filter-client", "version=2").flush()
	b.text("capability=clean", "capability=smudge", "capability=delay").flush()
	for _, c := range cmds {
		c(&b)
	}
	return b.Bytes()
}

func cmd(command, path string, content []byte) func(*pktBuilder) {
	return func(b *pktBuilder) {
		b.text("command="+command, "pathname="+path)
		if command == "smudge" {
			b.text("can-delay=1")
		}
		b.flush().content(content)
	}
}

// parseResponses decodes the server stream: handshake then per-command
// (status, content, finalStatus).
type response struct {
	status, finalStatus string
	content             []byte
}

func parseResponses(t *testing.T, data []byte) []response {
	t.Helper()
	pr := newPktReader(bytes.NewReader(data))
	welcome, err := pr.readTextList()
	if err != nil || len(welcome) != 2 || welcome[0] != "git-filter-server" || welcome[1] != "version=2" {
		t.Fatalf("bad welcome %v %v", welcome, err)
	}
	caps, err := pr.readTextList()
	if err != nil || strings.Join(caps, ",") != "capability=clean,capability=smudge" {
		t.Fatalf("bad capabilities %v %v (delay must not be advertised)", caps, err)
	}
	var out []response
	for {
		st, err := pr.readTextList()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		r := response{status: strings.Join(st, ",")}
		if r.status == "status=error" || r.status == "status=abort" {
			out = append(out, r)
			continue
		}
		c, err := io.ReadAll(pr.content())
		if err != nil {
			t.Fatal(err)
		}
		r.content = c
		fin, err := pr.readTextList()
		if err != nil {
			t.Fatal(err)
		}
		r.finalStatus = strings.Join(fin, ",")
		out = append(out, r)
	}
}

// fakeBackend is an in-memory Backend for protocol tests: it ingests into
// the engine cache, records hydration intent per path, and can "fetch" from a
// side map of objects.
type fakeBackend struct {
	c        *cache.Cache
	intent   map[string]bool // path -> hydrated
	remote   map[string][]byte
	recorded map[string]string // path -> digest recorded hydrated by clean
	required []string
	pub      map[string]map[string]bool
}

func newFakeBackend(c *cache.Cache) *fakeBackend {
	return &fakeBackend{c: c, intent: map[string]bool{}, remote: map[string][]byte{}, recorded: map[string]string{}, pub: map[string]map[string]bool{}}
}

func (f *fakeBackend) Ingest(_ context.Context, content io.Reader, _ string) (pointer.Pointer, error) {
	res, err := f.c.Put(testDomain, content)
	if err != nil {
		return pointer.Pointer{}, err
	}
	if err := f.c.Pin(testDomain, res.Digest); err != nil {
		return pointer.Pointer{}, err
	}
	return WholePointer(testDomain, res.Digest, res.Size)
}

func (f *fakeBackend) HydrationIntent(_ context.Context, path string) (bool, bool, error) {
	h, ok := f.intent[path]
	return h, ok, nil
}

func (f *fakeBackend) RecordHydrated(_ context.Context, path, digest string, _ int64) error {
	f.recorded[path] = digest
	f.intent[path] = true
	return nil
}

func (f *fakeBackend) FetchObject(_ context.Context, digest string) error {
	b, ok := f.remote[digest]
	if !ok {
		return errors.New("fake remote: not found")
	}
	_, err := f.c.Put(testDomain, bytes.NewReader(b))
	return err
}

// ContentObjects / OpenContent model the whole/1 case only: the fake stores
// one object per pointer, like the Stage 1 engine did.
func (f *fakeBackend) ContentObjects(_ context.Context, ptr pointer.Pointer) ([]string, error) {
	return []string{ptr.OID}, nil
}

func (f *fakeBackend) OpenContent(ctx context.Context, ptr pointer.Pointer) (io.ReadCloser, error) {
	rd, err := f.c.Open(testDomain, ptr.OID)
	if errors.Is(err, cache.ErrNotFound) {
		if ferr := f.FetchObject(ctx, ptr.OID); ferr != nil {
			return nil, ferr
		}
		rd, err = f.c.Open(testDomain, ptr.OID)
	}
	if err != nil {
		return nil, err
	}
	if rd.Size() != ptr.Size {
		_ = rd.Close()
		return nil, fmt.Errorf("%w: cached size %d, pointer size %d", ErrObjectMismatch, rd.Size(), ptr.Size)
	}
	return rd, nil
}

func (f *fakeBackend) MissingRemotes(_ context.Context, ptr pointer.Pointer) ([]string, error) {
	var missing []string
	for _, r := range f.required {
		if !f.pub[ptr.OID][r] {
			missing = append(missing, r)
		}
	}
	return missing, nil
}

func (f *fakeBackend) MissingRemotesMany(ctx context.Context, ptrs []pointer.Pointer) (map[string][]string, error) {
	out := map[string][]string{}
	for _, p := range ptrs {
		m, err := f.MissingRemotes(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(m) > 0 {
			out[p.OID] = m
		}
	}
	return out, nil
}

func (f *fakeBackend) RequiredRemotes() []string { return f.required }

func (f *fakeBackend) Publish(context.Context, []string, io.Writer) []PublishFailure { return nil }
func (f *fakeBackend) Close() error                                                  { return nil }

func newEngine(t *testing.T) *Engine {
	t.Helper()
	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{Cache: c, Domain: testDomain, Installed: format.MustParseVersion("0.0.0-dev")}
}

// newBackedEngine returns an engine with a fakeBackend attached.
func newBackedEngine(t *testing.T) (*Engine, *fakeBackend) {
	t.Helper()
	eng := newEngine(t)
	fb := newFakeBackend(eng.Cache)
	eng.Backend = fb
	return eng, fb
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestServeCleanSmudgeRoundTrip(t *testing.T) {
	eng, fb := newBackedEngine(t)
	asset := randomBytes(3<<20+17, 1) // spans many packets, not packet aligned
	sum := sha256.Sum256(asset)
	wantPtr, err := WholePointer(testDomain, cache.Digest(sum[:]), int64(len(asset)))
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := wantPtr.MustEncode()

	in := gitSession(
		cmd("clean", "a.bin", asset),
		cmd("clean", "a.bin", asset),                              //nolint:gocritic // deliberate duplicate: proves determinism
		cmd("clean", "a.bin", wantBytes),                          // pointer pass-through (idempotent)
		cmd("smudge", "a.bin", wantBytes),                         // hydrated intent (recorded by clean): content from cache
		cmd("smudge", "old.bin", []byte("historical raw blob\n")), // pass-through
		cmd("clean", "empty.bin", nil),
		cmd("smudge", "other.bin", wantBytes), // no intent recorded: pointer stays (unhydrated default)
	)
	var out bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(in), &out, eng, io.Discard); err != nil {
		t.Fatal(err)
	}
	rs := parseResponses(t, out.Bytes())
	if len(rs) != 7 {
		t.Fatalf("got %d responses", len(rs))
	}
	if fb.recorded["a.bin"] != wantPtr.OID {
		t.Fatalf("clean did not record hydrated intent for a.bin: %v", fb.recorded)
	}
	if rs[6].status != "status=success" || !bytes.Equal(rs[6].content, wantBytes) {
		t.Fatalf("unhydrated default violated for other.bin: %+v", rs[6])
	}
	for i := 0; i < 3; i++ {
		if rs[i].status != "status=success" || rs[i].finalStatus != "" || !bytes.Equal(rs[i].content, wantBytes) {
			t.Fatalf("clean #%d: %+v", i, rs[i])
		}
	}
	if !bytes.Equal(rs[3].content, asset) || rs[3].status != "status=success" {
		t.Fatalf("smudge did not reproduce asset (%d bytes)", len(rs[3].content))
	}
	if string(rs[4].content) != "historical raw blob\n" {
		t.Fatalf("raw blob not passed through: %q", rs[4].content)
	}
	// Zero-length content is stored inline as the empty blob, never as a
	// pointer: a pointer here makes the index entry permanently stat-dirty in
	// Git and breaks every merge in the repository.
	if rs[5].status != "status=success" || len(rs[5].content) != 0 {
		t.Fatalf("empty file clean must emit no bytes, got %d: %+v", len(rs[5].content), rs[5])
	}
	if _, ok := fb.recorded["empty.bin"]; ok {
		t.Fatalf("empty file must not record hydration intent: %v", fb.recorded)
	}
	// The object is cached and pinned.
	info, err := eng.Cache.Stat(testDomain, wantPtr.OID)
	if err != nil || !info.Pinned || info.Size != int64(len(asset)) {
		t.Fatalf("cache stat %+v %v", info, err)
	}
}

func TestServeSmudgeMissingObjectEmitsPointer(t *testing.T) {
	eng := newEngine(t)
	p, _ := WholePointer(testDomain, "sha256:"+strings.Repeat("ab", 32), 10)
	pb := p.MustEncode()
	var out bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(gitSession(cmd("smudge", "x.bin", pb))), &out, eng, io.Discard); err != nil {
		t.Fatal(err)
	}
	rs := parseResponses(t, out.Bytes())
	if len(rs) != 1 || rs[0].status != "status=success" || !bytes.Equal(rs[0].content, pb) {
		t.Fatalf("unhydrated default violated: %+v", rs)
	}
}

func TestServeErrors(t *testing.T) {
	eng, fb := newBackedEngine(t)
	unsupported := []byte("version https://fxvcs.dev/pointer/v2\ndomain domain_test_01\n")
	malformed := []byte("version https://fxvcs.dev/pointer/v1\ndomain domain_test_01\n")
	// Corrupt cached object: put, then flip a byte on disk.
	asset := []byte("asset bytes for corruption")
	res, err := eng.Cache.Put(testDomain, bytes.NewReader(asset))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := WholePointer(testDomain, res.Digest, int64(len(asset)))
	corruptOnDisk(t, eng.Cache, testDomain, res.Digest, asset)
	fb.intent["c.bin"] = true
	// Pointer whose manifest does not derive from its oid/size.
	bad := p
	bad.ManifestDigest = "sha256:" + strings.Repeat("00", 32)
	// Pointer from another storage domain.
	foreign, _ := WholePointer("domain_other_01", res.Digest, int64(len(asset)))
	// Hydrated path whose object is neither cached nor fetchable.
	gone, _ := WholePointer(testDomain, "sha256:"+strings.Repeat("cd", 32), 5)
	fb.intent["gone.bin"] = true
	// Hydrated path whose object is only on the remote: fetched then served.
	remoteAsset := []byte("remote-only bytes")
	rp, _ := WholePointer(testDomain, sha256Digest(remoteAsset), int64(len(remoteAsset)))
	fb.remote[rp.OID] = remoteAsset
	fb.intent["fetched.bin"] = true

	var log bytes.Buffer
	in := gitSession(
		cmd("smudge", "n.bin", unsupported),
		cmd("clean", "n.bin", unsupported),
		cmd("clean", "m.bin", malformed),
		cmd("smudge", "c.bin", p.MustEncode()),
		cmd("smudge", "b.bin", bad.MustEncode()),
		cmd("clean", "ok.bin", []byte("still serving after errors")),
		cmd("smudge", "f.bin", foreign.MustEncode()),
		cmd("smudge", "gone.bin", gone.MustEncode()),
		cmd("smudge", "fetched.bin", rp.MustEncode()),
	)
	var out bytes.Buffer
	if err := Serve(context.Background(), bytes.NewReader(in), &out, eng, &log); err != nil {
		t.Fatal(err)
	}
	rs := parseResponses(t, out.Bytes())
	if len(rs) != 9 {
		t.Fatalf("got %d responses: %+v", len(rs), rs)
	}
	if rs[6].status != "status=error" {
		t.Fatalf("foreign-domain pointer accepted: %+v", rs[6])
	}
	if rs[7].status != "status=error" {
		t.Fatalf("hydrated path with unavailable object did not fail: %+v", rs[7])
	}
	if rs[8].status != "status=success" || !bytes.Equal(rs[8].content, remoteAsset) {
		t.Fatalf("hydrated path was not served after fetch: %+v", rs[8])
	}
	if !strings.Contains(log.String(), "fxvcs load gone.bin") {
		t.Fatalf("unavailable-object message lacks the remedy:\n%s", log.String())
	}
	for i := 0; i < 3; i++ {
		if rs[i].status != "status=error" {
			t.Fatalf("response %d should be status=error, got %+v", i, rs[i])
		}
	}
	// Corrupt object: content may have started, but the final status is error
	// so Git discards it; never a success with wrong bytes.
	if rs[3].status == "status=success" && rs[3].finalStatus != "status=error" {
		t.Fatalf("corrupt object reported success: %+v", rs[3])
	}
	if rs[4].status != "status=error" {
		t.Fatalf("inconsistent manifest accepted: %+v", rs[4])
	}
	if rs[5].status != "status=success" || !pointer.Sniff(rs[5].content) {
		t.Fatalf("server did not keep serving: %+v", rs[5])
	}
	l := log.String()
	if !strings.Contains(l, "0.0.0-dev") || !strings.Contains(l, "pointer format v1") || !strings.Contains(l, "fxvcs upgrade --check") {
		t.Fatalf("unsupported-version log lacks installed/required detail:\n%s", l)
	}
}

func corruptOnDisk(t *testing.T, c *cache.Cache, domain, digest string, original []byte) {
	t.Helper()
	// Locate via Open (path is internal); rewrite through the known layout.
	hexPart := strings.TrimPrefix(digest, "sha256:")
	p := c.Root() + "/objects/" + domain + "/" + hexPart[:2] + "/" + hexPart[2:4] + "/" + hexPart
	corrupt := append([]byte(nil), original...)
	corrupt[0] ^= 0xff
	if err := writeFileForce(p, corrupt); err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeRejectsWrongVersion(t *testing.T) {
	var b pktBuilder
	b.text("git-filter-client", "version=1").flush()
	err := Serve(context.Background(), bytes.NewReader(b.Bytes()), io.Discard, newEngine(t), io.Discard)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected ErrProtocol, got %v", err)
	}
}

func TestPktLineFraming(t *testing.T) {
	var b pktBuilder
	b.text("a=b").content(bytes.Repeat([]byte("x"), MaxPacketPayload+5))
	pr := newPktReader(bytes.NewReader(b.Bytes()))
	p, fl, err := pr.readPacket()
	if err != nil || fl || string(p) != "a=b\n" {
		t.Fatalf("first packet %q %v %v", p, fl, err)
	}
	p, _, _ = pr.readPacket()
	if len(p) != MaxPacketPayload {
		t.Fatalf("max packet len %d", len(p))
	}
	p, _, _ = pr.readPacket()
	if len(p) != 5 {
		t.Fatalf("tail packet len %d", len(p))
	}
	if _, fl, _ = pr.readPacket(); !fl {
		t.Fatal("expected flush")
	}
	if _, _, err = pr.readPacket(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
	if _, _, err = newPktReader(strings.NewReader("zzzz")).readPacket(); !errors.Is(err, errBadPacketLength) {
		t.Fatalf("bad length: %v", err)
	}
	if _, _, err = newPktReader(strings.NewReader("0003")).readPacket(); !errors.Is(err, errBadPacketLength) {
		t.Fatalf("length 3: %v", err)
	}
	if _, _, err = newPktReader(strings.NewReader("00")).readPacket(); !errors.Is(err, errUnexpectedEOF) {
		t.Fatalf("truncated: %v", err)
	}
	var out bytes.Buffer
	pw := newPktWriter(&out)
	if err := pw.writePacket(make([]byte, MaxPacketPayload+1)); err == nil {
		t.Fatal("oversized packet accepted")
	}
}
