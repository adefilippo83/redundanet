// Package session keeps one QUIC connection per peer and moves it between the
// relay and a direct path. The hub names the dialer: the device that asked for
// the rendezvous, or the smaller ID when both asked; the other side only
// accepts. Direct paths are found by punching the candidates the hub hands
// out; the dialer migrates the live connection onto a confirmed one with
// quic-go's path API and migrates back when the keepalives stop.
package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/spike/internal/hubclient"
	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/mux"
	"github.com/adefilippo83/redundanet/spike/internal/wire"
)

// Path names.
const (
	PathNone   = ""
	PathRelay  = "relay"
	PathDirect = "direct"
)

const (
	punchRounds     = 5
	punchInterval   = 300 * time.Millisecond
	rendezvousEvery = 15 * time.Second
	keepaliveEvery  = 2 * time.Second
	directSilence   = 6 * time.Second
	probeTimeout    = 5 * time.Second
	dialTimeout     = 10 * time.Second
	maxEvents       = 100

	// A session nobody here asked for (the hub announced the peer) is dropped
	// after this long without a connection, so it stops asking the hub.
	idleSession = 60 * time.Second

	// quic-go 0.64 reserves one of the peer's connection IDs for every path
	// the accepting side validates and never frees the one it switches to,
	// so with the default budget of four a connection migrates twice and
	// the third probe starves. Past that the dialer reconnects instead.
	maxMigrations = 2
)

// peerQUICConfig: notice a dead peer in 20 s, keep NAT mappings warm, and give
// a single stream enough window for a fast link.
//
// Inner packets stay at 1200 bytes so that one fits, with the 32-byte peer
// ID, in a single datagram on the hub connection.
var peerQUICConfig = &quic.Config{
	InitialPacketSize:              1200,
	DisablePathMTUDiscovery:        true,
	MaxIdleTimeout:                 20 * time.Second,
	KeepAlivePeriod:                5 * time.Second,
	MaxIncomingStreams:             1024,
	InitialStreamReceiveWindow:     4 << 20,
	MaxStreamReceiveWindow:         32 << 20,
	InitialConnectionReceiveWindow: 8 << 20,
	MaxConnectionReceiveWindow:     64 << 20,
}

// Event is one line of a session's history, timestamped for the measurements.
type Event struct {
	Time time.Time `json:"time"`
	What string    `json:"what"`
}

