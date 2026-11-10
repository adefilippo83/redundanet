package object

import (
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// A descriptor is a claim by whoever signed it, and a server works out
// sizes from it before any data arrives. Claims outside the bounds are
// refused, and the size of a share is worked out in constant time.
func TestGeometryIsBounded(t *testing.T) {
	var owner identity.Key
	owner[0] = 1
	base := Descriptor{Version: FormatVersion, Size: 3 << 20, SegmentSize: DefaultSegmentSize, K: 2, N: 3, Cipher: CipherAESGCM, ShareRoots: make([]Hash, 3), Owner: owner}
	if err := base.check(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Descriptor){
		"a segment of one byte":            func(d *Descriptor) { d.SegmentSize = 1 },
		"a segment below the minimum":      func(d *Descriptor) { d.SegmentSize = MinSegmentSize - 1 },
		"a segment above the maximum":      func(d *Descriptor) { d.SegmentSize = MaxSegmentSize + 1 },
		"a size above the maximum":         func(d *Descriptor) { d.Size = MaxObjectSize + 1 },
		"a size that wraps the arithmetic": func(d *Descriptor) { d.Size = ^uint64(0) },
		"too many segments":                func(d *Descriptor) { d.SegmentSize, d.Size = MinSegmentSize, uint64(MaxSegments+1)*MinSegmentSize },
	} {
		d := base
		change(&d)
		if err := d.check(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, change := range map[string]func(*Descriptor){
		"the largest object":   func(d *Descriptor) { d.Size = MaxObjectSize },
		"the smallest segment": func(d *Descriptor) { d.SegmentSize, d.Size = MinSegmentSize, 10*MinSegmentSize },
		"the most segments":    func(d *Descriptor) { d.SegmentSize, d.Size = MinSegmentSize, uint64(MaxSegments)*MinSegmentSize },
		"an empty object":      func(d *Descriptor) { d.Size = 0 },
	} {
		d := base
		change(&d)
		if err := d.check(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The closed form equals the walk over the segments.
	for _, g := range []Geometry{{0, 4096, 2}, {1, 4096, 2}, {4096, 4096, 3}, {4097, 4096, 3}, {3<<20 + 777, DefaultSegmentSize, 2}, {10 * 8192, 8192, 7}} {
		var walked int64
		for s := 0; s < g.Segments(); s++ {
			walked += int64(g.BlockSize(s))
		}
		if got := g.ShareBlocksSize(); got != walked {
			t.Errorf("%+v: %d, the walk gives %d", g, got, walked)
		}
	}
	start := time.Now()
	_ = Geometry{Size: MaxObjectSize, SegmentSize: MinSegmentSize, K: 1}.ShareBlocksSize()
	if d := time.Since(start); d > time.Second {
		t.Errorf("the size of a share took %v to work out", d)
	}
}
