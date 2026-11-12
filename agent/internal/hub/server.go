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
	// OnTaskReport receives a repair worker's word on a task.
	OnTaskReport func(peer identity.Peer, r *wire.TaskReport)
	// OnRevocation receives a member's signed device revocation a device
	// delivered, verified; the hub's membership keeps it and answers
	// whether it was new, and only a new one is passed on to the other
	// devices. Revocations is what a device that connects is sent, so one
	// that was offline learns what it missed.
	OnRevocation func(peer identity.Peer, r *identity.SignedRevocation) bool
	Revocations  func() []*identity.SignedRevocation
	Log          *slog.Logger
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

// controlQueue bounds the control frames that wait for one device. They
// are few (candidates, pongs, a repair task now and then), so a full queue
// means the device is not reading, and the hub closes its channel.
const controlQueue = 64

// writeTimeout is how long one write to a device may take. A device that
// does not take a frame in that time loses its channel and connects again.
// A variable so tests need not wait for it.
var writeTimeout = 10 * time.Second

// ErrNotReading is the answer for a device whose control queue is full.
var ErrNotReading = errors.New("hub: the device is not reading its channel")

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

// client is one connected device. Nothing but its own goroutines ever
// waits on its socket: every other part of the hub hands it a frame with
// enqueue, which never blocks, so a device that stops reading costs only
// itself.
type controlFrame struct {
	typ     byte
	payload []byte
}

type client struct {
	peer     identity.Peer
	carrier  string
	w        *wire.Writer
	deadline func(time.Time) error // the write deadline of what w writes to
	conn     *quic.Conn            // datagrams, nil over TCP
	out      chan []byte           // relayed packets
	ctl      chan controlFrame     // control frames, written before relayed packets
	remote   func() string
	locals   []string
	since    time.Time
	close    func()

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
			c := &client{peer: *peer, carrier: "udp", w: wire.NewWriter(st), deadline: st.SetWriteDeadline, conn: dg, since: time.Now(),
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
			c := &client{peer: *peer, carrier: "tcp", w: wire.NewWriter(tc), deadline: tc.SetWriteDeadline, since: time.Now(),
				remote: func() string { return remote }, close: func() { _ = tc.Close() }}
			s.serve(ctx, c, tc)
			_ = tc.Close()
		}()
	}
}

func (s *Server) serve(ctx context.Context, c *client, r io.Reader) {
	c.out = make(chan []byte, relayQueue)
	c.ctl = make(chan controlFrame, controlQueue)
	done := make(chan struct{})
	defer close(done)
	go c.writeLoop(done)

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
	c.locals = wire.Addrs(hello.Locals, wire.MaxCandidates)
	c.mu.Unlock()
	if err := c.writeMessage(wire.FrameWelcome, wire.Welcome{Observed: c.remote(), Carrier: c.carrier, Time: time.Now()}); err != nil {
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
	if s.cfg.Revocations != nil {
		// Written here and not queued: the list can be longer than the
		// queue, and this goroutine waits on no socket but its own device's.
		for _, r := range s.cfg.Revocations() {
			if err := c.writeMessage(wire.FrameRevocation, r); err != nil {
				c.close()
				break
			}
		}
	}
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
			c.locals = wire.Addrs(hello.Locals, wire.MaxCandidates)
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
		case wire.FrameRevocation:
			rv, err := identity.DecodeRevocation(payload)
			if err != nil {
				return
			}
			if s.cfg.OnRevocation == nil || !s.cfg.OnRevocation(c.peer, rv) {
				continue
			}
			s.mu.Lock()
			others := make([]*client, 0, len(s.clients))
			for _, o := range s.clients {
				if o != c {
					others = append(others, o)
				}
			}
			s.mu.Unlock()
			for _, o := range others {
				_ = o.enqueueMessage(wire.FrameRevocation, rv)
			}
		case wire.FrameTaskReport:
			var r wire.TaskReport
			if err := identity.UnmarshalLenient(payload, &r); err != nil {
				return
			}
			if s.cfg.OnTaskReport != nil {
				s.cfg.OnTaskReport(c.peer, &r)
			}
		case wire.FramePing:
			_ = c.enqueue(wire.FramePong, nil)
		case wire.FramePong:
		}
	}
}

// ErrNoChannel is Send's answer for a device without a channel right now.
var ErrNoChannel = errors.New("hub: the device has no channel")

// Send queues a control frame for a device on its channel: how the
// collector hands a repair task or a release hint to a storage node. It
// never waits for the device, so it is safe under the caller's own locks;
// nil means queued, not received.
func (s *Server) Send(device identity.Key, typ byte, v any) error {
	s.mu.Lock()
	c := s.clients[device]
	s.mu.Unlock()
	if c == nil {
		return ErrNoChannel
	}
	return c.enqueueMessage(typ, v)
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

// write puts one frame on the device's socket within writeTimeout. Only
// the device's own two goroutines call it: serve, before the device is
// registered and for the revocation list, and writeLoop.
func (c *client) write(typ byte, payload []byte) error {
	if c.deadline != nil {
		_ = c.deadline(time.Now().Add(writeTimeout))
	}
	return c.w.Frame(typ, payload)
}

func (c *client) writeMessage(typ byte, v any) error {
	b, err := identity.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(typ, b)
}

// enqueue hands the device a control frame without waiting for it. A full
// queue closes the device's channel: it has not read for a long while.
func (c *client) enqueue(typ byte, payload []byte) error {
	select {
	case c.ctl <- controlFrame{typ, payload}:
		return nil
	default:
		c.close()
		return ErrNotReading
	}
}

func (c *client) enqueueMessage(typ byte, v any) error {
	b, err := identity.Marshal(v)
	if err != nil {
		return err
	}
	return c.enqueue(typ, b)
}

// writeLoop is the one writer of a registered device: control frames
// first, then relayed packets. A write that fails or runs out of time
// closes the whole connection, since a frame may be half written.
func (c *client) writeLoop(done <-chan struct{}) {
	control := func(f controlFrame) bool {
		if err := c.write(f.typ, f.payload); err != nil {
			c.close()
			return false
		}
		return true
	}
	for {
		select {
		case <-done:
			return
		case f := <-c.ctl:
			if !control(f) {
				return
			}
			continue
		default:
		}
		select {
		case <-done:
			return
		case f := <-c.ctl:
			if !control(f) {
				return
			}
		case b := <-c.out:
			if c.conn != nil && c.conn.SendDatagram(b) == nil {
				continue
			}
			if err := c.write(wire.FrameRelay, b); err != nil {
				c.close()
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
		_ = c.enqueueMessage(wire.FrameCandidates, wire.Candidates{Peer: peerID, Dialer: cDials})
		return
	}
	_ = c.enqueueMessage(wire.FrameCandidates, wire.Candidates{Peer: peerID, Addrs: peer.candidates(), Dialer: cDials, Online: true})
	_ = peer.enqueueMessage(wire.FrameCandidates, wire.Candidates{Peer: c.peer.Device, Addrs: c.candidates(), Dialer: !cDials, Online: true})
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
