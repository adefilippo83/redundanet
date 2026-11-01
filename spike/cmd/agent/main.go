// Command agent is the spike's device side: one UDP socket, a channel to the
// hub, a QUIC session per peer that moves between relay and direct paths, and
// the loopback bridge that lets an unmodified Tahoe node talk through it.
//
//	agent id -key agent.key                      print this device's id
//	agent -key agent.key -hub ID@host:4433 ...   run
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/spike/internal/bridge"
	"github.com/adefilippo83/redundanet/spike/internal/hubclient"
	"github.com/adefilippo83/redundanet/spike/internal/identity"
	"github.com/adefilippo83/redundanet/spike/internal/mux"
	"github.com/adefilippo83/redundanet/spike/internal/portmap"
	"github.com/adefilippo83/redundanet/spike/internal/session"
	"github.com/adefilippo83/redundanet/spike/internal/wire"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "id" {
		fs := flag.NewFlagSet("id", flag.ExitOnError)
		keyPath := fs.String("key", "agent.key", "device key file, created when missing")
		_ = fs.Parse(os.Args[2:])
		key, err := identity.Load(*keyPath)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(key.ID)
		return
	}

	keyPath := flag.String("key", "agent.key", "device key file, created when missing")
	hubFlag := flag.String("hub", "", "hub as ID@host:port (QUIC over UDP)")
	hubTCP := flag.String("hub-tcp", "", "hub TLS listener host:port, used when UDP gets no answer")
	tcpOnly := flag.Bool("tcp-only", false, "never try UDP to the hub: simulates a UDP-blocked network")
	listen := flag.String("listen", ":0", "UDP address to bind")
	expose := flag.String("expose", "", "loopback TCP ports peers may bridge to, comma separated")
	var bridges, connects multiFlag
	flag.Var(&bridges, "bridge", "LOCALADDR=PEERID:PORT, repeatable: listen locally and tunnel to the peer's port")
	flag.Var(&connects, "connect", "PEERID, repeatable: keep a session with this peer")
	benchPeer := flag.String("bench", "", "PEERID: send -bench-size MiB to the peer, print the rate, exit")
	benchSize := flag.Int64("bench-size", 64, "MiB per bench run")
	benchPath := flag.String("bench-path", "auto", "auto, relay or direct")
	benchWait := flag.Duration("bench-wait", 8*time.Second, "time to let the direct path come up before an auto bench")
	noDirect := flag.Bool("no-direct", false, "never punch or migrate: relay only")
	loopbackCand := flag.Bool("loopback-candidates", false, "advertise 127.0.0.1 too, for tests on one machine")
	onlyReflexive := flag.Bool("only-reflexive", false, "advertise no interface addresses: tests the public IPv4 punch alone")
	portMap := flag.Bool("portmap", false, "ask the router for a UDP port mapping (PCP, NAT-PMP, UPnP) and advertise it: reachable even from a symmetric NAT")
	includeOverlay := flag.Bool("include-overlay", false, "advertise overlay addresses too (Tailscale, carrier-grade ranges); off, so an overlay's own relay never counts as a direct path")
	httpAddr := flag.String("http", "127.0.0.1:4481", "local status page; empty disables")
	flag.Parse()

	lg := log.New(os.Stderr, "", log.Ltime|log.Lmicroseconds)
	key, err := identity.Load(*keyPath)
	if err != nil {
		lg.Fatal(err)
	}
	lg.Printf("device id %s", key.ID)

	hubID, hubAddr, err := parseHub(*hubFlag)
	if err != nil {
		lg.Fatal(err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		lg.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		lg.Fatal(err)
	}
	mx := mux.New(key.ID, udp, lg)
	tr := &quic.Transport{Conn: mx.Default()}
	lg.Printf("udp socket %s", udp.LocalAddr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A router mapping is a candidate any peer can reach. It is asked for
	// before the hub channel comes up, so the first Hello carries it.
	var mapped []string
	if *portMap {
		mctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		m, notes := portmap.Map(mctx, mx.LocalPort(), time.Hour)
		cancel()
		for _, n := range notes {
			lg.Printf("portmap: %s", n)
		}
		if m != nil {
			lg.Printf("portmap: %s", m)
			mapped = append(mapped, m.External.String())
			defer func() { _ = m.Release() }()
			go func() {
				t := time.NewTicker(m.Lifetime / 2)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
						if r, err := portmap.MapWith(rctx, m.Proto, mx.LocalPort(), time.Hour); err != nil {
							lg.Printf("portmap: refresh failed: %v", err)
						} else {
							m = r
						}
						cancel()
					}
				}
			}()
		}
	}

	exposed := map[int]bool{}
	for _, p := range strings.Split(*expose, ",") {
		if p = strings.TrimSpace(p); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				lg.Fatalf("-expose: %v", err)
			}
			exposed[n] = true
		}
	}
	srv := &bridge.Server{Expose: exposed, Log: lg}

	// A relay bench must stay on the relay: otherwise a punch that lands
	// mid-run migrates the connection and the number is a direct one.
	if *benchPeer != "" && *benchPath == session.PathRelay {
		*noDirect = true
	}
	mgr := &session.Manager{Key: key, Mux: mx, Tr: tr, Allow: identity.Any, NoDirect: *noDirect, OnStream: srv.Handle, Log: lg}
	hub := hubclient.New(hubclient.Config{
		Key: key, HubID: hubID, UDPAddr: hubAddr, TCPAddr: *hubTCP, TCPOnly: *tcpOnly,
		Transport: tr, Mux: mx,
		Locals: func() []string {
			if *onlyReflexive {
				return mapped
			}
			return append(append([]string(nil), mapped...), localCandidates(mx.LocalPort(), *loopbackCand, *includeOverlay)...)
		},
		OnCandidates: func(c wire.Candidates) { mgr.OnCandidates(c) },
		OnUp:         func() { mgr.HubUp() },
		Log:          lg,
	})
	mgr.Hub = hub
	if err := mgr.Start(ctx); err != nil {
		lg.Fatal(err)
	}
	go hub.Run(ctx)

	for _, c := range connects {
		id, err := identity.ParseID(c)
		if err != nil {
			lg.Fatal(err)
		}
		mgr.Connect(id)
	}
	for _, b := range bridges {
		local, id, port, err := parseBridge(b)
		if err != nil {
			lg.Fatal(err)
		}
		go func() {
			if err := bridge.Listen(ctx, mgr, local, id, port, lg); err != nil {
				lg.Printf("bridge %s: %v", local, err)
			}
		}()
	}

	if *httpAddr != "" {
		h := http.NewServeMux()
		h.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
			accepted, refused := srv.Counts()
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{
				"device":          key.ID.String(),
				"udp":             udp.LocalAddr().String(),
				"hub":             hub.Status(),
				"sessions":        mgr.Status(),
				"bridged":         accepted,
				"bridge_refused":  refused,
				"dropped_packets": mx.Dropped(),
				"relay_drops":     hub.Dropped(),
			})
		})
		h.HandleFunc("/switch", func(w http.ResponseWriter, r *http.Request) {
			id, err := identity.ParseID(r.URL.Query().Get("peer"))
			if err == nil {
				err = mgr.Switch(id, r.URL.Query().Get("path"))
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, "now on %s\n", mgr.Active(id))
		})
		h.HandleFunc("/blackhole", func(w http.ResponseWriter, r *http.Request) {
			id, err := identity.ParseID(r.URL.Query().Get("peer"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			addr := mgr.DirectAddr(id)
			if addr == "" {
				http.Error(w, "no direct address to cut", http.StatusBadRequest)
				return
			}
			on := r.URL.Query().Get("on") != "0"
			mx.Blackhole(addr, on)
			lg.Printf("blackhole %s: %v", addr, on)
			fmt.Fprintf(w, "blackhole %s %v\n", addr, on)
		})
		h.HandleFunc("/connect", func(w http.ResponseWriter, r *http.Request) {
			id, err := identity.ParseID(r.URL.Query().Get("peer"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mgr.Connect(id)
			fmt.Fprintln(w, "ok")
		})
		hs := &http.Server{Addr: *httpAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				lg.Printf("http: %v", err)
			}
		}()
		lg.Printf("status on http://%s/status", *httpAddr)
	}

	if *benchPeer != "" {
		id, err := identity.ParseID(*benchPeer)
		if err != nil {
			lg.Fatal(err)
		}
		if err := runBench(ctx, mgr, id, *benchSize<<20, *benchPath, *benchWait, lg); err != nil {
			lg.Printf("bench: %v", err)
			os.Exit(1)
		}
		if len(bridges) == 0 {
			return
		}
	}
	<-ctx.Done()
	_ = mx.Close()
}

func runBench(ctx context.Context, mgr *session.Manager, peer identity.ID, size int64, path string, wait time.Duration, lg *log.Logger) error {
	mgr.Connect(peer)
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for mgr.Active(peer) == session.PathNone {
		select {
		case <-wctx.Done():
			return fmt.Errorf("no connection to %s", peer.Short())
		case <-time.After(100 * time.Millisecond):
		}
	}
	switch path {
	case "auto":
		deadline := time.Now().Add(wait)
		for mgr.Active(peer) != session.PathDirect && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	case session.PathRelay, session.PathDirect:
		deadline := time.Now().Add(wait)
		for {
			err := mgr.Switch(peer, path)
			if err == nil && mgr.Active(peer) == path {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("cannot get onto the %s path: %v", path, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	default:
		return fmt.Errorf("unknown -bench-path %q", path)
	}
	res, err := bridge.Bench(ctx, mgr, peer, size)
	if err != nil {
		return err
	}
	lg.Printf("bench: %d MiB to %s via %s in %s: %.1f Mbit/s (%.1f MiB/s)",
		size>>20, peer.Short(), res.Path, res.Duration.Round(time.Millisecond), res.MbitPerS, res.MiBPerS)
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	return nil
}

func parseHub(s string) (identity.ID, string, error) {
	id, addr, ok := strings.Cut(s, "@")
	if !ok {
		return identity.ID{}, "", fmt.Errorf("-hub must be ID@host:port, got %q", s)
	}
	hid, err := identity.ParseID(id)
	return hid, addr, err
}

func parseBridge(s string) (string, identity.ID, int, error) {
	local, rest, ok := strings.Cut(s, "=")
	if !ok {
		return "", identity.ID{}, 0, fmt.Errorf("-bridge must be LOCALADDR=PEERID:PORT, got %q", s)
	}
	idStr, portStr, ok := strings.Cut(rest, ":")
	if !ok {
		return "", identity.ID{}, 0, fmt.Errorf("-bridge must be LOCALADDR=PEERID:PORT, got %q", s)
	}
	id, err := identity.ParseID(idStr)
	if err != nil {
		return "", identity.ID{}, 0, err
	}
	port, err := strconv.Atoi(portStr)
	return local, id, port, err
}

// Overlay networks whose addresses are reachable only through someone else's
// relay: Tailscale's IPv4 and IPv6 ranges, and carrier-grade NAT space.
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

// localCandidates lists this host's addresses with the socket's port: LAN
// peers connect to these without hairpinning through the relay.
func localCandidates(port int, loopback, overlay bool) []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
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
			if ip.IsLoopback() && !loopback {
				continue
			}
			if ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			if isOverlay(ip) && !overlay {
				continue
			}
			out = append(out, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		}
	}
	return out
}
