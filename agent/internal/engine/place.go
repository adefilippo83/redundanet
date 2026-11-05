package engine

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

// server is a storage node from the manifest.
type server struct {
	Device identity.Key
	Member string
}

// servers lists the storage servers and the current encoding.
func (e *Native) servers() ([]server, object.Params, error) {
	m := e.cfg.Manifest()
	if m == nil {
		return nil, object.Params{}, ErrNoServers
	}
	var out []server
	for _, s := range m.Storage {
		out = append(out, server{Device: s.Device, Member: s.Member})
	}
	if len(out) == 0 {
		return nil, object.Params{}, ErrNoServers
	}
	return out, object.DefaultParams(m.Network.Encoding.K, m.Network.Encoding.N), nil
}

// order interleaves servers across members, so consecutive shares land on
// different members first, and rotates by the storage index so load spreads
// across objects.
func order(si object.StorageIndex, servers []server) []server {
	byMember := map[string][]server{}
	var members []string
	for _, s := range servers {
		if _, ok := byMember[s.Member]; !ok {
			members = append(members, s.Member)
		}
		byMember[s.Member] = append(byMember[s.Member], s)
	}
	sort.Strings(members)
	for _, m := range members {
		sort.Slice(byMember[m], func(i, j int) bool { return byMember[m][i].Device.Less(byMember[m][j].Device) })
	}
	var out []server
	for len(out) < len(servers) {
		for _, m := range members {
			if q := byMember[m]; len(q) > 0 {
				out = append(out, q[0])
				byMember[m] = q[1:]
			}
		}
	}
	rot := int(si[0]) % len(out)
	return append(out[rot:], out[:rot]...)
}

// plan assigns share numbers to servers: at most ceil(n / S) per server,
// never a server that already holds that number, skipping servers marked
// bad. held counts the shares each server holds already.
func plan(si object.StorageIndex, n int, shares []int, servers []server, held map[identity.Key]int, bad map[identity.Key]bool) map[int]identity.Key {
	ord := order(si, servers)
	var usable []server
	for _, s := range ord {
		if !bad[s.Device] {
			usable = append(usable, s)
		}
	}
	if len(usable) == 0 {
		return nil
	}
	maxPer := (n + len(servers) - 1) / len(servers)
	if len(usable) < len(servers) {
		maxPer = (n + len(usable) - 1) / len(usable)
	}
	counts := map[identity.Key]int{}
	for k, v := range held {
		counts[k] = v
	}
	out := map[int]identity.Key{}
	next := 0
	for _, sh := range shares {
		for tries := 0; tries < len(usable); tries++ {
			s := usable[(next+tries)%len(usable)]
			if counts[s.Device] < maxPer {
				out[sh] = s.Device
				counts[s.Device]++
				next = next + tries + 1
				break
			}
		}
	}
	return out
}

// presence is what every server holds of an object, asked all at once.
type presence struct {
	Pres     map[identity.Key]*shareproto.Presence
	RTT      map[identity.Key]time.Duration
	Asked    int
	Answered int
}

func (e *Native) presence(ctx context.Context, si object.StorageIndex, servers []server) *presence {
	p := &presence{Pres: map[identity.Key]*shareproto.Presence{}, RTT: map[identity.Key]time.Duration{}, Asked: len(servers)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel()
			start := time.Now()
			pr, err := e.cfg.Client.Has(cctx, s.Device, si)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				return
			}
			p.Answered++
			p.RTT[s.Device] = time.Since(start)
			if pr != nil {
				p.Pres[s.Device] = pr
			}
		}(s)
	}
	wg.Wait()
	return p
}

// health turns a presence into a Health: the complete shares under the
// capability's descriptor hash, and how many servers and members hold
// them. A tombstoned holder does not count, so an upload clears the
// tombstone and repair never fills a deleted object in; it is kept aside
// for the owner's reads within the window.
func health(vc object.VerifyCap, k, n int, p *presence, servers []server) *Health {
	h := &Health{Cap: vc, K: k, N: n, Holders: map[identity.Key][]uint8{}, Asked: p.Asked, Answered: p.Answered}
	memberOf := map[identity.Key]string{}
	for _, s := range servers {
		memberOf[s.Device] = s.Member
	}
	distinct := map[uint8]bool{}
	members := map[string]bool{}
	for dev, pr := range p.Pres {
		if pr.DescriptorHash != vc.DescriptorHash || len(pr.Complete) == 0 {
			continue
		}
		if pr.Tombstoned {
			h.Tombstoned = true
			if h.Ghosts == nil {
				h.Ghosts = map[identity.Key][]uint8{}
			}
			h.Ghosts[dev] = append([]uint8(nil), pr.Complete...)
			continue
		}
		h.Holders[dev] = append([]uint8(nil), pr.Complete...)
		members[memberOf[dev]] = true
		for _, c := range pr.Complete {
			distinct[c] = true
		}
	}
	for c := range distinct {
		h.Shares = append(h.Shares, c)
	}
	sort.Slice(h.Shares, func(i, j int) bool { return h.Shares[i] < h.Shares[j] })
	h.Servers = len(h.Holders)
	h.Members = len(members)
	h.Readable = len(h.Shares) >= k
	h.Placed = len(h.Shares) >= n
	return h
}
