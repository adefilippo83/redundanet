// Package api is the agent's local HTTP API and web page: served on
// 127.0.0.1 only, used by the CLI with a bearer token and by the browser
// with a session that `redundanet open` starts. No GET changes anything.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/diag"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/service"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
	"github.com/adefilippo83/redundanet/agent/internal/transport"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

// Snapshot is everything the page and `redundanet status` show.
type Snapshot struct {
	Version    string             `json:"version"`
	Uptime     string             `json:"uptime"`
	DataDir    string             `json:"data_dir"`
	Network    string             `json:"network"`
	Device     string             `json:"device"`
	Member     string             `json:"member"`
	MemberCode string             `json:"member_code"`
	Admission  string             `json:"admission,omitempty"` // "listed" once the manifest names the member, "waiting" before
	Roles      []string           `json:"roles"`
	Setup      *SetupStatus       `json:"setup,omitempty"` // an agent without an identity yet: where the wizard stands
	Transport  *transport.Status  `json:"transport,omitempty"`
	Membership *membership.Status `json:"membership,omitempty"`
	Update     *update.Status     `json:"update,omitempty"`
	Jobs       *jobs.Status       `json:"jobs,omitempty"`
	Repair     *repair.Status     `json:"repair,omitempty"` // a storage node's repair worker
	Service    *service.Status    `json:"service,omitempty"`
	Placement  *Placement         `json:"placement,omitempty"` // how the network spreads an object, from the manifest
	Devices    []Device           `json:"devices,omitempty"`   // the devices this one paired, from its record
	// JoinURL is the join form filled in from this device, when there is
	// something to ask for: membership, or a paired storage node the
	// manifest does not list (JoinDevice names it). For the page only.
	JoinURL    string    `json:"-"`
	JoinDevice string    `json:"-"`
	Generated  time.Time `json:"generated"`
}

// Device is a device this one paired for its member.
type Device struct {
	Key      string    `json:"device"`
	Name     string    `json:"name,omitempty"`
	Roles    []string  `json:"roles"`
	PairedAt time.Time `json:"paired_at"`
}

// Placement is the network's encoding as the page explains it: an object
// is split into N shares on distinct storage devices and any K rebuild it.
type Placement struct {
	K       int `json:"k"`
	N       int `json:"n"`
	Servers int `json:"servers"` // storage devices the manifest lists
}

// Actions are the state changes the API can trigger.
type Actions struct {
	CheckUpdate func(ctx context.Context) (string, error) // returns the available version, "" for none
	ApplyUpdate func(ctx context.Context) error
	RetryUpdate func(ctx context.Context, version string) error // forgets a failed probation
	// SetChannel chooses the update channel on this device.
	SetChannel func(channel string) error
	// Revoke applies a member's signed device revocation here and sends it
	// to every storage server and hub: the direct path of "Losing a device".
	Revoke func(ctx context.Context, r *identity.SignedRevocation) (RevokeReport, error)
	// PairDevice certifies a new device of the member from the recovery
	// phrase and the code that device shows, and hands it its identity.
	// The phrase is used for this and kept nowhere.
	PairDevice func(ctx context.Context, code, phrase string, storage bool) (Device, error)
}

// RevokeReport is how far a revocation got right away; the signed
// manifest carries it to the rest.
type RevokeReport struct {
	Servers      int `json:"servers"`       // storage servers that took it
	ServersTotal int `json:"servers_total"` // storage servers in the manifest
	Hubs         int `json:"hubs"`          // hubs that took it
	HubsTotal    int `json:"hubs_total"`
}

// Config wires the server. With Snapshot set the server starts with its
// runtime; without it the server starts in setup mode, serving the wizard
// that makes the device's identity, until Attach hands it the runtime.
type Config struct {
	Addr     string // 127.0.0.1:4470 by default
	DataDir  string
	Version  string // shown in setup mode, before there is a snapshot to take it from
	Snapshot func() Snapshot
	Actions  Actions
	Jobs     *jobs.Runner // backup, restore test and restore; nil on a hub
	Logs     *logbuf.Buffer
	Log      *slog.Logger
	Setup    *SetupActions // the wizard's actions; nil once the device has an identity
}

