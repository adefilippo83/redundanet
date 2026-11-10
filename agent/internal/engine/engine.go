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
	"fmt"
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
	ErrRecentlyTouched  = errors.New("engine: a server refused the delete token: the object was touched within the immunity")
	ErrIncomplete       = errors.New("engine: not every server answered")
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
	// Own is the highest version of this device's own root among the
	// answers, and Confirmed the version this device last published to a
	// majority, from its local database: 0 when it has no record (a new
	// device, a replaced database) or when the roots are another
	// member's. See Trusted.
	Own       uint64
	Confirmed uint64
}

// ErrRootsUnsure is why a reading of the roots is not built on.
var ErrRootsUnsure = errors.New("engine: the storage servers did not give a sure view of the member's roots")

// Trusted says whether the views are sure to hold every root that was
// confirmed, and why not. A root is confirmed on a majority W of the S
// servers, so S-W+1 answers include a holder of each; and whatever the
// servers say, this device's own root is not older than the one it
// confirmed itself. What is built on an older root (a backup's snapshot
// list, a cleanup's set of referenced objects) drops what came after it.
func (r *Roots) Trusted() error {
	if w := r.Servers/2 + 1; r.Servers == 0 || r.Answered < r.Servers-w+1 {
		return fmt.Errorf("%w: %d of %d answered", ErrRootsUnsure, r.Answered, r.Servers)
	}
	if r.Own < r.Confirmed {
		return fmt.Errorf("%w: they returned version %d of this device's root, and it confirmed version %d", ErrRootsUnsure, r.Own, r.Confirmed)
	}
	return nil
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
	// Tombstoned says a holder has the object under a delete token; its
	// shares are in Ghosts, which the owner may still read within the
	// window, and count toward nothing else.
	Tombstoned bool
	Ghosts     map[identity.Key][]uint8
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
	// Usage is what the storage servers hold for the member, in share
	// bytes, summed over the servers that answered.
	Usage(ctx context.Context) (Usage, error)
}

// Usage is the member's share bytes across the storage servers.
type Usage struct {
	Bytes    int64 // share bytes on the servers that answered
	Servers  int   // storage servers in the manifest
	Answered int   // that answered; the sum is a lower bound when fewer
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
