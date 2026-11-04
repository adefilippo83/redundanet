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
	"github.com/adefilippo83/redundanet/agent/internal/cleanup"
	"github.com/adefilippo83/redundanet/agent/internal/engine"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/rebalance"
	"github.com/adefilippo83/redundanet/agent/internal/restore"
	"github.com/adefilippo83/redundanet/agent/internal/snapshot"
)

// File is backup.yaml in the data directory.
type File struct {
	Version      int                `yaml:"version"` // of this file's defaults; 2 made the restore test weekly
	Sets         []backup.Set       `yaml:"sets"`
	Every        time.Duration      `yaml:"every"`         // between runs of each set; default 24h
	VerifyEvery  time.Duration      `yaml:"verify_every"`  // between restore tests; default 7 days
	CleanupEvery time.Duration      `yaml:"cleanup_every"` // between cleanups of unreferenced objects; default 7 days
	Retention    snapshot.Retention `yaml:"retention"`
}

const fileName = "backup.yaml"

// Defaults of the file.
const (
	fileVersion        = 2
	defaultVerifyEvery = 7 * 24 * time.Hour
	oldVerifyEvery     = 30 * 24 * time.Hour // the default files of version 1 were saved with
)

// Load reads the configuration; a missing file is an empty one. A file
// saved by an earlier version with the monthly restore test it then
// defaulted to moves to the weekly one: the month the G2 gate measures
// is made of weekly tests.
func Load(dir string) (*File, error) {
	f := &File{Version: fileVersion, Every: 24 * time.Hour, VerifyEvery: defaultVerifyEvery, CleanupEvery: 7 * 24 * time.Hour, Retention: snapshot.DefaultRetention}
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	f.Version = 0
	if err := yaml.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("jobs: %s: %w", fileName, err)
	}
	if f.Version < 2 && f.VerifyEvery == oldVerifyEvery {
		f.VerifyEvery = defaultVerifyEvery
	}
	f.Version = fileVersion
	if f.Every <= 0 {
		f.Every = 24 * time.Hour
	}
	if f.VerifyEvery <= 0 {
		f.VerifyEvery = defaultVerifyEvery
	}
	if f.CleanupEvery <= 0 {
		f.CleanupEvery = 7 * 24 * time.Hour
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
	Sets        []backup.Set          `json:"sets"`
	Every       string                `json:"every"`
	VerifyEvery string                `json:"verify_every"`
	Runs        []RunStatus           `json:"runs"`
	LastVerify  *restore.Verification `json:"last_verify,omitempty"`
	// Verifications are the newest restore tests first, as the device
	// kept them; VerifyStreak is how many weeks in a row had a passing
	// one, counting back from this week.
	Verifications []localdb.Verification `json:"verifications,omitempty"`
	VerifyStreak  int                    `json:"verify_streak"`
	Restore       *RestoreStatus         `json:"restore,omitempty"`
	LastCleanup   *cleanup.Report        `json:"last_cleanup,omitempty"`
	Cleaning      bool                   `json:"cleaning"`
	Rebalance     *rebalance.Status      `json:"rebalance,omitempty"` // the conversion running or last run
}

// Config wires the runner.
type Config struct {
	DataDir string
	Engine  engine.Engine
	DB      *localdb.DB
	Member  identity.Key
	Device  identity.Key
	// Revocations lists the member's revoked devices, for the cleanup; nil
	// when the agent runs from a manifest file.
	Revocations func() []*identity.SignedRevocation
	// Manifest is the current manifest, for the conversion job; nil means
	// no conversion ever runs.
	Manifest func() *manifest.Manifest
	SpoolDir string
	Log      *slog.Logger
	Now      func() time.Time
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

	lastVerify  *restore.Verification
	restore     *RestoreStatus
	lastCleanup *cleanup.Report
	cleaning    bool
	rebalance   *rebalance.Status
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
	// The last restore test survives a restart: the census says when it
	// was, and the page what it found.
	if vs, err := cfg.DB.Verifications(1); err == nil && len(vs) == 1 {
		r.lastVerify = fromRecord(vs[0])
	}
	return r, nil
}

func fromRecord(v localdb.Verification) *restore.Verification {
	return &restore.Verification{At: v.At, Snapshot: v.Snapshot, Sampled: v.Sampled, Verified: v.Verified, Compared: v.Compared, Unchanged: v.Unchanged, Failed: v.Failed}
}

func toRecord(v *restore.Verification) localdb.Verification {
	return localdb.Verification{At: v.At, Snapshot: v.Snapshot, Sampled: v.Sampled, Verified: v.Verified, Compared: v.Compared, Unchanged: v.Unchanged, Failed: v.Failed}
}

// streak counts the weeks in a row with a passing restore test, counting
// back from the week of now. A week with no test yet, this one, does not
// break the streak; the first week without one before it does.
func streak(tests []localdb.Verification, now time.Time) int {
	passed := map[[2]int]bool{}
	for _, v := range tests {
		if v.Passed() {
			y, w := v.At.UTC().ISOWeek()
			passed[[2]int{y, w}] = true
		}
	}
	n := 0
	t := now.UTC()
	y, w := t.ISOWeek()
	if !passed[[2]int{y, w}] {
		t = t.AddDate(0, 0, -7) // this week has had no test yet
	}
	for {
		y, w := t.ISOWeek()
		if !passed[[2]int{y, w}] {
			return n
		}
		n++
		t = t.AddDate(0, 0, -7)
	}
}

