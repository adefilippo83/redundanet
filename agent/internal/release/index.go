// Package release is the signed release index of RFC 0001: what an agent
// consults to learn that a newer binary exists, where to fetch it and what
// its hash must be. The index is signed by the release key, whose public
// half is embedded in the binary and rotates only through its own chain;
// neither the manifest nor an operator key can name a release.
package release

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
	"runtime"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/semver"
)

// Channels.
const (
	ChannelStable = "stable"
	ChannelEarly  = "early"
)

const (
	domainIndex    = "release-index"
	domainRotation = "release-key-rotation"
	keyPEMType     = "REDUNDANET RELEASE KEY"
)

// Index is `releases/index.yaml`.
type Index struct {
	Revision  uint64     `yaml:"revision"`
	Releases  []Release  `yaml:"releases"`
	Rotations []Rotation `yaml:"rotations,omitempty"`
}

// Release is one published version.
type Release struct {
	Version   string           `yaml:"version"`
	Channel   string           `yaml:"channel"`
	Withdrawn bool             `yaml:"withdrawn,omitempty"`
	Notes     string           `yaml:"notes,omitempty"`
	Assets    map[string]Asset `yaml:"assets"` // keyed by os/arch
}

// Asset is one downloadable archive.
type Asset struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Size   int64  `yaml:"size"`
}

// Rotation hands the release key to a successor, signed by the key it
// retires. Verification walks the chain from the embedded key.
type Rotation struct {
	NewKey    identity.Key `yaml:"new_key" cbor:"new_key"`
	ValidFrom time.Time    `yaml:"valid_from" cbor:"valid_from"`
	Signature string       `yaml:"signature" cbor:"-"` // base64, by the previous key
}

type rotationBody struct {
	NewKey    identity.Key `cbor:"new_key"`
	ValidFrom time.Time    `cbor:"valid_from"`
}

// Key is the release signing key, kept offline.
type Key struct {
	priv ed25519.PrivateKey
}

// NewKey draws a key.
func NewKey() (*Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Key{priv: priv}, nil
}

// Public is the half embedded in binaries.
func (k *Key) Public() identity.Key { return identity.KeyOf(k.priv.Public().(ed25519.PublicKey)) }

// Save writes the key as a PEM file readable only by its owner.
func (k *Key) Save(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists", path)
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: keyPEMType, Bytes: k.priv.Seed()}), 0o600)
}

// LoadKey reads a key saved by Save.
func LoadKey(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != keyPEMType || len(block.Bytes) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a release key", path)
	}
	return &Key{priv: ed25519.NewKeyFromSeed(block.Bytes)}, nil
}

// SignRotation makes the record that hands this key over to next.
func (k *Key) SignRotation(next identity.Key, validFrom time.Time) (Rotation, error) {
	r := Rotation{NewKey: next, ValidFrom: validFrom.UTC().Truncate(time.Second)}
	body, err := identity.Marshal(rotationBody{NewKey: r.NewKey, ValidFrom: r.ValidFrom})
	if err != nil {
		return r, err
	}
	r.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, domainMessage(domainRotation, body)))
	return r, nil
}

func domainMessage(domain string, body []byte) []byte {
	return append([]byte("redundanet/3/"+domain+"\x00"), body...)
}

// Signature is `releases/index.sig`.
type Signature struct {
	Revision  uint64    `yaml:"revision" cbor:"revision"`
	IssuedAt  time.Time `yaml:"issued_at" cbor:"issued_at"`
	SHA256    string    `yaml:"sha256" cbor:"sha256"`
	Signer    string    `yaml:"signer" cbor:"-"`
	Signature string    `yaml:"signature" cbor:"-"`
}

type signatureBody struct {
	Revision uint64    `cbor:"revision"`
	IssuedAt time.Time `cbor:"issued_at"`
	SHA256   string    `cbor:"sha256"`
}

func (s *Signature) body() ([]byte, error) {
	return identity.Marshal(signatureBody{Revision: s.Revision, IssuedAt: s.IssuedAt, SHA256: s.SHA256})
}

// Parse reads an index file.
func Parse(data []byte) (*Index, error) {
	var idx Index
	if err := yaml.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("release index: %w", err)
	}
	if idx.Revision == 0 {
		return nil, errors.New("release index: revision is required")
	}
	for i, r := range idx.Releases {
		if _, err := semver.Parse(r.Version); err != nil {
			return nil, fmt.Errorf("release index: releases[%d]: %w", i, err)
		}
		if r.Channel != ChannelStable && r.Channel != ChannelEarly {
			return nil, fmt.Errorf("release index: %s: channel must be stable or early", r.Version)
		}
		for target, a := range r.Assets {
			if a.URL == "" || len(a.SHA256) != 64 || a.Size <= 0 {
				return nil, fmt.Errorf("release index: %s: asset %s needs url, sha256 and size", r.Version, target)
			}
		}
	}
	return &idx, nil
}

