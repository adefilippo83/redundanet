// Command hub is the spike's rendezvous and relay: it introduces devices to
// each other, reflects their public addresses, and forwards end-to-end QUIC
// packets it cannot read. It listens for QUIC on UDP and, for networks that
// block UDP, for TLS on TCP.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/wire"
)

var hubQUICConfig = &quic.Config{
	MaxIdleTimeout:                 30 * time.Second,
	KeepAlivePeriod:                10 * time.Second,
	MaxIncomingStreams:             16,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

type client struct {
	id      identity.ID
	carrier string
	w       *wire.Writer
	remote  func() string // live, so a migrated channel reports the new address
	locals  []string
	since   time.Time
	close   func()

	mu       sync.Mutex
	bytesIn  uint64
	bytesOut uint64
}

func (c *client) candidates() []string {
	out := []string{}
	if c.carrier == "udp" {
		out = append(out, c.remote())
	}
	return append(out, c.locals...)
}

type hub struct {
	key   *identity.Key
	lg    *log.Logger
	start time.Time

	mu      sync.Mutex
	clients map[identity.ID]*client
	dropped uint64
}

func main() {
	keyPath := flag.String("key", "hub.key", "device key file, created when missing")
	udpAddr := flag.String("udp", ":4433", "QUIC listener")
	tcpAddr := flag.String("tcp", "", "TLS listener for UDP-blocked devices, for example :4443")
	httpAddr := flag.String("http", "127.0.0.1:4480", "status page")
	allowFile := flag.String("allow", "", "file of allowed device ids, one per line; empty allows every device")
	flag.Parse()

	lg := log.New(os.Stderr, "", log.Ltime|log.Lmicroseconds)
	key, err := identity.Load(*keyPath)
	if err != nil {
		lg.Fatal(err)
	}
	fmt.Printf("hub id %s\n", key.ID)

	allow := identity.Any
	if *allowFile != "" {
		allow, err = loadAllow(*allowFile)
		if err != nil {
			lg.Fatal(err)
		}
	}

	h := &hub{key: key, lg: lg, start: time.Now(), clients: map[identity.ID]*client{}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	udp, err := net.ListenPacket("udp", *udpAddr)
	if err != nil {
		lg.Fatal(err)
	}
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(key.ServerTLS(allow), hubQUICConfig)
	if err != nil {
		lg.Fatal(err)
	}
	lg.Printf("QUIC on %s", udp.LocalAddr())
	go h.acceptQUIC(ctx, ln)

	if *tcpAddr != "" {
		tl, err := tls.Listen("tcp", *tcpAddr, key.ServerTLS(allow))
		if err != nil {
			lg.Fatal(err)
		}
		lg.Printf("TLS on %s", tl.Addr())
		go h.acceptTCP(ctx, tl)
	}

	if *httpAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/status", h.status)
		srv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				lg.Printf("http: %v", err)
			}
		}()
		lg.Printf("status on http://%s/status", *httpAddr)
	}

	<-ctx.Done()
	_ = ln.Close()
	_ = tr.Close()
}

func loadAllow(path string) (identity.Allow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := map[identity.ID]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, err := identity.ParseID(line)
		if err != nil {
			return nil, err
		}
		set[id] = true
	}
	return func(id identity.ID) bool { return set[id] }, nil
}

func (h *hub) acceptQUIC(ctx context.Context, ln *quic.Listener) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			id, err := identity.FromTLS(conn.ConnectionState().TLS)
			if err != nil {
				_ = conn.CloseWithError(1, "bad identity")
				return
			}
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			st, err := conn.AcceptStream(sctx)
			cancel()
			if err != nil {
				_ = conn.CloseWithError(1, "no channel stream")
				return
			}
			c := &client{
				id: id, carrier: "udp", w: wire.NewWriter(st), since: time.Now(),
				remote: func() string { return conn.RemoteAddr().String() },
				close:  func() { _ = conn.CloseWithError(0, "replaced") },
			}
			h.serve(c, st)
			_ = conn.CloseWithError(0, "bye")
		}()
	}
}

