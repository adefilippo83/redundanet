// Package pairing adds a device without moving the phrase (RFC 0001,
// "Adding a device", later): the new device shows a one-time code and
// listens; an existing device of the member, where the phrase is typed,
// connects, learns the new device's key, certifies it and hands over the
// identity and the network descriptor. Every message is sealed under a key
// derived from the code, so only the two holders of the code can take part
// and nothing in between learns anything. The code carries the new device's
// addresses, so the two must be able to reach each other: the same network,
// or a public IPv6 path.
package pairing

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

const (
	prefix      = "rn3:pair:"
	version     = 1
	secretBytes = 20 // 160 bits: not guessable within the code's life
	// maxAttempts is how many connections may fail the code before the
	// listener gives up; a wrong code reveals nothing either way.
	maxAttempts = 3
	// connTimeout bounds one exchange.
	connTimeout = 30 * time.Second
	maxMessage  = 1 << 16
	infoLabel   = "redundanet/3/pair/1"
)

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Code is what the new device shows and the existing device types or
// pastes: where to connect and the secret both sides prove they hold.
type Code struct {
	Version int      `cbor:"v"`
	Addrs   []string `cbor:"addrs"`
	Secret  []byte   `cbor:"secret"`
}

// String is the code as shown: a prefix and base32.
func (c *Code) String() string {
	b, _ := identity.Marshal(c)
	return prefix + strings.ToLower(enc.EncodeToString(b))
}

// Parse reads a code as shown.
func Parse(s string) (*Code, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, prefix) {
		return nil, errors.New("not a pairing code: it starts with " + prefix)
	}
	b, err := enc.DecodeString(strings.ToUpper(strings.TrimPrefix(s, prefix)))
	if err != nil {
		return nil, fmt.Errorf("pairing code: %w", err)
	}
	var c Code
	if err := identity.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("pairing code: %w", err)
	}
	if c.Version != version || len(c.Secret) != secretBytes || len(c.Addrs) == 0 {
		return nil, errors.New("pairing code: not one this version makes")
	}
	return &c, nil
}

// Offer is what the existing device hands over: the new device's identity
// (certificate and member secret, as `identity certify` makes it) and the
// network descriptor, so the new device can join at once.
type Offer struct {
	State      []byte `cbor:"state"`
	Descriptor string `cbor:"descriptor,omitempty"`
}

// message is every sealed frame of the exchange. Name is the new device's
// host name, in the hello, so the existing device can record which machine
// it certified and name it in the join request.
type message struct {
	Type    string       `cbor:"type"`
	Nonce   []byte       `cbor:"nonce,omitempty"`
	Network string       `cbor:"network,omitempty"`
	Device  identity.Key `cbor:"device,omitempty"`
	Name    string       `cbor:"name,omitempty"`
	Offer   *Offer       `cbor:"offer,omitempty"`
}

// Listener is the new device's side: one code, one exchange.
type Listener struct {
	ln      net.Listener
	code    *Code
	network string
	dev     identity.Key
	name    string
}

// Listen binds a TCP port and makes the code. bind is ":0" for every
// interface, whose addresses then go into the code; a specific address,
// for tests, goes in alone.
func Listen(network string, dev identity.Key, bind string) (*Listener, error) {
	if bind == "" {
		bind = ":0"
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		_ = ln.Close()
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	var addrs []string
	if host, _, _ := net.SplitHostPort(bind); host != "" && host != "0.0.0.0" && host != "::" {
		addrs = []string{net.JoinHostPort(host, fmt.Sprint(port))}
	} else {
		addrs = localAddrs(port)
	}
	if len(addrs) == 0 {
		_ = ln.Close()
		return nil, errors.New("this device has no address another device could reach")
	}
	name, _ := os.Hostname()
	return &Listener{ln: ln, code: &Code{Version: version, Addrs: addrs, Secret: secret}, network: network, dev: dev, name: name}, nil
}

// Code is what to show.
func (l *Listener) Code() string { return l.code.String() }

// Close stops listening.
func (l *Listener) Close() error { return l.ln.Close() }

// Wait serves connections until one completes the exchange and returns
// its offer, or maxAttempts fail the code, or the context ends.
func (l *Listener) Wait(ctx context.Context) (*Offer, error) {
	go func() {
		<-ctx.Done()
		_ = l.ln.Close()
	}()
	attempts := 0
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		offer, err := l.serve(conn)
		if err == nil {
			_ = l.ln.Close()
			return offer, nil
		}
		attempts++
		if attempts >= maxAttempts {
			_ = l.ln.Close()
			return nil, fmt.Errorf("%d connections failed the code (last: %v); make a new code", attempts, err)
		}
	}
}

