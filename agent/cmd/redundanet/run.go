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
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/api"
	"github.com/adefilippo83/redundanet/agent/internal/collector"
	"github.com/adefilippo83/redundanet/agent/internal/diskfree"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/ratelimit"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
	"github.com/adefilippo83/redundanet/agent/internal/version"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
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
	logs    *logbuf.Buffer
	dataDir string
	started time.Time
}

// snapshot is what the local API shows.
func (rc *runtimeConfig) snapshot(ep *transport.Endpoint, u *update.Updater, w *repair.Worker) api.Snapshot {
	s := api.Snapshot{Version: version.Version, Uptime: time.Since(rc.started).Round(time.Second).String(), DataDir: rc.dataDir,
		Device: rc.device.String(), Member: rc.state.Member.String(), MemberCode: identity.DisplayCode(rc.state.Member),
		Roles: rc.state.Certificate.Certificate.Roles, Generated: time.Now()}
	if m := rc.current(); m != nil {
		s.Network = m.Network.Name
	}
	if ep != nil {
		t := ep.Status()
		s.Transport = &t
	}
	if rc.member != nil {
		m := rc.member.Status()
		s.Membership = &m
	}
	if u != nil {
		st := u.Status()
		s.Update = &st
	}
	if w != nil {
		st := w.Status()
		s.Repair = &st
	}
	sv := service.Query("")
	s.Service = &sv
	return s
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
	ring := logbuf.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), 500)
	lg := slog.New(ring)
	rc := &runtimeConfig{state: st, tlsID: tlsID, device: dev.ID(), log: lg, logs: ring, dataDir: d, started: time.Now()}

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
	status := fs.String("status", "127.0.0.1:4480", "status page and status.json; empty disables")
	legacy := fs.String("legacy-status", "", "a RedundaNet 2 status.json to show beside the mesh census, for example https://redundanet-hub.fly.dev/status.json")
	publish := fs.String("publish", "", "publish the page and status.json under this URL every minute, for example https://redundanet-hub.fly.dev/v3/")
	tokenFile := fs.String("publish-token-file", "", "file holding the bearer token --publish sends")
	every := fs.Duration("status-every", time.Minute, "log a status line this often")
	channel := fs.String("channel", release.ChannelStable, "release channel for self-update: stable or early")
	noUpdate := fs.Bool("no-update", false, "do not check for releases")
	probation := fs.Bool("probation", false, "run as an update candidate: report on stdout when healthy, for the supervising hub")
	probationPeers := fs.String("probation-peers", "", "device keys the candidate must see connected to pass probation")
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
	var token string
	if *publish != "" {
		if *tokenFile == "" {
			return errors.New("--publish needs --publish-token-file")
		}
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			return fmt.Errorf("%s is empty", *tokenFile)
		}
	}
	var wanted []identity.Key
	for _, c := range strings.Split(*probationPeers, ",") {
		if c = strings.TrimSpace(c); c != "" {
			k, err := identity.ParseKey(c)
			if err != nil {
				return err
			}
			wanted = append(wanted, k)
		}
	}

	// Everything a self-update hands over lives in one bundle: the relay,
	// the status listener and the collector's loops. The census on disk is
	// what the candidate inherits.
	type bundle struct {
		h        *hub.Server
		col      *collector.Collector
		cancel   context.CancelFunc
		stopHTTP func()
	}
	var (
		mu  sync.Mutex
		cur *bundle
	)
	hubKey, err := rc.state.Device()
	if err != nil {
		return err
	}
	start := func() (*bundle, error) {
		bctx, cancel := context.WithCancel(ctx)
		col, err := collector.New(collector.Config{Device: rc.device, Signer: hubKey, Manifest: rc.current, Path: filepath.Join(rc.dataDir, "census.json"),
			Placement: filepath.Join(rc.dataDir, "placement.sqlite"), Legacy: *legacy, Publish: *publish, PublishToken: token, Started: rc.started, Log: rc.log})
		if err != nil {
			cancel()
			return nil, err
		}
		h, err := hub.Start(bctx, hub.Config{Identity: rc.tlsID, Device: rc.device, Allow: rc.allow, UDP: *udp, TCP: *tcp, OnCensus: col.Record, OnShares: col.RecordShares, OnTaskReport: col.RecordTaskReport, Log: rc.log})
		if err != nil {
			cancel()
			return nil, err
		}
		col.SetLive(h.Status)
		col.SetSend(h.Send)
		go col.Run(bctx)
		b := &bundle{h: h, col: col, cancel: cancel, stopHTTP: func() {}}
		if *status != "" {
			_, stopHTTP, err := col.Serve(bctx, *status)
			if err != nil {
				_ = h.Close()
				cancel()
				return nil, err
			}
			b.stopHTTP = stopHTTP
		}
		return b, nil
	}
	stopBundle := func(b *bundle) {
		b.stopHTTP()
		_ = b.h.Close()
		b.cancel()
		if err := b.col.Save(); err != nil {
			rc.log.Warn("census not saved at handover", "err", err)
		}
		_ = b.col.Close()
	}
	cur, err = start()
	if err != nil {
		return err
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if cur != nil {
			stopBundle(cur)
		}
	}()
	current := func() *bundle { mu.Lock(); defer mu.Unlock(); return cur }

	if *probation {
		return runHubProbation(ctx, func() hub.Status { return current().h.Status() }, wanted)
	}

	if !*noUpdate && *mPath == "" {
		var peersMu sync.Mutex
		var lastPeers []string
		probe := func(pctx context.Context, exe string) error {
			peersMu.Lock()
			peers := append([]string(nil), lastPeers...)
			peersMu.Unlock()
			return probeAgent(pctx, exe, candidateArgs(os.Args[1:], peers))
		}
		handover := func(stopIt bool) {
			mu.Lock()
			defer mu.Unlock()
			if stopIt {
				if cur != nil {
					var peers []string
					for _, c := range cur.h.Status().Clients {
						peers = append(peers, c.Device)
					}
					peersMu.Lock()
					lastPeers = peers
					peersMu.Unlock()
					stopBundle(cur)
					cur = nil
				}
				return
			}
			if cur == nil {
				if b, err := start(); err == nil {
					cur = b
				} else {
					rc.log.Error("could not restart the hub after a failed probation", "err", err)
				}
			}
		}
		u, err := newUpdater(rc.dataDir, *channel, probe, handover)
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
			if b := current(); b != nil {
				st["hub"], st["collector"] = b.h.Status(), b.col.Health()
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
		if b := current(); b != nil {
			st["hub"], st["collector"] = b.h.Status(), b.col.Health()
		}
		if rc.member != nil {
			st["membership"] = rc.member.Status()
		}
		return st
	}, rc.log)
	return nil
}

