package collector

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/manifest"
)

// TestPreview renders the status page from a status.json, for working on
// its look without a hub:
//
//	curl -s https://hub.redundanet.com/status.json > /tmp/status.json
//	REDUNDANET_STATUS_PREVIEW=/tmp/status.json:/tmp/status.html go test ./internal/collector -run TestPreview
//
// Without the variable the test does nothing.
func TestPreview(t *testing.T) {
	spec := os.Getenv("REDUNDANET_STATUS_PREVIEW")
	if spec == "" {
		t.Skip("set REDUNDANET_STATUS_PREVIEW=status.json:out.html to render the page from a status")
	}
	in, out, ok := strings.Cut(spec, ":")
	if !ok {
		t.Fatal("REDUNDANET_STATUS_PREVIEW is IN.json:OUT.html")
	}
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Manifest: func() *manifest.Manifest { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	html, err := c.render(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, html, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The first line of the page, case by case; and the page carries no
// inline style attribute, only its one style block.
func TestVerdict(t *testing.T) {
	for name, c := range map[string]struct {
		st   Status
		want string
	}{
		"a hub that tracks no placement": {Status{}, ""},
		"all placed":                     {Status{Grid: &Grid{Objects: 3, Placed: 3}, Devices: []Device{{State: "online"}, {State: "dead"}, {State: "never"}}}, "ok"},
		"a device away":                  {Status{Grid: &Grid{Objects: 3, Placed: 3}, Devices: []Device{{State: "online"}, {State: "down"}}}, "warn"},
		"repair due":                     {Status{Grid: &Grid{Objects: 3, Placed: 2, UnderPlaced: 1, Repair: 1}}, "warn"},
		"unreadable wins":                {Status{Grid: &Grid{Objects: 3, UnderPlaced: 1, Unreadable: 1}, Devices: []Device{{State: "down"}}}, "bad"},
		"servers disagree":               {Status{Grid: &Grid{Objects: 3, Placed: 3, Conflicts: 1}}, "bad"},
	} {
		if got := pageData(c.st).Verdict; got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	c, err := New(Config{Manifest: func() *manifest.Manifest { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	html, err := c.render(Status{Network: "testnet", Grid: &Grid{Objects: 3, Placed: 3, Conversion: &Conversion{From: "2-of-4", To: "2-of-3", Objects: 3, Converted: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	page := string(html)
	if !strings.Contains(page, "Everything stored is fully placed") || !strings.Contains(page, `<progress value="1" max="3">`) || strings.Contains(page, " style=") || strings.Contains(page, "<script") || strings.Contains(page, "%!") {
		t.Fatalf("page: %s", page[strings.Index(page, "<body>"):])
	}
}
