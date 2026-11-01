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
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
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
	channel := fs.String("channel", release.ChannelStable, "release channel for self-update: stable or early")
	noUpdate := fs.Bool("no-update", false, "do not check for releases")
	probation := fs.Bool("probation", false, "run as an update candidate: report on stdout when healthy, for the supervising agent")
	probationPeers := fs.String("probation-peers", "", "device keys the candidate must reach to pass probation")
	verbose := fs.Bool("v", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		epMu sync.Mutex
		ep   *transport.Endpoint
	)
	current := func() *transport.Endpoint { epMu.Lock(); defer epMu.Unlock(); return ep }
	rc, err := loadRuntime(ctx, *dir, *network, *mPath, *sigPath, *verbose, func(*manifest.Manifest) {
		if e := current(); e != nil {
			e.Recheck()
		}
	})
	if err != nil {
		return err
	}
	var wanted []identity.Key
	for _, c := range strings.Split(*connect+","+*probationPeers, ",") {
		if c = strings.TrimSpace(c); c != "" {
			k, err := identity.ParseKey(c)
			if err != nil {
				return err
			}
			wanted = append(wanted, k)
		}
	}
	start := func() (*transport.Endpoint, error) {
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
		e, err := transport.Start(ctx, transport.Config{
			Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, Hubs: hubs, Listen: *listen,
			UDPPort: udpPort, PortMap: *portMap, TCPOnly: *tcpOnly, LoopbackCandidates: *loopback,
			OnStream: echoStream, Log: rc.log,
		})
		if err != nil {
			return nil, err
		}
		for _, k := range wanted {
			e.Connect(k)
		}
		return e, nil
	}
	ep, err = start()
	if err != nil {
		return err
	}
	defer func() {
		if e := current(); e != nil {
			_ = e.Close()
		}
	}()

	if *probation {
		return runProbation(ctx, ep, wanted)
	}

	if !*noUpdate && *mPath == "" {
		d, _ := dataDir(*dir)
		// The peers this agent has are noted when the endpoint is handed
		// over, since by the time the candidate runs the endpoint is gone.
		var peersMu sync.Mutex
		var lastPeers []string
		probe := func(pctx context.Context, exe string) error {
			// The candidate takes over this device's place on the mesh and
			// must reach the hubs and the peers this agent had.
			peersMu.Lock()
			peers := append([]string(nil), lastPeers...)
			peersMu.Unlock()
			args := []string{"agent", "--network", *network, "--data-dir", d, "--listen", *listen, "--probation", "--no-update"}
			if len(peers) > 0 {
				args = append(args, "--probation-peers", strings.Join(peers, ","))
			}
			if *tcpOnly {
				args = append(args, "--tcp-only")
			}
			if *loopback {
				args = append(args, "--loopback-candidates")
			}
			return probeAgent(pctx, exe, args)
		}
		handover := func(stopIt bool) {
			epMu.Lock()
			defer epMu.Unlock()
			if stopIt {
				if ep != nil {
					var peers []string
					for _, s := range ep.Status().Sessions {
						if s.Relay || s.Direct != "" {
							peers = append(peers, s.Peer)
						}
					}
					peersMu.Lock()
					lastPeers = peers
					peersMu.Unlock()
					_ = ep.Close()
					ep = nil
				}
				return
			}
			if ep == nil {
				if e, err := start(); err == nil {
					ep = e
				} else {
					rc.log.Error("could not restart the endpoint after a failed probation", "err", err)
				}
			}
		}
		u, err := newUpdater(d, *channel, probe, handover)
		if err != nil {
			return err
		}
		u.MinVersionFrom(func() string {
			if m := rc.current(); m != nil {
				return m.Network.MinVersion
			}
			return ""
		})
		go func() {
			if err := u.Run(ctx); errors.Is(err, update.ErrRestart) {
				rc.log.Info("new version installed, exiting so the service starts it")
				stop()
				os.Exit(0)
			}
		}()
		logEvery(ctx, *every, func() any {
			st := map[string]any{"update": u.Status()}
			if e := current(); e != nil {
				st["transport"] = e.Status()
			}
			if rc.member != nil {
				st["membership"] = rc.member.Status()
			}
			return st
		}, rc.log)
		return nil
	}

	logEvery(ctx, *every, func() any {
		st := map[string]any{}
		if e := current(); e != nil {
			st["transport"] = e.Status()
		}
		if rc.member != nil {
			st["membership"] = rc.member.Status()
		}
		return st
	}, rc.log)
	return nil
}

// echoStream answers a stream with its own bytes: nothing speaks over the
// mesh yet, and this lets a path be exercised.
func echoStream(_ identity.Peer, _ string, s *quic.Stream) {
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
}

// runProbation is the candidate's side: healthy means every hub channel is
// up and every peer named is connected, within the supervisor's deadline.
func runProbation(ctx context.Context, ep *transport.Endpoint, peers []identity.Key) error {
	deadline := time.After(110 * time.Second)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("probation: not healthy within the deadline")
		case <-t.C:
		}
		st := ep.Status()
		ok := len(st.Hubs) > 0
		for _, h := range st.Hubs {
			ok = ok && h.Up
		}
		have := map[string]bool{}
		for _, s := range st.Sessions {
			if s.Relay || s.Direct != "" {
				have[s.Peer] = true
			}
		}
		for _, p := range peers {
			ok = ok && have[p.String()]
		}
		if ok {
			fmt.Println(probationOK)
			<-ctx.Done() // keep serving until the supervisor stops us
			return nil
		}
	}
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
