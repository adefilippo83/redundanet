package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

const (
	punchRounds     = 5
	punchInterval   = 300 * time.Millisecond
	rendezvousEvery = 15 * time.Second
	dialTimeout     = 10 * time.Second
	idleSession     = 60 * time.Second // a session nobody here asked for is dropped after this long without a connection
	maxEvents       = 50
)

// Event is one line of a session's history.
type Event struct {
	Time time.Time `json:"time"`
	What string    `json:"what"`
}

// SessionStatus is a session snapshot.
type SessionStatus struct {
	Peer       string    `json:"peer"`
	Member     string    `json:"member,omitempty"`
	Dialer     bool      `json:"dialer"`
	Relay      bool      `json:"relay"`
	Direct     string    `json:"direct,omitempty"`
	Candidates []string  `json:"candidates,omitempty"`
	Events     []Event   `json:"events"`
	Since      time.Time `json:"since"`
}

// Session is the public handle on a session.
type Session struct{ *session }

// Best reports the path new streams would take: "relay", "direct" or "".
func (s *Session) Best() string {
	_, path := s.best()
	return path
}

type session struct {
	ep   *Endpoint
	peer identity.Key
	ctx  context.Context
	stop context.CancelFunc

	mu         sync.Mutex
	member     identity.Key
	peerInfo   identity.Peer // the verified peer, once a handshake has completed
	wanted     bool
	roleKnown  bool
	dialer     bool
	online     bool
	hub        *hubClient
	cands      []string
	seen       map[string]bool
	direct     *net.UDPAddr // best confirmed direct address
	relayConn  *quic.Conn
	directConn *quic.Conn
	dialing    map[string]bool
	events     []Event
	created    time.Time
	lastConn   time.Time
}

func newSession(e *Endpoint, peer identity.Key) *session {
	ctx, cancel := context.WithCancel(e.ctx)
	s := &session{ep: e, peer: peer, ctx: ctx, stop: cancel, seen: map[string]bool{}, dialing: map[string]bool{}, created: time.Now()}
	go s.loop()
	return s
}

func (s *session) event(format string, args ...any) {
	ev := Event{Time: time.Now(), What: fmt.Sprintf(format, args...)}
	s.ep.log.Info("peer "+s.peer.Short(), "event", ev.What)
	s.mu.Lock()
	s.events = append(s.events, ev)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	s.mu.Unlock()
}

func (s *session) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SessionStatus{Peer: s.peer.String(), Dialer: s.dialer, Relay: s.relayConn != nil, Candidates: s.cands, Events: append([]Event(nil), s.events...), Since: s.created}
	if !s.member.IsZero() {
		st.Member = s.member.String()
	}
	if s.directConn != nil {
		st.Direct = s.directConn.RemoteAddr().String()
	}
	return st
}

func (s *session) setPeer(p identity.Peer) {
	s.mu.Lock()
	s.member = p.Member
	s.peerInfo = p
	s.mu.Unlock()
}

// best is the connection new streams use: direct when it is alive.
func (s *session) best() (*quic.Conn, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directConn != nil && s.directConn.Context().Err() == nil {
		return s.directConn, PathDirect
	}
	if s.relayConn != nil && s.relayConn.Context().Err() == nil {
		return s.relayConn, PathRelay
	}
	return nil, ""
}

func (s *session) wantsRendezvous() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.directConn == nil || s.relayConn == nil
}

// setConn files a connection, direct or relayed, replacing one of the same
// kind, and serves its streams until it ends.
func (s *session) setConn(conn *quic.Conn, path string) {
	s.mu.Lock()
	var old *quic.Conn
	if path == PathDirect {
		old, s.directConn = s.directConn, conn
	} else {
		old, s.relayConn = s.relayConn, conn
	}
	s.lastConn = time.Now()
	s.mu.Unlock()
	if old != nil && old != conn {
		_ = old.CloseWithError(0, "replaced")
	}
	s.event("%s connection up (%s)", path, conn.RemoteAddr())
	go s.serve(conn, path)
}

func (s *session) serve(conn *quic.Conn, path string) {
	go func() {
		for {
			st, err := conn.AcceptStream(s.ctx)
			if err != nil {
				return
			}
			if s.ep.cfg.OnStream != nil {
				s.mu.Lock()
				peer := identity.Peer{Device: s.peer, Member: s.member}
				s.mu.Unlock()
				go s.ep.cfg.OnStream(peer, path, st)
			} else {
				st.CancelRead(0)
				st.CancelWrite(0)
			}
		}
	}()
	<-conn.Context().Done()
	s.mu.Lock()
	mine := false
	if path == PathDirect && s.directConn == conn {
		s.directConn, mine = nil, true
		s.direct = nil
		s.seen = map[string]bool{}
	}
	if path == PathRelay && s.relayConn == conn {
		s.relayConn, mine = nil, true
	}
	s.mu.Unlock()
	if mine {
		s.event("%s connection closed: %v", path, context.Cause(conn.Context()))
	}
}