func (l *Listener) serve(conn net.Conn) (*Offer, error) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(connTimeout))
	s := newSealer(l.code.Secret)
	open, err := s.read(conn, dirToNew)
	if err != nil {
		return nil, err
	}
	if open.Type != "open" || len(open.Nonce) < 16 {
		return nil, errors.New("the exchange did not start with an open")
	}
	if err := s.write(conn, dirToExisting, &message{Type: "hello", Nonce: open.Nonce, Network: l.network, Device: l.dev, Name: l.name}); err != nil {
		return nil, err
	}
	m, err := s.read(conn, dirToNew)
	if err != nil {
		return nil, err
	}
	if m.Type != "offer" || m.Offer == nil || len(m.Offer.State) == 0 {
		return nil, errors.New("no offer came back")
	}
	_ = s.write(conn, dirToExisting, &message{Type: "done"})
	return m.Offer, nil
}

// Pair is the existing device's side: reach the new device through the
// code, learn its key and name, have certify make the offer, hand it
// over. It returns the device key that was paired and the name the new
// device gave (its host name, possibly empty).
func Pair(ctx context.Context, code string, network string, certify func(device identity.Key, name string) (*Offer, error)) (identity.Key, string, error) {
	c, err := Parse(code)
	if err != nil {
		return identity.Key{}, "", err
	}
	var last error
	for _, addr := range c.Addrs {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
		cancel()
		if err != nil {
			last = err
			continue
		}
		dev, name, err := pairOver(conn, c.Secret, network, certify)
		_ = conn.Close()
		if err == nil {
			return dev, name, nil
		}
		last = err
		if !errors.Is(err, errUnreachable) {
			return identity.Key{}, "", err
		}
	}
	if last == nil {
		last = errors.New("the code names no address")
	}
	return identity.Key{}, "", fmt.Errorf("the new device could not be reached at %s: %w", strings.Join(c.Addrs, ", "), last)
}

var errUnreachable = errors.New("unreachable")

func pairOver(conn net.Conn, secret []byte, network string, certify func(identity.Key, string) (*Offer, error)) (identity.Key, string, error) {
	_ = conn.SetDeadline(time.Now().Add(connTimeout))
	s := newSealer(secret)
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return identity.Key{}, "", err
	}
	if err := s.write(conn, dirToNew, &message{Type: "open", Nonce: nonce}); err != nil {
		return identity.Key{}, "", fmt.Errorf("%w: %v", errUnreachable, err)
	}
	hello, err := s.read(conn, dirToExisting)
	if err != nil {
		return identity.Key{}, "", fmt.Errorf("the device at %s did not answer the code: %w", conn.RemoteAddr(), err)
	}
	if hello.Type != "hello" || string(hello.Nonce) != string(nonce) {
		return identity.Key{}, "", errors.New("the answer did not match the open")
	}
	if hello.Network != network {
		return identity.Key{}, "", fmt.Errorf("the new device waits for network %q, this one is %q", hello.Network, network)
	}
	if hello.Device.IsZero() {
		return identity.Key{}, "", errors.New("the new device sent no key")
	}
	offer, err := certify(hello.Device, hello.Name)
	if err != nil {
		return identity.Key{}, "", err
	}
	if err := s.write(conn, dirToNew, &message{Type: "offer", Offer: offer}); err != nil {
		return identity.Key{}, "", err
	}
	if done, err := s.read(conn, dirToExisting); err != nil || done.Type != "done" {
		return identity.Key{}, "", errors.New("the new device did not confirm the offer")
	}
	return hello.Device, hello.Name, nil
}

// Directions, part of every nonce and of the associated data, so a frame
// never passes for one of the other direction.
const (
	dirToNew      byte = 1
	dirToExisting byte = 2
)

// sealer frames and seals messages: a two-byte length, then AES-GCM under
// the key the code derives, a nonce of direction and counter.
type sealer struct {
	aead  cipher.AEAD
	mu    sync.Mutex
	count map[byte]uint64
}

func newSealer(secret []byte) *sealer {
	key := make([]byte, 32)
	_, _ = io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte(infoLabel)), key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	return &sealer{aead: aead, count: map[byte]uint64{}}
}

func (s *sealer) nonce(dir byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := make([]byte, 12)
	n[0] = dir
	binary.BigEndian.PutUint64(n[4:], s.count[dir])
	s.count[dir]++
	return n
}

func (s *sealer) write(w io.Writer, dir byte, m *message) error {
	body, err := identity.Marshal(m)
	if err != nil {
		return err
	}
	ct := s.aead.Seal(nil, s.nonce(dir), body, []byte{dir})
	if len(ct) > maxMessage {
		return errors.New("pairing message too large")
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(ct)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(ct)
	return err
}

func (s *sealer) read(r io.Reader, dir byte) (*message, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	ct := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, ct); err != nil {
		return nil, err
	}
	body, err := s.aead.Open(nil, s.nonce(dir), ct, []byte{dir})
	if err != nil {
		return nil, errors.New("a message did not open under the code")
	}
	var m message
	if err := identity.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// localAddrs lists the addresses another device might reach this one at:
// every unicast address of an interface that is up, except loopback and
// link-local ones.
func localAddrs(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsMulticast() {
				continue
			}
			out = append(out, net.JoinHostPort(ipn.IP.String(), fmt.Sprint(port)))
		}
	}
	return out
}
