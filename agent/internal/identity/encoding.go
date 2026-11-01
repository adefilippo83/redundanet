package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// Key is a 32-byte Ed25519 public key: a member key or a device key.
type Key [ed25519.PublicKeySize]byte

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// String is the 52-character lowercase base32 form used in the manifest, on
// the wire and in every log line.
func (k Key) String() string { return strings.ToLower(b32.EncodeToString(k[:])) }

// Short is the first eight characters, for logs.
func (k Key) Short() string { return k.String()[:8] }

// IsZero reports an unset key.
func (k Key) IsZero() bool { return k == Key{} }

// Less orders keys bytewise.
func (k Key) Less(o Key) bool { return bytes.Compare(k[:], o[:]) < 0 }

// Public returns the key in the standard library's type.
func (k Key) Public() ed25519.PublicKey { return ed25519.PublicKey(k[:]) }

// ParseKey decodes the string form.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(s)))
	if err != nil {
		return k, fmt.Errorf("key %q: %w", s, err)
	}
	if len(b) != len(k) {
		return k, fmt.Errorf("key %q: %d bytes, want %d", s, len(b), len(k))
	}
	copy(k[:], b)
	return k, nil
}

// KeyOf converts a public key.
func KeyOf(pub ed25519.PublicKey) Key {
	var k Key
	copy(k[:], pub)
	return k
}

// MarshalText and UnmarshalText make Key usable in JSON, YAML and CBOR.
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText parses the string form.
func (k *Key) UnmarshalText(b []byte) error {
	p, err := ParseKey(string(b))
	if err != nil {
		return err
	}
	*k = p
	return nil
}

// Deterministic CBOR: every signed record is encoded the same way by every
// implementation, so a signature made on one device verifies on another.
var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	var err error
	encMode, err = cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	decMode, err = cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		IndefLength:       cbor.IndefLengthForbidden,
		ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
		MaxArrayElements:  1 << 16,
		MaxMapPairs:       1 << 16,
	}.DecMode()
	if err != nil {
		panic(err)
	}
}

// Marshal encodes a record deterministically.
func Marshal(v any) ([]byte, error) { return encMode.Marshal(v) }

// Unmarshal decodes a record strictly: no unknown fields, no duplicate keys,
// no indefinite lengths.
func Unmarshal(b []byte, v any) error { return decMode.Unmarshal(b, v) }

// Signatures are over a domain-separated message, so a signature made for
// one kind of record can never verify as another kind.
func signMessage(priv ed25519.PrivateKey, domain string, body []byte) []byte {
	return ed25519.Sign(priv, domainMessage(domain, body))
}

func verifyMessage(pub Key, domain string, body, sig []byte) error {
	if len(sig) != ed25519.SignatureSize {
		return errors.New("signature has the wrong length")
	}
	if !ed25519.Verify(pub.Public(), domainMessage(domain, body), sig) {
		return fmt.Errorf("%s: signature does not verify", domain)
	}
	return nil
}

func domainMessage(domain string, body []byte) []byte {
	m := make([]byte, 0, len(domain)+1+len(body))
	m = append(m, "redundanet/3/"+domain...)
	m = append(m, 0)
	return append(m, body...)
}
