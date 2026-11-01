// Package portmap asks the home router to forward a UDP port to this host,
// with whichever protocol it speaks: PCP (RFC 6887), NAT-PMP (RFC 6886) or
// UPnP IGD. A mapped port is a candidate any peer can reach, including one
// behind a symmetric NAT, which hole punching cannot serve.
package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/jackpal/gateway"
	natpmp "github.com/jackpal/go-nat-pmp"
)

// Mapping is one forwarded port.
type Mapping struct {
	Proto    string
	External netip.AddrPort
	Lifetime time.Duration
	release  func() error
}

// Release removes the mapping from the router.
func (m *Mapping) Release() error {
	if m == nil || m.release == nil {
		return nil
	}
	return m.release()
}

func (m *Mapping) String() string {
	return fmt.Sprintf("%s %s for %s", m.Proto, m.External, m.Lifetime)
}

// Protocols in the order Map tries them.
var Protocols = []string{"pcp", "natpmp", "upnp"}

// Map tries every protocol in turn and returns the first mapping, with one
// note per failed attempt.
func Map(ctx context.Context, port int, lifetime time.Duration) (*Mapping, []string) {
	var notes []string
	for _, p := range Protocols {
		m, err := MapWith(ctx, p, port, lifetime)
		if err == nil {
			return m, notes
		}
		notes = append(notes, fmt.Sprintf("%s: %v", p, err))
	}
	return nil, notes
}

// MapWith uses one protocol.
func MapWith(ctx context.Context, proto string, port int, lifetime time.Duration) (*Mapping, error) {
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return nil, fmt.Errorf("no default gateway: %w", err)
	}
	switch proto {
	case "pcp":
		return mapPCP(ctx, gw, port, lifetime)
	case "natpmp":
		return mapNATPMP(gw, port, lifetime)
	case "upnp":
		return mapUPnP(ctx, port, lifetime)
	}
	return nil, fmt.Errorf("unknown protocol %q", proto)
}

// ---- PCP ----

func mapPCP(ctx context.Context, gw net.IP, port int, lifetime time.Duration) (*Mapping, error) {
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: gw, Port: 5351})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	client := conn.LocalAddr().(*net.UDPAddr).IP
	var nonce [12]byte
	_, _ = rand.Read(nonce[:])
	req := func(life uint32) []byte {
		b := make([]byte, 60)
		b[0] = 2 // version
		b[1] = 1 // MAP request
		binary.BigEndian.PutUint32(b[4:8], life)
		copy(b[8:24], client.To16())
		copy(b[24:36], nonce[:])
		b[36] = 17 // UDP
		binary.BigEndian.PutUint16(b[40:42], uint16(port))
		binary.BigEndian.PutUint16(b[42:44], uint16(port)) // suggest the same port outside
		copy(b[44:60], net.IPv4zero.To16())
		return b
	}
	resp, err := pcpExchange(ctx, conn, req(uint32(lifetime.Seconds())))
	if err != nil {
		return nil, err
	}
	extPort := binary.BigEndian.Uint16(resp[42:44])
	extIP, _ := netip.AddrFromSlice(resp[44:60])
	life := time.Duration(binary.BigEndian.Uint32(resp[4:8])) * time.Second
	return &Mapping{
		Proto: "pcp", External: netip.AddrPortFrom(extIP.Unmap(), extPort), Lifetime: life,
		release: func() error {
			c2, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: gw, Port: 5351})
			if err != nil {
				return err
			}
			defer c2.Close()
			rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = pcpExchange(rctx, c2, req(0))
			return err
		},
	}, nil
}

var pcpResults = map[byte]string{
	1: "unsupported version", 2: "not authorized", 3: "malformed request", 4: "unsupported opcode",
	5: "unsupported option", 6: "malformed option", 7: "network failure", 8: "no resources",
	9: "unsupported protocol", 10: "user exceeded quota", 11: "cannot provide external", 12: "address mismatch",
	13: "excessive remote peers",
}

func pcpExchange(ctx context.Context, conn *net.UDPConn, req []byte) ([]byte, error) {
	buf := make([]byte, 1100)
	wait := 250 * time.Millisecond
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		n, err := conn.Read(buf)
		if err == nil {
			if n < 60 || buf[1] != 0x81 {
				if n >= 4 && buf[0] == 0 {
					return nil, errors.New("router answered as NAT-PMP only")
				}
				return nil, fmt.Errorf("unexpected answer of %d bytes", n)
			}
			if code := buf[3]; code != 0 {
				return nil, fmt.Errorf("refused: %s (%d)", pcpResults[code], code)
			}
			return buf[:n], nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		wait *= 2
	}
	return nil, errors.New("no answer on port 5351")
}

// ---- NAT-PMP ----

func mapNATPMP(gw net.IP, port int, lifetime time.Duration) (*Mapping, error) {
	c := natpmp.NewClientWithTimeout(gw, 2*time.Second)
	ext, err := c.GetExternalAddress()
	if err != nil {
		return nil, err
	}
	res, err := c.AddPortMapping("udp", port, port, int(lifetime.Seconds()))
	if err != nil {
		return nil, err
	}
	ip := netip.AddrFrom4(ext.ExternalIPAddress)
	return &Mapping{
		Proto: "natpmp", External: netip.AddrPortFrom(ip, res.MappedExternalPort),
		Lifetime: time.Duration(res.PortMappingLifetimeInSeconds) * time.Second,
		release: func() error {
			_, err := c.AddPortMapping("udp", port, 0, 0)
			return err
		},
	}, nil
}

// ---- UPnP IGD ----

type igd interface {
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	AddPortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string, intPort uint16, intClient string, enabled bool, desc string, lease uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, extPort uint16, proto string) error
}

func mapUPnP(ctx context.Context, port int, lifetime time.Duration) (*Mapping, error) {
	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var clients []igd
	if cs, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(dctx); err == nil {
		for _, c := range cs {
			clients = append(clients, c)
		}
	}
	if cs, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(dctx); err == nil {
		for _, c := range cs {
			clients = append(clients, c)
		}
	}
	if cs, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(dctx); err == nil {
		for _, c := range cs {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		return nil, errors.New("no UPnP internet gateway answered")
	}
	local, err := gateway.DiscoverInterface()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, c := range clients {
		extStr, err := c.GetExternalIPAddressCtx(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		ext, err := netip.ParseAddr(extStr)
		if err != nil {
			lastErr = fmt.Errorf("external address %q: %w", extStr, err)
			continue
		}
		for _, extPort := range []int{port, port + 1, port + 2} {
			err = c.AddPortMappingCtx(ctx, "", uint16(extPort), "UDP", uint16(port), local.String(), true, "redundanet spike", uint32(lifetime.Seconds()))
			if err != nil {
				lastErr = err
				continue
			}
			c, ep := c, uint16(extPort)
			return &Mapping{
				Proto: "upnp", External: netip.AddrPortFrom(ext, ep), Lifetime: lifetime,
				release: func() error {
					rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					return c.DeletePortMappingCtx(rctx, "", ep, "UDP")
				},
			}, nil
		}
	}
	return nil, lastErr
}
