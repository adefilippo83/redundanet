package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// MemberSecretSize is the size of the member secret.
const MemberSecretSize = 32

// MemberSecret is derived from the seed and stored on every device of the
// member, encrypted at rest. The storage engine derives its convergence
// secret and its root key from it with Subkey.
type MemberSecret [MemberSecretSize]byte

// Derivation labels. Changing one changes every key of every member, so
// they are versioned here and nowhere else.
const (
	labelSigning = "redundanet/3/member/sign"
	labelSecret  = "redundanet/3/member/secret"
)

func derive(ikm []byte, label string, n int) []byte {
	out := make([]byte, n)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, nil, []byte(label)), out); err != nil {
		panic(err)
	}
	return out
}

// SigningKey derives the member signing key. Callers use it for one act and
// let it go out of scope; nothing in this package keeps it.
func (s Seed) SigningKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(derive(s[:], labelSigning, ed25519.SeedSize))
}

// MemberKey is the public half of the signing key, the member's identity in
// the manifest.
func (s Seed) MemberKey() Key {
	return KeyOf(s.SigningKey().Public().(ed25519.PublicKey))
}

// Secret derives the member secret.
func (s Seed) Secret() MemberSecret {
	var m MemberSecret
	copy(m[:], derive(s[:], labelSecret, MemberSecretSize))
	return m
}

// Subkey derives a purpose-bound key from the member secret, for example
// "convergence" or "root". The purpose is part of the label, so two purposes
// never share a key.
func (m MemberSecret) Subkey(purpose string, n int) []byte {
	return derive(m[:], "redundanet/3/member/secret/"+purpose, n)
}

// DisplayCode is the ten-group code of a key: what a member reads to a
// friend over the phone and what the operator compares against a join
// request. It is the first 200 bits of a hash of the key, in base32, which
// has no 0, 1 or 8 to confuse with letters, in ten groups of four.
func DisplayCode(k Key) string {
	h := sha256.Sum256(append([]byte("redundanet/3/display-code\x00"), k[:]...))
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:25])
	groups := make([]string, 0, 10)
	for i := 0; i < 40; i += 4 {
		groups = append(groups, s[i:i+4])
	}
	return strings.Join(groups, " ")
}
