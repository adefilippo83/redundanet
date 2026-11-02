// Package jobs runs the backup, restore test and restore jobs inside the
// agent: the configuration file of backup sets, the schedule, one run at a
// time, and the status the local page and the census show.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/backup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// File is backup.yaml in the data directory.
type File struct {
	Sets        []backup.Set       `yaml:"sets"`
	Every       time.Duration      `yaml:"every"`        // between runs of each set; default 24h
	VerifyEvery time.Duration      `yaml:"verify_every"` // between restore tests; default 30 days
	Retention   snapshot.Retention `yaml:"retention"`
}

const fileName = "backup.yaml"

// Load reads the configuration; a missing file is an empty one.
func Load(dir string) (*File, error) {
	f := &File{Every: 24 * time.Hour, VerifyEvery: 30 * 24 * time.Hour, Retention: snapshot.DefaultRetention}
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("jobs: %s: %w", fileName, err)
	}
	if f.Every <= 0 {
		f.Every = 24 * time.Hour
	}
	if f.VerifyEvery <= 0 {
		f.VerifyEvery = 30 * 24 * time.Hour
	}
	if f.Retention == (snapshot.Retention{}) {
		f.Retention = snapshot.DefaultRetention
	}
	return f, nil
}

// Save writes the configuration.
func (f *File) Save(dir string) error {
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, fileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, fileName))
}

// Find returns a set by name.
func (f *File) Find(name string) *backup.Set {
	for i := range f.Sets {
		if f.Sets[i].Name == name {
			return &f.Sets[i]
		}
	}
	return nil
}

// RunStatus is the last run of one set, and the run in progress.
type RunStatus struct {
	Set      string           `json:"set"`
	Running  bool             `json:"running"`
	Progress *backup.Progress `json:"progress,omitempty"`
	LastAt   time.Time        `json:"last_at,omitempty"`
	Last     *backup.Result   `json:"last,omitempty"`
	LastErr  string           `json:"last_error,omitempty"`
}

// RestoreStatus is the restore in progress or the last one.
type RestoreStatus struct {
	Running bool            `json:"running"`
	Device  string          `json:"device,omitempty"`
	Snap    string          `json:"snapshot,omitempty"`
	Path    string          `json:"path,omitempty"`
	Target  string          `json:"target,omitempty"`
	At      time.Time       `json:"at,omitempty"`
	Report  *restore.Report `json:"report,omitempty"`
	Err     string          `json:"error,omitempty"`
}

// Status is everything the page shows about the jobs.
type Status struct {
	Sets       []backup.Set          `json:"sets"`
	Every      string                `json:"every"`
	Runs       []RunStatus           `json:"runs"`
	LastVerify *restore.Verification `json:"last_verify,omitempty"`
	Restore    *RestoreStatus        `json:"restore,omitempty"`
}

// Config wires the runner.
type Config struct {
	DataDir string
	Engine  engine.Engine
	DB      *localdb.DB
	Member  identity.Key
	Device  identity.Key
	Log     *slog.Logger
	Now     func() time.Time
}

// Runner schedules and runs the jobs.
type Runner struct {
	cfg  Config
	log  *slog.Logger
	mu   sync.Mutex
	file *File
	runs map[string]*RunStatus
	busy bool
	kick chan string

	lastVerify *restore.Verification
	restore    *RestoreStatus
}

