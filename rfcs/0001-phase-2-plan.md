# RFC 0001, phase 2: building the native engine

- Date: 2026-11-02
- Status: plan; the RFC's Design part is the specification, this page is the
  order of work and the test each step must pass
- Gate it serves: G2, end of July 2027

## Where phase 1 ended

The control plane is on the network: the hub on hub-hetzner relays, collects
the census and publishes the page under `/v3/`; falcon and falcon-1 run the
agent as a system service, took three signed updates by themselves and
survived a deliberately failed one; the maintainer's Mac joins as a client
device; manifest revision 3 carries the first member with a GPG attestation
and the join guide is written. What G1 still needs runs at the members' pace
and does not block the engine.

Phase 1 also fixed two things the engine inherits: the hub and the endpoint
now free their sockets on shutdown, so a self-update can hand over on the
same ports, and the census heartbeat is the carrier that phase 2 turns into
the share census.

## Principles for the build

- **The format first, frozen by tests.** The share format and the descriptor
  are versioned from day one and pinned by known-answer tests; everything
  else can change, those bytes cannot.
- **Everything above the engine against the interface.** Backup, restore,
  census, repair and the page are written against the RFC's `Engine`
  interface and tested with a fake; the engine is tested against the same
  expectations.
- **Offline first.** Every milestone has unit tests with in-memory servers
  and a fake clock; the live grid (hub, falcon, falcon-1, the Mac) is the
  acceptance test, never the only test.
- **The second grid on the same disks.** On the nodes the engine stores under
  `/mnt/storage/redundanet3/`, beside Tahoe's data, with a free-space floor
  above Tahoe's reserved space, exactly as the RFC's migration step 2 says.
- **No cgo.** klauspost/reedsolomon, zeebo/blake3 and modernc.org/sqlite are
  pure Go, as the RFC's library table chose.

## Milestones

Each milestone is a series of pull requests on the agent module, each ends
with a test the grid passes, and each ships as an alpha tag so the nodes
carry the code before it is switched on.

### M1. Object format and codec, pure

The RFC's "Native engine" section as a library with no network in it.

- `internal/object`: segments of 1 MiB; the convergent key from the member
  secret (`BLAKE3_keyed(convergence_secret, plaintext_hash || k || n ||
  segment_size)`), the storage index; AES-256-GCM per segment with the
  nonce and additional-data rule and a cipher id in the header;
  Reed-Solomon k-of-n over the segments; share roots as BLAKE3 trees over the
  blocks; the ciphertext hash; the descriptor as deterministic CBOR signed by
  the device key with its certificate; the descriptor hash; read and verify
  capabilities (`rn:r1:`, `rn:v1:`).
- Encode is the RFC's two-pass spool: one pass computes the roots, the
  second regenerates the identical blocks for sending, so memory holds one
  segment. Decode takes any k shares, verifies every block against the
  signed share root and the plaintext against the ciphertext hash and the
  key, and refuses anything else.
- Known-answer tests freeze the format: a fixed member secret and fixed
  inputs give fixed storage indices, share bytes and descriptor hashes.
  Property tests: round trips over sizes from zero bytes to several
  segments and chunk boundaries, every single-block corruption detected, two
  uploads by the same member produce one descriptor hash with two
  signatures, two members never collide.
- Exit: the test suite, plus throughput measured on falcon-1 (Pi 5) and the
  Mac, written into this page. The RFC expects tens of megabytes per second
  on the Pi, which is far above its uplink.
- Done 2026-11-02: `internal/object`, 60 round-trip cases over five
  encodings, every single-block corruption caught, known answers pinned and
  identical on arm64 and on the Mac. A 64 MiB object at 2-of-4, encode
  including every share and decode from two shares with full verification:

  | Machine | Encode | Decode |
  | --- | --- | --- |
  | Mac, Apple silicon | 102 MB/s | 208 MB/s |
  | falcon-1, Raspberry Pi 5 | 43 MB/s | 90 MB/s |

### M2. The storage server and the share protocol

