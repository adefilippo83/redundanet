package main

import (
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// devicesFile, in the data directory, lists the devices this one paired or
// certified: their keys, names and roles. `setup` and `join-request` take
// the storage node from it, so its key is never copied by hand into the
// join form.
const devicesFile = "devices.yaml"

type pairedDevice struct {
	Device   string    `yaml:"device"`
	Name     string    `yaml:"name,omitempty"`
	Roles    []string  `yaml:"roles"`
	PairedAt time.Time `yaml:"paired_at"`
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

// storageDevice is the first storage device on record, for the join request.
func storageDevice(devices []pairedDevice) *pairedDevice {
	for i := range devices {
		for _, r := range devices[i].Roles {
			if r == "storage" {
				return &devices[i]
			}
		}
	}
	return nil
}
