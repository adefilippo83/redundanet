package collector

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/pairing"
)

// The pairing mailbox answers under /pair/ on the status page when the hub
// mounts it, and the page's own routes stay.
func TestStatusPageMountsThePairingRelay(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := New(Config{Manifest: func() *manifest.Manifest { return nil }, Log: lg, Pair: pairing.NewRelay(lg)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()
	room := strings.Repeat("ab", 16)
	resp, err := http.Post(srv.URL+"/pair/"+room+"/new", "application/octet-stream", bytes.NewReader([]byte("frame")))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("post to the mailbox: %d", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/status.json")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status.json beside the mailbox: %d", resp.StatusCode)
	}
	plain, err := New(Config{Manifest: func() *manifest.Manifest { return nil }, Log: lg})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	srv2 := httptest.NewServer(plain.Handler())
	defer srv2.Close()
	resp, err = http.Post(srv2.URL+"/pair/"+room+"/new", "application/octet-stream", bytes.NewReader([]byte("frame")))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a hub without the mailbox: %d, want 405 from the page's GET-only root, which device pair reads as \"needs 3.0.10\"", resp.StatusCode)
	}
}
