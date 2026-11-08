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

	st = testState(t, identity.RoleClient, identity.RoleStorage)
	q = must(t, joinRequestURL(prefillFor(st, nil)))
	dev, _ := st.Device()
	if q.Get("device") != dev.ID().String() || q.Has("gb") {
		t.Errorf("laptop with the storage role: %v", q)
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
