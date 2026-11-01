package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// State is what a device keeps at rest: its own key, the member secret and
// its certificate. The seed and the member signing key are never in it.
type State struct {
	Network      string            `cbor:"network"`
	Member       Key               `cbor:"member"`
	MemberSecret MemberSecret      `cbor:"member_secret"`
	DeviceSeed   []byte            `cbor:"device_seed"`
	Certificate  SignedCertificate `cbor:"certificate"`
}

// Device rebuilds the device key.
func (s *State) Device() (*DeviceKey, error) { return DeviceKeyFromSeed(s.DeviceSeed) }

// The file is a CBOR envelope. With a passphrase the state is sealed with
// XChaCha20-Poly1305 under an Argon2id key; without one, as on a headless
// node, it is stored in the clear and the file mode is the only protection,
// which is the RFC's stated position for such nodes. A key held in the OS
// keystore is a later addition that changes only this file.
type envelope struct {
	Version int    `cbor:"v"`
	KDF     string `cbor:"kdf"` // "none" or "argon2id"
	Salt    []byte `cbor:"salt,omitempty"`
	Time    uint32 `cbor:"time,omitempty"`
	Memory  uint32 `cbor:"memory,omitempty"` // KiB
	Threads uint8  `cbor:"threads,omitempty"`
	Nonce   []byte `cbor:"nonce,omitempty"`
	Box     []byte `cbor:"box"`
}

const (
	envelopeVersion = 1
	argonTime       = 3
	argonMemory     = 64 * 1024
	argonThreads    = 2
)

// ErrPassphrase is returned when the file is sealed and the passphrase is
// missing or wrong.
var ErrPassphrase = errors.New("the identity file is sealed with a passphrase")

// Save writes the state to path, sealed when passphrase is not empty. The
// file is written beside its final name and renamed into place.
func Save(path string, st *State, passphrase []byte) error {
	plain, err := Marshal(st)
	if err != nil {
		return err
	}
	env := envelope{Version: envelopeVersion, KDF: "none", Box: plain}
	if len(passphrase) > 0 {
		env.KDF = "argon2id"
		env.Salt = make([]byte, 16)
		env.Nonce = make([]byte, chacha20poly1305.NonceSizeX)
		if _, err := rand.Read(env.Salt); err != nil {
			return err
		}
		if _, err := rand.Read(env.Nonce); err != nil {
			return err
		}
		env.Time, env.Memory, env.Threads = argonTime, argonMemory, argonThreads
		key := argon2.IDKey(passphrase, env.Salt, env.Time, env.Memory, env.Threads, chacha20poly1305.KeySize)
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return err
		}
		env.Box = aead.Seal(nil, env.Nonce, plain, []byte(st.Network))
	}
	b, err := Marshal(env)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a state file. The network is needed to open a sealed file,
// because it is bound into the seal.
func Load(path string, network string, passphrase []byte) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if env.Version != envelopeVersion {
		return nil, fmt.Errorf("identity file: version %d, this binary reads %d", env.Version, envelopeVersion)
	}
	plain := env.Box
	switch env.KDF {
	case "none":
	case "argon2id":
		if len(passphrase) == 0 {
			return nil, ErrPassphrase
		}
		key := argon2.IDKey(passphrase, env.Salt, env.Time, env.Memory, env.Threads, chacha20poly1305.KeySize)
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, err
		}
		plain, err = aead.Open(nil, env.Nonce, env.Box, []byte(network))
		if err != nil {
			return nil, ErrPassphrase
		}
	default:
		return nil, fmt.Errorf("identity file: unknown kdf %q", env.KDF)
	}
	var st State
	if err := Unmarshal(plain, &st); err != nil {
		return nil, fmt.Errorf("identity file: %w", err)
	}
	if st.Network != network {
		return nil, fmt.Errorf("identity file is for network %q, not %q", st.Network, network)
	}
	if err := st.Certificate.Verify(); err != nil {
		return nil, err
	}
	dev, err := st.Device()
	if err != nil {
		return nil, err
	}
	if dev.ID() != st.Certificate.Certificate.Device || st.Member != st.Certificate.Certificate.Member {
		return nil, errors.New("identity file: the certificate is not for this device and member")
	}
	return &st, nil
}

// IsSealed reports whether the file needs a passphrase, without opening it.
func IsSealed(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var env envelope
	if err := Unmarshal(b, &env); err != nil {
		return false, fmt.Errorf("identity file: %w", err)
	}
	return env.KDF != "none", nil
}
