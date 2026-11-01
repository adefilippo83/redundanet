// Command portmap reports which port-mapping protocols the router in front of
// this host answers: it binds a UDP port, asks for a two-minute mapping with
// each protocol in turn, prints the result and removes the mapping at once.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/adefilippo83/redundanet/spike/internal/portmap"
)

func main() {
	lifetime := flag.Duration("lifetime", 2*time.Minute, "mapping lifetime asked for")
	flag.Parse()
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer conn.Close()
	port := conn.LocalAddr().(*net.UDPAddr).Port
	host, _ := os.Hostname()
	fmt.Printf("host %s, local udp port %d\n", host, port)
	for _, proto := range portmap.Protocols {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		m, err := portmap.MapWith(ctx, proto, port, *lifetime)
		cancel()
		if err != nil {
			fmt.Printf("%-7s no: %v\n", proto, err)
			continue
		}
		note := ""
		if a := m.External.Addr(); a.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
			note = " (private or carrier-grade address: another NAT sits above this router)"
		}
		fmt.Printf("%-7s yes: %s%s\n", proto, m, note)
		if err := m.Release(); err != nil {
			fmt.Printf("%-7s release failed: %v\n", proto, err)
		} else {
			fmt.Printf("%-7s released\n", proto)
		}
	}
}
