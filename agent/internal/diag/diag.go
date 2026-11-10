// Package diag writes the diagnostics bundle of RFC 0001's supportability
// row: one file the member sends by hand, nothing uploaded, that works
// from the log files when the agent is down. Its contents are an
// allowlist: versions, the manifest revision, the service state, hubs and
// peers reachable as counts, the backup sets with their last runs, error
// kinds and counts, a disk check, and the last log lines. File and folder
// names count as secrets, as keys and addresses do: a path appears as the
// set's label plus a hash, or as a hash alone, and an address not at all.
package diag

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/diskfree"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

// Input is everything a bundle may draw from; any part may be missing.
type Input struct {
	Now      time.Time
	Version  string
	Network  string
	DataDir  string
	Device   string // the device key, public
	Member   string // the member key's short form
	Roles    []string
	Service  *service.Status
	Manifest *ManifestInfo
	// From the running agent, when it answers.
	AgentUp    bool
	Uptime     string
	Transport  *transport.Status
	Membership *membership.Status
	Update     *update.Status
	Jobs       *jobs.Status
	Repair     *repair.Status
	Lines      []logbuf.Line // the agent's recent log lines
	// LogText is the service's log file or journal, last lines first
	// kept whole; empty when none is at hand.
	LogText string
}

// ManifestInfo is what the manifest on disk says.
type ManifestInfo struct {
	Revision uint64
	Encoding string
	Hubs     int
	Members  int
	Storage  int
	SignedAt time.Time
}

// Summary is the allowlisted projection the bundle carries as JSON.
type Summary struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Version     string             `json:"version"`
	Platform    string             `json:"platform"`
	Go          string             `json:"go"`
	Network     string             `json:"network,omitempty"`
	Device      string             `json:"device,omitempty"`
	Member      string             `json:"member,omitempty"`
	Roles       []string           `json:"roles,omitempty"`
	AgentUp     bool               `json:"agent_up"`
	Uptime      string             `json:"uptime,omitempty"`
	Service     *service.Status    `json:"service,omitempty"`
	Manifest    *ManifestInfo      `json:"manifest,omitempty"`
	Membership  *membership.Status `json:"membership,omitempty"`
	Update      *update.Status     `json:"update,omitempty"`
	Hubs        []HubSummary       `json:"hubs,omitempty"`
	Peers       *PeerSummary       `json:"peers,omitempty"`
	Sets        []SetSummary       `json:"sets,omitempty"`
	RestoreTest *RestoreSummary    `json:"restore_tests,omitempty"`
	Cleanup     *CleanupSummary    `json:"cleanup,omitempty"`
	Repair      *repair.Status     `json:"repair,omitempty"`
	Disks       []DiskSummary      `json:"disks,omitempty"`
	Errors      []ErrorCount       `json:"errors,omitempty"`
}

// HubSummary is one hub channel without its addresses.
type HubSummary struct {
	Hub        string `json:"hub"`
	Up         bool   `json:"up"`
	Carrier    string `json:"carrier,omitempty"`
	Reconnects int    `json:"reconnects"`
	LastError  string `json:"last_error,omitempty"`
}

// PeerSummary counts the sessions.
type PeerSummary struct {
	Direct  int `json:"direct"`
	Relayed int `json:"relayed"`
	Wanted  int `json:"wanted_not_connected"`
}

// SetSummary is a backup set with its folders as label and hash.
type SetSummary struct {
	Name     string   `json:"name"`
	Folders  []string `json:"folders"` // "name:hash8"
	Excludes int      `json:"excludes"`
	LastAt   string   `json:"last_at,omitempty"`
	Files    int64    `json:"files,omitempty"`
	Bytes    int64    `json:"bytes,omitempty"`
	Skipped  int64    `json:"skipped,omitempty"`
	Partial  bool     `json:"partial,omitempty"`
	Error    string   `json:"error,omitempty"`
	Needs    []string `json:"needs,omitempty"`
	Running  bool     `json:"running,omitempty"`
}

// RestoreSummary is the weekly test's state.
type RestoreSummary struct {
	Every    string `json:"every"`
	Streak   int    `json:"weeks_in_a_row"`
	LastAt   string `json:"last_at,omitempty"`
	Sampled  int    `json:"sampled,omitempty"`
	Verified int    `json:"verified,omitempty"`
	Failed   int    `json:"failed,omitempty"`
}

