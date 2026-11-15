package hub

import (
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The NAT probe's hub side (wire/probe.go): a second UDP port from which
// the hub knocks on a device's channel address, and on which it tells a
// device the address its ask came from. It answers only a device that has
// a channel and sends that channel's token.

const (
	knocks     = 3
	knockEvery = 200 * time.Millisecond
	// A device sends a handful of asks when its channel comes up and up
	// to a dozen for each port search later, to read its router's counter
	// around the packets it writes: probeAnswers at once, one more every
	// probeRefill. An answer is as long as its ask, so the bound is
	// on the hub's work and not on what it could be made to send.
	probeAnswers = 64
	probeRefill  = time.Second
	// seekBurst and seekRefill bound the port searches a hub passes on
	// for one device.
	seekBurst  = 16
	seekRefill = 2 * time.Second
)

// listenProbe opens the probe port. A hub that cannot have it runs
// without the probe: devices then learn nothing about their routers and
// everything else works as before.
func (s *Server) listenProbe(addr string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.log.Warn("no NAT probe: the second UDP port cannot be opened", "addr", addr, "err", err)
		return
	}
	ap, err := netip.ParseAddrPort(pc.LocalAddr().String())
	if err != nil || ap.Port() == 0 {
		_ = pc.Close()
		return
	}
	s.probe, s.probePort = pc, ap.Port()
	s.log.Info("NAT probe up", "udp", pc.LocalAddr())
	go s.probeLoop(pc)
}

func probeToken() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0
	}
	return binary.BigEndian.Uint64(b[:]) | 1 // never zero: zero means no probe
}

// knock sends the unasked packets to the address a device's channel
// comes from. The address is that of an authenticated connection.
func (s *Server) knock(c *client) {
	to, err := netip.ParseAddrPort(c.remote())
	if err != nil || s.probe == nil {
		return
	}
	b := wire.Probe{Kind: wire.ProbeKnock, Device: s.cfg.Device, Token: c.probeToken}.Encode()
	for i := 0; i < knocks; i++ {
		_, _ = s.probe.WriteTo(b, net.UDPAddrFromAddrPort(to))
		time.Sleep(knockEvery)
	}
}

func (s *Server) probeLoop(pc net.PacketConn) {
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		p, ok := wire.ParseProbe(buf[:n])
		if !ok || p.Kind != wire.ProbeAsk {
			continue
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		s.mu.Lock()
		c := s.clients[p.Device]
		s.mu.Unlock()
		if c == nil || c.probeToken == 0 || c.probeToken != p.Token {
			continue
		}
		c.mu.Lock()
		ok = c.probed.Take(time.Now())
		c.mu.Unlock()
		if !ok {
			continue
		}
		seen := udp.AddrPort()
		tell := wire.Probe{Kind: wire.ProbeTell, Device: s.cfg.Device, Token: p.Token, Addr: netip.AddrPortFrom(seen.Addr().Unmap(), seen.Port())}
		_, _ = pc.WriteTo(tell.Encode(), from)
	}
}

// ProbeAddr is where the NAT probe listens, nil without one.
func (s *Server) ProbeAddr() net.Addr {
	if s.probe == nil {
		return nil
	}
	return s.probe.LocalAddr()
}
