// Package identity holds the spike's device identity: an Ed25519 key, the
// device ID derived from it, and the TLS material that presents the key as the
// QUIC identity (RFC 0001, "Wire identity"). The spike has no member keys and
// no device certificates: the leaf is self-signed and the ID is the public key.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"
)

// ALPN is the protocol every spike connection negotiates.
const ALPN = "redundanet-spike/0"

const pemType = "REDUNDANET SPIKE DEVICE KEY"

// ID is a device's Ed25519 public key.
type ID [ed25519.PublicKeySize]byte

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// String encodes the ID as 52 lowercase base32 characters.
func (id ID) String() string { return strings.ToLower(enc.EncodeToString(id[:])) }

// Short is the first eight characters, for logs.
func (id ID) Short() string { return id.String()[:8] }

// Less orders IDs; the smaller ID of a pair dials.
func (id ID) Less(o ID) bool { return bytes.Compare(id[:], o[:]) < 0 }

// IsZero reports an unset ID.
func (id ID) IsZero() bool { return id == ID{} }

// ParseID decodes the string form.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := enc.DecodeString(strings.ToUpper(strings.TrimSpace(s)))
	if err != nil {
		return id, fmt.Errorf("device id %q: %w", s, err)
	}
	if len(b) != len(id) {
		return id, fmt.Errorf("device id %q: %d bytes, want %d", s, len(b), len(id))
	}
	copy(id[:], b)
	return id, nil
}

// Key is a device key with its TLS certificate.
type Key struct {
	ID   ID
	priv ed25519.PrivateKey
	cert tls.Certificate
}

// Load reads the key at path, creating it when the file does not exist.
func Load(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != pemType || len(block.Bytes) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s: not a spike device key", path)
		}
		return fromSeed(block.Bytes)
	case errors.Is(err, os.ErrNotExist):
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		out := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: seed})
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return nil, err
		}
		return fromSeed(seed)
	default:
		return nil, err
	}
}

func fromSeed(seed []byte) (*Key, error) {
	priv := ed25519.NewKeyFromSeed(seed)
	k := &Key{priv: priv}
	copy(k.ID[:], priv.Public().(ed25519.PublicKey))
	cert, err := selfSigned(priv, k.ID)
	if err != nil {
		return nil, err
	}
	k.cert = cert
	return k, nil
}

func selfSigned(priv ed25519.PrivateKey, id ID) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id.String()},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// Allow decides whether a peer with the given ID may connect.
type Allow func(ID) bool

// Any accepts every device: the spike has no manifest to check against.
func Any(ID) bool { return true }

// Only accepts exactly one device.
func Only(want ID) Allow { return func(id ID) bool { return id == want } }

// ServerTLS presents the device key and requires the client to present one.
func (k *Key) ServerTLS(allow Allow) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{k.cert},
		NextProtos:            []string{ALPN},
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifyPeer(allow),
	}
}

// ClientTLS presents the device key and accepts only the expected server.
func (k *Key) ClientTLS(expect ID) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{k.cert},
		NextProtos:            []string{ALPN},
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, // the chain is replaced by verifyPeer, which pins the device key
		VerifyPeerCertificate: verifyPeer(Only(expect)),
	}
}

// verifyPeer replaces chain validation: the leaf must be self-signed by an
// Ed25519 key, and that key must be allowed. Nothing else about the
// certificate matters.
func verifyPeer(allow Allow) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		id, err := idFromRaw(rawCerts)
		if err != nil {
			return err
		}
		if !allow(id) {
			return fmt.Errorf("device %s is not allowed", id.Short())
		}
		return nil
	}
}

func idFromRaw(rawCerts [][]byte) (ID, error) {
	var id ID
	if len(rawCerts) != 1 {
		return id, fmt.Errorf("peer presented %d certificates, want 1", len(rawCerts))
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return id, err
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return id, errors.New("peer key is not Ed25519")
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return id, fmt.Errorf("leaf is not self-signed: %w", err)
	}
	copy(id[:], pub)
	return id, nil
}

// FromTLS returns the device ID a verified connection presented.
func FromTLS(cs tls.ConnectionState) (ID, error) {
	raw := make([][]byte, 0, len(cs.PeerCertificates))
	for _, c := range cs.PeerCertificates {
		raw = append(raw, c.Raw)
	}
	return idFromRaw(raw)
}
