// Package collector is the hub's status collector for phase 1. It keeps the
// census heartbeats devices send over the hub channel, joins them with the
// manifest and the hub's live channels, and serves the hub's status page
// and status.json.
//
// It publishes only the projection the RFC allows: names, roles, versions,
// liveness, free space, relay traffic. Never an address, a path, or a key
// the manifest does not already carry.
package collector

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Thresholds of the RFC's Health section: a device is down when its
// heartbeat has been missing for ten minutes, dead after the manifest's
// repair_after_hours, three days when the manifest does not say.
const (
	downAfter        = 10 * time.Minute
	defaultDeadAfter = 72 * time.Hour
	saveEvery        = 30 * time.Second
)

// Config wires a collector.
type Config struct {
	Device   identity.Key                                          // this hub
	Manifest func() *manifest.Manifest                             // the current manifest, may return nil
	Live     func() hub.Status                                     // the hub's channels right now; set with SetLive after the hub starts
	Path     string                                                // the census file kept across restarts; "" keeps it in memory
	Fetch    func(ctx context.Context, url string) ([]byte, error) // defaults to an HTTP GET with a timeout
	// Placement is the SQLite file of the placement table; "" keeps it in
	// memory, for tests.
	Placement string
	// Signer is the hub's device key, which signs repair tasks; nil means
	// this hub never schedules repair.
	Signer  *identity.DeviceKey
	Started time.Time
	Now     func() time.Time
	Log     *slog.Logger
	// Pair, when set, answers /pair/ on the status page: the hub's mailbox
	// that relays a pairing between two devices that cannot reach each other.
	Pair http.Handler
}

// entry is what the collector remembers about one device.
type entry struct {
	Member   identity.Key `json:"member"`
	Carrier  string       `json:"carrier"`
	LastSeen time.Time    `json:"last_seen"`
	Census   wire.Census  `json:"census"`
}

// Collector is a running collector.
type Collector struct {
	cfg  Config
	log  *slog.Logger
	tmpl *template.Template

	mu      sync.Mutex
	entries map[identity.Key]*entry
	pl      *placement // the placement table, from the share census
	send    func(device identity.Key, typ byte, v any) error
	rep     repairState
	dirty   bool

	pageMu   sync.Mutex // the served copy; never held with mu
	pageAt   time.Time
	pageJSON []byte
	pageHTML []byte
}

//go:embed templates/status.html
var templates embed.FS

// New loads the census kept on disk, if any.
func New(cfg Config) (*Collector, error) {
	if cfg.Manifest == nil {
		return nil, errors.New("collector: a manifest source is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Started.IsZero() {
		cfg.Started = cfg.Now()
	}
	if cfg.Fetch == nil {
		cfg.Fetch = httpFetch
	}
	pl, err := openPlacement(cfg.Placement)
	if err != nil {
		return nil, err
	}
	c := &Collector{cfg: cfg, log: cfg.Log.With("component", "collector"), entries: map[identity.Key]*entry{}, pl: pl, rep: repairState{hinted: map[objectKey]time.Time{}}}
	funcs := template.FuncMap{"short": short, "since": c.since, "gb": gb, "size": size, "dur": dur, "pct": pct, "plural": plural}
	tmpl, err := template.New("status.html").Funcs(funcs).ParseFS(templates, "templates/status.html")
	if err != nil {
		_ = pl.close()
		return nil, err
	}
	c.tmpl = tmpl
	if cfg.Path != "" {
		if err := c.load(); err != nil {
			_ = pl.close()
			return nil, err
		}
	}
	return c, nil
}

// Close releases the placement table; Save first.
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pl.close()
}

// SetLive gives the collector the hub's live view once the hub exists.
func (c *Collector) SetLive(live func() hub.Status) {
	c.mu.Lock()
	c.cfg.Live = live
	c.mu.Unlock()
}

