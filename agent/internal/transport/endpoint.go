// Package transport is the userspace mesh of RFC 0001: one UDP socket, QUIC
// with the device key as the TLS identity, the hub as rendezvous and relay,
// a direct path when the NATs allow one.
//
// A session with a peer holds up to two QUIC connections, one through the
// relay and one direct. New streams open on the best live connection;
// streams already open stay on theirs until they end, so a path that dies
// takes its streams with it and the request layer retries on the other.
// That is the resume-within-seconds property the RFC asks for, without QUIC
// path migration, which quic-go cannot do off a dead path (see the G0
// decision record).
//
// quic-go owns the raw socket, so it keeps its batching and buffer sizing;
// punch packets, the only non-QUIC traffic, come back through its
// ReadNonQUICPacket. Relayed connections live on a second, virtual packet
// conn fed by the hub channel.
package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/portmap"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Path kinds.
const (
	PathRelay  = "relay"
	PathDirect = "direct"
)

// Config wires an endpoint.
type Config struct {
	Identity *identity.TLSIdentity
	Device   identity.Key
	Allow    identity.Allow // the membership policy; every connection passes it
	Hubs     []HubAddr
	Listen   string // UDP address to bind, default ":0"
	// UDPPort is a port the member forwards on their router to this device;
	// advertised with the hub-observed address as a candidate.
	UDPPort int
	// PortMap asks the router for a mapping with PCP, NAT-PMP or UPnP.
	PortMap bool
	// TCPOnly never tries UDP to a hub (a network that blocks it).
	TCPOnly bool
	// LoopbackCandidates advertises 127.0.0.1 too, for tests on one machine.
	LoopbackCandidates bool
	// OnStream handles a stream a peer opened.
	OnStream func(peer identity.Peer, path string, s wire.Stream)
	// Census, when set, supplies the device-level fields of the heartbeat
	// sent to each hub; the endpoint adds the peers itself.
	Census func() wire.Census
	// ShareCensus, when set, supplies a storage node's share census since
	// a generation; the endpoint sends it to each hub in chunks whenever
	// the generation moved.
	ShareCensus func(since uint64) (*wire.ShareCensus, error)
	// OnRevocation receives a member's signed device revocation a hub
	// sent, verified; the membership applies and keeps it.
	OnRevocation func(r *identity.SignedRevocation)
	// OnTask receives a repair task a hub sent; report answers that hub on
	// the same channel. It must not block: the worker queues the task.
	OnTask func(t *wire.RepairTask, report func(*wire.TaskReport) error)
	// OnRelease receives a hub's hint that this node holds a surplus share.
	OnRelease func(h *wire.ReleaseHint)
	Log       *slog.Logger
}

// Endpoint is one device's presence on the mesh.
type Endpoint struct {
	cfg Config
	log *slog.Logger
	udp *net.UDPConn
	tr  *quic.Transport
	ln  *quic.Listener

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	hubs     []*hubClient
	sessions map[identity.Key]*session
	mapped   []string
}

