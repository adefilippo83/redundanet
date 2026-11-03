// Package rebalance is the owner's side of a k-of-n change, RFC 0001's
// "Rebalance": each device converts what its own root references. For
// every snapshot in the device's index it walks the directory tree, reads
// each object still under the old encoding, stores it again under the
// manifest's encoding, which makes a new object with its own key and
// storage index, rewrites the directory objects up to the top, updates
// the local database before anything is published, and publishes the
// root after each snapshot. The old objects become unreferenced and the
// cleanup job retires them. A run that stops resumes where it left off:
// what is converted costs only lookups the next time.
package rebalance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// Grid is what the job needs of the engine: reads, writes under the
// current encoding, roots, and the encoding of an object the local
// database does not know.
type Grid interface {
	engine.Engine
	Describe(ctx context.Context, rc object.ReadCap) (*object.Descriptor, error)
}

// Config wires one run.
type Config struct {
	Grid     Grid
	DB       *localdb.DB
	Member   identity.Key
	Device   identity.Key
	Manifest func() *manifest.Manifest
	SpoolDir string
	Progress func(Status)
	Now      func() time.Time
	Log      *slog.Logger
}

// Status is what the local page shows about the conversion.
type Status struct {
	Running   bool      `json:"running"`
	From      string    `json:"from,omitempty"` // "2-of-4"
	To        string    `json:"to,omitempty"`
	Snapshot  string    `json:"snapshot,omitempty"` // the snapshot being converted
	Snapshots int       `json:"snapshots"`          // in the index
	Done      int       `json:"snapshots_done"`     // converted this run or before
	Objects   int       `json:"objects"`            // objects re-encoded this run
	Bytes     int64     `json:"bytes"`              // plaintext bytes re-encoded this run
	At        time.Time `json:"at,omitempty"`       // when the run ended
	Err       string    `json:"error,omitempty"`    //
	Nothing   bool      `json:"nothing_to_do"`      // no transition in the manifest, or no index
}

