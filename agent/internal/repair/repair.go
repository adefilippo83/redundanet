// Package repair is the node side of repair without the owner, RFC 0001's
// "Health" section: a storage node that volunteers as a worker takes a
// task the scheduling hub signed, fetches the descriptor and k shares from
// the holders, regenerates the missing share numbers without the key,
// checks them against the signed share roots and stores them where the
// task says, then reports. It also answers a hub's surplus hint, giving a
// share up only after confirming with the other holders itself that every
// share number is held elsewhere.
package repair

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// Config wires a worker.
type Config struct {
	Device   identity.Key
	Manifest func() *manifest.Manifest
	Client   *shareproto.Client
	// Store is this node's own store: shares it holds are read from it,
	// and a share the task places on this node is written to it, since a
	// node cannot open a stream to itself.
	Store   *store.Store
	Timeout time.Duration // per server operation; default 30 s
	Now     func() time.Time
	Log     *slog.Logger
}

// queueSize bounds the tasks waiting; a hub never issues more than a few.
const queueSize = 32

type job struct {
	task   *wire.RepairTask
	report func(*wire.TaskReport) error
}

// Worker runs one task at a time.
type Worker struct {
	cfg   Config
	log   *slog.Logger
	queue chan job

	mu sync.Mutex
	st Status
}

// Status is what the local page shows about repair.
type Status struct {
	Queued   int       `json:"queued"`
	Running  string    `json:"running,omitempty"` // the object under repair
	Done     int       `json:"done"`              // tasks completed since start
	Failed   int       `json:"failed"`            // tasks that failed since start
	Released int       `json:"released"`          // surplus shares given up since start
	Last     string    `json:"last,omitempty"`    // the last outcome, in words
	LastAt   time.Time `json:"last_at,omitempty"`
}

// New wires a worker.
func New(cfg Config) (*Worker, error) {
	if cfg.Manifest == nil || cfg.Client == nil || cfg.Store == nil || cfg.Device.IsZero() {
		return nil, errors.New("repair: device, manifest, client and store are required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Worker{cfg: cfg, log: cfg.Log.With("component", "repair"), queue: make(chan job, queueSize)}, nil
}

// Status snapshots the worker.
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := w.st
	st.Queued = len(w.queue)
	return st
}

// Handle queues a task; the transport calls it for every task a hub sends.
// A full queue answers the hub at once, so it reassigns.
func (w *Worker) Handle(t *wire.RepairTask, report func(*wire.TaskReport) error) {
	select {
	case w.queue <- job{task: t, report: report}:
	default:
		_ = report(&wire.TaskReport{ID: t.ID, Failed: "the worker's queue is full"})
	}
}

// Run takes tasks from the queue until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-w.queue:
			w.mu.Lock()
			w.st.Running = objectID(j.task.StorageIndex)
			w.mu.Unlock()
			done, err := w.run(ctx, j.task)
			r := &wire.TaskReport{ID: j.task.ID, Done: done}
			w.mu.Lock()
			w.st.Running = ""
			w.st.LastAt = w.cfg.Now()
			if err != nil {
				r.Failed = err.Error()
				w.st.Failed++
				w.st.Last = fmt.Sprintf("task %d for %s: %v", j.task.ID, objectID(j.task.StorageIndex), err)
				w.log.Warn("repair task failed", "task", j.task.ID, "si", objectID(j.task.StorageIndex), "done", done, "err", err)
			} else {
				w.st.Done++
				w.st.Last = fmt.Sprintf("task %d for %s: stored %d shares", j.task.ID, objectID(j.task.StorageIndex), len(done))
				w.log.Info("repair task done", "task", j.task.ID, "si", objectID(j.task.StorageIndex), "shares", done)
			}
			w.mu.Unlock()
			if err := j.report(r); err != nil {
				w.log.Info("repair report not sent", "task", j.task.ID, "err", err)
			}
		}
	}
}

func objectID(si [16]byte) string { return object.StorageIndex(si).String()[:8] }

// putStream is a share being stored, on a server or on this node.
type putStream interface {
	Write(segment int, b []byte) error
	Finish() error
	Abandon()
}

// localPut stores a share in this node's own store.
type localPut struct {
	p      *store.Put
	resume int
}

