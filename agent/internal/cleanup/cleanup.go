// Package cleanup is the owner's cleanup job of RFC 0001's "Deletion":
// it computes the reachable set over every root of the member, lists
// what the grid holds for the member, and sends a delete token for each
// object nothing references. It runs against the grid, never against the
// local cache, so a lost local database cannot cause a wrong deletion;
// it refuses to run without the root quorum, and refuses when a root or
// a directory could not be read, since a partial reachable set would
// delete live data. The servers add the rest of the invariant: the 7-day
// immunity on their own clocks, and the 30-day tombstone window.
package cleanup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// Grid is what the job needs of the engine: reads for the walk, the
// listing and the token delivery by storage index.
type Grid interface {
	engine.Engine
	ListObjects(ctx context.Context) (*engine.Listing, error)
	DeleteByIndex(ctx context.Context, si object.StorageIndex, dhash object.Hash) (*engine.DeleteResult, error)
}

// Config wires one run.
type Config struct {
	Grid   Grid
	Member identity.Key
	// Revocations lists the member's revoked devices; a root of a device
	// revoked longer ago than the tombstone window no longer keeps its
	// objects alive.
	Revocations func() []*identity.SignedRevocation
	DryRun      bool // compute and count, send nothing
	// Force lifts the bound on how much one run may delete.
	Force    bool
	Parallel int // tokens in flight at once; default 8
	Now      func() time.Time
	Log      *slog.Logger
}

// Windows the job applies on its side.
const (
	tombstoneWindow = 30 * 24 * time.Hour
	immunity        = 7 * 24 * time.Hour
	// maxGarbageShare is how much of what the grid holds one run may
	// delete without --force: a guard against a wrong reachable set, on
	// top of the two-phase deletion.
	maxGarbageShare = 0.5
	minGarbageGuard = 100
)

// Report is what a run found and did.
type Report struct {
	At            time.Time `json:"at"`
	DryRun        bool      `json:"dry_run,omitempty"`
	Servers       int       `json:"servers"`
	RootsAnswered int       `json:"roots_answered"` // servers that answered the roots query
	Roots         int       `json:"roots"`          // device roots walked
	RootsSkipped  int       `json:"roots_skipped"`  // revoked past the window
	Reachable     int       `json:"reachable"`      // distinct objects some root references
	Listed        int       `json:"listed"`         // distinct objects the servers hold for the member
	ListAnswered  int       `json:"list_answered"`  // servers that answered the listing
	Garbage       int       `json:"garbage"`        // listed, unreferenced, not yet tombstoned
	Tombstoned    int       `json:"tombstoned"`     // tokens the servers accepted this run
	Recent        int       `json:"recent"`         // refused for a recent touch; next run
	Incomplete    int       `json:"incomplete"`     // not every holder answered; next run
	Failed        int       `json:"failed"`         // other errors
	Error         string    `json:"error,omitempty"`
}

// Errors a caller tells apart.
var (
	ErrQuorum     = errors.New("cleanup: too few servers answered the roots query to be sure every confirmed root was seen")
	ErrUnreadable = errors.New("cleanup: a root or an object of the reachable set could not be read; nothing deleted")
	ErrTooMuch    = errors.New("cleanup: refusing to delete that large a share of what the grid holds; run with --force if that is right")
	ErrNoListing  = errors.New("cleanup: no server listed the member's objects")
)