// CleanupSummary is the last cleanup's counts.
type CleanupSummary struct {
	At         string `json:"at"`
	Reachable  int    `json:"reachable"`
	Listed     int    `json:"listed"`
	Garbage    int    `json:"garbage"`
	Tombstoned int    `json:"tombstoned"`
	Error      string `json:"error,omitempty"`
}

// DiskSummary is free and total space where it matters.
type DiskSummary struct {
	What  string `json:"what"` // "data directory" or a set's label
	Free  uint64 `json:"free_bytes"`
	Total uint64 `json:"total_bytes"`
}

// ErrorCount is one warning or error message and how often it appeared.
type ErrorCount struct {
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Count int    `json:"count"`
}

// Hash is the short stable hash a path appears under.
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

var (
	url      = regexp.MustCompile(`[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	ipv4     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?\b`)
	ipv6     = regexp.MustCompile(`\[[0-9A-Fa-f:.]+(?:%[A-Za-z0-9._-]+)?\](?::\d{1,5})?`)
	ipv6Bare = regexp.MustCompile(`[0-9A-Fa-f:.]{2,}(?:%[A-Za-z0-9._-]+)?`)
	hostPort = regexp.MustCompile(`\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+:\d{2,5}\b`)
)

// Redact replaces every URL, path and address in a line. Paths keep a
// hash so a reader can tell one from another and see the same one twice.
func Redact(s string) string {
	s = url.ReplaceAllStringFunc(s, func(u string) string {
		core := strings.TrimRight(u, ":,.;)")
		return "<url>" + u[len(core):]
	})
	s = redactPaths(s)
	s = ipv6.ReplaceAllString(s, "<addr>")
	s = ipv4.ReplaceAllString(s, "<addr>")
	s = hostPort.ReplaceAllString(s, "<addr>")
	return redactBareIPv6(s)
}

// redactPaths replaces each path with its hash. A folder or file name may
// hold spaces, commas, colons and brackets, so where a path ends is taken
// from where it stands, and when in doubt more is taken, not less:
//
//   - right after a quote, it runs to the closing quote;
//   - elsewhere inside a quoted value, or in plain text, it runs to the
//     first colon followed by a space (where an error's reason begins),
//     to the closing quote, or to the end of the line;
//   - as an unquoted key=value, which holds no space, it runs to the next
//     space.
func redactPaths(s string) string {
	var b strings.Builder
	quoted := false // inside a double-quoted value
	for i := 0; i < len(s); {
		c := s[i]
		if c == '"' && (i == 0 || s[i-1] != '\\') {
			quoted = !quoted
		}
		if c == '\n' {
			quoted = false
		}
		if !pathStart(s, i) {
			b.WriteByte(c)
			i++
			continue
		}
		var prev byte
		if i > 0 {
			prev = s[i-1]
		}
		end := len(s)
		if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
			end = i + n
		}
		switch {
		case prev == '"' || prev == '\'':
			if n := strings.IndexByte(s[i:end], prev); n >= 0 {
				end = i + n
			}
		case prev == '=' && !quoted:
			if n := strings.IndexAny(s[i:end], " \t"); n >= 0 {
				end = i + n
			}
		default:
			// The drive of a Windows path has a colon of its own, never
			// followed by a space.
			if n := strings.Index(s[i:end], ": "); n >= 0 {
				end = i + n
			}
			if n := strings.IndexByte(s[i:end], '"'); n >= 0 {
				end = i + n
			}
		}
		path := strings.TrimRight(s[i:end], " \t\\:,;)]")
		if path == "" {
			b.WriteByte(c)
			i++
			continue
		}
		b.WriteString("<path:" + Hash(path) + ">")
		i += len(path)
	}
	return b.String()
}

