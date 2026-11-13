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
	"time"

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
	TypeDelete   = "delete"   // a signed delete token: the first phase of deletion
	TypeUndelete = "undelete" // the owner's change of mind within the window
	TypeList     = "list"     // the objects this server holds for the asking member
	TypeRevoke   = "revoke"   // a member's signed device revocation, applied and kept by the server
	TypeUsage    = "usage"    // the share bytes this server holds for the asking member
)

// Statuses a response carries; everything but StatusOK is a refusal the
// client reports as a *StatusError.
const (
	StatusOK           = "ok"
	StatusRefused      = "refused"       // policy: member, owner, role
	StatusQuota        = "quota"         // the owner is at the per-server cap
	StatusNoSpace      = "no-space"      // the server is below its free-space floor
	StatusTombstoned   = "tombstoned"    // deleted by its owner
	StatusDescriptor   = "descriptor"    // another descriptor is held for the object, or another member uses the storage index
	StatusNotFound     = "not-found"     //
	StatusPartial      = "partial"       // the share is not complete
	StatusLowerVersion = "lower-version" // a root at or above this version is held
	StatusUnavailable  = "unavailable"   // the storage disk is missing
	StatusRecent       = "recent"        // delete: the owner touched the object within the immunity
	StatusStale        = "stale"         // delete: the token predates the object's last touch
	StatusBad          = "bad-request"   //
	StatusFailed       = "error"         // the server failed
)

// maxMessage bounds a control message; a descriptor with every root and a
// certificate is a few kilobytes.
const maxMessage = 1 << 20

// Request opens every exchange.
type Request struct {
	Type         string                     `cbor:"type"`
	StorageIndex object.StorageIndex        `cbor:"si"`
	Number       uint8                      `cbor:"n"`
	Descriptor   *object.SignedDescriptor   `cbor:"descriptor,omitempty"` // put
	From         int                        `cbor:"from"`                 // get: first segment
	To           int                        `cbor:"to"`                   // get: one past the last segment; 0 means all
	HashesOnly   bool                       `cbor:"hashes_only"`          // get: the hash list, no blocks
	Member       identity.Key               `cbor:"member"`               // get-roots
	Root         *RootEnvelope              `cbor:"root,omitempty"`       // put-root
	Task         *wire.RepairTask           `cbor:"task,omitempty"`       // put by a repair worker: the hub's signed order
	Token        *DeleteToken               `cbor:"token,omitempty"`      // delete
	Revocation   *identity.SignedRevocation `cbor:"revocation,omitempty"` // revoke
	After        object.StorageIndex        `cbor:"after"`                // list: the page starts after this index
	// Owner says whose object a has or a get is about: an object is the
	// pair (owner, storage index). A member's device sends its member key,
	// a repair worker the owner its task names. Zero is a version that
	// does not know the field; the server then answers as Server.subject
	// describes. The other requests name their owner already: a put by its
	// descriptor, a delete by its token, the rest act for the asking
	// member.
	Owner identity.Key `cbor:"owner"`
}

// Response answers a request; for put it precedes the blocks and a second
// one closes the exchange, for get it precedes the blocks.
type Response struct {
	Status     string                   `cbor:"status"`
	Message    string                   `cbor:"message,omitempty"`
	Resume     int                      `cbor:"resume"`               // put: the segment to continue from
	Usage      int64                    `cbor:"usage,omitempty"`      // usage: share bytes held for the member
	Complete   bool                     `cbor:"complete"`             // put: nothing to send
	Hashes     []object.Hash            `cbor:"hashes,omitempty"`     // get: the share's block hashes
	Descriptor *object.SignedDescriptor `cbor:"descriptor,omitempty"` // get with hashes only: the share's signed descriptor
	Segments   int                      `cbor:"segments"`             // get: how many the object has
	Presence   *Presence                `cbor:"presence,omitempty"`   // has, delete
	Roots      []*RootEnvelope          `cbor:"roots,omitempty"`      // get-roots
	Objects    []ListedObject           `cbor:"objects,omitempty"`    // list
	More       bool                     `cbor:"more"`                 // list: another page follows
}

