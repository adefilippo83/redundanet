// Package localdb is a device's local database: a cache of what the
// device has stored and scanned, the counter behind its root version, and
// the queues the backup job keeps. It is rebuilt from the grid and the
// files when lost; nothing on the grid depends on it (RFC 0001, "The local
// database is a cache").
package localdb

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go

	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// DB is an open local database.
type DB struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the database in dir.
func Open(dir string, now func() time.Time) (*DB, error) {
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "local.sqlite")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	d := &DB{db: db, now: now}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS objects (
			plaintext_hash BLOB NOT NULL, k INTEGER NOT NULL, nn INTEGER NOT NULL, segment_size INTEGER NOT NULL,
			cap TEXT NOT NULL, size INTEGER NOT NULL, placed INTEGER NOT NULL, created INTEGER NOT NULL, last_used INTEGER NOT NULL,
			PRIMARY KEY (plaintext_hash, k, nn, segment_size))`,
		`CREATE INDEX IF NOT EXISTS objects_placed ON objects(placed)`,
		`CREATE TABLE IF NOT EXISTS scan (
			path TEXT PRIMARY KEY, key TEXT NOT NULL, hash BLOB, size INTEGER NOT NULL,
			cap TEXT, chunks TEXT, last_seen INTEGER NOT NULL, last_hashed INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS scan_hashed ON scan(last_hashed)`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('root_version', 0)`,
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('root_confirmed', 0)`,
		`CREATE TABLE IF NOT EXISTS owner (id INTEGER PRIMARY KEY CHECK (id = 1), member TEXT NOT NULL, device TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS verifications (at INTEGER PRIMARY KEY, snapshot TEXT NOT NULL, sampled INTEGER NOT NULL,
			verified INTEGER NOT NULL, compared INTEGER NOT NULL, unchanged INTEGER NOT NULL, failed TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("localdb: %w", err)
		}
	}
	// last_confirmed came after the first databases: a row from before it
	// was never confirmed, which is what zero says.
	var has int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scan') WHERE name = 'last_confirmed'`).Scan(&has); err == nil && has == 0 {
		_, err = db.Exec(`ALTER TABLE scan ADD COLUMN last_confirmed INTEGER NOT NULL DEFAULT 0`)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("localdb: %w", err)
		}
	} else if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("localdb: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS scan_confirmed ON scan(last_confirmed)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("localdb: %w", err)
	}
	return d, nil
}

// What Bind found.
const (
	BoundSame   = ""       // the identity the database already served, or its first
	BoundDevice = "device" // the same member under a new device key
	BoundMember = "member" // another member
)

