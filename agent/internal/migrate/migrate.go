// Package migrate retires the RedundaNet 2 stack on a node that already
// runs the agent beside it: the RFC's `migrate-node`, phase 3. The stack is
// stopped and removed as containers, never as files: the compose files, the
// .env and the Tahoe share directories stay on disk, so `--rollback` starts
// it again, and the shares go only on the network's stop date, which is a
// separate decision. The agent is untouched; it was a host service already.
package migrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/jobs"
)

// Where the RedundaNet 2 installer puts the stack.
const (
	DefaultComposeDir = "/opt/redundanet/docker"
	DefaultEnvFile    = "/opt/redundanet/.env"
	DefaultProject    = "redundanet"
	// Marker is the file in the agent's data directory that says the stack
	// was retired here, holding what rollback needs.
	Marker = "migrated"
)

// overrideNames are the files Docker would load beside the compose file
// on its own; passing -f disables that, so every one present is passed
// explicitly. A storage node's disk bind-mount lives there.
var overrideNames = []string{
	"docker-compose.override.yml", "docker-compose.override.yaml",
	"compose.override.yml", "compose.override.yaml",
}

// stopTimeout is how long compose gives each container to stop; Tahoe
// closes its lease crawler state on the way out.
const stopTimeout = "60"

// Config says where the stack is and how commands run.
type Config struct {
	ComposeDir string
	EnvFile    string
	Project    string
	DataDir    string
	Run        func(name string, args ...string) (string, error)
	Now        func() time.Time
}

// Agent is what the plan needs to know about the running agent.
type Agent struct {
	Device string
	HubsUp int
	Jobs   *jobs.Status
}

// Plan is what migrate-node would do on this node, and why it might not.
type Plan struct {
	Compose  []string // the compose command, up to the subcommand
	Services []string // the stack's containers by service, tinc first
	Profiles []string // every compose profile the file defines
	SyncDir  string   // the folder backup-sync pushed to the old grid; "" when sync was off
	Covered  string   // the native backup set that holds SyncDir
	Refusals []string // what is not yet on the native grid, or not ready
	Notes    []string
	Steps    []string
}

// marker is what Marker holds.
type marker struct {
	At       time.Time `json:"at"`
	Device   string    `json:"device"`
	Compose  []string  `json:"compose"`
	Services []string  `json:"services"`
	SyncDir  string    `json:"sync_dir,omitempty"`
	Forced   bool      `json:"forced,omitempty"`
}

// Exec runs a command and returns its combined output.
func Exec(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// NewPlan reads the stack's files, asks Docker what runs, reads the
// agent's state and says what would happen. It changes nothing.
func NewPlan(cfg Config, agent *Agent) (*Plan, error) {
	composeFile := filepath.Join(cfg.ComposeDir, "docker-compose.yml")
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("no RedundaNet 2 stack here: %s: %w", composeFile, err)
	}
	p := &Plan{Compose: []string{"docker", "compose", "-p", cfg.Project, "-f", composeFile}}
	overrides := 0
	for _, name := range overrideNames {
		if f := filepath.Join(cfg.ComposeDir, name); exists(f) {
			p.Compose = append(p.Compose, "-f", f)
			overrides++
		}
	}
	if exists(cfg.EnvFile) {
		p.Compose = append(p.Compose, "--env-file", cfg.EnvFile)
	}
	if overrides == 0 {
		p.Notes = append(p.Notes, "no override file beside the compose file: this stack has no storage disk bind-mount of its own")
	}
	var err error
	if p.Services, err = services(cfg); err != nil {
		return nil, err
	}
	if p.Profiles, err = profiles(cfg, p.Compose); err != nil {
		return nil, err
	}
	if len(p.Services) == 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("no container of the compose project %q exists: the stack is not running here, and there is nothing for migrate-node to stop", cfg.Project))
	}

	env, err := readEnv(cfg.EnvFile)
	if err != nil {
		return nil, err
	}
	if truthy(env["SYNC_ENABLED"]) {
		p.SyncDir = env["SYNC_DIR"]
		if p.SyncDir == "" {
			p.SyncDir = "./sync"
		}
		if !filepath.IsAbs(p.SyncDir) {
			p.SyncDir = filepath.Join(cfg.ComposeDir, p.SyncDir)
		}
		p.SyncDir = filepath.Clean(p.SyncDir)
	} else {
		p.Notes = append(p.Notes, "backup-sync was off on this node: nothing went to the old grid from here by itself; what was uploaded by other means is yours to check")
	}

	if agent.HubsUp == 0 {
		p.Refusals = append(p.Refusals, "the agent is not connected to any hub, so the node would leave the status page")
	}
	if p.SyncDir != "" {
		p.Refusals = append(p.Refusals, coverage(p.SyncDir, agent.Jobs)...)
	}
	if p.SyncDir != "" && p.Covered == "" {
		if set := covering(p.SyncDir, agent.Jobs); set != "" {
			p.Covered = set
		}
	}

	p.Steps = []string{
		fmt.Sprintf("stop the stack (%s): %s", strings.Join(p.Services, ", "), strings.Join(p.down(), " ")),
		"  every container at once, tinc last by its dependencies; every profile named, since the Tahoe services sit behind one each",
		"  then check that no container of the project is left: one would run on a dead network namespace, so the stack is started again and migrate-node fails",
		"  the compose files, the .env and the Tahoe share directories stay on disk",
		"mark this node migrated: " + filepath.Join(cfg.DataDir, Marker),
		"the agent keeps running as it is; the share directories go on the network's stop date, not today",
	}
	return p, nil
}

