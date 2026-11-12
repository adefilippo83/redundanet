package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/release"
)

type fakeRepo struct {
	key   *release.Key
	index []byte
	sig   []byte
	asset []byte
	hash  string
}

func newFakeRepo(t *testing.T, revision int, versions ...string) *fakeRepo {
	t.Helper()
	r := &fakeRepo{}
	r.key, _ = release.NewKey()
	r.asset, r.hash = fakeArchive("new binary bytes")
	var b strings.Builder
	fmt.Fprintf(&b, "revision: %d\nreleases:\n", revision)
	for _, v := range versions {
		fmt.Fprintf(&b, "  - version: %s\n    channel: stable\n    assets:\n      %s: {url: https://example.org/%s.tar.gz, sha256: %s, size: %d}\n", v, release.Target(), v, r.hash, len(r.asset))
	}
	r.index = []byte(b.String())
	sig, err := release.Sign(r.key, r.index, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.sig, _ = sig.Encode()
	return r
}

func fakeArchive(content string) ([]byte, string) {
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "redundanet-x/redundanet", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(content))
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(tgz.Bytes())
	return tgz.Bytes(), hex.EncodeToString(sum[:])
}

func (r *fakeRepo) fetch(_ context.Context, url string) ([]byte, error) {
	switch {
	case strings.HasSuffix(url, "index.yaml"):
		return r.index, nil
	case strings.HasSuffix(url, "index.sig"):
		return r.sig, nil
	}
	return nil, errors.New("404 " + url)
}

func (r *fakeRepo) fetchAsset(_ context.Context, _ string, w io.Writer) error {
	_, err := w.Write(r.asset)
	return err
}

func newUpdater(t *testing.T, r *fakeRepo, dir string, probation func(context.Context, string) error) (*Updater, string) {
	t.Helper()
	exe := filepath.Join(dir, "redundanet")
	if _, err := os.Stat(exe); err != nil {
		if err := os.WriteFile(exe, []byte("old binary bytes"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	u := New(Config{DataDir: dir, Version: "3.0.0", Embedded: r.key.Public(), IndexURL: "https://example.org/releases/index.yaml",
		Exe: exe, Probation: probation, Fetch: r.fetch, FetchAsset: r.fetchAsset, MinVersion: func() string { return "" }})
	return u, exe
}

func TestDisabledWithoutKey(t *testing.T) {
	u := New(Config{DataDir: t.TempDir(), Version: "3.0.0"})
	if u.Enabled() {
		t.Fatal("enabled without a key")
	}
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("check succeeded without a key")
	}
}

func TestCheckApplyAndRollback(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.0", "3.0.1")
	dir := t.TempDir()
	var probed []string
	handovers := []bool{}
	u, exe := newUpdater(t, r, dir, func(_ context.Context, cand string) error {
		probed = append(probed, cand)
		b, _ := os.ReadFile(cand)
		if string(b) != "new binary bytes" {
			return errors.New("candidate is not the downloaded binary")
		}
		return nil
	})
	u.cfg.Handover = func(stop bool) { handovers = append(handovers, stop) }
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil || rel.Version != "3.0.1" {
		t.Fatalf("check: %v %+v", err, rel)
	}
	if st := u.Status(); st.Available != "3.0.1" || st.LastRevision != 1 {
		t.Fatalf("status: %+v", st)
	}
	err = u.Apply(context.Background(), rel)
	if !errors.Is(err, ErrRestart) {
		t.Fatalf("apply: %v", err)
	}
	if len(probed) != 1 || len(handovers) != 1 || !handovers[0] {
		t.Fatalf("probation %v handovers %v", probed, handovers)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary bytes" {
		t.Fatal("the executable was not replaced")
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old binary bytes" {
		t.Fatal("the previous version was not kept")
	}
	// State persisted: a new updater knows the revision and the install.
	u2 := New(Config{DataDir: dir, Version: "3.0.1", Embedded: r.key.Public()})
	if u2.st.LastRevision != 1 || u2.st.Installed["3.0.1"] == "" {
		t.Fatalf("state: %+v", u2.st)
	}
}

func TestProbationFailureKeepsTheRunningVersion(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	handovers := []bool{}
	u, exe := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("hub never answered") })
	u.cfg.Handover = func(stop bool) { handovers = append(handovers, stop) }
	rel, _ := u.Check(context.Background())
	if err := u.Apply(context.Background(), rel); err == nil || errors.Is(err, ErrRestart) {
		t.Fatalf("apply: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary bytes" {
		t.Fatal("the executable changed after a failed probation")
	}
	if len(handovers) != 2 || handovers[0] != true || handovers[1] != false {
		t.Fatalf("handovers %v", handovers)
	}
	// The failed version is remembered and not offered at the next check.
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "tried again after") {
		t.Fatalf("second check: %v", err)
	}
	u2, _ := newUpdater(t, r, dir, nil)
	if _, err := u2.Check(context.Background()); err == nil {
		t.Fatal("a restart forgot the failed version")
	}
}

func TestIndexRollbackAndBadSignature(t *testing.T) {
	r := newFakeRepo(t, 5, "3.0.1")
	dir := t.TempDir()
	u, _ := newUpdater(t, r, dir, nil)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	older := newFakeRepo(t, 4, "3.0.2")
	older.key = r.key
	sig, _ := release.Sign(r.key, older.index, time.Now())
	older.sig, _ = sig.Encode()
	u.cfg.Fetch = older.fetch
	if _, err := u.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("rollback: %v", err)
	}
	rogue := newFakeRepo(t, 6, "3.0.9") // signed by another key
	u.cfg.Fetch = rogue.fetch
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("an index signed by a stranger was accepted")
	}
	if u.Status().LastRevision != 5 {
		t.Fatal("a refused index moved the revision")
	}
}

