package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// The restore view. Reading is a chain of links: the devices of the member
// with their versions, then a version's folders, down to the folder or the
// file wanted; every step is a GET that fetches only the directory shown
// and changes nothing. The one change is the form at the end, which starts
// the restore into a folder the member names.
//
// A place in a version is two strings: root, the top entry (a backed up
// folder by its full path, which holds separators), and sub, the names
// below it joined with slashes.

// restoreTimeout bounds what one view of the page waits for the network.
const restoreTimeout = 30 * time.Second

type restoreDevice struct {
	Key      string
	Short    string
	This     bool
	Err      string
	Versions []restoreVersion // newest first
}

type restoreVersion struct {
	At      time.Time
	Set     string
	Files   int64
	Bytes   int64
	Partial bool
	Href    string
}

type crumb struct {
	Label string
	Href  string
}

type restoreEntry struct {
	Name    string
	Href    string // "" for what cannot be opened: a skipped file, a name that is not text
	Dir     bool
	Link    bool   // a symbolic link
	Skipped string // why it was not backed up
	Files   int64  // a folder: the files below it
	Bytes   int64
	At      time.Time
}

// restoreData is what the restore view shows: the devices, or one place in
// one version.
type restoreData struct {
	Err      string
	Answered int
	Servers  int
	Devices  []restoreDevice

	Browsing bool
	Device   string
	Snapshot string
	Root     string
	Sub      string
	When     string // the version, as a time when its name is one
	Crumbs   []crumb
	Here     string // the name of the place shown
	IsFile   bool
	Bytes    int64
	Entries  []restoreEntry
	Folder   string // where restored files go unless the member says otherwise
}

func restoreHref(device, snap, root, sub string) string {
	q := url.Values{"device": {device}, "snapshot": {snap}}
	if root != "" {
		q.Set("root", root)
		if sub != "" {
			q.Set("sub", sub)
		}
	}
	return "/restore?" + q.Encode()
}

// restorePath is the path string the runner resolves.
func restorePath(root, sub string) string {
	if root == "" || sub == "" {
		return root
	}
	return strings.TrimSuffix(root, "/") + "/" + sub
}

// lastName is the last name of a path written with either separator.
func lastName(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(p, `\`, "/"), "/")
	return p[strings.LastIndex(p, "/")+1:]
}

func defaultRestoreFolder() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "RedundaNet restored")
}

// restoreView fills the restore view from the request's query.
func (s *Server) restoreView(r *http.Request, self string) *restoreData {
	d := &restoreData{Folder: defaultRestoreFolder()}
	j := s.jobs()
	if j == nil {
		return d
	}
	ctx, cancel := context.WithTimeout(r.Context(), restoreTimeout)
	defer cancel()
	q := r.URL.Query()
	d.Device, d.Snapshot, d.Root, d.Sub = q.Get("device"), q.Get("snapshot"), q.Get("root"), strings.Trim(q.Get("sub"), "/")
	if d.Device == "" || d.Snapshot == "" {
		l, err := j.List(ctx)
		if err != nil {
			d.Err = err.Error()
			return d
		}
		d.Answered, d.Servers = l.Answered, l.Servers
		for _, dev := range l.Devices {
			rd := restoreDevice{Key: dev.Device.String(), Short: dev.Device.Short(), This: dev.Device.String() == self, Err: dev.Err}
			if dev.Index != nil {
				for i := len(dev.Index.Snapshots) - 1; i >= 0; i-- {
					sn := dev.Index.Snapshots[i]
					rd.Versions = append(rd.Versions, restoreVersion{At: sn.Started, Set: sn.Set, Files: sn.Files, Bytes: sn.Bytes, Partial: sn.Partial, Href: restoreHref(rd.Key, sn.ID, "", "")})
				}
			}
			d.Devices = append(d.Devices, rd)
		}
		return d
	}
	d.Browsing = true
	d.When = d.Snapshot
	if t, err := time.Parse(time.RFC3339Nano, d.Snapshot); err == nil {
		d.When = t.Local().Format("2006-01-02 15:04")
	}
	d.Crumbs = []crumb{{Href: restoreHref(d.Device, d.Snapshot, "", "")}}
	if d.Root != "" {
		d.Here = d.Root
		d.Crumbs = append(d.Crumbs, crumb{Label: d.Root, Href: restoreHref(d.Device, d.Snapshot, d.Root, "")})
		var below []string
		for _, name := range strings.Split(d.Sub, "/") {
			if name == "" {
				continue
			}
			below = append(below, name)
			d.Here = name
			d.Crumbs = append(d.Crumbs, crumb{Label: name, Href: restoreHref(d.Device, d.Snapshot, d.Root, strings.Join(below, "/"))})
		}
	}
	dev, err := identity.ParseKey(d.Device)
	if err != nil {
		d.Err = "bad device key"
		return d
	}
	entry, dir, err := j.Browse(ctx, dev, d.Snapshot, restorePath(d.Root, d.Sub))
	if err != nil {
		d.Err = err.Error()
		return d
	}
	if entry.Kind != snapshot.KindDir || dir == nil {
		d.IsFile, d.Bytes = true, entry.Size
		return d
	}
	for i := range dir.Entries {
		en := &dir.Entries[i]
		re := restoreEntry{Name: en.NameString(), Dir: en.Kind == snapshot.KindDir, Link: en.Kind == snapshot.KindSymlink, Files: en.Entries, Bytes: en.Size}
		if en.Mtime != 0 {
			re.At = time.Unix(0, en.Mtime)
		}
		if re.Dir {
			re.Bytes = en.Bytes
		}
		switch {
		case en.Kind == snapshot.KindSkipped:
			re.Skipped = en.Reason
		case !en.UTF8 || (d.Root != "" && strings.ContainsAny(re.Name, `/\`)):
			// A name that is not text, or one that would read as two
			// names in a path, is restored with the folder it is in.
		case d.Root == "":
			re.Href = restoreHref(d.Device, d.Snapshot, re.Name, "")
		default:
			re.Href = restoreHref(d.Device, d.Snapshot, d.Root, path.Join(d.Sub, re.Name))
		}
		d.Entries = append(d.Entries, re)
	}
	return d
}

// startRestore is the form at the end: what the place in the version
// holds goes into the folder named. A folder or a file keeps its name
// inside that folder; the whole version goes into the folder itself.
func (s *Server) startRestore(r *http.Request) (target string, err error) {
	folder := strings.TrimSpace(r.FormValue("folder"))
	if folder == "" || !filepath.IsAbs(folder) {
		return "", errors.New("the folder is given as a full path")
	}
	dev, err := identity.ParseKey(r.FormValue("device"))
	if err != nil {
		return "", errors.New("bad device key")
	}
	snap, root, sub := r.FormValue("snapshot"), r.FormValue("root"), strings.Trim(r.FormValue("sub"), "/")
	p := restorePath(root, sub)
	target = filepath.Clean(folder)
	// A file is written at its target and nothing above it is made, so the
	// folder the member names is made here, as it would be for a folder.
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", err
	}
	if p != "" {
		name := lastName(p)
		if name == "" || name == "." || name == ".." {
			return "", errors.New("nothing to restore at this place")
		}
		target = filepath.Join(target, restore.TargetName(name))
	}
	return target, s.jobs().StartRestore(dev, snap, p, target, r.FormValue("overwrite") == "on")
}
