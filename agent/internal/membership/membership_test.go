package membership

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/semver"
)

func seed(b byte) identity.Seed {
	var s identity.Seed
	s[0] = b
	return s
}

// repo serves a manifest and its signature like the repository's raw URL.
type repo struct {
	mu   sync.Mutex
	srv  *httptest.Server
	data map[string][]byte
	down bool
}

func newRepo(t *testing.T) *repo {
	r := &repo{data: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.down {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		b, ok := r.data[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *repo) publish(t *testing.T, op *manifest.OperatorKey, yaml string, at time.Time) {
	t.Helper()
	sig, err := manifest.Sign(op, []byte(yaml), at)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := sig.Encode()
	r.mu.Lock()
	r.data["/manifests/manifest.yaml"] = []byte(yaml)
	r.data["/manifests/manifest.sig"] = enc
	r.mu.Unlock()
}

type world struct {
	op, op2      *manifest.OperatorKey
	hub          *identity.DeviceKey
	alice, bob   identity.Seed
	aliceStorage *identity.DeviceKey
}

func newWorld(t *testing.T) *world {
	w := &world{alice: seed(1), bob: seed(2)}
	var err error
	if w.op, err = manifest.NewOperatorKey(); err != nil {
		t.Fatal(err)
	}
	if w.op2, err = manifest.NewOperatorKey(); err != nil {
		t.Fatal(err)
	}
	w.hub, _ = identity.NewDeviceKey()
	w.aliceStorage, _ = identity.NewDeviceKey()
	return w
}

// manifestYAML builds a manifest; withBob adds a second member, ops is the
// operators list to declare.
func (w *world) manifestYAML(revision int, withBob bool, ops ...*manifest.OperatorKey) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema: 3\nrevision: %d\nnetwork:\n  name: testnet\n  encoding: {k: 1, n: 2}\n  signature_max_age_days: 30\noperators:\n", revision)
	for i, o := range ops {
		fmt.Fprintf(&b, "  - name: op%d\n    key: %s\n", i, o.Public())
	}
	fmt.Fprintf(&b, "hubs:\n  - name: hub\n    relay: hub.example.org:4433\n    key: %s\nmembers:\n  - name: alice\n    key: %s\n    contribution_gb: 100\n    joined: 2026-01-01\n    status: active\n", w.hub.ID(), w.alice.MemberKey())
	if withBob {
		fmt.Fprintf(&b, "  - name: bob\n    key: %s\n    contribution_gb: 0\n    allocation_gb: 5\n    sponsor: {member: reserve, gb: 5}\n    joined: 2026-02-01\n    status: active\n", w.bob.MemberKey())
	}
	fmt.Fprintf(&b, "storage:\n  - member: alice\n    device: %s\n    name: falcon\n", w.aliceStorage.ID())
	return b.String()
}

func (w *world) descriptor(r *repo) *Descriptor {
	return &Descriptor{Network: "testnet", Repository: r.srv.URL, Hubs: []HubHint{{Key: w.hub.ID(), Relay: "hub.example.org:4433"}}, Operators: []identity.Key{w.op.Public()}}
}

func peerFor(t *testing.T, s identity.Seed, serial uint64) identity.Peer {
	t.Helper()
	d, _ := identity.NewDeviceKey()
	sc, err := identity.Issue(s, identity.Certificate{Network: "testnet", Device: d.ID(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: serial})
	if err != nil {
		t.Fatal(err)
	}
	return identity.Peer{Device: d.ID(), Member: s.MemberKey(), Certificate: sc}
}

func fetchFrom(r *repo) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, url string) ([]byte, error) { return httpFetch(ctx, url) }
}

func TestDescriptorRoundTrip(t *testing.T) {
	w := newWorld(t)
	d := &Descriptor{Network: "testnet", Repository: "https://raw.example.org/repo/main", Hubs: []HubHint{{Key: w.hub.ID(), Relay: "h:4433"}}, Operators: []identity.Key{w.op.Public()}}
	s, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "rn3:net:") {
		t.Fatal(s)
	}
	back, err := ParseDescriptor("  " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if back.Network != "testnet" || back.Operators[0] != w.op.Public() || back.Hubs[0].Relay != "h:4433" {
		t.Fatalf("%+v", back)
	}
	if _, err := ParseDescriptor("rn3:net:!!!"); err == nil {
		t.Fatal("garbage parsed")
	}
	if _, err := (&Descriptor{Network: "x"}).Encode(); err == nil {
		t.Fatal("a descriptor without operators encoded")
	}
	path := filepath.Join(t.TempDir(), "network.yaml")
	if err := SaveDescriptor(path, d); err != nil {
		t.Fatal(err)
	}
	if l, err := LoadDescriptor(path); err != nil || l.Repository != d.Repository {
		t.Fatalf("load: %v", err)
	}
}

func TestFirstFetchThenOfflineStart(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(1, false, w.op), time.Now())
	dir := t.TempDir()
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptor(r), Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	alice := peerFor(t, w.alice, 1)
	if err := m.Allow(alice); err == nil {
		t.Fatal("a member was allowed before any manifest")
	}
	hub := identity.Peer{Device: w.hub.ID()}
	if err := m.Allow(hub); err != nil {
		t.Fatalf("the descriptor's hub was refused before the manifest: %v", err)
	}
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 1 || st.LastError != "" {
		t.Fatalf("status after fetch: %+v", st)
	}
	if err := m.Allow(alice); err != nil {
		t.Fatalf("alice refused: %v", err)
	}
	if err := m.Allow(peerFor(t, w.bob, 1)); err == nil {
		t.Fatal("bob allowed before he is a member")
	}
	for _, f := range []string{fileManifest, fileSignature, fileTrust, fileDescriptor} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not written", f)
		}
	}
	// A restart with the repository down runs on the last good copy.
	r.mu.Lock()
	r.down = true
	r.mu.Unlock()
	m2, err := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m2.Refresh(context.Background())
	if st := m2.Status(); st.Revision != 1 || st.LastError == "" {
		t.Fatalf("offline restart: %+v", st)
	}
	if err := m2.Allow(alice); err != nil {
		t.Fatalf("alice refused after an offline restart: %v", err)
	}
}