// down is the compose command that stops and removes the whole stack.
// A service behind a profile is invisible to compose unless its profile
// is named, so every profile the file defines is; `down` alone would
// remove tinc and leave the Tahoe containers running on its dead
// network namespace, which the first drill did.
func (p *Plan) down() []string {
	args := append([]string{}, p.Compose...)
	for _, pr := range p.Profiles {
		args = append(args, "--profile", pr)
	}
	return append(args, "down", "--timeout", stopTimeout)
}

// up is the compose command that starts the services the marker names:
// naming them enables their profiles, --force-recreate rebuilds any
// container that exists, and depends_on orders tinc before the Tahoe
// services, so they join its new network namespace, never a dead one.
// It is the Python CLI's recreate, the one safe way to restart this stack.
func up(compose, services []string) []string {
	return append(append(append([]string{}, compose...), "up", "-d", "--force-recreate"), services...)
}

// String is the plan as migrate-node prints it.
func (p *Plan) String() string {
	var b strings.Builder
	if p.SyncDir != "" {
		fmt.Fprintf(&b, "backup-sync pushed %s to the old grid", p.SyncDir)
		if p.Covered != "" {
			fmt.Fprintf(&b, "; the native set %q holds it", p.Covered)
		}
		b.WriteString("\n")
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, r := range p.Refusals {
		fmt.Fprintf(&b, "not yet: %s\n", r)
	}
	if len(p.Refusals) > 0 {
		b.WriteString("migrate-node refuses until the above is settled, or runs with --force, losing what it names\n")
	}
	b.WriteString("plan:\n")
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	return b.String()
}

// Apply runs the plan. With refusals it stops unless forced. A container
// that survives the down puts the stack back and fails, rather than
// leaving half of it on a dead network namespace.
func Apply(cfg Config, p *Plan, agent *Agent, force bool) error {
	if len(p.Refusals) > 0 && !force {
		return fmt.Errorf("migrate-node: %d thing(s) not yet settled; see the plan, or use --force", len(p.Refusals))
	}
	if exists(filepath.Join(cfg.DataDir, Marker)) {
		return errors.New("migrate-node: this node is already marked migrated; --rollback first to do it again")
	}
	if len(p.Services) == 0 {
		return fmt.Errorf("migrate-node: no container of the compose project %q exists; nothing to stop, nothing marked", cfg.Project)
	}
	down := p.down()
	if _, err := cfg.Run(down[0], down[1:]...); err != nil {
		return fmt.Errorf("stopping the stack: %w", err)
	}
	left, err := services(cfg)
	if err != nil {
		return fmt.Errorf("after the down: %w", err)
	}
	if len(left) > 0 {
		restore := up(p.Compose, p.Services)
		_, rerr := cfg.Run(restore[0], restore[1:]...)
		msg := fmt.Sprintf("migrate-node: %s survived the down and would run on a dead network namespace; the stack was started again as it was, nothing marked", strings.Join(left, ", "))
		if rerr != nil {
			return fmt.Errorf("%s; and starting it again failed: %w; run by hand: %s", msg, rerr, strings.Join(restore, " "))
		}
		return errors.New(msg)
	}
	m := marker{At: cfg.Now().UTC(), Device: agent.Device, Compose: p.Compose, Services: p.Services, SyncDir: p.SyncDir, Forced: force && len(p.Refusals) > 0}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.DataDir, Marker), append(data, '\n'), 0o644)
}

