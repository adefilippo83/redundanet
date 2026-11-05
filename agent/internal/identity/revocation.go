package identity

import (
	"errors"
	"fmt"
	"time"
)

const domainRevocation = "device-revocation"

// Revocation retires one device certificate. Signatures the device made
// before RevokedAt stay valid, so what it wrote while trusted keeps
// verifying; from RevokedAt on, servers and peers refuse it.
type Revocation struct {
	Network   string    `cbor:"network"`
	Member    Key       `cbor:"member"`
	Device    Key       `cbor:"device"`
	Serial    uint64    `cbor:"serial"`
	RevokedAt time.Time `cbor:"revoked_at"`
}

// SignedRevocation is a revocation with the member's signature.
type SignedRevocation struct {
	Revocation Revocation `cbor:"revocation"`
	Signature  []byte     `cbor:"signature"`
}

// Revoke signs a revocation with the member's seed.
func Revoke(seed Seed, r Revocation) (*SignedRevocation, error) {
	if r.Network == "" || r.Device.IsZero() {
		return nil, errors.New("revocation: network and device are required")
	}
	r.Member = seed.MemberKey()
	if r.RevokedAt.IsZero() {
		r.RevokedAt = time.Now()
	}
	r.RevokedAt = r.RevokedAt.UTC().Truncate(time.Second)
	body, err := Marshal(r)
	if err != nil {
		return nil, err
	}
	return &SignedRevocation{Revocation: r, Signature: signMessage(seed.SigningKey(), domainRevocation, body)}, nil
}

// Verify checks the member's signature.
func (s *SignedRevocation) Verify() error {
	if s.Revocation.Member.IsZero() || s.Revocation.Device.IsZero() || s.Revocation.Network == "" {
		return errors.New("revocation: member, device and network are required")
	}
	body, err := Marshal(s.Revocation)
	if err != nil {
		return err
	}
	return verifyMessage(s.Revocation.Member, domainRevocation, body, s.Signature)
}

// Covers reports whether the revocation retires the given certificate: the
// one with its serial, or every certificate of the device when the serial
// is 0, which is what a lost device gets.
func (s *SignedRevocation) Covers(c *SignedCertificate) bool {
	r, cc := s.Revocation, c.Certificate
	return r.Network == cc.Network && r.Member == cc.Member && r.Device == cc.Device && (r.Serial == 0 || r.Serial == cc.Serial)
}

// Encode is the wire and at-rest form.
func (s *SignedRevocation) Encode() ([]byte, error) { return Marshal(s) }

// DecodeRevocation parses and verifies a revocation.
func DecodeRevocation(b []byte) (*SignedRevocation, error) {
	var s SignedRevocation
	if err := Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("revocation: %w", err)
	}
	if err := s.Verify(); err != nil {
		return nil, err
	}
	return &s, nil
}
