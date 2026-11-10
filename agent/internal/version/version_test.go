package version_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/release"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// The release key written in this package is the one that verifies the
// release index in this repository, directly or through the index's own
// rotation chain. A build made from this source can therefore follow the
// maintainer's index and no other; a change to the key that the index
// does not carry fails here.
func TestReleaseKeyVerifiesTheIndex(t *testing.T) {
	key, err := identity.ParseKey(version.ReleaseKey)
	if err != nil {
		t.Fatalf("the release key in source: %v", err)
	}
	dir := filepath.Join("..", "..", "..", "releases")
	data, err := os.ReadFile(filepath.Join(dir, "index.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "index.sig"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := release.ParseSignature(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sig.Verify(data, key, time.Now()); err != nil {
		t.Fatalf("releases/index.yaml does not verify under the release key in source: %v", err)
	}
	var other identity.Key
	other[0] = 1
	if _, _, err := sig.Verify(data, other, time.Now()); err == nil {
		t.Fatal("the index verified under a key that did not sign it")
	}
}
