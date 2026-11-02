// Package hub is the hub mode's rendezvous and relay: it reflects each
// device's public address, introduces pairs, and forwards end-to-end QUIC
// packets it cannot read. It admits only devices the membership policy
// allows, and it never holds a key or a share.
package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Config wires a hub.
type Config struct {
	Identity *identity.TLSIdentity
	Device   identity.Key
	Allow    identity.Allow
	UDP      string // QUIC listener, for example ":4433"
	TCP      string // TLS listener for UDP-blocked devices, "" for none
	// OnCensus receives each device's heartbeat; the collector keeps them.
	OnCensus func(peer identity.Peer, carrier string, c wire.Census)
	// OnShares receives a storage node's share census chunks.
	OnShares func(peer identity.Peer, c *wire.ShareCensus)
	Log      *slog.Logger
}

var quicConfig = &quic.Config{
	MaxIdleTimeout:                 30 * time.Second,
	KeepAlivePeriod:                10 * time.Second,
	EnableDatagrams:                true,
	MaxIncomingStreams:             16,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

// relayQueue bounds what waits for one device; a slow downlink drops
// packets instead of buffering them.
const relayQueue = 256

// Server is a running hub.
type Server struct {
	cfg   Config
	log   *slog.Logger
	udp   net.PacketConn
	tr    *quic.Transport
	ln    *quic.Listener
	tcpLn net.Listener
	start time.Time

	mu      sync.Mutex
	clients map[identity.Key]*client
	roles   map[[2]identity.Key]identity.Key // pair -> dialer, fixed at the first request
	dropped uint64
}

type client struct {
	peer    identity.Peer
	carrier string
	w       *wire.Writer
	conn    *quic.Conn // datagrams, nil over TCP
	out     chan []byte
	remote  func() string
	locals  []string
	since   time.Time
	close   func()

	mu       sync.Mutex
	bytesIn  uint64
	bytesOut uint64
	drops    uint64
}

// Start listens and serves until ctx ends or Close is called.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Identity == nil || cfg.Allow == nil {
		return nil, errors.New("hub: identity and allow policy are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.UDP == "" {
		cfg.UDP = ":4433"
	}
	udp, err := net.ListenPacket("udp", cfg.UDP)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: cfg.Log.With("component", "hub"), udp: udp, start: time.Now(), clients: map[identity.Key]*client{}, roles: map[[2]identity.Key]identity.Key{}}
	s.tr = &quic.Transport{Conn: udp}
	s.ln, err = s.tr.Listen(cfg.Identity.ServerConfig(cfg.Allow), quicConfig)
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	go s.acceptQUIC(ctx)
	if cfg.TCP != "" {
		s.tcpLn, err = tls.Listen("tcp", cfg.TCP, cfg.Identity.ServerConfig(cfg.Allow))
		if err != nil {
			_ = s.ln.Close()
			_ = udp.Close()
			return nil, err
		}
		go s.acceptTCP(ctx)
	}
	s.log.Info("hub up", "udp", udp.LocalAddr(), "tcp", cfg.TCP, "device", cfg.Device.Short())
	return s, nil
}

// Close tells every device to reconnect now and stops the listeners. Closing
// the listeners alone would leave established connections to die at the
// idle timeout, half a minute a self-update's candidate cannot wait.
func (s *Server) Close() error {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.close()
	}
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	_ = s.ln.Close()
	err := s.tr.Close()
	// quic-go closes only sockets it opened itself; this one is ours, and a
	// self-update's candidate binds the same port right after.
	_ = s.udp.Close()
	return err
}

// UDPAddr is the QUIC listener's address.
func (s *Server) UDPAddr() *net.UDPAddr { return s.udp.LocalAddr().(*net.UDPAddr) }

// TCPAddr is the TLS listener's address, nil without one.
func (s *Server) TCPAddr() net.Addr {
	if s.tcpLn == nil {
		return nil
	}
	return s.tcpLn.Addr()
}

