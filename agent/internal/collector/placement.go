package collector

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite" // the placement table, pure Go

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The placement table is the grid-wide view of RFC 0001's "Health"
// section: every complete share the storage nodes report in their share
// census, every tombstone, and per object the history the repair rules
// need. It lives in SQLite so a node that is down keeps counting across a
// hub restart, as the RFC asks, and so the page's numbers survive one.

// Thresholds of the "Health" section.
const (
	// quiescence is how long an object that never reached its target is
	// left alone before it becomes a repair task: a slow first backup is
	// still sending its shares.
	quiescence = 24 * time.Hour
)

type placement struct {
	db *sql.DB
}

func openPlacement(path string) (*placement, error) {
	dsn := ":memory:"
	if path != "" {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS shares (device BLOB NOT NULL, si BLOB NOT NULL, n INTEGER NOT NULL, owner BLOB NOT NULL,
			size INTEGER NOT NULL, k INTEGER NOT NULL, nn INTEGER NOT NULL, dhash BLOB NOT NULL, PRIMARY KEY (device, si, n))`,
		`CREATE INDEX IF NOT EXISTS shares_si ON shares(si)`,
		`CREATE TABLE IF NOT EXISTS tombstones (device BLOB NOT NULL, si BLOB NOT NULL, owner BLOB NOT NULL, dhash BLOB NOT NULL,
			purged INTEGER NOT NULL, PRIMARY KEY (device, si))`,
		`CREATE TABLE IF NOT EXISTS devices (device BLOB PRIMARY KEY, generation INTEGER NOT NULL, updated INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS objects (si BLOB PRIMARY KEY, first_seen INTEGER NOT NULL, last_change INTEGER NOT NULL, reached INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tasks (id INTEGER PRIMARY KEY AUTOINCREMENT, si BLOB NOT NULL, worker BLOB NOT NULL, issued INTEGER NOT NULL,
			expires INTEGER NOT NULL, bytes INTEGER NOT NULL, task BLOB NOT NULL, state TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '', ended INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS tasks_state ON tasks(state)`,
	} {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("collector: placement table: %w", err)
		}
	}
	return &placement{db: db}, nil
}

func (p *placement) close() error { return p.db.Close() }

// apply records a chunk of a storage node's share census. A chunk since
// generation zero under a new generation replaces what the table held for
// the device: the node was rebuilt, or its store was.
func (p *placement) apply(device identity.Key, sc *wire.ShareCensus, now time.Time) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var gen sql.NullInt64
	if err := tx.QueryRow(`SELECT generation FROM devices WHERE device = ?`, device[:]).Scan(&gen); err != nil && err != sql.ErrNoRows {
		return err
	}
	if sc.Since == 0 && gen.Valid && gen.Int64 != 0 && uint64(gen.Int64) != sc.Generation {
		for _, q := range []string{`DELETE FROM shares WHERE device = ?`, `DELETE FROM tombstones WHERE device = ?`} {
			if _, err := tx.Exec(q, device[:]); err != nil {
				return err
			}
		}
		gen.Int64 = 0
	}
	ts := now.Unix()
	touch := func(si [16]byte) error {
		_, err := tx.Exec(`INSERT INTO objects (si, first_seen, last_change, reached) VALUES (?, ?, ?, 0)
			ON CONFLICT (si) DO UPDATE SET last_change = excluded.last_change`, si[:], ts, ts)
		return err
	}
	for _, e := range sc.Added {
		var have int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM shares WHERE device = ? AND si = ? AND n = ? AND dhash = ?`, device[:], e.StorageIndex[:], e.Number, e.DescriptorHash[:]).Scan(&have); err != nil {
			return err
		}
		if have > 0 {
			continue // the node resent what the table holds
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO shares (device, si, n, owner, size, k, nn, dhash) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			device[:], e.StorageIndex[:], e.Number, e.Owner[:], e.Size, e.K, e.N, e.DescriptorHash[:]); err != nil {
			return err
		}
		if err := touch(e.StorageIndex); err != nil {
			return err
		}
	}
	for _, k := range sc.Removed {
		res, err := tx.Exec(`DELETE FROM shares WHERE device = ? AND si = ? AND n = ?`, device[:], k.StorageIndex[:], k.Number)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if err := touch(k.StorageIndex); err != nil {
				return err
			}
		}
	}
	// Tombstones are listed whole: a full census replaces them even when
	// it lists none, a delta replaces them when it carries any.
	if (sc.Since == 0 && !sc.More) || len(sc.Tombstones) > 0 {
		if _, err := tx.Exec(`DELETE FROM tombstones WHERE device = ?`, device[:]); err != nil {
			return err
		}
		for _, t := range sc.Tombstones {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO tombstones (device, si, owner, dhash, purged) VALUES (?, ?, ?, ?, ?)`,
				device[:], t.StorageIndex[:], t.Owner[:], t.DescriptorHash[:], boolInt(t.Purged)); err != nil {
				return err
			}
		}
	}
	g := gen.Int64
	if !sc.More {
		g = int64(sc.Generation)
	}
	if _, err := tx.Exec(`INSERT INTO devices (device, generation, updated) VALUES (?, ?, ?)
		ON CONFLICT (device) DO UPDATE SET generation = excluded.generation, updated = excluded.updated`, device[:], g, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// forget drops everything a device reported: it was retired from the
// manifest, so its shares count no more.
func (p *placement) forget(device identity.Key) error {
	for _, q := range []string{`DELETE FROM shares WHERE device = ?`, `DELETE FROM tombstones WHERE device = ?`, `DELETE FROM devices WHERE device = ?`} {
		if _, err := p.db.Exec(q, device[:]); err != nil {
			return err
		}
	}
	return nil
}

// devices lists the storage devices the table holds a census of.
func (p *placement) devices() []identity.Key {
	rows, err := p.db.Query(`SELECT device FROM devices`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []identity.Key
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return out
		}
		var k identity.Key
		copy(k[:], b)
		out = append(out, k)
	}
	return out
}

// Object is one object's health as the placement table sees it: what the
// scheduler of M5's repair tasks works from. Shares on dead servers are
// left out, as the RFC says; shares on servers that are merely down
// count, because home servers reboot.
type Object struct {
	StorageIndex [16]byte
	Owner        identity.Key
	Hash         [32]byte // the descriptor hash the holders agree on
	K, N         int
	Size         int64                    // bytes of one share as accounted
	Holders      map[identity.Key][]uint8 // complete share numbers per server not dead
	Shares       []uint8                  // distinct complete share numbers
	Missing      []uint8                  // share numbers no server not dead holds
	Spread       int                      // distinct servers holding at least one
	Target       int                      // servers the object should spread over: min(n, servers not dead)
	Placed       bool                     // every share number held and the spread reached
	Readable     bool                     // at least k distinct shares
	Surplus      int                      // complete shares beyond n
	Conflict     bool                     // servers disagree on the descriptor
	Tombstoned   bool                     // a server holds a tombstone for it
	Reached      bool                     // it was fully placed at some point: a loss since is a server's death or a failed check
	LastChange   time.Time                // when a holder last gained or lost one of its shares
	Need         string                   // "" (nothing to do), "quiescing" or "repair"
}

// Grid is the public projection of the placement table: counts, never a
// storage index or an owner.
type Grid struct {
	Servers     int            `json:"servers"`      // storage servers not dead
	Objects     int            `json:"objects"`      // with at least one complete share on a server not dead
	Copies      map[string]int `json:"copies"`       // objects per number of distinct shares held
	Placed      int            `json:"placed"`       // fully placed
	UnderPlaced int            `json:"under_placed"` // short of a share number or of spread
	Unreadable  int            `json:"unreadable"`   // fewer than k shares: lost unless a dead server returns
	Surplus     int            `json:"surplus"`      // objects with more complete shares than n
	Conflicts   int            `json:"conflicts"`    // objects whose servers disagree on the descriptor
	Tombstoned  int            `json:"tombstoned"`   // deleted by their owner, in the 30-day window or purged
	Quiescing   int            `json:"quiescing"`    // under-placed but younger than the quiescence window
	Repair      int            `json:"repair"`       // objects a repair task is due for
	Bytes       int64          `json:"bytes"`        // bytes of complete shares on servers not dead
	// The repair scheduler's view, filled by the collector.
	RepairRunning int    `json:"repair_running"`      // tasks handed to workers and not yet closed
	RepairDone    int    `json:"repair_done_24h"`     // tasks closed as done in the last day
	RepairFailed  int    `json:"repair_failed_24h"`   // tasks that failed or expired in the last day
	RepairBlocked int    `json:"repair_blocked"`      // objects due with no server to place on
	RepairShort   int64  `json:"repair_short_bytes"`  // bytes the live servers lack above their floors for them
	Scheduler     string `json:"scheduler,omitempty"` // the hub scheduling repair, as this hub sees it
	// Conversion is the k-of-n change in progress, when the manifest
	// declares one.
	Conversion *Conversion `json:"conversion,omitempty"`
}

// Conversion is the progress of a k-of-n change across the grid: the
// objects under the manifest's encoding against all, and what the old
// encoding still holds, accounted at zero until the deadline.
type Conversion struct {
	From      string `json:"from"`     // "2-of-4"
	To        string `json:"to"`       //
	Deadline  string `json:"deadline"` // YYYY-MM-DD
	Counting  bool   `json:"counting"` // the deadline has passed: old-encoding shares count again
	Objects   int    `json:"objects"`  // objects not deleted
	Converted int    `json:"converted"`
	OldBytes  int64  `json:"old_bytes"` // bytes of shares under the old encoding
}

// encodingOf is "k-of-n".
func encodingOf(k, n int) string { return fmt.Sprintf("%d-of-%d", k, n) }

// compute walks the table once and classifies every object. alive lists
// the storage servers whose shares count, servers is how many there are;
// a dead, retired or never-seen server's rows are skipped. enc is the
// manifest's encoding and conv its transition, nil without one.
func (p *placement) compute(now time.Time, alive map[identity.Key]bool, servers int, enc manifest.Encoding, conv *manifest.Transition) ([]Object, *Grid, error) {
	rows, err := p.db.Query(`SELECT device, si, n, owner, size, k, nn, dhash FROM shares ORDER BY si, n, device`)
	if err != nil {
		return nil, nil, err
	}
	type row struct {
		device identity.Key
		n      uint8
		size   int64
		k, nn  int
		dhash  [32]byte
		owner  identity.Key
	}
	bySI := map[[16]byte][]row{}
	var order [][16]byte
	for rows.Next() {
		var dev, si, owner, dhash []byte
		var r row
		var n int
		if err := rows.Scan(&dev, &si, &n, &owner, &r.size, &r.k, &r.nn, &dhash); err != nil {
			rows.Close()
			return nil, nil, err
		}
		copy(r.device[:], dev)
		copy(r.owner[:], owner)
		copy(r.dhash[:], dhash)
		r.n = uint8(n)
		if !alive[r.device] {
			continue
		}
		var key [16]byte
		copy(key[:], si)
		if _, ok := bySI[key]; !ok {
			order = append(order, key)
		}
		bySI[key] = append(bySI[key], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	tomb := map[[16]byte]bool{}
	rows, err = p.db.Query(`SELECT device, si FROM tombstones`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var dev, si []byte
		if err := rows.Scan(&dev, &si); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var d identity.Key
		copy(d[:], dev)
		if !alive[d] {
			continue
		}
		var key [16]byte
		copy(key[:], si)
		tomb[key] = true
	}
	rows.Close()
	history := map[[16]byte]struct {
		lastChange int64
		reached    bool
	}{}
	rows, err = p.db.Query(`SELECT si, last_change, reached FROM objects`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var si []byte
		var h struct {
			lastChange int64
			reached    bool
		}
		if err := rows.Scan(&si, &h.lastChange, &h.reached); err != nil {
			rows.Close()
			return nil, nil, err
		}
		var key [16]byte
		copy(key[:], si)
		history[key] = h
	}
	rows.Close()

	g := &Grid{Servers: servers, Copies: map[string]int{}}
	if conv != nil {
		g.Conversion = &Conversion{From: encodingOf(conv.From.K, conv.From.N), To: encodingOf(conv.To.K, conv.To.N), Deadline: conv.Deadline}
		if d, err := conv.DeadlineTime(); err == nil && !now.Before(d) {
			g.Conversion.Counting = true
		}
	}
	var objects []Object
	var reachedNow [][16]byte
	for _, si := range order {
		rs := bySI[si]
		// The descriptor the holders agree on: the one with the most shares.
		votes := map[[32]byte]int{}
		for _, r := range rs {
			votes[r.dhash]++
		}
		var best [32]byte
		bestN := -1
		for h, n := range votes {
			if n > bestN || (n == bestN && string(h[:]) < string(best[:])) {
				best, bestN = h, n
			}
		}
		o := Object{StorageIndex: si, Hash: best, Holders: map[identity.Key][]uint8{}, Conflict: len(votes) > 1, Tombstoned: tomb[si]}
		distinct := map[uint8]bool{}
		total := 0
		for _, r := range rs {
			if r.dhash != best {
				continue
			}
			o.Owner, o.K, o.N = r.owner, r.k, r.nn
			if r.size > o.Size {
				o.Size = r.size
			}
			o.Holders[r.device] = append(o.Holders[r.device], r.n)
			distinct[r.n] = true
			total++
			g.Bytes += r.size
		}
		for n := range distinct {
			o.Shares = append(o.Shares, n)
		}
		sort.Slice(o.Shares, func(i, j int) bool { return o.Shares[i] < o.Shares[j] })
		for n := 0; n < o.N; n++ {
			if !distinct[uint8(n)] {
				o.Missing = append(o.Missing, uint8(n))
			}
		}
		o.Spread = len(o.Holders)
		o.Target = min(o.N, servers)
		o.Placed = len(o.Shares) == o.N && o.Spread >= o.Target
		o.Readable = len(o.Shares) >= o.K
		if total > o.N {
			o.Surplus = total - o.N
		}
		h := history[si]
		o.Reached = h.reached
		o.LastChange = time.Unix(h.lastChange, 0)
		if o.Placed && !h.reached {
			o.Reached = true
			reachedNow = append(reachedNow, si)
		}
		switch {
		case o.Tombstoned, o.Placed:
		case o.Reached:
			o.Need = "repair" // it had every share: a server died or a share failed its check
		case now.Sub(o.LastChange) >= quiescence:
			o.Need = "repair"
		default:
			o.Need = "quiescing"
		}
		objects = append(objects, o)
		g.Objects++
		// A deleted object is neither placed nor under-placed: it counts
		// as tombstoned while a server still reports a share of it, and a
		// record kept after the purge is bookkeeping.
		if o.Tombstoned {
			g.Tombstoned++
			continue
		}
		if c := g.Conversion; c != nil {
			c.Objects++
			if o.K == enc.K && o.N == enc.N {
				c.Converted++
			} else {
				c.OldBytes += o.Size * int64(len(o.Shares))
			}
		}
		g.Copies[fmt.Sprint(len(o.Shares))]++
		if o.Placed {
			g.Placed++
		} else {
			g.UnderPlaced++
		}
		if !o.Readable {
			g.Unreadable++
		}
		if o.Surplus > 0 {
			g.Surplus++
		}
		if o.Conflict {
			g.Conflicts++
		}
		switch o.Need {
		case "quiescing":
			g.Quiescing++
		case "repair":
			g.Repair++
		}
	}
	if len(reachedNow) > 0 {
		tx, err := p.db.Begin()
		if err != nil {
			return nil, nil, err
		}
		for _, si := range reachedNow {
			if _, err := tx.Exec(`UPDATE objects SET reached = 1 WHERE si = ?`, si[:]); err != nil {
				_ = tx.Rollback()
				return nil, nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
	}
	return objects, g, nil
}

// usage sums what every storage node accounts to each owner, and the
// distinct objects per owner: the public "usage as servers see it". Every
// server counts here, dead or not, because the bytes are on its disk.
// With an encoding being retired before its deadline, its shares are
// accounted at zero, as the servers do, and reported apart; the objects
// still under it are counted per owner.
func (p *placement) usage(old *manifest.Encoding, counting bool) (bytes, oldBytes map[identity.Key]int64, objects, oldObjects map[identity.Key]int, err error) {
	bytes, oldBytes, objects, oldObjects = map[identity.Key]int64{}, map[identity.Key]int64{}, map[identity.Key]int{}, map[identity.Key]int{}
	rows, err := p.db.Query(`SELECT owner, k, nn, SUM(size), COUNT(DISTINCT si) FROM shares GROUP BY owner, k, nn`)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner []byte
		var k, nn int
		var b int64
		var n int
		if err := rows.Scan(&owner, &k, &nn, &b, &n); err != nil {
			return nil, nil, nil, nil, err
		}
		var key identity.Key
		copy(key[:], owner)
		objects[key] += n
		if old != nil && k == old.K && nn == old.N {
			oldBytes[key] += b
			oldObjects[key] += n
			if !counting {
				continue
			}
		}
		bytes[key] += b
	}
	return bytes, oldBytes, objects, oldObjects, rows.Err()
}
