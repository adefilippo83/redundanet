// Package restore reads snapshots back: list a member's device roots and
// their indexes, browse a snapshot's folders without downloading what is
// not asked for, restore a path with safe writes, and verify a sample of
// files unattended (RFC 0001, "Restore" and "Restore test").
package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// DeviceIndex is one device's root and the index it points to.
type DeviceIndex struct {
	Device  identity.Key
	Version uint64
	Index   *snapshot.Index
	Err     string // why the index could not be read, if it could not
}

// Listing is what a member can restore from.
type Listing struct {
	Devices  []DeviceIndex
	Answered int
	Servers  int
}

// List fetches every device root of the member and its index.
func List(ctx context.Context, e engine.Engine, member identity.Key) (*Listing, error) {
	roots, err := e.GetRoots(ctx, member)
	if err != nil {
		return nil, err
	}
	l := &Listing{Answered: roots.Answered, Servers: roots.Servers}
	for _, v := range roots.Views {
		d := DeviceIndex{Device: v.Device, Version: v.Version}
		switch {
		case v.Root == nil:
			d.Err = "the root could not be opened with this member's key"
		case v.Root.IndexCap == "":
			d.Err = "no backup yet"
		default:
			x, err := fetchIndex(ctx, e, v.Root.IndexCap)
			if err != nil {
				d.Err = err.Error()
			} else {
				d.Index = x
			}
		}
		l.Devices = append(l.Devices, d)
	}
	sort.Slice(l.Devices, func(i, j int) bool { return l.Devices[i].Device.Less(l.Devices[j].Device) })
	return l, nil
}

func fetchIndex(ctx context.Context, e engine.Engine, capStr string) (*snapshot.Index, error) {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := e.Get(ctx, rc, &buf, nil); err != nil {
		return nil, err
	}
	return snapshot.DecodeIndex(buf.Bytes())
}

// Directory fetches a directory object.
func Directory(ctx context.Context, e engine.Engine, capStr string) (*snapshot.Directory, error) {
	rc, err := object.ParseReadCap(capStr)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := e.Get(ctx, rc, &buf, nil); err != nil {
		return nil, err
	}
	return snapshot.DecodeDirectory(buf.Bytes())
}

// Browse walks from a snapshot's top down the path elements and returns
// the entry reached and, for a directory, its listing. An empty path is
// the top.
func Browse(ctx context.Context, e engine.Engine, snap *snapshot.Snapshot, elems []string) (*snapshot.Entry, *snapshot.Directory, error) {
	entry := &snapshot.Entry{Kind: snapshot.KindDir, Cap: snap.Top}
	for _, name := range elems {
		if entry.Kind != snapshot.KindDir {
			return nil, nil, snapshot.ErrNotFound
		}
		d, err := Directory(ctx, e, entry.Cap)
		if err != nil {
			return nil, nil, err
		}
		next := d.Find(name)
		if next == nil {
			return nil, nil, snapshot.ErrNotFound
		}
		entry = next
	}
	if entry.Kind != snapshot.KindDir {
		return entry, nil, nil
	}
	d, err := Directory(ctx, e, entry.Cap)
	if err != nil {
		return nil, nil, err
	}
	return entry, d, nil
}

// Resolve turns a path string into the element list Browse takes. The
// top directory names folders by their absolute path, which contains
// separators, so the longest folder name that prefixes the path is the
// first element and the rest is split on slashes. An empty path is the
// top.
func Resolve(ctx context.Context, e engine.Engine, snap *snapshot.Snapshot, p string) ([]string, error) {
	p = slashes(p)
	if p == "" || p == "/" {
		return nil, nil
	}
	top, err := Directory(ctx, e, snap.Top)
	if err != nil {
		return nil, err
	}
	// The top entry is matched on a slash-normalised form, since the path
	// may be typed on another platform than the one that backed it up, but
	// the element handed on is the entry's own name.
	best, bestName := "", ""
	for _, en := range top.Entries {
		name := slashes(en.NameString())
		if (p == name || strings.HasPrefix(p, strings.TrimSuffix(name, "/")+"/")) && len(name) > len(best) {
			best, bestName = name, en.NameString()
		}
	}
	if best == "" {
		return nil, snapshot.ErrNotFound
	}
	elems := []string{bestName}
	for _, part := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(p, best), "/"), "/") {
		if part != "" {
			elems = append(elems, part)
		}
	}
	return elems, nil
}

