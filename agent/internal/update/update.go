// Package update is the two-stage self-update of RFC 0001. The running
// agent stays in charge: it fetches the signed release index, downloads and
// verifies the release, starts the new binary on probation, and only when
// the probation passes renames it into place and asks to be restarted. A
// binary that cannot start therefore never strands the node, and the
// previous version is always kept beside the new one.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/release"
)

// Config wires an updater.
type Config struct {
	DataDir  string
	Version  string       // the running version
	Embedded identity.Key // the release key; zero disables updates
	IndexURL string
	Channel  string // stable or early
	// MinVersion returns the manifest's min_version, "" when unknown.
	MinVersion func() string
	// Exe is the running executable's path, the file that gets replaced.
	Exe string
	// Probation runs the candidate and returns nil when it proved healthy.
	Probation func(ctx context.Context, exe string) error
	// Handover stops the running agent's own work before probation (true)
	// and restarts it when probation fails (false). Optional.
	Handover func(stop bool)
	// Fetch and FetchAsset override the HTTP fetches, for tests.
	Fetch      func(ctx context.Context, url string) ([]byte, error)
	FetchAsset func(ctx context.Context, url string, w io.Writer) error
	Interval   time.Duration // between checks; default one hour
	Log        *slog.Logger
}

// ErrRestart is returned by Apply when the new binary is in place: the
// caller exits and the service manager starts the new version.
var ErrRestart = errors.New("update installed: restart to run it")

// state is what persists between runs.
type state struct {
	Channel      string            `yaml:"channel,omitempty"` // the channel of the last check, for `update status` from another process
	LastRevision uint64            `yaml:"last_index_revision"`
	Failed       map[string]string `yaml:"failed"` // version -> why probation failed
	Installed    map[string]string `yaml:"installed"`
	LastCheck    time.Time         `yaml:"last_check"`
	LastError    string            `yaml:"last_error,omitempty"`
}

// Updater checks and applies releases.
type Updater struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	st    state
	avail *release.Release
	busy  bool
}