// ListedObject is one object of the asking member as a server lists it.
type ListedObject struct {
	StorageIndex   object.StorageIndex `cbor:"si"`
	DescriptorHash object.Hash         `cbor:"dhash"`
	LastTouched    time.Time           `cbor:"touched"`
	Tombstoned     bool                `cbor:"tombstoned"`
}

// ListPage bounds a list answer.
const ListPage = 1000

// Presence is what a server holds of an object.
type Presence struct {
	DescriptorHash object.Hash      `cbor:"dhash"`
	Owner          identity.Key     `cbor:"owner"`
	Complete       []uint8          `cbor:"complete"`
	Partial        map[uint8]uint32 `cbor:"partial"`
	Tombstoned     bool             `cbor:"tombstoned"`
	Purged         bool             `cbor:"purged,omitempty"` // the tombstone's window has run
}

// DeleteToken is the owner's signed order to tombstone one object, RFC
// 0001's `{network, member, storage index, descriptor hash, issued_at}`
// signed by a device key certified for the member. A server applies it
// only past the immunity and only when it was issued after the object's
// last touch, so a captured token never deletes a copy uploaded later.
type DeleteToken struct {
	Network        string                     `cbor:"network"`
	Member         identity.Key               `cbor:"member"`
	Device         identity.Key               `cbor:"device"`
	StorageIndex   object.StorageIndex        `cbor:"si"`
	DescriptorHash object.Hash                `cbor:"dhash"`
	IssuedAt       time.Time                  `cbor:"issued_at"`
	Signature      []byte                     `cbor:"signature"`
	Certificate    identity.SignedCertificate `cbor:"certificate"`
}

type deleteBody struct {
	Network        string              `cbor:"network"`
	Member         identity.Key        `cbor:"member"`
	Device         identity.Key        `cbor:"device"`
	StorageIndex   object.StorageIndex `cbor:"si"`
	DescriptorHash object.Hash         `cbor:"dhash"`
	IssuedAt       time.Time           `cbor:"issued_at"`
}

const domainDelete = "delete-token"

// SignDelete makes a delete token for one object.
func SignDelete(network string, dev *identity.DeviceKey, cert *identity.SignedCertificate, si object.StorageIndex, dhash object.Hash, issuedAt time.Time) (*DeleteToken, error) {
	if cert == nil || cert.Certificate.Device != dev.ID() {
		return nil, errors.New("shareproto: the certificate is not this device's")
	}
	t := &DeleteToken{Network: network, Member: cert.Certificate.Member, Device: dev.ID(), StorageIndex: si, DescriptorHash: dhash,
		IssuedAt: issuedAt.UTC().Truncate(time.Second), Certificate: *cert}
	body, err := identity.Marshal(deleteBody{t.Network, t.Member, t.Device, t.StorageIndex, t.DescriptorHash, t.IssuedAt})
	if err != nil {
		return nil, err
	}
	t.Signature = dev.Sign(domainDelete, body)
	return t, nil
}

// Verify checks the token: certificate valid for the network and for this
// member and device, signature by the device over the body.
func (t *DeleteToken) Verify(network string) error {
	if t.Network != network {
		return fmt.Errorf("shareproto: delete token for network %q", t.Network)
	}
	if err := t.Certificate.Verify(); err != nil {
		return fmt.Errorf("shareproto: delete token certificate: %w", err)
	}
	c := t.Certificate.Certificate
	if c.Network != network || c.Member != t.Member || c.Device != t.Device {
		return errors.New("shareproto: delete token certificate does not match the token")
	}
	body, err := identity.Marshal(deleteBody{t.Network, t.Member, t.Device, t.StorageIndex, t.DescriptorHash, t.IssuedAt})
	if err != nil {
		return err
	}
	if err := identity.VerifyDevice(t.Device, domainDelete, body, t.Signature); err != nil {
		return fmt.Errorf("shareproto: delete token signature: %w", err)
	}
	return nil
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