// slashes turns both separators into "/", whatever the platform: a backup
// made on Windows is browsed from a Mac with the names it has, and the
// other way round. A Unix name containing a backslash is split like a
// path here, so browse it by elements instead.
func slashes(p string) string { return strings.ReplaceAll(filepath.ToSlash(p), `\`, "/") }

// Options steer a restore.
type Options struct {
	Overwrite bool // replace existing files; otherwise they are kept with a suffix
	Log       *slog.Logger
}

// Report is what a restore did; Exceptions makes it "complete with
// exceptions", never plain complete.
type Report struct {
	Files      int64    `json:"files"`
	Bytes      int64    `json:"bytes"`
	Dirs       int64    `json:"dirs"`
	Symlinks   int64    `json:"symlinks"`
	Exceptions []string `json:"exceptions,omitempty"`
}

// dest is where a restore writes: the folder the member chose, opened as
// a root, so that no name, no ".." and no symlink takes a write out of
// it. A snapshot is data from the grid, written by whoever held the
// member's secret and a certified device at the time, which after a theft
// is not the member; its names and links are not trusted to stay inside.
type dest struct {
	root *os.Root
	base string // the folder's path, for messages
}

func (d *dest) path(rel string) string { return filepath.Join(d.base, rel) }

// localName is the path, relative to the folder being written, that an
// entry's name stands for, and whether a restore writes it at all. Below
// the top a name is one path element. The top directory names each folder
// by its whole path, so there the separators stay and the folder is
// recreated under the target; no element may be "..".
func localName(name string, top bool) (string, bool) {
	if top {
		name = strings.TrimLeft(filepath.ToSlash(name), "/")
		for _, el := range strings.Split(name, "/") {
			if el == ".." {
				return "", false
			}
		}
		name = filepath.FromSlash(name)
	} else if strings.ContainsRune(name, '/') || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	name = filepath.Clean(name)
	if name == "." || !filepath.IsLocal(name) {
		return "", false
	}
	return name, true
}

// Restore writes the entry at path elems of a snapshot into target: a
// file becomes target itself, a directory is recreated under it. Nothing
// is written outside target (see dest). Every file is written under a
// temporary name and renamed when complete and verified; mtime and
// permission bits come after the data, directory mtimes after their
// children; ownership and the set-user-ID, set-group-ID and sticky bits
// are not restored.
func Restore(ctx context.Context, e engine.Engine, snap *snapshot.Snapshot, elems []string, target string, opts Options) (*Report, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	entry, dir, err := Browse(ctx, e, snap, elems)
	if err != nil {
		return nil, err
	}
	rep := &Report{}
	if entry.Kind == snapshot.KindDir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(target)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := restoreDir(ctx, e, dir, &dest{root: root, base: target}, "", len(elems) == 0, opts, rep); err != nil {
			return rep, err
		}
		return rep, nil
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return rep, restoreEntry(ctx, e, entry, &dest{root: root, base: filepath.Dir(target)}, filepath.Base(target), opts, rep)
}

// restoreDir recreates a directory's entries under rel, a path inside the
// destination. top says the directory is the snapshot's top, whose names
// are whole folder paths.
func restoreDir(ctx context.Context, e engine.Engine, d *snapshot.Directory, dst *dest, rel string, top bool, opts Options, rep *Report) error {
	// Names that collide on a case-insensitive or normalizing target are
	// detected before any write; one gets a suffix and is listed.
	seen := map[string]bool{}
	for i := range d.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		en := &d.Entries[i]
		name, mapped := safeName(en)
		name, ok := localName(name, top)
		if !ok {
			rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: not a name a restore writes", en.NameString()))
			continue
		}
		if seen[strings.ToLower(name)] {
			name += ".restored-duplicate"
			mapped = true
		}
		seen[strings.ToLower(name)] = true
		if mapped {
			rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: restored as %s", en.NameString(), name))
		}
		p := filepath.Join(rel, name)
		switch en.Kind {
		case snapshot.KindDir:
			if err := dst.root.MkdirAll(p, 0o755); err != nil {
				rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: %v", dst.path(p), err))
				continue
			}
			sub, err := Directory(ctx, e, en.Cap)
			if err != nil {
				rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: %v", dst.path(p), err))
				continue
			}
			if err := restoreDir(ctx, e, sub, dst, p, false, opts, rep); err != nil {
				return err
			}
			if en.Mtime != 0 {
				t := time.Unix(0, en.Mtime)
				_ = dst.root.Chtimes(p, t, t)
			}
			rep.Dirs++
		case snapshot.KindSkipped:
			rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: not in the snapshot (%s)", en.NameString(), en.Reason))
		default:
			if err := restoreEntry(ctx, e, en, dst, p, opts, rep); err != nil {
				if ctx.Err() != nil {
					return err
				}
				rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s: %v", dst.path(p), err))
			}
		}
	}
	return nil
}

// restoreEntry writes one file or symlink at p, a path inside the
// destination.
func restoreEntry(ctx context.Context, e engine.Engine, en *snapshot.Entry, dst *dest, p string, opts Options, rep *Report) error {
	if _, err := dst.root.Lstat(p); err == nil && !opts.Overwrite {
		p += ".restored"
		rep.Exceptions = append(rep.Exceptions, fmt.Sprintf("%s exists: written as %s", en.NameString(), filepath.Base(p)))
	}
	switch en.Kind {
	case snapshot.KindSymlink:
		_ = dst.root.Remove(p)
		if err := dst.root.Symlink(en.Target, p); err != nil {
			if runtime.GOOS == "windows" {
				return fmt.Errorf("symlink not created on this account: %w", err)
			}
			return err
		}
		rep.Symlinks++
		return nil
	case snapshot.KindFile:
	default:
		return fmt.Errorf("cannot restore a %s", en.Kind)
	}
	tmp := p + ".rn3-restoring"
	// A symlink left under the temporary name is removed, never written
	// through, even when it points inside the destination.
	if fi, err := dst.root.Lstat(tmp); err == nil && !fi.Mode().IsRegular() {
		if err := dst.root.Remove(tmp); err != nil {
			return err
		}
	}
	f, err := dst.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = dst.root.Remove(tmp)
		}
	}()
	var n int64
	switch {
	case en.Inline != nil || (en.Cap == "" && len(en.Chunks) == 0):
		m, err := f.Write(en.Inline)
		if err != nil {
			return err
		}
		n = int64(m)
	case en.Cap != "":
		rc, err := object.ParseReadCap(en.Cap)
		if err != nil {
			return err
		}
		if err := e.Get(ctx, rc, f, nil); err != nil {
			return err
		}
		n = int64(rc.Size)
	default:
		for _, c := range en.Chunks {
			rc, err := object.ParseReadCap(c)
			if err != nil {
				return err
			}
			if err := e.Get(ctx, rc, f, nil); err != nil {
				return err
			}
			n += int64(rc.Size)
		}
	}
	if n != en.Size {
		return fmt.Errorf("restored %d bytes, the snapshot says %d", n, en.Size)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := dst.root.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	if en.Mode != 0 {
		_ = dst.root.Chmod(p, os.FileMode(en.Mode).Perm())
	}
	if en.Mtime != 0 {
		t := time.Unix(0, en.Mtime)
		_ = dst.root.Chtimes(p, t, t)
	}
	rep.Files++
	rep.Bytes += n
	return nil
}

// safeName maps a name the target cannot hold to an escaped form and says
// whether it did.
func safeName(en *snapshot.Entry) (string, bool) {
	name := string(en.Name)
	if !en.UTF8 {
		return fmt.Sprintf("%x.invalid-name", en.Name), true
	}
	if runtime.GOOS != "windows" {
		return name, false
	}
	mapped := false
	for _, c := range `<>:"/\|?*` {
		if strings.ContainsRune(name, c) {
			name = strings.ReplaceAll(name, string(c), "_")
			mapped = true
		}
	}
	upper := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	for _, r := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3"} {
		if upper == r {
			name, mapped = "_"+name, true
		}
	}
	if t := strings.TrimRight(name, ". "); t != name {
		name, mapped = t+"_", true
	}
	return name, mapped
}

