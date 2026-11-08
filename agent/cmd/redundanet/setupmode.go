package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/api"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/pairing"
	"github.com/adefilippo83/redundanet/agent/internal/version"
)

// Setup mode: an agent started before this device has an identity serves
// the local page alone, with the wizard that makes one, and continues into
// normal operation in the same process once the identity exists, whether
// the page made it or the CLI did. Without it an agent started by the
// service would exit and be restarted every ten seconds.

// setupState is the wizard behind the page: a pending phrase, a pending
// pairing, or nothing. Secrets stay here; the page sees words once.
type setupState struct {
	d, network string
	log        *slog.Logger

	mu       sync.Mutex
	phase    string // choose, confirm, pairing, done
	seed     *identity.Seed
	words    []string
	check    []int // zero-based positions asked back
	storage  bool
	code     string
	pairStop context.CancelFunc
	errMsg   string
}

func newSetupState(d, network string, lg *slog.Logger) *setupState {
	return &setupState{d: d, network: network, log: lg, phase: "choose"}
}

func (st *setupState) actions() *api.SetupActions {
	return &api.SetupActions{Begin: st.begin, Confirm: st.confirm, Restore: st.restore, Pair: st.pair, Cancel: st.cancel, Status: st.status}
}

func (st *setupState) status() api.SetupStatus {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := api.SetupStatus{Phase: st.phase, Storage: st.storage, Code: st.code, Error: st.errMsg}
	for _, i := range st.check {
		s.Check = append(s.Check, i+1)
	}
	return s
}

// begin draws a new member's seed and keeps it until the three words are
// confirmed; nothing is written before that.
func (st *setupState) begin(storage bool) ([]string, []int, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phase == "done" {
		return nil, nil, errors.New("this device has its identity already")
	}
	st.dropLocked()
	seed, err := identity.NewSeed()
	if err != nil {
		return nil, nil, err
	}
	st.seed, st.words, st.check, st.storage, st.phase = &seed, seed.Phrase(), threeIndices(12), storage, "confirm"
	check := make([]int, len(st.check))
	for i, c := range st.check {
		check[i] = c + 1
	}
	return st.words, check, nil
}

// confirm compares the words typed back with the phrase and writes the
// identity when they match. A wrong word keeps the pending phrase, so the
// member looks again at what they wrote down.
func (st *setupState) confirm(answers []string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phase != "confirm" || st.seed == nil {
		return errors.New("no phrase is waiting to be confirmed; start again")
	}
	if len(answers) != len(st.check) {
		return errors.New("three words are needed")
	}
	for i, idx := range st.check {
		if strings.TrimSpace(strings.ToLower(answers[i])) != st.words[idx] {
			return fmt.Errorf("word %d is not what the phrase says; look at what you wrote down", idx+1)
		}
	}
	if _, err := createIdentity(st.d, st.network, *st.seed, st.storage, 1, nil); err != nil {
		return err
	}
	st.seed.Zero()
	st.dropLocked()
	st.phase = "done"
	st.log.Info("identity made from the page", "storage", st.storage)
	return nil
}

// restore puts an existing member on this device from the typed phrase.
func (st *setupState) restore(phrase string, storage bool) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phase == "done" {
		return errors.New("this device has its identity already")
	}
	seed, err := identity.ParsePhrase(strings.Fields(phrase))
	if err != nil {
		return err
	}
	defer seed.Zero()
	if _, err := createIdentity(st.d, st.network, seed, storage, restoreSerial(), nil); err != nil {
		return err
	}
	st.dropLocked()
	st.phase = "done"
	st.log.Info("identity restored from the phrase on the page", "storage", storage)
	return nil
}

// pair shows a code and waits, in the background, for a device of the
// member to certify this one; the identity and the network arrive through
// the code, as `identity pair` does in the terminal.
func (st *setupState) pair() (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phase == "done" {
		return "", errors.New("this device has its identity already")
	}
	if st.phase == "pairing" && st.code != "" {
		return st.code, nil
	}
	st.dropLocked()
	dev, err := pendingDeviceKey(st.d)
	if err != nil {
		return "", err
	}
	l, err := pairing.Listen(st.network, dev.ID(), "")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	st.phase, st.code, st.pairStop, st.errMsg = "pairing", l.Code(), cancel, ""
	go func() {
		defer l.Close()
		offer, err := l.Wait(ctx)
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.pairStop == nil || st.phase != "pairing" {
			return // cancelled meanwhile
		}
		if err == nil {
			var state identity.State
			if err = identity.Unmarshal(offer.State, &state); err == nil {
				err = acceptState(st.d, st.network, dev, &state)
			}
			if err == nil {
				if _, jerr := joinFromOffer(st.d, st.network, offer.Descriptor); jerr != nil {
					st.log.Warn("paired, but the network could not be joined yet", "err", jerr)
				}
			}
		}
		if err != nil {
			st.phase, st.code, st.pairStop, st.errMsg = "choose", "", nil, "pairing did not complete: "+err.Error()
			return
		}
		st.phase, st.code, st.pairStop = "done", "", nil
		st.log.Info("identity received by pairing")
	}()
	return st.code, nil
}

// cancel drops whatever is pending and returns to the choices.
func (st *setupState) cancel() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phase == "done" {
		return
	}
	st.dropLocked()
	st.phase = "choose"
}

// dropLocked forgets a pending seed or pairing; the lock is held.
func (st *setupState) dropLocked() {
	if st.seed != nil {
		st.seed.Zero()
	}
	st.seed, st.words, st.check, st.code, st.errMsg = nil, nil, nil, "", ""
	if st.pairStop != nil {
		st.pairStop()
		st.pairStop = nil
	}
}

// runSetupMode starts the local API in setup mode when this device has no
// identity and returns, with the server to attach the runtime to, once it
// has one. A device that has its identity returns at once with no server.
func runSetupMode(ctx context.Context, d, network, apiAddr string, lg *slog.Logger) (*api.Server, error) {
	if _, err := os.Stat(identityPath(d)); err == nil {
		return nil, nil
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	st := newSetupState(d, network, lg)
	srv, err := api.Start(ctx, api.Config{Addr: apiAddr, DataDir: d, Version: version.Version, Log: lg, Setup: st.actions()})
	if err != nil {
		return nil, err
	}
	lg.Info("no identity yet: the local page offers the setup", "addr", srv.Addr(), "hint", "redundanet open, or redundanet setup")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			if _, err := os.Stat(identityPath(d)); err == nil {
				st.mu.Lock()
				st.dropLocked()
				st.phase = "done"
				st.mu.Unlock()
				return srv, nil
			}
		}
	}
}
