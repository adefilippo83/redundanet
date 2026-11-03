// Package store is a storage node's share store: share files in the format
// of package object, an SQLite index that makes the census and the usage
// per owner queries rather than directory walks, the partial and complete
// states with resume, the free-space floor, tombstones, root records, and
// the marker that ties the store to its disk. It applies no membership
// policy: that is the protocol layer's job. It is the node half of RFC
// 0001's "Ownership, accounting and quotas" and "Share on disk".
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the index, pure Go

	"github.com/adefilippo83/redundanet/agent/internal/diskfree"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
)

// Errors a caller tells apart.
var (
	ErrUnavailable     = errors.New("store: storage unavailable: the disk or its marker is missing")
	ErrNoSpace         = errors.New("store: free space is below the floor")
	ErrTombstoned      = errors.New("store: the object is tombstoned on this server")
	ErrDescriptor      = errors.New("store: a share of this object with another descriptor is already held")
	ErrAlreadyComplete = errors.New("store: this share is already complete")
	ErrNotFound        = errors.New("store: no such share")
	ErrPartial         = errors.New("store: the share is not complete")
	ErrOutOfOrder      = errors.New("store: blocks must arrive in segment order")
	ErrRoot            = errors.New("store: the blocks do not match the signed share root")
	ErrLowerVersion    = errors.New("store: a root with this version or a higher one is already held")
	ErrRecent          = errors.New("store: the object was touched by its owner in the last 7 days")
	ErrStale           = errors.New("store: the delete token was issued before the object was last touched")
	ErrNotOwner        = errors.New("store: the object belongs to another member")
)

// Windows of RFC 0001's "Deletion": an object touched by its owner within
// the immunity refuses a delete token; a tombstone keeps the shares for
// the window and the record for a while after the purge, so a late repair
// write is still refused.
const (
	Immunity        = 7 * 24 * time.Hour
	TombstoneWindow = 30 * 24 * time.Hour
	tombstoneKeep   = 90 * 24 * time.Hour
)

// Usage rounds a share's bytes on disk up to 4 KiB, so an object costs at
// least that much per share and its inode and index row are paid for.
const usageUnit = 4096

// Marker files: one on the disk, one record beside the node's identity.
const (
	markerName = ".redundanet3-id"
	dbName     = "index.sqlite"
)

// Config opens a store.
type Config struct {
	Root       string // the share store, for example /mnt/storage/redundanet3
	RecordPath string // where the node keeps the marker's value, outside the disk
	Floor      int64  // bytes of free space below which writes are refused
	Now        func() time.Time
	Log        *slog.Logger
}

// Store is an open share store.
type Store struct {
	cfg Config
	log *slog.Logger
	db  *sql.DB
	now func() time.Time
}

// Open establishes or verifies the marker and opens the index.
func Open(cfg Config) (*Store, error) {
	if cfg.Root == "" || cfg.RecordPath == "" {
		return nil, errors.New("store: root and record path are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Floor <= 0 {
		cfg.Floor = 10 << 30
	}
	s := &Store{cfg: cfg, log: cfg.Log.With("component", "store"), now: cfg.Now}
	if err := s.establish(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(cfg.Root, dbName)+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s.db = db
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// Root is the store's directory.
func (s *Store) Root() string { return s.cfg.Root }

// establish writes the marker and the record the first time; after that
// both must exist and agree. The store never creates its root: a missing
// mount point must not be filled on the boot disk.
func (s *Store) establish() error {
	if st, err := os.Stat(s.cfg.Root); err != nil || !st.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnavailable, s.cfg.Root)
	}
	marker := filepath.Join(s.cfg.Root, markerName)
	rec, recErr := os.ReadFile(s.cfg.RecordPath)
	mk, mkErr := os.ReadFile(marker)
	switch {
	case recErr == nil && mkErr == nil:
		if strings.TrimSpace(string(rec)) != strings.TrimSpace(string(mk)) {
			return fmt.Errorf("%w: the marker on %s belongs to another node", ErrUnavailable, s.cfg.Root)
		}
		return nil
	case recErr != nil && mkErr != nil && errors.Is(recErr, os.ErrNotExist) && errors.Is(mkErr, os.ErrNotExist):
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return err
		}
		v := hex.EncodeToString(id[:]) + "\n"
		if err := os.WriteFile(marker, []byte(v), 0o644); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.cfg.RecordPath), 0o700); err != nil {
			return err
		}
		return os.WriteFile(s.cfg.RecordPath, []byte(v), 0o600)
	case recErr == nil:
		return fmt.Errorf("%w: the marker is missing from %s", ErrUnavailable, s.cfg.Root)
	default:
		return fmt.Errorf("%w: this node has no record of %s", ErrUnavailable, s.cfg.Root)
	}
}