The node side of the RFC's "Ownership, accounting and quotas" and the share
half of "Share on disk".

- `internal/store`: shares at `shares/<prefix>/<si>/<n>` with the header;
  the SQLite index with the RFC's columns; `partial` and `complete` states,
  resume by offset, the 7-day expiry of partials, usage per owner from the
  index, `last_touched_at` on every put and hit; the bounded integrity pass
  recording `verified_at`; the storage marker and volume check with the
  storage-unavailable state; the free-space floor.
- `internal/shareproto`: one QUIC stream per request, length-prefixed CBOR
  messages, large payloads streamed after the header. Requests: PutShare,
  GetShare with ranges, HasShares, PutRoot, GetRoots, PendingSet,
  DeleteToken, and the carriage of a signed repair task. The server applies
  the RFC's checks in order: device chains to an active member, descriptor
  signature valid and owner active or leaving, share root in the descriptor,
  per-server cap, one share number per server outside small networks,
  tombstones.
- The agent's stream handler dispatches to the store when the device has the
  storage role, under `--storage-path`. The census heartbeat gains the share
  census: `(storage index, share number, owner, size, k, n, descriptor
  hash)` deltas since a generation, tombstones and the storage state.
- Exit: in tests, two endpoints on loopback store, resume, verify and serve
  shares under every refusal rule. On the grid, falcon and falcon-1 accept
  shares from the Mac, and the hub's collector receives their share census.
- Done 2026-11-02, alpha.7: the first object on the native grid, 3 MB put
  from the Mac with two shares on each node (about 2 s per share to falcon
  over the relay, 130 ms to falcon-1 over a direct IPv6 path), read back
  byte for byte from both nodes and from falcon-1 alone through parity
  reconstruction; the page shows the shares per node and the owner's usage
  as the servers account it. Two lessons, both fixed in alpha.8: the
  confined service needs the store directory granted (the installer's
  `storage` step), and an update candidate must inherit the service's own
  flags, or it opens its store in the wrong place during probation.

### M3. The client engine, roots, placement

The RFC's `Engine` interface, implemented over the share protocol.

- `internal/engine`: `Put`, `Get`, `Has`, `Check`, `Delete`, `PutRoot`,
  `GetRoots`, `Census`. Placement: distinct servers, distinct members where
  the network has enough, then free space and recent success; fewer than n
  eligible servers writes to what is there if at least k and records
  under-placed; fewer than k refuses with the reason. Small networks double
  share numbers per server as the RFC says, which is the case of today's grid
  with two storage servers. `Get` asks the k best holders plus one spare and
  keeps the first k blocks that verify. Roots use the W quorum over the S
  storage servers, with the version counter persisted and recovered.
- `internal/localdb`: the device's SQLite cache of scan keys, capabilities
  with their engine and encoding, the pending set, the upload queue; a lost
  database is rebuilt from the index object and by re-hashing.
- A fake engine for the jobs of M4, with the same tests.
- Debug commands `redundanet object put|get|check` for the grid test.
- Exit: an object put from the Mac is read back with one of the two nodes
  stopped; the per-server cap and the free-space floor refuse as specified;
  the hub's status shows the object's placement.
- Done 2026-11-02: `internal/engine` and `internal/localdb`. The suite runs
  one contract against the fake and the native engine over a loopback grid
  of two members' servers, plus placement across members, a server that
  refuses (every share lands on the other), a server down (read through
  parity, check under-placed, the owner's repair onto the survivor, a root
  refused for lack of quorum). On the grid, `object put` placed a 5 MB
  file from the Mac in 8 s with two shares on each node, `check` and `has`
  found it, `get` read it back verified in 9 s, and a second put moved
  nothing. Still owner-side: repair and deletion by hub tasks come in M5.

### M4. Backup and restore

The product feature, against the interface, from the RFC's "Backup and
restore" and "Roots and snapshots".

- `internal/backup`: scan with the per-platform change-detection key and the
  rolling 1% re-hash, the default excludes, per-file failures counted and
  retried, inline files under 256 bytes, chunking at 64 MiB, the spool, the
  directory and index objects, root publish including the partial snapshot
  at the end of every run, retention with the 7-day rule, the pending set
  every few minutes, the `HasShares` check before `PutRoot`.
