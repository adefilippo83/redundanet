package manifest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

const (
	domainManifest  = "manifest"
	operatorPEMType = "REDUNDANET OPERATOR KEY"
)

// OperatorKey is the offline key whose signature makes a manifest take
// effect. It is separate from every member and device key.
type OperatorKey struct {
	priv ed25519.PrivateKey
}

// NewOperatorKey draws a key.
func NewOperatorKey() (*OperatorKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &OperatorKey{priv: priv}, nil
}

// Public is the key listed in the manifest's operators.
func (k *OperatorKey) Public() identity.Key {
	return identity.KeyOf(k.priv.Public().(ed25519.PublicKey))
}

// Save writes the key as a PEM file readable only by its owner.
func (k *OperatorKey) Save(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists", path)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: operatorPEMType, Bytes: k.priv.Seed()})
	return os.WriteFile(path, out, 0o600)
}

// LoadOperatorKey reads a key saved by Save.
func LoadOperatorKey(path string) (*OperatorKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != operatorPEMType || len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not an operator key", path)
	}
	return &OperatorKey{priv: ed25519.NewKeyFromSeed(block.Bytes)}, nil
}

// Signature is `manifest.sig`: the operator's statement that the file with
// this hash, at this revision, is the one nodes should run.
type Signature struct {
	Network   string    `yaml:"network" cbor:"network"`
	Revision  uint64    `yaml:"revision" cbor:"revision"`
	IssuedAt  time.Time `yaml:"issued_at" cbor:"issued_at"`
	SHA256    string    `yaml:"sha256" cbor:"sha256"`
	Operator  string    `yaml:"operator" cbor:"-"`
	Signature string    `yaml:"signature" cbor:"-"`
}

type signedBody struct {
	Network  string    `cbor:"network"`
	Revision uint64    `cbor:"revision"`
	IssuedAt time.Time `cbor:"issued_at"`
	SHA256   string    `cbor:"sha256"`
}

func (s *Signature) body() ([]byte, error) {
	return identity.Marshal(signedBody{Network: s.Network, Revision: s.Revision, IssuedAt: s.IssuedAt, SHA256: s.SHA256})
}

// Sign signs the manifest file as it is on disk. It parses the file first,
// so an operator never signs a manifest the agent could not read.
func Sign(k *OperatorKey, data []byte, now time.Time) (*Signature, error) {
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if errs := m.Validate().Errors(); len(errs) > 0 {
		return nil, fmt.Errorf("the manifest has %d blocking problems; fix them before signing (%s)", len(errs), errs[0].Msg)
	}
	sum := sha256.Sum256(data)
	s := &Signature{Network: m.Network.Name, Revision: m.Revision, IssuedAt: now.UTC().Truncate(time.Second), SHA256: hex.EncodeToString(sum[:]), Operator: k.Public().String()}
	body, err := s.body()
	if err != nil {
		return nil, err
	}
	s.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, append([]byte("redundanet/3/"+domainManifest+"\x00"), body...)))
	return s, nil
}

// Encode is the YAML form written beside the manifest.
func (s *Signature) Encode() ([]byte, error) { return yaml.Marshal(s) }

// ParseSignature reads `manifest.sig`.
func ParseSignature(data []byte) (*Signature, error) {
	var s Signature
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("manifest.sig: %w", err)
	}
	return &s, nil
}

// Verify checks the signature against the manifest bytes and a set of
// trusted operator keys, and returns the parsed manifest. Age and revision
// are the caller's rules (see Freshness); this is the cryptographic check
// and the binding to this exact file.
func (s *Signature) Verify(data []byte, trusted []identity.Key) (*Manifest, error) {
	op, err := identity.ParseKey(s.Operator)
	if err != nil {
		return nil, fmt.Errorf("manifest.sig: operator: %w", err)
	}
	ok := false
	for _, t := range trusted {
		if t == op {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("manifest.sig: operator %s is not one this device trusts", op.Short())
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != s.SHA256 {
		return nil, errors.New("manifest.sig: the manifest file is not the one that was signed")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return nil, fmt.Errorf("manifest.sig: signature: %w", err)
	}
	body, err := s.body()
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(op.Public(), append([]byte("redundanet/3/"+domainManifest+"\x00"), body...), sig) {
		return nil, errors.New("manifest.sig: the signature does not verify")
	}
	m, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if m.Network.Name != s.Network || m.Revision != s.Revision {
		return nil, errors.New("manifest.sig: network or revision differ from the manifest")
	}
	return m, nil
}

// Freshness is what an agent applies after Verify: a manifest must be newer
// than the one it runs, and one past its maximum age keeps the node working
// but admits nobody new (the RFC's expiry rule).
type Freshness struct {
	Newer   bool          // revision above the last applied
	Expired bool          // issued_at older than signature_max_age_days
	Age     time.Duration // since issued_at
}

// Freshness evaluates the signature's age and revision.
func (s *Signature) Freshness(m *Manifest, lastApplied uint64, now time.Time) Freshness {
	age := now.Sub(s.IssuedAt)
	return Freshness{
		Newer:   s.Revision > lastApplied,
		Expired: age > time.Duration(m.Network.SignatureMaxAgeDays)*24*time.Hour,
		Age:     age,
	}
}
