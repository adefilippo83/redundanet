package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// Fake is an in-memory engine for the jobs' tests: the same contract, no
// network, no disk, full placement always.
type Fake struct {
	Member identity.Key
	Device identity.Key
	K, N   int
	Params object.Params

	mu        sync.Mutex
	objects   map[object.Hash][]byte // plaintext hash -> bytes
	caps      map[object.Hash]object.ReadCap
	tombstone map[object.Hash]bool // deleted: the owner may still read, nothing else counts it
	roots     map[identity.Key]RootView
	unopen    map[identity.Key]bool // roots that do not open with the member's key
	version   uint64
	Puts      int // how many uploads actually happened
	// UsageBytes, when set, is what Usage reports instead of the sum of
	// the objects held, times n/k: a test's budget scenario.
	UsageBytes int64
	Deletes    int // how many delete tokens were sent
	Checks     int // how many placement checks were asked
	// RootsAnswered is how many servers GetRoots reports answering; -1
	// means all of them.
	RootsAnswered int
}

// NewFake makes a fake engine for one member and device.
func NewFake(member, device identity.Key, k, n int) *Fake {
	return &Fake{Member: member, Device: device, K: k, N: n, Params: object.DefaultParams(k, n), objects: map[object.Hash][]byte{}, caps: map[object.Hash]object.ReadCap{},
		tombstone: map[object.Hash]bool{}, roots: map[identity.Key]RootView{}, unopen: map[identity.Key]bool{}, RootsAnswered: -1}
}

// SetUnopenable makes a device's root one the member's key does not open.
func (f *Fake) SetUnopenable(device identity.Key) {
	f.mu.Lock()
	f.unopen[device] = true
	f.mu.Unlock()
}

func (f *Fake) capOf(data []byte) (object.Hash, object.ReadCap) {
	ph := object.Sum(data)
	var conv [32]byte
	copy(conv[:], f.Member[:])
	key := object.DeriveKey(conv, ph, f.Params)
	return ph, object.ReadCap{Key: key, DescriptorHash: object.Sum(append([]byte("desc"), ph[:]...)), Size: uint64(len(data))}
}

// Put stores the bytes; a repeat is a no-op.
func (f *Fake) Put(_ context.Context, r io.Reader, size int64) (object.ReadCap, error) {
	data, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return object.ReadCap{}, err
	}
	if int64(len(data)) != size {
		return object.ReadCap{}, ErrSize
	}
	ph, rc := f.capOf(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[ph]; !ok || f.tombstone[ph] {
		f.objects[ph] = data
		f.caps[ph] = rc
		delete(f.tombstone, ph)
		f.Puts++
	}
	return rc, nil
}

func (f *Fake) find(rc object.ReadCap) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ph, c := range f.caps {
		if c == rc {
			return f.objects[ph], true
		}
	}
	return nil, false
}

// Get reads the bytes back.
func (f *Fake) Get(_ context.Context, rc object.ReadCap, w io.Writer, rng *Range) error {
	data, ok := f.find(rc)
	if !ok {
		return ErrNotReadable
	}
	if rng != nil {
		if rng.Offset < 0 || rng.Length < 0 || rng.Offset+rng.Length > int64(len(data)) {
			return errors.New("engine: range outside the object")
		}
		data = data[rng.Offset : rng.Offset+rng.Length]
	}
	_, err := w.Write(data)
	return err
}

// Has looks the bytes up by hash; a tombstoned object is not held.
func (f *Fake) Has(_ context.Context, ph object.Hash, size int64) (object.ReadCap, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, ok := f.caps[ph]
	if !ok || rc.Size != uint64(size) || f.tombstone[ph] {
		return object.ReadCap{}, false, nil
	}
	return rc, true, nil
}

// Check reports full placement for anything stored and not tombstoned.
func (f *Fake) Check(_ context.Context, rc object.ReadCap) (*Health, error) {
	f.mu.Lock()
	f.Checks++
	f.mu.Unlock()
	h := &Health{Cap: rc.Verify(), K: f.K, N: f.N, Holders: map[identity.Key][]uint8{}, Asked: f.N, Answered: f.N}
	if _, ok := f.find(rc); !ok {
		return h, nil
	}
	if f.Tombstoned(rc) {
		h.Tombstoned = true
		return h, nil
	}
	for i := 0; i < f.N; i++ {
		h.Shares = append(h.Shares, uint8(i))
		var dev identity.Key
		dev[0] = byte(i + 1)
		h.Holders[dev] = []uint8{uint8(i)}
	}
	h.Servers, h.Members, h.Readable, h.Placed = f.N, f.N, true, true
	return h, nil
}

