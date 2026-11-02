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
	Placed        int // distinct complete share numbers known at the last put or check
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
	err := d.db.QueryRow(`SELECT cap, placed FROM objects WHERE plaintext_hash = ? AND k = ? AND nn = ? AND segment_size = ?`,
		h[:], p.K, p.N, p.SegmentSize).Scan(&capStr, &placed)
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
	_, _ = d.db.Exec(`UPDATE objects SET last_used = ? WHERE plaintext_hash = ? AND k = ? AND nn = ? AND segment_size = ?`, d.now().Unix(), h[:], p.K, p.N, p.SegmentSize)
	return &Object{PlaintextHash: h, Params: p, Cap: rc, Placed: placed}, nil
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