func TestNewerRevisionAppliesOlderIsIgnored(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(5, false, w.op), time.Now())
	applied := 0
	m, err := Open(Config{DataDir: t.TempDir(), Descriptor: w.descriptor(r), Fetch: fetchFrom(r), OnApply: func(*manifest.Manifest) { applied++ }})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	bob := peerFor(t, w.bob, 1)
	if err := m.Allow(bob); err == nil {
		t.Fatal("bob allowed at revision 5")
	}
	r.publish(t, w.op, w.manifestYAML(6, true, w.op), time.Now())
	m.Refresh(context.Background())
	if err := m.Allow(bob); err != nil {
		t.Fatalf("bob refused at revision 6: %v", err)
	}
	if applied != 2 {
		t.Fatalf("OnApply ran %d times", applied)
	}
	// A rollback to an older revision is refused and the newer one stays.
	r.publish(t, w.op, w.manifestYAML(4, false, w.op), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 6 || !strings.Contains(st.LastError, "older") {
		t.Fatalf("after a rollback attempt: %+v", st)
	}
	if err := m.Allow(bob); err != nil {
		t.Fatal("bob lost access to a rollback")
	}
	// The same revision again is a no-op.
	r.publish(t, w.op, w.manifestYAML(6, true, w.op), time.Now())
	m.Refresh(context.Background())
	if applied != 2 {
		t.Fatal("an unchanged manifest was re-applied")
	}
}

func TestUntrustedSignerIsIgnoredAndRotationNeedsATrustedOne(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(1, false, w.op), time.Now())
	dir := t.TempDir()
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptor(r), Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	// A manifest signed by a key nobody pinned changes nothing, even if it
	// lists itself as the operator.
	r.publish(t, w.op2, w.manifestYAML(2, true, w.op2), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 1 || !strings.Contains(st.LastError, "not one this device trusts") {
		t.Fatalf("after a rogue manifest: %+v", st)
	}
	// Rotation: the trusted key signs a manifest that adds op2; from then on
	// op2's signatures count and op's no longer do, after op leaves.
	r.publish(t, w.op, w.manifestYAML(2, false, w.op, w.op2), time.Now())
	m.Refresh(context.Background())
	if m.Status().Revision != 2 {
		t.Fatal("rotation manifest not applied")
	}
	r.publish(t, w.op2, w.manifestYAML(3, true, w.op2), time.Now())
	m.Refresh(context.Background())
	if m.Status().Revision != 3 {
		t.Fatalf("manifest signed by the rotated-in key not applied: %+v", m.Status())
	}
	r.publish(t, w.op, w.manifestYAML(4, false, w.op), time.Now())
	m.Refresh(context.Background())
	if m.Status().Revision != 4-1 {
		t.Fatalf("the rotated-out key still signs: %+v", m.Status())
	}
	// The pinned set survives a restart.
	m2, err := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m2.mu.RLock()
	trusted := m2.trusted
	m2.mu.RUnlock()
	if len(trusted) != 1 || trusted[0] != w.op2.Public() {
		t.Fatalf("pinned operators after restart: %v", trusted)
	}
}

