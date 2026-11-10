package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Another program holds the port the CLI finds in api.addr. Whatever it
// answers, the CLI sends it one question with a random number in it and
// then stops: no token, no request.
func TestTheCLISendsNothingToAProgramThatIsNotTheAgent(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, answer := range map[string]func(w http.ResponseWriter, r *http.Request){
		"that knows nothing": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"that says yes":      func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"proof":"00"}`) },
		"that echoes the nonce": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"proof":"`+r.URL.Query().Get("nonce")+`"}`)
		},
		"that asks for the token": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		},
	} {
		var mu sync.Mutex
		var seen []string
		squatter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.URL.RawQuery+" "+string(b))
			mu.Unlock()
			answer(w, r)
		}))
		c := NewClient(strings.TrimPrefix(squatter.URL, "http://"), token)
		ctx := context.Background()
		if _, err := c.Status(ctx); !errors.Is(err, ErrNotTheAgent) {
			t.Fatalf("a program %s: status answered %v", name, err)
		}
		if err := c.BackupRun(ctx, "documents"); !errors.Is(err, ErrNotTheAgent) {
			t.Fatalf("a program %s: a change answered %v", name, err)
		}
		if _, err := c.LoginLink(ctx); !errors.Is(err, ErrNotTheAgent) {
			t.Fatalf("a program %s: a login link answered %v", name, err)
		}
		squatter.Close()
		if len(seen) != 1 || !strings.HasPrefix(seen[0], "GET "+proofPath+"  nonce=") {
			t.Fatalf("a program %s was sent: %q", name, seen)
		}
		if strings.Contains(strings.Join(seen, "\n"), token) || strings.Contains(seen[0], "documents") {
			t.Fatalf("a program %s got the token or a request: %q", name, seen)
		}
	}
}

// The agent accepts a signed request once, for the address and body it
// was signed for, near the time it was signed.
func TestSignedRequests(t *testing.T) {
	h := newHarness(t)
	c := NewClient(h.srv.Addr(), h.token)
	send := func(method, path, body, authz string) int {
		t.Helper()
		return h.req(method, path, map[string]string{"Authorization": authz}, body).StatusCode
	}
	sign := func(method, path, body string, at time.Time, nonce string) string {
		ts := strconv.FormatInt(at.Unix(), 10)
		return schemeSigned + " " + ts + ":" + nonce + ":" + requestMAC(h.token, method, path, ts, nonce, []byte(body))
	}
	now := time.Now()
	a := sign("GET", "/api/status", "", now, randomHex(16))
	if code := send("GET", "/api/status", "", a); code != http.StatusOK {
		t.Fatalf("a signed request: %d", code)
	}
	if code := send("GET", "/api/status", "", a); code != http.StatusUnauthorized {
		t.Fatalf("the same signed request again: %d", code)
	}
	for what, authz := range map[string]string{
		"signed for another address": sign("GET", "/api/backups", "", now, randomHex(16)),
		"signed for another method":  sign("POST", "/api/status", "", now, randomHex(16)),
		"signed ten minutes ago":     sign("GET", "/api/status", "", now.Add(-10*time.Minute), randomHex(16)),
		"signed ten minutes ahead":   sign("GET", "/api/status", "", now.Add(10*time.Minute), randomHex(16)),
		"signed with another token":  schemeSigned + " " + strconv.FormatInt(now.Unix(), 10) + ":" + strings.Repeat("a", 32) + ":" + requestMAC(strings.Repeat("f", 64), "GET", "/api/status", strconv.FormatInt(now.Unix(), 10), strings.Repeat("a", 32), nil),
		"the server's own proof":     schemeSigned + " " + strconv.FormatInt(now.Unix(), 10) + ":" + randomHex(16) + ":" + serverProof(h.token, randomHex(32)),
	} {
		if code := send("GET", "/api/status", "", authz); code != http.StatusUnauthorized {
			t.Errorf("a request %s: %d", what, code)
		}
	}
	// A body is part of what is signed, and the handler still reads it.
	body := `{"upload_kbps":100,"download_kbps":0}`
	if code := send("POST", "/api/backup/limit", `{"upload_kbps":999,"download_kbps":0}`, sign("POST", "/api/backup/limit", body, now, randomHex(16))); code != http.StatusUnauthorized {
		t.Fatalf("a signed request with another body: %d", code)
	}
	// The client's own calls go through, a second client too, and the
	// answer to the question it asks first is the agent's.
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LoginLink(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(h.srv.Addr(), strings.Repeat("f", 64)).Status(context.Background()); !errors.Is(err, ErrNotTheAgent) {
		t.Fatalf("a client with another token took the agent for its own: %v", err)
	}
	// A script that reads api.token itself may still send it.
	if code := send("GET", "/api/status", "", "Bearer "+h.token); code != http.StatusOK {
		t.Fatalf("the token itself: %d", code)
	}
}
