package api

import (
	"sort"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
)

// The page is five views of one snapshot, rendered on the server in the
// reader's language. This file turns the snapshot into what a member reads
// first: one sentence on how their backup stands and the list of what
// needs them, each with the button that settles it.

// view is one page of the site the agent serves.
type view struct {
	Path string // "/" is the overview
	Name string // the template and the dictionary prefix
}

var views = []view{{"/", "overview"}, {"/backups", "backups"}, {"/restore", "restore"}, {"/network", "network"}, {"/settings", "settings"}, {"/advanced", "advanced"}}

func viewAt(path string) (view, bool) {
	for _, v := range views {
		if v.Path == path {
			return v, true
		}
	}
	return view{}, false
}

type navItem struct {
	Path    string
	Label   string
	Current bool
}

// flash is the outcome of the last action, shown once.
type flash struct {
	Text string
	Bad  bool
}

// issue is one thing that needs the member, with at most one way to
// settle it from the page: a form posted to a page action, or a link.
type issue struct {
	Kind    string // bad, warn or info: how loudly it is shown
	Title   string
	Text    string
	Action  string // the page action its button posts to
	Version string // sent with the action, for the update ones
	Href    string // or a link instead
	Label   string
}

// activity is one line of what the device did lately.
type activity struct {
	At   time.Time
	Text string
}

// overviewData is the first view.
type overviewData struct {
	State      string // protected, running, attention, waiting, empty, node
	Title, Sub string
	Issues     []issue
	LastBackup time.Time
	Files      int64
	Bytes      int64
	Activity   []activity
}

// setView is a backup set with its run, found by name.
type setView struct {
	backup.Set
	Run      *jobs.RunStatus
	Excluded string // the set's own excludes, as the form shows them
}

type option struct {
	Value    string
	Label    string
	Selected bool
}

// pageData is what every view renders from.
type pageData struct {
	Snapshot
	tr
	View     string
	Nav      []navItem
	Lang     string
	Langs    []Language
	CSRF     string
	Back     string // the view's path, where an action returns to
	Flash    *flash
	Online   string // up, down or "" when the device has no hub to reach
	Overview overviewData
	Sets     []setView
	Every    []option
	Verify   []option
	Cleanup  []option
	Logs     []logbuf.Line
	Restore  *restoreData // the restore view only
	Paired   []deviceView // the devices this one paired, with how each is reached now
}

// deviceView is a paired device and its connection at the moment.
type deviceView struct {
	Device
	Short string
	Link  string // direct, relay or "" for none
}

// joinURL is where a member not listed yet sends the request.
const joinURL = "https://redundanet.com/join.html"