// Run performs one cleanup.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.Grid == nil || cfg.Member.IsZero() {
		return nil, errors.New("cleanup: a grid and a member are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Parallel <= 0 {
		cfg.Parallel = 8
	}
	lg := cfg.Log.With("component", "cleanup")
	rep := &Report{At: cfg.Now(), DryRun: cfg.DryRun}
	fail := func(err error) (*Report, error) {
		rep.Error = err.Error()
		return rep, err
	}

	// The roots, under quorum: any S-W+1 answers include every confirmed
	// root, with W a majority of S.
	roots, err := cfg.Grid.GetRoots(ctx, cfg.Member)
	if err != nil {
		return fail(err)
	}
	rep.Servers, rep.RootsAnswered = roots.Servers, roots.Answered
	w := roots.Servers/2 + 1
	if roots.Servers == 0 || roots.Answered < roots.Servers-w+1 {
		return fail(fmt.Errorf("%w: %d of %d", ErrQuorum, roots.Answered, roots.Servers))
	}
	revokedAt := map[identity.Key]time.Time{}
	if cfg.Revocations != nil {
		for _, r := range cfg.Revocations() {
			if r.Revocation.Member == cfg.Member {
				revokedAt[r.Revocation.Device] = r.Revocation.RevokedAt
			}
		}
	}
	// The reachable set: every object any root references, through the
	// index, the snapshots, the directories, the files and their chunks.
	reachable := map[object.StorageIndex]bool{}
	add := func(capStr string) error {
		rc, err := object.ParseReadCap(capStr)
		if err != nil {
			return err
		}
		reachable[rc.Key.StorageIndex()] = true
		return nil
	}
	for _, v := range roots.Views {
		if at, ok := revokedAt[v.Device]; ok && cfg.Now().Sub(at) > tombstoneWindow {
			rep.RootsSkipped++
			continue
		}
		if v.Root == nil {
			return fail(fmt.Errorf("%w: the root of device %s does not open with this member's key", ErrUnreadable, v.Device.Short()))
		}
		rep.Roots++
		if v.Root.IndexCap == "" {
			continue
		}
		if err := add(v.Root.IndexCap); err != nil {
			return fail(fmt.Errorf("%w: %v", ErrUnreadable, err))
		}
		x, err := fetchIndex(ctx, cfg.Grid, v.Root.IndexCap)
		if err != nil {
			return fail(fmt.Errorf("%w: index of device %s: %v", ErrUnreadable, v.Device.Short(), err))
		}
		for _, snap := range x.Snapshots {
			if err := walk(ctx, cfg.Grid, snap.Top, reachable); err != nil {
				return fail(fmt.Errorf("%w: snapshot %s of device %s: %v", ErrUnreadable, snap.ID, v.Device.Short(), err))
			}
		}
	}
	rep.Reachable = len(reachable)

	// What the grid holds, minus what is reachable or already tombstoned.
	listing, err := cfg.Grid.ListObjects(ctx)
	if err != nil {
		return fail(err)
	}
	rep.Listed, rep.ListAnswered = len(listing.Objects), listing.Answered
	if listing.Answered == 0 {
		return fail(ErrNoListing)
	}
	var garbage []*engine.ListedObject
	for si, o := range listing.Objects {
		if reachable[si] || o.Tombstoned {
			continue
		}
		garbage = append(garbage, o)
	}
	sort.Slice(garbage, func(i, j int) bool { return bytes.Compare(garbage[i].StorageIndex[:], garbage[j].StorageIndex[:]) < 0 })
	rep.Garbage = len(garbage)
	if !cfg.Force && len(garbage) > minGarbageGuard && float64(len(garbage)) > maxGarbageShare*float64(len(listing.Objects)) {
		return fail(fmt.Errorf("%w: %d of %d objects", ErrTooMuch, len(garbage), len(listing.Objects)))
	}
	if cfg.DryRun || len(garbage) == 0 {
		lg.Info("cleanup", "dry_run", cfg.DryRun, "reachable", rep.Reachable, "listed", rep.Listed, "garbage", rep.Garbage)
		return rep, nil
	}

	// Tokens, a few at a time.
	var mu sync.Mutex
	sem := make(chan struct{}, cfg.Parallel)
	var wg sync.WaitGroup
	for _, o := range garbage {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(o *engine.ListedObject) {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := cfg.Grid.DeleteByIndex(ctx, o.StorageIndex, o.DescriptorHash)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				rep.Tombstoned++
			case errors.Is(err, engine.ErrRecentlyTouched):
				rep.Recent++
			case errors.Is(err, engine.ErrIncomplete):
				rep.Incomplete++
			default:
				rep.Failed++
				lg.Info("delete token not delivered", "si", o.StorageIndex.String()[:8], "err", err)
			}
		}(o)
	}
	wg.Wait()
	lg.Info("cleanup", "reachable", rep.Reachable, "listed", rep.Listed, "garbage", rep.Garbage, "tombstoned", rep.Tombstoned, "recent", rep.Recent, "incomplete", rep.Incomplete, "failed", rep.Failed)
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return rep, nil
}

func fetchIndex(ctx context.Context, g Grid, capStr string) (*snapshot.Index, error) {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := g.Get(ctx, rc, &buf, nil); err != nil {
		return nil, err
	}
	return snapshot.DecodeIndex(buf.Bytes())
}

// walk adds a directory object and everything below it. A directory
// already reached is not read again: unchanged folders share their
// objects across snapshots, which is what keeps the walk small.
func walk(ctx context.Context, g Grid, capStr string, reachable map[object.StorageIndex]bool) error {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return err
	}
	if reachable[rc.Key.StorageIndex()] {
		return nil
	}
	reachable[rc.Key.StorageIndex()] = true
	var buf bytes.Buffer
	if err := g.Get(ctx, rc, &buf, nil); err != nil {
		return err
	}
	d, err := snapshot.DecodeDirectory(buf.Bytes())
	if err != nil {
		return err
	}
	for _, en := range d.Entries {
		switch en.Kind {
		case snapshot.KindDir:
			if err := walk(ctx, g, en.Cap, reachable); err != nil {
				return err
			}
		case snapshot.KindFile:
			if en.Cap != "" {
				c, err := object.ParseReadCap(en.Cap)
				if err != nil {
					return err
				}
				reachable[c.Key.StorageIndex()] = true
			}
			for _, ch := range en.Chunks {
				c, err := object.ParseReadCap(ch)
				if err != nil {
					return err
				}
				reachable[c.Key.StorageIndex()] = true
			}
		}
	}
	return nil
}

var _ Grid = (*engine.Native)(nil)
var _ Grid = (*engine.Fake)(nil)
