// Command natprobe classifies the NAT in front of a host, RFC 5780 style:
// how it maps a socket to a public address and port, and which inbound packets
// it lets through. It is the first number the spike needs, because the exit
// criterion is a direct path for the NAT types the live members have.
//
// Run it on the host, not in a container. It needs UDP out to the STUN
// servers and nothing else.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pion/stun/v3"
)

// Report is one host's result.
type Report struct {
	Host      string    `json:"host"`
	Arch      string    `json:"arch"`
	Time      time.Time `json:"time"`
	Local     string    `json:"local_addr"`
	Observed  []string  `json:"observed"` // mapped address per simple server
	Mapping   string    `json:"mapping"`
	Filtering string    `json:"filtering"`
	Server    string    `json:"rfc5780_server,omitempty"`
	IPv6      string    `json:"ipv6"`
	Notes     []string  `json:"notes,omitempty"`
}

var errTimeout = errors.New("no answer")

func main() {
	simple := flag.String("simple", "stun.l.google.com:19302,stun.cloudflare.com:3478", "servers for the mapped-address comparison")
	full := flag.String("servers", "stun.sipgate.net:3478,stun.voipgate.com:3478,stun.nextcloud.com:3478", "RFC 5780 servers to try, first that answers with OTHER-ADDRESS wins")
	v6 := flag.String("ipv6", "stun.l.google.com:19302", "server to reach over IPv6")
	timeout := flag.Duration("timeout", 2*time.Second, "per request")
	asJSON := flag.Bool("json", false, "print JSON only")
	flag.Parse()

	r := Report{Arch: runtime.GOOS + "/" + runtime.GOARCH, Time: time.Now(), Mapping: "unknown", Filtering: "unknown", IPv6: "none"}
	r.Host, _ = os.Hostname()

	// Mapped address from two unrelated servers through one socket: equal
	// means endpoint-independent mapping, the case hole punching needs.
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		fatal(err)
	}
	r.Local = conn.LocalAddr().String()
	for _, srv := range split(*simple) {
		resp, err := request(conn, srv, nil, *timeout)
		if err != nil {
			r.Notes = append(r.Notes, fmt.Sprintf("%s: %v", srv, err))
			continue
		}
		if xa := mapped(resp); xa != nil {
			r.Observed = append(r.Observed, xa.String())
		}
	}
	switch {
	case len(r.Observed) == 0:
		r.Notes = append(r.Notes, "no STUN server answered over IPv4")
	case allEqual(r.Observed) && sameHost(r.Observed[0], localIPs()):
		r.Mapping = "no NAT"
	case allEqual(r.Observed):
		r.Mapping = "endpoint independent"
	default:
		r.Mapping = "address dependent or worse"
	}
	_ = conn.Close()

	// RFC 5780 tests refine mapping and add filtering.
	for _, srv := range split(*full) {
		m, f, err := rfc5780(srv, *timeout)
		if err != nil {
			r.Notes = append(r.Notes, fmt.Sprintf("%s: %v", srv, err))
			continue
		}
		r.Server = srv
		if m != "" {
			r.Mapping = m
		}
		r.Filtering = f
		break
	}

	if host, _, err := net.SplitHostPort(*v6); err == nil {
		if c6, err := net.ListenUDP("udp6", nil); err == nil {
			if resp, err := request(c6, *v6, nil, *timeout); err == nil {
				if xa := mapped(resp); xa != nil {
					r.IPv6 = "mapped " + xa.String()
				}
			} else {
				r.Notes = append(r.Notes, fmt.Sprintf("ipv6 %s: %v", host, err))
			}
			_ = c6.Close()
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(r)
		return
	}
	fmt.Printf("host       %s (%s)\n", r.Host, r.Arch)
	fmt.Printf("local      %s\n", r.Local)
	fmt.Printf("observed   %s\n", strings.Join(r.Observed, ", "))
	fmt.Printf("mapping    %s\n", r.Mapping)
	fmt.Printf("filtering  %s\n", r.Filtering)
	fmt.Printf("ipv6       %s\n", r.IPv6)
	for _, n := range r.Notes {
		fmt.Printf("note       %s\n", n)
	}
}

