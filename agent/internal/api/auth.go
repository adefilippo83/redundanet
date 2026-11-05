package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Three guards, as the RFC's Browser safety section lists them: the bearer
// token in a file only the agent's user can read, for the CLI; a session
// cookie set by a one-time link, HttpOnly and SameSite=Strict, for the
// browser, with a CSRF token on every change; and a Host and Origin check
// on every request, so a web page the member visits cannot drive the API
// through the browser.

const (
	tokenFile      = "api.token"
	addrFile       = "api.addr"
	sessionCookie  = "redundanet_session"
	sessionTTL     = 12 * time.Hour
	loginLinkTTL   = 2 * time.Minute
	maxLoginLinks  = 16
	headerCSRF     = "X-CSRF-Token"
	formCSRF       = "csrf"
	sessionsAtMost = 32
)

type session struct {
	csrf    string
	expires time.Time
}

type auth struct {
	token string

	mu       sync.Mutex
	sessions map[string]session
	links    map[string]time.Time
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loadToken reads the bearer token, creating it on first run.
func loadToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, tokenFile)
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	t := randomHex(32)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

func newAuth(token string) *auth {
	return &auth{token: token, sessions: map[string]session{}, links: map[string]time.Time{}}
}

// bearer reports whether the request carries the CLI token.
func (a *auth) bearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(a.token)) == 1
}

// sessionOf returns the browser session, if the cookie names a live one.
func (a *auth) sessionOf(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.expires) {
		delete(a.sessions, c.Value)
		return session{}, false
	}
	return s, true
}

// newLoginLink mints a one-time token for `redundanet open`.
func (a *auth) newLoginLink() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for t, exp := range a.links {
		if now.After(exp) {
			delete(a.links, t)
		}
	}
	if len(a.links) >= maxLoginLinks {
		return ""
	}
	t := randomHex(16)
	a.links[t] = now.Add(loginLinkTTL)
	return t
}

// redeem turns a one-time token into a session and returns its cookie id.
func (a *auth) redeem(t string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.links[t]
	delete(a.links, t)
	if !ok || time.Now().After(exp) {
		return "", false
	}
	if len(a.sessions) >= sessionsAtMost {
		for id, s := range a.sessions {
			if time.Now().After(s.expires) {
				delete(a.sessions, id)
			}
		}
		if len(a.sessions) >= sessionsAtMost {
			return "", false
		}
	}
	id := randomHex(16)
	a.sessions[id] = session{csrf: randomHex(16), expires: time.Now().Add(sessionTTL)}
	return id, true
}

// hostOK accepts only loopback hosts, so a DNS name that resolves here
// cannot be used from a web page.
func hostOK(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// originOK accepts a state change only from the page itself.
func originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser, or a same-origin form in an old browser; the CSRF token still applies
	}
	o = strings.TrimPrefix(o, "http://")
	return o == r.Host
}

var errUnauthorized = errors.New("unauthorized")