// New loads the configuration.
func New(cfg Config) (*Runner, error) {
	if cfg.Engine == nil || cfg.DB == nil || cfg.DataDir == "" {
		return nil, errors.New("jobs: engine, db and data dir are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	f, err := Load(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	r := &Runner{cfg: cfg, log: cfg.Log.With("component", "jobs"), file: f, runs: map[string]*RunStatus{}, kick: make(chan string, 8)}
	return r, nil
}

// Status snapshots the runner.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Sets: append([]backup.Set{}, r.file.Sets...), Every: r.file.Every.String(), LastVerify: r.lastVerify}
	if r.restore != nil {
		cp := *r.restore
		st.Restore = &cp
	}
	for _, s := range r.file.Sets {
		if rs := r.runs[s.Name]; rs != nil {
			st.Runs = append(st.Runs, *rs)
		} else {
			st.Runs = append(st.Runs, RunStatus{Set: s.Name})
		}
	}
	return st
}

// AddSet creates or replaces a set and saves the file.
func (r *Runner) AddSet(s backup.Set) error {
	if s.Name == "" || len(s.Folders) == 0 {
		return errors.New("jobs: a set needs a name and at least one folder")
	}
	for i, f := range s.Folders {
		abs, err := filepath.Abs(f)
		if err != nil {
			return err
		}
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			return fmt.Errorf("jobs: %s is not a directory", f)
		}
		s.Folders[i] = abs
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.file.Find(s.Name); cur != nil {
		*cur = s
	} else {
		r.file.Sets = append(r.file.Sets, s)
	}
	return r.file.Save(r.cfg.DataDir)
}

// RemoveSet deletes a set; its snapshots stay in the index.
func (r *Runner) RemoveSet(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.file.Sets {
		if s.Name == name {
			r.file.Sets = append(r.file.Sets[:i], r.file.Sets[i+1:]...)
			return r.file.Save(r.cfg.DataDir)
		}
	}
	return errors.New("jobs: no such set")
}

// RunNow asks for a run of one set, or of every set with "".
func (r *Runner) RunNow(set string) {
	select {
	case r.kick <- set:
	default:
	}
}

// Run is the scheduler: a run of every set at each interval, a restore
// test at its own interval, and runs asked for in between.
func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	every, verifyEvery := r.file.Every, r.file.VerifyEvery
	r.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	v := time.NewTicker(verifyEvery)
	defer v.Stop()
	// The first run shortly after start, so a new set is not a day away.
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			r.runAll(ctx, "")
		case <-t.C:
			r.runAll(ctx, "")
		case name := <-r.kick:
			if name == "verify" {
				r.verify(ctx)
			} else {
				r.runAll(ctx, name)
			}
		case <-v.C:
			r.verify(ctx)
		}
	}
}

func (r *Runner) runAll(ctx context.Context, only string) {
	r.mu.Lock()
	sets := append([]backup.Set{}, r.file.Sets...)
	retention := r.file.Retention
	r.mu.Unlock()
	for _, s := range sets {
		if only != "" && s.Name != only {
			continue
		}
		r.RunSet(ctx, s, retention)
	}
}

// RunSet runs one set now, one job at a time.
func (r *Runner) RunSet(ctx context.Context, s backup.Set, retention snapshot.Retention) *RunStatus {
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return nil
	}
	r.busy = true
	rs := r.runs[s.Name]
	if rs == nil {
		rs = &RunStatus{Set: s.Name}
		r.runs[s.Name] = rs
	}
	rs.Running, rs.Progress = true, &backup.Progress{Phase: "starting"}
	r.mu.Unlock()
	res, err := backup.Run(ctx, backup.Config{Engine: r.cfg.Engine, DB: r.cfg.DB, Member: r.cfg.Member, Device: r.cfg.Device, Set: s, Retention: retention,
		Log: r.log, Now: r.cfg.Now, Progress: func(p backup.Progress) {
			r.mu.Lock()
			cp := p
			rs.Progress = &cp
			r.mu.Unlock()
		}})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = false
	rs.Running, rs.Progress, rs.LastAt = false, nil, r.cfg.Now()
	rs.Last, rs.LastErr = res, ""
	if err != nil {
		rs.LastErr = err.Error()
		r.log.Warn("backup failed", "set", s.Name, "err", err)
	}
	return rs
}

