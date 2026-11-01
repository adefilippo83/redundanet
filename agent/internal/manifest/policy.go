package manifest

import (
	"fmt"
	"sync"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Policy answers "may this device connect?" from the signed manifest and the
// revocation records: the RFC's Who may connect rule. It is the Allow the
// identity package's TLS configs take, and it is swapped whole when a newer
// manifest or a revocation arrives.
type Policy struct {
	mu      sync.RWMutex
	m       *Manifest
	revoked map[revocationKey]bool
}

type revocationKey struct {
	member identity.Key
	device identity.Key
	serial uint64
}

// NewPolicy builds the policy for a manifest and the revocations known so
// far. Revocations that do not verify are ignored, not trusted.
func NewPolicy(m *Manifest, revocations []*identity.SignedRevocation) *Policy {
	p := &Policy{m: m, revoked: map[revocationKey]bool{}}
	for _, r := range revocations {
		p.addRevocation(r)
	}
	return p
}

func (p *Policy) addRevocation(r *identity.SignedRevocation) {
	if r.Verify() != nil || r.Revocation.Network != p.m.Network.Name {
		return
	}
	p.revoked[revocationKey{r.Revocation.Member, r.Revocation.Device, r.Revocation.Serial}] = true
}

// AddRevocation records a revocation that arrived after the policy was built.
func (p *Policy) AddRevocation(r *identity.SignedRevocation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addRevocation(r)
}

// Manifest is the manifest the policy enforces.
func (p *Policy) Manifest() *Manifest {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.m
}

// Allow is the connection rule: the certificate is for this network and is
// not revoked, and either the device is a listed hub or the member is
// active or leaving.
func (p *Policy) Allow(peer identity.Peer) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c := peer.Certificate.Certificate
	if c.Network != p.m.Network.Name {
		return fmt.Errorf("device %s belongs to network %q", peer.Device.Short(), c.Network)
	}
	if p.revoked[revocationKey{c.Member, c.Device, c.Serial}] {
		return fmt.Errorf("device %s is revoked", peer.Device.Short())
	}
	if p.m.HubByKey(peer.Device) != nil {
		return nil
	}
	mem := p.m.MemberByKey(peer.Member)
	if mem == nil {
		return fmt.Errorf("member %s is not in the manifest", peer.Member.Short())
	}
	if mem.Status != StatusActive && mem.Status != StatusLeaving {
		return fmt.Errorf("member %s is %s", mem.Name, mem.Status)
	}
	return nil
}

// MayWrite is the storage rule: writes need an active member; a leaving
// member may still read and be repaired for.
func (p *Policy) MayWrite(peer identity.Peer) error {
	if err := p.Allow(peer); err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	mem := p.m.MemberByKey(peer.Member)
	if mem == nil || mem.Status != StatusActive {
		return fmt.Errorf("member %s may not write", peer.Member.Short())
	}
	return nil
}