// Available re-checks the marker, which is the RFC's rule before each batch
// of storage operations and at every census tick.
func (s *Store) Available() error {
	rec, err := os.ReadFile(s.cfg.RecordPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	mk, err := os.ReadFile(filepath.Join(s.cfg.Root, markerName))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if strings.TrimSpace(string(rec)) != strings.TrimSpace(string(mk)) {
		return fmt.Errorf("%w: the marker on %s belongs to another node", ErrUnavailable, s.cfg.Root)
	}
	return nil
}

// Free is the disk's free and total bytes.
func (s *Store) Free() (free, total uint64, err error) { return diskfree.Free(s.cfg.Root) }

// Floor is the free-space floor.
func (s *Store) Floor() int64 { return s.cfg.Floor }

func (s *Store) migrate() error {
	stmts := []string{
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
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("store: index: %w", err)
		}
	}
	return nil
}

// bump advances the census generation inside tx and returns the new value.
func bump(tx *sql.Tx) (uint64, error) {
	if _, err := tx.Exec(`UPDATE meta SET value = value + 1 WHERE key = 'gen'`); err != nil {
		return 0, err
	}
	var g uint64
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = 'gen'`).Scan(&g)
	return g, err
}

// Generation is the census generation: it advances whenever a share
// becomes complete or goes away.
func (s *Store) Generation() (uint64, error) {
	var g uint64
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'gen'`).Scan(&g)
	return g, err
}

func (s *Store) sharePath(si object.StorageIndex, n uint8) string {
	name := si.String()
	return filepath.Join(s.cfg.Root, "shares", name[:2], name, fmt.Sprintf("%d", n))
}

func roundUsage(n int64) int64 { return (n + usageUnit - 1) / usageUnit * usageUnit }

// Presence is what a server knows about an object.
type Presence struct {
	DescriptorHash object.Hash
	Owner          identity.Key
	Complete       []uint8
	Partial        map[uint8]uint32 // share number -> segments received
	LastTouched    time.Time        // the newest touch of any share, on this server's clock
	Tombstoned     bool
	TombstoneOwner identity.Key // who issued the tombstone, when Tombstoned
	Purged         bool         // the tombstone's window has run and the shares are gone
}