func (l *localPut) Write(segment int, b []byte) error {
	if segment < l.resume {
		return nil
	}
	return l.p.WriteBlock(segment, b)
}
func (l *localPut) Finish() error { return l.p.Complete() }
func (l *localPut) Abandon()      { l.p.Abort() }

// run does one task.
func (w *Worker) run(ctx context.Context, t *wire.RepairTask) (done []uint8, err error) {
	m := w.cfg.Manifest()
	if m == nil {
		return nil, errors.New("no manifest")
	}
	if err := t.Verify(m.Network.Name, w.cfg.Now()); err != nil {
		return nil, err
	}
	if m.HubByKey(t.Hub) == nil {
		return nil, errors.New("the task is not signed by a hub of this network")
	}
	if t.Worker != w.cfg.Device {
		return nil, errors.New("the task names another worker")
	}
	si := object.StorageIndex(t.StorageIndex)
	dhash := object.Hash(t.DescriptorHash)

	// The holders: the task's hint first, then every other storage server,
	// until k distinct share numbers under the task's descriptor are in hand.
	var candidates []identity.Key
	seenDev := map[identity.Key]bool{}
	for _, dev := range t.Holders {
		if !seenDev[dev] {
			candidates, seenDev[dev] = append(candidates, dev), true
		}
	}
	for _, s := range m.Storage {
		if !seenDev[s.Device] {
			candidates, seenDev[s.Device] = append(candidates, s.Device), true
		}
	}
	var sd *object.SignedDescriptor
	var sources []object.Source
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	have := map[uint8]bool{}
	var lastErr error
	for _, dev := range candidates {
		if sd != nil && len(have) >= int(sd.Descriptor.K) {
			break
		}
		pres, err := w.has(ctx, dev, si)
		if err != nil {
			lastErr = err
			continue
		}
		if pres == nil || pres.DescriptorHash != dhash || pres.Tombstoned {
			continue
		}
		for _, n := range pres.Complete {
			if have[n] {
				continue
			}
			src, info, closer, err := w.source(ctx, dev, si, n)
			if err != nil {
				lastErr = err
				continue
			}
			if sd == nil {
				if err := w.checkDescriptor(info, m.Network.Name, si, dhash, t.Owner); err != nil {
					closer()
					lastErr = err
					continue
				}
				sd = info
			}
			sources = append(sources, src)
			closers = append(closers, closer)
			have[n] = true
			if len(have) >= int(sd.Descriptor.K) {
				break
			}
		}
	}
	if sd == nil {
		if lastErr != nil {
			return nil, fmt.Errorf("no holder could give the descriptor: %w", lastErr)
		}
		return nil, errors.New("no holder could give the descriptor")
	}
	if len(have) < int(sd.Descriptor.K) {
		return nil, fmt.Errorf("%d of the %d shares needed are reachable", len(have), sd.Descriptor.K)
	}

	// The targets: a put per share the task places; a server that already
	// holds the share is done at once.
	streams := map[uint8]putStream{}
	failed := map[uint8]string{}
	for _, s := range t.Shares {
		ps, complete, err := w.beginPut(ctx, s.Server, sd, s.Number, t)
		switch {
		case err != nil:
			failed[s.Number] = s.Server.Short() + ": " + err.Error()
		case complete:
			done = append(done, s.Number)
		default:
			streams[s.Number] = ps
		}
	}
	var wanted []uint8
	for n := range streams {
		wanted = append(wanted, n)
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i] < wanted[j] })
	if len(wanted) > 0 {
		rctx, cancel := context.WithTimeout(ctx, time.Until(t.Expires))
		defer cancel()
		_, err := object.Reconstruct(rctx, &sd.Descriptor, sources, wanted, func(n uint8, segment int, block []byte) error {
			if failed[n] != "" {
				return nil
			}
			if err := streams[n].Write(segment, block); err != nil {
				failed[n] = err.Error()
			}
			return rctx.Err()
		})
		if err != nil {
			for _, ps := range streams {
				ps.Abandon()
			}
			return done, err
		}
		for _, n := range wanted {
			ps := streams[n]
			if failed[n] != "" {
				ps.Abandon()
				continue
			}
			if err := ps.Finish(); err != nil {
				failed[n] = err.Error()
				continue
			}
			done = append(done, n)
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i] < done[j] })
	if len(failed) > 0 {
		var parts []string
		for _, s := range t.Shares {
			if msg, ok := failed[s.Number]; ok {
				parts = append(parts, fmt.Sprintf("share %d: %s", s.Number, msg))
			}
		}
		return done, errors.New(strings.Join(parts, "; "))
	}
	return done, nil
}

