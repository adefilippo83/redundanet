package main

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/api"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
)

func aKey(t *testing.T) identity.Key {
	t.Helper()
	k, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.ID()
}

// The three actions are all a device starts for another; anything else,
// and anything before the device is ready, is refused.
func TestRemoteActKnowsThreeActions(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, a := range []string{"restore-start", "update-apply", "backup-remove", "settings", ""} {
		if err := remoteAct(a, nil, nil, lg); err == nil || !strings.Contains(err.Error(), "unknown action") {
			t.Errorf("action %q: %v", a, err)
		}
	}
	for _, a := range []string{api.RemoteBackup, api.RemoteVerify, api.RemoteUpdateCheck} {
		if err := remoteAct(a, nil, nil, lg); err == nil || strings.Contains(err.Error(), "unknown") {
			t.Errorf("action %q on a device that cannot do it: %v", a, err)
		}
	}
	answers := &remoteAnswers{}
	h := answers.handler(aKey(t))
	if _, err := h.View(); err == nil {
		t.Error("a device that is starting gave a view")
	}
	if err := h.Act(api.RemoteBackup); err == nil {
		t.Error("a device that is starting took an action")
	}
	answers.set(func() ([]byte, error) { return []byte("{}"), nil }, func(string) error { return nil })
	if b, err := h.View(); err != nil || string(b) != "{}" {
		t.Errorf("view once ready: %q %v", b, err)
	}
}

func TestRemoteErrorNamesAnOlderDevice(t *testing.T) {
	for _, se := range []*shareproto.StatusError{
		{Status: shareproto.StatusBad, Message: "unknown request view"},
		{Status: shareproto.StatusRefused, Message: "this device is not a storage node"},
	} {
		if err := remoteError(se); !strings.Contains(err.Error(), "version that does not offer this") {
			t.Errorf("%v became %v", se, err)
		}
	}
	if err := remoteError(&shareproto.StatusError{Status: shareproto.StatusRefused, Message: "this device answers only its own member's devices"}); !strings.Contains(err.Error(), "own member") {
		t.Errorf("a refusal lost its reason: %v", err)
	}
	plain := errors.New("no connection")
	if remoteError(plain) != plain || remoteError(nil) != nil {
		t.Error("another error was changed")
	}
}

func TestOwnDeviceByNameOrKey(t *testing.T) {
	a, b, self := aKey(t).String(), aKey(t).String(), aKey(t).String()
	devs := []api.Device{{Key: a, Name: "falcon"}, {Key: b}}
	for word, want := range map[string]string{"falcon": a, "FALCON": a, a: a, b[:10]: b, strings.ToUpper(b[:10]): b} {
		if got, err := ownDevice(devs, self, word); err != nil || got != want {
			t.Errorf("%q: %q %v", word, got, err)
		}
	}
	other := aKey(t).String()
	if got, err := ownDevice(devs, self, other); err != nil || got != other {
		t.Errorf("a whole key not listed here: %q %v", got, err)
	}
	for _, word := range []string{"nothing", self, ""} {
		if got, err := ownDevice(devs, self, word); err == nil && word != "" {
			t.Errorf("%q found %q", word, got)
		}
	}
}

func TestListedNodesJoinTheDevices(t *testing.T) {
	me, self, paired, node, theirs := aKey(t), aKey(t), aKey(t), aKey(t), aKey(t)
	m := &manifest.Manifest{
		Members: []manifest.Member{{Name: "me", Key: me}, {Name: "them", Key: aKey(t)}},
		Storage: []manifest.StorageDevice{{Member: "me", Device: self, Name: "this"}, {Member: "me", Device: paired, Name: "pi"}, {Member: "me", Device: node, Name: "falcon"}, {Member: "them", Device: theirs, Name: "other"}},
	}
	got := withListedNodes([]api.Device{{Key: paired.String(), Name: "pi"}}, m, me, self)
	if len(got) != 2 || got[1].Key != node.String() || got[1].Name != "falcon" {
		t.Fatalf("devices: %+v", got)
	}
	if out := withListedNodes(nil, m, aKey(t), self); len(out) != 0 {
		t.Fatalf("a member the manifest does not list got devices: %+v", out)
	}
}
