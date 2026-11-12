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

// Files kept in the data directory. DescriptorFile is exported so the
// commands can tell whether a device has joined a network by hand.
const (
	DescriptorFile  = "network.yaml"
	fileManifest    = "manifest.yaml"
	fileSignature   = "manifest.sig"
	fileRevocations = "revocations.cbor"
	fileTrust       = "operators.yaml"
	fileAnchor      = "anchor.yaml"
)

// Config wires a Membership.
type Config struct {
	DataDir    string
	Descriptor *Descriptor
	// Anchor says the descriptor given may replace the operator keys this
	// device pinned: the one built into the binary, which the signed
	// release index vouches for, and one a person gave `network join`.
	// Not one that merely came along, such as a pairing offer's. See
	// reanchor.
	Anchor bool
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

	mu      sync.RWMutex
	trusted []identity.Key // operator keys pinned at join, updated only by a manifest they signed
	// pinned is operators.yaml as it was at Open: its presence says the
	// manifest on disk is one this device applied itself.
	pinned []identity.Key
	// After a descriptor replaced the pinned keys (anchor.go): the keys it
	// dropped, which no manifest may bring back, and whether the device
	// still waits for a manifest signed by one of the new keys.
	dropped  []identity.Key
	awaiting bool

	manifest    *manifest.Manifest
	signature   *manifest.Signature
	policy      *manifest.Policy
	revocations []*identity.SignedRevocation
	held        []*identity.SignedRevocation // of members no manifest lists yet; see hold
	seen        map[identity.Key]bool        // devices admitted while the manifest was fresh
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
	// AwaitingOperators is set after a new descriptor replaced the operator
	// keys, until a member list signed by one of the new keys has arrived;
	// the list in force meanwhile is the last one under the earlier keys.
	AwaitingOperators bool `json:"awaiting_operators,omitempty"`
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
		// Before the descriptor on disk is replaced: whether this one
		// changes whom the device trusts.
		if cfg.Anchor {
			if err := m.reanchor(); err != nil {
				return nil, err
			}
		}
		if err := SaveDescriptor(filepath.Join(cfg.DataDir, DescriptorFile), cfg.Descriptor); err != nil {
			return nil, err
		}
	} else {
		d, err := LoadDescriptor(filepath.Join(cfg.DataDir, DescriptorFile))
		if err != nil {
			return nil, fmt.Errorf("no network descriptor: join a network first (%w)", err)
		}
		m.cfg.Descriptor = d
	}
	m.trusted = append([]identity.Key(nil), m.cfg.Descriptor.Operators...)
	m.pinned, _ = loadTrust(filepath.Join(cfg.DataDir, fileTrust))
	if len(m.pinned) > 0 {
		m.trusted = m.pinned
	}
	if a, err := loadAnchor(filepath.Join(cfg.DataDir, fileAnchor)); err == nil {
		m.dropped = a.Dropped
		if a.Pending {
			m.trusted, m.awaiting = a.Operators, true
			m.log.Warn("the operator keys were replaced by a new network descriptor; the member list on disk stays in force until one signed by a new key arrives", "operators", len(a.Operators), "dropped", len(a.Dropped))
		}
	}
	// One record per certificate, whatever an earlier version kept.
	loaded := loadRevocations(filepath.Join(cfg.DataDir, fileRevocations), m.log)
	for _, r := range loaded {
		_, _ = m.keep(r)
	}
	if len(m.revocations) != len(loaded) {
		m.log.Info("revocations on disk reduced to one per certificate", "before", len(loaded), "after", len(m.revocations))
		if err := saveRevocations(filepath.Join(cfg.DataDir, fileRevocations), m.revocations); err != nil {
			m.log.Warn("revocation file not rewritten", "err", err)
		}
	}
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

// What bounds the revocations a device keeps. A member has a handful of
// devices and each certificate is revoked once, so the first is far above
// any honest use; the second is the records that wait for a manifest that
// lists their member.
const (
	maxRevocationsPerMember = 256
	maxHeldRevocations      = 64
)

// ErrTooManyRevocations refuses a record of a member that has
// maxRevocationsPerMember already.
var ErrTooManyRevocations = errors.New("the member has too many revocations already")

func sameCertificate(a, b identity.Revocation) bool {
	return a.Member == b.Member && a.Device == b.Device && a.Serial == b.Serial
}

// listed reports whether the manifest lists the member as active or
// leaving, the rule the share server has for a revocation too.
func listed(man *manifest.Manifest, member identity.Key) bool {
	if man == nil {
		return false
	}
	mem := man.MemberByKey(member)
	return mem != nil && (mem.Status == manifest.StatusActive || mem.Status == manifest.StatusLeaving)
}

// keep puts a record in the list: one per certificate, the earliest date
// when two name the same one, since signatures before that date stay good
// and a later record must not widen that. The caller holds m.mu.
func (m *Membership) keep(r *identity.SignedRevocation) (bool, error) {
	ofMember := 0
	for i, have := range m.revocations {
		if sameCertificate(have.Revocation, r.Revocation) {
			if !r.Revocation.RevokedAt.Before(have.Revocation.RevokedAt) {
				return false, nil
			}
			m.revocations[i] = r
			return true, nil
		}
		if have.Revocation.Member == r.Revocation.Member {
			ofMember++
		}
	}
	if ofMember >= maxRevocationsPerMember {
		return false, ErrTooManyRevocations
	}
	m.revocations = append(m.revocations, r)
	return true, nil
}

