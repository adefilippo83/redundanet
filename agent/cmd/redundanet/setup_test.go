package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
)

func testState(t *testing.T, roles ...string) *identity.State {
	t.Helper()
	var seed identity.Seed
	seed[0] = 7
	dev, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: dev.ID(), Roles: roles, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &identity.State{Network: "testnet", Member: seed.MemberKey(), MemberSecret: seed.Secret(), DeviceSeed: dev.Seed(), Certificate: *sc}
}

// The join form is handed the member key and the storage node on record,
// or a contribution of 0 when there is none; a laptop with the storage role
// names itself.
func TestJoinRequestPrefill(t *testing.T) {
	st := testState(t, identity.RoleClient)
	u, err := url.Parse(joinRequestURL(prefillFor(st, nil)))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != joinFormURL {
		t.Errorf("url %s", u)
	}
	if q.Get("key") != st.Member.String() || q.Get("gb") != "0" || q.Has("device") {
		t.Errorf("no storage device: %v", q)
	}

	dd := t.TempDir()
	if err := recordDevice(dd, pairedDevice{Device: "laptop2key", Roles: []string{"client"}}); err != nil {
		t.Fatal(err)
	}
	if err := recordDevice(dd, pairedDevice{Device: "nodekey", Name: "nas-1", Roles: []string{"client", "storage"}}); err != nil {
		t.Fatal(err)
	}
	if err := recordDevice(dd, pairedDevice{Device: "nodekey", Name: "nas-1", Roles: []string{"client", "storage"}}); err != nil {
		t.Fatal(err)
	}
	devices, err := loadDevices(dd)
	if err != nil || len(devices) != 2 {
		t.Fatalf("devices %v, %v: want two, the second recorded once", devices, err)
	}
	q = must(t, joinRequestURL(prefillFor(st, devices)))
	if q.Get("device") != "nodekey" || q.Get("devicename") != "nas-1" || q.Has("gb") {
		t.Errorf("with a storage node: %v", q)
	}

	// A laptop with the storage role is not named for the role alone: its
	// member has to have said it is their storage server.
	st = testState(t, identity.RoleClient, identity.RoleStorage)
	dev, _ := st.Device()
	laptop := t.TempDir()
	q = must(t, joinRequestURL(prefillFor(st, withSelf(laptop, st, nil))))
	if q.Has("device") || q.Get("gb") != "0" {
		t.Errorf("laptop with the storage role and no word from its member: %v", q)
	}
	if err := recordServerChoice(laptop, false); err != nil {
		t.Fatal(err)
	}
	if q = must(t, joinRequestURL(prefillFor(st, withSelf(laptop, st, nil)))); q.Has("device") {
		t.Errorf("laptop whose member said no: %v", q)
	}
	if err := recordServerChoice(laptop, true); err != nil {
		t.Fatal(err)
	}
	q = must(t, joinRequestURL(prefillFor(st, withSelf(laptop, st, nil))))
	if q.Get("device") != dev.ID().String() || q.Has("gb") {
		t.Errorf("laptop whose member said it is their server: %v", q)
	}
	// A node it paired comes first.
	if q = must(t, joinRequestURL(prefillFor(st, withSelf(laptop, st, devices)))); q.Get("device") != "nodekey" {
		t.Errorf("a paired node and a laptop that is a server too: %v", q)
	}
	// The word without the role names nothing.
	if got := withSelf(laptop, testState(t, identity.RoleClient), nil); len(got) != 0 {
		t.Errorf("a client said to be a server: %v", got)
	}
}