// Verification is a restore test's outcome.
type Verification struct {
	At        time.Time `json:"at"`
	Snapshot  string    `json:"snapshot"`
	Sampled   int       `json:"sampled"`
	Verified  int       `json:"verified"` // read back and matched the capability's key
	Compared  int       `json:"compared"` // also matched the source file, which was unchanged
	Failed    []string  `json:"failed,omitempty"`
	Unchanged int       `json:"unchanged"` // sources that still matched the snapshot's size and mtime
}

// Verify restores a sample of files from a snapshot into a temporary
// directory and compares each with its source when the source is
// unchanged. Every read verifies against the capability's key on its own.
func Verify(ctx context.Context, e engine.Engine, snap *snapshot.Snapshot, sample int, now time.Time) (*Verification, error) {
	v := &Verification{At: now, Snapshot: snap.ID}
	var files []sampled
	if err := collect(ctx, e, snap.Top, "", &files, sample*8); err != nil {
		return v, err
	}
	if len(files) == 0 {
		return v, nil
	}
	// Spread the sample over the collected files.
	step := len(files) / sample
	if step < 1 {
		step = 1
	}
	tmp, err := os.MkdirTemp("", "rn3-verify-")
	if err != nil {
		return v, err
	}
	defer os.RemoveAll(tmp)
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return v, err
	}
	defer root.Close()
	scratch := &dest{root: root, base: tmp}
	for i := 0; i < len(files) && v.Sampled < sample; i += step {
		f := files[i]
		v.Sampled++
		name := fmt.Sprintf("f%d", i)
		dst := filepath.Join(tmp, name)
		rep := &Report{}
		if err := restoreEntry(ctx, e, &f.entry, scratch, name, Options{Overwrite: true, Log: slog.Default()}, rep); err != nil {
			v.Failed = append(v.Failed, f.path+": "+err.Error())
			continue
		}
		v.Verified++
		// The source, when it still looks like the snapshot's file.
		if fi, err := os.Lstat(f.source); err == nil && fi.Mode().IsRegular() && fi.Size() == f.entry.Size && fi.ModTime().UnixNano() == f.entry.Mtime {
			v.Unchanged++
			if same, err := sameContent(f.source, dst); err == nil && same {
				v.Compared++
			} else {
				v.Failed = append(v.Failed, f.path+": differs from the unchanged source")
			}
		}
	}
	return v, nil
}