// hold keeps a record of a member the manifest does not list, in memory
// only, until a manifest that lists the member applies: a manifest that
// lags must not cost a genuine record, and a key nobody listed must not
// reach the disk. The oldest gives way. The caller holds m.mu.
func (m *Membership) hold(r *identity.SignedRevocation) {
	for _, have := range m.held {
		if have.Revocation == r.Revocation {
			return
		}
	}
	if len(m.held) >= maxHeldRevocations {
		m.held = append(m.held[:0], m.held[1:]...)
	}
	m.held = append(m.held, r)
}

// AddRevocation applies and persists a revocation, and reports whether it
// changed the list: false for one already known, and for one whose member
// the manifest does not list, which only waits.
func (m *Membership) AddRevocation(r *identity.SignedRevocation) (bool, error) {
	if err := r.Verify(); err != nil {
		return false, err
	}
	m.mu.Lock()
	if r.Revocation.Network != m.cfg.Descriptor.Network {
		m.mu.Unlock()
		return false, errors.New("revocation for another network")
	}
	if !listed(m.manifest, r.Revocation.Member) {
		m.hold(r)
		m.mu.Unlock()
		return false, nil
	}
	added, err := m.keep(r)
	if err != nil || !added {
		m.mu.Unlock()
		return false, err
	}
	if m.policy != nil {
		m.policy.AddRevocation(r)
	}
	revs := append([]*identity.SignedRevocation(nil), m.revocations...)
	man := m.manifest
	m.mu.Unlock()
	if err := saveRevocations(filepath.Join(m.cfg.DataDir, fileRevocations), revs); err != nil {
		return false, err
	}
	m.log.Info("revocation applied", "member", r.Revocation.Member.Short(), "device", r.Revocation.Device.Short())
	if m.cfg.OnApply != nil && man != nil {
		m.cfg.OnApply(man)
	}
	return true, nil
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
		st.AwaitingOperators = m.awaiting
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
	awaiting := m.awaiting
	dropped := append([]identity.Key(nil), m.dropped...)
	last := uint64(0)
	if m.manifest != nil {
		last = m.manifest.Revision
		// The very manifest in force, fetched again: nothing to do, also
		// when its signer has left the operators since.
		if persist && !awaiting && s.Revision == last && s.SHA256 == m.signature.SHA256 && s.Signature == m.signature.Signature {
			m.mu.RUnlock()
			return nil
		}
	}
	// The manifest on disk beside pinned keys is one this device applied
	// itself, under the keys it trusted then. Those may be gone since: the
	// manifest's own signer when it named other operators and left, or
	// every earlier key after a new descriptor. So it is checked for being
	// the file that was signed, stays the policy, and names the operators
	// of the next one only when its signer is still among the pinned keys
	// and no descriptor has replaced them.
	own := !persist && len(m.pinned) > 0
	adopt := true
	if own {
		signer, perr := identity.ParseKey(s.Operator)
		if perr != nil {
			m.mu.RUnlock()
			return fmt.Errorf("manifest.sig: operator: %w", perr)
		}
		adopt = !awaiting && hasKey(m.pinned, signer)
		trusted = []identity.Key{signer}
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
		if awaiting {
			return fmt.Errorf("manifest revision %d is the one applied under the earlier operator keys: the new keys must sign a higher revision", last)
		}
		return nil // unchanged
	}
	if !own {
		// A key a descriptor dropped does not come back through a manifest,
		// whoever signed it: neither an older one replayed nor a new one.
		for _, o := range man.Operators {
			for _, d := range dropped {
				if o.Key == d {
					return fmt.Errorf("manifest revision %d lists operator key %s, which a newer network descriptor dropped", man.Revision, d.Short())
				}
			}
		}
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
	// The records that waited for this manifest to list their member.
	var promoted []*identity.SignedRevocation
	stillHeld := m.held[:0]
	for _, r := range m.held {
		if !listed(man, r.Revocation.Member) {
			stillHeld = append(stillHeld, r)
			continue
		}
		if added, _ := m.keep(r); added {
			promoted = append(promoted, r)
		}
	}
	m.held = stillHeld
	revs := append([]*identity.SignedRevocation(nil), m.revocations...)
	m.policy = manifest.NewPolicy(man, m.revocations)
	if adopt {
		m.trusted = ops
	}
	if persist {
		m.awaiting = false
	}
	m.seen = map[identity.Key]bool{}
	m.mu.Unlock()
	if len(promoted) > 0 {
		if err := saveRevocations(filepath.Join(m.cfg.DataDir, fileRevocations), revs); err != nil {
			return err
		}
		for _, r := range promoted {
			m.log.Info("revocation applied", "member", r.Revocation.Member.Short(), "device", r.Revocation.Device.Short())
		}
	}
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
		if awaiting {
			// The wait is over: the pinned keys are the manifest's again.
			if err := saveAnchor(filepath.Join(dir, fileAnchor), anchor{Dropped: dropped}); err != nil {
				return err
			}
			m.log.Info("a member list signed by a new operator key arrived; the earlier keys are gone", "revision", man.Revision)
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