// SetSend gives the collector the hub's channel writer, for repair tasks
// and release hints; the hub's Send fits. The collector calls it with its
// lock held, so send must never wait for the device: the hub's queues the
// frame and returns.
func (c *Collector) SetSend(send func(device identity.Key, typ byte, v any) error) {
	c.mu.Lock()
	c.send = send
	c.mu.Unlock()
}

// Record keeps a device's heartbeat; the hub calls it.
func (c *Collector) Record(peer identity.Peer, carrier string, cs wire.Census) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[peer.Device] = &entry{Member: peer.Member, Carrier: carrier, LastSeen: c.cfg.Now(), Census: cs}
	c.dirty = true
}

// RecordShares applies a chunk of a storage node's share census to the
// placement table. A chunk since generation zero under a new generation
// replaces what the table held for the device.
func (c *Collector) RecordShares(peer identity.Peer, sc *wire.ShareCensus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pl.apply(peer.Device, sc, c.cfg.Now()); err != nil {
		c.log.Warn("share census not recorded", "device", peer.Device.Short(), "err", err)
	}
}

// Objects classifies every object the placement table knows, with the
// grid summary: what the repair scheduler and the page work from.
func (c *Collector) Objects() ([]Object, *Grid, error) {
	now := c.cfg.Now()
	m := c.cfg.Manifest()
	c.mu.Lock()
	defer c.mu.Unlock()
	alive, servers := c.aliveServers(now, m)
	enc, conv := encodingOfManifest(m)
	return c.pl.compute(now, alive, servers, enc, conv)
}

// encodingOfManifest is the manifest's encoding and transition, zero
// values without a manifest.
func encodingOfManifest(m *manifest.Manifest) (manifest.Encoding, *manifest.Transition) {
	if m == nil {
		return manifest.Encoding{}, nil
	}
	return m.Network.Encoding, m.Network.EncodingTransition
}

// aliveServers lists the manifest's storage servers whose shares count:
// seen within the dead threshold, or on a channel now. A server the
// manifest retired is forgotten with its shares. Called with the lock.
func (c *Collector) aliveServers(now time.Time, m *manifest.Manifest) (map[identity.Key]bool, int) {
	alive := map[identity.Key]bool{}
	if m == nil {
		return alive, 0
	}
	deadAfter := defaultDeadAfter
	if m.Network.RepairAfterHours > 0 {
		deadAfter = time.Duration(m.Network.RepairAfterHours) * time.Hour
	}
	listed := map[identity.Key]bool{}
	for _, s := range m.Storage {
		listed[s.Device] = true
		if e, ok := c.entries[s.Device]; ok && now.Sub(e.LastSeen) <= deadAfter {
			alive[s.Device] = true
		}
	}
	if c.cfg.Live != nil {
		for _, cl := range c.cfg.Live().Clients {
			if k, err := identity.ParseKey(cl.Device); err == nil && listed[k] {
				alive[k] = true
			}
		}
	}
	for _, dev := range c.pl.devices() {
		if !listed[dev] {
			if err := c.pl.forget(dev); err == nil {
				c.log.Info("storage device retired from the manifest; its shares no longer count", "device", dev.Short())
			}
		}
	}
	return alive, len(alive)
}

// Run schedules repair and saves the census until ctx ends, then saves
// once more.
func (c *Collector) Run(ctx context.Context) {
	save := time.NewTicker(saveEvery)
	schedule := time.NewTicker(scheduleEvery)
	defer save.Stop()
	defer schedule.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := c.Save(); err != nil {
				c.log.Warn("census not saved", "err", err)
			}
			return
		case <-schedule.C:
			c.Schedule(ctx)
		case <-save.C:
			if err := c.Save(); err != nil {
				c.log.Warn("census not saved", "err", err)
			}
		}
	}
}

// Health is the collector's own state, for the hub's status line.
type Health struct {
	Devices int `json:"devices"`
}

// Health reports the collector's own state.
func (c *Collector) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Health{Devices: len(c.entries)}
}

type file struct {
	Entries map[string]*entry `json:"entries"`
}

