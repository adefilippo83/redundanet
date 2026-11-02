package diskfree

import "testing"

func TestFree(t *testing.T) {
	free, total, err := Free(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free > total {
		t.Fatalf("free %d of total %d", free, total)
	}
	if _, _, err := Free(t.TempDir() + "/missing"); err == nil {
		t.Fatal("a missing path should fail")
	}
}
