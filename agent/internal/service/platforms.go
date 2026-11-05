package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// ---- macOS: launchd user agent ----

func plistPath(s Spec) (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "Library", "LaunchAgents", "org.redundanet."+s.label()+".plist"), nil
}

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func installLaunchd(s Spec) (string, error) {
	p, err := plistPath(s)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	target := launchdDomain() + "/org.redundanet." + s.label()
	_, _ = run("launchctl", "bootout", target)
	waitLaunchdGone(target)
	if err := os.WriteFile(p, []byte(LaunchdPlist(s)), 0o644); err != nil {
		return "", err
	}
	return p, bootstrapLaunchd(p)
}

// launchctl bootout returns before launchd has torn the old instance
// down, and a bootstrap that arrives while the label still exists fails
// with "Input/output error" (5): a reinstall of a running service hit
// that. waitLaunchdGone polls until the label is gone, for as long as
// launchd itself gives a service to stop, and bootstrapLaunchd retries
// past that, since the moment the label becomes free is not observable.
func waitLaunchdGone(target string) {
	deadline := time.Now().Add(launchdStopWait)
	for time.Now().Before(deadline) {
		if _, err := run("launchctl", "print", target); err != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// launchdStopWait is launchd's own SIGTERM to SIGKILL grace (20 s) and
// some margin.
var launchdStopWait = 25 * time.Second

func bootstrapLaunchd(p string) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		if _, err = run("launchctl", "bootstrap", launchdDomain(), p); err == nil || !strings.Contains(err.Error(), "Input/output error") {
			return err
		}
	}
	return err
}

func uninstallLaunchd(s Spec) error {
	p, err := plistPath(s)
	if err != nil {
		return err
	}
	_, _ = run("launchctl", "bootout", launchdDomain()+"/org.redundanet."+s.label())
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func statusLaunchd(s Spec) Status {
	p, err := plistPath(s)
	if err != nil {
		return Status{Detail: err.Error()}
	}
	st := Status{Path: p}
	if _, err := os.Stat(p); err != nil {
		return st
	}
	st.Installed = true
	out, err := run("launchctl", "print", launchdDomain()+"/org.redundanet."+s.label())
	if err != nil {
		st.Detail = "not loaded"
		return st
	}
	st.Running = strings.Contains(out, "state = running")
	return st
}

// ---- Linux: systemd user unit ----

func unitPath(s Spec) (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(h, ".config")
	}
	return filepath.Join(dir, "systemd", "user", s.label()+".service"), nil
}

func installSystemd(s Spec) (string, error) {
	p, err := unitPath(s)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(SystemdUnit(s)), 0o644); err != nil {
		return "", err
	}
	if _, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return p, err
	}
	if _, err := run("systemctl", "--user", "enable", "--now", s.label()+".service"); err != nil {
		return p, err
	}
	// Linger keeps the user's services running with nobody logged in. It
	// may need a polkit prompt; a failure is reported, not fatal.
	if u := os.Getenv("USER"); u != "" {
		if out, err := run("loginctl", "enable-linger", u); err != nil {
			return p, fmt.Errorf("installed and started, but lingering could not be enabled, so the service stops at logout: %s", strings.TrimSpace(out))
		}
	}
	return p, nil
}

func uninstallSystemd(s Spec) error {
	p, err := unitPath(s)
	if err != nil {
		return err
	}
	_, _ = run("systemctl", "--user", "disable", "--now", s.label()+".service")
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _ = run("systemctl", "--user", "daemon-reload")
	return nil
}

func statusSystemd(s Spec) Status {
	p, err := unitPath(s)
	if err != nil {
		return Status{Detail: err.Error()}
	}
	st := Status{Path: p}
	if _, err := os.Stat(p); err != nil {
		return st
	}
	st.Installed = true
	out, _ := exec.Command("systemctl", "--user", "is-active", s.label()+".service").Output()
	st.Running = strings.TrimSpace(string(out)) == "active"
	st.Detail = strings.TrimSpace(string(out))
	return st
}

// ---- Windows: scheduled task at logon ----

func taskName(s Spec) string { return "RedundaNet " + s.label() }

func installTask(s Spec) (string, error) {
	tmp := filepath.Join(os.TempDir(), "redundanet-task.xml")
	// schtasks reads UTF-16 with a BOM.
	if err := os.WriteFile(tmp, utf16le(TaskXML(s)), 0o644); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if _, err := run("schtasks", "/Create", "/TN", taskName(s), "/XML", tmp, "/F"); err != nil {
		return "", err
	}
	if _, err := run("schtasks", "/Run", "/TN", taskName(s)); err != nil {
		return taskName(s), err
	}
	return taskName(s), nil
}

func uninstallTask(s Spec) error {
	_, _ = run("schtasks", "/End", "/TN", taskName(s))
	_, err := run("schtasks", "/Delete", "/TN", taskName(s), "/F")
	return err
}

func statusTask(s Spec) Status {
	st := Status{Path: taskName(s)}
	out, err := run("schtasks", "/Query", "/TN", taskName(s), "/FO", "LIST")
	if err != nil {
		return st
	}
	st.Installed = true
	st.Running = strings.Contains(out, "Running")
	return st
}

func utf16le(s string) []byte {
	out := []byte{0xff, 0xfe}
	for _, r := range s {
		if r > 0xffff {
			r = '?'
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
