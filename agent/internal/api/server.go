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
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/diag"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/membership"
	"github.com/adefilippo83/redundanet/agent/internal/repair"
	"github.com/adefilippo83/redundanet/agent/internal/service"
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
	Roles      []string           `json:"roles"`
	Transport  *transport.Status  `json:"transport,omitempty"`
	Membership *membership.Status `json:"membership,omitempty"`
	Update     *update.Status     `json:"update,omitempty"`
	Jobs       *jobs.Status       `json:"jobs,omitempty"`
	Repair     *repair.Status     `json:"repair,omitempty"` // a storage node's repair worker
	Service    *service.Status    `json:"service,omitempty"`
	Generated  time.Time          `json:"generated"`
}

// Actions are the state changes the API can trigger.
type Actions struct {
	CheckUpdate func(ctx context.Context) (string, error) // returns the available version, "" for none
	ApplyUpdate func(ctx context.Context) error
	RetryUpdate func(ctx context.Context, version string) error // forgets a failed probation
	// Revoke applies a member's signed device revocation here and sends it
	// to every storage server and hub: the direct path of "Losing a device".
	Revoke func(ctx context.Context, r *identity.SignedRevocation) (RevokeReport, error)
}

// RevokeReport is how far a revocation got right away; the signed
// manifest carries it to the rest.
type RevokeReport struct {
	Servers      int `json:"servers"`       // storage servers that took it
	ServersTotal int `json:"servers_total"` // storage servers in the manifest
	Hubs         int `json:"hubs"`          // hubs that took it
	HubsTotal    int `json:"hubs_total"`
}

// Config wires the server.
type Config struct {
	Addr     string // 127.0.0.1:4470 by default
	DataDir  string
	Snapshot func() Snapshot
	Actions  Actions
	Jobs     *jobs.Runner // backup, restore test and restore; nil on a hub
	Logs     *logbuf.Buffer
	Log      *slog.Logger
}

// Server is the running API.
type Server struct {
	cfg  Config
	log  *slog.Logger
	auth *auth
	ln   net.Listener
	srv  *http.Server
	tmpl *template.Template

	mu      sync.Mutex
	lastMsg string // the outcome of the last action, shown once on the page
}