// Has reports the shares held for a storage index; nil when nothing is
// held and no tombstone exists.
func (s *Store) Has(si object.StorageIndex) (*Presence, error) {
	rows, err := s.db.Query(`SELECT n, owner, dhash, state, received, last_touched FROM shares WHERE si = ? ORDER BY n`, si[:])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var p *Presence
	for rows.Next() {
		var n int
		var owner, dhash []byte
		var state string
		var received, touched int64
		if err := rows.Scan(&n, &owner, &dhash, &state, &received, &touched); err != nil {
			return nil, err
		}
		if p == nil {
			p = &Presence{Partial: map[uint8]uint32{}}
			copy(p.DescriptorHash[:], dhash)
			copy(p.Owner[:], owner)
		}
		if t := time.Unix(touched, 0); t.After(p.LastTouched) {
			p.LastTouched = t
		}
		if state == "complete" {
			p.Complete = append(p.Complete, uint8(n))
		} else {
			p.Partial[uint8(n)] = uint32(received)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var purged sql.NullInt64
	var owner, dhash []byte
	err = s.db.QueryRow(`SELECT owner, dhash, purged FROM tombstones WHERE si = ?`, si[:]).Scan(&owner, &dhash, &purged)
	switch {
	case err == nil:
		if p == nil {
			p = &Presence{Partial: map[uint8]uint32{}}
			copy(p.DescriptorHash[:], dhash)
			copy(p.Owner[:], owner)
		}
		p.Tombstoned = true
		p.Purged = purged.Valid
		copy(p.TombstoneOwner[:], owner)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, err
	}
	return p, nil
}

// Touch records that a device of the owner referred to the object now; the
// 7-day immunity from deletion runs from this clock.
func (s *Store) Touch(si object.StorageIndex) error {
	_, err := s.db.Exec(`UPDATE shares SET last_touched = ? WHERE si = ?`, s.now().Unix(), si[:])
	return err
}

// Usage is the bytes accounted to an owner on this server, partial shares
// included so they cannot slip past a quota.
func (s *Store) Usage(owner identity.Key) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT SUM(size) FROM shares WHERE owner = ?`, owner[:]).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// UsageExcluding is Usage without the shares under one encoding: during
// a transition servers account the old encoding at zero until the
// deadline, so a member near budget can convert first.
func (s *Store) UsageExcluding(owner identity.Key, k, n int) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(`SELECT SUM(size) FROM shares WHERE owner = ? AND NOT (k = ? AND nn = ?)`, owner[:], k, n).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// Totals is what the store holds: complete shares and their bytes.
func (s *Store) Totals() (shares int64, bytes int64, err error) {
	var b sql.NullInt64
	err = s.db.QueryRow(`SELECT COUNT(*), SUM(size) FROM shares WHERE state = 'complete'`).Scan(&shares, &b)
	return shares, b.Int64, err
}

// VerifiedSince is the oldest integrity check among the complete shares,
// so every share was read back since then; zero while any share has not
// been checked yet, and when the store is empty.
func (s *Store) VerifiedSince() (time.Time, error) {
	var oldest, unchecked sql.NullInt64
	if err := s.db.QueryRow(`SELECT MIN(verified), SUM(verified IS NULL) FROM shares WHERE state = 'complete'`).Scan(&oldest, &unchecked); err != nil {
		return time.Time{}, err
	}
	if !oldest.Valid || unchecked.Int64 > 0 {
		return time.Time{}, nil
	}
	return time.Unix(oldest.Int64, 0), nil
}

// Remove deletes a share and records the removal for the census.
func (s *Store) Remove(si object.StorageIndex, n uint8, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.Exec(`DELETE FROM shares WHERE si = ? AND n = ?`, si[:], n)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return ErrNotFound
	}
	g, err := bump(tx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO removed (si, n, gen) VALUES (?, ?, ?)`, si[:], n, g); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = os.Remove(s.sharePath(si, n))
	s.log.Info("share removed", "si", si.String()[:8], "n", n, "reason", reason)
	return nil
}

// ExpirePartials deletes partial shares with no block for the given age.
func (s *Store) ExpirePartials(age time.Duration) (int, error) {
	cutoff := s.now().Add(-age).Unix()
	rows, err := s.db.Query(`SELECT si, n FROM shares WHERE state = 'partial' AND last_touched < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	type key struct {
		si object.StorageIndex
		n  uint8
	}
	var stale []key
	for rows.Next() {
		var si []byte
		var n int
		if err := rows.Scan(&si, &n); err != nil {
			rows.Close()
			return 0, err
		}
		var k key
		copy(k.si[:], si)
		k.n = uint8(n)
		stale = append(stale, k)
	}
	rows.Close()
	for _, k := range stale {
		if err := s.Remove(k.si, k.n, "partial for too long"); err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	return len(stale), nil
}

// Tombstone marks an object deleted by its owner: served to nobody but the
// owner's devices, purged after the window, refused to repair.
func (s *Store) Tombstone(si object.StorageIndex, owner identity.Key, dhash object.Hash, issued time.Time) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO tombstones (si, owner, dhash, issued, created, purged) VALUES (?, ?, ?, ?, ?, NULL)`,
		si[:], owner[:], dhash[:], issued.Unix(), s.now().Unix())
	return err
}

