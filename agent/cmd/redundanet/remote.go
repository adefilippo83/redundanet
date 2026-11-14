package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/api"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/logbuf"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/update"
)

// A member looks at another device of theirs from this one: the page's
// remote view and `redundanet open DEVICE`. The device asked answers with
// its status and starts one of three actions, for a device of its own
// member and nobody else; see shareproto.Remote.

// withListedNodes adds the member's storage nodes the manifest lists to
// the devices this one paired: a node paired from another device of the
// member is the member's all the same, and the page links to each.
func withListedNodes(devices []api.Device, m *manifest.Manifest, member, self identity.Key) []api.Device {
	name := ""
	for _, mem := range m.Members {
		if mem.Key == member {
			name = mem.Name
		}
	}
	if name == "" {
		return devices
	}
	known := map[string]bool{self.String(): true}
	for _, d := range devices {
		known[d.Key] = true
	}
	for _, sd := range m.Storage {
		if sd.Member != name || known[sd.Device.String()] {
			continue
		}
		known[sd.Device.String()] = true
		devices = append(devices, api.Device{Key: sd.Device.String(), Name: sd.Name, Roles: []string{identity.RoleStorage}})
	}
	return devices
}

// remoteAnswers is what this device answers its member's other devices
// from. The jobs and the updater start after the mesh endpoint is wired,
// so the answers arrive later: until then, and for good in an update
// candidate, the device says it is starting.
type remoteAnswers struct {
	mu   sync.Mutex
	view func() ([]byte, error)
	act  func(string) error
}

func (a *remoteAnswers) set(view func() ([]byte, error), act func(string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.view, a.act = view, act
}

// handler is the share protocol's side of it, for this device's member.
func (a *remoteAnswers) handler(member identity.Key) *shareproto.Remote {
	starting := errors.New("the device is starting")
	return &shareproto.Remote{Member: member,
		View: func() ([]byte, error) {
			a.mu.Lock()
			f := a.view
			a.mu.Unlock()
			if f == nil {
				return nil, starting
			}
			return f()
		},
		Act: func(action string) error {
			a.mu.Lock()
			f := a.act
			a.mu.Unlock()
			if f == nil {
				return starting
			}
			return f(action)
		}}
}

// remoteView is what this device tells another device of its member:
// the snapshot its own page shows and its last log lines.
func remoteView(snap api.Snapshot, logs *logbuf.Buffer) ([]byte, error) {
	v := api.RemoteView{Snapshot: snap}
	if logs != nil {
		v.Logs = logs.Last(api.RemoteLogLines)
	}
	return json.Marshal(v)
}

// remoteAct starts one of the three things a member may start on this
// device from another of theirs. Nothing here takes a path, a setting or
// a secret, and a name it does not know is refused.
func remoteAct(action string, runner *jobs.Runner, u *update.Updater, lg *slog.Logger) error {
	switch action {
	case api.RemoteBackup:
		if runner == nil {
			return errors.New("this device runs no backups")
		}
		runner.RunNow("")
	case api.RemoteVerify:
		if runner == nil {
			return errors.New("this device runs no backups")
		}
		runner.RunNow("verify")
	case api.RemoteUpdateCheck:
		if u == nil {
			return errors.New("updates are off on this device")
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := u.Check(ctx); err != nil {
				lg.Info("update check asked from another device failed", "err", err)
			}
		}()
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return nil
}

// remoteError says in the member's terms why another device gave no view.
func remoteError(err error) error {
	var se *shareproto.StatusError
	if errors.As(err, &se) {
		switch {
		case se.Status == shareproto.StatusBad && strings.Contains(se.Message, "unknown request"),
			se.Status == shareproto.StatusRefused && strings.Contains(se.Message, "not a storage node"):
			return errors.New("the device runs a version that does not offer this yet")
		case se.Message != "":
			return errors.New(se.Message)
		}
	}
	return err
}

// askRemote is this device's side: the page's two actions over the share
// client. self is refused, a device has its own page for that.
func askRemote(client *shareproto.Client, self identity.Key) (view func(context.Context, string) (*api.RemoteView, error), act func(context.Context, string, string) error) {
	parse := func(device string) (identity.Key, error) {
		k, err := identity.ParseKey(device)
		if err == nil && k == self {
			err = errors.New("that is this device")
		}
		return k, err
	}
	view = func(ctx context.Context, device string) (*api.RemoteView, error) {
		k, err := parse(device)
		if err != nil {
			return nil, err
		}
		body, err := client.View(ctx, k)
		if err != nil {
			return nil, remoteError(err)
		}
		var v api.RemoteView
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, fmt.Errorf("the device's answer could not be read: %w", err)
		}
		return &v, nil
	}
	act = func(ctx context.Context, device, action string) error {
		k, err := parse(device)
		if err != nil {
			return err
		}
		return remoteError(client.Act(ctx, k, action))
	}
	return view, act
}

// ownDevice finds the device of the member that a word names: a name
// this device knows, whatever its case, a whole key, or the start of one
// that fits a single device.
func ownDevice(devices []api.Device, self, word string) (string, error) {
	var found []string
	for _, d := range devices {
		if strings.EqualFold(d.Name, word) || d.Key == word {
			return d.Key, nil
		}
		if strings.HasPrefix(d.Key, strings.ToLower(word)) {
			found = append(found, d.Key)
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return "", fmt.Errorf("%q fits %d of your devices; give more of the key", word, len(found))
	}
	if k, err := identity.ParseKey(word); err == nil {
		if k.String() == self {
			return "", errors.New("that is this device: redundanet open")
		}
		return k.String(), nil
	}
	return "", fmt.Errorf("no device of yours is known here as %q: the page's \"Devices and network\" view lists them, and a whole device key always works", word)
}