- `internal/restore`: list roots and snapshots, browse down a path, fetch
  only what is chosen, verify every block and the key, write under a
  temporary name, map and report names the target cannot hold, pause the
  covering backup set, "complete with exceptions". The monthly unattended
  restore test with its per-member date on the page.
- Local API, page and CLI: `/backups`, `/restore`, `/settings` for folders,
  schedule, bandwidth cap and retention; `backup add|run|list`, `restore`.
  The endpoint's bandwidth cap with the two token buckets, own traffic and
  service traffic.
- Exit: the maintainer's own folders back up nightly from the Mac to the two
  nodes, a restore to a temporary folder verifies against the sources, and
  the status page shows the last backup and the last verified restore. This
  starts the month that G2 measures.
- Done 2026-11-02: `internal/snapshot` (directory and index objects,
  retention), `internal/backup` (scan keys per platform, the 1% re-hash,
  excludes, inline and chunked files, dedup against the local database and
  the grid, a partial snapshot when a run is cut short), `internal/restore`
  (list, browse, restore under temporary names with `.restored` for what
  exists, the sampled restore test), `internal/jobs` (`backup.yaml`, the
  nightly run, the monthly test, the restore in the background), the API,
  the page card and `redundanet backup|restore`; the census carries the
  two dates and the hub page shows them per member. On the grid from the
  Mac: a 6 MB set of three files backed up to both nodes in 12 s with the
  duplicate file sent once, a restore to a temporary folder came back
  identical, the restore test verified 3 of 3. The grid test also found
  that the alpha.7 hub closed the channel on the first heartbeat with the
  new field (strict CBOR on protocol messages); channel frames and share
  messages now skip unknown fields, so a new field no longer needs the
  whole fleet on one version. Left for M5 and M6: the bandwidth cap, the
  pending-set root publish during a long run, pausing a set while it is
  being restored, and a settings page for schedule and retention (today
  they are edited in `backup.yaml`).
- 2026-11-03: the first real set (a 10,000-file workspace from the Mac)
  stalled after 238 files: neither end of the share protocol completed the
  read half of a request stream, so after 1024 requests the server's
  stream limit held every further request for its 30 s deadline. Both
  ends now cancel the read half once the last byte is in; a test sends
  1500 requests over one connection. Shipped as alpha.9. Reinstalling the
  Mac service for it hit a second race: launchctl bootout returns before
  the old instance is gone, and the bootstrap that followed failed with
  "Input/output error". The installer now waits for the old instance and
  retries the bootstrap; a gated test reinstalls a throwaway agent. With
  all three devices on alpha.9 the set backed up in two minutes (113
  files once node_modules was excluded, a second run two seconds later
  sent nothing) and the page shows the day. Three files were iCloud Drive
  placeholders, evicted by optimised storage: a service may not fetch
  them and the read fails with EDEADLK, so they are listed as skipped;
  the reason now says they are in the cloud, not on this computer.

### M5. Health: census table, repair, deletion, rebalance

The hub and node sides of the RFC's "Health" section.

- Hub: the placement table in SQLite from the share census, copies per object
  against its own n, under-placed objects, down and dead servers by the
  heartbeat rules, surplus, the one-scheduler rule between hubs, signed
  repair tasks with the quiescence window, the public projection on the
  page.
- Nodes: the repair worker that fetches k shares, regenerates the missing
  numbers, verifies them against the signed roots and stores them where the
  task says; surplus release only after confirming n holders directly.
- Deletion: the owner's cleanup job with the root quorum and the reachable
  set computed from the grid, delete tokens, tombstones with the 30-day
  window, `first_seen_missing` and the grace period for leaving members,
  late tokens for holders that were offline.
- Rebalance: the encoding transition in the manifest, conversion by the
  owner's devices, old-encoding shares accounted at zero until the deadline.