// ClearTombstone is an upload from a device of the owner, or the owner's
// change of mind within the window.
func (s *Store) ClearTombstone(si object.StorageIndex) error {
	_, err := s.db.Exec(`DELETE FROM tombstones WHERE si = ?`, si[:])
	return err
}

// ApplyDelete is a delete token's effect, the first phase of deletion:
// the object becomes tombstoned, served to the owner's devices only. The
// token is refused when the object belongs to another member or sits
// under another descriptor, when the owner touched it within the
// immunity, and when it was issued before the last touch, so a captured
// token cannot delete a copy uploaded again later. Nothing held is
// nothing to do.
func (s *Store) ApplyDelete(si object.StorageIndex, owner identity.Key, dhash object.Hash, issued time.Time) (*Presence, error) {
	p, err := s.Has(si)
	if err != nil || p == nil {
		return nil, err
	}
	if p.Owner != owner {
		return p, ErrNotOwner
	}
	if p.DescriptorHash != dhash {
		return p, ErrDescriptor
	}
	if p.Tombstoned {
		return p, nil
	}
	now := s.now()
	if now.Sub(p.LastTouched) < Immunity {
		return p, ErrRecent
	}
	if issued.Before(p.LastTouched) {
		return p, ErrStale
	}
	if err := s.Tombstone(si, owner, dhash, issued); err != nil {
		return p, err
	}
	s.log.Info("object tombstoned", "si", si.String()[:8], "owner", owner.Short())
	p.Tombstoned, p.TombstoneOwner = true, owner
	return p, nil
}

// Undelete is the owner's change of mind within the window: the tombstone
// goes and the shares serve again. After the purge there is nothing to
// bring back; a new upload does that.
func (s *Store) Undelete(si object.StorageIndex, owner identity.Key) error {
	p, err := s.Has(si)
	if err != nil {
		return err
	}
	if p == nil || !p.Tombstoned {
		return ErrNotFound
	}
	if p.TombstoneOwner != owner {
		return ErrNotOwner
	}
	if p.Purged {
		return fmt.Errorf("%w: the tombstone window has run and the shares are gone", ErrNotFound)
	}
	return s.ClearTombstone(si)
}

// PurgeTombstones is the second phase: once a tombstone is older than the
// window, the shares go and the record stays, so a repair write for the
// object is still refused. Records purged long ago are dropped, which
// bounds the table. The removals reach the census like any other.
func (s *Store) PurgeTombstones() (purged int, err error) {
	now := s.now()
	rows, err := s.db.Query(`SELECT si FROM tombstones WHERE purged IS NULL AND created <= ?`, now.Add(-TombstoneWindow).Unix())
	if err != nil {
		return 0, err
	}
	var due []object.StorageIndex
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return 0, err
		}
		var si object.StorageIndex
		copy(si[:], b)
		due = append(due, si)
	}
	rows.Close()
	for _, si := range due {
		p, err := s.Has(si)
		if err != nil {
			return purged, err
		}
		for n := range p.Partial {
			if err := s.Remove(si, n, "tombstone window elapsed"); err != nil && !errors.Is(err, ErrNotFound) {
				return purged, err
			}
		}
		for _, n := range p.Complete {
			if err := s.Remove(si, n, "tombstone window elapsed"); err != nil && !errors.Is(err, ErrNotFound) {
				return purged, err
			}
		}
		if _, err := s.db.Exec(`UPDATE tombstones SET purged = ? WHERE si = ?`, now.Unix(), si[:]); err != nil {
			return purged, err
		}
		purged++
	}
	_, err = s.db.Exec(`DELETE FROM tombstones WHERE purged IS NOT NULL AND purged <= ?`, now.Add(-tombstoneKeep).Unix())
	return purged, err
}

