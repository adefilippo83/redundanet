// Package snapshot is what a backup stores besides the files: directory
// objects, one per folder, listing children with their capabilities, and
// the index object listing a device's snapshots. Both are immutable
// objects encoded as deterministic CBOR with a versioned entry format
// (RFC 0001, "Roots and snapshots", "File kinds").
package snapshot

import (
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// Format versions.
const (
	EntryVersion = 1
	IndexVersion = 1
	// InlineMax is the largest file kept inline in its directory entry.
	InlineMax = 256
	// ChunkSize splits files above it into fixed chunks, each its own object.
	ChunkSize = 64 << 20
)

// Kinds of entry.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
	KindSkipped = "skipped" // listed with the reason, not backed up this run
)

// Entry is one child of a directory.
type Entry struct {
	Name    []byte   `cbor:"name"`              // the bytes the filesystem returned
	UTF8    bool     `cbor:"utf8"`              // whether Name is valid UTF-8
	Kind    string   `cbor:"kind"`              //
	Size    int64    `cbor:"size,omitempty"`    // file: bytes
	Mtime   int64    `cbor:"mtime,omitempty"`   // unix nanoseconds
	Mode    uint32   `cbor:"mode,omitempty"`    // permission bits; on Windows only the read-only bit
	Cap     string   `cbor:"cap,omitempty"`     // file up to ChunkSize or a directory: one object
	Chunks  []string `cbor:"chunks,omitempty"`  // file above ChunkSize: one capability per chunk
	Inline  []byte   `cbor:"inline,omitempty"`  // file under InlineMax: the bytes themselves
	Target  string   `cbor:"target,omitempty"`  // symlink
	Reason  string   `cbor:"reason,omitempty"`  // skipped: why
	Entries int64    `cbor:"entries,omitempty"` // dir: files below, for the page
	Bytes   int64    `cbor:"bytes,omitempty"`   // dir: bytes below
}

// NameString is the name for display: the bytes when valid UTF-8, an
// escaped form otherwise.
func (e *Entry) NameString() string {
	if e.UTF8 {
		return string(e.Name)
	}
	return fmt.Sprintf("%q", e.Name)
}

// Directory is a directory object.
type Directory struct {
	Version int     `cbor:"version"`
	Entries []Entry `cbor:"entries"`
}

// NewDirectory sorts the entries by name, so an unchanged folder encodes
// to the same bytes on every run.
func NewDirectory(entries []Entry) *Directory {
	sort.Slice(entries, func(i, j int) bool { return string(entries[i].Name) < string(entries[j].Name) })
	return &Directory{Version: EntryVersion, Entries: entries}
}

// Encode is the deterministic encoding.
func (d *Directory) Encode() ([]byte, error) { return identity.Marshal(d) }

// DecodeDirectory parses a directory object.
func DecodeDirectory(b []byte) (*Directory, error) {
	var d Directory
	if err := identity.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("snapshot: directory: %w", err)
	}
	if d.Version != EntryVersion {
		return nil, fmt.Errorf("snapshot: directory entry version %d is not %d", d.Version, EntryVersion)
	}
	return &d, nil
}

// Find returns the entry of a name.
func (d *Directory) Find(name string) *Entry {
	for i := range d.Entries {
		if string(d.Entries[i].Name) == name {
			return &d.Entries[i]
		}
	}
	return nil
}

// MakeName records a name as the filesystem gave it.
func MakeName(name string) ([]byte, bool) {
	return []byte(name), utf8.ValidString(name)
}

// Snapshot is one entry of the index: a backup run.
type Snapshot struct {
	ID       string    `cbor:"id"`       // the run's start time in RFC 3339, unique per device
	Started  time.Time `cbor:"started"`  //
	Finished time.Time `cbor:"finished"` //
	Set      string    `cbor:"set"`      // the backup set's name
	Folders  []string  `cbor:"folders"`  // the folders it covered, as configured
	Top      string    `cbor:"top"`      // read capability of the top directory object
	Files    int64     `cbor:"files"`    //
	Bytes    int64     `cbor:"bytes"`    // bytes of files, before encoding
	Uploaded int64     `cbor:"uploaded"` // bytes of objects actually sent this run
	Skipped  int64     `cbor:"skipped"`  // files listed with a reason instead of stored
	Partial  bool      `cbor:"partial"`  // the run did not finish; what is listed is stored
}

