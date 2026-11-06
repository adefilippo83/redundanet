package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

// sealRoot encrypts a root record under the member's root key with a
// random nonce in front, so servers never see a capability.
func (e *Native) sealRoot(r Root) ([]byte, error) {
	body, err := identity.Marshal(r)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(e.rootKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, body, []byte("redundanet/3/root")), nil
}

func (e *Native) openRoot(ct []byte) (*Root, error) {
	block, err := aes.NewCipher(e.rootKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < gcm.NonceSize() {
		return nil, errors.New("engine: root ciphertext too short")
	}
	body, err := gcm.Open(nil, ct[:gcm.NonceSize()], ct[gcm.NonceSize():], []byte("redundanet/3/root"))
	if err != nil {
		return nil, fmt.Errorf("engine: root does not open with this member's key: %w", err)
	}
	var r Root
	if err := identity.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// PutRoot publishes this device's root with the next version, confirmed
// once a majority of the storage servers hold it. The version is spent
// either way; a root that missed quorum belongs to a run that did not
// complete, and the caller retries with a new version.
func (e *Native) PutRoot(ctx context.Context, root Root) (uint64, error) {
	servers, _, err := e.servers()
	if err != nil {
		return 0, err
	}
	if root.WrittenAt.IsZero() {
		root.WrittenAt = e.cfg.Now()
	}
	v, err := e.cfg.DB.NextRootVersion()
	if err != nil {
		return 0, err
	}
	ct, err := e.sealRoot(root)
	if err != nil {
		return v, err
	}
	env, err := shareproto.SignRoot(e.cfg.Network, e.cfg.Device, e.cfg.Cert, v, ct)
	if err != nil {
		return v, err
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	acks := 0
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel()
			if err := e.cfg.Client.PutRoot(cctx, s.Device, env); err != nil {
				e.log.Info("root not stored", "server", s.Device.Short(), "version", v, "err", err)
				return
			}
			mu.Lock()
			acks++
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	w := len(servers)/2 + 1
	if acks < w {
		return v, fmt.Errorf("%w: %d of %d, needed %d", ErrQuorum, acks, len(servers), w)
	}
	return v, nil
}

// GetRoots asks every server for a member's device roots, keeps the
// highest version per device, opens the member's own, and reports how
// many servers answered: any S-W+1 answers include every confirmed root.
func (e *Native) GetRoots(ctx context.Context, member identity.Key) (*Roots, error) {
	servers, _, err := e.servers()
	if err != nil {
		return nil, err
	}
	out := &Roots{Servers: len(servers)}
	best := map[identity.Key]*shareproto.RootEnvelope{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel()
			envs, err := e.cfg.Client.GetRoots(cctx, s.Device, member)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			out.Answered++
			for _, env := range envs {
				if env.Verify(e.cfg.Network) != nil || env.Member != member {
					continue
				}
				if cur := best[env.Device]; cur == nil || env.Version > cur.Version {
					best[env.Device] = env
				}
			}
		}(s)
	}
	wg.Wait()
	var highestOwn uint64
	for dev, env := range best {
		view := RootView{Device: dev, Version: env.Version, Envelope: env}
		if member == e.cfg.Member {
			if r, err := e.openRoot(env.Ciphertext); err == nil {
				view.Root = r
			}
			if dev == e.cfg.Device.ID() && env.Version > highestOwn {
				highestOwn = env.Version
			}
		}
		out.Views = append(out.Views, view)
	}
	if highestOwn > 0 {
		_ = e.cfg.DB.RecoverRootVersion(highestOwn)
	}
	return out, nil
}

// Usage asks every storage server what it holds for the member and sums
// the answers: the budget check's measure.
func (e *Native) Usage(ctx context.Context) (Usage, error) {
	servers, _, err := e.servers()
	if err != nil {
		return Usage{}, err
	}
	u := Usage{Servers: len(servers)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel()
			n, err := e.cfg.Client.Usage(cctx, s.Device)
			if err != nil {
				e.log.Info("usage not answered", "server", s.Device.Short(), "err", err)
				return
			}
			mu.Lock()
			u.Bytes += n
			u.Answered++
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	return u, nil
}
