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
	} {
		if _, err := db.Exec(q); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("localdb: %w", err)
		}
	}
	return d, nil
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

// RootVersion is the last version this device confirmed.
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
}

// LookupScan returns the row of a path, nil when unknown.
func (d *DB) LookupScan(path string) (*ScanRow, error) {
	var r ScanRow
	var h []byte
	var chunks sql.NullString
	var capStr sql.NullString
	var seen, hashed int64
	err := d.db.QueryRow(`SELECT path, key, hash, size, cap, chunks, last_seen, last_hashed FROM scan WHERE path = ?`, path).
		Scan(&r.Path, &r.Key, &h, &r.Size, &capStr, &chunks, &seen, &hashed)
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
	return &r, nil
}

// RememberScan upserts a row.
func (d *DB) RememberScan(r ScanRow) error {
	_, err := d.db.Exec(`INSERT INTO scan (path, key, hash, size, cap, chunks, last_seen, last_hashed) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET key = excluded.key, hash = excluded.hash, size = excluded.size, cap = excluded.cap, chunks = excluded.chunks,
		last_seen = excluded.last_seen, last_hashed = excluded.last_hashed`,
		r.Path, r.Key, r.Hash[:], r.Size, r.Cap, joinChunks(r.Chunks), r.LastSeen.Unix(), r.LastHashed.Unix())
	return err
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
