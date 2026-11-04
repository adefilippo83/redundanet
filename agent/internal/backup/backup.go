// Package backup is the backup job of RFC 0001: scan the configured
// folders with change detection, deduplicate through the local database
// and the engine, upload what is new as objects, build the directory and
// index objects, publish the device's root, and keep retention. It is
// written against engine.Engine and tested with the fake.
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zeebo/blake3"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// Set is one backup set: folders and excludes.
type Set struct {
	Name     string   `yaml:"name" json:"name"`
	Folders  []string `yaml:"folders" json:"folders"`
	Excludes []string `yaml:"excludes,omitempty" json:"excludes,omitempty"` // base-name globs or relative path prefixes, beside the defaults
}

// DefaultExcludes are caches, the systems' own folders and the cloud
// providers' databases, by base name or by relative path prefix inside a
// folder: what no one wants restored, on every platform at once.
var DefaultExcludes = []string{
	// macOS
	".DS_Store", ".Trash", ".Trashes", "Library/Caches", ".Spotlight-V100", ".fseventsd", ".TemporaryItems", ".DocumentRevisions-V100",
	// Windows
	"Thumbs.db", "desktop.ini", "$RECYCLE.BIN", "System Volume Information", "pagefile.sys", "hiberfil.sys", "swapfile.sys", "~$*",
	"AppData/Local/Temp", "AppData/Local/Microsoft/Windows/INetCache",
	// Linux and everywhere
	".cache", ".dropbox.cache", "*.partial", "*.crdownload",
}

// Config runs one backup.
type Config struct {
	Engine    engine.Engine
	DB        *localdb.DB
	Member    identity.Key
	Device    identity.Key
	Set       Set
	Retention snapshot.Retention
	Log       *slog.Logger
	Now       func() time.Time
	Progress  func(Progress)
}

// Progress is what the page shows during a run.
type Progress struct {
	Phase    string `json:"phase"` // scanning, publishing, done
	Files    int64  `json:"files"`
	Bytes    int64  `json:"bytes"`
	Uploaded int64  `json:"uploaded"`
	Skipped  int64  `json:"skipped"`
	Current  string `json:"current,omitempty"`
}

// Result is a finished run.
type Result struct {
	Snapshot    snapshot.Snapshot `json:"snapshot"`
	RootVersion uint64            `json:"root_version"`
	Reasons     map[string]int64  `json:"reasons,omitempty"` // why files were skipped, counted
}

// Errors a caller tells apart.
var (
	ErrNoFolders = errors.New("backup: the set has no folders")
)

type run struct {
	cfg          Config
	log          *slog.Logger
	start        time.Time
	id           string
	excludes     []string
	rehash       map[string]bool
	prog         Progress
	reasons      map[string]int64
	lastSnapshot time.Time // when the last published snapshot started; zero before the first
}

