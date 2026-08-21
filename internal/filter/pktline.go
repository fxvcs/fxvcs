package filter

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// pkt-line framing as used by Git's long-running filter protocol: each packet
// is a 4-hex-digit length (including the 4 length bytes) followed by payload.
// "0000" is the flush packet that terminates a list.
const (
	// MaxPacketPayload is the largest payload one packet may carry.
	MaxPacketPayload = 65516
	maxPacketLen     = MaxPacketPayload + 4
)

var (
	errBadPacketLength = errors.New("filter: malformed pkt-line length")
	errUnexpectedEOF   = errors.New("filter: unexpected end of pkt-line stream")
)

// pktReader reads pkt-lines from a byte stream.
type pktReader struct {
	r   *bufio.Reader
	buf []byte
}

func newPktReader(r io.Reader) *pktReader {
	return &pktReader{r: bufio.NewReaderSize(r, 1<<16), buf: make([]byte, maxPacketLen)}
}

// readPacket returns the payload of the next packet. flush is true for
// "0000" (payload is nil). io.EOF is returned only at a clean packet boundary.
func (p *pktReader) readPacket() (payload []byte, flush bool, err error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(p.r, lenBuf[:]); err != nil {
		if err == io.EOF {
			return nil, false, io.EOF
		}
		return nil, false, errUnexpectedEOF
	}
	var n [2]byte
	if _, err := hex.Decode(n[:], lenBuf[:]); err != nil {
		return nil, false, fmt.Errorf("%w: %q", errBadPacketLength, lenBuf)
	}
	length := int(n[0])<<8 | int(n[1])
	switch {
	case length == 0:
		return nil, true, nil
	case length < 4 || length > maxPacketLen:
		return nil, false, fmt.Errorf("%w: %d", errBadPacketLength, length)
	}
	payload = p.buf[:length-4]
	if _, err := io.ReadFull(p.r, payload); err != nil {
		return nil, false, errUnexpectedEOF
	}
	return payload, false, nil
}

// readTextList reads packets until flush, treating each payload as a text
// line (trailing "\n" removed).
func (p *pktReader) readTextList() ([]string, error) {
	var out []string
	for {
		payload, flush, err := p.readPacket()
		if err != nil {
			return out, err
		}
		if flush {
			return out, nil
		}
		s := string(payload)
		if len(s) > 0 && s[len(s)-1] == '\n' {
			s = s[:len(s)-1]
		}
		out = append(out, s)
	}
}

// contentReader exposes the packets up to the next flush as an io.Reader.
type contentReader struct {
	p       *pktReader
	pending []byte
	done    bool
	err     error
}

func (p *pktReader) content() *contentReader { return &contentReader{p: p} }

func (c *contentReader) Read(b []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.done {
			return 0, io.EOF
		}
		if c.err != nil {
			return 0, c.err
		}
		payload, flush, err := c.p.readPacket()
		if err != nil {
			if err == io.EOF {
				err = errUnexpectedEOF
			}
			c.err = err
			return 0, err
		}
		if flush {
			c.done = true
			return 0, io.EOF
		}
		c.pending = payload
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// drain consumes the rest of the content list so the stream stays in sync.
func (c *contentReader) drain() error {
	_, err := io.Copy(io.Discard, c)
	return err
}

// pktWriter writes pkt-lines.
type pktWriter struct {
	w *bufio.Writer
}

func newPktWriter(w io.Writer) *pktWriter { return &pktWriter{w: bufio.NewWriterSize(w, 1<<16)} }

func (p *pktWriter) writePacket(payload []byte) error {
	if len(payload) > MaxPacketPayload {
		return fmt.Errorf("filter: packet payload %d exceeds %d", len(payload), MaxPacketPayload)
	}
	if _, err := fmt.Fprintf(p.w, "%04x", len(payload)+4); err != nil {
		return err
	}
	_, err := p.w.Write(payload)
	return err
}

// writeText writes one text packet ("key=value\n").
func (p *pktWriter) writeText(s string) error { return p.writePacket([]byte(s + "\n")) }

func (p *pktWriter) writeFlush() error {
	_, err := p.w.WriteString("0000")
	return err
}

func (p *pktWriter) flush() error { return p.w.Flush() }

// contentWriter splits arbitrary bytes into content packets.
type contentWriter struct {
	p *pktWriter
	n int64
}

func (c *contentWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > MaxPacketPayload {
			chunk = b[:MaxPacketPayload]
		}
		if err := c.p.writePacket(chunk); err != nil {
			return total, err
		}
		total += len(chunk)
		c.n += int64(len(chunk))
		b = b[len(chunk):]
	}
	return total, nil
}