// Status is a session snapshot.
type Status struct {
	Peer        string    `json:"peer"`
	Dialer      bool      `json:"dialer"`
	Connected   bool      `json:"connected"`
	Active      string    `json:"active"`
	RemoteAddr  string    `json:"remote_addr,omitempty"`
	DirectAddr  string    `json:"direct_addr,omitempty"`
	Candidates  []string  `json:"candidates,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	ConnectedAt time.Time `json:"connected_at,omitempty"`
	DirectAt    time.Time `json:"direct_at,omitempty"`
	Events      []Event   `json:"events"`
}

// Manager owns the listener and the sessions.
type Manager struct {
	Key      *identity.Key
	Mux      *mux.Mux
	Tr       *quic.Transport // the default view's transport
	Hub      *hubclient.Client
	Allow    identity.Allow
	NoDirect bool
	OnStream func(peer identity.ID, s *quic.Stream)
	Log      *log.Logger

	ctx      context.Context
	ln       *quic.Listener
	mu       sync.Mutex
	sessions map[identity.ID]*Session
}

// Start opens the listener and installs the punch handler.
func (m *Manager) Start(ctx context.Context) error {
	ln, err := m.Tr.Listen(m.Key.ServerTLS(m.Allow), peerQUICConfig)
	if err != nil {
		return err
	}
	m.ctx = ctx
	m.ln = ln
	m.sessions = map[identity.ID]*Session{}
	m.Mux.SetPunchHandler(m.onPunch)
	go m.acceptLoop()
	return nil
}

func (m *Manager) acceptLoop() {
	for {
		conn, err := m.ln.Accept(m.ctx)
		if err != nil {
			if m.ctx.Err() == nil {
				m.Log.Printf("listener: %v", err)
			}
			return
		}
		id, err := identity.FromTLS(conn.ConnectionState().TLS)
		if err != nil {
			_ = conn.CloseWithError(1, "bad identity")
			continue
		}
		s := m.session(id, true)
		s.setConn(conn, kindOf(conn.RemoteAddr()))
		go s.serveConn(conn)
		go s.watchRemote(conn)
	}
}

func kindOf(a net.Addr) string {
	if wire.IsRelayAddr(a) {
		return PathRelay
	}
	return PathDirect
}

// session returns the session for peer, creating it when create is set.
func (m *Manager) session(peer identity.ID, create bool) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[peer]
	if s == nil && create {
		ctx, cancel := context.WithCancel(m.ctx)
		s = &Session{mgr: m, peer: peer, ctx: ctx, cancel: cancel, createdAt: time.Now()}
		m.sessions[peer] = s
		s.event("session created")
		go s.rendezvousLoop()
	}
	return s
}

// Connect starts (or returns) the session with peer and marks it wanted.
func (m *Manager) Connect(peer identity.ID) *Session {
	s := m.session(peer, true)
	s.mu.Lock()
	s.wanted = true
	s.mu.Unlock()
	return s
}

func (m *Manager) drop(s *Session) {
	m.mu.Lock()
	if m.sessions[s.peer] == s {
		delete(m.sessions, s.peer)
	}
	m.mu.Unlock()
	s.cancel()
	s.mu.Lock()
	if s.relayView != nil {
		_ = s.relayView.Close()
	}
	if s.directView != nil {
		_ = s.directView.Close()
	}
	s.mu.Unlock()
	s.event("session dropped: idle")
}

// HubUp repeats the rendezvous for every session that still needs one; the
// hub client calls it whenever the channel comes up.
func (m *Manager) HubUp() {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	for _, s := range list {
		s.mu.Lock()
		need := s.conn == nil || (s.active != PathDirect && !m.NoDirect)
		s.mu.Unlock()
		if need {
			_ = m.Hub.Connect(s.peer)
		}
	}
}

// OnCandidates is the hub client's callback.
func (m *Manager) OnCandidates(c wire.Candidates) {
	peer, err := identity.ParseID(c.Peer)
	if err != nil {
		m.Log.Printf("hub: candidates for a bad id: %v", err)
		return
	}
	s := m.session(peer, true)
	s.mu.Lock()
	s.candidates = c.Addrs
	first := !s.roleKnown || s.dialer != c.Dialer
	s.roleKnown = true
	s.dialer = c.Dialer
	s.online = c.Online
	conn, dialing := s.conn, s.dialing
	// A listener must not hold views: they would claim the dialer's packets
	// and keep them from the listener on the default view.
	if !c.Dialer && conn == nil && !dialing {
		if s.relayView != nil {
			_ = s.relayView.Close()
			s.relayView, s.relayTr = nil, nil
		}
		if s.directView != nil {
			_ = s.directView.Close()
			s.directView, s.directTr = nil, nil
		}
	}
	s.mu.Unlock()
	if !c.Online {
		s.event("hub: peer is offline")
		return
	}
	if first {
		s.event("hub: I am the %s", map[bool]string{true: "dialer", false: "listener"}[c.Dialer])
	}
	if c.Dialer && conn == nil && !dialing {
		go s.dialRelay()
	}
	if !m.NoDirect {
		go s.punch(c.Addrs)
	}
}

func (m *Manager) onPunch(from identity.ID, addr *net.UDPAddr, kind byte, _ uint32) {
	s := m.session(from, false)
	if s == nil {
		return
	}
	fresh := kind == mux.PunchOpen || kind == mux.PunchAck
	s.mu.Lock()
	s.lastAck = time.Now()
	key := addr.String()
	newAddr := fresh && !s.confirmed[key]
	if newAddr {
		if s.confirmed == nil {
			s.confirmed = map[string]bool{}
		}
		s.confirmed[key] = true
	}
	// Keep the best confirmed address: a LAN neighbour beats global IPv6,
	// which beats the public IPv4 mapping; overlay networks come last.
	better := fresh && (s.directAddr == nil || rank(addr) < rank(s.directAddr))
	if better {
		s.directAddr = addr
	}
	dialer, conn, active, migrating, best := s.dialer, s.conn, s.active, s.migrating, s.directAddr
	s.mu.Unlock()
	if newAddr {
		s.event("direct candidate confirmed: %s (%s)", addr, rankName(addr))
	}
	// Only a fresh punch round moves the connection, onto the best address
	// confirmed so far: a late keepalive answer after a fallback must not
	// bounce it straight back.
	if fresh && dialer && conn != nil && active == PathRelay && !migrating && !m.NoDirect {
		go s.toDirect(best)
	}
}

// OpenStream opens a stream to peer on whatever path is active, waiting for a
// connection first.
func (m *Manager) OpenStream(ctx context.Context, peer identity.ID) (*quic.Stream, string, error) {
	s := m.Connect(peer)
	for {
		s.mu.Lock()
		conn, active := s.conn, s.active
		s.mu.Unlock()
		if conn != nil {
			st, err := conn.OpenStreamSync(ctx)
			return st, active, err
		}
		select {
		case <-ctx.Done():
			return nil, PathNone, fmt.Errorf("no connection to %s: %w", peer.Short(), ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Switch forces the dialer onto a path; the bench uses it.
func (m *Manager) Switch(peer identity.ID, path string) error {
	s := m.session(peer, false)
	if s == nil {
		return errors.New("no session")
	}
	s.mu.Lock()
	dialer, conn, active, addr := s.dialer, s.conn, s.active, s.directAddr
	s.mu.Unlock()
	if !dialer {
		return errors.New("only the dialer can switch paths")
	}
	if conn == nil {
		return errors.New("not connected")
	}
	switch path {
	case PathDirect:
		if active == PathDirect {
			return nil
		}
		if addr == nil {
			return errors.New("no direct candidate confirmed yet")
		}
		return s.toDirect(addr)
	case PathRelay:
		if active == PathRelay {
			return nil
		}
		return s.toRelay()
	}
	return fmt.Errorf("unknown path %q", path)
}

// DirectAddr is the confirmed direct address of peer, if any.
func (m *Manager) DirectAddr(peer identity.ID) string {
	s := m.session(peer, false)
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directAddr == nil {
		return ""
	}
	return s.directAddr.String()
}

// Active reports the path in use with peer.
func (m *Manager) Active(peer identity.ID) string {
	s := m.session(peer, false)
	if s == nil {
		return PathNone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Status snapshots every session.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	out := make([]Status, 0, len(list))
	for _, s := range list {
		out = append(out, s.status())
	}
	return out
}

// Session is the state with one peer.
type Session struct {
	mgr    *Manager
	peer   identity.ID
	dialer bool
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	wanted      bool // asked for locally, never dropped
	lastConn    time.Time
	roleKnown   bool // the hub has said who dials
	online      bool // the hub's last word on whether the peer has a channel
	confirmed   map[string]bool
	conn        *quic.Conn
	active      string
	directAddr  *net.UDPAddr
	lastAck     time.Time
	candidates  []string
	relayView   *mux.View
	directView  *mux.View
	relayTr     *quic.Transport
	directTr    *quic.Transport
	oldPath     *quic.Path // the path we left, closed once the switch is done
	migrations  int        // switches done on the current connection
	dialing     bool
	migrating   bool
	keepSeq     uint32
	events      []Event
	createdAt   time.Time
	connectedAt time.Time
	directAt    time.Time
}

func (s *Session) event(format string, args ...any) {
	e := Event{Time: time.Now(), What: fmt.Sprintf(format, args...)}
	s.mgr.Log.Printf("peer %s: %s", s.peer.Short(), e.What)
	s.mu.Lock()
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	s.mu.Unlock()
}

func (s *Session) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Peer: s.peer.String(), Dialer: s.dialer, Connected: s.conn != nil, Active: s.active,
		Candidates: s.candidates, CreatedAt: s.createdAt, ConnectedAt: s.connectedAt, DirectAt: s.directAt,
		Events: append([]Event(nil), s.events...),
	}
	if s.conn != nil {
		st.RemoteAddr = s.conn.RemoteAddr().String()
	}
	if s.directAddr != nil {
		st.DirectAddr = s.directAddr.String()
	}
	return st
}

func (s *Session) setConn(conn *quic.Conn, kind string) {
	s.mu.Lock()
	s.conn = conn
	s.active = kind
	s.migrating = false
	s.migrations = 0
	s.oldPath = nil
	if s.connectedAt.IsZero() {
		s.connectedAt = time.Now()
	}
	if kind == PathDirect && s.directAt.IsZero() {
		s.directAt = time.Now()
	}
	s.mu.Unlock()
	s.event("connected via %s (%s)", kind, conn.RemoteAddr())
}

// rendezvousLoop asks the hub for candidates until a direct path is up, and
// keeps the dialer dialing while there is no connection.
func (s *Session) rendezvousLoop() {
	for {
		s.mu.Lock()
		conn, active, dialing, online := s.conn, s.active, s.dialing, s.online
		s.mu.Unlock()
		wait := rendezvousEvery
		s.mu.Lock()
		if conn != nil {
			s.lastConn = time.Now()
		}
		idle := !s.wanted && conn == nil && time.Since(s.lastConn) > idleSession && time.Since(s.createdAt) > idleSession
		s.mu.Unlock()
		if idle {
			s.mgr.drop(s)
			return
		}
		if conn == nil || (active != PathDirect && !s.mgr.NoDirect) {
			if err := s.mgr.Hub.Connect(s.peer); err != nil {
				s.event("rendezvous: %v", err)
				wait = 2 * time.Second
			}
		}
		if s.dialer && online && conn == nil && !dialing {
			go s.dialRelay()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *Session) punch(addrs []string) {
	targets := make([]*net.UDPAddr, 0, len(addrs))
	for _, a := range addrs {
		u, err := net.ResolveUDPAddr("udp", a)
		if err != nil || wire.IsRelayAddr(u) {
			continue
		}
		targets = append(targets, u)
	}
	if len(targets) == 0 {
		return
	}
	s.event("punching %d candidates", len(targets))
	for i := 0; i < punchRounds; i++ {
		for _, t := range targets {
			_ = s.mgr.Mux.SendPunch(t, mux.PunchOpen, uint32(i))
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(punchInterval):
		}
	}
}

func (s *Session) dialRelay() {
	s.mu.Lock()
	if s.dialing || s.conn != nil {
		s.mu.Unlock()
		return
	}
	s.dialing = true
	if s.relayView == nil {
		s.relayView = s.mgr.Mux.NewRelayView(s.peer)
		s.relayTr = &quic.Transport{Conn: s.relayView}
	}
	tr := s.relayTr
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	started := time.Now()
	conn, err := tr.Dial(ctx, wire.RelayAddr(s.peer), s.mgr.Key.ClientTLS(s.peer), peerQUICConfig)
	s.mu.Lock()
	s.dialing = false
	s.mu.Unlock()
	if err != nil {
		s.event("relay dial failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		return
	}
	s.event("relay handshake took %s", time.Since(started).Round(time.Millisecond))
	s.setConn(conn, PathRelay)
	go s.serveConn(conn)
}

// serveConn accepts the peer's streams and notices the connection's end.
func (s *Session) serveConn(conn *quic.Conn) {
	go func() {
		for {
			st, err := conn.AcceptStream(s.ctx)
			if err != nil {
				return
			}
			go s.mgr.OnStream(s.peer, st)
		}
	}()
	<-conn.Context().Done()
	s.mu.Lock()
	mine := s.conn == conn
	if mine {
		s.conn = nil
		s.active = PathNone
		s.migrating = false
		s.directAddr = nil
	}
	s.mu.Unlock()
	if !mine {
		return
	}
	s.event("connection closed: %v", context.Cause(conn.Context()))
	if s.dialer {
		time.Sleep(time.Second)
		go s.dialRelay()
	}
}

// watchRemote lets the accepting side record the dialer's migrations.
func (s *Session) watchRemote(conn *quic.Conn) {
	last := conn.RemoteAddr().String()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-conn.Context().Done():
			return
		case <-t.C:
		}
		now := conn.RemoteAddr().String()
		if now == last {
			continue
		}
		last = now
		kind := kindOf(conn.RemoteAddr())
		s.mu.Lock()
		if s.conn == conn {
			s.active = kind
			if kind == PathDirect && s.directAt.IsZero() {
				s.directAt = time.Now()
			}
		}
		s.mu.Unlock()
		s.event("peer moved to %s (%s)", kind, now)
	}
}

// toDirect migrates the dialer's connection onto addr.
func (s *Session) toDirect(addr *net.UDPAddr) error {
	s.mu.Lock()
	if s.migrating || s.conn == nil || s.active == PathDirect {
		s.mu.Unlock()
		return nil
	}
	s.migrating = true
	conn := s.conn
	if s.directView == nil {
		s.directView = s.mgr.Mux.NewDirectView(s.peer)
		s.directTr = &quic.Transport{Conn: s.directView}
	}
	s.directView.SetTarget(addr)
	tr := s.directTr
	budgetLeft := s.migrations < maxMigrations
	s.mu.Unlock()

	if !budgetLeft {
		return s.redial(PathDirect, tr, addr)
	}
	done := func(err error) error {
		s.mu.Lock()
		s.migrating = false
		s.mu.Unlock()
		return err
	}
	started := time.Now()
	path, err := conn.AddPath(tr)
	if err != nil {
		s.event("direct: add path: %v", err)
		return done(err)
	}
	ctx, cancel := context.WithTimeout(s.ctx, probeTimeout)
	defer cancel()
	if err := path.Probe(ctx); err != nil {
		s.event("direct: probe of %s failed after %s: %v", addr, time.Since(started).Round(time.Millisecond), err)
		_ = path.Close()
		return done(err)
	}
	if err := path.Switch(); err != nil {
		s.event("direct: switch: %v", err)
		return done(err)
	}
	s.mu.Lock()
	s.active = PathDirect
	if s.directAt.IsZero() {
		s.directAt = time.Now()
	}
	old := s.oldPath
	s.oldPath = path
	s.lastAck = time.Now()
	s.migrating = false
	s.migrations++
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	s.event("switched to direct %s (probe+switch %s)", addr, time.Since(started).Round(time.Millisecond))
	go s.keepalive(conn, addr)
	return nil
}

// keepalive probes the direct path and falls back when it goes silent.
func (s *Session) keepalive(conn *quic.Conn, addr *net.UDPAddr) {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-conn.Context().Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.conn != conn || s.active != PathDirect {
			s.mu.Unlock()
			return
		}
		s.keepSeq++
		seq := s.keepSeq
		silent := time.Since(s.lastAck)
		s.mu.Unlock()
		if silent > directSilence {
			s.event("direct path silent for %s", silent.Round(time.Millisecond))
			if err := s.toRelay(); err != nil {
				s.event("fallback to relay failed: %v", err)
			}
			return
		}
		_ = s.mgr.Mux.SendPunch(addr, mux.PunchKeepalive, seq)
	}
}

// toRelay migrates the dialer's connection back onto the relay.
func (s *Session) toRelay() error {
	s.mu.Lock()
	if s.migrating || s.conn == nil || s.active == PathRelay {
		s.mu.Unlock()
		return nil
	}
	s.migrating = true
	conn, tr := s.conn, s.relayTr
	budgetLeft := s.migrations < maxMigrations
	s.mu.Unlock()

	if !budgetLeft {
		return s.redial(PathRelay, tr, nil)
	}
	started := time.Now()
	path, err := conn.AddPath(tr)
	if err == nil {
		ctx, cancel := context.WithTimeout(s.ctx, probeTimeout)
		err = path.Probe(ctx)
		cancel()
		if err != nil {
			_ = path.Close()
		}
	}
	if err == nil {
		err = path.Switch()
	}
	if err != nil {
		s.event("relay: migration back failed (%v); reconnecting", err)
		s.mu.Lock()
		s.migrating = false
		s.mu.Unlock()
		_ = conn.CloseWithError(2, "path lost")
		return err
	}
	s.mu.Lock()
	s.active = PathRelay
	old := s.oldPath
	s.oldPath = path
	s.migrating = false
	s.migrations++
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	s.event("switched back to relay (probe+switch %s)", time.Since(started).Round(time.Millisecond))
	return nil
}

// redial opens a fresh connection on the wanted path and swaps it in; the
// streams on the old connection break, which is the weaker property RFC 0001
// accepts (resume within seconds at segment granularity).
func (s *Session) redial(kind string, tr *quic.Transport, addr *net.UDPAddr) error {
	done := func(err error) error {
		s.mu.Lock()
		s.migrating = false
		s.mu.Unlock()
		return err
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	conn, err := tr.Dial(ctx, wire.RelayAddr(s.peer), s.mgr.Key.ClientTLS(s.peer), peerQUICConfig)
	if err != nil {
		s.event("%s: reconnect failed after %s: %v", kind, time.Since(started).Round(time.Millisecond), err)
		return done(err)
	}
	s.mu.Lock()
	old := s.conn
	s.conn = conn
	s.active = kind
	s.migrating = false
	s.migrations = 0
	s.oldPath = nil
	s.lastAck = time.Now()
	if kind == PathDirect && s.directAt.IsZero() {
		s.directAt = time.Now()
	}
	s.mu.Unlock()
	if old != nil {
		_ = old.CloseWithError(3, "path change")
	}
	s.event("reconnected via %s (%s) in %s: migration budget was spent", kind, conn.RemoteAddr(), time.Since(started).Round(time.Millisecond))
	go s.serveConn(conn)
	if kind == PathDirect {
		go s.keepalive(conn, addr)
	}
	return nil
}

// rank orders confirmed addresses, lower is better.
func rank(a *net.UDPAddr) int {
	ip := a.IP
	switch {
	case ip.IsLoopback():
		return 0
	case ip.To4() != nil && isCGNAT(ip):
		return 5 // Tailscale and carrier-grade ranges: an overlay or a shared NAT
	case ip.To4() != nil && ip.IsPrivate():
		return 1 // same LAN
	case ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate():
		return 2 // global IPv6, end to end
	case ip.To4() != nil:
		return 3 // public IPv4, the punched mapping
	default:
		return 4 // IPv6 ULA, usually an overlay
	}
}

func rankName(a *net.UDPAddr) string {
	return [...]string{"loopback", "lan", "ipv6", "ipv4", "ula", "overlay"}[rank(a)]
}

func isCGNAT(ip net.IP) bool {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n.Contains(ip)
}