// verify is the unattended restore test on this device's latest snapshot.
func (r *Runner) verify(ctx context.Context) {
	l, err := restore.List(ctx, r.cfg.Engine, r.cfg.Member)
	if err != nil {
		r.log.Warn("restore test: roots", "err", err)
		return
	}
	for _, d := range l.Devices {
		if d.Device != r.cfg.Device || d.Index == nil || d.Index.Latest() == nil {
			continue
		}
		v, err := restore.Verify(ctx, r.cfg.Engine, d.Index.Latest(), 20, r.cfg.Now())
		if err != nil {
			r.log.Warn("restore test failed to run", "err", err)
		}
		r.mu.Lock()
		r.lastVerify = v
		r.mu.Unlock()
		if v != nil {
			r.log.Info("restore test", "sampled", v.Sampled, "verified", v.Verified, "compared", v.Compared, "failed", len(v.Failed))
		}
	}
}

// Verify runs the restore test now.
func (r *Runner) Verify(ctx context.Context) *restore.Verification {
	r.verify(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastVerify
}

// StartRestore runs a restore in the background; one at a time.
func (r *Runner) StartRestore(device identity.Key, snapID string, path string, target string, overwrite bool) error {
	r.mu.Lock()
	if r.restore != nil && r.restore.Running {
		r.mu.Unlock()
		return errors.New("jobs: a restore is already running")
	}
	st := &RestoreStatus{Running: true, Device: device.String(), Snap: snapID, Path: path, Target: target, At: r.cfg.Now()}
	r.restore = st
	r.mu.Unlock()
	go func() {
		ctx := context.Background()
		rep, err := r.doRestore(ctx, device, snapID, path, target, overwrite)
		r.mu.Lock()
		defer r.mu.Unlock()
		st.Running, st.Report = false, rep
		if err != nil {
			st.Err = err.Error()
		}
	}()
	return nil
}

func (r *Runner) doRestore(ctx context.Context, device identity.Key, snapID string, path string, target string, overwrite bool) (*restore.Report, error) {
	snap, err := r.snapshot(ctx, device, snapID)
	if err != nil {
		return nil, err
	}
	elems, err := restore.Resolve(ctx, r.cfg.Engine, snap, path)
	if err != nil {
		return nil, err
	}
	return restore.Restore(ctx, r.cfg.Engine, snap, elems, target, restore.Options{Overwrite: overwrite, Log: r.log})
}

// snapshot finds a device's snapshot by id.
func (r *Runner) snapshot(ctx context.Context, device identity.Key, snapID string) (*snapshot.Snapshot, error) {
	l, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range l.Devices {
		if d.Device != device || d.Index == nil {
			continue
		}
		snap := d.Index.Find(snapID)
		if snap == nil {
			return nil, errors.New("jobs: no such snapshot")
		}
		return snap, nil
	}
	return nil, errors.New("jobs: no such device")
}

// LastBackup is the newest completed backup and whether it was partial,
// for the census.
func (r *Runner) LastBackup() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var at time.Time
	partial := false
	for _, rs := range r.runs {
		if rs.Last != nil && rs.LastErr == "" && rs.LastAt.After(at) {
			at, partial = rs.LastAt, rs.Last.Snapshot.Partial
		}
	}
	return at, partial
}

// LastVerify is the last restore test, for the census.
func (r *Runner) LastVerify() *restore.Verification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastVerify
}

// List is what the member can restore from.
func (r *Runner) List(ctx context.Context) (*restore.Listing, error) {
	return restore.List(ctx, r.cfg.Engine, r.cfg.Member)
}

// Browse walks a snapshot of a device down a path string.
func (r *Runner) Browse(ctx context.Context, device identity.Key, snapID string, path string) (*snapshot.Entry, *snapshot.Directory, error) {
	snap, err := r.snapshot(ctx, device, snapID)
	if err != nil {
		return nil, nil, err
	}
	elems, err := restore.Resolve(ctx, r.cfg.Engine, snap, path)
	if err != nil {
		return nil, nil, err
	}
	return restore.Browse(ctx, r.cfg.Engine, snap, elems)
}