// Run performs one backup of the set and publishes the root. When ctx
// ends mid-run, what is already stored is published as a partial
// snapshot and the rest is listed as skipped.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Engine == nil || cfg.DB == nil {
		return nil, errors.New("backup: an engine and a database are required")
	}
	if len(cfg.Set.Folders) == 0 {
		return nil, ErrNoFolders
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Retention == (snapshot.Retention{}) {
		cfg.Retention = snapshot.DefaultRetention
	}
	r := &run{cfg: cfg, log: cfg.Log.With("component", "backup", "set", cfg.Set.Name), start: cfg.Now(), reasons: map[string]int64{}}
	r.id = r.start.UTC().Format(time.RFC3339)
	r.excludes = append(append([]string{}, DefaultExcludes...), cfg.Set.Excludes...)
	r.prog.Phase = "scanning"

	// The current index, from this device's root; empty on the first run.
	index, err := r.currentIndex(ctx)
	if err != nil {
		return nil, err
	}
	if last := index.Latest(); last != nil {
		r.lastSnapshot = last.Started
	}
	// The rolling re-hash: about 1% of the known paths, oldest first.
	if rows, err := cfg.DB.OldestHashed(100); err == nil {
		r.rehash = rows
	}

	var tops []snapshot.Entry
	partial := false
	for _, folder := range cfg.Set.Folders {
		abs, err := filepath.Abs(folder)
		if err != nil {
			abs = folder
		}
		// The probe first: a folder the system guards is listed with the
		// setting to change, not scanned into a tree of refusals.
		if err := ProbeFolder(abs); err != nil {
			name, u := snapshot.MakeName(abs)
			tops = append(tops, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: err.Error()})
			r.skip("folder unreadable")
			r.log.Warn("folder not backed up", "folder", abs, "err", err)
			continue
		}
		e, err := r.scanDir(ctx, abs, abs)
		if err != nil {
			// A folder that cannot be read at all is listed as skipped.
			name, u := snapshot.MakeName(abs)
			tops = append(tops, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: err.Error()})
			r.skip("folder unreadable")
			continue
		}
		name, u := snapshot.MakeName(abs)
		e.Name, e.UTF8 = name, u
		tops = append(tops, *e)
		if ctx.Err() != nil {
			partial = true
		}
	}
	// The top directory: one entry per configured folder, by its path.
	r.prog.Phase = "publishing"
	r.report()
	topCap, err := r.putDirectory(context.WithoutCancel(ctx), snapshot.NewDirectory(tops))
	if err != nil {
		return nil, err
	}
	snap := snapshot.Snapshot{ID: r.id, Started: r.start, Finished: cfg.Now(), Set: cfg.Set.Name, Folders: cfg.Set.Folders, Top: topCap,
		Files: r.prog.Files, Bytes: r.prog.Bytes, Uploaded: r.prog.Uploaded, Skipped: r.prog.Skipped, Partial: partial || ctx.Err() != nil}
	index.Snapshots = append(index.Snapshots, snap)
	index.Snapshots = snapshot.Prune(index.Snapshots, cfg.Retention, cfg.Now())
	indexCap, err := r.putIndex(context.WithoutCancel(ctx), index)
	if err != nil {
		return nil, err
	}
	version, err := cfg.Engine.PutRoot(context.WithoutCancel(ctx), engine.Root{WrittenAt: cfg.Now(), IndexCap: indexCap})
	if err != nil {
		return &Result{Snapshot: snap, RootVersion: version, Reasons: r.reasons}, fmt.Errorf("backup: root version %d: %w", version, err)
	}
	r.prog.Phase = "done"
	r.report()
	r.log.Info("backup published", "snapshot", snap.ID, "files", snap.Files, "bytes", snap.Bytes, "uploaded", snap.Uploaded, "skipped", snap.Skipped, "partial", snap.Partial, "root", version)
	return &Result{Snapshot: snap, RootVersion: version, Reasons: r.reasons}, nil
}

func (r *run) report() {
	if r.cfg.Progress != nil {
		r.cfg.Progress(r.prog)
	}
}

func (r *run) skip(reason string) {
	r.prog.Skipped++
	r.reasons[reason]++
}

// currentIndex reads this device's index object through its root.
func (r *run) currentIndex(ctx context.Context) (*snapshot.Index, error) {
	roots, err := r.cfg.Engine.GetRoots(ctx, r.cfg.Member)
	if err != nil {
		return nil, err
	}
	empty := &snapshot.Index{Version: snapshot.IndexVersion, Device: r.cfg.Device.String()}
	for _, v := range roots.Views {
		if v.Device != r.cfg.Device || v.Root == nil || v.Root.IndexCap == "" {
			continue
		}
		rc, err := object.ParseReadCap(v.Root.IndexCap)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := r.cfg.Engine.Get(ctx, rc, &buf, nil); err != nil {
			return nil, fmt.Errorf("backup: index object: %w", err)
		}
		return snapshot.DecodeIndex(buf.Bytes())
	}
	return empty, nil
}