// hubHealthy is a candidate hub's health: it is serving, and every device
// the previous hub had is back on a channel.
func hubHealthy(st hub.Status, peers []identity.Key) bool {
	have := map[string]bool{}
	for _, c := range st.Clients {
		have[c.Device] = true
	}
	for _, p := range peers {
		if !have[p.String()] {
			return false
		}
	}
	return true
}

// runHubProbation is the candidate hub's side of a self-update.
func runHubProbation(ctx context.Context, status func() hub.Status, peers []identity.Key) error {
	deadline := time.After(110 * time.Second)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("probation: the devices did not come back within the deadline")
		case <-t.C:
		}
		if hubHealthy(status(), peers) {
			fmt.Println(probationOK)
			<-ctx.Done() // keep serving until the supervisor stops us
			return nil
		}
	}
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
	apiAddr := fs.String("api", "127.0.0.1:4470", "local API and page; loopback only")
	storagePath := fs.String("storage-path", "", "the disk a storage device stores on; the store lives in its redundanet3 directory (default: the data directory)")
	storageFloor := fs.Int64("storage-floor-gb", 10, "free gigabytes below which the store refuses writes")
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
	var (
		uMu sync.Mutex
		u   *update.Updater
	)
	getU := func() *update.Updater { uMu.Lock(); defer uMu.Unlock(); return u }
	// A storage device opens its store once; the endpoint may restart
	// around it during a self-update.
	var st *store.Store
	isStorage := slices.Contains(rc.state.Certificate.Certificate.Roles, identity.RoleStorage)
	storeErr := ""
	if isStorage {
		disk := *storagePath
		if disk == "" {
			disk = rc.dataDir
		}
		st, err = openStore(disk, rc.dataDir, *storageFloor, rc.log)
		if err != nil {
			storeErr = err.Error()
			rc.log.Error("storage unavailable: this device will not serve shares until the disk is back", "disk", disk, "err", err)
		} else {
			defer st.Close()
			pol := &manifestPolicy{rc: rc}
			go storeMaintenance(ctx, st, pol.memberActive, pol.grace, rc.log)
		}
	}
	onStream := shareproto.RefuseAll
	if st != nil {
		onStream = shareproto.NewServer(st, &manifestPolicy{rc: rc}, rc.log).Handle
	}

	// The engine and the jobs: over whatever endpoint is current, so a
	// self-update's handover does not strand them.
	ldb, err := localdb.Open(rc.dataDir, nil)
	if err != nil {
		return err
	}
	defer ldb.Close()
	devKey, err := rc.state.Device()
	if err != nil {
		return err
	}
	spoolDir := filepath.Join(rc.dataDir, "spool")
	_ = os.MkdirAll(spoolDir, 0o700)
	// This device's own server is reached in process, whatever the mesh is
	// doing: a storage node never dials itself.
	local := shareproto.LocalOpener(identity.Peer{Device: rc.device, Member: rc.state.Member, Certificate: &rc.state.Certificate}, onStream)
	// The bandwidth caps pace every mesh stream, opened or accepted; the
	// in-process path to this device's own store is never paced. The jobs
	// file holds the caps and feeds them in.
	upCap, downCap := ratelimit.New(0), ratelimit.New(0)
	pacedOnStream := func(p identity.Peer, path string, st wire.Stream) {
		onStream(p, path, ratelimit.Stream(st, upCap, downCap))
	}
	client := shareproto.NewClient(func(ctx context.Context, peer identity.Key) (wire.Stream, string, error) {
		if peer == rc.device {
			return local(ctx, peer)
		}
		e := current()
		if e == nil {
			return nil, "", errors.New("the endpoint is handing over")
		}
		st, path, err := e.OpenStream(ctx, peer)
		if err != nil {
			return nil, "", err
		}
		return ratelimit.Stream(st, upCap, downCap), path, nil
	})
	var runner *jobs.Runner
	// A storage node volunteers as a repair worker: it takes the hub's
	// signed tasks and answers its surplus hints.
	var worker *repair.Worker
	if !*probation {
		eng, err := engine.New(engine.Config{Network: rc.state.Network, Member: rc.state.Member, Secret: rc.state.MemberSecret, Device: devKey, Cert: &rc.state.Certificate,
			Manifest: rc.current, Client: client, DB: ldb, SpoolDir: spoolDir, Store: st, Log: rc.log})
		if err != nil {
			return err
		}
		var revocations func() []*identity.SignedRevocation
		if rc.member != nil {
			revocations = rc.member.Revocations
		}
		runner, err = jobs.New(jobs.Config{DataDir: rc.dataDir, Engine: eng, DB: ldb, Member: rc.state.Member, Device: rc.device, Revocations: revocations,
			OnLimits: func(up, down int) { upCap.SetKBps(up); downCap.SetKBps(down) },
			Manifest: rc.current, SpoolDir: spoolDir, Log: rc.log})
		if err != nil {
			return err
		}
		if st != nil {
			worker, err = repair.New(repair.Config{Device: rc.device, Manifest: rc.current, Client: client, Store: st, Log: rc.log})
			if err != nil {
				return err
			}
			go worker.Run(ctx)
		}
	}
	var onTask func(*wire.RepairTask, func(*wire.TaskReport) error)
	var onRelease func(*wire.ReleaseHint)
	if worker != nil {
		onTask, onRelease = worker.Handle, worker.Release
	}

	// census is the heartbeat each hub gets: no address and no path in it.
	census := func() wire.Census {
		cs := wire.Census{Version: version.Version, Uptime: uint64(time.Since(rc.started).Seconds()), OS: runtime.GOOS + "/" + runtime.GOARCH, Roles: rc.state.Certificate.Certificate.Roles}
		if m := rc.current(); m != nil {
			cs.Manifest = m.Revision
		}
		if isStorage {
			p := *storagePath
			if p == "" {
				p = rc.dataDir
			}
			cs.Storage = &wire.CensusStorage{State: "unavailable"}
			if free, total, err := diskfree.Free(p); err == nil {
				cs.Storage.FreeBytes, cs.Storage.TotalBytes = free, total
			}
			switch {
			case st == nil:
				cs.Storage.State = "unavailable: " + storeErr
			case st.Available() != nil:
				cs.Storage.State = "unavailable: disk or marker missing"
			default:
				cs.Storage.State = "ok"
				cs.Storage.Floor = uint64(st.Floor())
				cs.Worker = worker != nil
				if shares, bytes, err := st.Totals(); err == nil {
					cs.Storage.Shares, cs.Storage.Bytes = uint64(shares), uint64(bytes)
				}
				if t, err := st.VerifiedSince(); err == nil {
					cs.Storage.Verified = t
				}
			}
		}
		if runner != nil {
			at, partial := runner.LastBackup()
			cb := &wire.CensusBackup{LastBackup: at, Partial: partial}
			if v := runner.LastVerify(); v != nil {
				cb.LastVerify, cb.VerifyOK = v.At, len(v.Failed) == 0 && v.Verified > 0
				cb.VerifyStreak = runner.VerifyStreak()
			}
			if !at.IsZero() || !cb.LastVerify.IsZero() {
				cs.Backup = cb
			}
		}
		if u := getU(); u != nil {
			st := u.Status()
			switch {
			case len(st.Failed) > 0:
				cs.Update = st.Failed[len(st.Failed)-1] + " failed probation"
			case st.Available != "":
				cs.Update = st.Available + " available"
			case st.LastError != "":
				cs.Update = "check failed"
			}
		}
		return cs
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
			hubs = append(hubs, transport.HubAddr{Key: h.Key, UDP: h.Relay, TCP: h.TCPAddr()})
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
			OnStream: pacedOnStream, Census: census, ShareCensus: shareCensus(st), OnTask: onTask, OnRelease: onRelease, Log: rc.log,
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

	go runner.Run(ctx)
	apiSrv, err := api.Start(ctx, api.Config{
		Addr: *apiAddr, DataDir: rc.dataDir, Logs: rc.logs, Log: rc.log, Jobs: runner,
		Snapshot: func() api.Snapshot {
			s := rc.snapshot(current(), getU(), worker)
			js := runner.Status()
			s.Jobs = &js
			return s
		},
		Actions: api.Actions{
			CheckUpdate: func(ctx context.Context) (string, error) {
				u := getU()
				if u == nil {
					return "", errors.New("updates are off")
				}
				rel, err := u.Check(ctx)
				if err != nil || rel == nil {
					return "", err
				}
				return rel.Version, nil
			},
			RetryUpdate: func(ctx context.Context, v string) error {
				u := getU()
				if u == nil {
					return errors.New("updates are off")
				}
				return u.Retry(v)
			},
			ApplyUpdate: func(ctx context.Context) error {
				u := getU()
				if u == nil {
					return errors.New("updates are off")
				}
				rel, err := u.Check(ctx)
				if err != nil {
					return err
				}
				if rel == nil {
					return errors.New("nothing to apply")
				}
				go func() {
					if err := u.Apply(context.Background(), rel); errors.Is(err, update.ErrRestart) {
						rc.log.Info("new version installed, exiting so the service starts it")
						stop()
						os.Exit(0)
					} else if err != nil {
						rc.log.Warn("update failed", "err", err)
					}
				}()
				return nil
			},
		},
	})
	if err != nil {
		return err
	}
	_ = apiSrv

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
			// The candidate runs with this process's own flags, so it opens
			// the same store on the same disk and listens where this one
			// did; only the probation flags differ.
			return probeAgent(pctx, exe, candidateArgs(os.Args[1:], peers))
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
		nu, err := newUpdater(d, *channel, probe, handover)
		if err != nil {
			return err
		}
		uMu.Lock()
		u = nu
		uMu.Unlock()
		nu.MinVersionFrom(func() string {
			if m := rc.current(); m != nil {
				return m.Network.MinVersion
			}
			return ""
		})
		go func() {
			if err := nu.Run(ctx); errors.Is(err, update.ErrRestart) {
				rc.log.Info("new version installed, exiting so the service starts it")
				stop()
				os.Exit(0)
			}
		}()
		logEvery(ctx, *every, func() any {
			st := map[string]any{"update": nu.Status()}
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

// candidateArgs is the command line of an update candidate: the running
// process's own arguments with the probation flags replaced, so a flag
// added later never has to be threaded through by hand. A flag may come
// as --name value or --name=value.
func candidateArgs(args []string, peers []string) []string {
	skipValue := map[string]bool{"--probation-peers": true, "-probation-peers": true}
	skip := map[string]bool{"--probation": true, "-probation": true, "--no-update": true, "-no-update": true}
	out := make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		a := args[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq > 0 {
			name = a[:eq]
		}
		switch {
		case skip[name]:
			continue
		case skipValue[name]:
			if name == a && i+1 < len(args) {
				i++ // the value is the next argument
			}
			continue
		}
		out = append(out, a)
	}
	out = append(out, "--probation", "--no-update")
	if len(peers) > 0 {
		out = append(out, "--probation-peers", strings.Join(peers, ","))
	}
	return out
}

// openStore opens the share store under the storage disk. The disk itself
// must exist, which the store checks; its redundanet3 directory is ours to
// create.
func openStore(disk, dataDir string, floorGB int64, lg *slog.Logger) (*store.Store, error) {
	if fi, err := os.Stat(disk); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", disk)
	}
	root := filepath.Join(disk, "redundanet3")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return store.Open(store.Config{Root: root, RecordPath: filepath.Join(dataDir, "storage-id"), Floor: floorGB << 30, Log: lg})
}

// storeMaintenance expires stale partial shares, scrubs complete ones at
// a rate that covers the store about once a month, purges the shares of
// tombstones past the window, and applies membership-driven deletion on
// this server's own clock.
func storeMaintenance(ctx context.Context, st *store.Store, active func(identity.Key) bool, grace func() time.Duration, lg *slog.Logger) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st.Available() != nil {
			continue
		}
		if n, err := st.ExpirePartials(7 * 24 * time.Hour); err != nil {
			lg.Warn("partial shares not expired", "err", err)
		} else if n > 0 {
			lg.Info("partial shares expired", "count", n)
		}
		if n, err := st.PurgeTombstones(); err != nil {
			lg.Warn("tombstones not purged", "err", err)
		} else if n > 0 {
			lg.Info("tombstoned objects purged after the window", "count", n)
		}
		if active != nil {
			if purged, err := st.ApplyMembership(active, grace()); err != nil {
				lg.Warn("membership not applied to the store", "err", err)
			} else if len(purged) > 0 {
				lg.Info("members' data purged after the grace period", "members", len(purged))
			}
		}
		shares, _, err := st.Totals()
		if err != nil {
			continue
		}
		limit := int(shares/4320) + 1 // 4320 ticks a month
		if checked, failed, err := st.Scrub(ctx, limit); err != nil && ctx.Err() == nil {
			lg.Warn("scrub failed", "err", err)
		} else if failed > 0 {
			lg.Warn("scrub removed corrupt shares", "checked", checked, "failed", failed)
		}
	}
}

