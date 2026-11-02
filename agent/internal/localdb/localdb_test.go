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