// loop keeps the rendezvous going while something is missing and drops a
// session nobody asked for once it has been idle.
func (s *session) loop() {
	for {
		s.mu.Lock()
		wanted, hasAny := s.wanted, s.relayConn != nil || s.directConn != nil
		idle := !wanted && !hasAny && time.Since(s.lastConn) > idleSession && time.Since(s.created) > idleSession
		s.mu.Unlock()
		if idle {
			s.ep.dropSession(s)
			s.stop()
			return
		}
		wait := rendezvousEvery
		if s.wantsRendezvous() {
			if err := s.ep.rendezvous(s.peer); err != nil {
				wait = 2 * time.Second
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// candidates is the hub's answer to a rendezvous.
func (s *session) candidates(c *hubClient, cand wire.Candidates) {
	s.mu.Lock()
	s.hub = c
	s.cands = cand.Addrs
	s.online = cand.Online
	changed := !s.roleKnown || s.dialer != cand.Dialer
	s.roleKnown, s.dialer = true, cand.Dialer
	dialer, relayUp := s.dialer, s.relayConn != nil
	s.mu.Unlock()
	if !cand.Online {
		return
	}
	if changed {
		role := "listener"
		if dialer {
			role = "dialer"
		}
		s.event("hub says I am the %s", role)
	}
	if dialer && !relayUp {
		go s.dialRelay(c)
	}
	go s.punch(cand.Addrs)
}

func (s *session) punch(addrs []string) {
	var targets []*net.UDPAddr
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
	for i := 0; i < punchRounds; i++ {
		for _, t := range targets {
			s.ep.sendPunch(t, punchOpen, uint64(i))
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(punchInterval):
		}
	}
}

// confirmed records a direct address a punch or its answer came from, and
// has the dialer dial it when it is the best one so far.
func (s *session) confirmed(addr *net.UDPAddr) {
	s.mu.Lock()
	key := addr.String()
	fresh := !s.seen[key]
	s.seen[key] = true
	better := s.direct == nil || rank(addr) < rank(s.direct)
	if better {
		s.direct = addr
	}
	dialer, hasDirect, dialing := s.dialer, s.directConn != nil, s.dialing[PathDirect]
	s.mu.Unlock()
	if fresh {
		s.event("direct candidate confirmed: %s (%s)", addr, rankName(addr))
	}
	if dialer && !hasDirect && !dialing && better {
		go s.dialDirect(addr)
	}
}

func (s *session) dialRelay(c *hubClient) {
	s.mu.Lock()
	if s.dialing[PathRelay] || s.relayConn != nil {
		s.mu.Unlock()
		return
	}
	s.dialing[PathRelay] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.dialing[PathRelay] = false; s.mu.Unlock() }()

	c.relay.register(s.peer)
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	started := time.Now()
	conn, err := c.relayTr.Dial(ctx, wire.RelayAddr(s.peer), s.ep.cfg.Identity.ClientConfig(s.peer, s.ep.cfg.Allow), relayedQUICConfig)
	if err != nil {
		s.event("relay dial failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		return
	}
	s.event("relay handshake took %s", time.Since(started).Round(time.Millisecond))
	if p, err := identity.PeerFromConnectionState(conn.ConnectionState().TLS); err == nil {
		s.setPeer(*p)
	}
	s.setConn(conn, PathRelay)
}

func (s *session) dialDirect(addr *net.UDPAddr) {
	s.mu.Lock()
	if s.dialing[PathDirect] || s.directConn != nil {
		s.mu.Unlock()
		return
	}
	s.dialing[PathDirect] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.dialing[PathDirect] = false; s.mu.Unlock() }()

	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	started := time.Now()
	conn, err := s.ep.tr.Dial(ctx, addr, s.ep.cfg.Identity.ClientConfig(s.peer, s.ep.cfg.Allow), peerQUICConfig)
	if err != nil {
		s.event("direct dial of %s failed after %s: %v", addr, time.Since(started).Round(time.Millisecond), err)
		return
	}
	s.event("direct handshake with %s took %s", addr, time.Since(started).Round(time.Millisecond))
	s.setConn(conn, PathDirect)
}

// rank orders confirmed addresses, lower is better: a LAN neighbour, then
// global IPv6, then the public IPv4 mapping; overlays never get here.
func rank(a *net.UDPAddr) int {
	ip := a.IP
	switch {
	case ip.IsLoopback():
		return 0
	case ip.To4() != nil && ip.IsPrivate():
		return 1
	case ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate():
		return 2
	case ip.To4() != nil:
		return 3
	default:
		return 4
	}
}

func rankName(a *net.UDPAddr) string {
	return [...]string{"loopback", "lan", "ipv6", "ipv4", "ula"}[rank(a)]
}
