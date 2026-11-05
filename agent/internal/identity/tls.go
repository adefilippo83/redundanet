package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// ALPN is the protocol every RedundaNet 3 connection negotiates.
const ALPN = "redundanet/1"

// CertificateExtensionOID marks the X.509 extension that carries the device
// certificate: 1.3.6.1.4.1.67508.1.1, under the project's IANA private
// enterprise number 67508 (.1 is RedundaNet 3, .1.1 this extension). The
// certificate inside is versioned by its own encoding, not by the OID.
var CertificateExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 67508, 1, 1}

// provisionalExtensionOID is the arc the extension lived under before the
// enterprise number was assigned: chosen at random under 2.25, short because
// a proper UUID-based arc there has a 128-bit component Go's X.509 parser
// cannot read. Verification accepts both arcs so a fleet moves one device at
// a time.
var provisionalExtensionOID = asn1.ObjectIdentifier{2, 25, 1409274851}

// presentedExtensionOID is the arc a leaf is built under. It is the
// provisional one until every device verifies the enterprise arc, which is
// the release after the one that introduced it (alpha.19); then it becomes
// CertificateExtensionOID, and 3.0.0 drops the provisional arc from
// verification.
var presentedExtensionOID = provisionalExtensionOID

// Peer is what a verified connection tells the application about the other
// side.
type Peer struct {
	Device      Key
	Member      Key
	Certificate *SignedCertificate
}

// Allow decides whether a verified device may connect: the membership
// package answers it from the signed manifest and the revocation list. It is
// called with the member's status unknown to this package.
type Allow func(p Peer) error

// TLSIdentity is a device's wire identity: an X.509 leaf whose key is the
// device key, self-signed, with the member-signed device certificate in a
// critical extension.
type TLSIdentity struct {
	cert tls.Certificate
	id   Key
}

// NewTLSIdentity builds the leaf for a device and its certificate.
func NewTLSIdentity(dev *DeviceKey, sc *SignedCertificate) (*TLSIdentity, error) {
	if sc.Certificate.Device != dev.ID() {
		return nil, errors.New("the certificate is for another device")
	}
	ext, err := sc.Encode()
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dev.ID().String()},
		// No validity window matters here (see Certificate); X.509 needs one,
		// so it is wide and verification never looks at it.
		NotBefore:             time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		ExtraExtensions:       []pkix.Extension{{Id: presentedExtensionOID, Critical: true, Value: ext}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, dev.Private().Public(), dev.Private())
	if err != nil {
		return nil, err
	}
	return &TLSIdentity{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: dev.Private()}, id: dev.ID()}, nil
}

// ServerConfig presents the identity and requires the client to present one
// that allow accepts.
func (t *TLSIdentity) ServerConfig(allow Allow) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{t.cert},
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		ClientAuth:             tls.RequireAnyClientCert,
		VerifyPeerCertificate:  verifyPeer(allow, Key{}),
		SessionTicketsDisabled: true, // no 0-RTT, no resumption: every connection is a full handshake
	}
}

// ClientConfig presents the identity and accepts only the expected server
// device, which allow must also accept.
func (t *TLSIdentity) ClientConfig(expect Key, allow Allow) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{t.cert},
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		InsecureSkipVerify:     true, // chain validation is replaced by verifyPeer, which pins the device key
		VerifyPeerCertificate:  verifyPeer(allow, expect),
		SessionTicketsDisabled: true,
	}
}

// verifyPeer is the one verification function of the RFC's Wire identity:
// the leaf is self-signed by an Ed25519 key, that key is the device named in
// the embedded certificate, the member's signature on the certificate is
// valid, and the application allows the pair.
func verifyPeer(allow Allow, expect Key) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		p, err := PeerFromRaw(rawCerts)
		if err != nil {
			return err
		}
		if !expect.IsZero() && p.Device != expect {
			return fmt.Errorf("connected to device %s, expected %s", p.Device.Short(), expect.Short())
		}
		if allow == nil {
			return errors.New("no allow policy configured")
		}
		return allow(*p)
	}
}

// PeerFromRaw verifies a presented chain and returns the peer. It is what
// QUIC and TLS listeners call after the handshake to learn who connected.
func PeerFromRaw(rawCerts [][]byte) (*Peer, error) {
	if len(rawCerts) != 1 {
		return nil, fmt.Errorf("peer presented %d certificates, want 1", len(rawCerts))
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return nil, err
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("peer key is not Ed25519")
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		return nil, fmt.Errorf("leaf is not self-signed: %w", err)
	}
	// Either arc carries the certificate; the enterprise one wins if a leaf
	// has both.
	var raw []byte
	for _, e := range leaf.Extensions {
		if e.Id.Equal(CertificateExtensionOID) || (raw == nil && e.Id.Equal(provisionalExtensionOID)) {
			raw = e.Value
		}
	}
	if raw == nil {
		return nil, errors.New("leaf carries no device certificate")
	}
	sc, err := DecodeCertificate(raw)
	if err != nil {
		return nil, err
	}
	device := KeyOf(pub)
	if sc.Certificate.Device != device {
		return nil, errors.New("leaf key is not the device the certificate names")
	}
	return &Peer{Device: device, Member: sc.Certificate.Member, Certificate: sc}, nil
}

// PeerFromConnectionState is PeerFromRaw for a finished handshake.
func PeerFromConnectionState(cs tls.ConnectionState) (*Peer, error) {
	raw := make([][]byte, 0, len(cs.PeerCertificates))
	for _, c := range cs.PeerCertificates {
		raw = append(raw, c.Raw)
	}
	return PeerFromRaw(raw)
}
