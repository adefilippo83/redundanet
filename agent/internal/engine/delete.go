package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

// Deletion, the first phase: a delete token signed by this device goes to
// every storage server, which tombstones the object if it holds it. The
// second phase is the servers' own, after the window.

// DeleteResult is what every server said to a token.
type DeleteResult struct {
	Servers    int
	Answered   int
	Tombstoned int // held and now tombstoned, or tombstoned already
	NotHeld    int
	Recent     int // refused: touched within the immunity
	Stale      int // refused: the token predates the last touch
	Failed     map[identity.Key]string
}

// Delete tombstones an object everywhere, by its capability.
func (e *Native) Delete(ctx context.Context, rc object.ReadCap) error {
	_, err := e.DeleteByIndex(ctx, rc.Key.StorageIndex(), rc.DescriptorHash)
	return err
}

// DeleteByIndex tombstones an object everywhere, by what a listing gives:
// the storage index and the descriptor hash the holders agree on. It
// returns ErrRecentlyTouched when a server refused the token for a recent
// touch and ErrIncomplete when a server could not be asked; both mean
// try again later. A stale refusal counts like a recent one.
func (e *Native) DeleteByIndex(ctx context.Context, si object.StorageIndex, dhash object.Hash) (*DeleteResult, error) {
	servers, _, err := e.servers()
	if err != nil {
		return nil, err
	}
	token, err := shareproto.SignDelete(e.cfg.Network, e.cfg.Device, e.cfg.Cert, si, dhash, e.cfg.Now())
	if err != nil {
		return nil, err
	}
	res := &DeleteResult{Servers: len(servers), Failed: map[identity.Key]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel()
			p, err := e.cfg.Client.Delete(cctx, s.Device, token)
			mu.Lock()
			defer mu.Unlock()
			var se *shareproto.StatusError
			switch {
			case err == nil:
				res.Answered++
				if p == nil {
					res.NotHeld++
				} else {
					res.Tombstoned++
				}
			case errors.As(err, &se) && se.Status == shareproto.StatusRecent:
				res.Answered++
				res.Recent++
			case errors.As(err, &se) && se.Status == shareproto.StatusStale:
				res.Answered++
				res.Stale++
			default:
				res.Failed[s.Device] = err.Error()
			}
		}(s)
	}
	wg.Wait()
	switch {
	case res.Recent > 0 || res.Stale > 0:
		return res, ErrRecentlyTouched
	case len(res.Failed) > 0:
		return res, fmt.Errorf("%w: %d of %d", ErrIncomplete, res.Answered, res.Servers)
	}
	return res, nil
}

// ListedObject is one object of this member as the grid holds it.
type ListedObject struct {
	StorageIndex   object.StorageIndex
	DescriptorHash object.Hash
	Holders        []identity.Key
	Tombstoned     bool // on every holder that answered
}

// Listing is every object the live servers hold for this member.
type Listing struct {
	Objects  map[object.StorageIndex]*ListedObject
	Answered int
	Servers  int
}

// ListObjects asks every storage server what it holds for this member:
// the grid's side of the cleanup's reachable-set check, never the local
// cache. A server that does not answer is left out, and the count says
// so; its objects wait for a later run.
func (e *Native) ListObjects(ctx context.Context) (*Listing, error) {
	servers, _, err := e.servers()
	if err != nil {
		return nil, err
	}
	out := &Listing{Objects: map[object.StorageIndex]*ListedObject{}, Servers: len(servers)}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s server) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 10*e.cfg.Timeout)
			defer cancel()
			list, err := e.cfg.Client.List(cctx, s.Device)
			if err != nil {
				e.log.Info("objects not listed", "server", s.Device.Short(), "err", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			out.Answered++
			for _, l := range list {
				o := out.Objects[l.StorageIndex]
				if o == nil {
					o = &ListedObject{StorageIndex: l.StorageIndex, DescriptorHash: l.DescriptorHash, Tombstoned: true}
					out.Objects[l.StorageIndex] = o
				}
				o.Holders = append(o.Holders, s.Device)
				o.Tombstoned = o.Tombstoned && l.Tombstoned
			}
		}(s)
	}
	wg.Wait()
	for _, o := range out.Objects {
		sort.Slice(o.Holders, func(i, j int) bool { return o.Holders[i].Less(o.Holders[j]) })
	}
	return out, nil
}
