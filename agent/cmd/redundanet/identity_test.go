package main

import (
	"strings"
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

// device pair takes only the phrase of the member this device belongs to;
// a device with no identity has nothing to compare a phrase with, and a
// sealed identity is not opened for it.
func TestPairingTakesOnlyThisMembersPhrase(t *testing.T) {
	seed, _ := identity.NewSeed()
	other, _ := identity.NewSeed()
	dev, _ := identity.NewDeviceKey()
	sc, err := identity.Issue(seed, identity.Certificate{Network: "testnet", Device: dev.ID(), Member: seed.MemberKey(), Roles: []string{identity.RoleClient}, IssuedAt: time.Now(), Serial: 1})
	if err != nil {
		t.Fatal(err)
	}
	st := &identity.State{Network: "testnet", Member: seed.MemberKey(), MemberSecret: seed.Secret(), DeviceSeed: dev.Seed(), Certificate: *sc}
	dd := t.TempDir()
	if err := ownPhrase(dd, "testnet", other); err != nil {
		t.Fatalf("no identity here: %v", err)
	}
	if err := identity.Save(identityPath(dd), st, nil); err != nil {
		t.Fatal(err)
	}
	if err := ownPhrase(dd, "testnet", seed); err != nil {
		t.Fatalf("the member's own phrase: %v", err)
	}
	err = ownPhrase(dd, "testnet", other)
	if err == nil || !strings.Contains(err.Error(), other.MemberKey().Short()) || !strings.Contains(err.Error(), seed.MemberKey().Short()) {
		t.Fatalf("another member's phrase: %v", err)
	}
	sealed := t.TempDir()
	if err := identity.Save(identityPath(sealed), st, []byte("a passphrase")); err != nil {
		t.Fatal(err)
	}
	if err := ownPhrase(sealed, "testnet", other); err != nil {
		t.Fatalf("a sealed identity: %v", err)
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
