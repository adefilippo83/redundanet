// Package engine is the client side of the native grid: the Engine
// interface of RFC 0001 that backup, restore and the tools are written
// against, its native implementation over the storage protocol, and a
// fake for tests. The native engine spools, deduplicates, encodes, places
// shares on the manifest's storage servers, reads with verification, and
// keeps the device's root record under quorum.
package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/localdb"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/object"
	"github.com/adefilippo83/redundanet/agent/internal/shareproto"
	"github.com/adefilippo83/redundanet/agent/internal/store"
)

// Errors a caller tells apart.
var (
	ErrNotEnoughServers = errors.New("engine: fewer than k servers accepted shares; the object is not readable")
	ErrNoServers        = errors.New("engine: the manifest lists no storage servers")
	ErrQuorum           = errors.New("engine: the root did not reach a majority of the storage servers")
	ErrNotReadable      = errors.New("engine: fewer than k shares are reachable")
	ErrNotImplemented   = errors.New("engine: not in this phase")
	ErrSize             = errors.New("engine: the reader did not deliver the announced size")
)

// Range is a byte range of an object.
type Range struct{ Offset, Length int64 }

// Root is a device's root record as the device sees it; servers hold it
// encrypted under the member's root key.
type Root struct {
	WrittenAt time.Time `cbor:"written_at"`
	IndexCap  string    `cbor:"index_cap"`      // read capability of the device's index object, "" before the first backup
	Note      string    `cbor:"note,omitempty"` // free text for tools and tests
}

// RootView is one device's highest root as the servers returned it; Root
// is set when the member's secret could open it.
type RootView struct {
	Device   identity.Key
	Version  uint64
	Root     *Root
	Envelope *shareproto.RootEnvelope
}

// Roots is what GetRoots learned and from how many servers.
type Roots struct {
	Views    []RootView
	Answered int
	Servers  int
}

// Health is where an object's shares are.
type Health struct {
	Cap      object.VerifyCap
	K, N     int
	Holders  map[identity.Key][]uint8 // complete share numbers per server
	Shares   []uint8                  // distinct complete share numbers
	Servers  int                      // distinct servers holding at least one
	Members  int                      // distinct members among them
	Readable bool                     // at least k distinct shares
	Placed   bool                     // all n distinct shares
	Asked    int
	Answered int
}

// Engine is what everything above the engine is written against.
type Engine interface {
	Put(ctx context.Context, r io.Reader, size int64) (object.ReadCap, error)
	Get(ctx context.Context, rc object.ReadCap, w io.Writer, rng *Range) error
	Has(ctx context.Context, plaintextHash object.Hash, size int64) (object.ReadCap, bool, error)
	Check(ctx context.Context, rc object.ReadCap) (*Health, error)
	Repair(ctx context.Context, rc object.ReadCap) (*Health, error)
	Delete(ctx context.Context, rc object.ReadCap) error
	PutRoot(ctx context.Context, root Root) (uint64, error)
	GetRoots(ctx context.Context, member identity.Key) (*Roots, error)
	Census(ctx context.Context, since uint64) (*store.CensusDelta, error)
}

// Config wires the native engine.
type Config struct {
	Network  string
	Member   identity.Key
	Secret   identity.MemberSecret
	Device   *identity.DeviceKey
	Cert     *identity.SignedCertificate
	Manifest func() *manifest.Manifest
	Client   *shareproto.Client
	DB       *localdb.DB
	SpoolDir string
	Store    *store.Store  // this node's own store, for Census; optional
	Timeout  time.Duration // per server operation; default 30 s
	Log      *slog.Logger
	Now      func() time.Time
}

// Native is the engine over the storage protocol.
type Native struct {
	cfg     Config
	conv    [32]byte
	rootKey [32]byte
	log     *slog.Logger
}

const labelRootKey = "redundanet/3/root/key"

// New wires a native engine.
func New(cfg Config) (*Native, error) {
	switch {
	case cfg.Manifest == nil || cfg.Client == nil || cfg.DB == nil || cfg.Device == nil || cfg.Cert == nil:
		return nil, errors.New("engine: manifest, client, db, device and certificate are required")
	case cfg.Network == "":
		return nil, errors.New("engine: a network name is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	e := &Native{cfg: cfg, conv: object.ConvergenceSecret(cfg.Secret), log: cfg.Log.With("component", "engine")}
	copy(e.rootKey[:], cfg.Secret.Subkey(labelRootKey, 32))
	return e, nil
}

// Census is what this node's own server holds; only a storage node has one.
func (e *Native) Census(_ context.Context, since uint64) (*store.CensusDelta, error) {
	if e.cfg.Store == nil {
		return nil, errors.New("engine: this device has no store")
	}
	return e.cfg.Store.Census(since)
}

// Delete comes with the two-phase deletion of M5.
func (e *Native) Delete(context.Context, object.ReadCap) error { return ErrNotImplemented }