func buildPage(snap Snapshot, v view, l tr) pageData {
	d := pageData{Snapshot: snap, tr: l, View: v.Name, Back: v.Path, Lang: string(l), Langs: Languages}
	for _, x := range views {
		if ((x.Name == "backups" || x.Name == "restore") && snap.Jobs == nil) || (x.Name == "settings" && snap.Jobs == nil && snap.Update == nil) {
			continue
		}
		d.Nav = append(d.Nav, navItem{Path: x.Path, Label: l.T("nav." + x.Name), Current: x.Name == v.Name})
	}
	if t := snap.Transport; t != nil && len(t.Hubs) > 0 {
		d.Online = "down"
		for _, h := range t.Hubs {
			if h.Up {
				d.Online = "up"
			}
		}
	}
	d.Overview = buildOverview(snap, d.Online, l)
	for _, dev := range snap.Devices {
		dv := deviceView{Device: dev, Short: short(dev.Key)}
		if t := snap.Transport; t != nil {
			for _, sess := range t.Sessions {
				switch {
				case sess.Peer != dev.Key:
				case sess.Direct != "":
					dv.Link = "direct"
				case sess.Relay:
					dv.Link = "relay"
				}
			}
		}
		d.Paired = append(d.Paired, dv)
	}
	if j := snap.Jobs; j != nil {
		for _, s := range j.Sets {
			sv := setView{Set: s, Excluded: strings.Join(s.Excludes, ", ")}
			for i := range j.Runs {
				if j.Runs[i].Set == s.Name {
					sv.Run = &j.Runs[i]
				}
			}
			d.Sets = append(d.Sets, sv)
		}
		d.Every = intervalOptions(l, j.Every, time.Hour, 6*time.Hour, 12*time.Hour, 24*time.Hour, 48*time.Hour, 7*24*time.Hour)
		d.Verify = intervalOptions(l, j.VerifyEvery, 24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
		d.Cleanup = intervalOptions(l, j.CleanupEvery, 24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	}
	return d
}

// intervalOptions is a schedule menu: the usual intervals, plus the
// current one when it is none of them (set from the command line).
func intervalOptions(l tr, current string, choices ...time.Duration) []option {
	cur, err := time.ParseDuration(current)
	var out []option
	found := false
	for _, c := range choices {
		o := option{Value: c.String(), Label: intervalLabel(l, c), Selected: err == nil && c == cur}
		found = found || o.Selected
		out = append(out, o)
	}
	if !found && current != "" {
		out = append(out, option{Value: current, Label: current, Selected: true})
	}
	return out
}

func intervalLabel(l tr, d time.Duration) string {
	switch {
	case d == time.Hour:
		return l.T("every.hour")
	case d < 24*time.Hour:
		return l.T("every.hours", int(d.Hours()))
	case d == 24*time.Hour:
		return l.T("every.day")
	case d == 7*24*time.Hour:
		return l.T("every.week")
	case d == 30*24*time.Hour:
		return l.T("every.month")
	}
	return l.T("every.days", int(d.Hours()/24))
}

func buildOverview(s Snapshot, online string, l tr) overviewData {
	var o overviewData
	add := func(i issue) { o.Issues = append(o.Issues, i) }

	if s.Admission == "waiting" {
		href := joinURL
		if s.JoinURL != "" {
			href = s.JoinURL // the form filled in from this device
		}
		add(issue{Kind: "warn", Title: l.T("issue.waiting.title"), Text: l.T("issue.waiting.text"), Href: href, Label: l.T("issue.waiting.button")})
	} else if s.JoinURL != "" {
		add(issue{Kind: "info", Title: l.T("issue.node.title", s.JoinDevice), Text: l.T("issue.node.text"), Href: s.JoinURL, Label: l.T("issue.node.button")})
	}
	if online == "down" {
		add(issue{Kind: "bad", Title: l.T("issue.offline.title"), Text: l.T("issue.offline.text")})
	}
	if m := s.Membership; m != nil {
		if m.NeedsUpdate != "" {
			add(issue{Kind: "bad", Title: l.T("issue.minversion.title", m.NeedsUpdate), Text: l.T("issue.minversion.text")})
		}
		if m.Expired {
			add(issue{Kind: "warn", Title: l.T("issue.expired.title"), Text: l.T("issue.expired.text")})
		}
	}
	running := false
	if j := s.Jobs; j != nil {
		if b := j.Budget; b != nil && b.Exhausted {
			add(issue{Kind: "bad", Title: l.T("issue.full.title"), Text: l.T("issue.full.text", b.AllowedGB)})
		}
		for _, r := range j.Runs {
			running = running || r.Running
			for _, need := range r.Needs {
				add(issue{Kind: "bad", Title: l.T("issue.unreadable.title", r.Set), Text: need})
			}
			if r.LastErr != "" {
				add(issue{Kind: "bad", Title: l.T("issue.failed.title", r.Set), Text: r.LastErr})
			}
			if r.Last != nil {
				if r.LastAt.After(o.LastBackup) {
					o.LastBackup = r.LastAt
				}
				o.Files += r.Last.Snapshot.Files
				o.Bytes += r.Last.Snapshot.Bytes
				text := l.T("activity.backup", r.Set, r.Last.Snapshot.Files)
				if r.Last.Snapshot.Partial {
					text = l.T("activity.backup.partial", r.Set, r.Last.Snapshot.Files)
				}
				o.Activity = append(o.Activity, activity{At: r.LastAt, Text: text})
			}
		}
		if len(j.Verifications) > 0 {
			v := j.Verifications[0]
			if v.Passed() {
				o.Activity = append(o.Activity, activity{At: v.At, Text: l.T("activity.verify.passed", v.Verified, v.Sampled)})
			} else {
				add(issue{Kind: "bad", Title: l.T("issue.verify.title"), Text: l.T("issue.verify.text", v.Verified, v.Sampled), Href: "backups", Label: l.T("issue.verify.button")})
				o.Activity = append(o.Activity, activity{At: v.At, Text: l.T("activity.verify.failed", v.Verified, v.Sampled)})
			}
		}
		if c := j.LastCleanup; c != nil && !c.DryRun {
			o.Activity = append(o.Activity, activity{At: c.At, Text: l.T("activity.cleanup", c.Reachable, c.Tombstoned)})
		}
		if r := j.Restore; r != nil && !r.Running && r.Report != nil {
			o.Activity = append(o.Activity, activity{At: r.At, Text: l.T("activity.restore", r.Report.Files, r.Target)})
		}
	}
	if u := s.Update; u != nil {
		for _, v := range u.Failed {
			add(issue{Kind: "warn", Title: l.T("issue.updatefailed.title", v), Text: l.T("issue.updatefailed.text"), Action: "update-retry", Version: v, Label: l.T("issue.updatefailed.button")})
		}
		if u.Available != "" {
			add(issue{Kind: "info", Title: l.T("issue.update.title", u.Available), Text: l.T("issue.update.text"), Action: "update-apply", Label: l.T("issue.update.button", u.Available)})
		}
	}
	if sv := s.Service; sv != nil && !sv.Installed {
		add(issue{Kind: "info", Title: l.T("issue.noservice.title"), Text: l.T("issue.noservice.text")})
	}
	sort.SliceStable(o.Activity, func(a, b int) bool { return o.Activity[a].At.After(o.Activity[b].At) })
	if len(o.Activity) > 6 {
		o.Activity = o.Activity[:6]
	}

	bad := false
	for _, i := range o.Issues {
		bad = bad || i.Kind == "bad"
	}
	switch {
	case s.Jobs == nil:
		o.State, o.Title, o.Sub = "node", l.T("hero.node.title"), l.T("hero.node.sub")
	case running:
		o.State, o.Title, o.Sub = "running", l.T("hero.running.title"), l.T("hero.running.sub")
	case len(s.Jobs.Sets) == 0:
		o.State, o.Title, o.Sub = "empty", l.T("hero.empty.title"), l.T("hero.empty.sub")
	case bad:
		o.State, o.Title, o.Sub = "attention", l.T("hero.attention.title"), l.T("hero.attention.sub")
	case o.LastBackup.IsZero():
		o.State, o.Title, o.Sub = "waiting", l.T("hero.waiting.title"), l.T("hero.waiting.sub")
	default:
		o.State, o.Title, o.Sub = "protected", l.T("hero.protected.title"), l.T("hero.protected.sub", l.Since(o.LastBackup))
	}
	return o
}