// Status for the local page.
type Status struct {
	Enabled      bool      `json:"enabled"`
	Running      string    `json:"running"`
	Channel      string    `json:"channel"`
	Available    string    `json:"available,omitempty"`
	LastCheck    time.Time `json:"last_check,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Failed       []string  `json:"failed,omitempty"`
	LastRevision uint64    `json:"last_index_revision"`
}

// New loads the persisted state.
func New(cfg Config) *Updater {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Interval == 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Channel == "" {
		cfg.Channel = release.ChannelStable
	}
	if cfg.Fetch == nil {
		cfg.Fetch = release.FetchBytes
	}
	u := &Updater{cfg: cfg, log: cfg.Log.With("component", "update")}
	u.st.Failed = map[string]string{}
	u.st.Installed = map[string]string{}
	if b, err := os.ReadFile(u.statePath()); err == nil {
		_ = yaml.Unmarshal(b, &u.st)
		if u.st.Failed == nil {
			u.st.Failed = map[string]string{}
		}
		if u.st.Installed == nil {
			u.st.Installed = map[string]string{}
		}
	}
	return u
}

func (u *Updater) statePath() string { return filepath.Join(u.cfg.DataDir, "updates", "state.yaml") }

func (u *Updater) save() {
	_ = os.MkdirAll(filepath.Dir(u.statePath()), 0o700)
	b, err := yaml.Marshal(&u.st)
	if err == nil {
		_ = os.WriteFile(u.statePath(), b, 0o644)
	}
}

// MinVersionFrom sets where the manifest's min_version is read from.
func (u *Updater) MinVersionFrom(f func() string) {
	u.mu.Lock()
	u.cfg.MinVersion = f
	u.mu.Unlock()
}

// Enabled reports whether a release key is embedded.
func (u *Updater) Enabled() bool { return !u.cfg.Embedded.IsZero() && u.cfg.Version != "dev" }

// Status snapshots the updater.
func (u *Updater) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := Status{Enabled: u.Enabled(), Running: u.cfg.Version, Channel: u.cfg.Channel, LastCheck: u.st.LastCheck, LastError: u.st.LastError, LastRevision: u.st.LastRevision}
	if u.st.Channel != "" {
		// What the service last checked with, which is what the CLI's
		// `update status` from another process wants to know.
		st.Channel = u.st.Channel
	}
	if u.avail != nil {
		st.Available = u.avail.Version
	}
	for v := range u.st.Failed {
		st.Failed = append(st.Failed, v)
	}
	return st
}

// Check fetches and verifies the index and returns the release to install,
// nil when there is none.
func (u *Updater) Check(ctx context.Context) (*release.Release, error) {
	if !u.Enabled() {
		return nil, errors.New("self-update is disabled: no release key in this build")
	}
	rel, err := u.check(ctx)
	u.mu.Lock()
	u.st.LastCheck = time.Now()
	u.st.Channel = u.cfg.Channel
	u.avail = rel
	if err != nil {
		u.st.LastError = err.Error()
	} else {
		u.st.LastError = ""
	}
	u.save()
	u.mu.Unlock()
	return rel, err
}

func (u *Updater) check(ctx context.Context) (*release.Release, error) {
	data, err := u.cfg.Fetch(ctx, u.cfg.IndexURL)
	if err != nil {
		return nil, err
	}
	sigData, err := u.cfg.Fetch(ctx, sigURL(u.cfg.IndexURL))
	if err != nil {
		return nil, err
	}
	sig, err := release.ParseSignature(sigData)
	if err != nil {
		return nil, err
	}
	idx, _, err := sig.Verify(data, u.cfg.Embedded, time.Now())
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	last := u.st.LastRevision
	failed := u.st.Failed
	u.mu.Unlock()
	if idx.Revision < last {
		return nil, fmt.Errorf("release index revision %d is older than the %d already seen", idx.Revision, last)
	}
	u.mu.Lock()
	u.st.LastRevision = idx.Revision
	u.mu.Unlock()
	u.mu.Lock()
	minFn := u.cfg.MinVersion
	u.mu.Unlock()
	minVersion := ""
	if minFn != nil {
		minVersion = minFn()
	}
	rel := idx.Pick(u.cfg.Version, u.cfg.Channel, minVersion, release.Target())
	if rel == nil {
		return nil, nil
	}
	if why, ok := failed[rel.Version]; ok {
		return nil, fmt.Errorf("release %s failed probation earlier (%s) and is not retried", rel.Version, why)
	}
	return rel, nil
}

// sigURL is the signature beside the index: index.yaml -> index.sig.
func sigURL(indexURL string) string {
	if ext := filepath.Ext(indexURL); ext != "" {
		return indexURL[:len(indexURL)-len(ext)] + ".sig"
	}
	return indexURL + ".sig"
}

// Apply downloads, verifies, probes and installs a release. On success it
// returns ErrRestart.
func (u *Updater) Apply(ctx context.Context, rel *release.Release) error {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return errors.New("an update is already in progress")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	asset, ok := rel.Assets[release.Target()]
	if !ok {
		return fmt.Errorf("release %s has no asset for %s", rel.Version, release.Target())
	}
	dir := filepath.Join(u.cfg.DataDir, "versions", rel.Version)
	u.log.Info("downloading release", "version", rel.Version, "url", asset.URL)
	candidate, err := release.Download(ctx, asset, dir, u.cfg.FetchAsset)
	if err != nil {
		return fmt.Errorf("download %s: %w", rel.Version, err)
	}
	if u.cfg.Probation != nil {
		if u.cfg.Handover != nil {
			u.cfg.Handover(true)
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := u.cfg.Probation(pctx, candidate)
		cancel()
		if err != nil {
			u.mu.Lock()
			u.st.Failed[rel.Version] = err.Error()
			u.st.LastError = "probation of " + rel.Version + " failed: " + err.Error()
			u.save()
			u.mu.Unlock()
			u.log.Error("probation failed, keeping the running version", "version", rel.Version, "err", err)
			if u.cfg.Handover != nil {
				u.cfg.Handover(false)
			}
			return fmt.Errorf("probation of %s failed: %w", rel.Version, err)
		}
	}
	if err := commit(candidate, u.cfg.Exe); err != nil {
		if u.cfg.Handover != nil {
			u.cfg.Handover(false)
		}
		return err
	}
	u.mu.Lock()
	u.st.Installed[rel.Version] = time.Now().UTC().Format(time.RFC3339)
	u.st.LastError = ""
	u.save()
	u.mu.Unlock()
	u.log.Info("release installed", "version", rel.Version, "exe", u.cfg.Exe)
	return ErrRestart
}

// commit puts the candidate in place of the running executable. The old
// file is renamed, not deleted, which Windows allows for a running
// program, and it stays as the previous version.
func commit(candidate, exe string) error {
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("move the running executable aside: %w", err)
	}
	if err := os.Rename(candidate, exe); err != nil {
		// Put the old one back; the candidate stays in versions/.
		_ = os.Rename(old, exe)
		return fmt.Errorf("install the new executable: %w", err)
	}
	return nil
}

// Run checks on the interval and applies what it finds, until ctx ends or
// an update is installed (ErrRestart is returned).
func (u *Updater) Run(ctx context.Context) error {
	if !u.Enabled() {
		u.log.Info("self-update disabled: no release key in this build")
		<-ctx.Done()
		return nil
	}
	t := time.NewTicker(u.cfg.Interval)
	defer t.Stop()
	for {
		rel, err := u.Check(ctx)
		if err != nil {
			u.log.Warn("update check failed", "err", err)
		} else if rel != nil {
			if err := u.Apply(ctx, rel); errors.Is(err, ErrRestart) {
				return err
			} else if err != nil {
				u.log.Warn("update failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
