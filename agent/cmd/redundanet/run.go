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
	"github.com/adefilippo83/redundanet/agent/internal/transport"
)

// `hub` and `agent` run the two long-lived modes against a signed manifest
// on disk. Fetching the manifest from the repository and the hub mirror,
// the local API and the service integration come in later steps; until
// then these commands are for the first tests on real machines.

type runtimeConfig struct {
	state    *identity.State
	tlsID    *identity.TLSIdentity
	device   identity.Key
	manifest *manifest.Manifest
	policy   *manifest.Policy
	log      *slog.Logger
}

func loadRuntime(dir, network, mPath, sigPath string, verbose bool) (*runtimeConfig, error) {
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
	// Until the network descriptor is implemented, the operators listed in
	// the manifest are the trusted ones: this proves consistency, not
	// authenticity, and the agent says so.
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
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	lg.Warn("trusting the operator keys listed in the manifest itself; pinning from the network descriptor is not implemented yet")
	return &runtimeConfig{state: st, tlsID: tlsID, device: dev.ID(), manifest: m, policy: manifest.NewPolicy(m, nil), log: lg}, nil
}

func runHub(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	mPath := fs.String("manifest", "manifests/manifest.yaml", "signed manifest file")
	sigPath := fs.String("sig", "", "signature file (default: manifest.sig beside the manifest)")
	udp := fs.String("udp", ":4433", "QUIC listener")
	tcp := fs.String("tcp", ":4443", "TLS listener for UDP-blocked devices; empty disables")
	every := fs.Duration("status-every", time.Minute, "log a status line this often")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rc, err := loadRuntime(*dir, *network, *mPath, *sigPath, *verbose)
	if err != nil {
		return err
	}
	if rc.manifest.HubByKey(rc.device) == nil {
		return fmt.Errorf("device %s is not listed as a hub in the manifest", rc.device.Short())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h, err := hub.Start(ctx, hub.Config{Identity: rc.tlsID, Device: rc.device, Allow: rc.policy.Allow, UDP: *udp, TCP: *tcp, Log: rc.log})
	if err != nil {
		return err
	}
	defer h.Close()
	logEvery(ctx, *every, func() any { return h.Status() }, rc.log)
	return nil
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	dir, network := commonFlags(fs)
	mPath := fs.String("manifest", "manifests/manifest.yaml", "signed manifest file")
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
	rc, err := loadRuntime(*dir, *network, *mPath, *sigPath, *verbose)
	if err != nil {
		return err
	}
	var hubs []transport.HubAddr
	for _, h := range rc.manifest.Hubs {
		hubs = append(hubs, transport.HubAddr{Key: h.Key, UDP: h.Relay, TCP: tcpFor(h.Relay)})
	}
	udpPort := 0
	for _, s := range rc.manifest.Storage {
		if s.Device == rc.device {
			udpPort = s.UDPPort
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ep, err := transport.Start(ctx, transport.Config{
		Identity: rc.tlsID, Device: rc.device, Allow: rc.policy.Allow, Hubs: hubs, Listen: *listen,
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
	logEvery(ctx, *every, func() any { return ep.Status() }, rc.log)
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