// shareCensus adapts the store's census to the wire for the hub channel.
func shareCensus(st *store.Store) func(since uint64) (*wire.ShareCensus, error) {
	if st == nil {
		return nil
	}
	return func(since uint64) (*wire.ShareCensus, error) {
		if since > 0 {
			if g, err := st.Generation(); err != nil || g == since {
				return nil, err
			}
		}
		d, err := st.Census(since)
		if err != nil {
			return nil, err
		}
		out := &wire.ShareCensus{Since: d.Since, Generation: d.Generation, Added: make([]wire.ShareEntry, 0, len(d.Added)), Removed: make([]wire.ShareKey, 0, len(d.Removed)), Tombstones: make([]wire.TombstoneEntry, 0, len(d.Tombstones))}
		for _, e := range d.Added {
			out.Added = append(out.Added, wire.ShareEntry{StorageIndex: e.StorageIndex, Number: e.Number, Owner: e.Owner, Size: e.Size, K: e.K, N: e.N, DescriptorHash: e.DescriptorHash})
		}
		for _, k := range d.Removed {
			out.Removed = append(out.Removed, wire.ShareKey{StorageIndex: k.StorageIndex, Number: k.Number})
		}
		for _, t := range d.Tombstones {
			out.Tombstones = append(out.Tombstones, wire.TombstoneEntry{StorageIndex: t.StorageIndex, Owner: t.Owner, DescriptorHash: t.DescriptorHash, Purged: t.Purged})
		}
		return out, nil
	}
}

