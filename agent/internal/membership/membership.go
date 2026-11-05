package membership

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/semver"
)

// Files kept in the data directory.
const (
	fileDescriptor  = "network.yaml"
	fileManifest    = "manifest.yaml"
	fileSignature   = "manifest.sig"
	fileRevocations = "revocations.cbor"
	fileTrust       = "operators.yaml"
)

// Config wires a Membership.
type Config struct {
	DataDir    string
	Descriptor *Descriptor
	// Version is the running binary's version, checked against the
	// manifest's min_version.
	Version string
	// Fetch overrides the HTTP fetch, for tests and for the hub mirror.
	Fetch func(ctx context.Context, url string) ([]byte, error)
	// Interval between fetches; default 5 minutes.
	Interval time.Duration
	// OnApply runs after a new manifest or revocation changed the policy.
	OnApply func(m *manifest.Manifest)
	Log     *slog.Logger
}

// Membership is the live view.
type Membership struct {
	cfg Config
	log *slog.Logger

	mu          sync.RWMutex
	trusted     []identity.Key // operator keys pinned at join, updated only by a manifest they signed
	manifest    *manifest.Manifest
	signature   *manifest.Signature
	policy      *manifest.Policy
	revocations []*identity.SignedRevocation
	seen        map[identity.Key]bool // devices admitted while the manifest was fresh
	lastFetch   time.Time
	lastErr     string
	expired     bool
}

// Status is the local page's view of membership.
type Status struct {
	Network     string    `json:"network"`
	Revision    uint64    `json:"revision"`
	SignedAt    time.Time `json:"signed_at"`
	Expired     bool      `json:"expired"`
	Members     int       `json:"members"`
	Hubs        int       `json:"hubs"`
	Revocations int       `json:"revocations"`
	LastFetch   time.Time `json:"last_fetch"`
	LastError   string    `json:"last_error,omitempty"`
	NeedsUpdate string    `json:"needs_update,omitempty"`
}

// Open loads the last good state from the data directory. Without one it
// needs a descriptor (cfg.Descriptor) and a first successful fetch, which
// Start performs; until then Allow refuses everyone but the hubs named in
// the descriptor.
func Open(cfg Config) (*Membership, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Fetch == nil {
		cfg.Fetch = httpFetch
	}
	m := &Membership{cfg: cfg, log: cfg.Log.With("component", "membership"), seen: map[identity.Key]bool{}}
	if cfg.Descriptor != nil {
		if err := SaveDescriptor(filepath.Join(cfg.DataDir, fileDescriptor), cfg.Descriptor); err != nil {
			return nil, err
		}
	} else {
		d, err := LoadDescriptor(filepath.Join(cfg.DataDir, fileDescriptor))
		if err != nil {
			return nil, fmt.Errorf("no network descriptor: join a network first (%w)", err)
		}
		m.cfg.Descriptor = d
	}
	m.trusted = append([]identity.Key(nil), m.cfg.Descriptor.Operators...)
	if t, err := loadTrust(filepath.Join(cfg.DataDir, fileTrust)); err == nil && len(t) > 0 {
		m.trusted = t
	}
	m.revocations = loadRevocations(filepath.Join(cfg.DataDir, fileRevocations), m.log)
	if err := m.loadFromDisk(); err != nil {
		m.log.Info("no manifest on disk yet", "reason", err)
	}
	return m, nil
}

// Start fetches now and then every interval until ctx ends.
func (m *Membership) Start(ctx context.Context) {
	m.Refresh(ctx)
	go func() {
		t := time.NewTicker(m.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Refresh(ctx)
			}
		}
	}()
}

// Descriptor is the network this device joined.
func (m *Membership) Descriptor() *Descriptor { return m.cfg.Descriptor }

// Manifest is the current manifest, nil before the first good one.
func (m *Membership) Manifest() *manifest.Manifest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.manifest
}

// Allow is the transport's policy. It never changes identity, so it can be
// handed out once; what it decides follows the current manifest.
func (m *Membership) Allow(p identity.Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.policy == nil {
		// No manifest yet: only the hubs the descriptor names, so the first
		// fetch through a hub mirror can happen.
		for _, h := range m.cfg.Descriptor.Hubs {
			if h.Key == p.Device {
				return nil
			}
		}
		return errors.New("no manifest yet")
	}
	if err := m.policy.Allow(p); err != nil {
		return err
	}
	if m.expired && !m.seen[p.Device] && m.manifest.HubByKey(p.Device) == nil {
		return fmt.Errorf("the manifest signature is older than %d days: new devices are refused until it is re-signed", m.manifest.Network.SignatureMaxAgeDays)
	}
	m.seen[p.Device] = true
	return nil
}

// MayWrite is the storage rule from the current policy.
func (m *Membership) MayWrite(p identity.Peer) error {
	m.mu.RLock()
	pol := m.policy
	m.mu.RUnlock()
	if pol == nil {
		return errors.New("no manifest yet")
	}
	return pol.MayWrite(p)
}

