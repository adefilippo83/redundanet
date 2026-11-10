package object

import (
	"errors"
	"fmt"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Descriptor is the public record of an object, stored in the header of
// every share. It holds nothing that varies between two uploads of the same
// object by the same member, so its hash is the same for both and either
// upload's signature is accepted.
type Descriptor struct {
	Version        uint8        `cbor:"version"`
	StorageIndex   StorageIndex `cbor:"storage_index"`
	Size           uint64       `cbor:"size"`
	SegmentSize    uint32       `cbor:"segment_size"`
	K              uint8        `cbor:"k"`
	N              uint8        `cbor:"n"`
	Cipher         uint8        `cbor:"cipher"`
	CiphertextHash Hash         `cbor:"ciphertext_hash"`
	ShareRoots     []Hash       `cbor:"share_roots"`
	Owner          identity.Key `cbor:"owner"`
}

// Encode is the deterministic CBOR of the record.
func (d *Descriptor) Encode() ([]byte, error) { return identity.Marshal(d) }

// Hash identifies the object's descriptor independently of who signed it.
func (d *Descriptor) Hash() (Hash, error) {
	b, err := d.Encode()
	if err != nil {
		return Hash{}, err
	}
	return Sum(b), nil
}

// Geometry is the descriptor's size answers.
func (d *Descriptor) Geometry() Geometry {
	return Geometry{Size: d.Size, SegmentSize: d.SegmentSize, K: d.K}
}

// Params is the encoding the descriptor records.
func (d *Descriptor) Params() Params {
	return Params{K: d.K, N: d.N, SegmentSize: d.SegmentSize, Cipher: d.Cipher}
}

func (d *Descriptor) check() error {
	if d.Version != FormatVersion {
		return fmt.Errorf("object: descriptor version %d is not %d", d.Version, FormatVersion)
	}
	if err := d.Params().check(); err != nil {
		return err
	}
	if d.Size > MaxObjectSize {
		return fmt.Errorf("object: descriptor size %d is above %d", d.Size, uint64(MaxObjectSize))
	}
	if n := d.Geometry().Segments(); n < 1 || n > MaxSegments {
		return fmt.Errorf("object: descriptor has %d segments, at most %d", n, MaxSegments)
	}
	if len(d.ShareRoots) != int(d.N) {
		return fmt.Errorf("object: descriptor lists %d share roots for n=%d", len(d.ShareRoots), d.N)
	}
	if d.Owner.IsZero() {
		return errors.New("object: descriptor has no owner")
	}
	return nil
}

// SignedDescriptor is a descriptor with the uploading device's signature
// and the certificate that chains the device to the owner.
type SignedDescriptor struct {
	Descriptor  Descriptor                 `cbor:"descriptor"`
	Signature   []byte                     `cbor:"signature"`
	Certificate identity.SignedCertificate `cbor:"certificate"`
}

// Sign signs a descriptor with the device's key, carrying its certificate.
func Sign(d Descriptor, dev *identity.DeviceKey, cert *identity.SignedCertificate) (*SignedDescriptor, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	if cert == nil || cert.Certificate.Device != dev.ID() {
		return nil, errors.New("object: the certificate is not this device's")
	}
	if cert.Certificate.Member != d.Owner {
		return nil, errors.New("object: the device is not certified for the descriptor's owner")
	}
	body, err := d.Encode()
	if err != nil {
		return nil, err
	}
	return &SignedDescriptor{Descriptor: d, Signature: dev.Sign(domainDescriptor, body), Certificate: *cert}, nil
}

// Verify checks the record end to end: the certificate is valid and for
// this network, it names the descriptor's owner, the signing device is the
// certified one, and the signature covers the descriptor as encoded.
func (s *SignedDescriptor) Verify(network string) error {
	if err := s.Descriptor.check(); err != nil {
		return err
	}
	if err := s.Certificate.Verify(); err != nil {
		return fmt.Errorf("object: descriptor certificate: %w", err)
	}
	c := s.Certificate.Certificate
	if c.Network != network {
		return fmt.Errorf("object: descriptor certificate is for network %q", c.Network)
	}
	if c.Member != s.Descriptor.Owner {
		return errors.New("object: descriptor signed by a device of another member")
	}
	body, err := s.Descriptor.Encode()
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(c.Device, domainDescriptor, body, s.Signature); err != nil {
		return fmt.Errorf("object: descriptor signature: %w", err)
	}
	return nil
}

// Encode is the CBOR of the signed record, as a share header carries it.
func (s *SignedDescriptor) Encode() ([]byte, error) { return identity.Marshal(s) }

// DecodeSignedDescriptor parses a share header's record; it is not verified.
func DecodeSignedDescriptor(b []byte) (*SignedDescriptor, error) {
	var s SignedDescriptor
	if err := identity.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("object: descriptor: %w", err)
	}
	return &s, nil
}