//go:embed templates/page.html
var templates embed.FS

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
	tmpl, err := template.New("page.html").Funcs(template.FuncMap{"short": short, "since": since, "lastIndex": func(v []transport.Event) int { return len(v) - 1 }}).ParseFS(templates, "templates/page.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: cfg.Log.With("component", "api"), auth: newAuth(token), tmpl: tmpl}
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
	mux.HandleFunc("GET /diagnostics.zip", s.diagnostics)
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("POST /action/{name}", s.pageAction)
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
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
	writeJSON(w, s.cfg.Snapshot())
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
	if s.cfg.Logs != nil {
		lines = s.cfg.Logs.Last(n)
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
	if s.cfg.Actions.Revoke == nil {
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
	rep, err := s.cfg.Actions.Revoke(r.Context(), rv)
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
	if s.cfg.Actions.CheckUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	v, err := s.cfg.Actions.CheckUpdate(r.Context())
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
	if s.cfg.Actions.ApplyUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	if err := s.cfg.Actions.ApplyUpdate(r.Context()); err != nil {
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
	if s.cfg.Actions.RetryUpdate == nil {
		http.Error(w, "updates are not configured", http.StatusNotImplemented)
		return
	}
	v := r.FormValue("version")
	if v == "" {
		http.Error(w, "version is required", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Actions.RetryUpdate(r.Context(), v); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"result": v + " may be installed again at the next check"})
}

func (s *Server) needJobs(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireAPI(w, r) {
		return false
	}
	if s.cfg.Jobs == nil {
		http.Error(w, "this device runs no backup jobs", http.StatusNotImplemented)
		return false
	}
	return true
}

func (s *Server) apiBackups(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	writeJSON(w, s.cfg.Jobs.Status())
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
	if err := s.cfg.Jobs.AddSet(set); err != nil {
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
	if err := s.cfg.Jobs.SetLimits(l.UploadKBps, l.DownloadKBps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "limits saved"})
}

func (s *Server) apiBackupRemove(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	if err := s.cfg.Jobs.RemoveSet(r.FormValue("set")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"result": "set removed"})
}

func (s *Server) apiBackupRun(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	s.cfg.Jobs.RunNow(r.FormValue("set"))
	writeJSON(w, map[string]string{"result": "backup requested"})
}

func (s *Server) apiBackupVerify(w http.ResponseWriter, r *http.Request) {
	if !s.needJobs(w, r) {
		return
	}
	s.cfg.Jobs.RunNow("verify")
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
	rep, err := s.cfg.Jobs.RunCleanup(r.Context(), req.DryRun, req.Force)
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
	st, err := s.cfg.Jobs.RunRebalance(r.Context())
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
	l, err := s.cfg.Jobs.List(r.Context())
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
	entry, dir, err := s.cfg.Jobs.Browse(r.Context(), dev, r.FormValue("snapshot"), r.FormValue("path"))
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
	if err := s.cfg.Jobs.StartRestore(dev, req.Snapshot, req.Path, req.Target, req.Overwrite); err != nil {
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

type pageData struct {
	Snapshot
	CSRF    string
	Message string
	Logs    []logbuf.Line
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.auth.sessionOf(r)
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, "<!doctype html><title>RedundaNet</title><p style=\"font-family:system-ui;margin:3em\">Open this page with <code>redundanet open</code>, which prints a one-time link.</p>")
		return
	}
	s.mu.Lock()
	msg := s.lastMsg
	s.lastMsg = ""
	s.mu.Unlock()
	data := pageData{Snapshot: s.cfg.Snapshot(), CSRF: sess.csrf, Message: msg}
	if s.cfg.Logs != nil {
		data.Logs = s.cfg.Logs.Last(40)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		s.log.Error("page", "err", err)
	}
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
	snap := s.cfg.Snapshot()
	in := diag.Input{Now: time.Now(), Version: snap.Version, Network: snap.Network, DataDir: s.cfg.DataDir, Device: snap.Device, Member: snap.Member, Roles: snap.Roles,
		AgentUp: true, Uptime: snap.Uptime, Service: snap.Service, Transport: snap.Transport, Membership: snap.Membership, Update: snap.Update, Jobs: snap.Jobs, Repair: snap.Repair}
	if s.cfg.Logs != nil {
		in.Lines = s.cfg.Logs.Last(2000)
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
	var msg string
	switch r.PathValue("name") {
	case "update-check":
		if s.cfg.Actions.CheckUpdate == nil {
			msg = "updates are not configured in this build"
			break
		}
		v, err := s.cfg.Actions.CheckUpdate(r.Context())
		switch {
		case err != nil:
			msg = "update check failed: " + err.Error()
		case v == "":
			msg = "no newer release on your channel"
		default:
			msg = "release " + v + " is available"
		}
	case "update-apply":
		if s.cfg.Actions.ApplyUpdate == nil {
			msg = "updates are not configured in this build"
			break
		}
		if err := s.cfg.Actions.ApplyUpdate(r.Context()); err != nil {
			msg = "update failed: " + err.Error()
		} else {
			msg = "update started: the agent restarts when the new version passes probation"
		}
	case "backup-run":
		if s.cfg.Jobs == nil {
			msg = "this device runs no backup jobs"
			break
		}
		s.cfg.Jobs.RunNow(r.FormValue("set"))
		msg = "backup requested; progress shows below"
	case "backup-verify":
		if s.cfg.Jobs == nil {
			msg = "this device runs no backup jobs"
			break
		}
		s.cfg.Jobs.RunNow("verify")
		msg = "restore test requested"
	case "update-retry":
		if s.cfg.Actions.RetryUpdate == nil {
			msg = "updates are not configured in this build"
			break
		}
		v := r.FormValue("version")
		if err := s.cfg.Actions.RetryUpdate(r.Context(), v); err != nil {
			msg = "retry failed: " + err.Error()
		} else {
			msg = v + " may be installed again at the next check"
		}
	default:
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.lastMsg = msg
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func since(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("2006-01-02")
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
