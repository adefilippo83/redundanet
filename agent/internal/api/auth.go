package api

import (
	"context"
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

// Three guards, as the RFC's Browser safety section lists them: the token
// in a file only the agent's user can read, for the CLI; a session cookie
// set by a one-time link, HttpOnly and SameSite=Strict, for the browser,
// with a CSRF token on every change; and a Host and Origin check on every
// request, so a web page the member visits cannot drive the API through
// the browser.
//
// Two things about them. A browser scopes a cookie by host, not by port,
// so a cookie for 127.0.0.1 would reach every other program that serves
// pages on this machine. The session therefore has a second secret, its
// key, which is part of the page's address: the page lives under
// /s/<key>/, the cookie's path is that prefix, and a request counts as the
// session's only with both. Another port never learns the address, so the
// browser never sends it the cookie, and a cookie learned some other way
// opens nothing without the key.
//
// And the CLI never sends the token: whatever holds the port would get it.
// It asks the server to prove it holds the token first, then signs each
// request with it (proof.go).

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
	key     string // the address secret: the page of this session is under /s/<key>/
	expires time.Time
}

// keyedPrefix is where a session's page lives: keyedPrefix + key + "/".
const keyedPrefix = "/s/"

type keyedCtx struct{}

// keyOf is the session key a request's address carried, "" when none.
func keyOf(r *http.Request) string {
	k, _ := r.Context().Value(keyedCtx{}).(string)
	return k
}

// base is the prefix of the page a request belongs to, for a redirect.
func base(r *http.Request) string {
	if k := keyOf(r); k != "" {
		return keyedPrefix + k
	}
	return ""
}

// keyed takes the session key off the address, so the routes below are
// the same with and without it, and remembers it for sessionOf.
func keyed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, keyedPrefix)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		key, path, ok := strings.Cut(rest, "/")
		if !ok || key == "" {
			http.NotFound(w, r)
			return
		}
		r2 := r.WithContext(context.WithValue(r.Context(), keyedCtx{}, key))
		u := *r.URL
		u.Path, u.RawPath = "/"+path, ""
		r2.URL = &u
		next.ServeHTTP(w, r2)
	})
}

type auth struct {
	token  string
	nonces seenNonces

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

// bearer reports whether the request comes from a holder of the token:
// signed with it, as the CLI does, or carrying it, as a script may.
func (a *auth) bearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, schemeSigned+" ") {
		return a.signed(r)
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(a.token)) == 1
}

// sessionOf returns the browser session: the cookie names a live one and
// the address carried that session's key. The cookie alone is no session.
func (a *auth) sessionOf(r *http.Request) (session, bool) {
	key := keyOf(r)
	if key == "" {
		return session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// A browser may hold the cookies of several sessions, each under its
	// own path; the one of this address is the one that counts.
	for _, c := range r.Cookies() {
		if c.Name != sessionCookie {
			continue
		}
		s, ok := a.sessions[c.Value]
		if !ok {
			continue
		}
		if time.Now().After(s.expires) {
			delete(a.sessions, c.Value)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(s.key), []byte(key)) == 1 {
			return s, true
		}
	}
	return session{}, false
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

// redeem turns a one-time token into a session and returns its cookie id
// and its key.
func (a *auth) redeem(t string) (id, key string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.links[t]
	delete(a.links, t)
	if !ok || time.Now().After(exp) {
		return "", "", false
	}
	if len(a.sessions) >= sessionsAtMost {
		for id, s := range a.sessions {
			if time.Now().After(s.expires) {
				delete(a.sessions, id)
			}
		}
		if len(a.sessions) >= sessionsAtMost {
			return "", "", false
		}
	}
	id, key = randomHex(16), randomHex(16)
	a.sessions[id] = session{csrf: randomHex(16), key: key, expires: time.Now().Add(sessionTTL)}
	return id, key, true
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