func (w *Worker) checkDescriptor(sd *object.SignedDescriptor, network string, si object.StorageIndex, dhash object.Hash, owner identity.Key) error {
	if sd == nil {
		return errors.New("a holder served a share without a descriptor")
	}
	if err := sd.Verify(network); err != nil {
		return err
	}
	h, err := sd.Descriptor.Hash()
	if err != nil {
		return err
	}
	if h != dhash || sd.Descriptor.StorageIndex != si || sd.Descriptor.Owner != owner {
		return errors.New("a holder's descriptor is not the task's")
	}
	return nil
}

// has asks a server, or this node's own store, what it holds.
func (w *Worker) has(ctx context.Context, dev identity.Key, si object.StorageIndex) (*shareproto.Presence, error) {
	if dev == w.cfg.Device {
		p, err := w.cfg.Store.Has(si)
		if err != nil || p == nil {
			return nil, err
		}
		return &shareproto.Presence{DescriptorHash: p.DescriptorHash, Owner: p.Owner, Complete: p.Complete, Partial: p.Partial, Tombstoned: p.Tombstoned}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	return w.cfg.Client.Has(cctx, dev, si)
}

// source is one share as a decode source, with its signed descriptor and
// a closer for the local file.
func (w *Worker) source(ctx context.Context, dev identity.Key, si object.StorageIndex, n uint8) (object.Source, *object.SignedDescriptor, func(), error) {
	if dev == w.cfg.Device {
		sh, err := w.cfg.Store.OpenShare(si, n)
		if err != nil {
			return object.Source{}, nil, nil, err
		}
		return object.Source{Share: int(n), Hashes: sh.Hashes, Read: func(_ context.Context, segment int) ([]byte, error) { return sh.Block(segment) }},
			sh.Header.Descriptor, func() { sh.Close() }, nil
	}
	cctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	sd, hashes, err := w.cfg.Client.GetInfo(cctx, dev, si, n)
	if err != nil {
		return object.Source{}, nil, nil, err
	}
	if sd == nil {
		return object.Source{}, nil, nil, errors.New("a holder served a share without a descriptor")
	}
	geom := sd.Descriptor.Geometry()
	read := func(ctx context.Context, segment int) ([]byte, error) {
		rctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
		defer cancel()
		var block []byte
		_, err := w.cfg.Client.GetShare(rctx, dev, si, n, segment, segment+1, geom, func(_ int, b []byte) error { block = b; return nil })
		return block, err
	}
	return object.Source{Share: int(n), Hashes: hashes, Read: read}, sd, func() {}, nil
}

// beginPut opens the put of a share on its target, or on this node.
func (w *Worker) beginPut(ctx context.Context, dev identity.Key, sd *object.SignedDescriptor, n uint8, t *wire.RepairTask) (putStream, bool, error) {
	if dev == w.cfg.Device {
		p, err := w.cfg.Store.BeginRepair(sd, n)
		if errors.Is(err, store.ErrAlreadyComplete) {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		return &localPut{p: p, resume: p.Resume()}, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	ps, err := w.cfg.Client.BeginRepairPut(cctx, dev, sd, n, t)
	if err != nil {
		return nil, false, err
	}
	if ps.Complete() {
		ps.Abandon()
		return nil, true, nil
	}
	return ps, false, nil
}

// Release answers a hub's surplus hint in the background.
func (w *Worker) Release(h *wire.ReleaseHint) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*w.cfg.Timeout)
		defer cancel()
		if err := w.release(ctx, h); err != nil {
			w.log.Info("surplus share kept", "si", objectID(h.StorageIndex), "n", h.Number, "reason", err)
			return
		}
		w.mu.Lock()
		w.st.Released++
		w.mu.Unlock()
	}()
}