func (c *Collector) load() error {
	data, err := os.ReadFile(c.cfg.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("collector: %s: %w", c.cfg.Path, err)
	}
	for k, e := range f.Entries {
		key, err := identity.ParseKey(k)
		if err != nil {
			continue
		}
		c.entries[key] = e
	}
	return nil
}

// Save writes the census to disk when it changed since the last save.
func (c *Collector) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg.Path == "" || !c.dirty {
		return nil
	}
	f := file{Entries: map[string]*entry{}}
	for k, e := range c.entries {
		f.Entries[k.String()] = e
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.cfg.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.cfg.Path); err != nil {
		return err
	}
	c.dirty = false
	return nil
}

// ---- the projection ----

// Status is what the page and status.json show.
type Status struct {
	Network          string    `json:"network"`
	Hub              string    `json:"hub"`
	HubUptime        string    `json:"hub_uptime"`
	ManifestRevision uint64    `json:"manifest_revision"`
	GeneratedAt      time.Time `json:"generated_at"`
	Devices          []Device  `json:"devices"`
	Members          []Member  `json:"members"`
	Grid             *Grid     `json:"grid,omitempty"`
}

// Device is one device of the network as the collector knows it.
type Device struct {
	Device     string     `json:"device"`
	Name       string     `json:"name,omitempty"` // the storage name from the manifest
	Member     string     `json:"member"`
	Roles      []string   `json:"roles"`
	State      string     `json:"state"` // online, recent, down, dead, never
	Carrier    string     `json:"carrier,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	Version    string     `json:"version,omitempty"`
	OS         string     `json:"os,omitempty"`
	Uptime     uint64     `json:"uptime,omitempty"`
	Manifest   uint64     `json:"manifest,omitempty"`
	FreeBytes  *uint64    `json:"free_bytes,omitempty"`
	TotalBytes *uint64    `json:"total_bytes,omitempty"`
	Storage    string     `json:"storage,omitempty"`  // the store's state: "ok" or why it is not serving
	Shares     uint64     `json:"shares"`             // complete shares the node holds
	Stored     uint64     `json:"stored_bytes"`       // their bytes as accounted
	Verified   string     `json:"verified,omitempty"` // day the oldest integrity check of a share dates from: every share was read back since
	Direct     int        `json:"peers_direct"`
	Relayed    int        `json:"peers_relayed"`
	RelayIn    uint64     `json:"relay_bytes_in"`
	RelayOut   uint64     `json:"relay_bytes_out"`
	Update     string     `json:"update,omitempty"`
}

// Member is one member with the count of its devices and the usage the
// storage nodes account to it.
type Member struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	ContributionGB int    `json:"contribution_gb"`
	AllocationGB   int    `json:"allocation_gb"`
	Devices        int    `json:"devices"`
	Online         int    `json:"online"`
	StoredBytes    int64  `json:"stored_bytes"`                    // bytes of shares servers account to the member
	Objects        int    `json:"objects"`                         // distinct objects with at least one complete share
	LastBackup     string `json:"last_backup,omitempty"`           // day of the newest backup of any device, "" when none
	LastVerified   string `json:"last_verified_restore,omitempty"` // day of the newest passing restore test
	VerifyStreak   int    `json:"verify_streak"`                   // the longest run of weeks in a row with a passing restore test among the member's devices
	UnderPlaced    int    `json:"under_placed"`                    // the member's objects short of a share or of spread
	Unreadable     int    `json:"unreadable"`                      // the member's objects below k shares
	OldEncoding    int    `json:"old_encoding,omitempty"`          // objects still under the encoding a transition retires
	OldBytes       int64  `json:"old_encoding_bytes,omitempty"`    // their bytes, accounted at zero until the deadline
}

// Status computes the projection now.
func (c *Collector) Status() Status {
	now := c.cfg.Now()
	m := c.cfg.Manifest()
	c.mu.Lock()
	live := c.cfg.Live
	entries := make(map[identity.Key]entry, len(c.entries))
	for k, e := range c.entries {
		entries[k] = *e
	}
	enc, conv := encodingOfManifest(m)
	var old *manifest.Encoding
	counting := false
	if conv != nil {
		from := conv.From
		old = &from
		if d, err := conv.DeadlineTime(); err == nil && !now.Before(d) {
			counting = true
		}
	}
	usedBytes, oldBytes, usedObjects, oldObjects, err := c.pl.usage(old, counting)
	if err != nil {
		c.log.Warn("usage not computed", "err", err)
	}
	alive, servers := c.aliveServers(now, m)
	objects, grid, err := c.pl.compute(now, alive, servers, enc, conv)
	if err != nil {
		c.log.Warn("placement not computed", "err", err)
	}
	if grid != nil && m != nil {
		grid.RepairRunning, grid.RepairDone, grid.RepairFailed = c.taskCounts(now)
		out := plan(c.planInput(now, m, objects, servers))
		grid.RepairBlocked, grid.RepairShort = out.blocked, out.short
		grid.Scheduler = c.rep.scheduler
	}
	c.mu.Unlock()

	st := Status{Hub: c.cfg.Device.String(), HubUptime: now.Sub(c.cfg.Started).Round(time.Second).String(), GeneratedAt: now}
	deadAfter := defaultDeadAfter
	type listed struct{ name, member string }
	storage := map[identity.Key]listed{}
	memberName := map[identity.Key]string{}
	if m != nil {
		st.Network = m.Network.Name
		st.ManifestRevision = m.Revision
		if m.Network.RepairAfterHours > 0 {
			deadAfter = time.Duration(m.Network.RepairAfterHours) * time.Hour
		}
		for _, mem := range m.Members {
			memberName[mem.Key] = mem.Name
		}
		for _, s := range m.Storage {
			storage[s.Device] = listed{s.Name, s.Member}
		}
	}
	channels := map[identity.Key]hub.ClientStatus{}
	if live != nil {
		for _, cl := range live().Clients {
			if k, err := identity.ParseKey(cl.Device); err == nil {
				channels[k] = cl
			}
		}
	}

	known := map[identity.Key]bool{}
	for k := range storage {
		known[k] = true
	}
	for k := range entries {
		known[k] = true
	}
	for k := range channels {
		known[k] = true
	}
	perMember := map[string]*Member{}
	lastBackup, lastVerify := map[string]time.Time{}, map[string]time.Time{}
	streaks := map[string]int{}
	for k := range known {
		d := Device{Device: k.String(), State: "never"}
		var memberKey identity.Key
		if l, ok := storage[k]; ok {
			d.Name, d.Member, d.Roles = l.name, l.member, []string{identity.RoleStorage}
		}
		if e, ok := entries[k]; ok {
			memberKey = e.Member
			last := e.LastSeen
			d.LastSeen = &last
			d.Version, d.OS, d.Uptime, d.Manifest, d.Update = e.Census.Version, e.Census.OS, e.Census.Uptime, e.Census.Manifest, e.Census.Update
			if len(e.Census.Roles) > 0 {
				d.Roles = e.Census.Roles
			}
			if s := e.Census.Storage; s != nil {
				free, total := s.FreeBytes, s.TotalBytes
				d.FreeBytes, d.TotalBytes = &free, &total
				d.Storage, d.Shares, d.Stored = s.State, s.Shares, s.Bytes
				if !s.Verified.IsZero() {
					d.Verified = s.Verified.UTC().Format("2006-01-02")
				}
			}
			for _, p := range e.Census.Peers {
				if p.Direct {
					d.Direct++
				} else if p.Relay {
					d.Relayed++
				}
			}
			switch age := now.Sub(e.LastSeen); {
			case age > deadAfter:
				d.State = "dead"
			case age > downAfter:
				d.State = "down"
			default:
				d.State = "recent"
			}
		}
		if cl, ok := channels[k]; ok {
			d.State, d.Carrier, d.RelayIn, d.RelayOut = "online", cl.Carrier, cl.BytesIn, cl.BytesOut
			since := cl.Since
			d.Since = &since
			if mk, err := identity.ParseKey(cl.Member); err == nil {
				memberKey = mk
			}
		}
		if d.Member == "" {
			if n, ok := memberName[memberKey]; ok {
				d.Member = n
			} else if !memberKey.IsZero() {
				d.Member = memberKey.Short()
			}
		}
		if d.Roles == nil {
			d.Roles = []string{}
		}
		st.Devices = append(st.Devices, d)
		if e, ok := entries[k]; ok && e.Census.Backup != nil {
			b := e.Census.Backup
			if !b.Partial && b.LastBackup.After(lastBackup[d.Member]) {
				lastBackup[d.Member] = b.LastBackup
			}
			if b.VerifyOK && b.LastVerify.After(lastVerify[d.Member]) {
				lastVerify[d.Member] = b.LastVerify
			}
			if b.VerifyStreak > streaks[d.Member] {
				streaks[d.Member] = b.VerifyStreak
			}
		}
		if pm := perMember[d.Member]; pm != nil {
			pm.Devices++
			if d.State == "online" {
				pm.Online++
			}
		} else {
			perMember[d.Member] = &Member{Name: d.Member, Devices: 1, Online: boolInt(d.State == "online")}
		}
	}
	rank := map[string]int{"online": 0, "recent": 1, "down": 2, "dead": 3, "never": 4}
	sort.Slice(st.Devices, func(i, j int) bool {
		a, b := st.Devices[i], st.Devices[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if a.Member != b.Member {
			return a.Member < b.Member
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Device < b.Device
	})
	underPlaced, unreadable := map[identity.Key]int{}, map[identity.Key]int{}
	for _, o := range objects {
		if o.Tombstoned {
			continue
		}
		if !o.Placed {
			underPlaced[o.Owner]++
		}
		if !o.Readable {
			unreadable[o.Owner]++
		}
	}
	st.Grid = grid
	if m != nil {
		for _, mem := range m.Members {
			mm := Member{Name: mem.Name, Status: mem.Status, ContributionGB: mem.ContributionGB, AllocationGB: m.Allocation(mem.Name),
				StoredBytes: usedBytes[mem.Key], Objects: usedObjects[mem.Key], UnderPlaced: underPlaced[mem.Key], Unreadable: unreadable[mem.Key],
				OldEncoding: oldObjects[mem.Key], OldBytes: oldBytes[mem.Key]}
			if t := lastBackup[mem.Name]; !t.IsZero() {
				mm.LastBackup = t.UTC().Format("2006-01-02")
			}
			if t := lastVerify[mem.Name]; !t.IsZero() {
				mm.LastVerified = t.UTC().Format("2006-01-02")
				mm.VerifyStreak = streaks[mem.Name]
			}
			if pm := perMember[mem.Name]; pm != nil {
				mm.Devices, mm.Online = pm.Devices, pm.Online
			}
			st.Members = append(st.Members, mm)
		}
	}
	if st.Devices == nil {
		st.Devices = []Device{}
	}
	if st.Members == nil {
		st.Members = []Member{}
	}
	return st
}

// plural gives a word its plural when the count is not one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// pct is a percentage for the page's bars.
func pct(part, whole int) int {
	if whole <= 0 {
		return 100
	}
	return part * 100 / whole
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ---- the page ----

// Handler serves the page at / and the JSON at /status.json.
func (c *Collector) Handler() http.Handler {
	mux := http.NewServeMux()
	if c.cfg.Pair != nil {
		// Per method, so the pattern is more specific than the page's `GET /`.
		mux.Handle("GET /pair/", c.cfg.Pair)
		mux.Handle("POST /pair/", c.cfg.Pair)
	}
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		js, _, err := c.served()
		if err != nil {
			http.Error(w, "status could not be rendered", http.StatusInternalServerError)
			return
		}
		c.headers(w)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(js)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, html, err := c.served()
		if err != nil {
			http.Error(w, "page could not be rendered", http.StatusInternalServerError)
			return
		}
		c.headers(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(html)
	})
	return mux
}

// pageTTL is how long the listener answers from one computed status. The
// page is public, so what a request costs must not be the whole
// projection: however many ask, it is computed once in this long.
const pageTTL = 5 * time.Second

// served is the status as JSON and as the page, computed at most once per
// pageTTL. Requests that arrive while it is being computed wait for that
// one computation.
func (c *Collector) served() (js, html []byte, err error) {
	c.pageMu.Lock()
	defer c.pageMu.Unlock()
	now := c.cfg.Now()
	if c.pageJSON != nil && !now.Before(c.pageAt) && now.Sub(c.pageAt) < pageTTL {
		return c.pageJSON, c.pageHTML, nil
	}
	st := c.Status()
	js, err = json.Marshal(st)
	if err == nil {
		js = append(js, '\n')
		html, err = c.render(st)
	}
	if err != nil {
		c.log.Error("page", "err", err)
		return nil, nil, err
	}
	c.pageAt, c.pageJSON, c.pageHTML = now, js, html
	return js, html, nil
}

// render is the page for a status.
func (c *Collector) render(st Status) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.tmpl.Execute(&buf, pageData(st)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Collector) headers(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "public, max-age=30")
}

// Serve listens on addr and serves until ctx ends or stop is called; stop
// returns once the port is released, which a self-update's candidate
// needs before it binds the same address.
func (c *Collector) Serve(ctx context.Context, addr string) (net.Addr, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			_ = ln.Close()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	c.log.Info("status page up", "addr", ln.Addr())
	return ln.Addr(), stop, nil
}

type page struct {
	Status
	Online   int
	Active   int
	FreeSum  uint64
	TotalSum uint64
	// Verdict is the page's first line: ok when everything stored is fully
	// placed and no device that was seen is away, warn when repair is due
	// or running or a device is away, bad when an object is unreadable,
	// beyond repair or in conflict; "" before the hub tracks placement.
	Verdict string
}

func pageData(st Status) page {
	p := page{Status: st}
	if g := st.Grid; g != nil {
		p.Verdict = "ok"
		if g.UnderPlaced > 0 || g.Repair > 0 || g.RepairRunning > 0 || g.RepairBlocked > 0 {
			p.Verdict = "warn"
		}
		for _, d := range st.Devices {
			if d.State == "recent" || d.State == "down" {
				p.Verdict = "warn"
			}
		}
		if g.Unreadable > 0 || g.BeyondRepair > 0 || g.Conflicts > 0 {
			p.Verdict = "bad"
		}
	}
	for _, d := range st.Devices {
		if d.State == "online" {
			p.Online++
		}
		if d.FreeBytes != nil && d.State != "dead" && d.State != "never" {
			p.FreeSum += *d.FreeBytes
			p.TotalSum += *d.TotalBytes
		}
	}
	for _, m := range st.Members {
		if m.Status == "active" || m.Status == "" {
			p.Active++
		}
	}
	return p
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func (c *Collector) since(v any) string {
	var t time.Time
	switch x := v.(type) {
	case *time.Time:
		if x != nil {
			t = *x
		}
	case time.Time:
		t = x
	}
	if t.IsZero() {
		return "never"
	}
	d := c.cfg.Now().Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("2006-01-02")
}

func gb(b *uint64) string {
	if b == nil {
		return ""
	}
	return size(*b)
}

func size(v any) string {
	var b uint64
	switch x := v.(type) {
	case uint64:
		b = x
	case int64:
		if x > 0 {
			b = uint64(x)
		}
	case int:
		if x > 0 {
			b = uint64(x)
		}
	}
	v2 := float64(b)
	return sizeOf(b, v2)
}

func sizeOf(b uint64, v float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TB", v/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.0f GB", v/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", v/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f kB", v/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func dur(secs uint64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d d", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return strings.TrimSuffix(d.String(), "0s")
}
