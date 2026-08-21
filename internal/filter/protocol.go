// Package filter implements the Git process boundary of FXVCS: the
// long-running clean/smudge filter (Git filter protocol version 2), the
// pointer-aware diff and merge drivers, the pre-push publication barrier, and
// launcher installation into repository-local Git config.
//
// Normative behaviour is documented in docs/spec/filter-process.md,
// docs/spec/diff-merge-drivers.md, docs/spec/hook-integration.md,
// docs/spec/launcher-installation.md and docs/spec/failure-semantics.md.
package filter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"fxvcs.dev/fxvcs/internal/format"
)

// Protocol constants (git/Documentation/gitattributes: "Long Running Filter
// Process").
const (
	ProtocolVersion   = format.FilterProtocolVersion
	welcomeClient     = "git-filter-client"
	welcomeServer     = "git-filter-server"
	capabilityClean   = "clean"
	capabilitySmudge  = "smudge"
	commandClean      = "clean"
	commandSmudge     = "smudge"
	statusSuccess     = "success"
	statusError       = "error"
	statusAbort       = "abort"
	protocolErrPrefix = "filter: protocol"
)

// ErrProtocol is wrapped by every handshake/framing failure.
var ErrProtocol = errors.New("filter: protocol error")

// Request is one filter command from Git.
type Request struct {
	Command  string // clean | smudge
	Pathname string
	Ref      string // optional (smudge)
	Treeish  string // optional (smudge)
	Blob     string // optional (smudge)
	CanDelay bool   // Git offered delay; this implementation never delays
}

// Handler transforms content for one command. It reads the whole input from
// content and writes the result to out. Writing anything to out commits the
// response to status=success unless the handler then returns an error, in
// which case the protocol reports status=error after the partial content and
// Git discards the result. Returning an error before writing reports
// status=error immediately.
type Handler interface {
	Clean(ctx context.Context, req Request, content io.Reader, out io.Writer) error
	Smudge(ctx context.Context, req Request, content io.Reader, out io.Writer) error
}

// Serve speaks the long-running filter protocol on r/w until Git closes the
// stream. It returns nil on a clean EOF and a wrapped ErrProtocol on framing
// or handshake violations. Handler errors are reported to Git per command and
// do not stop the server; they are also written to log (may be nil).
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler, log io.Writer) error {
	pr := newPktReader(r)
	pw := newPktWriter(w)
	if log == nil {
		log = io.Discard
	}
	if err := handshake(pr, pw); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := readRequest(pr)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		content := pr.content()
		if err := serveOne(ctx, req, content, pw, h, log); err != nil {
			return err
		}
	}
}

func handshake(pr *pktReader, pw *pktWriter) error {
	welcome, err := pr.readTextList()
	if err != nil {
		return fmt.Errorf("%w: welcome: %v", ErrProtocol, err)
	}
	if len(welcome) == 0 || welcome[0] != welcomeClient {
		return fmt.Errorf("%w: expected %s, got %q", ErrProtocol, welcomeClient, welcome)
	}
	versionOK := false
	for _, l := range welcome[1:] {
		if l == fmt.Sprintf("version=%d", ProtocolVersion) {
			versionOK = true
		}
	}
	if !versionOK {
		return fmt.Errorf("%w: client offers %v, this filter requires version=%d", ErrProtocol, welcome[1:], ProtocolVersion)
	}
	if err := pw.writeText(welcomeServer); err != nil {
		return err
	}
	if err := pw.writeText(fmt.Sprintf("version=%d", ProtocolVersion)); err != nil {
		return err
	}
	if err := pw.writeFlush(); err != nil {
		return err
	}
	if err := pw.flush(); err != nil {
		return err
	}
	caps, err := pr.readTextList()
	if err != nil {
		return fmt.Errorf("%w: capabilities: %v", ErrProtocol, err)
	}
	offered := map[string]bool{}
	for _, c := range caps {
		if v, ok := strings.CutPrefix(c, "capability="); ok {
			offered[v] = true
		}
	}
	// Advertise only what Git offered and we implement. "delay" is deliberately
	// not advertised in this generation (see docs/spec/filter-process.md).
	for _, c := range []string{capabilityClean, capabilitySmudge} {
		if offered[c] {
			if err := pw.writeText("capability=" + c); err != nil {
				return err
			}
		}
	}
	if err := pw.writeFlush(); err != nil {
		return err
	}
	return pw.flush()
}