// manifestPolicy answers the storage server's questions from the manifest.
type manifestPolicy struct{ rc *runtimeConfig }

func (p *manifestPolicy) Network() string { return p.rc.state.Network }

func (p *manifestPolicy) Device() identity.Key { return p.rc.device }

func (p *manifestPolicy) Converting() (int, int, bool) {
	m := p.rc.current()
	if m == nil {
		return 0, 0, false
	}
	e, ok := m.Converting(time.Now())
	return e.K, e.N, ok
}

// memberActive says whether a member is listed and active: what starts
// and clears the store's grace clock for membership-driven deletion.
func (p *manifestPolicy) memberActive(k identity.Key) bool {
	m := p.rc.current()
	if m == nil {
		return true // no manifest in hand is no reason to start a clock
	}
	mem := m.MemberByKey(k)
	return mem != nil && mem.Status == manifest.StatusActive
}

// grace is the manifest's grace_days.
func (p *manifestPolicy) grace() time.Duration {
	if m := p.rc.current(); m != nil && m.Network.GraceDays > 0 {
		return time.Duration(m.Network.GraceDays) * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}

func (p *manifestPolicy) IsHub(k identity.Key) bool {
	m := p.rc.current()
	return m != nil && m.HubByKey(k) != nil
}

func (p *manifestPolicy) MemberStatus(k identity.Key) (bool, bool) {
	m := p.rc.current()
	if m == nil {
		return false, false
	}
	mem := m.MemberByKey(k)
	if mem == nil {
		return false, false
	}
	return mem.Status == "active", mem.Status == "leaving"
}

// OwnerCap is the RFC's per-server bound: budget times this server's share
// of the contribution times the headroom, where budget is the allocation
// times n/k. This server's contribution is its member's, divided among
// that member's storage devices.
func (p *manifestPolicy) OwnerCap(owner identity.Key) int64 {
	m := p.rc.current()
	if m == nil {
		return 0
	}
	mem := m.MemberByKey(owner)
	if mem == nil || m.Network.Encoding.K == 0 {
		return 0
	}
	var total, mine int64
	var myMember string
	devices := map[string]int{}
	for _, s := range m.Storage {
		devices[s.Member]++
		if s.Device == p.rc.device {
			myMember = s.Member
		}
	}
	for _, x := range m.Members {
		total += int64(x.ContributionGB)
		if x.Name == myMember && devices[myMember] > 0 {
			mine = int64(x.ContributionGB) / int64(devices[myMember])
		}
	}
	if total == 0 || mine == 0 {
		return 0
	}
	headroom := int64(m.Network.QuotaHeadroom)
	if headroom <= 0 {
		headroom = 2
	}
	budget := int64(m.Allocation(mem.Name)) << 30 * int64(m.Network.Encoding.N) / int64(m.Network.Encoding.K)
	return budget * mine / total * headroom
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
