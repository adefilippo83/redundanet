package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How the CLI and the agent know each other without the token crossing
// the port.
//
// The CLI finds the agent by the address in api.addr. Whatever program
// holds that port gets what the CLI sends, so the token itself is never
// sent. Instead:
//
//   - The CLI first asks GET /api/prove?nonce=N. The agent answers with an
//     HMAC of N under the token. A program that does not hold the token
//     cannot answer, and the CLI stops there.
//   - Every request then carries, in place of the token, an HMAC under it
//     of the method, the address asked, the time, a nonce and the hash of
//     the body. The agent accepts each once, within two minutes of its
//     time. What a listener in between sees is good for that one request.
//
// The two HMACs are made under different labels, so an answer of the one
// kind is never a valid instance of the other. The agent still accepts a
// plain "Bearer" token, for a script that reads api.token itself; the CLI
// does not send one.

const (
	proofPath    = "/api/prove"
	schemeSigned = "RN3-HMAC"
	labelServer  = "redundanet/3/local-api/server\x00"
	labelClient  = "redundanet/3/local-api/client\x00"
	signedWindow = 2 * time.Minute
	maxSigned    = 8 << 20 // the largest body a signed request may carry
	noncesAtMost = 4096
)

func mac(token string, parts ...string) string {
	h := hmac.New(sha256.New, []byte(token))
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serverProof(token, nonce string) string { return mac(token, labelServer, nonce) }

func requestMAC(token, method, uri, ts, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return mac(token, labelClient, method, uri, ts, nonce, hex.EncodeToString(sum[:]))
}

// prove answers the CLI's question whether this is the agent: it needs no
// credential, since the answer is of use only to who can check it.
func (s *Server) prove(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if len(nonce) != 64 {
		http.Error(w, "nonce: 64 hex characters", http.StatusBadRequest)
		return
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		http.Error(w, "nonce: 64 hex characters", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"proof": serverProof(s.auth.token, nonce)})
}

// seenNonces remembers the nonces of signed requests for as long as their
// time is acceptable, so none is accepted twice.
type seenNonces struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (n *seenNonces) fresh(nonce string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.at == nil {
		n.at = map[string]time.Time{}
	}
	for k, t := range n.at {
		if now.Sub(t) > 2*signedWindow {
			delete(n.at, k)
		}
	}
	if _, seen := n.at[nonce]; seen || len(n.at) >= noncesAtMost {
		return false
	}
	n.at[nonce] = now
	return true
}

// signed reports whether the request carries a valid signature of the
// CLI. It reads the body to check it and puts it back.
func (a *auth) signed(r *http.Request) bool {
	scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || scheme != schemeSigned {
		return false
	}
	f := strings.Split(rest, ":")
	if len(f) != 3 || len(f[1]) != 32 {
		return false
	}
	ts, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return false
	}
	now := time.Now()
	if d := now.Sub(time.Unix(ts, 0)); d > signedWindow || d < -signedWindow {
		return false
	}
	var body []byte
	if r.Body != nil {
		if body, err = io.ReadAll(io.LimitReader(r.Body, maxSigned+1)); err != nil || len(body) > maxSigned {
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	want := requestMAC(a.token, r.Method, r.URL.RequestURI(), f[0], f[1], body)
	if subtle.ConstantTimeCompare([]byte(want), []byte(f[2])) != 1 {
		return false
	}
	return a.nonces.fresh(f[1], now)
}

// ErrNotTheAgent is what the CLI says when the program at the agent's
// address could not prove it holds the token.
var ErrNotTheAgent = errors.New("the program at the agent's address did not prove it is the agent")

// verifyServer asks the program at the address to prove it holds the
// token, once per client, before anything else is sent to it.
func (c *Client) verifyServer(ctx context.Context) error {
	c.once.Do(func() {
		nonce := randomHex(32)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+c.addr+proofPath+"?nonce="+nonce, nil)
		if err != nil {
			c.proven = err
			return
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.proven = err
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var out struct {
			Proof string `json:"proof"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &out) != nil ||
			subtle.ConstantTimeCompare([]byte(out.Proof), []byte(serverProof(c.token, nonce))) != 1 {
			c.proven = fmt.Errorf("%w (%s answered %d): either the agent there is older than this command, and restarting the service fixes it, or another program holds the port; nothing was sent to it", ErrNotTheAgent, c.addr, resp.StatusCode)
		}
	})
	return c.proven
}

// sign puts the request's signature in place of the token.
func (c *Client) sign(req *http.Request, body []byte) {
	ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), randomHex(16)
	req.Header.Set("Authorization", schemeSigned+" "+ts+":"+nonce+":"+requestMAC(c.token, req.Method, req.URL.RequestURI(), ts, nonce, body))
}
