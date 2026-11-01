// Package service installs the agent as a per-user service that starts at
// login and restarts on failure, with no administrator rights: a launchd
// user agent on macOS, a systemd user unit on Linux, a logon task on
// Windows. An always-on storage node on macOS or Windows takes one more
// step, a LaunchDaemon or a Windows service, which is not here yet.
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Spec is what the service runs.
type Spec struct {
	Executable string   // absolute path of the binary
	Args       []string // arguments after the executable, for example agent --network NAME
	DataDir    string   // for the log file
	Label      string   // service name; default redundanet
}

func (s Spec) label() string {
	if s.Label == "" {
		return "redundanet"
	}
	return s.Label
}

// Status of the installed service.
type Status struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Path      string `json:"path,omitempty"` // the unit, plist or task
	Detail    string `json:"detail,omitempty"`
}

// Install writes the service definition and starts it.
func Install(s Spec) (string, error) {
	if !filepath.IsAbs(s.Executable) {
		return "", fmt.Errorf("executable must be an absolute path, got %q", s.Executable)
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(s)
	case "linux":
		return installSystemd(s)
	case "windows":
		return installTask(s)
	}
	return "", fmt.Errorf("no service integration for %s", runtime.GOOS)
}

// Uninstall stops and removes the service.
func Uninstall(label string) error {
	s := Spec{Label: label}
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(s)
	case "linux":
		return uninstallSystemd(s)
	case "windows":
		return uninstallTask(s)
	}
	return fmt.Errorf("no service integration for %s", runtime.GOOS)
}

// Query reports the service's state.
func Query(label string) Status {
	s := Spec{Label: label}
	switch runtime.GOOS {
	case "darwin":
		return statusLaunchd(s)
	case "linux":
		return statusSystemd(s)
	case "windows":
		return statusTask(s)
	}
	return Status{Detail: "no service integration for " + runtime.GOOS}
}

// The definitions are plain functions of the spec, so they are tested on
// every platform without installing anything.

// LaunchdPlist is the launchd user agent.
func LaunchdPlist(s Spec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape("org.redundanet."+s.label()) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range append([]string{s.Executable}, s.Args...) {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(s.DataDir, "service.log")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(s.DataDir, "service.log")) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// SystemdUnit is the systemd user unit.
func SystemdUnit(s Spec) string {
	exec := make([]string, 0, 1+len(s.Args))
	for _, a := range append([]string{s.Executable}, s.Args...) {
		exec = append(exec, systemdQuote(a))
	}
	return `[Unit]
Description=RedundaNet agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=` + strings.Join(exec, " ") + `
Restart=always
RestartSec=10
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`
}

// TaskXML is the Windows scheduled task: at logon, restart on failure.
func TaskXML(s Spec) string {
	args := make([]string, 0, len(s.Args))
	for _, a := range s.Args {
		args = append(args, windowsQuote(a))
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>RedundaNet agent</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
    <StartWhenAvailable>true</StartWhenAvailable>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(s.Executable) + `</Command>
      <Arguments>` + xmlEscape(strings.Join(args, " ")) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\$") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`).Replace(s) + `"`
}

func windowsQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func home() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return h, nil
}
