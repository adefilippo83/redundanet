package localdb

import (
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/object"
)

func TestObjectsAndRootVersion(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	d, err := Open(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var h object.Hash
	h[0] = 7
	p := object.DefaultParams(2, 4)
	rc := object.ReadCap{Size: 99}
	rc.Key[0], rc.DescriptorHash[0] = 1, 2
	if got, _ := d.LookupObject(h, p); got != nil {
		t.Fatal("unknown hash found")
	}
	if err := d.RememberObject(Object{PlaintextHash: h, Params: p, Cap: rc, Placed: 3}); err != nil {
		t.Fatal(err)
	}
	got, err := d.LookupObject(h, p)
	if err != nil || got == nil || got.Cap != rc || got.Placed != 3 {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	if other, _ := d.LookupObject(h, object.DefaultParams(3, 6)); other != nil {
		t.Fatal("another encoding must be a miss")
	}
	under, _ := d.UnderPlaced()
	if len(under) != 1 || under[0].Cap != rc {
		t.Fatalf("under-placed: %+v", under)
	}
	_ = d.RememberObject(Object{PlaintextHash: h, Params: p, Cap: rc, Placed: 4})
	if under, _ = d.UnderPlaced(); len(under) != 0 {
		t.Fatal("a fully placed object listed as under-placed")
	}
	v1, _ := d.NextRootVersion()
	v2, _ := d.NextRootVersion()
	if v1 != 1 || v2 != 2 {
		t.Fatalf("versions %d %d", v1, v2)
	}
	_ = d.RecoverRootVersion(10)
	if v, _ := d.RootVersion(); v != 10 {
		t.Fatalf("recovered to %d", v)
	}
	_ = d.RecoverRootVersion(3)
	if v, _ := d.RootVersion(); v != 10 {
		t.Fatal("recovery lowered the counter")
	}
}

func TestLookupByCapAndRewriteCaps(t *testing.T) {
	d, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var h object.Hash
	h[0] = 1
	old := object.DefaultParams(2, 4)
	rc := object.ReadCap{Size: 10}
	rc.Key[0] = 1
	if err := d.RememberObject(Object{PlaintextHash: h, Params: old, Cap: rc, Placed: 4}); err != nil {
		t.Fatal(err)
	}
	o, err := d.LookupByCap(rc.String())
	if err != nil || o == nil || o.Params != old || o.PlaintextHash != h || o.Placed != 4 {
		t.Fatalf("lookup by cap: %+v %v", o, err)
	}
	if o, err := d.LookupByCap("rn:r1:nope"); err != nil || o != nil {
		t.Fatalf("unknown cap: %+v %v", o, err)
	}
	// Scan rows carry the old capability as a file and as a chunk; the
	// rewrite replaces both and leaves the rest alone.
	newRC := rc
	newRC.Key[1] = 2
	other := rc
	other.Key[2] = 3
	rows := []ScanRow{
		{Path: "/a", Key: "k", Size: 10, Cap: rc.String()},
		{Path: "/b", Key: "k", Size: 10, Chunks: []string{other.String(), rc.String()}},
		{Path: "/c", Key: "k", Size: 10, Cap: other.String()},
	}
	for _, r := range rows {
		if err := d.RememberScan(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RewriteCaps(map[string]string{rc.String(): newRC.String()}); err != nil {
		t.Fatal(err)
	}
	a, _ := d.LookupScan("/a")
	b, _ := d.LookupScan("/b")
	c, _ := d.LookupScan("/c")
	if a.Cap != newRC.String() || len(b.Chunks) != 2 || b.Chunks[0] != other.String() || b.Chunks[1] != newRC.String() || c.Cap != other.String() {
		t.Fatalf("after the rewrite: %+v %+v %+v", a, b, c)
	}
}