// peerQUICConfig: notice a dead peer in 20 s and keep NAT mappings warm.
var peerQUICConfig = &quic.Config{
	MaxIdleTimeout:                 20 * time.Second,
	KeepAlivePeriod:                5 * time.Second,
	MaxIncomingStreams:             1024,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

// relayedQUICConfig keeps packets small enough for one hub datagram.
var relayedQUICConfig = func() *quic.Config {
	c := *peerQUICConfig
	c.InitialPacketSize = 1200
	c.DisablePathMTUDiscovery = true
	return &c
}()

// Start binds the socket, connects to the hubs and listens for peers.
func Start(ctx context.Context, cfg Config) (*Endpoint, error) {
	if cfg.Identity == nil || cfg.Allow == nil {
		return nil, errors.New("transport: identity and allow policy are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Listen == "" {
		cfg.Listen = ":0"
	}
	addr, err := net.ResolveUDPAddr("udp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	ep := &Endpoint{cfg: cfg, log: cfg.Log.With("component", "transport"), udp: udp, ctx: ctx, cancel: cancel, sessions: map[identity.Key]*session{}}
	ep.tr = &quic.Transport{Conn: udp}
	ln, err := ep.tr.Listen(cfg.Identity.ServerConfig(cfg.Allow), peerQUICConfig)
	if err != nil {
		cancel()
		_ = udp.Close()
		return nil, err
	}
	ep.ln = ln
	go ep.acceptLoop(ctx, ln, PathDirect)
	go ep.punchLoop(ctx)
	if cfg.PortMap {
		go ep.portMap(ctx)
	}
	for _, h := range cfg.Hubs {
		c := &hubClient{ep: ep, hub: h}
		ep.hubs = append(ep.hubs, c)
		go c.run(ctx)
	}
	ep.log.Info("endpoint up", "udp", udp.LocalAddr(), "device", cfg.Device.Short())
	return ep, nil
}

// Close stops everything, telling each hub goodbye first so its view of
// this device is current at once.
func (e *Endpoint) Close() error {
	e.cancel()
	e.mu.Lock()
	hubs := append([]*hubClient(nil), e.hubs...)
	e.mu.Unlock()
	for _, h := range hubs {
		h.goodbye()
	}
	_ = e.ln.Close()
	err := e.tr.Close()
	// quic-go closes only sockets it opened itself; this one is ours, and a
	// device with a fixed forwarded port must free it for the update's
	// candidate.
	_ = e.udp.Close()
	return err
}

// LocalAddr is the UDP socket's address.
func (e *Endpoint) LocalAddr() *net.UDPAddr { return e.udp.LocalAddr().(*net.UDPAddr) }

// Connect starts, or returns, the session with a peer and marks it wanted.
// ErrSelf answers OpenStream for this device's own key. A device does not
// dial itself: a session to oneself files both ends of the same connection
// and replaces one with the other for ever. Its own server is reached in
// process by the caller (the share protocol's LocalOpener).
var ErrSelf = errors.New("transport: this device does not open streams to itself")

func (e *Endpoint) Connect(peer identity.Key) *Session {
	if peer == e.cfg.Device {
		// No session and no rendezvous with oneself; a handle that reports
		// no path, for callers that connect to every server in a manifest.
		return &Session{&session{ep: e, peer: peer, ctx: e.ctx, stop: func() {}, seen: map[string]bool{}, dialing: map[string]bool{}, created: time.Now()}}
	}
	s := e.session(peer, true)
	s.mu.Lock()
	s.wanted = true
	s.mu.Unlock()
	return &Session{s}
}

// ErrNoPath answers OpenStream for a peer that has had no connection for
// some time and gained none in the short wait a request gives it.
var ErrNoPath = errors.New("transport: the peer has not been reachable for some time")

// OpenStream opens a stream to peer on the best live path, waiting for a
// connection first: up to the context's deadline for a peer that was
// connected until a moment ago or was only just asked for, and a short
// moment for one that has been out of reach for a while, so that one
// server that is off does not cost every request its whole deadline.
func (e *Endpoint) OpenStream(ctx context.Context, peer identity.Key) (wire.Stream, string, error) {
	if peer == e.cfg.Device {
		return nil, "", ErrSelf
	}
	s := e.Connect(peer).session
	start := time.Now()
	for {
		if conn, path := s.best(); conn != nil {
			st, err := conn.OpenStreamSync(ctx)
			if err == nil {
				return st, path, nil
			}
			if ctx.Err() != nil {
				return nil, "", err
			}
			// That connection just died; the loop picks the other one.
		} else if time.Since(start) > noPathWait && s.noPath() {
			return nil, "", fmt.Errorf("no connection to %s: %w", peer.Short(), ErrNoPath)
		}
		select {
		case <-ctx.Done():
			return nil, "", fmt.Errorf("no connection to %s: %w", peer.Short(), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// SendRevocation hands a signed device revocation to every hub whose
// channel is up, which keeps it and spreads it; the count is how many
// took it.
func (e *Endpoint) SendRevocation(r *identity.SignedRevocation) int {
	e.mu.Lock()
	hubs := append([]*hubClient(nil), e.hubs...)
	e.mu.Unlock()
	n := 0
	for _, h := range hubs {
		if h.sendMessage(wire.FrameRevocation, r) == nil {
			n++
		}
	}
	return n
}

// Recheck runs the allow policy over every session's peer again and closes
// the connections of those it no longer admits: what happens when a newly
// signed manifest or a revocation arrives.
func (e *Endpoint) Recheck() {
	e.mu.Lock()
	list := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		list = append(list, s)
	}
	e.mu.Unlock()
	for _, s := range list {
		s.mu.Lock()
		peer := s.peerInfo
		conns := []*quic.Conn{s.relayConn, s.directConn}
		s.mu.Unlock()
		if peer.Certificate == nil {
			continue
		}
		if err := e.cfg.Allow(peer); err != nil {
			s.event("no longer admitted: %v", err)
			for _, c := range conns {
				if c != nil {
					_ = c.CloseWithError(4, "no longer admitted")
				}
			}
		}
	}
}

// Status is a snapshot for the local page.
type Status struct {
	Device   string          `json:"device"`
	UDP      string          `json:"udp"`
	Hubs     []HubStatus     `json:"hubs"`
	Sessions []SessionStatus `json:"sessions"`
	Mapped   []string        `json:"mapped,omitempty"`
}

// Status snapshots the endpoint.
func (e *Endpoint) Status() Status {
	e.mu.Lock()
	hubs := append([]*hubClient(nil), e.hubs...)
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	mapped := append([]string(nil), e.mapped...)
	e.mu.Unlock()
	st := Status{Device: e.cfg.Device.String(), UDP: e.udp.LocalAddr().String(), Mapped: mapped}
	for _, h := range hubs {
		st.Hubs = append(st.Hubs, h.status())
	}
	for _, s := range sessions {
		st.Sessions = append(st.Sessions, s.status())
	}
	return st
}

// acceptLoop takes connections from a listener, direct or relayed, and
// files them under the peer's session.
func (e *Endpoint) acceptLoop(ctx context.Context, ln *quic.Listener, path string) {
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		peer, err := identity.PeerFromConnectionState(conn.ConnectionState().TLS)
		if err != nil {
			_ = conn.CloseWithError(1, "bad identity")
			continue
		}
		s := e.session(peer.Device, true)
		s.setPeer(*peer)
		s.setConn(conn, path)
	}
}

// punch packets: magic, kind, sender, nonce. Unauthenticated; they only
// open NAT mappings, the QUIC handshake proves who is at an address. An
// open is answered with an ack that echoes its nonce, and only an ack that
// echoes a nonce this device sent confirms the address it came from.
const punchMagic = "\x00RN3PNCH"

const (
	punchOpen byte = 0
	punchAck  byte = 1
)

const punchLen = len(punchMagic) + 1 + 32 + 8

func (e *Endpoint) sendPunch(to *net.UDPAddr, kind byte, nonce uint64) {
	b := make([]byte, punchLen)
	copy(b, punchMagic)
	b[len(punchMagic)] = kind
	copy(b[len(punchMagic)+1:], e.cfg.Device[:])
	binary.BigEndian.PutUint64(b[len(punchMagic)+33:], nonce)
	_, _ = e.tr.WriteTo(b, to)
}

func (e *Endpoint) punchLoop(ctx context.Context) {
	buf := make([]byte, 2048)
	for {
		n, addr, err := e.tr.ReadNonQUICPacket(ctx, buf)
		if err != nil {
			return
		}
		if n != punchLen || string(buf[:len(punchMagic)]) != punchMagic {
			continue
		}
		udp, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		kind := buf[len(punchMagic)]
		var from identity.Key
		copy(from[:], buf[len(punchMagic)+1:])
		nonce := binary.BigEndian.Uint64(buf[len(punchMagic)+33:])
		switch kind {
		case punchOpen:
			e.sendPunch(udp, punchAck, nonce)
		case punchAck:
			if s := e.session(from, false); s != nil {
				s.acked(udp, nonce)
			}
		}
	}
}

// localCandidates lists this device's own addresses with the socket's port,
// plus any router mapping. Overlay networks (Tailscale, carrier-grade
// ranges) are left out: their relay must not pass for a direct path.
func (e *Endpoint) localCandidates() []string {
	port := e.LocalAddr().Port
	var out []string
	e.mu.Lock()
	out = append(out, e.mapped...)
	e.mu.Unlock()
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() && !e.cfg.LoopbackCandidates {
				continue
			}
			if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || isOverlay(ip) {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}
	return out
}

var overlayNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

func isOverlay(ip net.IP) bool {
	for _, n := range overlayNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// portMap asks the router for a mapping and keeps it alive.
func (e *Endpoint) portMap(ctx context.Context) {
	port := e.LocalAddr().Port
	var m *portmap.Mapping
	for {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if m == nil {
			var notes []string
			m, notes = portmap.Map(mctx, port, time.Hour)
			for _, n := range notes {
				e.log.Info("portmap", "note", n)
			}
		} else if r, err := portmap.MapWith(mctx, m.Proto, port, time.Hour); err != nil {
			e.log.Warn("portmap refresh failed", "err", err)
		} else {
			m = r
		}
		cancel()
		if m != nil {
			e.mu.Lock()
			e.mapped = []string{m.External.String()}
			e.mu.Unlock()
			e.log.Info("portmap", "mapping", m.String())
		}
		wait := 10 * time.Minute
		if m != nil && m.Lifetime > 0 {
			wait = m.Lifetime / 2
		}
		select {
		case <-ctx.Done():
			if m != nil {
				_ = m.Release()
			}
			return
		case <-time.After(wait):
		}
	}
}

// onHubUp repeats pending rendezvous requests on a fresh channel and adds
// the forwarded-port candidate now that the public address is known.
func (e *Endpoint) onHubUp(c *hubClient) {
	if e.cfg.UDPPort != 0 {
		if host, _, err := net.SplitHostPort(c.status().Observed); err == nil {
			cand := net.JoinHostPort(host, strconv.Itoa(e.cfg.UDPPort))
			e.mu.Lock()
			found := false
			for _, m := range e.mapped {
				found = found || m == cand
			}
			if !found {
				e.mapped = append(e.mapped, cand)
			}
			e.mu.Unlock()
		}
	}
	e.mu.Lock()
	list := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		list = append(list, s)
	}
	e.mu.Unlock()
	for _, s := range list {
		if s.wantsRendezvous() {
			_ = c.sendMessage(wire.FrameConnect, wire.Connect{Peer: s.peer})
		}
	}
}

func (e *Endpoint) onCandidates(c *hubClient, cand wire.Candidates) {
	s := e.session(cand.Peer, true)
	s.candidates(c, cand)
}

// rendezvous asks the first hub that is up for a peer's candidates.
func (e *Endpoint) rendezvous(peer identity.Key) error {
	e.mu.Lock()
	hubs := append([]*hubClient(nil), e.hubs...)
	e.mu.Unlock()
	lastErr := errors.New("no hub channel is up")
	for _, h := range hubs {
		err := h.sendMessage(wire.FrameConnect, wire.Connect{Peer: peer})
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func (e *Endpoint) session(peer identity.Key, create bool) *session {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[peer]
	if s == nil && create {
		s = newSession(e, peer)
		e.sessions[peer] = s
	}
	return s
}

func (e *Endpoint) dropSession(s *session) {
	e.mu.Lock()
	if e.sessions[s.peer] == s {
		delete(e.sessions, s.peer)
	}
	e.mu.Unlock()
}