func (h *hub) acceptTCP(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			tc := raw.(*tls.Conn)
			_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = tc.Close()
				return
			}
			_ = tc.SetDeadline(time.Time{})
			id, err := identity.FromTLS(tc.ConnectionState())
			if err != nil {
				_ = tc.Close()
				return
			}
			remote := tc.RemoteAddr().String()
			c := &client{
				id: id, carrier: "tcp", w: wire.NewWriter(tc), since: time.Now(),
				remote: func() string { return remote },
				close:  func() { _ = tc.Close() },
			}
			h.serve(c, tc)
			_ = tc.Close()
		}()
	}
}

func (h *hub) serve(c *client, r io.Reader) {
	h.mu.Lock()
	if old := h.clients[c.id]; old != nil {
		old.close()
	}
	h.clients[c.id] = c
	h.mu.Unlock()
	h.lg.Printf("%s up over %s from %s", c.id.Short(), c.carrier, c.remote())
	defer func() {
		h.mu.Lock()
		if h.clients[c.id] == c {
			delete(h.clients, c.id)
		}
		h.mu.Unlock()
		h.lg.Printf("%s down", c.id.Short())
	}()

	for {
		typ, payload, err := wire.ReadFrame(r)
		if err != nil {
			return
		}
		switch typ {
		case wire.FrameHello:
			var hello wire.Hello
			if err := json.Unmarshal(payload, &hello); err != nil {
				return
			}
			c.locals = hello.Locals
			if err := c.w.JSON(wire.FrameWelcome, wire.Welcome{HubID: h.key.ID.String(), Observed: c.remote(), Carrier: c.carrier, Time: time.Now()}); err != nil {
				return
			}
		case wire.FrameConnect:
			var req wire.Connect
			if err := json.Unmarshal(payload, &req); err != nil {
				return
			}
			peerID, err := identity.ParseID(req.Peer)
			if err != nil {
				_ = c.w.JSON(wire.FrameError, wire.Error{Msg: err.Error()})
				continue
			}
			h.rendezvous(c, peerID)
		case wire.FrameRelay:
			dst, pkt, err := wire.SplitRelay(payload)
			if err != nil {
				return
			}
			h.relay(c, dst, pkt)
		case wire.FramePing:
			_ = c.w.Frame(wire.FramePong, nil)
		case wire.FramePong:
		}
	}
}

func (h *hub) rendezvous(c *client, peerID identity.ID) {
	h.mu.Lock()
	peer := h.clients[peerID]
	h.mu.Unlock()
	if peer == nil {
		_ = c.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: peerID.String(), Dialer: c.id.Less(peerID)})
		return
	}
	h.lg.Printf("rendezvous %s <-> %s", c.id.Short(), peerID.Short())
	_ = c.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: peerID.String(), Addrs: peer.candidates(), Dialer: c.id.Less(peerID), Online: true})
	_ = peer.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: c.id.String(), Addrs: c.candidates(), Dialer: peerID.Less(c.id), Online: true})
}

func (h *hub) relay(src *client, dst identity.ID, pkt []byte) {
	h.mu.Lock()
	peer := h.clients[dst]
	if peer == nil {
		h.dropped++
	}
	h.mu.Unlock()
	if peer == nil {
		return
	}
	if err := peer.w.Relay(src.id, pkt); err != nil {
		return
	}
	n := uint64(len(pkt))
	src.mu.Lock()
	src.bytesIn += n
	src.mu.Unlock()
	peer.mu.Lock()
	peer.bytesOut += n
	peer.mu.Unlock()
}

type clientStatus struct {
	ID       string    `json:"id"`
	Carrier  string    `json:"carrier"`
	Observed string    `json:"observed"`
	Locals   []string  `json:"locals"`
	Since    time.Time `json:"since"`
	BytesIn  uint64    `json:"relayed_bytes_in"`
	BytesOut uint64    `json:"relayed_bytes_out"`
}

func (h *hub) status(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	list := make([]clientStatus, 0, len(h.clients))
	for _, c := range h.clients {
		c.mu.Lock()
		list = append(list, clientStatus{ID: c.id.String(), Carrier: c.carrier, Observed: c.remote(), Locals: c.locals, Since: c.since, BytesIn: c.bytesIn, BytesOut: c.bytesOut})
		c.mu.Unlock()
	}
	dropped := h.dropped
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"hub":            h.key.ID.String(),
		"uptime_seconds": int(time.Since(h.start).Seconds()),
		"clients":        list,
		"dropped":        dropped,
	})
}