// Listed is one object of an owner as the owner's cleanup job lists it.
type Listed struct {
	StorageIndex   object.StorageIndex
	DescriptorHash object.Hash
	LastTouched    time.Time
	Tombstoned     bool
}

// ListOwner pages through the objects an owner has shares of here, in
// storage index order: the server's half of the reachable-set
// computation, which runs against the grid and never against a local
// cache. limit bounds a page; more says another follows.
func (s *Store) ListOwner(owner identity.Key, after object.StorageIndex, limit int) (list []Listed, more bool, err error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(`SELECT s.si, s.dhash, MAX(s.last_touched), t.si IS NOT NULL FROM shares s LEFT JOIN tombstones t ON t.si = s.si
		WHERE s.owner = ? AND s.si > ? GROUP BY s.si ORDER BY s.si LIMIT ?`, owner[:], after[:], limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Listed
		var si, dhash []byte
		var touched int64
		if err := rows.Scan(&si, &dhash, &touched, &l.Tombstoned); err != nil {
			return nil, false, err
		}
		copy(l.StorageIndex[:], si)
		copy(l.DescriptorHash[:], dhash)
		l.LastTouched = time.Unix(touched, 0)
		list = append(list, l)
	}
	if len(list) > limit {
		return list[:limit], true, rows.Err()
	}
	return list, false, rows.Err()
}

// ApplyMembership is membership-driven deletion, which never acts on
// absence alone: a member found leaving or unlisted is noted with the
// time, and their shares, roots and tombstones go only once the grace has
// run on this server's clock and the manifest still says so. A member who
// reappears clears the note. active says whether a member is listed and
// active in the current manifest.
func (s *Store) ApplyMembership(active func(member identity.Key) bool, grace time.Duration) (purged []identity.Key, err error) {
	now := s.now()
	members := map[identity.Key]bool{}
	for _, q := range []string{`SELECT DISTINCT owner FROM shares`, `SELECT DISTINCT member FROM roots`, `SELECT DISTINCT owner FROM tombstones`} {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				rows.Close()
				return nil, err
			}
			var k identity.Key
			copy(k[:], b)
			members[k] = true
		}
		rows.Close()
	}
	for m := range members {
		if active(m) {
			if _, err := s.db.Exec(`DELETE FROM missing WHERE member = ?`, m[:]); err != nil {
				return purged, err
			}
			continue
		}
		var since sql.NullInt64
		if err := s.db.QueryRow(`SELECT since FROM missing WHERE member = ?`, m[:]).Scan(&since); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return purged, err
		}
		if !since.Valid {
			if _, err := s.db.Exec(`INSERT INTO missing (member, since) VALUES (?, ?)`, m[:], now.Unix()); err != nil {
				return purged, err
			}
			s.log.Info("member leaving or unlisted; their data goes after the grace", "member", m.Short(), "grace", grace)
			continue
		}
		if now.Sub(time.Unix(since.Int64, 0)) < grace {
			continue
		}
		if err := s.purgeMember(m); err != nil {
			return purged, err
		}
		purged = append(purged, m)
	}
	return purged, nil
}

