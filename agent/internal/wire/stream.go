package wire

import (
	"io"
	"sync"

	"github.com/quic-go/quic-go"
)

// Stream is what the protocols need of a QUIC stream: a read half and a
// write half, released separately. Close ends the write half, so the far
// side reads to EOF after the bytes written so far; CancelRead discards
// the read half, so the far side's writes fail. A *quic.Stream is one. So
// is each end of a Loopback, which is how a device reaches its own server
// without the network.
type Stream interface {
	io.Reader
	io.Writer
	Close() error
	CancelRead(quic.StreamErrorCode)
}

// loopbackWindow bounds what one direction of a loopback buffers before
// writes wait for reads, the way QUIC flow control does.
const loopbackWindow = 4 << 20

// Loopback is a stream and its far end in the same process: what one
// writes the other reads, each direction buffered up to a window, each
// half closed like a QUIC stream.
func Loopback() (near, far Stream) {
	a, b := newPipe(), newPipe()
	return &loopbackHalf{read: a, write: b}, &loopbackHalf{read: b, write: a}
}

type loopbackHalf struct {
	read  *pipe // the far side writes here
	write *pipe // this side writes here
}

func (h *loopbackHalf) Read(p []byte) (int, error)  { return h.read.read(p) }
func (h *loopbackHalf) Write(p []byte) (int, error) { return h.write.write(p) }

// Close ends the write half: the far side reads what was written, then EOF.
func (h *loopbackHalf) Close() error { h.write.closeWrite(); return nil }

// CancelRead discards the read half with a code: the far side's writes
// fail with a stream error carrying it, as they would over QUIC.
func (h *loopbackHalf) CancelRead(code quic.StreamErrorCode) { h.read.cancelRead(code) }

// pipe is one direction: a bounded byte queue with QUIC's closing rules.
type pipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool  // the writer closed: EOF once buf drains
	err    error // the reader cancelled: writes and reads fail with it
}

func newPipe() *pipe {
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *pipe) write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for len(b) > 0 {
		switch {
		case p.err != nil:
			return n, p.err
		case p.closed:
			return n, io.ErrClosedPipe
		}
		if room := loopbackWindow - len(p.buf); room > 0 {
			if room > len(b) {
				room = len(b)
			}
			p.buf = append(p.buf, b[:room]...)
			b = b[room:]
			n += room
			p.cond.Broadcast()
			continue
		}
		p.cond.Wait()
	}
	return n, nil
}

func (p *pipe) read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if len(p.buf) > 0 {
			n := copy(b, p.buf)
			p.buf = p.buf[n:]
			p.cond.Broadcast()
			return n, nil
		}
		switch {
		case p.err != nil:
			return 0, p.err
		case p.closed:
			return 0, io.EOF
		}
		p.cond.Wait()
	}
}

func (p *pipe) closeWrite() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

func (p *pipe) cancelRead(code quic.StreamErrorCode) {
	p.mu.Lock()
	if p.err == nil {
		p.err = &quic.StreamError{ErrorCode: code, Remote: true}
	}
	p.buf = nil
	p.cond.Broadcast()
	p.mu.Unlock()
}