func TestSigURL(t *testing.T) {
	if sigURL("https://x/releases/index.yaml") != "https://x/releases/index.sig" {
		t.Fatal(sigURL("https://x/releases/index.yaml"))
	}
}

func TestStatusReportsTheServiceChannel(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	u, _ := newUpdater(t, r, dir, nil)
	u.cfg.Channel = release.ChannelEarly
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Another process, such as the CLI's `update status`, opens the same
	// state with the default channel and must see the one the service uses.
	other, _ := newUpdater(t, r, dir, nil)
	if got := other.Status().Channel; got != release.ChannelEarly {
		t.Fatalf("channel reported: %q", got)
	}
}

func TestRetryForgetsAFailure(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	u, _ := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("candidate crashed") })
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("check: %v %v", rel, err)
	}
	if err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("a failed probation must fail the apply")
	}
	if st := u.Status(); len(st.Failed) != 1 || st.Failed[0] != "3.0.1" {
		t.Fatalf("failed list: %v", st.Failed)
	}
	if err := u.Retry("3.0.2"); err == nil {
		t.Fatal("retrying a version that did not fail must be refused")
	}
	if err := u.Retry("3.0.1"); err != nil {
		t.Fatal(err)
	}
	again, _ := newUpdater(t, r, dir, nil)
	if st := again.Status(); len(st.Failed) != 0 {
		t.Fatalf("the retry was not persisted: %v", st.Failed)
	}
}

func TestCheckSeesARetryFromAnotherProcess(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	service, _ := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("candidate crashed") })
	rel, _ := service.Check(context.Background())
	if err := service.Apply(context.Background(), rel); err == nil {
		t.Fatal("a failed probation must fail the apply")
	}
	if rel, _ := service.Check(context.Background()); rel != nil {
		t.Fatal("a failed version must not be offered again")
	}
	// The CLI, another process over the same state, forgets the failure.
	cli, _ := newUpdater(t, r, dir, nil)
	if err := cli.Retry("3.0.1"); err != nil {
		t.Fatal(err)
	}
	if rel, err := service.Check(context.Background()); err != nil || rel == nil || rel.Version != "3.0.1" {
		t.Fatalf("the service did not see the retry without a restart: %v %v", rel, err)
	}
}

// TestChosenChannelOverridesTheFlag: a channel chosen on the device is
// kept with the state and wins over the service's flag at the next start.
func TestChosenChannelOverridesTheFlag(t *testing.T) {
	dir := t.TempDir()
	u := New(Config{DataDir: dir, Version: "3.0.1", Channel: "stable"})
	if u.Status().Channel != "stable" {
		t.Fatalf("channel: %s", u.Status().Channel)
	}
	if err := u.SetChannel("nightly"); err == nil {
		t.Fatal("an unknown channel was accepted")
	}
	if err := u.SetChannel("early"); err != nil {
		t.Fatal(err)
	}
	if u.Status().Channel != "early" {
		t.Fatalf("channel after the choice: %s", u.Status().Channel)
	}
	again := New(Config{DataDir: dir, Version: "3.0.1", Channel: "stable"})
	if again.Status().Channel != "early" {
		t.Fatalf("the choice did not survive a restart: %s", again.Status().Channel)
	}
}

// due makes a failed version's next attempt due now, as if the wait were over.
func due(t *testing.T, u *Updater, version string) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	f := u.st.FailedInfo[version]
	if f.RetryAt.IsZero() {
		t.Fatalf("%s has no next attempt", version)
	}
	f.RetryAt = time.Now().Add(-time.Minute)
	u.st.FailedInfo[version] = f
	u.save()
}