type sampled struct {
	path, source string
	entry        snapshot.Entry
}

// collect walks the snapshot breadth-first until it has enough files.
func collect(ctx context.Context, e engine.Engine, capStr, prefix string, out *[]sampled, limit int) error {
	d, err := Directory(ctx, e, capStr)
	if err != nil {
		return err
	}
	var dirs []snapshot.Entry
	for _, en := range d.Entries {
		p := en.NameString()
		if prefix != "" {
			p = filepath.Join(prefix, p)
		}
		switch en.Kind {
		case snapshot.KindFile:
			if len(*out) < limit {
				*out = append(*out, sampled{path: p, source: p, entry: en})
			}
		case snapshot.KindDir:
			dirs = append(dirs, en)
		}
	}
	for _, en := range dirs {
		if len(*out) >= limit {
			break
		}
		p := en.NameString()
		if prefix != "" {
			p = filepath.Join(prefix, p)
		}
		if err := collect(ctx, e, en.Cap, p, out, limit); err != nil {
			return err
		}
	}
	return nil
}

func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ba, bb := make([]byte, 1<<16), make([]byte, 1<<16)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			if (ea == io.EOF || ea == io.ErrUnexpectedEOF) && (eb == io.EOF || eb == io.ErrUnexpectedEOF) {
				return true, nil
			}
			if ea != nil && eb != nil {
				return false, errors.Join(ea, eb)
			}
			return false, nil
		}
	}
}

// TargetName is the name a restored entry called name gets on this
// machine: its own, or the escaped form a name the file system cannot hold
// is restored under. A caller that picks where one entry goes uses it.
func TargetName(name string) string {
	n, _ := safeName(&snapshot.Entry{Name: []byte(name), UTF8: true})
	return n
}
