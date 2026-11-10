package membership

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Replacing the operator keys from a descriptor.
//
// The keys a device trusts come from the descriptor it joined with and,
// from then on, from the manifests those keys sign: a manifest names the
// operators of the next one. That chain cannot recover from a stolen
// key, since the thief's manifests continue it as well as the owner's.
// The way out is a new descriptor from outside the chain: the one built
// into a release, which the release key vouches for, or one a person
// gives `network join`.
//
// A device takes its keys from such a descriptor when the descriptor's
// operator set differs from both the set of the descriptor it had and
// the set it trusts now. The first says the descriptor is new. The
// second says the device has not reached that set already through a
// manifest, so a descriptor that only catches up with an ordinary
// rotation, or an old one given again, changes nothing.
//
// What happens then:
//
//   - The descriptor's keys are the only ones trusted. The keys trusted
//     before and not in it are dropped, and a manifest that lists a
//     dropped key as an operator is refused from then on.
//   - The manifest on disk stays in force as the policy, so the device
//     keeps working, and its revision stays the floor. It no longer
//     decides whom the device trusts.
//   - The next manifest must be signed by one of the new keys and carry a
//     higher revision. When it arrives the chain continues from it.
//
// The state is in anchor.yaml and operators.yaml is left as it was until
// that manifest arrives, so the previous binary, put back by a failed
// update, still finds the files it understands.

type anchor struct {
	// Pending is set until a manifest signed by one of Operators applied.
	Pending   bool           `yaml:"pending,omitempty"`
	Operators []identity.Key `yaml:"operators,omitempty"`
	Dropped   []identity.Key `yaml:"dropped,omitempty"`
}

func loadAnchor(path string) (anchor, error) {
	var a anchor
	b, err := os.ReadFile(path)
	if err != nil {
		return a, err
	}
	err = yaml.Unmarshal(b, &a)
	return a, err
}

func saveAnchor(path string, a anchor) error {
	b, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	return writeFile(path, b, 0o644)
}

// reanchor decides, before the descriptor on disk is replaced, whether
// cfg.Descriptor replaces the pinned operator keys, and records it.
func (m *Membership) reanchor() error {
	dir := m.cfg.DataDir
	next := m.cfg.Descriptor.Operators
	prev, err := LoadDescriptor(filepath.Join(dir, DescriptorFile))
	if err != nil {
		return nil // no descriptor before this one: a first join
	}
	pinned, _ := loadTrust(filepath.Join(dir, fileTrust))
	old, _ := loadAnchor(filepath.Join(dir, fileAnchor))
	current := pinned
	if old.Pending {
		current = old.Operators
	}
	if len(current) == 0 || sameKeys(next, prev.Operators) || sameKeys(next, current) {
		return nil
	}
	a := anchor{Pending: true, Operators: append([]identity.Key(nil), next...)}
	for _, list := range [][]identity.Key{pinned, old.Operators, old.Dropped} {
		for _, k := range list {
			if !hasKey(next, k) && !hasKey(a.Dropped, k) {
				a.Dropped = append(a.Dropped, k)
			}
		}
	}
	if err := saveAnchor(filepath.Join(dir, fileAnchor), a); err != nil {
		return err
	}
	m.log.Warn("a new network descriptor replaces the operator keys of this device", "operators", len(a.Operators), "dropped", len(a.Dropped))
	return nil
}

func hasKey(list []identity.Key, k identity.Key) bool {
	for _, x := range list {
		if x == k {
			return true
		}
	}
	return false
}

func sameKeys(a, b []identity.Key) bool {
	for _, k := range a {
		if !hasKey(b, k) {
			return false
		}
	}
	for _, k := range b {
		if !hasKey(a, k) {
			return false
		}
	}
	return true
}
