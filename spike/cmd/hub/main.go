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
	EnableDatagrams:                true,
	MaxIncomingStreams:             16,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

// relayQueue bounds what waits for one device: a slow downlink drops
// packets instead of buffering them, so the end-to-end connection sees loss.
const relayQueue = 256

type client struct {
	id      identity.ID
	carrier string
	w       *wire.Writer
	conn    *quic.Conn    // QUIC carrier: relay packets go out as datagrams
	out     chan []byte   // src id || packet, waiting for this device
	remote  func() string // live, so a migrated channel reports the new address
	locals  []string
	since   time.Time
	close   func()

	mu       sync.Mutex
	bytesIn  uint64
	bytesOut uint64
	drops    uint64
}

// writeRelay drains the device's queue as datagrams, or as stream frames over
// TCP or when a packet does not fit in a datagram.
func (c *client) writeRelay(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case b := <-c.out:
			if c.conn != nil {
				err := c.conn.SendDatagram(b)
				if err == nil {
					continue
				}
				// Too large, or the peer has no datagram support: the
				// stream carries this packet instead.
			}
			var id identity.ID
			copy(id[:], b)
			if err := c.w.Relay(id, b[len(id):]); err != nil {
				return
			}
		}
	}
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
	roles   map[[2]identity.ID]identity.ID // pair (sorted) -> the device that dials, fixed at the first request
	dropped uint64
}

func pairKey(a, b identity.ID) [2]identity.ID {
	if b.Less(a) {
		a, b = b, a
	}
	return [2]identity.ID{a, b}
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

	h := &hub{key: key, lg: lg, start: time.Now(), clients: map[identity.ID]*client{}, roles: map[[2]identity.ID]identity.ID{}}
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
			var dg *quic.Conn // datagrams only when the device supports them (older spike agents do not)
			if conn.ConnectionState().SupportsDatagrams.Remote {
				dg = conn
			}
			c := &client{
				id: id, carrier: "udp", w: wire.NewWriter(st), conn: dg, since: time.Now(),
				remote: func() string { return conn.RemoteAddr().String() },
				close:  func() { _ = conn.CloseWithError(0, "replaced") },
			}
			go func() {
				for {
					b, err := conn.ReceiveDatagram(ctx)
					if err != nil {
						return
					}
					dst, pkt, err := wire.SplitRelay(b)
					if err != nil {
						continue
					}
					h.relay(c, dst, pkt)
				}
			}()
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
	c.out = make(chan []byte, relayQueue)
	done := make(chan struct{})
	defer close(done)
	go c.writeRelay(done)
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
			for k := range h.roles {
				if k[0] == c.id || k[1] == c.id {
					delete(h.roles, k)
				}
			}
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

// rendezvous answers both parties. The first device to ask for a pair dials,
// and the role stays until one of the two drops its channel, so a later
// request from the other side never flips it.
func (h *hub) rendezvous(c *client, peerID identity.ID) {
	h.mu.Lock()
	peer := h.clients[peerID]
	k := pairKey(c.id, peerID)
	dialer, ok := h.roles[k]
	if !ok && peer != nil { // a role is only fixed once both sides are here
		dialer = c.id
		h.roles[k] = dialer
	}
	cDials := !ok || dialer == c.id
	h.mu.Unlock()
	if peer == nil {
		_ = c.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: peerID.String(), Dialer: cDials})
		return
	}
	h.lg.Printf("rendezvous %s -> %s (dialer %s)", c.id.Short(), peerID.Short(), map[bool]string{true: c.id.Short(), false: peerID.Short()}[cDials])
	_ = c.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: peerID.String(), Addrs: peer.candidates(), Dialer: cDials, Online: true})
	_ = peer.w.JSON(wire.FrameCandidates, wire.Candidates{Peer: c.id.String(), Addrs: c.candidates(), Dialer: !cDials, Online: true})
}

func (h *hub) relay(src *client, dst identity.ID, pkt []byte) {
	h.mu.Lock()
	peer := h.clients[dst]
	if peer == nil {
		h.dropped++
	}
	h.mu.Unlock()
	if peer == nil || peer.out == nil {
		return
	}
	b := make([]byte, len(src.id)+len(pkt))
	copy(b, src.id[:])
	copy(b[len(src.id):], pkt)
	select {
	case peer.out <- b:
	default:
		peer.mu.Lock()
		peer.drops++
		peer.mu.Unlock()
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
	Drops    uint64    `json:"relay_drops"`
}

func (h *hub) status(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	list := make([]clientStatus, 0, len(h.clients))
	for _, c := range h.clients {
		c.mu.Lock()
		list = append(list, clientStatus{ID: c.id.String(), Carrier: c.carrier, Observed: c.remote(), Locals: c.locals, Since: c.since, BytesIn: c.bytesIn, BytesOut: c.bytesOut, Drops: c.drops})
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
