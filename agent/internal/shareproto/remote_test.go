package shareproto

import (
	"context"
	"errors"
	"testing"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

func remoteKey(t *testing.T) identity.Key {
	t.Helper()
	k, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	return k.ID()
}

// A device answers the view and the act of a device of its own member,
// and of nobody else; what it is not asked through these two it does not
// do, storage node or not.
func TestRemoteAnswersOnlyTheMembersOwnDevices(t *testing.T) {
	member, stranger := remoteKey(t), remoteKey(t)
	var acts []string
	remote := &Remote{Member: member,
		View: func() ([]byte, error) { return []byte(`{"snapshot":{}}`), nil },
		Act: func(a string) error {
			if a != "backup-run" {
				return errors.New("unknown action")
			}
			acts = append(acts, a)
			return nil
		}}
	handle := Serve(nil, remote, nil)
	as := func(m identity.Key) *Client {
		return NewClient(LocalOpener(identity.Peer{Device: remoteKey(t), Member: m}, handle))
	}
	ctx := context.Background()
	far := remoteKey(t)

	body, err := as(member).View(ctx, far)
	if err != nil || string(body) != `{"snapshot":{}}` {
		t.Fatalf("the member's own device: %q %v", body, err)
	}
	if err := as(member).Act(ctx, far, "backup-run"); err != nil || len(acts) != 1 {
		t.Fatalf("act: %v %v", err, acts)
	}
	status := func(err error) string {
		var se *StatusError
		if errors.As(err, &se) {
			return se.Status
		}
		return "no status: " + errString(err)
	}
	if err := as(member).Act(ctx, far, "restore-start"); status(err) != StatusBad {
		t.Errorf("an action the device does not know: %v", err)
	}
	// Another member's device, and one with no member at all, get nothing.
	for _, m := range []identity.Key{stranger, {}} {
		if _, err := as(m).View(ctx, far); status(err) != StatusRefused {
			t.Errorf("view by another member: %v", err)
		}
		if err := as(m).Act(ctx, far, "backup-run"); status(err) != StatusRefused {
			t.Errorf("act by another member: %v", err)
		}
	}
	if len(acts) != 1 {
		t.Fatalf("a stranger started something: %v", acts)
	}
	// A device with no store still refuses the storage protocol.
	if _, err := as(member).Usage(ctx, far); status(err) != StatusRefused {
		t.Errorf("usage on a device without a store: %v", err)
	}
	// Turned off, and a device with no member of its own: refused.
	for _, r := range []*Remote{nil, {View: remote.View, Act: remote.Act}} {
		c := NewClient(LocalOpener(identity.Peer{Device: remoteKey(t), Member: member}, Serve(nil, r, nil)))
		if _, err := c.View(ctx, far); status(err) != StatusRefused {
			t.Errorf("view on a device that does not answer: %v", err)
		}
	}
	// A device from before the view existed refuses the request, which the
	// asking side reads as "too old".
	old := NewClient(LocalOpener(identity.Peer{Device: remoteKey(t), Member: member}, RefuseAll))
	if _, err := old.View(ctx, far); status(err) != StatusRefused {
		t.Errorf("view on an older device: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