func TestExpiryAdmitsOnlyKnownDevices(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(1, true, w.op), time.Now().Add(-31*24*time.Hour))
	m, err := Open(Config{DataDir: t.TempDir(), Descriptor: w.descriptor(r), Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	if !m.Status().Expired {
		t.Fatal("a 31-day-old signature with a 30-day limit is not expired")
	}
	alice := peerFor(t, w.alice, 1)
	if err := m.Allow(alice); err == nil {
		t.Fatal("a device never seen was admitted on an expired manifest")
	}
	// Re-signing lifts it; devices seen while fresh stay admitted after the
	// next expiry.
	r.publish(t, w.op, w.manifestYAML(2, true, w.op), time.Now())
	m.Refresh(context.Background())
	if err := m.Allow(alice); err != nil {
		t.Fatalf("alice refused on a fresh manifest: %v", err)
	}
	m.mu.Lock()
	m.expired = true
	m.mu.Unlock()
	if err := m.Allow(alice); err != nil {
		t.Fatal("a device seen while fresh was refused after expiry")
	}
	if err := m.Allow(peerFor(t, w.bob, 1)); err == nil {
		t.Fatal("a new device was admitted after expiry")
	}
	if err := m.Allow(identity.Peer{Device: w.hub.ID(), Certificate: alice.Certificate}); err != nil {
		t.Fatal("a listed hub was refused after expiry")
	}
}

func TestRevocationsPersist(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(1, false, w.op), time.Now())
	dir := t.TempDir()
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptor(r), Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	lost := peerFor(t, w.alice, 7)
	if err := m.Allow(lost); err != nil {
		t.Fatal(err)
	}
	rv, _ := identity.Revoke(w.alice, identity.Revocation{Network: "testnet", Device: lost.Device, Serial: 7})
	if err := m.AddRevocation(rv); err != nil {
		t.Fatal(err)
	}
	if err := m.AddRevocation(rv); err != nil {
		t.Fatal("a duplicate revocation errored")
	}
	if err := m.Allow(lost); err == nil {
		t.Fatal("a revoked device was admitted")
	}
	if err := m.Allow(peerFor(t, w.alice, 8)); err != nil {
		t.Fatal("another device of the member was refused")
	}
	other, _ := identity.Revoke(w.alice, identity.Revocation{Network: "othernet", Device: lost.Device, Serial: 7})
	if err := m.AddRevocation(other); err == nil {
		t.Fatal("a revocation for another network was accepted")
	}
	m2, err := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Revocations()) != 1 {
		t.Fatalf("%d revocations after restart", len(m2.Revocations()))
	}
	if err := m2.Allow(lost); err == nil {
		t.Fatal("a revoked device was admitted after a restart")
	}
	// A new manifest keeps the revocations.
	r.publish(t, w.op, w.manifestYAML(2, true, w.op), time.Now())
	m2.Refresh(context.Background())
	if err := m2.Allow(lost); err == nil {
		t.Fatal("a new manifest forgot the revocation")
	}
}

func TestMinVersion(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	y := strings.Replace(w.manifestYAML(1, false, w.op), "  encoding: {k: 1, n: 2}\n", "  encoding: {k: 1, n: 2}\n  min_version: 3.2.0\n", 1)
	r.publish(t, w.op, y, time.Now())
	m, err := Open(Config{DataDir: t.TempDir(), Descriptor: w.descriptor(r), Fetch: fetchFrom(r), Version: "3.1.9"})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	if m.Status().NeedsUpdate != "3.2.0" {
		t.Fatalf("%+v", m.Status())
	}
	if semver.Less("3.10.0", "3.9.0") || !semver.Less("3.9.0", "3.10.0") || !semver.Less("3.2.0-alpha.1", "3.2.0") {
		t.Fatal("version compare is lexical")
	}
}