// Sign signs the index file as it is on disk.
func Sign(k *Key, data []byte, now time.Time) (*Signature, error) {
	if _, err := Parse(data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	s := &Signature{IssuedAt: now.UTC().Truncate(time.Second), SHA256: hex.EncodeToString(sum[:]), Signer: k.Public().String()}
	idx, _ := Parse(data)
	s.Revision = idx.Revision
	body, err := s.body()
	if err != nil {
		return nil, err
	}
	s.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(k.priv, domainMessage(domainIndex, body)))
	return s, nil
}

// Encode is the YAML form.
func (s *Signature) Encode() ([]byte, error) { return yaml.Marshal(s) }

// ParseSignature reads `index.sig`.
func ParseSignature(data []byte) (*Signature, error) {
	var s Signature
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("index.sig: %w", err)
	}
	return &s, nil
}

// Verify checks the index against the embedded key: it walks the rotation
// chain from that key and accepts a signature by the key the chain ends
// at. It returns the index and that key.
func (s *Signature) Verify(data []byte, embedded identity.Key, now time.Time) (*Index, identity.Key, error) {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != s.SHA256 {
		return nil, identity.Key{}, errors.New("index.sig: the index file is not the one that was signed")
	}
	idx, err := Parse(data)
	if err != nil {
		return nil, identity.Key{}, err
	}
	if idx.Revision != s.Revision {
		return nil, identity.Key{}, errors.New("index.sig: revision differs from the index")
	}
	current := embedded
	for i, r := range idx.Rotations {
		if r.ValidFrom.After(now) {
			break
		}
		body, err := identity.Marshal(rotationBody{NewKey: r.NewKey, ValidFrom: r.ValidFrom.UTC().Truncate(time.Second)})
		if err != nil {
			return nil, identity.Key{}, err
		}
		sig, err := base64.StdEncoding.DecodeString(r.Signature)
		if err != nil {
			return nil, identity.Key{}, fmt.Errorf("rotations[%d]: %w", i, err)
		}
		if !ed25519.Verify(current.Public(), domainMessage(domainRotation, body), sig) {
			return nil, identity.Key{}, fmt.Errorf("rotations[%d] is not signed by the key it retires", i)
		}
		current = r.NewKey
	}
	signer, err := identity.ParseKey(s.Signer)
	if err != nil {
		return nil, identity.Key{}, fmt.Errorf("index.sig: signer: %w", err)
	}
	if signer != current {
		return nil, identity.Key{}, fmt.Errorf("index.sig: signed by %s, the release key is %s", signer.Short(), current.Short())
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return nil, identity.Key{}, err
	}
	body, err := s.body()
	if err != nil {
		return nil, identity.Key{}, err
	}
	if !ed25519.Verify(signer.Public(), domainMessage(domainIndex, body), sig) {
		return nil, identity.Key{}, errors.New("index.sig: the signature does not verify")
	}
	return idx, current, nil
}

// Target is this build's os/arch key in the assets map.
func Target() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Pick chooses the release to install: the highest version above the
// running one in the member's channel (early sees stable too), not
// withdrawn, with an asset for this target. A manifest min_version above
// the running version widens the choice to any channel, so a required
// update is never blocked by a channel setting.
func (idx *Index) Pick(running, channel, minVersion, target string) *Release {
	var best *Release
	for i := range idx.Releases {
		r := &idx.Releases[i]
		if r.Withdrawn || !Less(running, r.Version) {
			continue
		}
		if _, ok := r.Assets[target]; !ok {
			continue
		}
		inChannel := r.Channel == ChannelStable || channel == ChannelEarly
		required := minVersion != "" && Less(running, minVersion) && !Less(r.Version, minVersion)
		if !inChannel && !required {
			continue
		}
		if best == nil || Less(best.Version, r.Version) {
			best = r
		}
	}
	return best
}

// Less reports whether version a sorts before version b, pre-releases
// before the release they precede (see package semver).
func Less(a, b string) bool { return semver.Less(a, b) }

// Versions lists the index's versions, highest first.
func (idx *Index) Versions() []string {
	out := make([]string, 0, len(idx.Releases))
	for _, r := range idx.Releases {
		out = append(out, r.Version)
	}
	sort.Slice(out, func(i, j int) bool { return Less(out[j], out[i]) })
	return out
}