// pathStart reports whether a path begins at s[i]: a slash after the
// start, a space, a quote or one of =(,[ and before a name; a drive
// letter with :\ ; or the two backslashes of a network share.
func pathStart(s string, i int) bool {
	var prev byte = ' '
	if i > 0 {
		prev = s[i-1]
	}
	switch c := s[i]; {
	case c == '/':
		if !strings.ContainsRune(" \t\"'=(,[", rune(prev)) || i+1 >= len(s) {
			return false
		}
		return !strings.ContainsRune(" \t\n\"'/", rune(s[i+1]))
	case c == '\\':
		return i+1 < len(s) && s[i+1] == '\\' && prev != '\\' && !wordByte(prev)
	case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z'):
		return !wordByte(prev) && strings.HasPrefix(s[i+1:], `:\`)
	}
	return false
}

func wordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// redactBareIPv6 replaces an IPv6 address written without brackets. A run
// of hex digits and colons is one only when it parses as one, so a time
// of day or a version stays.
func redactBareIPv6(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range ipv6Bare.FindAllStringIndex(s, -1) {
		tok := s[m[0]:m[1]]
		core := strings.TrimRight(tok, ":.")
		if strings.Count(core, ":") < 2 || (m[0] > 0 && wordByte(s[m[0]-1])) || (m[0]+len(core) < len(s) && wordByte(s[m[0]+len(core)])) {
			continue
		}
		if a, err := netip.ParseAddr(core); err != nil || !a.Is6() {
			continue
		}
		b.WriteString(s[last:m[0]])
		b.WriteString("<addr>")
		last = m[0] + len(core)
	}
	b.WriteString(s[last:])
	return b.String()
}

// redactor is Redact for one bundle, which also knows this device's own
// folders: whatever of them a line still shows after the patterns is
// replaced as well, longest first.
type redactor struct{ known []string }

func newRedactor(in Input) redactor {
	seen := map[string]bool{}
	var r redactor
	add := func(p string) {
		p = strings.TrimRight(p, `/\`)
		if len(p) < 4 || seen[p] {
			return
		}
		seen[p] = true
		r.known = append(r.known, p)
	}
	add(in.DataDir)
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	if in.Jobs != nil {
		for _, set := range in.Jobs.Sets {
			for _, f := range set.Folders {
				add(f)
			}
		}
	}
	sort.Slice(r.known, func(i, j int) bool { return len(r.known[i]) > len(r.known[j]) })
	return r
}

func (r redactor) do(s string) string {
	s = Redact(s)
	for _, k := range r.known {
		if strings.Contains(s, k) {
			s = strings.ReplaceAll(s, k, "<path:"+Hash(k)+">")
		}
	}
	return s
}

// label is how a set's folder appears: the set's name and the hash.
func label(set, folder string) string { return set + ":" + Hash(folder) }

// Summarise builds the allowlisted projection.
func Summarise(in Input) Summary {
	s := Summary{GeneratedAt: in.Now, Version: in.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Go: runtime.Version(), Network: in.Network,
		Device: in.Device, Member: in.Member, Roles: in.Roles, AgentUp: in.AgentUp, Uptime: in.Uptime, Manifest: in.Manifest}
	red := newRedactor(in).do
	if in.Update != nil {
		u := *in.Update
		u.LastError = red(u.LastError)
		s.Update = &u
	}
	if in.Repair != nil {
		r := *in.Repair
		r.Running, r.Last = red(r.Running), red(r.Last)
		s.Repair = &r
	}
	if in.Service != nil {
		sv := *in.Service
		sv.Path, sv.Detail = red(sv.Path), red(sv.Detail)
		s.Service = &sv
	}
	if in.Membership != nil {
		m := *in.Membership
		m.LastError = red(m.LastError)
		s.Membership = &m
	}
	if t := in.Transport; t != nil {
		for _, h := range t.Hubs {
			s.Hubs = append(s.Hubs, HubSummary{Hub: h.Hub, Up: h.Up, Carrier: h.Carrier, Reconnects: h.Reconnects, LastError: red(h.LastError)})
		}
		p := &PeerSummary{}
		for _, ss := range t.Sessions {
			switch {
			case ss.Direct != "":
				p.Direct++
			case ss.Relay:
				p.Relayed++
			default:
				p.Wanted++
			}
		}
		s.Peers = p
	}
	if j := in.Jobs; j != nil {
		for _, set := range j.Sets {
			ss := SetSummary{Name: set.Name, Excludes: len(set.Excludes)}
			for _, f := range set.Folders {
				ss.Folders = append(ss.Folders, label(set.Name, f))
			}
			for _, r := range j.Runs {
				if r.Set != set.Name {
					continue
				}
				ss.Running = r.Running
				ss.Error = red(r.LastErr)
				for _, n := range r.Needs {
					ss.Needs = append(ss.Needs, red(n))
				}
				if r.Last != nil {
					ss.LastAt = r.LastAt.UTC().Format(time.RFC3339)
					ss.Files, ss.Bytes, ss.Skipped, ss.Partial = r.Last.Snapshot.Files, r.Last.Snapshot.Bytes, r.Last.Snapshot.Skipped, r.Last.Snapshot.Partial
				}
			}
			s.Sets = append(s.Sets, ss)
			for _, f := range set.Folders {
				if free, total, err := diskfree.Free(f); err == nil {
					s.Disks = append(s.Disks, DiskSummary{What: label(set.Name, f), Free: free, Total: total})
				}
			}
		}
		rt := &RestoreSummary{Every: j.VerifyEvery, Streak: j.VerifyStreak}
		if v := j.LastVerify; v != nil {
			rt.LastAt, rt.Sampled, rt.Verified, rt.Failed = v.At.UTC().Format(time.RFC3339), v.Sampled, v.Verified, len(v.Failed)
		}
		s.RestoreTest = rt
		if c := j.LastCleanup; c != nil {
			s.Cleanup = &CleanupSummary{At: c.At.UTC().Format(time.RFC3339), Reachable: c.Reachable, Listed: c.Listed, Garbage: c.Garbage, Tombstoned: c.Tombstoned, Error: red(c.Error)}
		}
	}
	if in.DataDir != "" {
		if free, total, err := diskfree.Free(in.DataDir); err == nil {
			s.Disks = append([]DiskSummary{{What: "data directory", Free: free, Total: total}}, s.Disks...)
		}
	}
	s.Errors = countErrors(in.Lines, in.LogText, red)
	return s
}

var textLine = regexp.MustCompile(`level=(WARN|ERROR)\s+msg=("(?:[^"\\]|\\.)*"|\S+)`)

// countErrors tallies warnings and errors by message, from the agent's
// lines and from the log text, newest sources first.
func countErrors(lines []logbuf.Line, text string, red func(string) string) []ErrorCount {
	counts := map[[2]string]int{}
	for _, l := range lines {
		if l.Level == "WARN" || l.Level == "ERROR" {
			counts[[2]string{l.Level, red(l.Msg)}]++
		}
	}
	if len(lines) == 0 {
		for _, line := range strings.Split(text, "\n") {
			if m := textLine.FindStringSubmatch(line); m != nil {
				msg := strings.Trim(m[2], `"`)
				counts[[2]string{m[1], red(msg)}]++
			}
		}
	}
	var out []ErrorCount
	for k, n := range counts {
		out = append(out, ErrorCount{Level: k[0], Msg: k[1], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Msg < out[j].Msg
	})
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// Write writes the bundle as a zip: summary.txt for people, summary.json
// for tools, and log.txt with every path and address redacted.
func Write(w io.Writer, in Input) error {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	s := Summarise(in)
	z := zip.NewWriter(w)
	add := func(name string, body []byte) error {
		f, err := z.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: in.Now})
		if err != nil {
			return err
		}
		_, err = f.Write(body)
		return err
	}
	if err := add("summary.txt", []byte(text(s))); err != nil {
		return err
	}
	js, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := add("summary.json", js); err != nil {
		return err
	}
	red := newRedactor(in).do
	var log strings.Builder
	for _, l := range in.Lines {
		fmt.Fprintf(&log, "%s %-5s %s %s\n", l.Time.UTC().Format(time.RFC3339), l.Level, red(l.Msg), clip(red(l.Attrs)))
	}
	if in.LogText != "" {
		if log.Len() > 0 {
			log.WriteString("\n---- service log ----\n")
		}
		for _, line := range strings.Split(in.LogText, "\n") {
			log.WriteString(clip(red(line)))
			log.WriteByte('\n')
		}
	}
	if err := add("log.txt", []byte(log.String())); err != nil {
		return err
	}
	return z.Close()
}

// maxLine bounds a log line in the bundle: the agent's minute-by-minute
// status snapshots run to kilobytes each and say what the summary says.
const maxLine = 400

func clip(s string) string {
	if len(s) <= maxLine {
		return s
	}
	return s[:maxLine] + " [...]"
}

// text is summary.txt.
func text(s Summary) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("RedundaNet diagnostics, generated %s\n", s.GeneratedAt.UTC().Format(time.RFC3339))
	p("version %s on %s, %s\n", s.Version, s.Platform, s.Go)
	if s.Network != "" {
		p("network %s, device %s, member %s, roles %s\n", s.Network, s.Device, s.Member, strings.Join(s.Roles, " "))
	}
	if s.AgentUp {
		p("agent: running, up %s\n", s.Uptime)
	} else {
		p("agent: not answering on the local API; the rest comes from disk\n")
	}
	if sv := s.Service; sv != nil {
		p("service: installed %v, running %v %s\n", sv.Installed, sv.Running, sv.Detail)
	}
	if m := s.Manifest; m != nil {
		p("manifest on disk: revision %d, %s, %d hubs, %d members, %d storage devices, signed %s\n", m.Revision, m.Encoding, m.Hubs, m.Members, m.Storage, m.SignedAt.UTC().Format("2006-01-02"))
	}
	if m := s.Membership; m != nil {
		p("manifest applied: revision %d, signed %s, expired %v, last fetch %s %s\n", m.Revision, m.SignedAt.UTC().Format("2006-01-02"), m.Expired, m.LastFetch.UTC().Format(time.RFC3339), m.LastError)
	}
	if u := s.Update; u != nil {
		p("update: running %s on %s, available %q, last check %s %s, failed %v\n", u.Running, u.Channel, u.Available, u.LastCheck.UTC().Format(time.RFC3339), u.LastError, u.Failed)
	}
	for _, h := range s.Hubs {
		p("hub %s: up %v, %s, %d reconnects %s\n", h.Hub, h.Up, h.Carrier, h.Reconnects, h.LastError)
	}
	if pe := s.Peers; pe != nil {
		p("peers: %d direct, %d relayed, %d wanted and not connected\n", pe.Direct, pe.Relayed, pe.Wanted)
	}
	for _, set := range s.Sets {
		p("set %s: folders %s, %d excludes", set.Name, strings.Join(set.Folders, " "), set.Excludes)
		if set.LastAt != "" {
			p(", last run %s: %d files, %d bytes, %d skipped, partial %v", set.LastAt, set.Files, set.Bytes, set.Skipped, set.Partial)
		}
		if set.Error != "" {
			p(", error: %s", set.Error)
		}
		for _, n := range set.Needs {
			p(", needs: %s", n)
		}
		p("\n")
	}
	if r := s.RestoreTest; r != nil {
		p("restore tests: every %s, %d weeks in a row passing", r.Every, r.Streak)
		if r.LastAt != "" {
			p(", last %s: %d of %d read back, %d failed", r.LastAt, r.Verified, r.Sampled, r.Failed)
		}
		p("\n")
	}
	if c := s.Cleanup; c != nil {
		p("cleanup: %s, %d reachable, %d listed, %d garbage, %d tombstoned %s\n", c.At, c.Reachable, c.Listed, c.Garbage, c.Tombstoned, c.Error)
	}
	if r := s.Repair; r != nil {
		p("repair worker: %d done, %d failed, %d released, last: %s\n", r.Done, r.Failed, r.Released, r.Last)
	}
	for _, d := range s.Disks {
		p("disk %s: %d MB free of %d MB\n", d.What, d.Free>>20, d.Total>>20)
	}
	if len(s.Errors) > 0 {
		p("warnings and errors:\n")
		for _, e := range s.Errors {
			p("  %5d  %-5s %s\n", e.Count, e.Level, e.Msg)
		}
	}
	return b.String()
}

// TailFile reads the last limit bytes of a file, whole lines only.
func TailFile(path string, limit int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := st.Size() - limit
	if start < 0 {
		start = 0
	}
	b := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
		return "", err
	}
	if start > 0 {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return string(b), nil
}