// excluded applies the base-name globs and the relative prefixes.
func (r *run) excluded(root, p string, isDir bool) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	base := path.Base(rel)
	for _, pat := range r.excludes {
		if strings.Contains(pat, "/") {
			if rel == pat || strings.HasPrefix(rel, pat+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	_ = isDir
	return false
}

// scanDir builds the directory object of dir, bottom-up.
func (r *run) scanDir(ctx context.Context, root, dir string) (*snapshot.Entry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []snapshot.Entry
	var files, bytesBelow int64
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			break
		}
		p := filepath.Join(dir, it.Name())
		name, u := snapshot.MakeName(it.Name())
		if r.excluded(root, p, it.IsDir()) {
			continue
		}
		mode := it.Type()
		switch {
		case mode.IsDir():
			sub, err := r.scanDir(ctx, root, p)
			if err != nil {
				entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: err.Error()})
				r.skip("directory unreadable")
				continue
			}
			sub.Name, sub.UTF8 = name, u
			files += sub.Entries
			bytesBelow += sub.Bytes
			entries = append(entries, *sub)
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: err.Error()})
				r.skip("symlink unreadable")
				continue
			}
			entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSymlink, Target: target})
		case mode.IsRegular():
			e, err := r.scanFile(ctx, p)
			if err != nil {
				reason, detail := skipReason(ctx, err)
				entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: detail})
				r.skip(reason)
				continue
			}
			e.Name, e.UTF8 = name, u
			files++
			bytesBelow += e.Size
			entries = append(entries, *e)
		default:
			entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: "not a file, directory or symlink"})
			r.skip("special file")
		}
	}
	if ctx.Err() != nil {
		// Whatever the walk did not reach is listed, so the snapshot says
		// what it holds and what it does not.
		seen := map[string]bool{}
		for _, e := range entries {
			seen[string(e.Name)] = true
		}
		for _, it := range items {
			if !seen[it.Name()] && !r.excluded(root, filepath.Join(dir, it.Name()), it.IsDir()) {
				name, u := snapshot.MakeName(it.Name())
				entries = append(entries, snapshot.Entry{Name: name, UTF8: u, Kind: snapshot.KindSkipped, Reason: "interrupted"})
				r.skip("interrupted")
			}
		}
	}
	d := snapshot.NewDirectory(entries)
	capStr, err := r.putDirectory(context.WithoutCancel(ctx), d)
	if err != nil {
		return nil, err
	}
	e := &snapshot.Entry{Kind: snapshot.KindDir, Cap: capStr, Entries: files, Bytes: bytesBelow}
	if fi, err := os.Lstat(dir); err == nil {
		e.Mtime, e.Mode = fi.ModTime().UnixNano(), uint32(fi.Mode().Perm())
	}
	return e, nil
}

