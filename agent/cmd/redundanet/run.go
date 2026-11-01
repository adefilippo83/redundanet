package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

// `hub` and `agent` run the two long-lived modes against a signed manifest
// on disk. Fetching the manifest from the repository and the hub mirror,
// the local API and the service integration come in later steps; until
// then these commands are for the first tests on real machines.

type runtimeConfig struct {
	state   *identity.State
	tlsID   *identity.TLSIdentity
	device  identity.Key
	member  *membership.Membership // nil when running from a manifest file
	allow   identity.Allow
	current func() *manifest.Manifest
	log     *slog.Logger
}

// loadRuntime builds what agent and hub share. With a descriptor in the
// data directory (after `network join`) the manifest comes from the network
// and refreshes itself; with --manifest it comes from a file, trusting the
// operators that file lists, which is for tests and for a hub that serves
// the repository it is listed in.
func loadRuntime(ctx context.Context, dir, network, mPath, sigPath string, verbose bool, onApply func(*manifest.Manifest)) (*runtimeConfig, error) {
	if network == "" {
		return nil, errors.New("--network is required")
	}
	d, err := dataDir(dir)
	if err != nil {
		return nil, err
	}
	st, err := loadState(identityPath(d), network)
	if err != nil {
		return nil, err
	}
	dev, err := st.Device()
	if err != nil {
		return nil, err
	}
	tlsID, err := identity.NewTLSIdentity(dev, &st.Certificate)
	if err != nil {
		return nil, err
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	rc := &runtimeConfig{state: st, tlsID: tlsID, device: dev.ID(), log: lg}

	if mPath == "" {
		m, err := openMembership(ctx, d, onApply)
		if err != nil {
			return nil, err
		}
		if m.Descriptor().Network != network {
			return nil, fmt.Errorf("this device joined network %q", m.Descriptor().Network)
		}
		rc.member, rc.allow, rc.current = m, m.Allow, m.Manifest
		return rc, nil
	}

	data, err := os.ReadFile(mPath)
	if err != nil {
		return nil, err
	}
	if sigPath == "" {
		sigPath = filepath.Join(filepath.Dir(mPath), "manifest.sig")
	}
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return nil, fmt.Errorf("a signed manifest is required: %w", err)
	}
	sig, err := manifest.ParseSignature(sigData)
	if err != nil {
		return nil, err
	}
	pre, err := manifest.Parse(data)
	if err != nil {
		return nil, err
	}
	var trusted []identity.Key
	for _, o := range pre.Operators {
		trusted = append(trusted, o.Key)
	}
	m, err := sig.Verify(data, trusted)
	if err != nil {
		return nil, err
	}
	if m.Network.Name != network {
		return nil, fmt.Errorf("the manifest is for network %q", m.Network.Name)
	}
	lg.Warn("running from a manifest file: trusting the operator keys it lists itself, and it will not refresh")
	pol := manifest.NewPolicy(m, nil)
	rc.allow, rc.current = pol.Allow, func() *manifest.Manifest { return m }
	return rc, nil
}

func runHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	mPath := fs.String("manifest", "", "run from this signed manifest file instead of the joined network")
	sigPath := fs.String("sig", "", "signature file (default: manifest.sig beside the manifest)")
	udp := fs.String("udp", ":4433", "QUIC listener")
	tcp := fs.String("tcp", ":4443", "TLS listener for UDP-blocked devices; empty disables")
	every := fs.Duration("status-every", time.Minute, "log a status line this often")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rc, err := loadRuntime(ctx, *dir, *network, *mPath, *sigPath, *verbose, nil)
	if err != nil {
		return err
	}
	if rc.current().HubByKey(rc.device) == nil {
		return fmt.Errorf("device %s is not listed as a hub in the manifest", rc.device.Short())
	}
	h, err := hub.Start(ctx, hub.Config{Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, UDP: *udp, TCP: *tcp, Log: rc.log})
	if err != nil {
		return err
	}
	defer h.Close()
	logEvery(ctx, *every, func() any {
		st := map[string]any{"hub": h.Status()}
		if rc.member != nil {
			st["membership"] = rc.member.Status()
		}
		return st
	}, rc.log)
	return nil
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	mPath := fs.String("manifest", "", "run from this signed manifest file instead of the joined network")
	sigPath := fs.String("sig", "", "signature file (default: manifest.sig beside the manifest)")
	listen := fs.String("listen", ":0", "UDP address to bind")
	connect := fs.String("connect", "", "comma-separated device keys to keep sessions with")
	portMap := fs.Bool("portmap", false, "ask the router for a port mapping")
	tcpOnly := fs.Bool("tcp-only", false, "reach hubs over TCP only")
	loopback := fs.Bool("loopback-candidates", false, "advertise 127.0.0.1 too, for tests on one machine")
	every := fs.Duration("status-every", time.Minute, "log a status line this often")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var ep *transport.Endpoint
	rc, err := loadRuntime(ctx, *dir, *network, *mPath, *sigPath, *verbose, func(*manifest.Manifest) {
		if ep != nil {
			ep.Recheck()
		}
	})
	if err != nil {
		return err
	}
	man := rc.current()
	var hubs []transport.HubAddr
	for _, h := range man.Hubs {
		hubs = append(hubs, transport.HubAddr{Key: h.Key, UDP: h.Relay, TCP: tcpFor(h.Relay)})
	}
	udpPort := 0
	for _, s := range man.Storage {
		if s.Device == rc.device {
			udpPort = s.UDPPort
		}
	}
	ep, err = transport.Start(ctx, transport.Config{
		Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, Hubs: hubs, Listen: *listen,
		UDPPort: udpPort, PortMap: *portMap, TCPOnly: *tcpOnly, LoopbackCandidates: *loopback,
		OnStream: func(peer identity.Peer, path string, s *quic.Stream) {
			// Nothing speaks over the mesh yet; a stream is answered with
			// its own bytes so the path can be exercised.
			buf := make([]byte, 64<<10)
			for {
				n, err := s.Read(buf)
				if n > 0 {
					if _, werr := s.Write(buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
			_ = s.Close()
		},
		Log: rc.log,
	})
	if err != nil {
		return err
	}
	defer ep.Close()
	if *connect != "" {
		for _, c := range strings.Split(*connect, ",") {
			k, err := identity.ParseKey(c)
			if err != nil {
				return err
			}
			ep.Connect(k)
		}
	}
	logEvery(ctx, *every, func() any {
		st := map[string]any{"transport": ep.Status()}
		if rc.member != nil {
			st["membership"] = rc.member.Status()
		}
		return st
	}, rc.log)
	return nil
}

// tcpFor derives the TCP 443 carrier address from a relay address: same
// host, port 443, the RFC's rule for UDP-blocked networks.
func tcpFor(relay string) string {
	host := relay
	if i := strings.LastIndex(relay, ":"); i > 0 {
		host = relay[:i]
	}
	return host + ":443"
}

func logEvery(ctx context.Context, every time.Duration, status func() any, lg *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b, _ := json.Marshal(status())
			lg.Info("status", "snapshot", string(b))
		}
	}
}