func (s *Server) acceptQUIC(ctx context.Context) {
	for {
		conn, err := s.ln.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			peer, err := identity.PeerFromConnectionState(conn.ConnectionState().TLS)
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
			var dg *quic.Conn
			if conn.ConnectionState().SupportsDatagrams.Remote {
				dg = conn
			}
			c := &client{peer: *peer, carrier: "udp", w: wire.NewWriter(st), conn: dg, since: time.Now(),
				remote: func() string { return conn.RemoteAddr().String() },
				close:  func() { _ = conn.CloseWithError(0, "replaced") }}
			if dg != nil {
				go func() {
					for {
						b, err := conn.ReceiveDatagram(ctx)
						if err != nil {
							return
						}
						if dst, pkt, err := wire.SplitRelay(b); err == nil {
							s.relay(c, dst, pkt)
						}
					}
				}()
			}
			s.serve(ctx, c, st)
			_ = conn.CloseWithError(0, "bye")
		}()
	}
}

func (s *Server) acceptTCP(ctx context.Context) {
	for {
		raw, err := s.tcpLn.Accept()
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
			peer, err := identity.PeerFromConnectionState(tc.ConnectionState())
			if err != nil {
				_ = tc.Close()
				return
			}
			remote := tc.RemoteAddr().String()
			c := &client{peer: *peer, carrier: "tcp", w: wire.NewWriter(tc), since: time.Now(),
				remote: func() string { return remote }, close: func() { _ = tc.Close() }}
			s.serve(ctx, c, tc)
			_ = tc.Close()
		}()
	}
}

func (s *Server) serve(ctx context.Context, c *client, r io.Reader) {
	c.out = make(chan []byte, relayQueue)
	done := make(chan struct{})
	defer close(done)
	go c.writeRelay(done)

	// Hello and Welcome come first, before the device is registered: once
	// it is, a peer's rendezvous may write to it at any moment, and the
	// first frame a device reads must be its Welcome.
	typ, payload, err := wire.ReadFrame(r)
	if err != nil || typ != wire.FrameHello {
		return
	}
	var hello wire.Hello
	if err := identity.UnmarshalLenient(payload, &hello); err != nil {
		return
	}
	c.mu.Lock()
	c.locals = hello.Locals
	c.mu.Unlock()
	if err := c.w.Message(wire.FrameWelcome, wire.Welcome{Observed: c.remote(), Carrier: c.carrier, Time: time.Now()}); err != nil {
		return
	}

	id := c.peer.Device
	s.mu.Lock()
	if old := s.clients[id]; old != nil {
		old.close()
	}
	s.clients[id] = c
	s.mu.Unlock()
	s.log.Info("device up", "device", id.Short(), "carrier", c.carrier, "from", c.remote())
	defer func() {
		s.mu.Lock()
		if s.clients[id] == c {
			delete(s.clients, id)
			for k := range s.roles {
				if k[0] == id || k[1] == id {
					delete(s.roles, k)
				}
			}
		}
		s.mu.Unlock()
		s.log.Info("device down", "device", id.Short())
	}()

	for {
		typ, payload, err := wire.ReadFrame(r)
		if err != nil {
			return
		}
		switch typ {
		case wire.FrameHello:
			// A second Hello refreshes the candidates.
			var hello wire.Hello
			if err := identity.UnmarshalLenient(payload, &hello); err != nil {
				return
			}
			c.mu.Lock()
			c.locals = hello.Locals
			c.mu.Unlock()
		case wire.FrameConnect:
			var req wire.Connect
			if err := identity.UnmarshalLenient(payload, &req); err != nil {
				return
			}
			s.rendezvous(c, req.Peer)
		case wire.FrameRelay:
			if dst, pkt, err := wire.SplitRelay(payload); err == nil {
				s.relay(c, dst, pkt)
			}
		case wire.FrameCensus:
			var cs wire.Census
			if err := identity.UnmarshalLenient(payload, &cs); err != nil {
				return
			}
			if s.cfg.OnCensus != nil {
				s.cfg.OnCensus(c.peer, c.carrier, cs)
			}
		case wire.FrameShares:
			var sc wire.ShareCensus
			if err := identity.UnmarshalLenient(payload, &sc); err != nil {
				return
			}
			if s.cfg.OnShares != nil {
				s.cfg.OnShares(c.peer, &sc)
			}
		case wire.FramePing:
			_ = c.w.Frame(wire.FramePong, nil)
		case wire.FramePong:
		}
	}
}