func TestAProbationThatRanOutOfTimeIsTriedAgainLater(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	late := func(context.Context, string) error { return errors.New("the candidate did not report healthy in time") }
	u, _ := newUpdater(t, r, dir, late)
	fail := func() {
		t.Helper()
		rel, err := u.Check(context.Background())
		if err != nil || rel == nil {
			t.Fatalf("check: %v %v", rel, err)
		}
		if err := u.Apply(context.Background(), rel); err == nil {
			t.Fatal("a failed probation must fail the apply")
		}
	}
	waits := func(want time.Duration) {
		t.Helper()
		d := u.Status().FailedDetail
		if len(d) != 1 || d[0].Version != "3.0.1" {
			t.Fatalf("failed detail: %+v", d)
		}
		if got := time.Until(d[0].RetryAt); got < want-time.Minute || got > want+time.Minute {
			t.Fatalf("attempt %d: the next one is in %s, want %s", d[0].Attempts, got, want)
		}
		if rel, err := u.Check(context.Background()); rel != nil || err == nil || !strings.Contains(err.Error(), "tried again after") {
			t.Fatalf("a version that waits was offered: %v %v", rel, err)
		}
	}

	fail()
	waits(DefaultRetryAfter[0])
	due(t, u, "3.0.1")
	fail()
	waits(DefaultRetryAfter[1])
	due(t, u, "3.0.1")
	fail()
	d := u.Status().FailedDetail
	if len(d) != 1 || d[0].Attempts != 3 || !d[0].RetryAt.IsZero() {
		t.Fatalf("after the third failure: %+v", d)
	}
	if rel, err := u.Check(context.Background()); rel != nil || err == nil || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("a version that failed three times was offered: %v %v", rel, err)
	}
	// Asking still works, and starts the count again.
	if err := u.Retry("3.0.1"); err != nil {
		t.Fatal(err)
	}
	fail()
	waits(DefaultRetryAfter[0])
}

func TestACandidateThatExitedIsNotTriedAgainByItself(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	u, _ := newUpdater(t, r, t.TempDir(), func(context.Context, string) error {
		return fmt.Errorf("probe: %w", ErrCandidateExited)
	})
	rel, err := u.Check(context.Background())
	if err != nil || rel == nil {
		t.Fatalf("check: %v %v", rel, err)
	}
	if err := u.Apply(context.Background(), rel); err == nil {
		t.Fatal("a failed probation must fail the apply")
	}
	if d := u.Status().FailedDetail; len(d) != 1 || !d[0].RetryAt.IsZero() {
		t.Fatalf("failed detail: %+v", d)
	}
	if rel, err := u.Check(context.Background()); rel != nil || err == nil || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("a candidate that exited was offered again: %v %v", rel, err)
	}
}

func TestAProbationCutShortByAStopIsHeldAgainstNothing(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	u, _ := newUpdater(t, r, t.TempDir(), func(pctx context.Context, _ string) error {
		cancel() // the service is told to stop while the candidate runs
		<-pctx.Done()
		return errors.New("the candidate did not report healthy in time")
	})
	rel, err := u.Check(ctx)
	if err != nil || rel == nil {
		t.Fatalf("check: %v %v", rel, err)
	}
	if err := u.Apply(ctx, rel); err == nil {
		t.Fatal("an interrupted probation must not install")
	}
	if st := u.Status(); len(st.Failed) != 0 {
		t.Fatalf("held against the version: %v", st.Failed)
	}
	if rel, err := u.Check(context.Background()); err != nil || rel == nil {
		t.Fatalf("the version is no longer offered: %v %v", rel, err)
	}
}

// A rollback puts an older binary back, which knows only the map of
// reasons: it must read what this one writes, and what it wrote waits for
// `update retry` as it did then.
func TestTheFailedMapStaysWhatOlderBinariesRead(t *testing.T) {
	r := newFakeRepo(t, 1, "3.0.1")
	dir := t.TempDir()
	u, _ := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("late") })
	rel, _ := u.Check(context.Background())
	if rel == nil || u.Apply(context.Background(), rel) == nil {
		t.Fatal("setup: the probation did not fail")
	}
	b, err := os.ReadFile(filepath.Join(dir, "updates", "state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Failed map[string]string `yaml:"failed"`
	}
	if err := yaml.Unmarshal(b, &old); err != nil || old.Failed["3.0.1"] == "" {
		t.Fatalf("an older binary would not read the failure: %v %v\n%s", err, old.Failed, b)
	}

	// What an older binary wrote: a reason and nothing else.
	if err := os.WriteFile(filepath.Join(dir, "updates", "state.yaml"), []byte("failed:\n  3.0.1: the candidate did not report healthy in time\ninstalled: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, _ := newUpdater(t, r, dir, nil)
	if rel, err := again.Check(context.Background()); rel != nil || err == nil || !strings.Contains(err.Error(), "not retried") {
		t.Fatalf("a mark of an older binary was dropped: %v %v", rel, err)
	}

	// An older binary's `update retry` removes the reason only; the rest
	// goes with it.
	u2, _ := newUpdater(t, r, dir, func(context.Context, string) error { return errors.New("late") })
	_ = u2.Retry("3.0.1")
	rel, _ = u2.Check(context.Background())
	if rel == nil || u2.Apply(context.Background(), rel) == nil {
		t.Fatal("setup: the probation did not fail")
	}
	if err := os.WriteFile(filepath.Join(dir, "updates", "state.yaml"), []byte("failed: {}\ninstalled: {}\nfailed_info:\n  3.0.1:\n    at: 2026-11-12T00:00:00Z\n    attempts: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	u3, _ := newUpdater(t, r, dir, nil)
	if rel, err := u3.Check(context.Background()); err != nil || rel == nil {
		t.Fatalf("a version an older binary cleared is not offered: %v %v", rel, err)
	}
	if d := u3.Status().FailedDetail; len(d) != 0 {
		t.Fatalf("left over: %+v", d)
	}
}