- Exit, with shortened windows in a test network: delete a node's share
  directory and watch repair restore full placement without the owner;
  prune a snapshot and watch the space return after the tombstone window;
  change k-of-n and watch the conversion bar reach the end.
- Built in four slices, each a pull request: the placement table and the
  page, repair tasks and the worker, deletion, rebalance.
- 2026-11-03, slice 1: the placement table in `placement.sqlite` on the
  hub, fed by the share census as before, so a node that is down keeps
  counting across a hub restart. Per object: the distinct shares on
  servers not dead, the spread against min(n, servers not dead), fully
  placed, readable, surplus, the descriptor the holders agree on,
  tombstoned, and whether repair is due at once (it had been fully placed,
  so a server died or a share failed its check) or after the 24-hour
  quiescence window (it never was). A server the manifest retires is
  forgotten with its shares. The census carries the date a node's oldest
  integrity check dates from and its free-space floor. The page gets an
  objects card (counts per number of copies, under-placed, unreadable,
  due for repair, deleted), a placement column per member and a "checked
  since" column per device; `status.json` carries the same counts and
  never a storage index. The pull request's macOS run also caught a race
  in the transport: a handshake that passed the policy before a manifest
  change could complete after `Recheck` and be filed unchecked. A
  connection is now checked against the policy once more when it is
  filed.