// purgeMember removes everything a member has here.
func (s *Store) purgeMember(m identity.Key) error {
	rows, err := s.db.Query(`SELECT si, n FROM shares WHERE owner = ?`, m[:])
	if err != nil {
		return err
	}
	type key struct {
		si object.StorageIndex
		n  uint8
	}
	var all []key
	for rows.Next() {
		var si []byte
		var n int
		if err := rows.Scan(&si, &n); err != nil {
			rows.Close()
			return err
		}
		var k key
		copy(k.si[:], si)
		k.n = uint8(n)
		all = append(all, k)
	}
	rows.Close()
	for _, k := range all {
		if err := s.Remove(k.si, k.n, "member gone after the grace period"); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM roots WHERE member = ?`, `DELETE FROM tombstones WHERE owner = ?`, `DELETE FROM missing WHERE member = ?`} {
		if _, err := s.db.Exec(q, m[:]); err != nil {
			return err
		}
	}
	s.log.Info("member's data purged after the grace period", "member", m.Short(), "shares", len(all))
	return nil
}

// ---- roots ----

// RootRecord is a stored root envelope; the store does not parse it.
type RootRecord struct {
	Member   identity.Key
	Device   identity.Key
	Version  uint64
	Envelope []byte
}

// keepRoots is how many versions below the highest a server keeps.
const keepRoots = 10

// PutRoot stores a device's root, refusing a version at or below the
// highest held, and prunes versions beyond the kept window.
func (s *Store) PutRoot(r RootRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var highest sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(version) FROM roots WHERE member = ? AND device = ?`, r.Member[:], r.Device[:]).Scan(&highest); err != nil {
		return err
	}
	if highest.Valid && uint64(highest.Int64) >= r.Version {
		return ErrLowerVersion
	}
	if _, err := tx.Exec(`INSERT INTO roots (member, device, version, envelope, received) VALUES (?, ?, ?, ?, ?)`,
		r.Member[:], r.Device[:], r.Version, r.Envelope, s.now().Unix()); err != nil {
		return err
	}
	if r.Version > keepRoots {
		if _, err := tx.Exec(`DELETE FROM roots WHERE member = ? AND device = ? AND version < ?`, r.Member[:], r.Device[:], r.Version-keepRoots); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Roots returns the highest root of every device of a member.
func (s *Store) Roots(member identity.Key) ([]RootRecord, error) {
	rows, err := s.db.Query(`SELECT r.device, r.version, r.envelope FROM roots r
		JOIN (SELECT device, MAX(version) AS v FROM roots WHERE member = ? GROUP BY device) m
		ON r.device = m.device AND r.version = m.v WHERE r.member = ? ORDER BY r.device`, member[:], member[:])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RootRecord
	for rows.Next() {
		r := RootRecord{Member: member}
		var dev []byte
		if err := rows.Scan(&dev, &r.Version, &r.Envelope); err != nil {
			return nil, err
		}
		copy(r.Device[:], dev)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- census ----

// CensusEntry is one complete share as the hub's collector records it.
type CensusEntry struct {
	StorageIndex   object.StorageIndex `cbor:"si" json:"si"`
	Number         uint8               `cbor:"n" json:"n"`
	Owner          identity.Key        `cbor:"owner" json:"owner"`
	Size           int64               `cbor:"size" json:"size"`
	K              uint8               `cbor:"k" json:"k"`
	N              uint8               `cbor:"nn" json:"nn"`
	DescriptorHash object.Hash         `cbor:"dhash" json:"dhash"`
}

// CensusKey names a share that went away.
type CensusKey struct {
	StorageIndex object.StorageIndex `cbor:"si" json:"si"`
	Number       uint8               `cbor:"n" json:"n"`
}

// TombstoneEntry is a tombstone the census reports.
type TombstoneEntry struct {
	StorageIndex   object.StorageIndex `cbor:"si" json:"si"`
	Owner          identity.Key        `cbor:"owner" json:"owner"`
	DescriptorHash object.Hash         `cbor:"dhash" json:"dhash"`
	Purged         bool                `cbor:"purged" json:"purged"`
}

// CensusDelta is what changed since a generation.
type CensusDelta struct {
	Since      uint64           `cbor:"since" json:"since"`
	Generation uint64           `cbor:"generation" json:"generation"`
	Added      []CensusEntry    `cbor:"added" json:"added"`
	Removed    []CensusKey      `cbor:"removed" json:"removed"`
	Tombstones []TombstoneEntry `cbor:"tombstones" json:"tombstones"`
}

// Census lists the complete shares that appeared and the shares that went
// away since a generation; since 0 is the whole store. Tombstones are
// always listed whole.
func (s *Store) Census(since uint64) (*CensusDelta, error) {
	g, err := s.Generation()
	if err != nil {
		return nil, err
	}
	d := &CensusDelta{Since: since, Generation: g, Added: []CensusEntry{}, Removed: []CensusKey{}, Tombstones: []TombstoneEntry{}}
	rows, err := s.db.Query(`SELECT si, n, owner, size, k, nn, dhash FROM shares WHERE state = 'complete' AND gen > ? ORDER BY gen`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e CensusEntry
		var si, owner, dhash []byte
		var n, k, nn int
		if err := rows.Scan(&si, &n, &owner, &e.Size, &k, &nn, &dhash); err != nil {
			rows.Close()
			return nil, err
		}
		copy(e.StorageIndex[:], si)
		copy(e.Owner[:], owner)
		copy(e.DescriptorHash[:], dhash)
		e.Number, e.K, e.N = uint8(n), uint8(k), uint8(nn)
		d.Added = append(d.Added, e)
	}
	rows.Close()
	if since > 0 {
		rows, err = s.db.Query(`SELECT si, n FROM removed WHERE gen > ? ORDER BY gen`, since)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k CensusKey
			var si []byte
			var n int
			if err := rows.Scan(&si, &n); err != nil {
				rows.Close()
				return nil, err
			}
			copy(k.StorageIndex[:], si)
			k.Number = uint8(n)
			d.Removed = append(d.Removed, k)
		}
		rows.Close()
	}
	rows, err = s.db.Query(`SELECT si, owner, dhash, purged IS NOT NULL FROM tombstones`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t TombstoneEntry
		var si, owner, dhash []byte
		if err := rows.Scan(&si, &owner, &dhash, &t.Purged); err != nil {
			return nil, err
		}
		copy(t.StorageIndex[:], si)
		copy(t.Owner[:], owner)
		copy(t.DescriptorHash[:], dhash)
		d.Tombstones = append(d.Tombstones, t)
	}
	return d, rows.Err()
}

// Scrub re-reads up to limit complete shares, oldest verification first,
// checks every block against the signed hashes, removes a share that
// fails and reports it. The caller paces it: a full pass per month under
// 1% CPU is the RFC's budget.
func (s *Store) Scrub(ctx context.Context, limit int) (checked, failed int, err error) {
	rows, err := s.db.Query(`SELECT si, n FROM shares WHERE state = 'complete' ORDER BY verified ASC NULLS FIRST, created ASC LIMIT ?`, limit)
	if err != nil {
		return 0, 0, err
	}
	type key struct {
		si object.StorageIndex
		n  uint8
	}
	var todo []key
	for rows.Next() {
		var si []byte
		var n int
		if err := rows.Scan(&si, &n); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var k key
		copy(k.si[:], si)
		k.n = uint8(n)
		todo = append(todo, k)
	}
	rows.Close()
	for _, k := range todo {
		if err := ctx.Err(); err != nil {
			return checked, failed, err
		}
		sh, err := s.OpenShare(k.si, k.n)
		if err != nil {
			failed++
			_ = s.Remove(k.si, k.n, "unreadable: "+err.Error())
			continue
		}
		verr := sh.Verify()
		sh.Close()
		checked++
		if verr != nil {
			failed++
			_ = s.Remove(k.si, k.n, "integrity: "+verr.Error())
			continue
		}
		if _, err := s.db.Exec(`UPDATE shares SET verified = ? WHERE si = ? AND n = ?`, s.now().Unix(), k.si[:], k.n); err != nil {
			return checked, failed, err
		}
	}
	return checked, failed, nil
}