// Status snapshots the runner.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := Status{Sets: append([]backup.Set{}, r.file.Sets...), Every: r.file.Every.String(), VerifyEvery: r.file.VerifyEvery.String(), LastVerify: r.lastVerify, LastCleanup: r.lastCleanup, Cleaning: r.cleaning}
	if vs, err := r.cfg.DB.Verifications(8); err == nil {
		st.Verifications = vs
	}
	st.VerifyStreak = r.VerifyStreak()
	if r.rebalance != nil {
		cp := *r.rebalance
		st.Rebalance = &cp
	}
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
	every, verifyEvery, cleanupEvery := r.file.Every, r.file.VerifyEvery, r.file.CleanupEvery
	r.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	v := time.NewTicker(verifyEvery)
	defer v.Stop()
	cl := time.NewTicker(cleanupEvery)
	defer cl.Stop()
	// A conversion runs while the manifest declares a transition, a few
	// times a day, and after each backup run has had its say.
	rb := time.NewTicker(rebalanceEvery)
	defer rb.Stop()
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
			switch name {
			case "verify":
				r.verify(ctx)
			case "cleanup":
				_, _ = r.RunCleanup(ctx, false, false)
			case "rebalance":
				_, _ = r.RunRebalance(ctx)
			default:
				r.runAll(ctx, name)
			}
		case <-v.C:
			r.verify(ctx)
		case <-cl.C:
			_, _ = r.RunCleanup(ctx, false, false)
		case <-rb.C:
			if r.transitionDeclared() {
				_, _ = r.RunRebalance(ctx)
			}
		}
	}
}

// rebalanceEvery is how often a device looks for objects to convert while
// a transition is declared.
const rebalanceEvery = 6 * time.Hour

func (r *Runner) transitionDeclared() bool {
	if r.cfg.Manifest == nil {
		return false
	}
	m := r.cfg.Manifest()
	return m != nil && m.Network.EncodingTransition != nil
}

// ErrRebalancing says a conversion is already running.
var ErrRebalancing = errors.New("jobs: a conversion is already running")

// RunRebalance converts this device's snapshots to the manifest's
// encoding now, one run at a time, and keeps the status for the page.
func (r *Runner) RunRebalance(ctx context.Context) (*rebalance.Status, error) {
	g, ok := r.cfg.Engine.(rebalance.Grid)
	if !ok || r.cfg.Manifest == nil {
		return nil, errors.New("jobs: this engine cannot convert objects")
	}
	r.mu.Lock()
	if r.rebalance != nil && r.rebalance.Running {
		r.mu.Unlock()
		return nil, ErrRebalancing
	}
	r.rebalance = &rebalance.Status{Running: true}
	r.mu.Unlock()
	st, err := rebalance.Run(ctx, rebalance.Config{Grid: g, DB: r.cfg.DB, Member: r.cfg.Member, Device: r.cfg.Device, Manifest: r.cfg.Manifest, SpoolDir: r.cfg.SpoolDir,
		Now: r.cfg.Now, Log: r.log, Progress: func(s rebalance.Status) {
			r.mu.Lock()
			cp := s
			r.rebalance = &cp
			r.mu.Unlock()
		}})
	if st != nil {
		r.mu.Lock()
		cp := *st
		cp.Running = false
		r.rebalance = &cp
		r.mu.Unlock()
	} else {
		r.mu.Lock()
		r.rebalance = &rebalance.Status{At: r.cfg.Now(), Err: err.Error()}
		r.mu.Unlock()
	}
	if err != nil {
		r.log.Warn("conversion did not complete", "err", err)
	}
	return st, err
}

// ErrCleaning says a cleanup is already running.
var ErrCleaning = errors.New("jobs: a cleanup is already running")

// RunCleanup runs the owner's cleanup now, one at a time, and keeps the
// report for the page. It runs beside a backup, which the 7-day immunity
// on the servers and the probe before a root make safe.
func (r *Runner) RunCleanup(ctx context.Context, dryRun, force bool) (*cleanup.Report, error) {
	g, ok := r.cfg.Engine.(cleanup.Grid)
	if !ok {
		return nil, errors.New("jobs: this engine cannot list or delete objects")
	}
	r.mu.Lock()
	if r.cleaning {
		r.mu.Unlock()
		return nil, ErrCleaning
	}
	r.cleaning = true
	r.mu.Unlock()
	rep, err := cleanup.Run(ctx, cleanup.Config{Grid: g, Member: r.cfg.Member, Revocations: r.cfg.Revocations, DryRun: dryRun, Force: force, Now: r.cfg.Now, Log: r.log})
	r.mu.Lock()
	r.cleaning = false
	if rep != nil && !dryRun {
		r.lastCleanup = rep
	}
	r.mu.Unlock()
	if err != nil {
		r.log.Warn("cleanup did not complete", "err", err)
	}
	return rep, err
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
			if err := r.cfg.DB.RememberVerification(toRecord(v)); err != nil {
				r.log.Warn("restore test not recorded", "err", err)
			}
			r.log.Info("restore test", "sampled", v.Sampled, "verified", v.Verified, "compared", v.Compared, "failed", len(v.Failed), "weeks_in_a_row", r.VerifyStreak())
		}
	}
}

// VerifyStreak is the weeks in a row with a passing restore test, for the
// census and the page.
func (r *Runner) VerifyStreak() int {
	vs, err := r.cfg.DB.Verifications(60)
	if err != nil {
		return 0
	}
	return streak(vs, r.cfg.Now())
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