// Runtime is what the agent attaches once it has an identity and runs: the
// snapshot, the actions, the jobs and the log buffer the page and the API
// serve from.
type Runtime struct {
	Snapshot func() Snapshot
	Actions  Actions
	Jobs     *jobs.Runner
	Logs     *logbuf.Buffer
}

// SetupActions is the wizard behind the page before the device has an
// identity. Every secret stays on the agent's side: the page shows the
// phrase once and sends words back, never the seed.
type SetupActions struct {
	// Begin draws a new member's seed and returns the twelve words, to show
	// once, and the three one-based positions asked back.
	Begin func(storage bool) (words []string, check []int, err error)
	// Confirm checks the words at the positions Begin gave and, when they
	// match, writes the identity.
	Confirm func(answers []string) error
	// Restore puts an existing member on this device from the typed phrase.
	Restore func(phrase string, storage bool) error
	// Pair starts waiting for another device of the member and returns the
	// code to show; a second call while waiting returns the same code.
	Pair func() (code string, err error)
	// Cancel drops a pending phrase or pairing and goes back to the start.
	Cancel func()
	// Status is where the wizard stands.
	Status func() SetupStatus
}

// SetupStatus is the wizard's state, in the snapshot of an agent that has
// no identity yet and on the setup page.
type SetupStatus struct {
	Phase   string `json:"phase"` // choose, confirm, pairing, done
	Check   []int  `json:"check,omitempty"`
	Storage bool   `json:"storage,omitempty"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Server is the running API.
type Server struct {
	cfg     Config
	log     *slog.Logger
	auth    *auth
	ln      net.Listener
	srv     *http.Server
	tmpl    *template.Template
	started time.Time
	runtime atomic.Pointer[Runtime]

	mu        sync.Mutex
	lastFlash *flash   // the outcome of the last action, shown once on the page
	lastWords []string // a new member's phrase, shown once on the page after Begin
}

//go:embed templates/page.html templates/setup.html
var templates embed.FS

// The page's stylesheet and script, served from the agent itself: the
// content policy lets the page load nothing from anywhere else.
//
//go:embed static/app.css static/app.js
var static embed.FS

// contentPolicy is what the browser may do on the page: styles and one
// script from the agent's own origin, requests back to it, forms posted to
// it, and nothing inline, framed or from another origin.
const contentPolicy = "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// pairTimeout bounds how long the page waits for the new device to answer.
const pairTimeout = 90 * time.Second

// liveHeader marks the page's own refresh of a view: it reads the same
// view again and must not use up the message of the last action.
const liveHeader = "X-RedundaNet-Live"

// Attach hands the server its runtime: from here the page and the API
// serve the running agent instead of the wizard. The browser session made
// during setup stays valid.
func (s *Server) Attach(rt Runtime) {
	s.runtime.Store(&rt)
	s.log.Info("local api serving the agent")
}

func (s *Server) rt() *Runtime { return s.runtime.Load() }

// snapshot is the running agent's, or, in setup mode, the little there is
// to say: version, uptime and where the wizard stands.
func (s *Server) snapshot() Snapshot {
	if rt := s.rt(); rt != nil {
		return rt.Snapshot()
	}
	snap := Snapshot{Version: s.cfg.Version, Uptime: time.Since(s.started).Round(time.Second).String(), DataDir: s.cfg.DataDir, Generated: time.Now()}
	if s.cfg.Setup != nil && s.cfg.Setup.Status != nil {
		st := s.cfg.Setup.Status()
		snap.Setup = &st
	}
	return snap
}

func (s *Server) jobs() *jobs.Runner {
	if rt := s.rt(); rt != nil {
		return rt.Jobs
	}
	return nil
}

func (s *Server) actions() Actions {
	if rt := s.rt(); rt != nil {
		return rt.Actions
	}
	return Actions{}
}

func (s *Server) logs() *logbuf.Buffer {
	if rt := s.rt(); rt != nil {
		return rt.Logs
	}
	return nil
}

// Start binds and serves until ctx ends.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:4470"
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || !(host == "127.0.0.1" || host == "localhost" || host == "::1") {
		return nil, fmt.Errorf("api: the address must be on loopback, got %q", cfg.Addr)
	}
	token, err := loadToken(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: cfg.Log.With("component", "api"), auth: newAuth(token), tmpl: tmpl, started: time.Now()}
	if cfg.Snapshot != nil {
		s.runtime.Store(&Runtime{Snapshot: cfg.Snapshot, Actions: cfg.Actions, Jobs: cfg.Jobs, Logs: cfg.Logs})
	} else if cfg.Setup == nil {
		return nil, errors.New("api: a server needs a snapshot to serve or setup actions to offer")
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	s.ln = ln
	if err := os.WriteFile(filepath.Join(cfg.DataDir, addrFile), []byte(ln.Addr().String()+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.page)
	mux.HandleFunc("GET /static/{file}", s.static)
	mux.HandleFunc("GET /diagnostics.zip", s.diagnostics)
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("POST /action/{name}", s.pageAction)
	mux.HandleFunc("POST /setup/{name}", s.setupAction)
	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /api/logs", s.apiLogs)
	mux.HandleFunc("POST /api/login-link", s.apiLoginLink)
	mux.HandleFunc("POST /api/update/check", s.apiUpdateCheck)
	mux.HandleFunc("POST /api/update/apply", s.apiUpdateApply)
	mux.HandleFunc("POST /api/update/retry", s.apiUpdateRetry)
	mux.HandleFunc("GET /api/backups", s.apiBackups)
	mux.HandleFunc("POST /api/backup/add", s.apiBackupAdd)
	mux.HandleFunc("POST /api/backup/remove", s.apiBackupRemove)
	mux.HandleFunc("POST /api/backup/run", s.apiBackupRun)
	mux.HandleFunc("POST /api/backup/verify", s.apiBackupVerify)
	mux.HandleFunc("POST /api/backup/cleanup", s.apiBackupCleanup)
	mux.HandleFunc("POST /api/backup/rebalance", s.apiBackupRebalance)
	mux.HandleFunc("POST /api/backup/limit", s.apiBackupLimit)
	mux.HandleFunc("POST /api/revoke", s.apiRevoke)
	mux.HandleFunc("POST /api/settings", s.apiSettings)
	mux.HandleFunc("POST /api/backup/excludes", s.apiBackupExcludes)
	mux.HandleFunc("POST /api/update/channel", s.apiUpdateChannel)
	mux.HandleFunc("GET /api/restore/list", s.apiRestoreList)
	mux.HandleFunc("GET /api/restore/browse", s.apiRestoreBrowse)
	mux.HandleFunc("POST /api/restore/start", s.apiRestoreStart)
	s.srv = &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("api server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
	}()
	s.log.Info("local api up", "addr", ln.Addr())
	return s, nil
}

// Addr is where the server listens.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// guard is the Host and Origin check on every request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostOK(r) {
			http.Error(w, "the local API answers only on 127.0.0.1", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !originOK(r) {
			http.Error(w, "cross-origin requests are refused", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", contentPolicy)
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requireAPI admits the CLI token or a browser session; a session also
// needs the CSRF token on anything but GET.
func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.bearer(r) {
		return true
	}
	if sess, ok := s.auth.sessionOf(r); ok {
		if r.Method == http.MethodGet || r.Header.Get(headerCSRF) == sess.csrf {
			return true
		}
		http.Error(w, "missing or wrong CSRF token", http.StatusForbidden)
		return false
	}
	http.Error(w, "unauthorized: use the bearer token from api.token, or open the page with `redundanet open`", http.StatusUnauthorized)
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeJSON(w, s.snapshot())
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	n := 200
	if q := r.URL.Query().Get("n"); q != "" {
		_, _ = fmt.Sscanf(q, "%d", &n)
	}
	var lines []logbuf.Line
	if s.logs() != nil {
		lines = s.logs().Last(n)
	}
	writeJSON(w, lines)
}

// apiLoginLink mints the one-time link `redundanet open` prints: only the
// CLI token may ask for one.
func (s *Server) apiLoginLink(w http.ResponseWriter, r *http.Request) {
	if !s.auth.bearer(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	t := s.auth.newLoginLink()
	if t == "" {
		http.Error(w, "too many unused login links", http.StatusTooManyRequests)
		return
	}
	writeJSON(w, map[string]string{"url": "http://" + s.Addr() + "/login?t=" + t})
}

func (s *Server) apiRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if s.actions().Revoke == nil {
		http.Error(w, "revocation is not configured", http.StatusNotImplemented)
		return
	}
	var in struct {
		Record []byte `json:"record"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad record: "+err.Error(), http.StatusBadRequest)
		return
	}
	rv, err := identity.DecodeRevocation(in.Record)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := s.actions().Revoke(r.Context(), rv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if s.actions().CheckUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	v, err := s.actions().CheckUpdate(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"available": v})
}