// release removes this node's copy of a share once the object would still
// have every share number complete without it. The other holders are
// asked directly, and their answers carry no proof, so each is believed
// for no more share numbers than placement gives one server (see
// releasable): a hub and a storage server that lie together cannot talk
// the honest holders out of their shares.
func (w *Worker) release(ctx context.Context, h *wire.ReleaseHint) error {
	m := w.cfg.Manifest()
	if m == nil {
		return errors.New("no manifest")
	}
	si := object.StorageIndex(h.StorageIndex)
	sh, err := w.cfg.Store.OpenShare(si, h.Number)
	if err != nil {
		return err
	}
	sd := sh.Header.Descriptor
	sh.Close()
	dhash, err := sd.Descriptor.Hash()
	if err != nil {
		return err
	}
	if dhash != object.Hash(h.DescriptorHash) {
		return errors.New("the hint names another descriptor")
	}
	listed := map[identity.Key]bool{}
	for _, s := range m.Storage {
		listed[s.Device] = true
	}
	others := map[identity.Key][]uint8{}
	for _, dev := range h.Holders {
		if dev == w.cfg.Device || !listed[dev] {
			continue
		}
		pres, err := w.has(ctx, dev, si)
		if err != nil || pres == nil || pres.DescriptorHash != dhash || pres.Tombstoned {
			continue
		}
		others[dev] = pres.Complete
	}
	var mine []uint8
	if p, err := w.cfg.Store.Has(si); err == nil && p != nil && p.DescriptorHash == dhash {
		mine = p.Complete
	}
	if err := releasable(h.Number, int(sd.Descriptor.N), len(m.Storage), mine, others); err != nil {
		return err
	}
	return w.cfg.Store.Remove(si, h.Number, "surplus: every share number stays complete without it")
}

// releasable decides whether this node may give up its copy of share
// number n of an object with total share numbers, on a network of servers
// storage servers. mine is what this node holds complete, others what
// each other holder says it holds.
//
// Every share number must stay accounted for without this copy: by this
// node's other shares, or by another holder. Another holder's word counts
// for at most ceil(total / servers) share numbers, the most that placement
// gives one server, and each number is counted on one holder. So what a
// release can cost an object is bounded by what the lying servers were
// trusted to hold in the first place: with every honest holder following
// this rule, the honest servers together keep every share number that
// only they hold.
func releasable(n uint8, total, servers int, mine []uint8, others map[identity.Key][]uint8) error {
	if total < 1 || servers < 1 || int(n) >= total {
		return errors.New("the hint does not fit the object")
	}
	kept := map[uint8]bool{}
	for _, s := range mine {
		if s != n {
			kept[s] = true
		}
	}
	var need []uint8 // the numbers another holder must account for; n first
	need = append(need, n)
	for s := 0; s < total; s++ {
		if uint8(s) != n && !kept[uint8(s)] {
			need = append(need, uint8(s))
		}
	}
	maxPer := (total + servers - 1) / servers
	devs := make([]identity.Key, 0, len(others))
	claims := map[identity.Key]map[uint8]bool{}
	for dev, shares := range others {
		devs = append(devs, dev)
		claims[dev] = map[uint8]bool{}
		for _, s := range shares {
			claims[dev][s] = true
		}
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Less(devs[j]) })
	// A matching of share numbers to holders, each holder taking at most
	// maxPer: augmenting paths, which the few servers and at most 255
	// numbers make cheap.
	assigned := map[uint8]identity.Key{}
	load := map[identity.Key][]uint8{}
	var place func(s uint8, seen map[identity.Key]bool) bool
	place = func(s uint8, seen map[identity.Key]bool) bool {
		for _, dev := range devs {
			if !claims[dev][s] || seen[dev] {
				continue
			}
			seen[dev] = true
			if len(load[dev]) < maxPer {
				assigned[s] = dev
				load[dev] = append(load[dev], s)
				return true
			}
			for i, held := range load[dev] {
				if place(held, seen) {
					load[dev][i] = s
					assigned[s] = dev
					return true
				}
			}
		}
		return false
	}
	for i, s := range need {
		if !place(s, map[identity.Key]bool{}) {
			if i == 0 {
				return errors.New("no other server confirmed holding this share number")
			}
			return fmt.Errorf("only %d of %d share numbers would stay accounted for, each holder counted for at most %d", len(kept)+len(assigned), total, maxPer)
		}
	}
	return nil
}
