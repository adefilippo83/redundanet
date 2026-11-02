package manifest

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// fixture is a small network: alice contributes with two storage devices
// and invited bob, a client-only member sponsored from the reserve.
type fixture struct {
	alice, bob      identity.Seed
	aliceDev        *identity.DeviceKey
	aliceCert       *identity.SignedCertificate
	storage1, stor2 *identity.DeviceKey
	operator        *OperatorKey
	hubKey          *identity.DeviceKey
	vouch           Vouch
}

func seed(b byte) identity.Seed {
	var s identity.Seed
	s[0] = b
	return s
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{alice: seed(1), bob: seed(2)}
	var err error
	if f.aliceDev, err = identity.NewDeviceKey(); err != nil {
		t.Fatal(err)
	}
	f.aliceCert, err = identity.Issue(f.alice, identity.Certificate{Network: "testnet", Device: f.aliceDev.ID(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.storage1, _ = identity.NewDeviceKey()
	f.stor2, _ = identity.NewDeviceKey()
	f.hubKey, _ = identity.NewDeviceKey()
	if f.operator, err = NewOperatorKey(); err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	inv := Invite{Network: "testnet", Inviter: f.alice.MemberKey(), Nonce: nonce, Expiry: time.Now().Add(7 * 24 * time.Hour), Applicant: "bob", AllocationGB: 50}
	isig, _ := SignInvite(f.aliceDev, inv)
	csig, _ := SignConfirmation(f.aliceDev, Confirmation{Network: "testnet", Inviter: f.alice.MemberKey(), Applicant: f.bob.MemberKey(), Nonce: nonce})
	if f.vouch, err = NewVouch("alice", f.aliceCert, inv, isig, csig); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) yaml(t *testing.T, edits ...func(*string)) string {
	t.Helper()
	s := fmt.Sprintf(`schema: 3
revision: 7
network:
  name: testnet
  encoding: {k: 2, n: 4}
  reserve_percent: 15
  grace_days: 30
  min_version: 3.0.0
  signature_max_age_days: 90
  quota_headroom: 2
  objects_per_gb: 1000
  repair_after_hours: 72
operators:
  - name: alice
    key: %s
hubs:
  - name: hub-one
    relay: hub.example.org:4433
    status: https://status.example.org
    join: https://hub.example.org/join
    key: %s
members:
  - name: alice
    key: %s
    contribution_gb: 1000
    joined: 2025-03-01
    status: active
  - name: bob
    key: %s
    contribution_gb: 0
    allocation_gb: 50
    sponsor: {member: reserve, gb: 50}
    vouched_by:
      - member: %s
        device_cert: %s
        nonce: %s
        expiry: %s
        invite_sig: %s
        confirm_sig: %s
    joined: 2026-11-01
    status: active
storage:
  - member: alice
    device: %s
    name: falcon
    udp_port: 4433
  - member: alice
    device: %s
    name: falcon-1
`, f.operator.Public(), f.hubKey.ID(), f.alice.MemberKey(), f.bob.MemberKey(),
		f.vouch.Member, f.vouch.DeviceCert, f.vouch.Nonce, f.vouch.Expiry, f.vouch.InviteSig, f.vouch.ConfirmSig,
		f.storage1.ID(), f.stor2.ID())
	for _, e := range edits {
		e(&s)
	}
	return s
}

func mustParse(t *testing.T, y string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseAndValidateFixture(t *testing.T) {
	f := newFixture(t)
	m := mustParse(t, f.yaml(t))
	if m.Revision != 7 || m.Network.Encoding != (Encoding{2, 4}) || len(m.Members) != 2 || len(m.Storage) != 2 {
		t.Fatalf("parsed shape: %+v", m)
	}
	ps := m.Validate()
	if errs := ps.Errors(); len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	// n=4 with 2 servers and a 15% reserve smaller than alice's 1000 GB
	// are both warnings.
	if len(ps) != 2 {
		t.Fatalf("warnings: %v", ps)
	}
	if got := m.Allocation("alice"); got != 1000*85/100*2/4 {
		t.Fatalf("alice allocation %d", got)
	}
	if got := m.Allocation("bob"); got != 50 {
		t.Fatalf("bob allocation %d", got)
	}
}

func TestTransitionalManifestKeepsV2Keys(t *testing.T) {
	// The live RedundaNet 2 manifest, with the v3 keys appended: what main
	// carries during the migration.
	live, err := os.ReadFile("../../../manifests/manifest.yaml")
	if err != nil {
		t.Skip("live manifest not beside the module")
	}
	live = []byte(strings.ReplaceAll(string(live), "\r\n", "\n")) // a Windows checkout has CRLF
	f := newFixture(t)
	v3 := f.yaml(t)
	// Merge: the v3 fixture's network section lines go under the live one.
	v3 = strings.Replace(v3, "network:\n  name: testnet\n", "", 1)
	v3 = strings.Replace(v3, "  encoding: {k: 2, n: 4}\n  reserve_percent: 15\n  grace_days: 30\n  min_version: 3.0.0\n  signature_max_age_days: 90\n  quota_headroom: 2\n  objects_per_gb: 1000\n  repair_after_hours: 72\n", "", 1)
	liveStr := strings.Replace(string(live), "network:\n  name: redundanet\n", "network:\n  name: redundanet\n  encoding: {k: 2, n: 4}\n  reserve_percent: 15\n  grace_days: 30\n  min_version: 3.0.0\n  signature_max_age_days: 90\n  quota_headroom: 2\n  objects_per_gb: 1000\n  repair_after_hours: 72\n", 1)
	m, err := Parse([]byte(liveStr + v3))
	if err != nil {
		t.Fatal(err)
	}
	if m.Network.Name != "redundanet" || len(m.Members) != 2 {
		t.Fatalf("transitional parse: %+v", m.Network)
	}
	// The vouch is for network testnet, so it fails here; everything else must pass.
	for _, p := range m.Validate().Errors() {
		if !strings.Contains(p.Msg, "vouch") {
			t.Errorf("unexpected error: %s", p.Msg)
		}
	}
}

func TestParseRejectsUnknownAndMissing(t *testing.T) {
	f := newFixture(t)
	cases := map[string]func(*string){
		"unknown top key": func(s *string) { *s += "colour: blue\n" },
		"unknown network key": func(s *string) {
			*s = strings.Replace(*s, "  grace_days: 30\n", "  grace_days: 30\n  colour: blue\n", 1)
		},
		"unknown member key": func(s *string) {
			*s = strings.Replace(*s, "    joined: 2025-03-01\n", "    joined: 2025-03-01\n    colour: blue\n", 1)
		},
		"no schema":      func(s *string) { *s = strings.Replace(*s, "schema: 3\n", "", 1) },
		"wrong schema":   func(s *string) { *s = strings.Replace(*s, "schema: 3\n", "schema: 2\n", 1) },
		"no revision":    func(s *string) { *s = strings.Replace(*s, "revision: 7\n", "", 1) },
		"no encoding":    func(s *string) { *s = strings.Replace(*s, "  encoding: {k: 2, n: 4}\n", "", 1) },
		"bad member key": func(s *string) { *s = strings.Replace(*s, f.bob.MemberKey().String(), "notakey", 1) },
	}
	for name, edit := range cases {
		if _, err := Parse([]byte(f.yaml(t, edit))); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// v2 keys are tolerated.
	ok := f.yaml(t, func(s *string) {
		*s += "introducer_furl: pb://x\nnodes:\n  - name: old\n    internal_ip: 10.0.0.1\n"
		*s = strings.Replace(*s, "  grace_days: 30\n", "  grace_days: 30\n  version: 2.0.0\n  vpn_network: 10.100.0.0/16\n  tahoe: {shares_needed: 2}\n", 1)
	})
	mustParse(t, ok)
}

func TestValidateFindsProblems(t *testing.T) {
	f := newFixture(t)
	cases := map[string]struct {
		edit func(*string)
		want string
	}{
		"duplicate member name": {func(s *string) { *s = strings.Replace(*s, "  - name: bob\n", "  - name: alice\n", 1) }, "duplicated"},
		"leaving without date": {func(s *string) {
			*s = strings.Replace(*s, "    joined: 2025-03-01\n    status: active\n", "    joined: 2025-03-01\n    status: leaving\n", 1)
		}, "leaving_since"},
		"unknown status": {func(s *string) {
			*s = strings.Replace(*s, "    joined: 2025-03-01\n    status: active\n", "    joined: 2025-03-01\n    status: banned\n", 1)
		}, "status must be"},
		"k above servers": {func(s *string) { *s = strings.Replace(*s, "{k: 2, n: 4}", "{k: 3, n: 4}", 1) }, "k=3"},
		"n below k":       {func(s *string) { *s = strings.Replace(*s, "{k: 2, n: 4}", "{k: 4, n: 2}", 1) }, "n at least k"},
		"no operators": {func(s *string) {
			*s = strings.Replace(*s, "operators:\n  - name: alice\n    key: "+f.operator.Public().String()+"\n", "operators: []\n", 1)
		}, "operator key is required"},
		"client without sponsor": {func(s *string) { *s = strings.Replace(*s, "    sponsor: {member: reserve, gb: 50}\n", "", 1) }, "sponsor"},
		"storage of a stranger": {func(s *string) {
			*s = strings.Replace(*s, "  - member: alice\n    device: "+f.stor2.ID().String(), "  - member: carol\n    device: "+f.stor2.ID().String(), 1)
		}, "not a member"},
		"allocation overflow": {func(s *string) {
			*s = strings.Replace(*s, "allocation_gb: 50\n    sponsor: {member: reserve, gb: 50}", "allocation_gb: 900\n    sponsor: {member: reserve, gb: 900}", 1)
		}, "allocations need"},
		"vouch by a stranger": {func(s *string) { *s = strings.Replace(*s, "      - member: alice\n", "      - member: carol\n", 1) }, "not a member"},
		"vouch tampered":      {func(s *string) { *s = strings.Replace(*s, "allocation_gb: 50\n", "allocation_gb: 51\n", 1) }, "vouch"},
		"bad udp port":        {func(s *string) { *s = strings.Replace(*s, "udp_port: 4433", "udp_port: 80", 1) }, "udp_port"},
	}
	for name, c := range cases {
		m := mustParse(t, f.yaml(t, c.edit))
		errs := m.Validate().Errors()
		found := false
		for _, e := range errs {
			if strings.Contains(e.Msg, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: wanted an error containing %q, got %v", name, c.want, errs)
		}
	}
}

func TestSignAndVerify(t *testing.T) {
	f := newFixture(t)
	data := []byte(f.yaml(t))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	sig, err := Sign(f.operator, data, now)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sig.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSignature(enc)
	if err != nil {
		t.Fatal(err)
	}
	trusted := []identity.Key{f.operator.Public()}
	m, err := back.Verify(data, trusted)
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != 7 {
		t.Fatal("verify returned the wrong manifest")
	}
	fr := back.Freshness(m, 6, now.Add(24*time.Hour))
	if !fr.Newer || fr.Expired {
		t.Fatalf("freshness: %+v", fr)
	}
	fr = back.Freshness(m, 7, now.Add(91*24*time.Hour))
	if fr.Newer || !fr.Expired {
		t.Fatalf("freshness after expiry: %+v", fr)
	}

	other, _ := NewOperatorKey()
	if _, err := back.Verify(data, []identity.Key{other.Public()}); err == nil {
		t.Fatal("an untrusted operator verified")
	}
	tampered := []byte(strings.Replace(string(data), "contribution_gb: 1000", "contribution_gb: 9000", 1))
	if _, err := back.Verify(tampered, trusted); err == nil {
		t.Fatal("a changed file verified")
	}
	forged := *back
	forged.Revision = 8
	if _, err := forged.Verify(data, trusted); err == nil {
		t.Fatal("a changed revision verified")
	}
	// Signing refuses a manifest with blocking problems.
	bad := []byte(f.yaml(t, func(s *string) { *s = strings.Replace(*s, "{k: 2, n: 4}", "{k: 4, n: 2}", 1) }))
	if _, err := Sign(f.operator, bad, now); err == nil {
		t.Fatal("an invalid manifest was signed")
	}
}

func TestOperatorKeyFile(t *testing.T) {
	k, _ := NewOperatorKey()
	path := t.TempDir() + "/operator.key"
	if err := k.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := k.Save(path); err == nil {
		t.Fatal("overwrote an existing key")
	}
	back, err := LoadOperatorKey(path)
	if err != nil || back.Public() != k.Public() {
		t.Fatalf("load: %v", err)
	}
}

func peerFor(t *testing.T, s identity.Seed, network string, serial uint64) identity.Peer {
	t.Helper()
	d, _ := identity.NewDeviceKey()
	sc, err := identity.Issue(s, identity.Certificate{Network: network, Device: d.ID(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: serial})
	if err != nil {
		t.Fatal(err)
	}
	return identity.Peer{Device: d.ID(), Member: s.MemberKey(), Certificate: sc}
}

func TestPolicy(t *testing.T) {
	f := newFixture(t)
	m := mustParse(t, f.yaml(t, func(s *string) {
		*s = strings.Replace(*s, "    joined: 2026-11-01\n    status: active\n", "    joined: 2026-11-01\n    status: leaving\n    leaving_since: 2026-11-01\n", 1)
	}))
	p := NewPolicy(m, nil)
	alice := peerFor(t, f.alice, "testnet", 5)
	if err := p.Allow(alice); err != nil {
		t.Fatalf("active member refused: %v", err)
	}
	if err := p.MayWrite(alice); err != nil {
		t.Fatalf("active member may not write: %v", err)
	}
	bob := peerFor(t, f.bob, "testnet", 1)
	if err := p.Allow(bob); err != nil {
		t.Fatalf("leaving member refused: %v", err)
	}
	if err := p.MayWrite(bob); err == nil {
		t.Fatal("leaving member may write")
	}
	if err := p.Allow(peerFor(t, seed(9), "testnet", 1)); err == nil {
		t.Fatal("a stranger connected")
	}
	if err := p.Allow(peerFor(t, f.alice, "othernet", 5)); err == nil {
		t.Fatal("a certificate for another network connected")
	}
	// A hub device, certified by a key that is not a member.
	hubSeed := seed(10)
	hubCert, _ := identity.Issue(hubSeed, identity.Certificate{Network: "testnet", Device: f.hubKey.ID(), Roles: []string{identity.RoleHub}, IssuedAt: time.Now(), Serial: 1})
	hub := identity.Peer{Device: f.hubKey.ID(), Member: hubSeed.MemberKey(), Certificate: hubCert}
	if err := p.Allow(hub); err != nil {
		t.Fatalf("listed hub refused: %v", err)
	}
	// Revocation.
	rv, _ := identity.Revoke(f.alice, identity.Revocation{Network: "testnet", Device: alice.Device, Serial: 5})
	p.AddRevocation(rv)
	if err := p.Allow(alice); err == nil {
		t.Fatal("a revoked device connected")
	}
	other := peerFor(t, f.alice, "testnet", 6)
	if err := p.Allow(other); err != nil {
		t.Fatalf("another device of the same member refused after a revocation: %v", err)
	}
	// A revocation for another network is ignored.
	rv2, _ := identity.Revoke(f.alice, identity.Revocation{Network: "othernet", Device: other.Device, Serial: 6})
	p.AddRevocation(rv2)
	if err := p.Allow(other); err != nil {
		t.Fatal("a revocation for another network was applied")
	}
}

func TestHubTCPAddr(t *testing.T) {
	h := Hub{Relay: "hub.example.org:4433"}
	if h.TCPAddr() != "hub.example.org:443" {
		t.Fatalf("default: %s", h.TCPAddr())
	}
	h = Hub{Relay: "[2001:db8::1]:8443", TCP: "[2001:db8::1]:8443"}
	if h.TCPAddr() != "[2001:db8::1]:8443" {
		t.Fatalf("explicit: %s", h.TCPAddr())
	}
	if h = (Hub{Relay: "[2001:db8::1]:4433"}); h.TCPAddr() != "[2001:db8::1]:443" {
		t.Fatalf("ipv6 default: %s", h.TCPAddr())
	}
	m := &Manifest{Hubs: []Hub{{Name: "hub", Relay: "h:4433", TCP: "nocolon"}}}
	var found bool
	for _, p := range m.Validate() {
		found = found || strings.Contains(p.Msg, "tcp must be host:port")
	}
	if !found {
		t.Fatal("a tcp entry without a port was accepted")
	}
}
