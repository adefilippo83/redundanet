package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	bip39 "github.com/tyler-smith/go-bip39"
)

// SeedSize is 128 bits: twelve words.
const SeedSize = 16

// Seed is a member's root secret. It exists in memory while a member-level
// act needs it and is never written anywhere by this package.
type Seed [SeedSize]byte

// NewSeed draws a seed from the system's random source.
func NewSeed() (Seed, error) {
	var s Seed
	if _, err := rand.Read(s[:]); err != nil {
		return s, err
	}
	return s, nil
}

// Phrase is the seed as twelve BIP39 English words. The last word carries a
// four-bit checksum, so a typo is caught before anything is derived.
func (s Seed) Phrase() []string {
	m, err := bip39.NewMnemonic(s[:])
	if err != nil {
		panic(err) // 128 bits is always a valid entropy size
	}
	return strings.Fields(m)
}

// ParsePhrase accepts the words in any case and with any spacing.
func ParsePhrase(words []string) (Seed, error) {
	var s Seed
	clean := make([]string, 0, len(words))
	for _, w := range words {
		for _, f := range strings.Fields(strings.ToLower(w)) {
			clean = append(clean, f)
		}
	}
	if len(clean) != 12 {
		return s, fmt.Errorf("the phrase has %d words, it needs 12", len(clean))
	}
	ent, err := bip39.EntropyFromMnemonic(strings.Join(clean, " "))
	if err != nil {
		return s, errors.New("the phrase does not check out: a word is misspelt or in the wrong place")
	}
	copy(s[:], ent)
	return s, nil
}

// Zero overwrites the seed in memory.
func (s *Seed) Zero() {
	for i := range s {
		s[i] = 0
	}
}