// Run converts this device's snapshots to the manifest's encoding.
func Run(ctx context.Context, cfg Config) (*Status, error) {
	if cfg.Grid == nil || cfg.DB == nil || cfg.Manifest == nil || cfg.Member.IsZero() || cfg.Device.IsZero() {
		return nil, errors.New("rebalance: grid, db, manifest, member and device are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	lg := cfg.Log.With("component", "rebalance")
	st := &Status{Running: true}
	report := func() {
		if cfg.Progress != nil {
			cfg.Progress(*st)
		}
	}
	finish := func(err error) (*Status, error) {
		st.Running, st.At = false, cfg.Now()
		if err != nil {
			st.Err = err.Error()
		}
		report()
		return st, err
	}
	m := cfg.Manifest()
	if m == nil || m.Network.EncodingTransition == nil {
		st.Nothing = true
		return finish(nil)
	}
	t := m.Network.EncodingTransition
	target := object.DefaultParams(m.Network.Encoding.K, m.Network.Encoding.N)
	st.From, st.To = fmt.Sprintf("%d-of-%d", t.From.K, t.From.N), fmt.Sprintf("%d-of-%d", t.To.K, t.To.N)
	report()

	// This device's index, through its root.
	roots, err := cfg.Grid.GetRoots(ctx, cfg.Member)
	if err != nil {
		return finish(err)
	}
	var indexCap string
	for _, v := range roots.Views {
		if v.Device == cfg.Device && v.Root != nil {
			indexCap = v.Root.IndexCap
		}
	}
	if indexCap == "" {
		st.Nothing = true
		return finish(nil)
	}
	r := &run{cfg: cfg, lg: lg, st: st, target: target, params: map[string]object.Params{}, replaced: map[string]string{}, report: report}
	x, err := r.fetchIndex(ctx, indexCap)
	if err != nil {
		return finish(err)
	}
	st.Snapshots = len(x.Snapshots)
	for i := range x.Snapshots {
		snap := &x.Snapshots[i]
		st.Snapshot = snap.ID
		report()
		newTop, changed, err := r.convertDir(ctx, snap.Top)
		if err != nil {
			return finish(fmt.Errorf("snapshot %s: %w", snap.ID, err))
		}
		st.Done++
		if !changed {
			continue
		}
		snap.Top = newTop
		// The local database first, then the index and the root: a run
		// that stops between the two leaves a consistent cache either way.
		if err := cfg.DB.RewriteCaps(r.replaced); err != nil {
			return finish(err)
		}
		r.replaced = map[string]string{}
		if err := r.publish(ctx, x); err != nil {
			return finish(err)
		}
		lg.Info("snapshot converted", "snapshot", snap.ID, "objects", st.Objects, "bytes", st.Bytes)
	}
	st.Snapshot = ""
	return finish(nil)
}

type run struct {
	cfg      Config
	lg       *slog.Logger
	st       *Status
	target   object.Params
	params   map[string]object.Params // the encoding of each capability seen
	replaced map[string]string        // old capability -> new, for the scan rows
	report   func()
}

func (r *run) fetchIndex(ctx context.Context, capStr string) (*snapshot.Index, error) {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := r.cfg.Grid.Get(ctx, rc, &buf, nil); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return snapshot.DecodeIndex(buf.Bytes())
}

// encodingOf is the encoding an object was written under: the local
// database's word, or a holder's descriptor.
func (r *run) encodingOf(ctx context.Context, capStr string) (object.Params, error) {
	if p, ok := r.params[capStr]; ok {
		return p, nil
	}
	if o, err := r.cfg.DB.LookupByCap(capStr); err == nil && o != nil {
		r.params[capStr] = o.Params
		return o.Params, nil
	}
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return object.Params{}, err
	}
	d, err := r.cfg.Grid.Describe(ctx, rc)
	if err != nil {
		return object.Params{}, err
	}
	p := d.Params()
	r.params[capStr] = p
	return p, nil
}

func (r *run) current(p object.Params) bool {
	return p.K == r.target.K && p.N == r.target.N && p.SegmentSize == r.target.SegmentSize
}

// convertDir converts everything below a directory, then the directory
// itself when a child changed or it sits under the old encoding, and
// returns the directory's capability afterwards.
func (r *run) convertDir(ctx context.Context, capStr string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return "", false, err
	}
	var buf bytes.Buffer
	if err := r.cfg.Grid.Get(ctx, rc, &buf, nil); err != nil {
		return "", false, fmt.Errorf("directory: %w", err)
	}
	d, err := snapshot.DecodeDirectory(buf.Bytes())
	if err != nil {
		return "", false, err
	}
	changed := false
	for i := range d.Entries {
		en := &d.Entries[i]
		switch en.Kind {
		case snapshot.KindDir:
			newCap, c, err := r.convertDir(ctx, en.Cap)
			if err != nil {
				return "", false, err
			}
			if c {
				en.Cap, changed = newCap, true
			}
		case snapshot.KindFile:
			if en.Cap != "" {
				newCap, c, err := r.convertObject(ctx, en.Cap)
				if err != nil {
					return "", false, err
				}
				if c {
					en.Cap, changed = newCap, true
				}
			}
			for j, ch := range en.Chunks {
				newCap, c, err := r.convertObject(ctx, ch)
				if err != nil {
					return "", false, err
				}
				if c {
					en.Chunks[j], changed = newCap, true
				}
			}
		}
	}
	p, err := r.encodingOf(ctx, capStr)
	if err != nil {
		return "", false, err
	}
	if !changed && r.current(p) {
		return capStr, false, nil
	}
	b, err := d.Encode()
	if err != nil {
		return "", false, err
	}
	newRC, err := r.cfg.Grid.Put(ctx, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", false, fmt.Errorf("directory: %w", err)
	}
	r.params[newRC.String()] = r.target
	r.st.Objects++
	r.st.Bytes += int64(len(b))
	r.report()
	return newRC.String(), true, nil
}

// convertObject re-encodes one object unless it is current already.
func (r *run) convertObject(ctx context.Context, capStr string) (string, bool, error) {
	p, err := r.encodingOf(ctx, capStr)
	if err != nil {
		return "", false, err
	}
	if r.current(p) {
		return capStr, false, nil
	}
	if n, ok := r.replaced[capStr]; ok {
		return n, true, nil
	}
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return "", false, err
	}
	spool, err := os.CreateTemp(r.cfg.SpoolDir, "rn3-convert-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	if err := r.cfg.Grid.Get(ctx, rc, spool, nil); err != nil {
		return "", false, fmt.Errorf("object: %w", err)
	}
	if _, err := spool.Seek(0, 0); err != nil {
		return "", false, err
	}
	newRC, err := r.cfg.Grid.Put(ctx, spool, int64(rc.Size))
	if err != nil {
		return "", false, fmt.Errorf("object: %w", err)
	}
	r.params[newRC.String()] = r.target
	r.replaced[capStr] = newRC.String()
	r.st.Objects++
	r.st.Bytes += int64(rc.Size)
	r.report()
	return newRC.String(), true, nil
}

// publish stores the index and the root.
func (r *run) publish(ctx context.Context, x *snapshot.Index) error {
	b, err := x.Encode()
	if err != nil {
		return err
	}
	pctx := context.WithoutCancel(ctx)
	rc, err := r.cfg.Grid.Put(pctx, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if _, err := r.cfg.Grid.PutRoot(pctx, engine.Root{WrittenAt: r.cfg.Now(), IndexCap: rc.String()}); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	return nil
}

var _ Grid = (*engine.Native)(nil)