func (s *Server) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if s.actions().ApplyUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	if err := s.actions().ApplyUpdate(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"result": "update started; the agent restarts when the new version passes probation"})
}

// login redeems a one-time link for a session cookie and shows the page.
func (s *Server) apiUpdateRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if s.actions().RetryUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	v := r.FormValue("version")
	if v == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	if err := s.actions().RetryUpdate(r.Context(), v); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"result": v + " may be installed again at the next check"})
}

func (s *Server) needJobs(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAPI(w, r) {
		return false
	}
	if s.jobs() == nil {
		http.Error(w, "this device runs no backup jobs", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) apiBackups(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	writeJSON(w, s.jobs().Status())
}

func (s *Server) apiBackupAdd(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var set backup.Set
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&set); err != nil {
		http.Error(w, "bad set: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.jobs().AddSet(set); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "set " + set.Name + " saved"})
}

// Limits is the bandwidth caps as the API carries them.
type Limits struct {
	UploadKBps   int `json:"upload_kbps"`
	DownloadKBps int `json:"download_kbps"`
}

func (s *Server) apiBackupLimit(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var l Limits
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&l); err != nil {
		http.Error(w, "bad limits: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.jobs().SetLimits(l.UploadKBps, l.DownloadKBps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "limits saved"})
}

// Settings is what the settings endpoint changes: each field present is
// applied, the rest stays.
type Settings struct {
	Every        *string             `json:"every,omitempty"`
	VerifyEvery  *string             `json:"verify_every,omitempty"`
	CleanupEvery *string             `json:"cleanup_every,omitempty"`
	Retention    *snapshot.Retention `json:"retention,omitempty"`
	UploadKBps   *int                `json:"upload_kbps,omitempty"`
	DownloadKBps *int                `json:"download_kbps,omitempty"`
}

// applySettings changes what in is set; the message says what changed.
func (s *Server) applySettings(in Settings) (string, error) {
	j := s.jobs()
	st := j.Status()
	var changed []string
	if in.Every != nil || in.VerifyEvery != nil || in.CleanupEvery != nil {
		every, verify, cleanup, err := durations(st, in)
		if err != nil {
			return "", err
		}
		if err := j.SetSchedule(every, verify, cleanup); err != nil {
			return "", err
		}
		changed = append(changed, "schedule")
	}
	if in.Retention != nil {
		if err := j.SetRetention(*in.Retention); err != nil {
			return "", err
		}
		changed = append(changed, "retention")
	}
	if in.UploadKBps != nil || in.DownloadKBps != nil {
		up, down := st.UploadKBps, st.DownloadKBps
		if in.UploadKBps != nil {
			up = *in.UploadKBps
		}
		if in.DownloadKBps != nil {
			down = *in.DownloadKBps
		}
		if err := j.SetLimits(up, down); err != nil {
			return "", err
		}
		changed = append(changed, "bandwidth")
	}
	if len(changed) == 0 {
		return "nothing to change", nil
	}
	return "saved: " + strings.Join(changed, ", "), nil
}

func durations(st jobs.Status, in Settings) (every, verify, cleanup time.Duration, err error) {
	pick := func(cur string, p *string) (time.Duration, error) {
		if p != nil {
			cur = *p
		}
		return time.ParseDuration(cur)
	}
	if every, err = pick(st.Every, in.Every); err != nil {
		return
	}
	if verify, err = pick(st.VerifyEvery, in.VerifyEvery); err != nil {
		return
	}
	cleanup, err = pick(st.CleanupEvery, in.CleanupEvery)
	return
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var in Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	msg, err := s.applySettings(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": msg})
}

func (s *Server) apiBackupExcludes(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var in struct {
		Set      string   `json:"set"`
		Excludes []string `json:"excludes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "bad excludes: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.jobs().SetExcludes(in.Set, in.Excludes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "excludes of " + in.Set + " saved"})
}

func (s *Server) apiUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if s.actions().SetChannel == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	var in struct {
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
		http.Error(w, "bad channel: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.actions().SetChannel(in.Channel); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "channel " + in.Channel})
}

func (s *Server) apiBackupRemove(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	if err := s.jobs().RemoveSet(r.FormValue("set")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "set removed"})
}

func (s *Server) apiBackupRun(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	s.jobs().RunNow(r.FormValue("set"))
	writeJSON(w, map[string]string{"result": "backup requested"})
}

func (s *Server) apiBackupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	s.jobs().RunNow("verify")
	writeJSON(w, map[string]string{"result": "restore test requested"})
}

// CleanupRequest asks for the owner's cleanup: a dry run counts and
// deletes nothing; force lifts the bound on how much one run may delete.
type CleanupRequest struct {
	DryRun bool `json:"dry_run"`
	Force  bool `json:"force"`
}

func (s *Server) apiBackupCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var req CleanupRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	rep, err := s.jobs().RunCleanup(r.Context(), req.DryRun, req.Force)
	if err != nil && rep == nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) apiBackupRebalance(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	st, err := s.jobs().RunRebalance(r.Context())
	if err != nil && st == nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, st)
}

func (s *Server) apiRestoreList(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	l, err := s.jobs().List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, l)
}

func (s *Server) apiRestoreBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	dev, err := identity.ParseKey(r.FormValue("device"))
	if err != nil {
		http.Error(w, "bad device key", http.StatusBadRequest)
		return
	}
	entry, dir, err := s.jobs().Browse(r.Context(), dev, r.FormValue("snapshot"), r.FormValue("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"entry": entry, "directory": dir})
}

// RestoreRequest starts a restore.
type RestoreRequest struct {
	Device    string `json:"device"`
	Snapshot  string `json:"snapshot"`
	Path      string `json:"path"`
	Target    string `json:"target"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) apiRestoreStart(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	var req RestoreRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	dev, err := identity.ParseKey(req.Device)
	if err != nil {
		http.Error(w, "bad device key", http.StatusBadRequest)
		return
	}
	if req.Target == "" {
		http.Error(w, "a target directory is required", http.StatusBadRequest)
		return
	}
	if err := s.jobs().StartRestore(dev, req.Snapshot, req.Path, req.Target, req.Overwrite); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"result": "restore started"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	id, ok := s.auth.redeem(r.URL.Query().Get("t"))
	if !ok {
		http.Error(w, "this link has expired or was used already; run `redundanet open` again", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func parseTemplates() (*template.Template, error) {
	return template.New("page").Funcs(template.FuncMap{
		"short":     short,
		"inc":       func(i int) int { return i + 1 },
		"lastIndex": func(v []transport.Event) int { return len(v) - 1 },
	}).ParseFS(templates, "templates/page.html", "templates/setup.html")
}

// static serves the page's stylesheet and script. They hold no secret, so
// the page that says how to open a session can use them too.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	b, err := static.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	_, _ = w.Write(b)
}

// takeFlash is the message of the last action, once.
func (s *Server) takeFlash(keep bool) (*flash, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, words := s.lastFlash, s.lastWords
	if !keep {
		s.lastFlash, s.lastWords = nil, nil
	}
	return f, words
}

func (s *Server) setFlash(text string, bad bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastFlash = nil
	if text != "" {
		s.lastFlash = &flash{Text: text, Bad: bad}
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	v, known := viewAt(r.URL.Path)
	if !known {
		http.NotFound(w, r)
		return
	}
	l := tr(langOf(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	sess, ok := s.auth.sessionOf(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>RedundaNet</title><link rel=\"stylesheet\" href=\"/static/app.css\"><main class=\"narrow\"><section class=\"card\"><h1>RedundaNet</h1><p>%s</p><p class=\"code\">redundanet open</p></section></main>", template.HTMLEscapeString(l.T("session.open")))
		return
	}
	// The page's own refresh reads a view without using up what is shown
	// once: the message of the last action, a new member's phrase.
	f, words := s.takeFlash(r.Header.Get(liveHeader) != "")
	if s.rt() == nil {
		s.setupPage(w, sess, l, f, words)
		return
	}
	data := buildPage(s.snapshot(), v, l)
	data.CSRF, data.Flash = sess.csrf, f
	// The restore view reads the network; its own refresh, which only
	// follows a running restore, does not.
	if v.Name == "restore" {
		if r.Header.Get(liveHeader) == "" {
			data.Restore = s.restoreView(r, data.Device)
		} else {
			data.Restore = &restoreData{}
		}
	}
	if s.logs() != nil && v.Name == "advanced" {
		data.Logs = s.logs().Last(60)
	}
	if err := s.tmpl.ExecuteTemplate(w, "page", data); err != nil {
		s.log.Error("page", "err", err)
	}
}

// setupData is what the wizard's page renders.
type setupData struct {
	SetupStatus
	tr
	Lang    string
	Langs   []Language
	Back    string
	Version string
	CSRF    string
	Flash   *flash
	Words   []string // present only on the page right after Begin
}

// setupPage is the wizard: the device has no identity yet. The phrase is
// rendered once, from the one-shot store Begin filled; a reload shows the
// confirmation step without it, as the RFC wants the phrase shown once.
func (s *Server) setupPage(w http.ResponseWriter, sess session, l tr, f *flash, words []string) {
	data := setupData{tr: l, Lang: string(l), Langs: Languages, Back: "/", Version: s.cfg.Version, CSRF: sess.csrf, Flash: f, Words: words}
	if s.cfg.Setup != nil && s.cfg.Setup.Status != nil {
		data.SetupStatus = s.cfg.Setup.Status()
	}
	if data.Phase == "" {
		data.Phase = "choose"
	}
	if err := s.tmpl.ExecuteTemplate(w, "setup", data); err != nil {
		s.log.Error("setup page", "err", err)
	}
}

// setupAction handles the wizard's forms, with the page's session and
// CSRF token, then returns to the page with a message. Once the runtime is
// attached the wizard is over and its forms are gone.
func (s *Server) setupAction(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auth.sessionOf(r)
	if !ok {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if r.FormValue(formCSRF) != sess.csrf {
		http.Error(w, "missing or wrong CSRF token", http.StatusForbidden)
		return
	}
	if s.rt() != nil || s.cfg.Setup == nil {
		http.Error(w, "this device has its identity already", http.StatusConflict)
		return
	}
	l := tr(langOf(r))
	var msg string
	bad := true
	switch r.PathValue("name") {
	case "begin":
		words, _, err := s.cfg.Setup.Begin(r.FormValue("storage") == "on")
		if err != nil {
			msg = l.T("msg.setup.begin", err.Error())
			break
		}
		s.mu.Lock()
		s.lastWords = words
		s.mu.Unlock()
	case "confirm":
		if err := s.cfg.Setup.Confirm([]string{r.FormValue("w1"), r.FormValue("w2"), r.FormValue("w3")}); err != nil {
			msg = err.Error()
		} else {
			msg, bad = l.T("msg.setup.written"), false
		}
	case "restore":
		if err := s.cfg.Setup.Restore(r.FormValue("phrase"), r.FormValue("storage") == "on"); err != nil {
			msg = err.Error()
		} else {
			msg, bad = l.T("msg.setup.restored"), false
		}
	case "pair":
		if _, err := s.cfg.Setup.Pair(); err != nil {
			msg = l.T("msg.setup.pair", err.Error())
		}
	case "cancel":
		s.cfg.Setup.Cancel()
	default:
		http.NotFound(w, r)
		return
	}
	s.setFlash(msg, bad)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// pageAction handles the page's forms: same checks as the API, then back
// to the page with a message.
// diagnostics is the page's button: the bundle built from what the agent
// holds, downloaded to the member's own machine. It changes nothing and
// needs the page's session.
func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth.sessionOf(r); !ok {
		http.Error(w, "open the page with redundanet open first", http.StatusUnauthorized)
		return
	}
	snap := s.snapshot()
	in := diag.Input{Now: time.Now(), Version: snap.Version, Network: snap.Network, DataDir: s.cfg.DataDir, Device: snap.Device, Member: snap.Member, Roles: snap.Roles,
		AgentUp: true, Uptime: snap.Uptime, Service: snap.Service, Transport: snap.Transport, Membership: snap.Membership, Update: snap.Update, Jobs: snap.Jobs, Repair: snap.Repair}
	if s.logs() != nil {
		in.Lines = s.logs().Last(2000)
	}
	if text, err := diag.TailFile(filepath.Join(s.cfg.DataDir, "service.log"), 1<<20); err == nil {
		in.LogText = text
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="redundanet-diagnostics-%s.zip"`, in.Now.UTC().Format("20060102-150405")))
	if err := diag.Write(w, in); err != nil {
		s.cfg.Log.Warn("diagnostics bundle not written", "err", err)
	}
}

func (s *Server) pageAction(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.auth.sessionOf(r)
	if !ok {
		http.Error(w, "no session", http.StatusUnauthorized)
		return
	}
	if r.FormValue(formCSRF) != sess.csrf {
		http.Error(w, "missing or wrong CSRF token", http.StatusForbidden)
		return
	}
	// An action returns to the view its form was on; anything but one of
	// the page's own paths goes to the overview.
	back := "/"
	if v, ok := viewAt(r.FormValue("back")); ok {
		back = v.Path
	}
	l := tr(langOf(r))
	name := r.PathValue("name")
	if name == "language" {
		if code := r.FormValue("lang"); knownLang(code) {
			http.SetCookie(w, &http.Cookie{Name: langCookie, Value: code, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	msg, bad := s.runPageAction(r, name, l)
	if msg == "" && bad {
		http.NotFound(w, r)
		return
	}
	s.setFlash(msg, bad)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// runPageAction does what a form of the page asks and says how it went, in
// the reader's language; the reason of a failure is the agent's own text.
// An unknown action answers "" and true.
func (s *Server) runPageAction(r *http.Request, name string, l tr) (msg string, bad bool) {
	fail := func(key string, err error) (string, bool) { return l.T(key, err.Error()), true }
	switch name {
	case "device-pair":
		if s.actions().PairDevice == nil {
			return l.T("msg.pair.off"), true
		}
		code, phrase := strings.TrimSpace(r.FormValue("code")), r.FormValue("phrase")
		if code == "" || strings.TrimSpace(phrase) == "" {
			return l.T("msg.pair.incomplete"), true
		}
		ctx, cancel := context.WithTimeout(r.Context(), pairTimeout)
		defer cancel()
		d, err := s.actions().PairDevice(ctx, code, phrase, r.FormValue("storage") == "on")
		if err != nil {
			return fail("msg.pair.failed", err)
		}
		who := d.Name
		if who == "" {
			who = short(d.Key)
		}
		return l.T("msg.pair.done", who), false
	case "update-check", "update-apply", "update-retry", "settings-channel":
		if s.actions().CheckUpdate == nil && name == "update-check" || s.actions().ApplyUpdate == nil && name == "update-apply" ||
			s.actions().RetryUpdate == nil && name == "update-retry" || s.actions().SetChannel == nil && name == "settings-channel" {
			return l.T("msg.noupdates"), true
		}
	default:
		if s.jobs() == nil {
			switch name {
			case "backup-run", "backup-verify", "backup-add", "backup-remove", "restore-start", "settings-schedule", "settings-retention", "settings-bandwidth", "settings-excludes":
				return l.T("msg.nojobs"), true
			}
			return "", true
		}
	}
	switch name {
	case "update-check":
		v, err := s.actions().CheckUpdate(r.Context())
		switch {
		case err != nil:
			return fail("msg.check.failed", err)
		case v == "":
			return l.T("msg.check.none"), false
		}
		return l.T("msg.check.available", v), false
	case "update-apply":
		if err := s.actions().ApplyUpdate(r.Context()); err != nil {
			return fail("msg.apply.failed", err)
		}
		return l.T("msg.apply.started"), false
	case "update-retry":
		v := r.FormValue("version")
		if err := s.actions().RetryUpdate(r.Context(), v); err != nil {
			return fail("msg.retry.failed", err)
		}
		return l.T("msg.retry.done", v), false
	case "settings-channel":
		if err := s.actions().SetChannel(r.FormValue("channel")); err != nil {
			return fail("msg.notsaved", err)
		}
		return l.T("msg.saved"), false
	case "backup-run":
		s.jobs().RunNow(r.FormValue("set"))
		return l.T("msg.backup.requested"), false
	case "backup-verify":
		s.jobs().RunNow("verify")
		return l.T("msg.verify.requested"), false
	case "backup-add":
		set := backup.Set{Name: strings.TrimSpace(r.FormValue("name")), Folders: []string{strings.TrimSpace(r.FormValue("folder"))}}
		if set.Name == "" || set.Folders[0] == "" {
			return l.T("msg.add.incomplete"), true
		}
		for _, cur := range s.jobs().Status().Sets {
			if cur.Name == set.Name {
				// AddSet replaces a set of the same name, which is right for the
				// command line and a surprise in a form that says "add".
				return l.T("msg.add.exists", set.Name), true
			}
		}
		if err := s.jobs().AddSet(set); err != nil {
			return fail("msg.add.failed", err)
		}
		return l.T("msg.add.done", set.Name), false
	case "backup-remove":
		set := r.FormValue("set")
		if err := s.jobs().RemoveSet(set); err != nil {
			return fail("msg.notsaved", err)
		}
		return l.T("msg.remove.done", set), false
	case "restore-start":
		target, err := s.startRestore(r)
		if err != nil {
			return fail("msg.restore.failed", err)
		}
		return l.T("msg.restore.started", target), false
	case "settings-schedule", "settings-retention", "settings-bandwidth":
		in, err := settingsFromForm(r)
		if err != nil {
			return fail("msg.notsaved", err)
		}
		if _, err := s.applySettings(in); err != nil {
			return fail("msg.notsaved", err)
		}
		return l.T("msg.saved"), false
	case "settings-excludes":
		var excludes []string
		for _, e := range strings.Split(r.FormValue("excludes"), ",") {
			if e = strings.TrimSpace(e); e != "" {
				excludes = append(excludes, e)
			}
		}
		if err := s.jobs().SetExcludes(r.FormValue("set"), excludes); err != nil {
			return fail("msg.notsaved", err)
		}
		return l.T("msg.saved"), false
	}
	return "", true
}

// settingsFromForm reads the settings forms of the page: a field left
// empty stays as it is.
func settingsFromForm(r *http.Request) (Settings, error) {
	var in Settings
	str := func(name string) *string {
		if v := strings.TrimSpace(r.FormValue(name)); v != "" {
			return &v
		}
		return nil
	}
	num := func(name string) (*int, error) {
		v := strings.TrimSpace(r.FormValue(name))
		if v == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%s: not a number", name)
		}
		return &n, nil
	}
	in.Every, in.VerifyEvery, in.CleanupEvery = str("every"), str("verify_every"), str("cleanup_every")
	var err error
	if in.UploadKBps, err = num("upload_kbps"); err != nil {
		return in, err
	}
	if in.DownloadKBps, err = num("download_kbps"); err != nil {
		return in, err
	}
	if r.FormValue("days") != "" {
		var ret snapshot.Retention
		for name, dst := range map[string]*int{"days": &ret.Days, "daily": &ret.Daily, "weekly": &ret.Weekly, "monthly": &ret.Monthly} {
			n, err := num(name)
			if err != nil {
				return in, err
			}
			if n != nil {
				*dst = *n
			}
		}
		in.Retention = &ret
	}
	return in, nil
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ReadClientConfig finds the running agent's address and token in the data
// directory, for the CLI.
func ReadClientConfig(dataDir string) (addr, token string, err error) {
	a, err := os.ReadFile(filepath.Join(dataDir, addrFile))
	if err != nil {
		return "", "", fmt.Errorf("no running agent found in %s (%w)", dataDir, err)
	}
	t, err := os.ReadFile(filepath.Join(dataDir, tokenFile))
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(a)), strings.TrimSpace(string(t)), nil
}
