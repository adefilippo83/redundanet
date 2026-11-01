package identity

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Roles a device certificate can carry.
const (
	RoleClient  = "client"
	RoleStorage = "storage"
	RoleHub     = "hub"
)

const domainDeviceCert = "device-certificate"

// Certificate binds a device key to a member key for one network. The
// member signing key signs it; it has no validity window, so a node without
// a real-time clock cannot fail it, and it ends by revocation.
type Certificate struct {
	Network  string    `cbor:"network"`
	Device   Key       `cbor:"device"`
	Member   Key       `cbor:"member"`
	Roles    []string  `cbor:"roles"`
	IssuedAt time.Time `cbor:"issued_at"`
	Serial   uint64    `cbor:"serial"` // increases per device certified by this member, so a revocation names one certificate
}

// SignedCertificate is a certificate with the member's signature.
type SignedCertificate struct {
	Certificate Certificate `cbor:"certificate"`
	Signature   []byte      `cbor:"signature"`
}

// Issue certifies a device with the member's seed. The seed is used once and
// not retained.
func Issue(seed Seed, c Certificate) (*SignedCertificate, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	c.Member = seed.MemberKey()
	c.IssuedAt = c.IssuedAt.UTC().Truncate(time.Second)
	body, err := Marshal(c)
	if err != nil {
		return nil, err
	}
	priv := seed.SigningKey()
	return &SignedCertificate{Certificate: c, Signature: signMessage(priv, domainDeviceCert, body)}, nil
}

func (c Certificate) check() error {
	if c.Network == "" {
		return errors.New("certificate: network is empty")
	}
	if c.Device.IsZero() {
		return errors.New("certificate: device key is empty")
	}
	if len(c.Roles) == 0 {
		return errors.New("certificate: no roles")
	}
	for _, r := range c.Roles {
		if r != RoleClient && r != RoleStorage && r != RoleHub {
			return fmt.Errorf("certificate: unknown role %q", r)
		}
	}
	if c.IssuedAt.IsZero() {
		return errors.New("certificate: issued_at is unset")
	}
	return nil
}

// Verify checks the member's signature and the certificate's shape. It says
// nothing about whether the member is in the manifest or the device is
// revoked; that is the membership package's question.
func (s *SignedCertificate) Verify() error {
	if err := s.Certificate.check(); err != nil {
		return err
	}
	if s.Certificate.Member.IsZero() {
		return errors.New("certificate: member key is empty")
	}
	body, err := Marshal(s.Certificate)
	if err != nil {
		return err
	}
	return verifyMessage(s.Certificate.Member, domainDeviceCert, body, s.Signature)
}

// HasRole reports whether the certificate grants a role.
func (s *SignedCertificate) HasRole(role string) bool {
	return slices.Contains(s.Certificate.Roles, role)
}

// Encode is the wire and at-rest form.
func (s *SignedCertificate) Encode() ([]byte, error) { return Marshal(s) }

// DecodeCertificate parses and verifies a certificate.
func DecodeCertificate(b []byte) (*SignedCertificate, error) {
	var s SignedCertificate
	if err := Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	if err := s.Verify(); err != nil {
		return nil, err
	}
	return &s, nil
}