// Repair has nothing to do.
func (f *Fake) Repair(ctx context.Context, rc object.ReadCap) (*Health, error) {
	return f.Check(ctx, rc)
}

// Delete tombstones the object: the owner may still read it, Has and
// Check no longer count it, and a Put brings it back.
func (f *Fake) Delete(_ context.Context, rc object.ReadCap) error {
	_, err := f.DeleteByIndex(context.Background(), rc.Key.StorageIndex(), rc.DescriptorHash)
	return err
}

// DeleteByIndex is Delete by storage index and descriptor hash.
func (f *Fake) DeleteByIndex(_ context.Context, si object.StorageIndex, dhash object.Hash) (*DeleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Deletes++
	res := &DeleteResult{Servers: f.N, Answered: f.N, Failed: map[identity.Key]string{}}
	for ph, c := range f.caps {
		if c.Key.StorageIndex() == si && c.DescriptorHash == dhash {
			f.tombstone[ph] = true
			res.Tombstoned = f.N
			return res, nil
		}
	}
	res.NotHeld = f.N
	return res, nil
}

// Tombstoned reports whether Delete marked the object.
func (f *Fake) Tombstoned(rc object.ReadCap) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ph, c := range f.caps {
		if c == rc {
			return f.tombstone[ph]
		}
	}
	return false
}

// ListObjects lists everything stored, tombstoned or not.
func (f *Fake) ListObjects(context.Context) (*Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &Listing{Objects: map[object.StorageIndex]*ListedObject{}, Answered: f.N, Servers: f.N}
	for ph, c := range f.caps {
		si := c.Key.StorageIndex()
		o := &ListedObject{StorageIndex: si, DescriptorHash: c.DescriptorHash, Tombstoned: f.tombstone[ph]}
		for i := 0; i < f.N; i++ {
			var dev identity.Key
			dev[0] = byte(i + 1)
			o.Holders = append(o.Holders, dev)
		}
		out.Objects[si] = o
	}
	return out, nil
}

// AddRoot records another device's root, for the cleanup's tests.
func (f *Fake) AddRoot(device identity.Key, root Root) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	r := root
	f.roots[device] = RootView{Device: device, Version: f.version, Root: &r}
}

// DropRoot forgets a device's root, as a tombstoned root would after the
// window.
func (f *Fake) DropRoot(device identity.Key) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.roots, device)
}

// PutRoot keeps the highest root per device.
func (f *Fake) PutRoot(_ context.Context, root Root) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version++
	if root.WrittenAt.IsZero() {
		root.WrittenAt = time.Now()
	}
	r := root
	f.roots[f.Device] = RootView{Device: f.Device, Version: f.version, Root: &r}
	return f.version, nil
}

// GetRoots lists the member's roots.
func (f *Fake) GetRoots(_ context.Context, member identity.Key) (*Roots, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &Roots{Answered: f.N, Servers: f.N}
	if f.RootsAnswered >= 0 {
		out.Answered = f.RootsAnswered
	}
	if member != f.Member {
		return out, nil
	}
	for dev, v := range f.roots {
		if f.unopen[dev] {
			v.Root = nil
		}
		out.Views = append(out.Views, v)
	}
	return out, nil
}

// Census is empty: the fake holds no server.
func (f *Fake) Census(context.Context, uint64) (*store.CensusDelta, error) {
	return &store.CensusDelta{}, nil
}

// Bytes returns an object's bytes, for tests.
func (f *Fake) Bytes(rc object.ReadCap) []byte {
	data, _ := f.find(rc)
	return bytes.Clone(data)
}

var _ Engine = (*Fake)(nil)
var _ Engine = (*Native)(nil)

// Usage is the objects held, as share bytes, or the override.
func (f *Fake) Usage(_ context.Context) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UsageBytes > 0 {
		return Usage{Bytes: f.UsageBytes, Servers: 1, Answered: 1}, nil
	}
	var logical int64
	for h, b := range f.objects {
		if !f.tombstone[h] {
			logical += int64(len(b))
		}
	}
	k, n := f.K, f.N
	if k == 0 || n == 0 {
		k, n = 1, 1
	}
	return Usage{Bytes: logical * int64(n) / int64(k), Servers: 1, Answered: 1}, nil
}