// AddRevocation applies and persists a revocation.
func (m *Membership) AddRevocation(r *identity.SignedRevocation) error {
	if err := r.Verify(); err != nil {
		return err
	}
	m.mu.Lock()
	if r.Revocation.Network != m.cfg.Descriptor.Network {
		m.mu.Unlock()
		return errors.New("revocation for another network")
	}
	for _, have := range m.revocations {
		if have.Revocation == r.Revocation {
			m.mu.Unlock()
			return nil
		}
	}
	m.revocations = append(m.revocations, r)
	if m.policy != nil {
		m.policy.AddRevocation(r)
	}
	revs := append([]*identity.SignedRevocation(nil), m.revocations...)
	man := m.manifest
	m.mu.Unlock()
	if err := saveRevocations(filepath.Join(m.cfg.DataDir, fileRevocations), revs); err != nil {
		return err
	}
	m.log.Info("revocation applied", "member", r.Revocation.Member.Short(), "device", r.Revocation.Device.Short())
	if m.cfg.OnApply != nil && man != nil {
		m.cfg.OnApply(man)
	}
	return nil
}

// Revocations is the current list: what arrived directly, plus what the
// applied manifest carries.
func (m *Membership) Revocations() []*identity.SignedRevocation {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := append([]*identity.SignedRevocation(nil), m.revocations...)
	if m.manifest != nil {
		out = append(out, m.manifest.SignedRevocations()...)
	}
	return out
}

// Status snapshots the membership.
func (m *Membership) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{Network: m.cfg.Descriptor.Network, Expired: m.expired, Revocations: len(m.revocations), LastFetch: m.lastFetch, LastError: m.lastErr}
	if m.manifest != nil {
		st.Revision = m.manifest.Revision
		st.SignedAt = m.signature.IssuedAt
		st.Members = len(m.manifest.Members)
		st.Hubs = len(m.manifest.Hubs)
		st.Revocations += len(m.manifest.Revocations)
		if v := m.manifest.Network.MinVersion; v != "" && m.cfg.Version != "" && m.cfg.Version != "dev" && semver.Less(m.cfg.Version, v) {
			st.NeedsUpdate = v
		}
	}
	return st
}

// Refresh fetches the manifest and its signature once and applies them if
// they are newer and verify.
func (m *Membership) Refresh(ctx context.Context) {
	repo := strings.TrimRight(m.cfg.Descriptor.Repository, "/")
	if repo == "" {
		return
	}
	data, err := m.cfg.Fetch(ctx, repo+"/manifests/"+fileManifest)
	if err == nil {
		var sig []byte
		sig, err = m.cfg.Fetch(ctx, repo+"/manifests/"+fileSignature)
		if err == nil {
			err = m.apply(data, sig, true)
		}
	}
	m.mu.Lock()
	m.lastFetch = time.Now()
	if err != nil {
		m.lastErr = err.Error()
	} else {
		m.lastErr = ""
	}
	m.mu.Unlock()
	if err != nil {
		m.log.Warn("manifest refresh failed", "err", err)
	}
	m.recheckExpiry()
}

// apply verifies a manifest and makes it current when it is newer. With
// persist set the pair is written to disk.
func (m *Membership) apply(data, sig []byte, persist bool) error {
	s, err := manifest.ParseSignature(sig)
	if err != nil {
		return err
	}
	m.mu.RLock()
	trusted := append([]identity.Key(nil), m.trusted...)
	last := uint64(0)
	if m.manifest != nil {
		last = m.manifest.Revision
	}
	m.mu.RUnlock()
	man, err := s.Verify(data, trusted)
	if err != nil {
		return err
	}
	if man.Network.Name != m.cfg.Descriptor.Network {
		return fmt.Errorf("manifest is for network %q", man.Network.Name)
	}
	if errs := man.Validate().Errors(); len(errs) > 0 {
		return fmt.Errorf("manifest has blocking problems: %s", errs[0].Msg)
	}
	if man.Revision < last {
		return fmt.Errorf("manifest revision %d is older than the applied %d", man.Revision, last)
	}
	if man.Revision == last && persist {
		return nil // unchanged
	}
	// The operators list may change only through a manifest the current keys
	// signed, which Verify just established; from here on the new list is
	// what is trusted.
	var ops []identity.Key
	for _, o := range man.Operators {
		ops = append(ops, o.Key)
	}
	m.mu.Lock()
	m.manifest, m.signature = man, s
	m.policy = manifest.NewPolicy(man, m.revocations)
	m.trusted = ops
	m.seen = map[identity.Key]bool{}
	m.mu.Unlock()
	if persist {
		dir := m.cfg.DataDir
		if err := writeFile(filepath.Join(dir, fileManifest), data, 0o644); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, fileSignature), sig, 0o644); err != nil {
			return err
		}
		if err := saveTrust(filepath.Join(dir, fileTrust), ops); err != nil {
			return err
		}
		m.log.Info("manifest applied", "revision", man.Revision, "members", len(man.Members), "signed", s.IssuedAt.Format(time.RFC3339))
	}
	m.recheckExpiry()
	if m.cfg.OnApply != nil {
		m.cfg.OnApply(man)
	}
	return nil
}

func (m *Membership) loadFromDisk() error {
	dir := m.cfg.DataDir
	data, err := os.ReadFile(filepath.Join(dir, fileManifest))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(dir, fileSignature))
	if err != nil {
		return err
	}
	return m.apply(data, sig, false)
}

func (m *Membership) recheckExpiry() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.manifest == nil {
		return
	}
	fr := m.signature.Freshness(m.manifest, 0, time.Now())
	if fr.Expired && !m.expired {
		m.log.Warn("manifest signature expired: existing devices keep working, new ones are refused until an operator re-signs", "age", fr.Age.Round(time.Hour))
	}
	m.expired = fr.Expired
}

func httpFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "redundanet/3")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}
