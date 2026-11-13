package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// squat is what a storage member that saw a storage index announced can
// make: a descriptor of its own, for its own bytes, signed by its own
// device, under that index.
func squat(t *testing.T, o *owner, si object.StorageIndex, data []byte) *obj {
	t.Helper()
	r := bytes.NewReader(data)
	h, _ := object.HashSpool(r, int64(len(data)))
	e, err := object.Encode(r, int64(len(data)), h, small, o.conv, o.member)
	if err != nil {
		t.Fatal(err)
	}
	blocks := make([][][]byte, small.N)
	for i := range blocks {
		blocks[i] = make([][]byte, e.Geometry().Segments())
	}
	_ = e.Shares(context.Background(), func(share, seg int, b []byte) error { blocks[share][seg] = append([]byte(nil), b...); return nil })
	d := e.Descriptor
	d.StorageIndex = si
	sd, err := object.Sign(d, o.dev, o.cert)
	if err != nil {
		t.Fatal(err)
	}
	return &obj{sd: sd, blocks: blocks}
}

func indexNames(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

// Until second owners are accepted a server behaves as it did with one
// descriptor per storage index: the member that got there first holds the
// index, and the owner's put is refused. This is the weakness the wider
// key is there to close, kept on purpose until every server has it.
func TestASecondOwnerIsRefusedUntilAccepted(t *testing.T) {
	s, _ := newStore(t)
	owner, squatter := newOwner(t, 1), newOwner(t, 2)
	ob := encode(t, owner, data(9000, 1))
	si := ob.sd.Descriptor.StorageIndex
	sq := squat(t, squatter, si, data(300, 7))
	if s.SecondOwners() {
		t.Fatal("a new store accepts second owners")
	}
	if ix := indexNames(t, s); !ix[oldShareKey] || !ix[oldTombstoneKey] {
		t.Fatalf("the old key is not unique any more: %v", ix)
	}
	putAll(t, s, sq, 0)
	if _, err := s.BeginPut(ob.sd, 0); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("the owner's put under a taken index: %v", err)
	}
	if _, err := s.BeginPut(ob.sd, 1); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("the owner's put of another number under a taken index: %v", err)
	}
	if p, _ := s.Has(owner.member, si); p != nil {
		t.Fatalf("something is held for the owner: %+v", p)
	}
	// A tombstone alone holds the index too, as it did.
	dh, _ := sq.sd.Descriptor.Hash()
	if err := s.Remove(squatter.member, si, 0, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tombstone(si, squatter.member, dh, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginPut(ob.sd, 0); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("the owner's put under an index another member's tombstone holds: %v", err)
	}
}

