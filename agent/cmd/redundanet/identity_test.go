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