// Rollback starts the services the marker names, together and tinc
// first, and forgets the migration. The stack's files never left.
func Rollback(cfg Config) error {
	path := filepath.Join(cfg.DataDir, Marker)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("this node is not marked migrated: %w", err)
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || len(m.Compose) < 2 {
		return fmt.Errorf("%s is not a migration marker", path)
	}
	if len(m.Services) == 0 {
		return fmt.Errorf("%s names no services (written before the stack's profiles were handled); start the stack by hand with the Python CLI's recreate, naming tinc and the Tahoe services together, then remove the marker", path)
	}
	start := up(m.Compose, m.Services)
	if _, err := cfg.Run(start[0], start[1:]...); err != nil {
		return fmt.Errorf("starting the stack: %w", err)
	}
	return os.Remove(path)
}

// Migrated reports whether the marker is present, and when it was written.
func Migrated(dataDir string) (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(dataDir, Marker))
	if err != nil {
		return time.Time{}, false
	}
	var m marker
	if json.Unmarshal(data, &m) != nil {
		return time.Time{}, false
	}
	return m.At, true
}

// services asks Docker itself, not compose, which containers the project
// has, by service: compose would not list a service whose profile is off.
// tinc comes first, the rest sorted, since the order is what the operator
// reads and the network namespace is tinc's.
func services(cfg Config) ([]string, error) {
	out, err := cfg.Run("docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+cfg.Project, "--format", `{{.Label "com.docker.compose.service"}}`)
	if err != nil {
		return nil, fmt.Errorf("listing the stack's containers: %w", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" && !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "tinc") != (names[j] == "tinc") {
			return names[i] == "tinc"
		}
		return names[i] < names[j]
	})
	return names, nil
}

// profiles lists every profile the compose files define.
func profiles(cfg Config, compose []string) ([]string, error) {
	args := append(append([]string{}, compose[1:]...), "config", "--profiles")
	out, err := cfg.Run(compose[0], args...)
	if err != nil {
		return nil, fmt.Errorf("reading the stack's profiles: %w", err)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	sort.Strings(names)
	return names, nil
}

// coverage says what keeps the sync folder from counting as moved: no
// native set holds it, its last run did not complete, or no restore test
// has passed on this device.
func coverage(syncDir string, st *jobs.Status) []string {
	if st == nil {
		return []string{fmt.Sprintf("%s is on the old grid and the agent reports no backup sets", syncDir)}
	}
	set := covering(syncDir, st)
	if set == "" {
		return []string{fmt.Sprintf("%s is on the old grid and no native backup set holds it: redundanet backup add NAME %s", syncDir, syncDir)}
	}
	var out []string
	var run *jobs.RunStatus
	for i := range st.Runs {
		if st.Runs[i].Set == set {
			run = &st.Runs[i]
		}
	}
	switch {
	case run == nil || run.Last == nil:
		out = append(out, fmt.Sprintf("the native set %q has not completed a run: redundanet backup run %s", set, set))
	case run.LastErr != "":
		out = append(out, fmt.Sprintf("the native set %q failed its last run: %s", set, run.LastErr))
	case run.Last.Snapshot.Partial:
		out = append(out, fmt.Sprintf("the native set %q last ran only partially; a complete run is needed", set))
	}
	if len(st.Verifications) == 0 || !st.Verifications[0].Passed() {
		out = append(out, "no restore test has passed on this device yet: redundanet backup verify, then backup list")
	}
	return out
}

// covering is the native set whose folders hold dir, or "".
func covering(dir string, st *jobs.Status) string {
	if st == nil {
		return ""
	}
	for _, set := range st.Sets {
		for _, f := range set.Folders {
			rel, err := filepath.Rel(filepath.Clean(f), dir)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return set.Name
			}
		}
	}
	return ""
}

// readEnv parses the stack's .env: KEY=VALUE lines, quotes stripped,
// comments and blanks skipped. A missing file is an empty environment.
func readEnv(path string) (map[string]string, error) {
	env := map[string]string{}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		env[strings.TrimSpace(k)] = v
	}
	return env, sc.Err()
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
