package main

import (
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func TestPhraseMatchesTheIdentityItCameFrom(t *testing.T) {
	var seed, other identity.Seed
	seed[0], other[0] = 1, 2
	dev, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: seed.MemberKey(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	st := &identity.State{Network: "testnet", Member: seed.MemberKey(), MemberSecret: seed.Secret(), Certificate: *sc}
	if !phraseMatches(seed, st) {
		t.Fatal("the seed the identity came from does not match")
	}
	if phraseMatches(other, st) {
		t.Fatal("another seed matches")
	}
}

// TestAcceptStateChecksTheOfferBeforeWriting: what pairing and
// accept-certificate share refuses an identity made for another device or
// network and writes the right one, removing the pending key.
func TestAcceptStateChecksTheOfferBeforeWriting(t *testing.T) {
	var seed identity.Seed
	seed[0] = 3
	d := t.TempDir()
	dev, err := pendingDeviceKey(d)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(network string, device identity.Key) *identity.State {
		sc, err := identity.Issue(seed, identity.Certificate{Network: network, Device: device, Member: seed.MemberKey(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
		if err != nil {
			t.Fatal(err)
		}
		return &identity.State{Network: network, Member: seed.MemberKey(), MemberSecret: seed.Secret(), Certificate: *sc}
	}
	other, _ := identity.NewDeviceKey()
	if err := acceptState(d, "testnet", dev, issue("testnet", other.ID())); err == nil {
		t.Fatal("an identity for another device was accepted")
	}
	if err := acceptState(d, "testnet", dev, issue("othernet", dev.ID())); err == nil {
		t.Fatal("an identity for another network was accepted")
	}
	if err := acceptState(d, "testnet", dev, issue("testnet", dev.ID())); err != nil {
		t.Fatal(err)
	}
	st, err := identity.Load(identityPath(d), "testnet", nil)
	if err != nil || st.Certificate.Certificate.Device != dev.ID() {
		t.Fatalf("identity after accept: %v", err)
	}
	if _, err := pendingDeviceKey(d); err == nil {
		t.Fatal("a pending key was made beside an identity")
	}
}
