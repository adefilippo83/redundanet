// Package shareproto is the storage protocol between devices: one QUIC
// stream per request, a length-prefixed CBOR message each way, and the
// blocks of a share streamed after the header. The server applies the
// checks of RFC 0001's "Who may store what" in order and the store does
// the rest; the client resumes a share from where the server stopped.
package shareproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Request types.
const (
	TypePut      = "put"
	TypeGet      = "get"
	TypeHas      = "has"
	TypePutRoot  = "put-root"
	TypeGetRoots = "get-roots"
)

// Statuses a response carries; everything but StatusOK is a refusal the
// client reports as a *StatusError.
const (
	StatusOK           = "ok"
	StatusRefused      = "refused"       // policy: member, owner, role
	StatusQuota        = "quota"         // the owner is at the per-server cap
	StatusNoSpace      = "no-space"      // the server is below its free-space floor
	StatusTombstoned   = "tombstoned"    // deleted by its owner
	StatusDescriptor   = "descriptor"    // another descriptor is held for the storage index
	StatusNotFound     = "not-found"     //
	StatusPartial      = "partial"       // the share is not complete
	StatusLowerVersion = "lower-version" // a root at or above this version is held
	StatusUnavailable  = "unavailable"   // the storage disk is missing
	StatusBad          = "bad-request"   //
	StatusFailed       = "error"         // the server failed
)

// maxMessage bounds a control message; a descriptor with every root and a
// certificate is a few kilobytes.
const maxMessage = 1 << 20

// Request opens every exchange.
type Request struct {
	Type         string                   `cbor:"type"`
	StorageIndex object.StorageIndex      `cbor:"si"`
	Number       uint8                    `cbor:"n"`
	Descriptor   *object.SignedDescriptor `cbor:"descriptor,omitempty"` // put
	From         int                      `cbor:"from"`                 // get: first segment
	To           int                      `cbor:"to"`                   // get: one past the last segment; 0 means all
	HashesOnly   bool                     `cbor:"hashes_only"`          // get: the hash list, no blocks
	Member       identity.Key             `cbor:"member"`               // get-roots
	Root         *RootEnvelope            `cbor:"root,omitempty"`       // put-root
	Task         *wire.RepairTask         `cbor:"task,omitempty"`       // put by a repair worker: the hub's signed order
}

// Response answers a request; for put it precedes the blocks and a second
// one closes the exchange, for get it precedes the blocks.
type Response struct {
	Status     string                   `cbor:"status"`
	Message    string                   `cbor:"message,omitempty"`
	Resume     int                      `cbor:"resume"`               // put: the segment to continue from
	Complete   bool                     `cbor:"complete"`             // put: nothing to send
	Hashes     []object.Hash            `cbor:"hashes,omitempty"`     // get: the share's block hashes
	Descriptor *object.SignedDescriptor `cbor:"descriptor,omitempty"` // get with hashes only: the share's signed descriptor
	Segments   int                      `cbor:"segments"`             // get: how many the object has
	Presence   *Presence                `cbor:"presence,omitempty"`   // has
	Roots      []*RootEnvelope          `cbor:"roots,omitempty"`      // get-roots
}

// Presence is what a server holds of an object.
type Presence struct {
	DescriptorHash object.Hash      `cbor:"dhash"`
	Owner          identity.Key     `cbor:"owner"`
	Complete       []uint8          `cbor:"complete"`
	Partial        map[uint8]uint32 `cbor:"partial"`
	Tombstoned     bool             `cbor:"tombstoned"`
}

// RootEnvelope is a device's root record as it travels and is stored:
// the signed envelope of RFC 0001's "Mutable state". The ciphertext is the
// root's content under the member's root key; servers never see inside.
type RootEnvelope struct {
	Network     string                     `cbor:"network"`
	Member      identity.Key               `cbor:"member"`
	Device      identity.Key               `cbor:"device"`
	Version     uint64                     `cbor:"version"`
	Ciphertext  []byte                     `cbor:"ciphertext"`
	Signature   []byte                     `cbor:"signature"`
	Certificate identity.SignedCertificate `cbor:"certificate"`
}

type rootBody struct {
	Network    string       `cbor:"network"`
	Member     identity.Key `cbor:"member"`
	Device     identity.Key `cbor:"device"`
	Version    uint64       `cbor:"version"`
	Ciphertext []byte       `cbor:"ciphertext"`
}

const domainRoot = "root"

// SignRoot makes a device's root envelope.
func SignRoot(network string, dev *identity.DeviceKey, cert *identity.SignedCertificate, version uint64, ciphertext []byte) (*RootEnvelope, error) {
	if cert == nil || cert.Certificate.Device != dev.ID() {
		return nil, errors.New("shareproto: the certificate is not this device's")
	}
	e := &RootEnvelope{Network: network, Member: cert.Certificate.Member, Device: dev.ID(), Version: version, Ciphertext: ciphertext, Certificate: *cert}
	body, err := identity.Marshal(rootBody{e.Network, e.Member, e.Device, e.Version, e.Ciphertext})
	if err != nil {
		return nil, err
	}
	e.Signature = dev.Sign(domainRoot, body)
	return e, nil
}

// Verify checks the envelope: certificate valid for the network and for
// this member and device, signature by the device over the body.
func (e *RootEnvelope) Verify(network string) error {
	if e.Network != network {
		return fmt.Errorf("shareproto: root for network %q", e.Network)
	}
	if err := e.Certificate.Verify(); err != nil {
		return fmt.Errorf("shareproto: root certificate: %w", err)
	}
	c := e.Certificate.Certificate
	if c.Network != network || c.Member != e.Member || c.Device != e.Device {
		return errors.New("shareproto: root certificate does not match the envelope")
	}
	body, err := identity.Marshal(rootBody{e.Network, e.Member, e.Device, e.Version, e.Ciphertext})
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(e.Device, domainRoot, body, e.Signature); err != nil {
		return fmt.Errorf("shareproto: root signature: %w", err)
	}
	return nil
}

// Encode is the stored form of the envelope.
func (e *RootEnvelope) Encode() ([]byte, error) { return identity.Marshal(e) }

// DecodeRoot parses a stored envelope.
func DecodeRoot(b []byte) (*RootEnvelope, error) {
	var e RootEnvelope
	if err := identity.UnmarshalLenient(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// StatusError is a refusal the server sent.
type StatusError struct {
	Status  string
	Message string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return "shareproto: " + e.Status
	}
	return "shareproto: " + e.Status + ": " + e.Message
}

// ---- framing ----

func writeMsg(w io.Writer, v any) error {
	b, err := identity.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxMessage {
		return errors.New("shareproto: message too large")
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func readMsg(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxMessage {
		return fmt.Errorf("shareproto: message of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return identity.UnmarshalLenient(b, v)
}