// The attack, and the end state: a second member registers a descriptor
// under an index first, and the owner's upload, read, deletion and
// membership all go through, each member's rows charged to and removed for
// that member alone.
func TestTwoOwnersUnderOneStorageIndex(t *testing.T) {
	s, c := newStore(t)
	owner, squatter := newOwner(t, 1), newOwner(t, 2)
	ob := encode(t, owner, data(9000, 1))
	si := ob.sd.Descriptor.StorageIndex
	sq := squat(t, squatter, si, data(300, 7))
	if err := s.AcceptSecondOwners(); err != nil {
		t.Fatal(err)
	}
	if ix := indexNames(t, s); ix[oldShareKey] || ix[oldTombstoneKey] || !ix["shares_place0"] {
		t.Fatalf("indexes after accepting second owners: %v", ix)
	}
	// The squatter is first, on two numbers, one of them left partial.
	putAll(t, s, sq, 0)
	if _, err := s.BeginPut(sq.sd, 1); err != nil {
		t.Fatal(err)
	}
	// The owner's upload goes through on every number, the squatter's
	// included, and resumes like any other.
	p, err := s.BeginPut(ob.sd, 0)
	if err != nil {
		t.Fatalf("the owner's put: %v", err)
	}
	if err := p.WriteBlock(0, ob.blocks[0][0]); err != nil {
		t.Fatal(err)
	}
	p.Abort()
	if p, err = s.BeginPut(ob.sd, 0); err != nil || p.Resume() != 1 {
		t.Fatalf("the owner's resume: %v", err)
	}
	p.Abort()
	for n := uint8(0); n < 4; n++ {
		putAll(t, s, ob, n)
	}
	// Each has its own files; the owner's are beside the squatter's.
	if got, want := s.sharePath(squatter.member, si, 0, placeIndex), filepath.Join(s.cfg.Root, "shares", si.String()[:2], si.String(), "0"); got != want {
		t.Fatalf("the first owner's path %s, want %s", got, want)
	}
	for _, f := range []string{s.sharePath(squatter.member, si, 0, placeIndex), s.sharePath(owner.member, si, 0, placeOwner), s.sharePath(owner.member, si, 3, placeOwner)} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("share file: %v", err)
		}
	}
	// Each is seen, read and accounted on its own.
	po, _ := s.Has(owner.member, si)
	ps, _ := s.Has(squatter.member, si)
	odh, _ := ob.sd.Descriptor.Hash()
	sdh, _ := sq.sd.Descriptor.Hash()
	if po == nil || len(po.Complete) != 4 || po.DescriptorHash != odh || po.Owner != owner.member {
		t.Fatalf("the owner's presence: %+v", po)
	}
	if ps == nil || len(ps.Complete) != 1 || len(ps.Partial) != 1 || ps.DescriptorHash != sdh {
		t.Fatalf("the squatter's presence: %+v", ps)
	}
	if owners, _ := s.Owners(si); len(owners) != 2 {
		t.Fatalf("owners under the index: %v", owners)
	}
	for n := uint8(0); n < 4; n++ {
		sh, err := s.OpenShare(owner.member, si, n)
		if err != nil {
			t.Fatalf("the owner's share %d: %v", n, err)
		}
		if sh.Header.Descriptor.Descriptor.Owner != owner.member || sh.Verify() != nil {
			t.Fatalf("the owner's share %d is not the owner's or does not verify", n)
		}
		b, _ := sh.Block(0)
		sh.Close()
		if !bytes.Equal(b, ob.blocks[n][0]) {
			t.Fatalf("the owner's share %d reads other bytes", n)
		}
	}
	sh, err := s.OpenShare(squatter.member, si, 0)
	if err != nil || sh.Header.Descriptor.Descriptor.Owner != squatter.member || sh.Verify() != nil {
		t.Fatalf("the squatter's share: %v", err)
	}
	sh.Close()
	if _, err := s.OpenShare(squatter.member, si, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a share the squatter never stored: %v", err)
	}
	ou, _ := s.Usage(owner.member)
	su, _ := s.Usage(squatter.member)
	if ou == 0 || su == 0 || ou <= su {
		t.Fatalf("usage: owner %d, squatter %d", ou, su)
	}
	if n, f, err := s.Scrub(context.Background(), 100); err != nil || n != 5 || f != 0 {
		t.Fatalf("scrub: %d checked, %d failed, %v", n, f, err)
	}
	d, _ := s.Census(0)
	mine, theirs := 0, 0
	for _, e := range d.Added {
		switch {
		case e.Owner == owner.member && e.DescriptorHash == odh:
			mine++
		case e.Owner == squatter.member && e.DescriptorHash == sdh:
			theirs++
		}
	}
	if mine != 4 || theirs != 1 {
		t.Fatalf("census: %d of the owner's, %d of the squatter's: %+v", mine, theirs, d.Added)
	}
	if l, _, _ := s.ListOwner(owner.member, object.StorageIndex{}, 10); len(l) != 1 || l[0].DescriptorHash != odh || l[0].Tombstoned {
		t.Fatalf("the owner's listing: %+v", l)
	}
	// The squatter's delete token tombstones the squatter's object alone,
	// and the purge takes the squatter's files alone.
	gen, _ := s.Generation()
	c.t = c.t.Add(Immunity + time.Hour)
	if _, err := s.ApplyDelete(si, squatter.member, sdh, c.t); err != nil {
		t.Fatal(err)
	}
	if po, _ = s.Has(owner.member, si); po.Tombstoned {
		t.Fatal("the squatter's token tombstoned the owner's object")
	}
	if l, _, _ := s.ListOwner(owner.member, object.StorageIndex{}, 10); len(l) != 1 || l[0].Tombstoned {
		t.Fatalf("the owner's listing after the squatter's token: %+v", l)
	}
	if _, err := s.ApplyDelete(si, squatter.member, odh, c.t); !errors.Is(err, ErrDescriptor) {
		t.Fatalf("the squatter's token for the owner's descriptor: %v", err)
	}
	if po, _ = s.Has(owner.member, si); po.Tombstoned {
		t.Fatal("a token naming the owner's descriptor, signed by another member, tombstoned the owner's object")
	}
	c.t = c.t.Add(TombstoneWindow + time.Hour)
	if n, err := s.PurgeTombstones(); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := os.Stat(s.sharePath(squatter.member, si, 0, placeIndex)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the squatter's file outlived its purge")
	}
	if po, _ = s.Has(owner.member, si); len(po.Complete) != 4 {
		t.Fatalf("the owner's shares after the squatter's purge: %+v", po)
	}
	d, _ = s.Census(gen)
	if len(d.Removed) != 2 {
		t.Fatalf("removals reported: %+v", d.Removed)
	}
	for _, k := range d.Removed {
		if k.Owner != squatter.member {
			t.Fatalf("a removal reported for %s", k.Owner.Short())
		}
	}
	// Across a restart the store still accepts second owners and holds
	// both.
	root, record := s.cfg.Root, s.cfg.RecordPath
	_ = s.Close()
	s2, err := Open(Config{Root: root, RecordPath: record, Floor: 1, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if !s2.SecondOwners() {
		t.Fatal("the store forgot that it accepts second owners")
	}
	if ix := indexNames(t, s2); ix[oldShareKey] || ix[oldTombstoneKey] {
		t.Fatalf("the old key is unique again: %v", ix)
	}
	// The squatter is back, now second: its files go beside the owner's.
	putAll(t, s2, sq, 0)
	if _, err := os.Stat(s2.sharePath(squatter.member, si, 0, placeIndex)); err != nil {
		t.Fatalf("the index's own directory was free again: %v", err)
	}
	// The squatter leaves the network: its rows go after the grace, and
	// nothing of the owner's.
	active := func(m identity.Key) bool { return m == owner.member }
	_, _ = s2.ApplyMembership(active, time.Hour)
	c.t = c.t.Add(2 * time.Hour)
	if purged, err := s2.ApplyMembership(active, time.Hour); err != nil || len(purged) != 1 || purged[0] != squatter.member {
		t.Fatalf("membership: %v %v", purged, err)
	}
	if owners, _ := s2.Owners(si); len(owners) != 1 || owners[0] != owner.member {
		t.Fatalf("owners after the squatter left: %v", owners)
	}
	for n := uint8(0); n < 4; n++ {
		sh, err := s2.OpenShare(owner.member, si, n)
		if err != nil || sh.Verify() != nil {
			t.Fatalf("the owner's share %d after the squatter left: %v", n, err)
		}
		sh.Close()
	}
}

// The owner first, the squatter after: nothing of the owner's moves.
func TestTheFirstOwnerKeepsItsPlace(t *testing.T) {
	s, _ := newStore(t)
	owner, squatter := newOwner(t, 1), newOwner(t, 2)
	ob := encode(t, owner, data(5000, 1))
	si := ob.sd.Descriptor.StorageIndex
	putAll(t, s, ob, 0)
	if err := s.AcceptSecondOwners(); err != nil {
		t.Fatal(err)
	}
	sq := squat(t, squatter, si, data(300, 7))
	putAll(t, s, sq, 0)
	putAll(t, s, ob, 1)
	for n := uint8(0); n < 2; n++ {
		if _, err := os.Stat(s.sharePath(owner.member, si, n, placeIndex)); err != nil {
			t.Fatalf("the owner's share %d: %v", n, err)
		}
	}
	if _, err := os.Stat(s.sharePath(squatter.member, si, 0, placeOwner)); err != nil {
		t.Fatalf("the squatter's share: %v", err)
	}
	if err := s.Remove(squatter.member, si, 0, "test"); err != nil {
		t.Fatal(err)
	}
	sh, err := s.OpenShare(owner.member, si, 0)
	if err != nil || sh.Verify() != nil {
		t.Fatalf("the owner's share after the squatter's removal: %v", err)
	}
	sh.Close()
}

// The statements of the version before the wider key, as that version has
// them. A server that fails its probation goes back to it, on the index
// this version rebuilt.
var before = struct {
	schema                                                         []string
	insertShare, completeShare, hasShares, hasTombstone, tombstone string
	removeShare, removed, censusAdded, censusRemoved, usage, list  string
}{
	schema: []string{
		`CREATE TABLE IF NOT EXISTS shares (
			si BLOB NOT NULL, n INTEGER NOT NULL, owner BLOB NOT NULL, dhash BLOB NOT NULL,
			k INTEGER NOT NULL, nn INTEGER NOT NULL, size INTEGER NOT NULL, state TEXT NOT NULL,
			segments INTEGER NOT NULL, received INTEGER NOT NULL,
			created INTEGER NOT NULL, last_touched INTEGER NOT NULL, verified INTEGER, gen INTEGER NOT NULL,
			PRIMARY KEY (si, n))`,
		`CREATE INDEX IF NOT EXISTS shares_owner ON shares(owner)`,
		`CREATE INDEX IF NOT EXISTS shares_gen ON shares(gen)`,
		`CREATE INDEX IF NOT EXISTS shares_verified ON shares(state, verified)`,
		`CREATE TABLE IF NOT EXISTS removed (si BLOB NOT NULL, n INTEGER NOT NULL, gen INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS removed_gen ON removed(gen)`,
		`CREATE TABLE IF NOT EXISTS tombstones (si BLOB PRIMARY KEY, owner BLOB NOT NULL, dhash BLOB NOT NULL,
			issued INTEGER NOT NULL, created INTEGER NOT NULL, purged INTEGER)`,
		`CREATE TABLE IF NOT EXISTS roots (member BLOB NOT NULL, device BLOB NOT NULL, version INTEGER NOT NULL,
			envelope BLOB NOT NULL, received INTEGER NOT NULL, PRIMARY KEY (member, device, version))`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('gen', 0)`,
		`CREATE TABLE IF NOT EXISTS missing (member BLOB PRIMARY KEY, since INTEGER NOT NULL)`,
	},
	insertShare: `INSERT INTO shares (si, n, owner, dhash, k, nn, size, state, segments, received, created, last_touched, verified, gen)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'partial', ?, 0, ?, ?, NULL, 0)`,
	completeShare: `UPDATE shares SET state = 'complete', received = ?, last_touched = ?, gen = ? WHERE si = ? AND n = ?`,
	hasShares:     `SELECT n, owner, dhash, state, received, last_touched FROM shares WHERE si = ? ORDER BY n`,
	hasTombstone:  `SELECT owner, dhash, purged FROM tombstones WHERE si = ?`,
	tombstone:     `INSERT OR REPLACE INTO tombstones (si, owner, dhash, issued, created, purged) VALUES (?, ?, ?, ?, ?, NULL)`,
	removeShare:   `DELETE FROM shares WHERE si = ? AND n = ?`,
	removed:       `INSERT INTO removed (si, n, gen) VALUES (?, ?, ?)`,
	censusAdded:   `SELECT si, n, owner, size, k, nn, dhash FROM shares WHERE state = 'complete' AND gen > ? ORDER BY gen`,
	censusRemoved: `SELECT si, n FROM removed WHERE gen > ? ORDER BY gen`,
	usage:         `SELECT SUM(size) FROM shares WHERE owner = ?`,
	list: `SELECT s.si, s.dhash, MAX(s.last_touched), t.si IS NOT NULL FROM shares s LEFT JOIN tombstones t ON t.si = s.si
		WHERE s.owner = ? AND s.si > ? GROUP BY s.si ORDER BY s.si LIMIT ?`,
}

// oldIndex makes an index as the version before the wider key left it:
// objects of two members, each with complete shares, some tombstoned, and
// removals on record.
func oldIndex(t *testing.T, root string, objects int) (members [2]identity.Key, gen uint64) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range before.schema {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	members[0][0], members[1][0] = 1, 2
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < objects; i++ {
		var si object.StorageIndex
		si[0], si[1], si[2], si[15] = byte(i>>16), byte(i>>8), byte(i), 9
		var dh object.Hash
		dh[0], dh[1], dh[2] = byte(i), byte(i>>8), byte(i>>16)
		m := members[i%2]
		for n := 0; n < 2; n++ {
			if _, err := tx.Exec(before.insertShare, si[:], n, m[:], dh[:], 2, 3, 4096*(1+i%3), 1, 1000+i, 2000+i); err != nil {
				t.Fatal(err)
			}
			gen++
			if _, err := tx.Exec(before.completeShare, 1, 2000+i, gen, si[:], n); err != nil {
				t.Fatal(err)
			}
		}
		if i%10 == 0 {
			if _, err := tx.Exec(before.tombstone, si[:], m[:], dh[:], 3000, 3000); err != nil {
				t.Fatal(err)
			}
		}
		if i%7 == 0 {
			if _, err := tx.Exec(before.removeShare, si[:], 1); err != nil {
				t.Fatal(err)
			}
			gen++
			if _, err := tx.Exec(before.removed, si[:], 1, gen); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'gen'`, gen); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return members, gen
}