// rfc5780 returns the mapping and filtering behaviour, or an error when the
// server has no OTHER-ADDRESS to test with.
func rfc5780(srv string, timeout time.Duration) (mapping, filtering string, err error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	primary, err := net.ResolveUDPAddr("udp4", srv)
	if err != nil {
		return "", "", err
	}
	resp1, err := request(conn, primary.String(), nil, timeout)
	if err != nil {
		return "", "", err
	}
	xa1 := mapped(resp1)
	var other stun.OtherAddress
	if xa1 == nil || other.GetFrom(resp1) != nil {
		return "", "", errors.New("no OTHER-ADDRESS, server does not support RFC 5780")
	}
	otherIPSamePort := &net.UDPAddr{IP: other.IP, Port: primary.Port}
	otherBoth := &net.UDPAddr{IP: other.IP, Port: other.Port}

	// Mapping: same mapping toward another IP, then toward another IP and port.
	switch {
	case sameHost(xa1.String(), localIPs()):
		mapping = "no NAT"
	default:
		resp2, err := request(conn, otherIPSamePort.String(), nil, timeout)
		if err != nil {
			return "", "", fmt.Errorf("mapping test II: %w", err)
		}
		xa2 := mapped(resp2)
		if xa2 != nil && xa2.String() == xa1.String() {
			mapping = "endpoint independent"
		} else {
			resp3, err := request(conn, otherBoth.String(), nil, timeout)
			if err != nil {
				return "", "", fmt.Errorf("mapping test III: %w", err)
			}
			xa3 := mapped(resp3)
			if xa3 != nil && xa2 != nil && xa3.String() == xa2.String() {
				mapping = "address dependent"
			} else {
				mapping = "address and port dependent"
			}
		}
	}

	// Filtering: ask the server to answer from another IP and port, then from
	// another port only.
	if _, err := request(conn, primary.String(), []byte{0, 0, 0, 0x06}, timeout); err == nil {
		return mapping, "endpoint independent", nil
	} else if !errors.Is(err, errTimeout) {
		return mapping, "", err
	}
	if _, err := request(conn, primary.String(), []byte{0, 0, 0, 0x02}, timeout); err == nil {
		return mapping, "address dependent", nil
	} else if !errors.Is(err, errTimeout) {
		return mapping, "", err
	}
	return mapping, "address and port dependent", nil
}

// request sends one binding request, with an optional CHANGE-REQUEST, and
// waits for the matching answer, retrying once.
func request(conn *net.UDPConn, server string, change []byte, timeout time.Duration) (*stun.Message, error) {
	network := "udp4"
	if conn.LocalAddr().(*net.UDPAddr).IP.To4() == nil {
		network = "udp6"
	}
	addr, err := net.ResolveUDPAddr(network, server)
	if err != nil {
		return nil, err
	}
	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if change != nil {
		req.Add(stun.AttrChangeRequest, change)
	}
	buf := make([]byte, 1500)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := conn.WriteTo(req.Raw, addr); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(deadline)
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				break
			}
			m := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if m.Decode() != nil || m.TransactionID != req.TransactionID {
				continue
			}
			return m, nil
		}
	}
	return nil, errTimeout
}

func mapped(m *stun.Message) *stun.XORMappedAddress {
	var xa stun.XORMappedAddress
	if err := xa.GetFrom(m); err != nil {
		return nil
	}
	return &xa
}

func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func sameHost(hostport string, ips []string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	for _, ip := range ips {
		if ip == h {
			return true
		}
	}
	return false
}

func allEqual(s []string) bool {
	for _, v := range s[1:] {
		if v != s[0] {
			return false
		}
	}
	return true
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
