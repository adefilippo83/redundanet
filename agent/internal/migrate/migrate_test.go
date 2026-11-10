package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// recorder stands in for docker: it records every command, answers the
// container listing and the profile listing, and plays the stack: a down
// leaves whatever `survive` says (nothing, unless a test sets it), an up
// brings the named services back.
type recorder struct {
	calls      [][]string
	containers []string
	profiles   string
	survive    []string
}

func (r *recorder) run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	cmd := strings.Join(args, " ")
	switch {
	case name == "docker" && len(args) > 0 && args[0] == "ps":
		return strings.Join(r.containers, "\n") + "\n", nil
	case strings.HasSuffix(cmd, "config --profiles"):
		return r.profiles, nil
	case strings.Contains(cmd, " down "):
		r.containers = r.survive
	case strings.Contains(cmd, " up -d "):
		r.containers = args[len(args)-3:]
	}
	return "", nil
}

// changes are the recorded commands that touch the stack, leaving out
// the listings a plan makes.
func (r *recorder) changes() []string {
	var out []string
	for _, c := range r.calls {
		if s := strings.Join(c, " "); strings.Contains(s, " down ") || strings.Contains(s, " up ") {
			out = append(out, s)
		}
	}
	return out
}

// stack lays out a RedundaNet 2 installation: compose file, override, .env
// with backup-sync on, three containers behind two profiles, and returns
// the config and the synced folder.
func stack(t *testing.T, env string) (Config, *recorder, string) {
	t.Helper()
	root := t.TempDir()
	composeDir := filepath.Join(root, "opt", "redundanet", "docker")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(root, "mnt", "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"docker-compose.yml", "docker-compose.override.yml"} {
		if err := os.WriteFile(filepath.Join(composeDir, f), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	envFile := filepath.Join(root, "opt", "redundanet", ".env")
	if env == "" {
		env = "SYNC_ENABLED=true\nSYNC_DIR=" + share + "\n"
	}
	if err := os.WriteFile(envFile, []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "var", "lib", "redundanet3")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{containers: []string{"tahoe-storage", "tinc", "tahoe-client"}, profiles: "storage\nintroducer\nclient\n"}
	return Config{ComposeDir: composeDir, EnvFile: envFile, Project: "redundanet", DataDir: data, Run: rec.run,
		Now: func() time.Time { return time.Date(2027, 8, 1, 12, 0, 0, 0, time.UTC) }}, rec, share
}

// ready is an agent whose native set holds the synced folder, ran to
// completion and passed a restore test.
func ready(share string) *Agent {
	return &Agent{Device: "dev", HubsUp: 1, Jobs: &jobs.Status{
		Sets:          []backup.Set{{Name: "share", Folders: []string{filepath.Dir(share)}}},
		Runs:          []jobs.RunStatus{{Set: "share", Last: &backup.Result{Snapshot: snapshot.Snapshot{Files: 10}}}},
		Verifications: []localdb.Verification{{Sampled: 5, Verified: 5}},
	}}
}

func TestPlanStopsAndRollsBack(t *testing.T) {
	cfg, rec, share := stack(t, "")
	agent := ready(share)
	p, err := NewPlan(cfg, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Refusals) != 0 || p.SyncDir != share || p.Covered != "share" {
		t.Fatalf("plan: %+v", p)
	}
	want := []string{"docker", "compose", "-p", "redundanet", "-f", filepath.Join(cfg.ComposeDir, "docker-compose.yml"),
		"-f", filepath.Join(cfg.ComposeDir, "docker-compose.override.yml"), "--env-file", cfg.EnvFile}
	base := strings.Join(want, " ")
	if strings.Join(p.Compose, " ") != base {
		t.Fatalf("compose command: %v", p.Compose)
	}
	// tinc first, the rest sorted; every profile the file defines, sorted.
	if strings.Join(p.Services, ",") != "tinc,tahoe-client,tahoe-storage" || strings.Join(p.Profiles, ",") != "client,introducer,storage" {
		t.Fatalf("services %v, profiles %v", p.Services, p.Profiles)
	}
	if s := p.String(); !strings.Contains(s, "plan:") || strings.Contains(s, "not yet") || !strings.Contains(s, "stop the stack (tinc, tahoe-client, tahoe-storage)") {
		t.Fatalf("plan text: %s", s)
	}
	if len(rec.changes()) != 0 {
		t.Fatalf("the plan touched the stack: %v", rec.changes())
	}
	if err := Apply(cfg, p, agent, false); err != nil {
		t.Fatal(err)
	}
	down := base + " --profile client --profile introducer --profile storage down --timeout 60"
	if ch := rec.changes(); len(ch) != 1 || ch[0] != down {
		t.Fatalf("apply ran: %v", ch)
	}
	// The listing after the down is the check that nothing survived.
	last := strings.Join(rec.calls[len(rec.calls)-1], " ")
	if !strings.HasPrefix(last, "docker ps -a --filter label=com.docker.compose.project=redundanet") {
		t.Fatalf("no check after the down: %s", last)
	}
	at, ok := Migrated(cfg.DataDir)
	if !ok || at != cfg.Now() {
		t.Fatalf("marker: %v %v", at, ok)
	}
	var m marker
	data, _ := os.ReadFile(filepath.Join(cfg.DataDir, Marker))
	if err := json.Unmarshal(data, &m); err != nil || strings.Join(m.Services, ",") != "tinc,tahoe-client,tahoe-storage" {
		t.Fatalf("marker services: %s", data)
	}
	if err := Apply(cfg, p, agent, false); err == nil || !strings.Contains(err.Error(), "already marked") {
		t.Fatalf("second apply: %v", err)
	}
	if err := Rollback(cfg); err != nil {
		t.Fatal(err)
	}
	// All of them together, tinc first, recreated: never tinc alone.
	up := base + " up -d --force-recreate tinc tahoe-client tahoe-storage"
	if ch := rec.changes(); len(ch) != 2 || ch[1] != up {
		t.Fatalf("rollback ran: %v", ch)
	}
	if _, ok := Migrated(cfg.DataDir); ok {
		t.Fatal("marker survived the rollback")
	}
	if err := Rollback(cfg); err == nil {
		t.Fatal("rollback without a marker succeeded")
	}
}

// TestSurvivorsPutTheStackBack is the first drill on falcon-1: a down
// that misses the profiled services leaves the Tahoe containers on tinc's
// dead network namespace. Now that is a failure that restores the stack
// and writes no marker.
func TestSurvivorsPutTheStackBack(t *testing.T) {
	cfg, rec, share := stack(t, "")
	rec.survive = []string{"tahoe-client", "tahoe-storage"}
	agent := ready(share)
	p, err := NewPlan(cfg, agent)
	if err != nil {
		t.Fatal(err)
	}
	err = Apply(cfg, p, agent, false)
	if err == nil || !strings.Contains(err.Error(), "tahoe-client, tahoe-storage survived") || !strings.Contains(err.Error(), "nothing marked") {
		t.Fatalf("apply with survivors: %v", err)
	}
	ch := rec.changes()
	if len(ch) != 2 || !strings.Contains(ch[0], " down ") || !strings.HasSuffix(ch[1], " up -d --force-recreate tinc tahoe-client tahoe-storage") {
		t.Fatalf("ran: %v", ch)
	}
	if _, ok := Migrated(cfg.DataDir); ok {
		t.Fatal("a marker was written with the stack half down")
	}
}

func TestNothingRunningIsNothingToStop(t *testing.T) {
	cfg, rec, share := stack(t, "")
	rec.containers = nil
	agent := ready(share)
	p, err := NewPlan(cfg, agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Services) != 0 || !strings.Contains(p.String(), "nothing for migrate-node to stop") {
		t.Fatalf("plan: %s", p)
	}
	if err := Apply(cfg, p, agent, true); err == nil || !strings.Contains(err.Error(), "nothing to stop") {
		t.Fatalf("apply with no containers: %v", err)
	}
	if len(rec.changes()) != 0 {
		t.Fatalf("ran: %v", rec.changes())
	}
}

func TestOldMarkerIsNotRolledBackBlind(t *testing.T) {
	cfg, rec, _ := stack(t, "")
	old := `{"at":"2026-11-05T15:00:00Z","device":"dev","compose":["docker","compose","-p","redundanet","-f","x.yml"]}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, Marker), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(cfg); err == nil || !strings.Contains(err.Error(), "names no services") {
		t.Fatalf("rollback of an old marker: %v", err)
	}
	if len(rec.changes()) != 0 {
		t.Fatalf("ran: %v", rec.changes())
	}
}

func TestPlanRefuses(t *testing.T) {
	cfg, rec, share := stack(t, "")
	cases := []struct {
		name  string
		agent *Agent
		want  string
	}{
		{"no hub", &Agent{Jobs: ready(share).Jobs}, "not connected to any hub"},
		{"no set", &Agent{HubsUp: 1, Jobs: &jobs.Status{}}, "no native backup set holds it"},
		{"no sets at all", &Agent{HubsUp: 1}, "reports no backup sets"},
		{"never ran", &Agent{HubsUp: 1, Jobs: &jobs.Status{Sets: ready(share).Jobs.Sets}}, "has not completed a run"},
		{"failed run", func() *Agent { a := ready(share); a.Jobs.Runs[0].LastErr = "disk full"; return a }(), "failed its last run"},
		{"partial run", func() *Agent { a := ready(share); a.Jobs.Runs[0].Last.Snapshot.Partial = true; return a }(), "only partially"},
		{"no restore test", func() *Agent { a := ready(share); a.Jobs.Verifications = nil; return a }(), "no restore test has passed"},
		{"failed restore test", func() *Agent {
			a := ready(share)
			a.Jobs.Verifications = []localdb.Verification{{Sampled: 5, Verified: 4, Failed: []string{"x"}}}
			return a
		}(), "no restore test has passed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := NewPlan(cfg, c.agent)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Refusals) == 0 || !strings.Contains(strings.Join(p.Refusals, "\n"), c.want) {
				t.Fatalf("refusals: %v", p.Refusals)
			}
			if !strings.Contains(p.String(), "--force") {
				t.Fatalf("plan text does not say how to force: %s", p)
			}
			if err := Apply(cfg, p, c.agent, false); err == nil {
				t.Fatal("applied with refusals")
			}
			if len(rec.changes()) != 0 {
				t.Fatalf("the stack was touched: %v", rec.changes())
			}
		})
	}
	// Forced, it proceeds and the marker says so.
	p, _ := NewPlan(cfg, &Agent{Device: "dev", HubsUp: 1, Jobs: &jobs.Status{}})
	if err := Apply(cfg, p, &Agent{Device: "dev"}, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.DataDir, Marker))
	if !strings.Contains(string(data), `"forced": true`) || len(rec.changes()) != 1 {
		t.Fatalf("forced marker: %s %v", data, rec.changes())
	}
}

func TestPlanWithoutSyncOrOverride(t *testing.T) {
	cfg, _, _ := stack(t, "SYNC_ENABLED=false\nSYNC_DIR=/mnt/share\n")
	if err := os.Remove(filepath.Join(cfg.ComposeDir, "docker-compose.override.yml")); err != nil {
		t.Fatal(err)
	}
	p, err := NewPlan(cfg, &Agent{HubsUp: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.SyncDir != "" || len(p.Refusals) != 0 || len(p.Notes) != 2 {
		t.Fatalf("plan: %+v", p)
	}
	if strings.Contains(strings.Join(p.Compose, " "), "override") {
		t.Fatalf("a missing override was passed: %v", p.Compose)
	}
	// No stack at all is an error, not a plan.
	cfg.ComposeDir = t.TempDir()
	if _, err := NewPlan(cfg, &Agent{HubsUp: 1}); err == nil || !strings.Contains(err.Error(), "no RedundaNet 2 stack") {
		t.Fatalf("missing stack: %v", err)
	}
}

func TestEnvAndRelativeSyncDir(t *testing.T) {
	cfg, _, _ := stack(t, "# the stack\nexport SYNC_ENABLED=\"yes\"\nSYNC_DIR='./sync'\nSTORAGE_PATH=/mnt/storage\n\nbroken line\n")
	env, err := readEnv(cfg.EnvFile)
	if err != nil || env["SYNC_ENABLED"] != "yes" || env["SYNC_DIR"] != "./sync" || env["STORAGE_PATH"] != "/mnt/storage" || len(env) != 3 {
		t.Fatalf("env: %v %v", env, err)
	}
	p, err := NewPlan(cfg, &Agent{HubsUp: 1})
	if err != nil {
		t.Fatal(err)
	}
	if p.SyncDir != filepath.Join(cfg.ComposeDir, "sync") {
		t.Fatalf("relative sync dir: %s", p.SyncDir)
	}
	env, err = readEnv(filepath.Join(t.TempDir(), "none"))
	if err != nil || len(env) != 0 {
		t.Fatalf("missing env file: %v %v", env, err)
	}
}

// The marker lies in a directory the service user owns, and the rollback
// runs as root. Whatever the marker says the command is, the command that
// runs is the one built from the stack's files; a service name that could
// be read as anything else stops the rollback before a command runs.
func TestRollbackDoesNotRunWhatTheMarkerSays(t *testing.T) {
	cfg, rec, _ := stack(t, "")
	write := func(m string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg.DataDir, Marker), []byte(m), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A marker that names another program: its services are started with
	// this node's own compose command, and nothing of its command runs.
	write(`{"at":"2026-11-05T15:00:00Z","device":"dev","compose":["/tmp/evil","--now"],"services":["tinc","tahoe-storage"]}`)
	if err := Rollback(cfg); err != nil {
		t.Fatal(err)
	}
	ch := rec.changes()
	want, _ := composeCommand(cfg)
	if len(ch) != 1 || ch[0] != strings.Join(want, " ")+" up -d --force-recreate tinc tahoe-storage" || strings.Contains(ch[0], "evil") {
		t.Fatalf("rollback ran: %v", ch)
	}
	// A marker from before this version, with the command it recorded then:
	// its services still start.
	for _, bad := range []string{"--volume=/:/host", "-f", "a b", "../x", "tinc;reboot", ""} {
		before := len(rec.changes())
		b, _ := json.Marshal(map[string]any{"at": "2026-11-05T15:00:00Z", "device": "dev", "compose": []string{"docker", "compose"}, "services": []string{"tinc", bad}})
		write(string(b))
		if err := Rollback(cfg); err == nil || !strings.Contains(err.Error(), "not the name of a compose service") {
			t.Fatalf("a marker naming %q: %v", bad, err)
		}
		if len(rec.changes()) != before {
			t.Fatalf("a marker naming %q ran: %v", bad, rec.changes()[before:])
		}
		if _, ok := Migrated(cfg.DataDir); !ok {
			t.Fatalf("the marker naming %q was removed", bad)
		}
	}
}

// Root writes the marker in a directory the service user owns: a link
// planted under the marker's name, or its temporary name, is replaced,
// not followed.
func TestTheMarkerIsNotWrittenThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, Marker)
	for _, link := range []string{path, path + ".new"} {
		_ = os.Remove(path)
		_ = os.Remove(path + ".new")
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		if err := writeNew(path, []byte("marker\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(victim); string(b) != "mine" {
			t.Fatalf("with a link at %s the file it points to was written: %q", filepath.Base(link), b)
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("with a link at %s the marker is %v %v", filepath.Base(link), fi, err)
		}
	}
}
