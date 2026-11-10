package membership

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
)

func (w *world) descriptorOf(r *repo, ops ...*manifest.OperatorKey) *Descriptor {
	d := w.descriptor(r)
	d.Operators = nil
	for _, o := range ops {
		d.Operators = append(d.Operators, o.Public())
	}
	return d
}

func trustOf(m *Membership) []identity.Key {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]identity.Key(nil), m.trusted...)
}

// joined is a device that joined under op and applied revision 1.
func joined(t *testing.T, w *world, r *repo) string {
	t.Helper()
	r.publish(t, w.op, w.manifestYAML(1, false, w.op), time.Now())
	dir := t.TempDir()
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptor(r), Anchor: true, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	if m.Status().Revision != 1 {
		t.Fatalf("join: %+v", m.Status())
	}
	return dir
}

// The operator key was stolen. A new descriptor names another key. The
// device takes its trust from it: the thief's manifests stop counting at
// once, the member list on disk stays in force so the device keeps
// working, and the next list must come from the new key, above the
// revision the device has.
func TestANewDescriptorReplacesTheOperatorKeys(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	dir := joined(t, w, r)
	// Nothing under the new key is published yet.
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptorOf(r, w.op2), Anchor: true, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	if got := trustOf(m); len(got) != 1 || got[0] != w.op2.Public() {
		t.Fatalf("trusted after the new descriptor: %v", got)
	}
	if st := m.Status(); st.Revision != 1 || !st.AwaitingOperators {
		t.Fatalf("the list on disk did not stay in force: %+v", st)
	}
	if err := m.Allow(peerFor(t, w.alice, 1)); err != nil {
		t.Fatalf("a member of the list in force is refused: %v", err)
	}
	// The stolen key signs a newer list: refused.
	r.publish(t, w.op, w.manifestYAML(2, true, w.op), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 1 || !strings.Contains(st.LastError, "not one this device trusts") {
		t.Fatalf("a list signed by the replaced key: %+v", st)
	}
	// The new key signs the same revision, or a lower one: the floor holds.
	r.publish(t, w.op2, w.manifestYAML(1, true, w.op2), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); !st.AwaitingOperators || !strings.Contains(st.LastError, "must sign a higher revision") {
		t.Fatalf("the new key at the same revision: %+v", st)
	}
	// The new key lists the stolen one again: refused.
	r.publish(t, w.op2, w.manifestYAML(2, true, w.op2, w.op), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 1 || !strings.Contains(st.LastError, "dropped") {
		t.Fatalf("a list that brings the dropped key back: %+v", st)
	}
	// A restart in between changes nothing, with or without the descriptor.
	for _, d := range []*Descriptor{nil, w.descriptorOf(r, w.op2)} {
		again, err := Open(Config{DataDir: dir, Descriptor: d, Anchor: d != nil, Fetch: fetchFrom(r)})
		if err != nil {
			t.Fatal(err)
		}
		if got := trustOf(again); len(got) != 1 || got[0] != w.op2.Public() || again.Status().Revision != 1 || !again.Status().AwaitingOperators {
			t.Fatalf("after a restart: trusted %v, %+v", got, again.Status())
		}
	}
	// The new key signs a higher revision: applied, and the wait is over.
	r.publish(t, w.op2, w.manifestYAML(2, true, w.op2), time.Now())
	m.Refresh(context.Background())
	if st := m.Status(); st.Revision != 2 || st.AwaitingOperators || st.LastError != "" {
		t.Fatalf("the list under the new key: %+v", st)
	}
	after, err := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	if got := trustOf(after); len(got) != 1 || got[0] != w.op2.Public() || after.Status().Revision != 2 || after.Status().AwaitingOperators {
		t.Fatalf("after the new list and a restart: trusted %v, %+v", got, after.Status())
	}
	// The dropped key stays out for good, also in a later list.
	r.publish(t, w.op2, w.manifestYAML(3, true, w.op2, w.op), time.Now())
	after.Refresh(context.Background())
	if st := after.Status(); st.Revision != 2 || !strings.Contains(st.LastError, "dropped") {
		t.Fatalf("a later list that brings the dropped key back: %+v", st)
	}
}

// A descriptor that only catches up with a rotation the manifests made,
// an old descriptor given again, and one that merely came along, change
// nothing about whom the device trusts.
func TestADescriptorThatIsNotNewChangesNothing(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	dir := joined(t, w, r)
	// An ordinary rotation: op signs a list that names op2 alone.
	m, _ := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	r.publish(t, w.op, w.manifestYAML(2, false, w.op2), time.Now())
	m.Refresh(context.Background())
	if got := trustOf(m); m.Status().Revision != 2 || len(got) != 1 || got[0] != w.op2.Public() {
		t.Fatalf("rotation: %v %+v", got, m.Status())
	}
	// After a restart the device still has its list, though the key that
	// signed it has left the operators, and fetching it again is no error.
	restarted, err := Open(Config{DataDir: dir, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Refresh(context.Background())
	if st := restarted.Status(); st.Revision != 2 || st.LastError != "" {
		t.Fatalf("restart after the signer left: %+v", st)
	}
	if err := restarted.Allow(peerFor(t, w.alice, 1)); err != nil {
		t.Fatalf("a member is refused after the restart: %v", err)
	}
	// The key that left signs nothing new, also after the restart.
	r.publish(t, w.op, w.manifestYAML(3, true, w.op), time.Now())
	restarted.Refresh(context.Background())
	if st := restarted.Status(); st.Revision != 2 || !strings.Contains(st.LastError, "not one this device trusts") {
		t.Fatalf("the key that left signed a list: %+v", st)
	}
	r.publish(t, w.op, w.manifestYAML(2, false, w.op2), time.Now())
	check := func(what string, d *Descriptor, anchor bool) {
		t.Helper()
		x, err := Open(Config{DataDir: dir, Descriptor: d, Anchor: anchor, Fetch: fetchFrom(r)})
		if err != nil {
			t.Fatal(err)
		}
		if got := trustOf(x); len(got) != 1 || got[0] != w.op2.Public() || x.Status().Revision != 2 || x.Status().AwaitingOperators {
			t.Fatalf("%s: trusted %v, %+v", what, got, x.Status())
		}
		if _, err := os.Stat(filepath.Join(dir, fileAnchor)); err == nil {
			t.Fatalf("%s: left an anchor file", what)
		}
	}
	// The release after it carries the descriptor with op2: the same set
	// the device reached by itself.
	check("a descriptor that catches up", w.descriptorOf(r, w.op2), true)
	// The same descriptor again.
	check("the same descriptor again", w.descriptorOf(r, w.op2), true)
	// A descriptor with another key that only came along, as in a pairing offer.
	third, _ := manifest.NewOperatorKey()
	check("a descriptor that came along", w.descriptorOf(r, third), false)
}

// The files the previous binary reads are left as they were until a list
// under the new key arrives, so a failed update that puts the previous
// binary back finds a device it can start: the pinned keys and a list
// they signed.
func TestThePreviousBinaryStillStartsWhileWaiting(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	dir := joined(t, w, r)
	if _, err := Open(Config{DataDir: dir, Descriptor: w.descriptorOf(r, w.op2), Anchor: true, Fetch: fetchFrom(r)}); err != nil {
		t.Fatal(err)
	}
	pinned, err := loadTrust(filepath.Join(dir, fileTrust))
	if err != nil || len(pinned) != 1 || pinned[0] != w.op.Public() {
		t.Fatalf("operators.yaml while waiting: %v %v", pinned, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, fileManifest))
	sig, _ := os.ReadFile(filepath.Join(dir, fileSignature))
	s, err := manifest.ParseSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(data, pinned); err != nil {
		t.Fatalf("the list on disk does not verify under operators.yaml: %v", err)
	}
}

// Two operators, one key stolen: the descriptor keeps the other. A list
// the kept key signed earlier, which still names the stolen key, must not
// bring it back.
func TestAKeptKeysOlderListDoesNotBringTheDroppedKeyBack(t *testing.T) {
	w := newWorld(t)
	r := newRepo(t)
	r.publish(t, w.op, w.manifestYAML(1, false, w.op, w.op2), time.Now())
	dir := t.TempDir()
	m, err := Open(Config{DataDir: dir, Descriptor: w.descriptor(r), Anchor: true, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	m.Refresh(context.Background())
	// op is stolen; the descriptor now names op2 alone.
	x, err := Open(Config{DataDir: dir, Descriptor: w.descriptorOf(r, w.op2), Anchor: true, Fetch: fetchFrom(r)})
	if err != nil {
		t.Fatal(err)
	}
	if got := trustOf(x); len(got) != 1 || got[0] != w.op2.Public() || x.Status().Revision != 1 {
		t.Fatalf("after the descriptor: %v %+v", got, x.Status())
	}
	r.publish(t, w.op2, w.manifestYAML(5, false, w.op, w.op2), time.Now())
	x.Refresh(context.Background())
	if st := x.Status(); st.Revision != 1 || !strings.Contains(st.LastError, "dropped") {
		t.Fatalf("the kept key's list naming the stolen key: %+v", st)
	}
	r.publish(t, w.op2, w.manifestYAML(6, false, w.op2), time.Now())
	x.Refresh(context.Background())
	if st := x.Status(); st.Revision != 6 || st.AwaitingOperators {
		t.Fatalf("the kept key's clean list: %+v", st)
	}
}
