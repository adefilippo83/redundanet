// Package membership keeps a device's view of who is in the network: it
// fetches the signed manifest, verifies it against the operator keys pinned
// at join, keeps the last good copy on disk, applies revocations, and hands
// the transport one Allow that always reflects the current policy.
package membership

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Descriptor is the public description of a network a device joins with:
// what an invite link carries and what `redundanet join` takes. The
// operator keys in it are the trust anchor; everything else is a hint the
// signed manifest confirms.
type Descriptor struct {
	Network    string         `yaml:"network" cbor:"network"`
	Repository string         `yaml:"repository" cbor:"repository"` // base URL under which manifests/manifest.yaml and manifest.sig are served
	Hubs       []HubHint      `yaml:"hubs" cbor:"hubs"`
	Operators  []identity.Key `yaml:"operators" cbor:"operators"`
}

// HubHint is enough to reach a hub before the manifest is known.
type HubHint struct {
	Key   identity.Key `yaml:"key" cbor:"key"`
	Relay string       `yaml:"relay" cbor:"relay"`
}

const descriptorPrefix = "rn3:net:"

// Encode is the link form: a prefix and base64url of the deterministic CBOR.
func (d *Descriptor) Encode() (string, error) {
	if err := d.check(); err != nil {
		return "", err
	}
	b, err := identity.Marshal(d)
	if err != nil {
		return "", err
	}
	return descriptorPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseDescriptor reads the link form.
func ParseDescriptor(s string) (*Descriptor, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, descriptorPrefix) {
		return nil, fmt.Errorf("not a network descriptor: expected the prefix %q", descriptorPrefix)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, descriptorPrefix))
	if err != nil {
		return nil, fmt.Errorf("network descriptor: %w", err)
	}
	var d Descriptor
	if err := identity.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("network descriptor: %w", err)
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *Descriptor) check() error {
	if d.Network == "" {
		return errors.New("network descriptor: network name is empty")
	}
	if len(d.Operators) == 0 {
		return errors.New("network descriptor: no operator keys")
	}
	if d.Repository != "" {
		u, err := url.Parse(d.Repository)
		if err != nil {
			return fmt.Errorf("network descriptor: repository: %w", err)
		}
		// Plain HTTP only to this machine, for tests and local mirrors.
		local := u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || u.Hostname() == "localhost"
		if u.Scheme != "https" && !(u.Scheme == "http" && local) {
			return errors.New("network descriptor: the repository must be an https:// URL")
		}
	}
	return nil
}

// SaveDescriptor writes the descriptor as YAML; LoadDescriptor reads it.
func SaveDescriptor(path string, d *Descriptor) error {
	if err := d.check(); err != nil {
		return err
	}
	b, err := yaml.Marshal(d)
	if err != nil {
		return err
	}
	return writeFile(path, b, 0o644)
}

// LoadDescriptor reads a descriptor saved by SaveDescriptor.
func LoadDescriptor(path string) (*Descriptor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Descriptor
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := d.check(); err != nil {
		return nil, err
	}
	return &d, nil
}

// writeFile writes beside the target and renames into place.
func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
