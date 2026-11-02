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

	mu      sync.Mutex
	objects map[object.Hash][]byte // plaintext hash -> bytes
	caps    map[object.Hash]object.ReadCap
	roots   map[identity.Key]RootView
	version uint64
	Puts    int // how many uploads actually happened
}

// NewFake makes a fake engine for one member and device.
func NewFake(member, device identity.Key, k, n int) *Fake {
	return &Fake{Member: member, Device: device, K: k, N: n, Params: object.DefaultParams(k, n), objects: map[object.Hash][]byte{}, caps: map[object.Hash]object.ReadCap{}, roots: map[identity.Key]RootView{}}
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
	if _, ok := f.objects[ph]; !ok {
		f.objects[ph] = data
		f.caps[ph] = rc
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

// Has looks the bytes up by hash.
func (f *Fake) Has(_ context.Context, ph object.Hash, size int64) (object.ReadCap, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc, ok := f.caps[ph]
	if !ok || rc.Size != uint64(size) {
		return object.ReadCap{}, false, nil
	}
	return rc, true, nil
}

// Check reports full placement for anything stored.
func (f *Fake) Check(_ context.Context, rc object.ReadCap) (*Health, error) {
	h := &Health{Cap: rc.Verify(), K: f.K, N: f.N, Holders: map[identity.Key][]uint8{}, Asked: f.N, Answered: f.N}
	if _, ok := f.find(rc); !ok {
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

// Delete forgets the object.
func (f *Fake) Delete(_ context.Context, rc object.ReadCap) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ph, c := range f.caps {
		if c == rc {
			delete(f.caps, ph)
			delete(f.objects, ph)
			return nil
		}
	}
	return ErrNotReadable
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
	if member != f.Member {
		return out, nil
	}
	for _, v := range f.roots {
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
