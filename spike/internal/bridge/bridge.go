// Package bridge is the phase 1 loopback bridge: a local TCP port per peer,
// each accepted connection tunnelled over one QUIC stream to a port the peer
// exposes on its own loopback. It also carries the throughput bench.
package bridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/session"
)

// Header opens every stream: a port to bridge to, or a bench size.
type Header struct {
	Port  int   `json:"port,omitempty"`
	Bench int64 `json:"bench,omitempty"`
}

func writeHeader(w io.Writer, h Header) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	buf := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(buf, uint16(len(b)))
	copy(buf[2:], b)
	_, err = w.Write(buf)
	return err
}

func readHeader(r io.Reader) (Header, error) {
	var h Header
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return h, err
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return h, err
	}
	return h, json.Unmarshal(b, &h)
}

// Server answers the peer's streams.
type Server struct {
	Expose map[int]bool // loopback ports peers may reach
	Log    *log.Logger

	mu       sync.Mutex
	accepted int
	refused  int
}

// Handle is the session manager's OnStream callback.
func (s *Server) Handle(peer identity.ID, st *quic.Stream) {
	h, err := readHeader(st)
	if err != nil {
		st.CancelRead(1)
		st.CancelWrite(1)
		return
	}
	switch {
	case h.Bench > 0:
		n, err := io.CopyN(io.Discard, st, h.Bench)
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], uint64(n))
		_, _ = st.Write(out[:])
		_ = st.Close()
		if err != nil {
			s.Log.Printf("bench from %s: received %d of %d bytes: %v", peer.Short(), n, h.Bench, err)
		}
	case s.Expose[h.Port]:
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", h.Port), 5*time.Second)
		if err != nil {
			s.Log.Printf("bridge from %s to port %d: %v", peer.Short(), h.Port, err)
			st.CancelRead(2)
			st.CancelWrite(2)
			return
		}
		s.mu.Lock()
		s.accepted++
		s.mu.Unlock()
		pipe(c, st)
	default:
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		s.Log.Printf("bridge from %s to port %d refused: not exposed", peer.Short(), h.Port)
		st.CancelRead(3)
		st.CancelWrite(3)
	}
}

// Counts reports bridged and refused connections.
func (s *Server) Counts() (accepted, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted, s.refused
}

// Listen serves local TCP connections to peer's port until ctx ends.
func Listen(ctx context.Context, mgr *session.Manager, local string, peer identity.ID, port int, lg *log.Logger) error {
	ln, err := net.Listen("tcp", local)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	mgr.Connect(peer)
	lg.Printf("bridge: %s -> %s port %d", local, peer.Short(), port)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			octx, cancel := context.WithTimeout(ctx, 30*time.Second)
			st, path, err := mgr.OpenStream(octx, peer)
			cancel()
			if err != nil {
				lg.Printf("bridge: no stream to %s: %v", peer.Short(), err)
				_ = c.Close()
				return
			}
			if err := writeHeader(st, Header{Port: port}); err != nil {
				_ = c.Close()
				return
			}
			lg.Printf("bridge: %s -> %s:%d via %s", c.RemoteAddr(), peer.Short(), port, path)
			pipe(c, st)
		}()
	}
}

// pipe copies both ways and propagates half-closes, so a client that finishes
// writing still receives the server's answer.
func pipe(c net.Conn, st *quic.Stream) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(st, c)
		_ = st.Close()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, st)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		} else {
			_ = c.Close()
		}
	}()
	wg.Wait()
	_ = c.Close()
}

// Result is one bench run.
type Result struct {
	Bytes    int64         `json:"bytes"`
	Path     string        `json:"path"`
	Duration time.Duration `json:"duration"`
	MbitPerS float64       `json:"mbit_per_s"`
	MiBPerS  float64       `json:"mib_per_s"`
}

// Bench sends size bytes to peer on the active path and waits for the count.
func Bench(ctx context.Context, mgr *session.Manager, peer identity.ID, size int64) (Result, error) {
	st, path, err := mgr.OpenStream(ctx, peer)
	if err != nil {
		return Result{}, err
	}
	if err := writeHeader(st, Header{Bench: size}); err != nil {
		return Result{}, err
	}
	buf := make([]byte, 64<<10)
	started := time.Now()
	var sent int64
	for sent < size {
		n := int64(len(buf))
		if size-sent < n {
			n = size - sent
		}
		w, err := st.Write(buf[:n])
		sent += int64(w)
		if err != nil {
			return Result{}, fmt.Errorf("after %d bytes: %w", sent, err)
		}
	}
	_ = st.Close()
	var out [8]byte
	if _, err := io.ReadFull(st, out[:]); err != nil {
		return Result{}, fmt.Errorf("waiting for the count: %w", err)
	}
	d := time.Since(started)
	got := int64(binary.BigEndian.Uint64(out[:]))
	if got != size {
		return Result{}, errors.New("peer counted a different size")
	}
	return Result{
		Bytes: size, Path: path, Duration: d,
		MbitPerS: float64(size) * 8 / d.Seconds() / 1e6,
		MiBPerS:  float64(size) / d.Seconds() / (1 << 20),
	}, nil
}