- 2026-11-03, slice 2: repair without the owner. The hub channel gains
  the hub-signed repair task, the worker's report and the surplus release
  hint. The scheduling hub (the first in manifest order whose status page
  is fresh, the next taking over after ten minutes of silence) turns each
  object due into a task every minute: missing numbers to live servers
  with room above their floors, servers holding nothing of the object
  first, at most ceil(n / servers not dead) per server, spread raised by
  copying from the fullest holder, never a plan on fewer than k servers;
  blocked, with the bytes short, when no server has room. A storage node
  volunteers as a worker in its heartbeat and takes at most two tasks at
  a time. The worker fetches the descriptor and k shares (its own store
  for what it holds), regenerates the missing numbers with
  `object.Reconstruct`, which needs no key, checks them against the
  signed roots and stores them, through the protocol or into its own
  store when the task names it, then reports. The server accepts a share
  from a device that is not the owner's only with a task signed by a
  listed hub that names it, the object, the share number and the worker,
  past the owner's cap but never its floor, never over a tombstone, and
  for a leaving member too. Tasks close on the census, the report or
  their expiry and sit in `placement.sqlite`. A returning server's
  surplus is hinted to the holder with the most shares, which asks the
  others directly and removes its copy only when every share number stays
  complete. The page shows running, repaired and failed counts, the
  blocked objects with the bytes short, and which hub schedules. The
  slice's exit test runs in process: three nodes, one dies, the hub's
  task restores full placement with the owner idle, the node returns and
  the surplus is released. Shipped as alpha.11 the same day; the fleet
  was on it by 14:30 UTC, the page naming hub-hetzner as the scheduler.
  The first real table showed 1890 objects with 9 under-placed: seven
  with every share on one server (the engine's retry after a refusal),
  one short of a number, one unreadable stub with a single share that
  only the cleanup of slice 3 can remove. `storage drop` was added for
  the drill: it removes one share on a node beside the running agent, so
  a fully placed object loses a share on purpose and the task follows.
  The drill passed on the live grid the same afternoon: share 3 of a
  10 MB object dropped on falcon-1 at 14:57 UTC, task issued to falcon by
  the next tick, rebuilt from falcon's own two shares and stored back on
  falcon-1, reported done eleven seconds after the issue.
- 2026-11-03, slice 3: deletion. The store applies a delete token past
  the 7-day immunity on its own clock and only when the token is newer
  than the last touch, tombstones, purges after the 30-day window and
  keeps the record (dropped 90 days after the purge), undeletes within
  the window, lists an owner's objects by page, and purges a member's
  shares, roots and tombstones once `grace_days` have run after it first
  found them leaving or unlisted, clearing the note when they reappear.
  The share protocol gains `delete` with the signed token, `undelete`
  and `list`, and the statuses `recent` and `stale`. The engine sends
  tokens to every server, lists the member's objects across them, reads
  a tombstoned object for the owner within the window, and probes the
  servers before trusting a cache row older than six days. The backup
  probes a returning file's cached capability before a root is published.
  The cleanup job computes the reachable set on the grid from every root
  under quorum, refuses on an unreadable root or directory and on more
  than half the grid as garbage unless forced, and skips roots of devices
  revoked longer ago than the window; it runs weekly, on `backup
  cleanup`, with a dry run, and its report is on the local page. The
  node's maintenance purges tombstones and applies membership on its
  clock with the manifest's grace. The page's tombstone count holds only
  while a server still reports a share. Exit test in process on a real
  grid: an orphan refused within the immunity and tombstoned past it, a
  pruned snapshot's objects collected, the purge after the window, the
  owner's read failing only then. Pending sets, the RFC's third mechanism
  for a backup in progress, are not needed yet: a run's new objects are
  touched by their upload and its reused ones by the probes.
- 2026-11-03, slice 4: rebalance, and M5 complete. The manifest's
  `encoding_transition` gains its rules (to must be the encoding, from
  and to differ, a warning past the deadline) and `Converting(now)`.
  Servers account the old encoding at zero until the deadline. The hub's
  placement table counts objects per encoding: the page shows the
  conversion bar, the old bytes and whether they count, and per member
  the objects to convert. The owner's device converts its snapshots in
  `internal/rebalance`: every object under the old encoding is read and
  stored again under the new one, directories are rewritten up to the
  top, the scan rows are rewritten before the root is published after
  each snapshot, so the next backup reuses the new objects and never the
  old; a stopped run resumes. It runs every six hours while a transition
  is declared and on `backup rebalance`. Exit test in process on a real
  grid with the hub: a backup under 2-of-4, the manifest moved to 2-of-3,
  the conversion, the hub's bar reaching the end, a restore of the
  converted snapshot, the next backup uploading nothing, and the old
  objects tombstoned by a forced cleanup. The live cleanup of the stalled
  first backup's 1761 objects waits for the immunity to run out on
  10 November. The live conversion ran the same evening: manifest
  revision 4 moved the encoding to 2-of-3 with a transition until
  3 December, and `backup rebalance` on the Mac converted the ten
  snapshots of its index. The drill showed that snapshots sharing
  unchanged objects were each walked afresh, re-reading every object and
  counting it again (no second upload, the cache stopped that); the
  replacements and the walked directories are now kept for the whole
  run, so each object is converted once, which the exit test checks with
  two identical snapshots.

### M6. The second platform and the G2 month

- A macOS or Windows member backs up and restores: the Windows scan key and
  logon task, the macOS privacy probe for each folder, cloud placeholders
  skipped and counted, the default exclude lists, the diagnostics bundle.
- Signing and notarization in the release workflow once the credentials
  exist, so the first run on that platform is one dialog, not a hunt through
  settings.
- The month of weekly restore tests on the maintainer's data, recorded on
  the page, and the G2 record beside this plan.

## Order and reasons

M1 before anything because the format must be frozen before a byte lands on
a disk; M2 before M3 because a client without servers tests nothing; M4
before M5 because the backup job starts the G2 clock and repair protects
data that must first exist; M6 last because it needs real members' machines.
Each milestone is sized for one or two weeks of work at the pace of phase 1;
the calendar room before July is for the month of restore tests, the
cross-platform member and what the live grid will teach.

## Decisions taken with this plan

- The engine's storage root on a node is `<storage path>/redundanet3`, so
  today's `--storage-path /mnt/storage` is also the engine's disk.
- Today's live encoding, 2-of-4, is the native grid's first encoding; with
  two storage servers the small-network rule places two share numbers per
  server, and the collector's target is min(n, servers), so the page will not
  show the first objects as under-placed.
- Libraries, as in the RFC: klauspost/reedsolomon, zeebo/blake3,
  modernc.org/sqlite; the standard library for AES-GCM and Ed25519.
