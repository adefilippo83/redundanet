package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/jobs"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

type recorder struct{ calls [][]string }

func (r *recorder) run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return "", nil
}

// stack lays out a RedundaNet 2 installation: compose file, override, .env
// with backup-sync on, and returns the config and the synced folder.
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
	rec := &recorder{}
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
	if strings.Join(p.Compose, " ") != strings.Join(want, " ") {
		t.Fatalf("compose command: %v", p.Compose)
	}
	if !strings.Contains(p.String(), "plan:") || strings.Contains(p.String(), "not yet") {
		t.Fatalf("plan text: %s", p)
	}
	if err := Apply(cfg, p, agent, false); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || strings.Join(rec.calls[0], " ") != strings.Join(want, " ")+" down --timeout 60" {
		t.Fatalf("calls: %v", rec.calls)
	}
	at, ok := Migrated(cfg.DataDir)
	if !ok || at != cfg.Now() {
		t.Fatalf("marker: %v %v", at, ok)
	}
	if err := Apply(cfg, p, agent, false); err == nil || !strings.Contains(err.Error(), "already marked") {
		t.Fatalf("second apply: %v", err)
	}
	if err := Rollback(cfg); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 2 || strings.Join(rec.calls[1], " ") != strings.Join(want, " ")+" up -d" {
		t.Fatalf("calls: %v", rec.calls)
	}
	if _, ok := Migrated(cfg.DataDir); ok {
		t.Fatal("marker survived the rollback")
	}
	if err := Rollback(cfg); err == nil {
		t.Fatal("rollback without a marker succeeded")
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
			if len(rec.calls) != 0 {
				t.Fatalf("the stack was touched: %v", rec.calls)
			}
		})
	}
	// Forced, it proceeds and the marker says so.
	p, _ := NewPlan(cfg, &Agent{Device: "dev", HubsUp: 1, Jobs: &jobs.Status{}})
	if err := Apply(cfg, p, &Agent{Device: "dev"}, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.DataDir, Marker))
	if !strings.Contains(string(data), `"forced": true`) || len(rec.calls) != 1 {
		t.Fatalf("forced marker: %s %v", data, rec.calls)
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
