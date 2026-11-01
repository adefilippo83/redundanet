package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
)

// DeviceKey is a device's own Ed25519 key, random, never derived.
type DeviceKey struct {
	priv ed25519.PrivateKey
}

// NewDeviceKey draws a key.
func NewDeviceKey() (*DeviceKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &DeviceKey{priv: priv}, nil
}

// DeviceKeyFromSeed rebuilds a key from the 32-byte seed the keystore holds.
func DeviceKeyFromSeed(seed []byte) (*DeviceKey, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("device key seed has the wrong length")
	}
	return &DeviceKey{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// Seed is the 32 bytes that rebuild the key.
func (d *DeviceKey) Seed() []byte { return d.priv.Seed() }

// ID is the device's public key, its identity on the mesh.
func (d *DeviceKey) ID() Key { return KeyOf(d.priv.Public().(ed25519.PublicKey)) }

// Private exposes the key for TLS.
func (d *DeviceKey) Private() ed25519.PrivateKey { return d.priv }

// Sign signs a domain-separated message with the device key.
func (d *DeviceKey) Sign(domain string, body []byte) []byte {
	return signMessage(d.priv, domain, body)
}

// VerifyDevice checks a signature made by Sign.
func VerifyDevice(device Key, domain string, body, sig []byte) error {
	return verifyMessage(device, domain, body, sig)
}