// Index is the index object: a device's snapshots, newest last.
type Index struct {
	Version   int        `cbor:"version"`
	Device    string     `cbor:"device"`
	Snapshots []Snapshot `cbor:"snapshots"`
}

// Encode is the deterministic encoding.
func (x *Index) Encode() ([]byte, error) { return identity.Marshal(x) }

// DecodeIndex parses an index object.
func DecodeIndex(b []byte) (*Index, error) {
	var x Index
	if err := identity.Unmarshal(b, &x); err != nil {
		return nil, fmt.Errorf("snapshot: index: %w", err)
	}
	if x.Version != IndexVersion {
		return nil, fmt.Errorf("snapshot: index version %d is not %d", x.Version, IndexVersion)
	}
	return &x, nil
}

// Latest is the newest snapshot, nil when none.
func (x *Index) Latest() *Snapshot {
	if len(x.Snapshots) == 0 {
		return nil
	}
	return &x.Snapshots[len(x.Snapshots)-1]
}

// Find returns a snapshot by id.
func (x *Index) Find(id string) *Snapshot {
	for i := range x.Snapshots {
		if x.Snapshots[i].ID == id {
			return &x.Snapshots[i]
		}
	}
	return nil
}

// Retention is a device's policy: everything for Days, then one per day
// for Daily days, one per week for Weekly weeks, one per month for
// Monthly months.
type Retention struct {
	Days    int `yaml:"days" json:"days"`
	Daily   int `yaml:"daily" json:"daily"`
	Weekly  int `yaml:"weekly" json:"weekly"`
	Monthly int `yaml:"monthly" json:"monthly"`
}

// DefaultRetention is the RFC's default.
var DefaultRetention = Retention{Days: 7, Daily: 30, Weekly: 12, Monthly: 12}

// Prune applies the policy at time now and returns the snapshots to keep,
// in order. A partial snapshot is kept like any other until a complete
// one of the same set and day exists.
func Prune(snaps []Snapshot, r Retention, now time.Time) []Snapshot {
	keep := map[string]bool{}
	byDay := map[string]*Snapshot{}
	// Complete runs replace partial ones of the same set and day.
	for i := range snaps {
		s := &snaps[i]
		day := s.Set + "/" + s.Started.UTC().Format("2006-01-02")
		if cur := byDay[day]; cur == nil || (cur.Partial && !s.Partial) || (cur.Partial == s.Partial && s.Started.After(cur.Started)) {
			byDay[day] = s
		}
	}
	chosen := func(s *Snapshot) bool {
		day := s.Set + "/" + s.Started.UTC().Format("2006-01-02")
		return byDay[day] == s
	}
	for i := range snaps {
		s := &snaps[i]
		age := now.Sub(s.Started)
		switch {
		case age <= time.Duration(r.Days)*24*time.Hour:
			keep[s.ID] = true
		}
	}
	buckets := func(format string, limit int, unit time.Duration) {
		seen := map[string]bool{}
		for i := len(snaps) - 1; i >= 0; i-- {
			s := &snaps[i]
			if !chosen(s) || now.Sub(s.Started) > time.Duration(limit)*unit {
				continue
			}
			b := s.Set + "/" + s.Started.UTC().Format(format)
			if format == "week" {
				y, w := s.Started.UTC().ISOWeek()
				b = fmt.Sprintf("%s/%d-W%02d", s.Set, y, w)
			}
			if !seen[b] {
				seen[b] = true
				keep[s.ID] = true
			}
		}
	}
	buckets("2006-01-02", r.Daily, 24*time.Hour)
	buckets("week", r.Weekly, 7*24*time.Hour)
	buckets("2006-01", r.Monthly, 31*24*time.Hour)
	var out []Snapshot
	for _, s := range snaps {
		if keep[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

// ErrNotFound is a missing path inside a snapshot.
var ErrNotFound = errors.New("snapshot: no such path")