// scanFile stores one file: by the scan key when nothing changed, by hash
// otherwise, inline when tiny, as chunks when large.
func (r *run) scanFile(ctx context.Context, p string) (*snapshot.Entry, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if cloudPlaceholder(fi) {
		return nil, errPlaceholder
	}
	key := scanKey(p, fi)
	now := r.cfg.Now()
	r.prog.Current = p
	e := &snapshot.Entry{Kind: snapshot.KindFile, Size: fi.Size(), Mtime: fi.ModTime().UnixNano(), Mode: uint32(fi.Mode().Perm())}
	r.prog.Files++
	r.prog.Bytes += fi.Size()
	if fi.Size() < snapshot.InlineMax {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		e.Inline = b
		return e, nil
	}
	row, _ := r.cfg.DB.LookupScan(p)
	if row != nil && row.Key == key && !r.rehash[p] && r.stillStored(ctx, row) {
		e.Cap, e.Chunks = row.Cap, row.Chunks
		row.LastSeen = now
		_ = r.cfg.DB.RememberScan(*row)
		return e, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	var hash object.Hash
	copy(hash[:], h.Sum(nil))
	if fi2, err := f.Stat(); err == nil && scanKey(p, fi2) != key {
		return nil, errors.New("changed while being read")
	}
	newRow := localdb.ScanRow{Path: p, Key: key, Hash: hash, Size: fi.Size(), LastSeen: now, LastHashed: now}
	if fi.Size() <= snapshot.ChunkSize {
		capStr, err := r.storeSection(ctx, f, 0, fi.Size(), row, hash)
		if err != nil {
			return nil, err
		}
		e.Cap, newRow.Cap = capStr, capStr
	} else {
		for off := int64(0); off < fi.Size(); off += snapshot.ChunkSize {
			n := min(int64(snapshot.ChunkSize), fi.Size()-off)
			capStr, err := r.storeSection(ctx, f, off, n, nil, object.Hash{})
			if err != nil {
				return nil, err
			}
			e.Chunks = append(e.Chunks, capStr)
		}
		newRow.Chunks = e.Chunks
	}
	_ = r.cfg.DB.RememberScan(newRow)
	return e, nil
}

// stillStored is the RFC's check before a root: a capability taken from
// the scan cache that the previous root may not reference is confirmed
// on the servers, which also touches it there. A file seen in the last
// published snapshot is referenced by the current root and needs no
// probe; one that was away longer, or any file before the first
// snapshot, is probed, and an object that is gone or tombstoned makes
// the file a miss, hashed and stored again.
func (r *run) stillStored(ctx context.Context, row *localdb.ScanRow) bool {
	if !r.lastSnapshot.IsZero() && !row.LastSeen.Before(r.lastSnapshot) {
		return true
	}
	caps := row.Chunks
	if row.Cap != "" {
		caps = []string{row.Cap}
	}
	for _, c := range caps {
		rc, err := object.ParseReadCap(c)
		if err != nil {
			return false
		}
		h, err := r.cfg.Engine.Check(ctx, rc)
		if err != nil || h == nil || !h.Readable {
			r.log.Info("a cached object is gone from the grid; the file is stored again", "err", err)
			return false
		}
	}
	return true
}

// storeSection stores a file's bytes, or a chunk, as one object: asked
// of the engine by hash first, uploaded only when missing.
func (r *run) storeSection(ctx context.Context, f *os.File, off, n int64, prev *localdb.ScanRow, known object.Hash) (string, error) {
	sec := io.NewSectionReader(f, off, n)
	hash := known
	if hash == (object.Hash{}) {
		h := blake3.New()
		if _, err := io.Copy(h, sec); err != nil {
			return "", err
		}
		copy(hash[:], h.Sum(nil))
	}
	if rc, ok, err := r.cfg.Engine.Has(ctx, hash, n); err == nil && ok {
		return rc.String(), nil
	}
	if _, err := sec.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	rc, err := r.cfg.Engine.Put(ctx, sec, n)
	if err != nil {
		return "", err
	}
	r.prog.Uploaded += n
	r.report()
	_ = prev
	return rc.String(), nil
}

// putDirectory stores a directory object; an unchanged folder costs nothing.
func (r *run) putDirectory(ctx context.Context, d *snapshot.Directory) (string, error) {
	b, err := d.Encode()
	if err != nil {
		return "", err
	}
	return r.putBytes(ctx, b)
}

func (r *run) putIndex(ctx context.Context, x *snapshot.Index) (string, error) {
	b, err := x.Encode()
	if err != nil {
		return "", err
	}
	return r.putBytes(ctx, b)
}

func (r *run) putBytes(ctx context.Context, b []byte) (string, error) {
	hash := object.Sum(b)
	if rc, ok, err := r.cfg.Engine.Has(ctx, hash, int64(len(b))); err == nil && ok {
		return rc.String(), nil
	}
	rc, err := r.cfg.Engine.Put(ctx, bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return "", err
	}
	r.prog.Uploaded += int64(len(b))
	return rc.String(), nil
}

// skipReason classifies a file that could not be read: the reason counted
// for the page, and the wording the snapshot entry keeps. A placeholder
// of a file evicted to the cloud (iCloud Drive's optimised storage, or
// another file provider's online-only file) fails with EDEADLK on macOS
// when a service is not allowed to fetch it; the backup lists it rather
// than pulling it down.
func skipReason(ctx context.Context, err error) (counted, detail string) {
	switch {
	case errors.Is(err, errPlaceholder):
		return "not on this computer", err.Error()
	case errors.Is(err, fs.ErrPermission):
		return "permission denied", err.Error()
	case ctx.Err() != nil:
		return "interrupted", err.Error()
	case errors.Is(err, syscall.EDEADLK):
		return "not on this computer", "evicted to the cloud, not on this computer: " + err.Error()
	}
	return "unreadable", err.Error()
}

// errPlaceholder is a file a cloud provider keeps online-only.
var errPlaceholder = errors.New("evicted to the cloud, not on this computer")

// Reasons lists the skip reasons sorted by count, for the page.
func (res *Result) ReasonList() []string {
	var out []string
	for reason, n := range res.Reasons {
		out = append(out, fmt.Sprintf("%s: %d", reason, n))
	}
	sort.Strings(out)
	return out
}
