package service

import (
	"strings"
	"testing"
)

func TestDefinitions(t *testing.T) {
	s := Spec{Executable: `/Applications/Redunda Net/redundanet`, Args: []string{"agent", "--network", "red & net", "--data-dir", `/Users/a b/Library/Application Support/redundanet`}, DataDir: "/Users/a b/Library/Application Support/redundanet"}
	plist := LaunchdPlist(s)
	for _, want := range []string{"<key>KeepAlive</key><true/>", "<string>/Applications/Redunda Net/redundanet</string>", "<string>red &amp; net</string>", "org.redundanet.redundanet"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q", want)
		}
	}
	unit := SystemdUnit(s)
	for _, want := range []string{`ExecStart="/Applications/Redunda Net/redundanet" agent --network "red & net"`, "Restart=always", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	task := TaskXML(s)
	for _, want := range []string{"<LogonTrigger>", "<RestartOnFailure>", "<Command>/Applications/Redunda Net/redundanet</Command>", `&quot;red &amp; net&quot;`} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks %q", want)
		}
	}
	if _, err := Install(Spec{Executable: "relative/path"}); err == nil {
		t.Fatal("a relative executable was accepted")
	}
	if b := utf16le("A"); len(b) != 4 || b[0] != 0xff || b[2] != 'A' {
		t.Fatalf("utf16: %v", b)
	}
}