// Bind ties the database to the identity that uses it, and drops what
// belonged to another one: the data directory outlives an identity that is
// replaced in place. A new device key of the same member starts its root
// counters from zero, since the servers hold no root of that key and a
// version the old key confirmed would make every reading of the roots look
// like a loss. Another member keeps nothing: the objects, the scan and the
// verifications are of data it cannot read. A database from before this
// record takes the identity it is given and keeps what it has.
func (d *DB) Bind(member, device string) (string, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var m, dev string
	switch err := tx.QueryRow(`SELECT member, device FROM owner WHERE id = 1`).Scan(&m, &dev); {
	case errors.Is(err, sql.ErrNoRows):
		m, dev = member, device
	case err != nil:
		return "", err
	}
	found := BoundSame
	var reset []string
	switch {
	case m != member:
		found = BoundMember
		reset = []string{`DELETE FROM objects`, `DELETE FROM scan`, `DELETE FROM verifications`, `UPDATE meta SET value = 0 WHERE key IN ('root_version', 'root_confirmed')`}
	case dev != device:
		found = BoundDevice
		reset = []string{`UPDATE meta SET value = 0 WHERE key IN ('root_version', 'root_confirmed')`}
	}
	for _, q := range append(reset, `INSERT INTO owner (id, member, device) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET member = excluded.member, device = excluded.device`) {
		args := []any{}
		if strings.Contains(q, "?") {
			args = []any{member, device}
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return "", err
		}
	}
	return found, tx.Commit()
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Object is what the device remembers about an object it stored or found.
type Object struct {
	PlaintextHash object.Hash
	Params        object.Params
	Cap           object.ReadCap
	Placed        int       // distinct complete share numbers known at the last put or check
	LastUsed      time.Time // the previous lookup, before this one: when the grid last confirmed it
}

// RememberObject records an object under its plaintext hash and encoding.
func (d *DB) RememberObject(o Object) error {
	now := d.now().Unix()
	_, err := d.db.Exec(`INSERT INTO objects (plaintext_hash, k, nn, segment_size, cap, size, placed, created, last_used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (plaintext_hash, k, nn, segment_size) DO UPDATE SET cap = excluded.cap, placed = excluded.placed, last_used = excluded.last_used`,
		o.PlaintextHash[:], o.Params.K, o.Params.N, o.Params.SegmentSize, o.Cap.String(), o.Cap.Size, o.Placed, now, now)
	return err
}

// LookupObject finds an object by plaintext hash and encoding; nil when
// unknown. A row under another encoding is a miss, as the RFC says.
func (d *DB) LookupObject(h object.Hash, p object.Params) (*Object, error) {
	var capStr string
	var placed int
	var lastUsed int64
	err := d.db.QueryRow(`SELECT cap, placed, last_used FROM objects WHERE plaintext_hash = ? AND k = ? AND nn = ? AND segment_size = ?`,
		h[:], p.K, p.N, p.SegmentSize).Scan(&capStr, &placed, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	return &Object{PlaintextHash: h, Params: p, Cap: rc, Placed: placed, LastUsed: time.Unix(lastUsed, 0)}, nil
}

// LookupByCap finds an object by its capability, for the encoding it was
// written under; nil when unknown.
func (d *DB) LookupByCap(capStr string) (*Object, error) {
	var h []byte
	var k, nn int
	var segment uint32
	var placed int
	var lastUsed int64
	err := d.db.QueryRow(`SELECT plaintext_hash, k, nn, segment_size, placed, last_used FROM objects WHERE cap = ?`, capStr).Scan(&h, &k, &nn, &segment, &placed, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	o := &Object{Params: object.Params{K: uint8(k), N: uint8(nn), SegmentSize: segment, Cipher: object.CipherAESGCM}, Cap: rc, Placed: placed, LastUsed: time.Unix(lastUsed, 0)}
	copy(o.PlaintextHash[:], h)
	return o, nil
}

// RewriteCaps replaces capabilities in the scan rows: what rebalance
// does once it has re-encoded an object, so the next backup reuses the
// new object and never the old one.
func (d *DB) RewriteCaps(replace map[string]string) error {
	if len(replace) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for oldCap, newCap := range replace {
		if _, err := tx.Exec(`UPDATE scan SET cap = ? WHERE cap = ?`, newCap, oldCap); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`SELECT path, chunks FROM scan WHERE chunks != '' AND chunks IS NOT NULL`)
	if err != nil {
		return err
	}
	type upd struct{ path, chunks string }
	var updates []upd
	for rows.Next() {
		var path, chunks string
		if err := rows.Scan(&path, &chunks); err != nil {
			rows.Close()
			return err
		}
		parts := splitChunks(chunks)
		changed := false
		for i, c := range parts {
			if n, ok := replace[c]; ok {
				parts[i], changed = n, true
			}
		}
		if changed {
			updates = append(updates, upd{path, joinChunks(parts)})
		}
	}
	rows.Close()
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE scan SET chunks = ? WHERE path = ?`, u.chunks, u.path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Forget drops an object the grid no longer confirms.
func (d *DB) Forget(h object.Hash, p object.Params) error {
	_, err := d.db.Exec(`DELETE FROM objects WHERE plaintext_hash = ? AND k = ? AND nn = ? AND segment_size = ?`, h[:], p.K, p.N, p.SegmentSize)
	return err
}

// UnderPlaced lists objects whose last known placement is below n.
func (d *DB) UnderPlaced() ([]Object, error) {
	rows, err := d.db.Query(`SELECT plaintext_hash, k, nn, segment_size, cap, placed FROM objects WHERE placed < nn ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		var h []byte
		var capStr string
		var k, nn int
		if err := rows.Scan(&h, &k, &nn, &o.Params.SegmentSize, &capStr, &o.Placed); err != nil {
			return nil, err
		}
		copy(o.PlaintextHash[:], h)
		o.Params.K, o.Params.N, o.Params.Cipher = uint8(k), uint8(nn), object.CipherAESGCM
		if o.Cap, err = object.ParseReadCap(capStr); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// NextRootVersion advances and returns the device's root version counter.
func (d *DB) NextRootVersion() (uint64, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`UPDATE meta SET value = value + 1 WHERE key = 'root_version'`); err != nil {
		return 0, err
	}
	var v uint64
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'root_version'`).Scan(&v); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// ConfirmRootVersion records a root version that a majority of the
// storage servers took. The counter above is spent on every attempt; this
// is the last one that is certain to be out there, which a later reading
// of the roots must not fall behind.
func (d *DB) ConfirmRootVersion(v uint64) error {
	_, err := d.db.Exec(`UPDATE meta SET value = MAX(value, ?) WHERE key = 'root_confirmed'`, v)
	return err
}

// RootConfirmed is the last version ConfirmRootVersion recorded; 0 on a
// new or replaced database.
func (d *DB) RootConfirmed() (uint64, error) {
	var v uint64
	err := d.db.QueryRow(`SELECT value FROM meta WHERE key = 'root_confirmed'`).Scan(&v)
	return v, err
}

// RootVersion is the device's root version counter: the last version
// tried, confirmed or not.
func (d *DB) RootVersion() (uint64, error) {
	var v uint64
	err := d.db.QueryRow(`SELECT value FROM meta WHERE key = 'root_version'`).Scan(&v)
	return v, err
}

// RecoverRootVersion raises the counter after a lost database: the highest
// version the servers returned, so the next root is above it.
func (d *DB) RecoverRootVersion(seen uint64) error {
	_, err := d.db.Exec(`UPDATE meta SET value = MAX(value, ?) WHERE key = 'root_version'`, seen)
	return err
}

// ScanRow is what the scan remembers about one path: the change key, the
// plaintext hash, and the capabilities it was stored under.
type ScanRow struct {
	Path       string
	Key        string // the change-detection key
	Hash       object.Hash
	Size       int64
	Cap        string   // one object, or "" when chunked or inline
	Chunks     []string // chunk capabilities for a large file
	LastSeen   time.Time
	LastHashed time.Time
	// LastConfirmed is when the servers last answered for the objects, at
	// the upload or at a probe; zero when never.
	LastConfirmed time.Time
}

// LookupScan returns the row of a path, nil when unknown.
func (d *DB) LookupScan(path string) (*ScanRow, error) {
	var r ScanRow
	var h []byte
	var chunks sql.NullString
	var capStr sql.NullString
	var seen, hashed, confirmed int64
	err := d.db.QueryRow(`SELECT path, key, hash, size, cap, chunks, last_seen, last_hashed, last_confirmed FROM scan WHERE path = ?`, path).
		Scan(&r.Path, &r.Key, &h, &r.Size, &capStr, &chunks, &seen, &hashed, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	copy(r.Hash[:], h)
	r.Cap = capStr.String
	if chunks.Valid && chunks.String != "" {
		r.Chunks = splitChunks(chunks.String)
	}
	r.LastSeen, r.LastHashed = time.Unix(seen, 0), time.Unix(hashed, 0)
	if confirmed > 0 {
		r.LastConfirmed = time.Unix(confirmed, 0)
	}
	return &r, nil
}

// RememberScan upserts a row.
func (d *DB) RememberScan(r ScanRow) error {
	var confirmed int64
	if !r.LastConfirmed.IsZero() {
		confirmed = r.LastConfirmed.Unix()
	}
	_, err := d.db.Exec(`INSERT INTO scan (path, key, hash, size, cap, chunks, last_seen, last_hashed, last_confirmed) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET key = excluded.key, hash = excluded.hash, size = excluded.size, cap = excluded.cap, chunks = excluded.chunks,
		last_seen = excluded.last_seen, last_hashed = excluded.last_hashed, last_confirmed = excluded.last_confirmed`,
		r.Path, r.Key, r.Hash[:], r.Size, r.Cap, joinChunks(r.Chunks), r.LastSeen.Unix(), r.LastHashed.Unix(), confirmed)
	return err
}

// OldestConfirmed lists the paths whose objects the servers have not
// answered for since before the given time, oldest first: one part in
// cycle of the known paths per call, at least one and at most limit, so
// every file is confirmed about once a cycle of runs and no run asks for
// all of them.
func (d *DB) OldestConfirmed(before time.Time, cycle, limit int) (map[string]bool, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM scan`).Scan(&n); err != nil {
		return nil, err
	}
	take := max(1, (n+cycle-1)/cycle)
	if take > limit {
		take = limit
	}
	rows, err := d.db.Query(`SELECT path FROM scan WHERE last_confirmed < ? AND (cap <> '' OR chunks <> '') ORDER BY last_confirmed ASC, path ASC LIMIT ?`, before.Unix(), take)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// OldestHashed lists the paths whose hash is oldest, for the rolling
// re-hash: about 1% of the known paths per run, at least one and at most
// limit, so a file that defeats every timestamp is still caught.
func (d *DB) OldestHashed(limit int) (map[string]bool, error) {
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM scan`).Scan(&n); err != nil {
		return nil, err
	}
	take := max(1, n/100)
	if take > limit {
		take = limit
	}
	rows, err := d.db.Query(`SELECT path FROM scan ORDER BY last_hashed ASC LIMIT ?`, take)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

func joinChunks(c []string) string {
	out := ""
	for i, s := range c {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

func splitChunks(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

// Verification is one restore test as the device keeps it: the record
// behind the weekly tests the G2 gate counts.
type Verification struct {
	At        time.Time `json:"at"`
	Snapshot  string    `json:"snapshot"`
	Sampled   int       `json:"sampled"`
	Verified  int       `json:"verified"`
	Compared  int       `json:"compared"`
	Unchanged int       `json:"unchanged"`
	Failed    []string  `json:"failed,omitempty"`
}

// Passed says the test read every sampled file back and none differed.
func (v Verification) Passed() bool {
	return v.Sampled > 0 && v.Verified == v.Sampled && len(v.Failed) == 0
}

// RememberVerification keeps a restore test; one per second at most.
func (d *DB) RememberVerification(v Verification) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO verifications (at, snapshot, sampled, verified, compared, unchanged, failed) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.At.Unix(), v.Snapshot, v.Sampled, v.Verified, v.Compared, v.Unchanged, strings.Join(v.Failed, "\n"))
	return err
}

// Verifications lists the newest restore tests first, up to limit.
func (d *DB) Verifications(limit int) ([]Verification, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.Query(`SELECT at, snapshot, sampled, verified, compared, unchanged, failed FROM verifications ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		var v Verification
		var at int64
		var failed string
		if err := rows.Scan(&at, &v.Snapshot, &v.Sampled, &v.Verified, &v.Compared, &v.Unchanged, &failed); err != nil {
			return nil, err
		}
		v.At = time.Unix(at, 0)
		if failed != "" {
			v.Failed = strings.Split(failed, "\n")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