// A listed member's own computer, said to be their server and not in the
// manifest, is requested like a node, with its certificate; and the page is
// told when the role and the list disagree without that word.
func TestThisComputerAsAServerForAListedMember(t *testing.T) {
	st := testState(t, identity.RoleClient, identity.RoleStorage)
	dev := st.Certificate.Certificate.Device
	other, _ := identity.NewDeviceKey()
	dd := t.TempDir()
	m := &manifest.Manifest{Members: []manifest.Member{{Name: "alice", Key: st.Member, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: other.ID(), Name: "nas-1"}}}

	if p := devicePrefill(m, st.Member, withSelf(dd, st, nil)); p != nil {
		t.Fatalf("a request for a computer its member did not name: %+v", p)
	}
	if got := storageNote(m, dev, st.Certificate.Certificate.Roles, dd); got != "unlisted" {
		t.Errorf("the role, not listed, no word: %q", got)
	}
	if err := recordServerChoice(dd, true); err != nil {
		t.Fatal(err)
	}
	p := devicePrefill(m, st.Member, withSelf(dd, st, nil))
	if p == nil || !p.Device || p.DeviceKey != dev.String() || p.Certificate == "" || p.MemberName != "alice" {
		t.Fatalf("the request for this computer: %+v", p)
	}
	if got := storageNote(m, dev, st.Certificate.Certificate.Roles, dd); got != "" {
		t.Errorf("a server waiting for its request has the request, not a note: %q", got)
	}
	// Listed: nothing to request, nothing to say.
	m.Storage = append(m.Storage, manifest.StorageDevice{Member: "alice", Device: dev, Name: "desk"})
	if p := devicePrefill(m, st.Member, withSelf(dd, st, nil)); p != nil {
		t.Errorf("a listed server is requested again: %+v", p)
	}
	if got := storageNote(m, dev, st.Certificate.Certificate.Roles, dd); got != "" {
		t.Errorf("listed with the role: %q", got)
	}
	// Listed without the role.
	if got := storageNote(m, dev, []string{identity.RoleClient}, dd); got != "norole" {
		t.Errorf("listed without the role: %q", got)
	}
}

// Making an identity with the storage role, on the page or with the flag,
// is the member's word that this computer is their server.
func TestAnIdentityMadeAsAServerRecordsTheChoice(t *testing.T) {
	var seed identity.Seed
	seed[0] = 3
	for _, storage := range []bool{true, false} {
		dd := t.TempDir()
		if _, err := createIdentity(dd, "testnet", seed, storage, 1, nil); err != nil {
			t.Fatal(err)
		}
		if yes, known := serverChoice(dd); yes != storage || known != storage {
			t.Errorf("storage %v: recorded %v, known %v", storage, yes, known)
		}
	}
}

func must(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestMemberListed(t *testing.T) {
	st := testState(t, identity.RoleClient)
	m := &manifest.Manifest{Members: []manifest.Member{{Name: "other"}}}
	if memberListed(m, st.Member) || memberListed(nil, st.Member) {
		t.Error("listed without an entry")
	}
	m.Members = append(m.Members, manifest.Member{Name: "me", Key: st.Member})
	if !memberListed(m, st.Member) {
		t.Error("not listed with an entry")
	}
}

func TestSetupRefusesWithoutATerminalAndIdentity(t *testing.T) {
	err := runSetup([]string{"--data-dir", t.TempDir(), "--network", "testnet", "--print"})
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Errorf("err %v, want the terminal hint", err)
	}
}

// A storage node's certificate goes through the one-line form and back, and
// a listed member's request names the first paired node the manifest lacks.
func TestCertificateRoundTripAndDeviceRequest(t *testing.T) {
	st := testState(t, identity.RoleClient)
	var seed identity.Seed
	seed[0] = 7 // the member of testState
	node, _ := identity.NewDeviceKey()
	sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: node.ID(), Roles: []string{identity.RoleClient, identity.RoleStorage}, IssuedAt: time.Now(), Serial: 9})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encodeCertificate(sc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeCertificate(" " + enc + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if back.Certificate.Device != node.ID() || back.Certificate.Member != st.Member || !back.HasRole(identity.RoleStorage) {
		t.Fatalf("decoded %+v", back.Certificate)
	}
	if _, err := decodeCertificate(enc[:len(enc)-4] + "AAAA"); err == nil {
		t.Error("a tampered certificate verified")
	}

	m := &manifest.Manifest{Members: []manifest.Member{{Name: "me", Key: st.Member}}}
	devices := []pairedDevice{{Device: node.ID().String(), Name: "nas-1", Roles: []string{"client", "storage"}, Certificate: enc}}
	p := devicePrefill(m, st.Member, devices)
	if p == nil || !p.Device || p.MemberName != "me" || p.DeviceKey != node.ID().String() || p.Certificate != enc {
		t.Fatalf("device prefill %+v", p)
	}
	q := must(t, joinRequestURL(*p))
	if q.Get("mode") != "device" || q.Get("name") != "me" || q.Get("cert") != enc || q.Get("devicename") != "nas-1" {
		t.Errorf("device request url: %v", q)
	}
	m.Storage = []manifest.StorageDevice{{Member: "me", Device: node.ID(), Name: "nas-1"}}
	if p := devicePrefill(m, st.Member, devices); p != nil {
		t.Errorf("listed node still requested: %+v", p)
	}
}
