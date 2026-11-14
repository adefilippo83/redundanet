package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
)

// A member's view of another device of theirs. This device asks the other
// one over the mesh for its status and draws the view itself, inside its
// own page and session: nothing of the other device's page is served
// here, and the other device answers a status request and three named
// actions, nothing else (shareproto.Remote).

// RemoteView is what a device tells another device of its member.
type RemoteView struct {
	Snapshot Snapshot      `json:"snapshot"`
	Logs     []logbuf.Line `json:"logs,omitempty"`
}

// The actions a member may start on another device of theirs. None takes
// a path, a setting or a secret.
const (
	RemoteBackup      = "backup-run"
	RemoteVerify      = "backup-verify"
	RemoteUpdateCheck = "update-check"
)

// RemoteLogLines is how many log lines a remote view carries.
const RemoteLogLines = 60

// remotePath is the view's path; the device is the query's d.
const remotePath = "/device"

// remoteTTL is how long a fetched view is shown again without asking: the
// page refreshes itself every few seconds.
const remoteTTL = 8 * time.Second

// remoteTimeout bounds one question to the other device.
const remoteTimeout = 20 * time.Second

// remoteData is the other device as the view renders it.
type remoteData struct {
	Key   string
	Name  string
	Err   string    // why there is nothing to show
	Page  *pageData // the other device's own snapshot, read like this one's
	Logs  []logbuf.Line
	Asked time.Time
}

type remoteCache struct {
	mu sync.Mutex
	at map[string]remoteEntry
}

type remoteEntry struct {
	view *RemoteView
	when time.Time
}

func (c *remoteCache) get(key string) (*RemoteView, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.at[key]
	if !ok || time.Since(e.when) > remoteTTL {
		return nil, time.Time{}, false
	}
	return e.view, e.when, true
}

func (c *remoteCache) put(key string, v *RemoteView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil || len(c.at) > 64 {
		c.at = map[string]remoteEntry{}
	}
	c.at[key] = remoteEntry{view: v, when: time.Now()}
}

func (c *remoteCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.at, key)
}

// remoteBack is the remote view's address for a device key, as a form's
// "back" names it, and whether the text is one: the key must parse, so
// nothing else can be made the target of a redirect.
func remoteBack(back string) (string, bool) {
	q, ok := strings.CutPrefix(back, remotePath+"?d=")
	if !ok {
		return "", false
	}
	k, err := identity.ParseKey(q)
	if err != nil {
		return "", false
	}
	return remotePath + "?d=" + url.QueryEscape(k.String()), true
}

// deviceName is what this device calls another: the name it was paired
// or listed under, or the start of its key.
func deviceName(snap Snapshot, key string) string {
	for _, d := range snap.Devices {
		if d.Key == key && d.Name != "" {
			return d.Name
		}
	}
	return short(key)
}

func (s *Server) remotePage(w http.ResponseWriter, r *http.Request) {
	l := tr(langOf(r))
	sess, ok := s.auth.sessionOf(r)
	if !ok || s.rt() == nil {
		// Without a session, or before the device has an identity, the
		// overview says what to do.
		http.Redirect(w, r, "./", http.StatusSeeOther)
		return
	}
	k, err := identity.ParseKey(r.URL.Query().Get("d"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	key := k.String()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	f, _ := s.takeFlash(r.Header.Get(liveHeader) != "")
	local := s.snapshot()
	back, _ := remoteBack(remotePath + "?d=" + key)
	data := buildPage(local, view{Path: back, Name: "remote"}, l)
	data.Gone = goneText(l, s.cfg.DataDir)
	data.CSRF, data.Flash = sess.csrf, f
	rd := &remoteData{Key: key, Name: deviceName(local, key)}
	rv, when, cached := s.remote.get(key)
	if !cached {
		if s.actions().RemoteView == nil {
			err = fmt.Errorf("this agent cannot reach other devices")
		} else {
			ctx, cancel := context.WithTimeout(r.Context(), remoteTimeout)
			rv, err = s.actions().RemoteView(ctx, key)
			cancel()
		}
		if err == nil {
			s.remote.put(key, rv)
			when = time.Now()
		}
	}
	if err != nil {
		rd.Err = err.Error()
	} else {
		p := buildPage(rv.Snapshot, view{Name: "remote"}, l)
		rd.Page, rd.Logs, rd.Asked = &p, rv.Logs, when
	}
	data.Remote = rd
	if err := s.tmpl.ExecuteTemplate(w, "page", data); err != nil {
		s.log.Error("page", "err", err)
	}
}

// remoteAct is the page action behind the remote view's buttons.
func (s *Server) remoteAct(r *http.Request, l tr) (string, bool) {
	k, err := identity.ParseKey(r.FormValue("device"))
	if err != nil || s.actions().RemoteAct == nil {
		return "", true
	}
	act := r.FormValue("act")
	if act != RemoteBackup && act != RemoteVerify && act != RemoteUpdateCheck {
		return "", true
	}
	ctx, cancel := context.WithTimeout(r.Context(), remoteTimeout)
	defer cancel()
	if err := s.actions().RemoteAct(ctx, k.String(), act); err != nil {
		return l.T("msg.remote.failed", err.Error()), true
	}
	s.remote.drop(k.String())
	return l.T("msg.remote.requested", deviceName(s.snapshot(), k.String())), false
}
