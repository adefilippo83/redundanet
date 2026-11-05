package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

type testDevice struct {
	seed Seed
	dev  *DeviceKey
	cert *SignedCertificate
	tls  *TLSIdentity
}

func newTestDevice(t *testing.T, seed Seed, roles ...string) *testDevice {
	t.Helper()
	d, sc := issueTestCert(t, seed, roles...)
	id, err := NewTLSIdentity(d, sc)
	if err != nil {
		t.Fatal(err)
	}
	return &testDevice{seed: seed, dev: d, cert: sc, tls: id}
}

// handshake runs one TLS connection between a client and a server config
// over loopback and returns what each side learned, or the client's error.
func handshake(t *testing.T, server, client *tls.Config) (serverPeer, clientPeer *Peer, err error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type result struct {
		p   *Peer
		err error
	}
	srv := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srv <- result{nil, err}
			return
		}
		defer c.Close()
		tc := tls.Server(c, server)
		if err := tc.Handshake(); err != nil {
			srv <- result{nil, err}
			return
		}
		p, err := PeerFromConnectionState(tc.ConnectionState())
		srv <- result{p, err}
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tls.Client(c, client)
	if err := tc.Handshake(); err != nil {
		return nil, nil, err
	}
	if tc.ConnectionState().NegotiatedProtocol != ALPN {
		t.Fatalf("ALPN %q", tc.ConnectionState().NegotiatedProtocol)
	}
	cp, err := PeerFromConnectionState(tc.ConnectionState())
	if err != nil {
		return nil, nil, err
	}
	r := <-srv
	return r.p, cp, r.err
}

func allowAll(Peer) error { return nil }

func TestTLSHandshakeBetweenTwoDevices(t *testing.T) {
	var seedB Seed
	seedB[0] = 9
	a := newTestDevice(t, testSeed(), RoleStorage)
	b := newTestDevice(t, seedB, RoleClient)
	var seenByServer Peer
	allow := func(p Peer) error { seenByServer = p; return nil }
	sp, cp, err := handshake(t, a.tls.ServerConfig(allow), b.tls.ClientConfig(a.dev.ID(), allowAll))
	if err != nil {
		t.Fatal(err)
	}
	if sp.Device != b.dev.ID() || sp.Member != seedB.MemberKey() || !sp.Certificate.HasRole(RoleClient) {
		t.Fatal("server learned the wrong peer")
	}
	if cp.Device != a.dev.ID() || cp.Member != testSeed().MemberKey() {
		t.Fatal("client learned the wrong peer")
	}
	if seenByServer.Device != b.dev.ID() {
		t.Fatal("allow was not called with the client")
	}
}

// The extension's arc moved from a provisional one to the enterprise number;
// until every device verifies the new arc, leaves present the old one, and
// either verifies.
func TestTLSExtensionArcs(t *testing.T) {
	var seedB Seed
	seedB[0] = 9
	a := newTestDevice(t, testSeed(), RoleStorage)
	for _, oid := range []asn1.ObjectIdentifier{CertificateExtensionOID, provisionalExtensionOID} {
		ext, _ := a.cert.Encode()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(4), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
			ExtraExtensions: []pkix.Extension{{Id: oid, Critical: true, Value: ext}}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, a.dev.Private().Public(), a.dev.Private())
		p, err := PeerFromRaw([][]byte{der})
		if err != nil || p.Device != a.dev.ID() {
			t.Fatalf("leaf under %v: %v", oid, err)
		}
	}
	// A device presenting the enterprise arc and one presenting the
	// provisional arc shake hands both ways.
	defer func(oid asn1.ObjectIdentifier) { presentedExtensionOID = oid }(presentedExtensionOID)
	presentedExtensionOID = CertificateExtensionOID
	b := newTestDevice(t, seedB, RoleClient)
	if _, _, err := handshake(t, a.tls.ServerConfig(allowAll), b.tls.ClientConfig(a.dev.ID(), allowAll)); err != nil {
		t.Fatalf("new client to old server: %v", err)
	}
	if _, _, err := handshake(t, b.tls.ServerConfig(allowAll), a.tls.ClientConfig(b.dev.ID(), allowAll)); err != nil {
		t.Fatalf("old client to new server: %v", err)
	}
}

func TestTLSRejects(t *testing.T) {
	var seedB Seed
	seedB[0] = 9
	a := newTestDevice(t, testSeed(), RoleStorage)
	b := newTestDevice(t, seedB, RoleClient)

	t.Run("allow denies the client", func(t *testing.T) {
		deny := func(p Peer) error { return errors.New("not in the manifest") }
		if _, _, err := handshake(t, a.tls.ServerConfig(deny), b.tls.ClientConfig(a.dev.ID(), allowAll)); err == nil {
			t.Fatal("handshake succeeded")
		}
	})
	t.Run("client expects another device", func(t *testing.T) {
		if _, _, err := handshake(t, a.tls.ServerConfig(allowAll), b.tls.ClientConfig(b.dev.ID(), allowAll)); err == nil {
			t.Fatal("handshake succeeded")
		}
	})
	t.Run("client allow denies the server", func(t *testing.T) {
		deny := func(p Peer) error { return errors.New("revoked") }
		if _, _, err := handshake(t, a.tls.ServerConfig(allowAll), b.tls.ClientConfig(a.dev.ID(), deny)); err == nil {
			t.Fatal("handshake succeeded")
		}
	})
	t.Run("leaf without a device certificate", func(t *testing.T) {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bare"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
		bare := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}}, NextProtos: []string{ALPN}, MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
		if _, _, err := handshake(t, a.tls.ServerConfig(allowAll), bare); err == nil {
			t.Fatal("handshake succeeded")
		}
	})
	t.Run("leaf carrying another device's certificate", func(t *testing.T) {
		if _, err := NewTLSIdentity(a.dev, b.cert); err == nil {
			t.Fatal("identity built with a foreign certificate")
		}
		// Forge it by hand: leaf key A, extension with B's certificate.
		ext, _ := b.cert.Encode()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
			ExtraExtensions: []pkix.Extension{{Id: CertificateExtensionOID, Critical: true, Value: ext}}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, a.dev.Private().Public(), a.dev.Private())
		if _, err := PeerFromRaw([][]byte{der}); err == nil {
			t.Fatal("a leaf whose key is not the certified device verified")
		}
	})
	t.Run("tampered member signature", func(t *testing.T) {
		forged := *b.cert
		forged.Certificate.Roles = []string{RoleHub}
		ext, _ := forged.Encode()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(3), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
			ExtraExtensions: []pkix.Extension{{Id: CertificateExtensionOID, Critical: true, Value: ext}}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, b.dev.Private().Public(), b.dev.Private())
		if _, err := PeerFromRaw([][]byte{der}); err == nil {
			t.Fatal("a certificate with a broken member signature verified")
		}
	})
	t.Run("two certificates presented", func(t *testing.T) {
		raw := a.tls.cert.Certificate[0]
		if _, err := PeerFromRaw([][]byte{raw, raw}); err == nil {
			t.Fatal("a chain of two verified")
		}
	})
}
