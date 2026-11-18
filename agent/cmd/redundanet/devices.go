package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
)

// devicesFile, in the data directory, lists the devices this one paired or
// certified: their keys, names and roles. `setup` and `join-request` take
// the storage node from it, so its key is never copied by hand into the
// join form.
const devicesFile = "devices.yaml"

type pairedDevice struct {
	Device      string    `yaml:"device"`
	Name        string    `yaml:"name,omitempty"`
	Roles       []string  `yaml:"roles"`
	Certificate string    `yaml:"certificate,omitempty"` // the device's certificate as one line, the proof a device request carries
	PairedAt    time.Time `yaml:"paired_at"`
}

func loadDevices(dd string) ([]pairedDevice, error) {
	b, err := os.ReadFile(filepath.Join(dd, devicesFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []pairedDevice
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// recordDevice adds a device, replacing an earlier record of the same key.
func recordDevice(dd string, d pairedDevice) error {
	devices, err := loadDevices(dd)
	if err != nil {
		return err
	}
	kept := devices[:0]
	for _, old := range devices {
		if old.Device != d.Device {
			kept = append(kept, old)
		}
	}
	kept = append(kept, d)
	b, err := yaml.Marshal(kept)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dd, devicesFile), b, 0o600)
}

// serverChoiceFile holds the member's word on whether this computer is
// their storage server: "yes" or "no". The storage role alone is not that
// word. A computer set up as a server says yes when its identity is made;
// one that got the role before the question existed is asked once, by
// `join-request`.
const serverChoiceFile = "storage-server"

// serverChoice is the recorded answer, and whether there is one.
func serverChoice(dd string) (yes, known bool) {
	b, err := os.ReadFile(filepath.Join(dd, serverChoiceFile))
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(b)) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

func recordServerChoice(dd string, yes bool) error {
	v := "no\n"
	if yes {
		v = "yes\n"
	}
	if err := os.MkdirAll(dd, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dd, serverChoiceFile), []byte(v), 0o600)
}

// withSelf adds this device to the storage devices a request can name,
// after the nodes it paired, when its member said it is their storage
// server: a request never names a computer because of its role alone,
// which is how a laptop came to be listed in place of the node its member
// meant. The record carries the device's own certificate, the proof a
// device request needs.
func withSelf(dd string, st *identity.State, devices []pairedDevice) []pairedDevice {
	c := st.Certificate.Certificate
	if yes, _ := serverChoice(dd); !yes || !slices.Contains(c.Roles, identity.RoleStorage) {
		return devices
	}
	self := pairedDevice{Device: c.Device.String(), Roles: c.Roles}
	self.Name, _ = os.Hostname()
	self.Certificate, _ = encodeCertificate(&st.Certificate)
	return append(append([]pairedDevice(nil), devices...), self)
}

// storageDevice is the first storage device on record, for the join request.
func storageDevice(devices []pairedDevice) *pairedDevice {
	return storageDeviceNotIn(devices, nil)
}

// storageDeviceNotIn is the first storage device on record that the
// manifest does not list yet: the one a listed member still has to request.
func storageDeviceNotIn(devices []pairedDevice, listed map[string]bool) *pairedDevice {
	for i := range devices {
		if listed[devices[i].Device] {
			continue
		}
		for _, r := range devices[i].Roles {
			if r == "storage" {
				return &devices[i]
			}
		}
	}
	return nil
}