func (c *client) candidates() []string {
	out := []string{}
	if c.carrier == "udp" {
		out = append(out, c.remote())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append(out, c.locals...)
}

func (c *client) writeRelay(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case b := <-c.out:
			if c.conn != nil && c.conn.SendDatagram(b) == nil {
				continue
			}
			if err := c.w.Frame(wire.FrameRelay, b); err != nil {
				return
			}
		}
	}
}

func pairKey(a, b identity.Key) [2]identity.Key {
	if b.Less(a) {
		a, b = b, a
	}
	return [2]identity.Key{a, b}
}

// rendezvous answers both parties. The first device to ask for a pair
// dials, and the role holds until one of the two drops its channel.
func (s *Server) rendezvous(c *client, peerID identity.Key) {
	s.mu.Lock()
	peer := s.clients[peerID]
	k := pairKey(c.peer.Device, peerID)
	dialer, ok := s.roles[k]
	if !ok && peer != nil {
		dialer = c.peer.Device
		s.roles[k] = dialer
	}
	cDials := !ok || dialer == c.peer.Device
	s.mu.Unlock()
	if peer == nil {
		_ = c.w.Message(wire.FrameCandidates, wire.Candidates{Peer: peerID, Dialer: cDials})
		return
	}
	_ = c.w.Message(wire.FrameCandidates, wire.Candidates{Peer: peerID, Addrs: peer.candidates(), Dialer: cDials, Online: true})
	_ = peer.w.Message(wire.FrameCandidates, wire.Candidates{Peer: c.peer.Device, Addrs: c.candidates(), Dialer: !cDials, Online: true})
}

func (s *Server) relay(src *client, dst identity.Key, pkt []byte) {
	s.mu.Lock()
	peer := s.clients[dst]
	if peer == nil {
		s.dropped++
	}
	s.mu.Unlock()
	if peer == nil {
		return
	}
	select {
	case peer.out <- wire.RelayPayload(src.peer.Device, pkt):
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

// ClientStatus is one connected device.
type ClientStatus struct {
	Device   string    `json:"device"`
	Member   string    `json:"member"`
	Carrier  string    `json:"carrier"`
	Observed string    `json:"observed"`
	Since    time.Time `json:"since"`
	BytesIn  uint64    `json:"relayed_bytes_in"`
	BytesOut uint64    `json:"relayed_bytes_out"`
	Drops    uint64    `json:"relay_drops"`
}

// Status is the hub's snapshot.
type Status struct {
	Device  string         `json:"device"`
	Uptime  time.Duration  `json:"uptime"`
	Clients []ClientStatus `json:"clients"`
	Dropped uint64         `json:"dropped_for_offline_peers"`
}

// Status snapshots the hub.
func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Device: s.cfg.Device.String(), Uptime: time.Since(s.start), Dropped: s.dropped}
	for _, c := range s.clients {
		c.mu.Lock()
		st.Clients = append(st.Clients, ClientStatus{Device: c.peer.Device.String(), Member: c.peer.Member.String(), Carrier: c.carrier, Observed: c.remote(), Since: c.since, BytesIn: c.bytesIn, BytesOut: c.bytesOut, Drops: c.drops})
		c.mu.Unlock()
	}
	return st
}