// facts is what must be the same before and after the rebuild: the census
// generation, the usage per owner, every complete share with its owner,
// every removal on record and every tombstone.
type facts struct {
	gen        uint64
	usage      [2]int64
	added      int
	removed    int
	tombstones int
	sum        int64 // a checksum over the census's rows
}

func factsOf(t *testing.T, db *sql.DB, members [2]identity.Key) facts {
	t.Helper()
	var f facts
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'gen'`).Scan(&f.gen); err != nil {
		t.Fatal(err)
	}
	for i, m := range members {
		var u sql.NullInt64
		if err := db.QueryRow(before.usage, m[:]).Scan(&u); err != nil {
			t.Fatal(err)
		}
		f.usage[i] = u.Int64
	}
	rows, err := db.Query(before.censusAdded, 0)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var si, owner, dhash []byte
		var n, k, nn int
		var size int64
		if err := rows.Scan(&si, &n, &owner, &size, &k, &nn, &dhash); err != nil {
			t.Fatal(err)
		}
		f.added++
		f.sum += int64(si[2])*31 + int64(n)*7 + int64(owner[0])*131 + size + int64(dhash[0])
	}
	rows.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM removed`).Scan(&f.removed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM tombstones`).Scan(&f.tombstones); err != nil {
		t.Fatal(err)
	}
	return f
}

// The rebuild of an index from before the wider key loses nothing, and
// the version before still works on what it leaves: a rollback during
// probation returns to a store laid out as that version needs it.
func TestTheWiderKeyKeepsAnOldIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "disk")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	const objects = 3000
	members, gen := oldIndex(t, root, objects)
	raw, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	was := factsOf(t, raw, members)
	_ = raw.Close()
	if was.gen != gen || was.added == 0 || was.removed == 0 || was.tombstones == 0 {
		t.Fatalf("the old index is not what the test means: %+v", was)
	}

	// The marker of a store that exists already.
	record := filepath.Join(t.TempDir(), "node", "storage-id")
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{record, filepath.Join(root, markerName)} {
		if err := os.WriteFile(f, []byte("0123\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	s, err := Open(Config{Root: root, RecordPath: record, Floor: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("an index of %d objects and %d shares rebuilt and opened in %s", objects, was.added, time.Since(start).Round(time.Millisecond))
	if is := factsOf(t, s.db, members); is != was {
		t.Fatalf("the rebuild changed the index:\nwas %+v\nis  %+v", was, is)
	}
	if g, _ := s.Generation(); g != gen {
		t.Fatalf("generation %d, was %d", g, gen)
	}
	for i, m := range members {
		if u, _ := s.Usage(m); u != was.usage[i] {
			t.Fatalf("usage of member %d: %d, was %d", i, u, was.usage[i])
		}
	}
	d, err := s.Census(0)
	if err != nil || len(d.Added) != was.added || len(d.Tombstones) != was.tombstones {
		t.Fatalf("census after the rebuild: %d added, %d tombstones, %v", len(d.Added), len(d.Tombstones), err)
	}
	// A removal the version before recorded has no owner, and says so.
	d, _ = s.Census(1)
	if len(d.Removed) != was.removed || !d.Removed[0].Owner.IsZero() {
		t.Fatalf("removals after the rebuild: %d, was %d", len(d.Removed), was.removed)
	}
	// Every object is where it was: the first of each member.
	var si0, si1 object.StorageIndex
	si0[15], si1[2], si1[15] = 9, 1, 9
	p0, _ := s.Has(members[0], si0)
	p1, _ := s.Has(members[1], si1)
	if p0 == nil || !p0.Tombstoned || len(p0.Complete) != 1 || p0.place != placeIndex {
		t.Fatalf("the first member's first object: %+v", p0)
	}
	if p1 == nil || p1.Tombstoned || len(p1.Complete) != 2 {
		t.Fatalf("the second member's first object: %+v", p1)
	}
	if p, _ := s.Has(members[1], si0); p != nil {
		t.Fatalf("the first member's object shows for the second: %+v", p)
	}
	if ix := indexNames(t, s); !ix[oldShareKey] || !ix[oldTombstoneKey] || !ix["shares_owner"] || !ix["shares_gen"] || !ix["shares_verified"] {
		t.Fatalf("indexes after the rebuild: %v", ix)
	}
	// A second start finds nothing to rebuild.
	_ = s.Close()
	if s, err = Open(Config{Root: root, RecordPath: record, Floor: 1}); err != nil {
		t.Fatal(err)
	}
	if is := factsOf(t, s.db, members); is != was {
		t.Fatalf("a second start changed the index:\nwas %+v\nis  %+v", was, is)
	}
	_ = s.Close()

	// The version before, back on this index: its own schema statements do
	// nothing, and every statement it runs does what it did.
	old, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for _, q := range before.schema {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("the version before, opening the rebuilt index: %v", err)
		}
	}
	if is := factsOf(t, old, members); is != was {
		t.Fatalf("the version before reads other facts:\nwas %+v\nis  %+v", was, is)
	}
	var si object.StorageIndex
	si[0], si[15] = 0xee, 9
	var dh object.Hash
	dh[0] = 0xee
	m, other := members[0], members[1]
	if _, err := old.Exec(before.insertShare, si[:], 0, m[:], dh[:], 2, 3, 4096, 1, 5000, 5000); err != nil {
		t.Fatalf("the version before, storing a share: %v", err)
	}
	// One row per storage index and share number, whoever the owner: that
	// version reads a share by those two alone.
	if _, err := old.Exec(before.insertShare, si[:], 0, other[:], dh[:], 2, 3, 4096, 1, 5000, 5000); err == nil {
		t.Fatal("the version before stored a second row under one storage index and share number")
	}
	if _, err := old.Exec(before.completeShare, 1, 5001, gen+1, si[:], 0); err != nil {
		t.Fatal(err)
	}
	var n, received int
	var owner, dhash []byte
	var state string
	var touched int64
	if err := old.QueryRow(before.hasShares, si[:]).Scan(&n, &owner, &dhash, &state, &received, &touched); err != nil || state != "complete" || !bytes.Equal(owner, m[:]) {
		t.Fatalf("the version before, reading its share: %v %s", err, state)
	}
	// A tombstone replaces the one under the same storage index.
	for _, who := range []identity.Key{m, other} {
		if _, err := old.Exec(before.tombstone, si[:], who[:], dh[:], 6000, 6000); err != nil {
			t.Fatalf("the version before, writing a tombstone: %v", err)
		}
	}
	var count int
	if err := old.QueryRow(`SELECT COUNT(*) FROM tombstones WHERE si = ?`, si[:]).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d tombstones under one storage index after the version before wrote two: %v", count, err)
	}
	var purged sql.NullInt64
	if err := old.QueryRow(before.hasTombstone, si[:]).Scan(&owner, &dhash, &purged); err != nil || !bytes.Equal(owner, other[:]) {
		t.Fatalf("the version before, reading its tombstone: %v", err)
	}
	if res, err := old.Exec(before.removeShare, si[:], 0); err != nil {
		t.Fatal(err)
	} else if k, _ := res.RowsAffected(); k != 1 {
		t.Fatalf("the version before removed %d rows", k)
	}
	if _, err := old.Exec(before.removed, si[:], 0, gen+2); err != nil {
		t.Fatalf("the version before, recording a removal: %v", err)
	}
	rows, err := old.Query(before.censusRemoved, gen)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for rows.Next() {
		seen++
	}
	rows.Close()
	if seen != 1 {
		t.Fatalf("the version before reports %d removals since the rebuild", seen)
	}
	rows, err = old.Query(before.list, m[:], make([]byte, 16), 5)
	if err != nil {
		t.Fatalf("the version before, listing: %v", err)
	}
	listed := 0
	for rows.Next() {
		listed++
	}
	rows.Close()
	if listed != 5 {
		t.Fatalf("the version before lists %d objects", listed)
	}
	_ = old.Close()

	// And this version again, on what that one wrote.
	if s, err = Open(Config{Root: root, RecordPath: record, Floor: 1}); err != nil {
		t.Fatalf("this version, after the version before ran: %v", err)
	}
	defer s.Close()
	d, _ = s.Census(gen)
	if len(d.Removed) != 1 || !d.Removed[0].Owner.IsZero() || d.Removed[0].StorageIndex != si {
		t.Fatalf("the removal the version before recorded: %+v", d.Removed)
	}
	if p, _ := s.Has(other, si); p == nil || !p.Tombstoned {
		t.Fatalf("the tombstone the version before wrote: %+v", p)
	}
}

// TestIndexCopy rebuilds a copy of a live node's index: the check asked
// for before a release that changes the tables. It runs only with
// RN3_STORE_INDEX_COPY set to a copy of a store's index.sqlite, which it
// does not change: it works on a copy of its own. It reports how long the
// rebuild took and fails unless the census generation, the usage of every
// owner, every share row, every removal on record and every tombstone
// are the same before and after.
func TestIndexCopy(t *testing.T) {
	src := os.Getenv("RN3_STORE_INDEX_COPY")
	if src == "" {
		t.Skip("RN3_STORE_INDEX_COPY is not set")
	}
	root := filepath.Join(t.TempDir(), "disk")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dbName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "storage-id")
	for _, f := range []string{record, filepath.Join(root, markerName)} {
		if err := os.WriteFile(f, []byte("copy\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// One line per fact, in a fixed order, from statements that read the
	// same on both layouts.
	snapshot := func(db *sql.DB) []string {
		var out []string
		for _, q := range []string{
			`SELECT 'gen', value, 0, 0 FROM meta WHERE key = 'gen'`,
			`SELECT 'usage', hex(owner), SUM(size), COUNT(*) FROM shares GROUP BY owner ORDER BY owner`,
			`SELECT 'shares', COUNT(*), COALESCE(SUM(gen + n + received + segments + k + nn), 0), COALESCE(SUM(state = 'complete'), 0) FROM shares`,
			`SELECT 'removed', COUNT(*), COALESCE(SUM(gen + n), 0), 0 FROM removed`,
			`SELECT 'tombstones', COUNT(*), COALESCE(SUM(issued + created), 0), COALESCE(SUM(purged IS NOT NULL), 0) FROM tombstones`,
			`SELECT 'roots', COUNT(*), COALESCE(SUM(version), 0), 0 FROM roots`,
		} {
			rows, err := db.Query(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			for rows.Next() {
				var a, b, c, d sql.NullString
				if err := rows.Scan(&a, &b, &c, &d); err != nil {
					t.Fatal(err)
				}
				out = append(out, a.String+" "+b.String+" "+c.String+" "+d.String)
			}
			rows.Close()
		}
		return out
	}
	raw, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	was := snapshot(raw)
	_ = raw.Close()
	start := time.Now()
	s, err := Open(Config{Root: root, RecordPath: record, Floor: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	took := time.Since(start)
	is := snapshot(s.db)
	t.Logf("rebuilt and opened in %s", took.Round(time.Millisecond))
	for _, l := range is {
		t.Log(l)
	}
	if len(was) != len(is) {
		t.Fatalf("the rebuild changed the index: %d facts before, %d after", len(was), len(is))
	}
	for i := range was {
		if was[i] != is[i] {
			t.Fatalf("the rebuild changed the index:\nwas %s\nis  %s", was[i], is[i])
		}
	}
	if ix := indexNames(t, s); !ix[oldShareKey] || !ix[oldTombstoneKey] {
		t.Fatalf("the old key is not unique on the rebuilt index: %v", ix)
	}
}