func readRequest(pr *pktReader) (Request, error) {
	lines, err := pr.readTextList()
	if err != nil {
		if err == io.EOF && len(lines) == 0 {
			return Request{}, io.EOF
		}
		return Request{}, fmt.Errorf("%w: command header: %v", ErrProtocol, err)
	}
	var req Request
	for _, l := range lines {
		k, v, _ := strings.Cut(l, "=")
		switch k {
		case "command":
			req.Command = v
		case "pathname":
			req.Pathname = v
		case "ref":
			req.Ref = v
		case "treeish":
			req.Treeish = v
		case "blob":
			req.Blob = v
		case "can-delay":
			req.CanDelay = v == "1"
		}
	}
	if req.Command == "" {
		return Request{}, fmt.Errorf("%w: command header without command=", ErrProtocol)
	}
	return req, nil
}

// serveOne runs one command and writes the response.
func serveOne(ctx context.Context, req Request, content *contentReader, pw *pktWriter, h Handler, log io.Writer) error {
	rw := &responseWriter{pw: pw}
	var herr error
	switch req.Command {
	case commandClean:
		herr = h.Clean(ctx, req, content, rw)
	case commandSmudge:
		herr = h.Smudge(ctx, req, content, rw)
	default:
		herr = fmt.Errorf("unsupported filter command %q", req.Command)
	}
	// Always consume the rest of the input so the stream stays framed.
	if derr := content.drain(); derr != nil && herr == nil {
		herr = derr
	}
	if herr != nil {
		fmt.Fprintf(log, "fxvcs filter: %s %s: %v\n", req.Command, req.Pathname, herr)
	}
	return rw.finish(herr)
}

// responseWriter lazily emits status=success before the first content byte.
type responseWriter struct {
	pw      *pktWriter
	started bool
	failed  error
	cw      contentWriter
}

func (r *responseWriter) Write(b []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	if !r.started {
		if err := r.pw.writeText("status=" + statusSuccess); err != nil {
			r.failed = err
			return 0, err
		}
		if err := r.pw.writeFlush(); err != nil {
			r.failed = err
			return 0, err
		}
		r.started = true
		r.cw = contentWriter{p: r.pw}
	}
	n, err := r.cw.Write(b)
	if err != nil {
		r.failed = err
	}
	return n, err
}

// finish completes the response. herr is the handler's error (nil = success).
func (r *responseWriter) finish(herr error) error {
	if r.failed != nil {
		return r.failed
	}
	var err error
	switch {
	case herr == nil && !r.started:
		// Empty successful result (e.g. empty file).
		err = errorsJoin(
			r.pw.writeText("status="+statusSuccess),
			r.pw.writeFlush(), // end of header
			r.pw.writeFlush(), // end of (empty) content
			r.pw.writeFlush(), // keep status
		)
	case herr == nil:
		err = errorsJoin(
			r.pw.writeFlush(), // end of content
			r.pw.writeFlush(), // keep status=success
		)
	case !r.started:
		err = errorsJoin(
			r.pw.writeText("status="+statusError),
			r.pw.writeFlush(),
		)
	default:
		// Content already started: terminate it, then downgrade the status so
		// Git discards what it received.
		err = errorsJoin(
			r.pw.writeFlush(),
			r.pw.writeText("status="+statusError),
			r.pw.writeFlush(),
		)
	}
	if err != nil {
		return err
	}
	return r.pw.flush()
}

func errorsJoin(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
