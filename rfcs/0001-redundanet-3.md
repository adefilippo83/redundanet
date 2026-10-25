# RFC 0001: RedundaNet 3

One binary on Windows, macOS and Linux, with a native storage engine.

- Status: Draft
- Date: 2026-10-23
- Author: Alessandro De Filippo
- Discussion: https://github.com/adefilippo83/redundanet/discussions/132

---

## Part 1: Product requirements

### Summary

RedundaNet 3 is one binary that turns a group of people who trust each other
into a backup service for each other, on Windows, macOS and Linux, with no root
and no Docker. A member installs it, keeps a disk online, and gets space on
everyone else's disks in proportion to what they contribute. Files are
encrypted on the member's own machine and spread with erasure coding, so no
single node, and no single member, can read or lose anyone's data.

What stays from RedundaNet 2: the social contract (membership is a pull request
reviewed by people), the Git manifest as the source of truth, client-side
encryption, k-of-n erasure coding, the hub as a dumb relay plus a public status
page, and the GPL-3 licence that lets any community fork the repository and run
its own network.

What changes: Docker and tinc go away, replaced by an in-process userspace mesh
that needs no privileges; the Python runtime and the gpg binary go away,
replaced by a single static executable with its own keys; and, in a second
step, Tahoe-LAFS is replaced by a native storage engine that knows who owns
each share, so quotas are enforced by servers instead of trusted, repair works
without the owner, and there is no lease crawler to break.

The promise in one line: a storage community you join with a pull request, run
from one binary, that no company can take away.

### Problem

RedundaNet 2 works, with 10 nodes, 6 members, 263k objects and no file lost in
19 months, but it only works for people who already run Linux servers. Every
pain point below has been hit on the live network, not imagined.

**For a member**

- Linux only, root required, Docker required. The three candidates who would
  have joined from a Mac or a Windows PC could not.
- Joining takes a GPG key on a keyserver, a form, an issue, a bot PR and a
  merge, then a CLI with four commands. The identity step alone loses most
  non-technical people.
- Backup is a Samba share plus a sidecar on the node. A laptop user has nothing
  to point at a folder.
- Restore on a new machine depends on a capability string the member has to
  keep somewhere. Nobody has tested a restore from scratch.

**For the operator**

- Every outage of the last year came from the plumbing, not the storage: the
  tinc network namespace trap, the compose override silently dropped, the disk
  that mounted empty, Compose v1 on a new OS.
- Tahoe-LAFS upstream is dormant since December 2024. Its SFTP frontend never
  closes sessions, its lease crawler crashed on every restart and nobody
  collected garbage from September to October 2026, its dependencies are
  pinned by hand after a pyOpenSSL release broke every container.
- Quotas are on trust because the storage server does not know whose share it
  holds. A usage report 11 days old is the normal state of one member today.
- Repair needs the owner's client. If a member is away for months, nobody can
  repair their objects.
- The hub is a rendezvous that must see real IP addresses, which a proxied host
  cannot do, so the network needs two hubs of two different kinds.

**For a community that wants its own network**

- The quickstart says fork the repo and edit the manifest, and that is true,
  but the result is a Linux fleet with a Docker stack to operate, not something
  a club hands to its members.

The common cause is that RedundaNet 2 is a deployment of three excellent but
unrelated programs (Tahoe, tinc, Docker) glued by Python. The glue is where the
failures live, and the glue is what forbids other platforms.

### Goals and non-goals

**Goals**

1. One binary per platform (Windows x64, macOS arm64 and x64, Linux x64 and
   arm64) that installs without administrator rights and runs as a user
   service.
2. A member joins with the app: generate a key, write down a recovery phrase,
   submit a request, wait for a human to approve. No GPG, no CLI, no Git
   knowledge.
3. Back up folders from any device, restore them on any device with the
   recovery phrase and nothing else.
4. Contribute a disk and receive an allocation in proportion, enforced by the
   storage servers, not by trust.
5. Durability that is measured and repaired by the network itself, including
   for members who are offline for months.
6. Keep the trust model: client-side encryption, hosts see only ciphertext,
   membership decided by people, identity owned by the member, no central
   server that can read, admit or expel on its own.
7. Keep the fork story and make it real: a community starts its own network
   with one manifest and one hub, documented end to end.
8. Migrate the live network without a flag day and without losing a file.

**Non-goals, for this version**

- Public or anonymous networks. Members are people vetted by people; the design
  assumes it.
- Mobile apps. A phone backs up through a desktop or a NAS node, if at all.
- Real-time sync between devices, shared folders, or multi-writer
  collaboration. This is backup, one writer per root.
- A hosted service, billing, or any revenue path. Communities fund their own
  hubs, as today.
- Reading RedundaNet 2 share files with the new engine. Migration is by
  re-upload (see Migration plan).
- A native GUI toolkit. The first interface is a local web page served by the
  agent; a tray application comes later if members ask for it.

### Users and jobs to be done

Three kinds of people use the product, and they want different things from it.

| User | Who they are today | What they need to do | What success looks like |
| --- | --- | --- | --- |
| Member | Six people in the live network, mostly technical; the next ones are friends of members on Mac or Windows | Join, keep a node online, back up folders, restore on a new machine, see that their data is safe | Joined in a day, first backup the same evening, a restore test that works, never thinks about it again |
| Community operator | The maintainer, and the one or two people who review join requests | Run the hub, approve and remove members, watch the status page, roll out updates, handle the member whose node is misbehaving | An update that cannot strand a node, a page that tells the truth, a member removal that is one merge |
| Forker | A LUG, a cooperative, an office, a large family | Start a network of their own with their own rules and keep it running without the original maintainer | A manifest, a hub and a first member in one afternoon, following one guide |

The member is the one the binary is for. The operator and the forker are served
by the same binary in hub mode plus the repository as a template.

A member has two kinds of devices: a storage node that stays on (a Raspberry
Pi, a NAS, an old PC) and client devices that come and go (a laptop). The
product must treat both as the same member, and a member with only a laptop
must still be able to join as a client without contributing disk, at the
allocation their community decides.

### Principles

The six principles of RedundaNet 2 stay, worded as tests a design must pass.
Three are added by the move to a product.

1. **Nobody can read your data.** Encryption happens on the member's device; a
   storage host, a relay, the hub and the maintainer see ciphertext and opaque
   identifiers. Test: a complete copy of every server disk plus the hub reveals
   no file name and no content.
2. **Any node can die.** Erasure coding with k-of-n chosen by the community;
   the network repairs itself. Test: with n minus k servers down, every object
   is readable; when they come back or are replaced, the network returns to
   full placement without human action.
3. **No central authority.** Identity is a key the member generates;
   membership is a file in Git that people review; the hub cannot admit, expel
   or impersonate anyone. Test: a hub fully compromised can delay traffic and
   lie on a web page, nothing else.
4. **Any hardware, behind NAT.** Home routers, no port forwarding, slow
   uplinks. Test: two members both behind NAT exchange data; a relay carries it
   when hole punching fails.
5. **Verified members, not anonymous.** People approved by people, and the
   rules that rest on that trust are stated. Test: the manifest says who
   vouched for whom.
6. **Boring operations.** One binary, a service that restarts itself, an update
   that rolls back on failure, a status page that tells the truth. Test: an
   operator can leave for a month.
7. **No privileges.** (new) Install and run as an ordinary user on every
   platform; nothing in the design needs a kernel driver, a TUN device or a
   root daemon.
8. **Fairness is enforced, not assumed.** (new) Servers know whose bytes they
   hold and refuse writes beyond a member's allocation; the usage on the status
   page comes from servers, not from self-reports.
9. **Your community, your network.** (new) Nothing in the code points to the
   original network; a fork edits one file and runs its own hub. Test: a second
   network has run for a month on an unmodified binary.

### Functional requirements

Numbered so the design and the milestones can refer to them. "Must" is phase 1
or 2 scope; "later" is after the native engine ships.

**Identity and membership**

- F1. The app generates the member's key on first run and shows a recovery
  phrase once; the phrase alone restores identity and data on another device.
  Must.
- F2. A member can authorize additional devices from an existing one, without a
  change to the manifest, with a role per device (client only, or client and
  storage). Must.
- F3. Joining is a request the app submits (name, public key, contribution, who
  vouches) that lands as a reviewable change to the manifest; a human merges
  it. Must.
- F4. Leaving, or removal by operators, is a manifest change; the member's
  shares are deleted after a grace period the community sets. Must.
- F5. Every node verifies the manifest's origin before applying it; a
  compromised hosting account cannot add a node silently. Later (manifest
  signing).

**Network**

- F6. Nodes reach each other without open ports, with direct paths when NAT
  allows and a relay otherwise; no privileges required. Must.
- F7. Only nodes listed in the manifest, or certified by a listed member, can
  connect. Must.
- F8. Bandwidth limits per node, upload and download, with a schedule, so a
  backup never saturates a home uplink. Must.

**Storage**

- F9. A storage node contributes a directory on a disk of the member's choice;
  the app refuses to run storage on the wrong disk (the October 2026 incident
  must be impossible). Must.
- F10. Allocation per member = contribution × k/n minus the community reserve,
  computed from the manifest; servers refuse writes beyond it. Must (enforced
  in phase 2, computed in phase 1).
- F11. Placement puts the n shares of an object on n distinct servers, and on
  distinct members where possible. Must.
- F12. Every object is checkable and repairable from k shares by any node,
  without the owner's keys. Phase 2.

**Backup and restore**

- F13. A member picks folders; the app backs them up on a schedule,
  incrementally, deduplicated within the member's own data, with snapshots.
  Must.
- F14. Snapshots are retained by a policy the member sets (for example daily
  for 30 days, monthly for a year); pruned objects are deleted from the grid.
  Must.
- F15. Restore any snapshot, whole or by folder, on any device of the member,
  from the recovery phrase alone. Must.
- F16. A restore test the app runs on its own at intervals: download a sample,
  verify hashes, report. Must.
- F17. Excludes by pattern; large files resumable across restarts; moved and
  renamed files not re-uploaded. Must.

**Health and operations**

- F18. Each node reports liveness, free space, and a census of what it holds;
  the hub aggregates them into a public status page with per-object placement
  counts. Must.
- F19. The network repairs under-placed objects and rebalances after a k-of-n
  change, without human action; the status page shows progress. Phase 2 for
  repair without the owner.
- F20. Self-update from signed releases, with a health check and automatic
  rollback to the previous binary; the manifest can require a minimum version.
  Must.
- F21. A local page on each node shows status, backups, restores, logs and
  settings; the CLI drives the same API. Must.
- F22. The hub mode runs in the same binary: relay, manifest mirror, status
  page, join endpoint. Must.

**Fork kit**

- F23. One guide takes a new community from an empty repository to a running
  hub and a first member; the binary carries no reference to the original
  network except a default that the manifest overrides. Must.

### Non-functional requirements

The numbers come from the live network: a Raspberry Pi 4 with a USB disk, a
2 GB virtual machine, an uplink of 0.5 Mbit/s at one member, 263k objects today
and a million as the design point.

| Area | Requirement |
| --- | --- |
| Platforms | Windows 10+ x64; macOS 12+ on arm64 and x64; Linux glibc x64 and arm64 (Raspberry Pi 4 and 5, 64-bit OS). One static binary per target, no runtime to install |
| Privileges | Install, run and update as an ordinary user. No kernel driver, no TUN device, no root service. A per-user service that starts at login (and at boot on Linux with lingering) |
| Footprint | Agent idle under 100 MB RSS and under 1% CPU on a Pi 4; a storage node with 1M shares indexes them in SQLite and answers a census query in under 2 s without walking the disk |
| Throughput | Upload and download bounded by the member's uplink, not by the app: erasure coding and encryption above 300 MB/s on a laptop, above 80 MB/s on a Pi 4; a relayed path above 50 Mbit/s on the hub's side |
| Transfers | Every upload and download resumable at segment granularity across restarts and network changes; backoff on a dead peer; a node switching from relay to direct path keeps the transfer |
| Durability | With k-of-n = 2-of-4 and placement on distinct members, any two servers lost at once lose nothing; repair starts within one hour of a server being declared dead and completes at the speed of the slowest surviving uplink |
| Consistency | A member's root advances only by its own signed versions; two devices of one member never overwrite each other's snapshots (one root per device) |
| Availability | Reads need k servers up; writes need n distinct servers up, or the write is marked under-placed and repaired later; the hub down means no new connections for peers without a cached direct path, nothing else |
| Security | Ed25519 identities, authenticated encrypted transport, per-object keys, authenticated encryption on every segment, signed share ownership, signed releases verified before install |
| Privacy | Servers learn storage indices, sizes, owner identities and timing; never file names, counts of files per folder, or content. The hub learns liveness and census summaries |
| Compatibility | The manifest schema is versioned; an agent refuses a manifest newer than it understands and updates itself; the share format on disk is versioned from day one |
| Observability | Structured logs with a ring buffer visible in the local page; a status endpoint per node; the hub's page is the only public surface |
| Supportability | One command (or button) produces a diagnostic bundle: versions, manifest revision, peers reachable, last errors, disk check. No secrets in it |

### User journeys

Each journey is written as the member would tell it. The design sections say
how each step works.

**First run.** I download the app for my machine and open it. It asks for a
name and shows me twelve words to write down, and makes me type three of them
back. It asks whether this machine contributes disk; I pick a folder on my
external drive and a size. It shows a request to join the network of my
friends, with the name of the member who invited me. I send it. The app says
"waiting for approval" and shows me the page where that happens.

**Approval.** The operator sees a pull request with my name, my key, my
contribution and who vouched for me. They check the vouch, merge, and within
five minutes my app says "connected, 7 peers". I never saw Git.

**Backup.** I pick three folders and a schedule. The first backup runs for two
days on my uplink at the limit I set; the page shows progress, what is waiting,
and that each file is on four servers. From then on it runs every 15 minutes
and uploads what changed.

**Restore on a new laptop.** The old laptop is gone. I install the app, choose
"I already have a recovery phrase", type the twelve words. It finds my roots on
the network and lists my devices and snapshots. I pick last Tuesday and a
folder, and get it back, with the hashes verified.

**My node dies.** My Pi's disk fails. The status page shows my node down and my
member's objects still readable; after an hour the network starts repairing the
shares that lived on my disk onto other servers. I replace the disk, the app
sees an empty folder with no marker, asks me to confirm it is a new disk, and
rejoins. Nobody else did anything.

**I leave.** I ask the operator to remove me, or I open the request from the
app. After the merge my shares are deleted everywhere after the grace period,
my allocation goes back to the pool, and my devices are refused at the door.

**Update.** A new release appears. Each node downloads it, checks the
signature, swaps the binary, restarts, and checks that it can reach peers and
serve its page. One node fails the check and rolls back on its own; its owner
gets a line in the local page, the operator sees it on the status page.

**I start my own community.** I fork the repository, write a manifest with my
name as first member and my VPS as hub, run the hub mode there, install the app
on my machine pointed at my repository, and invite the next person. The guide
took an afternoon.

### Success metrics

Measured on the live network, read off the status page or the release
pipeline, not estimated.

| Metric | Today (RedundaNet 2) | Target after phase 1 | Target after phase 2 |
| --- | --- | --- | --- |
| Time from download to "connected" for a new member, excluding the human approval | Hours, Linux only | Under 30 minutes on any platform | Under 15 minutes |
| Members on macOS or Windows | 0 of 6 | 2 | Half of new members |
| Restores from the recovery phrase alone, tested per member per quarter | 0 | 1 per member, run by the app | 1 per member, run by the app |
| Usage figures older than 24 h on the status page | 1 member of 6 | 0 (still self-reported) | 0 (server-reported) |
| Objects under-placed for more than 7 days | 12k of 263k | Under 1% | Under 0.1%, repaired without owners |
| Plumbing incidents per quarter (network namespace, mounts, compose, packaging) | 4 in the last two quarters | 1 | 0 |
| Nodes stranded by an update | 0, thanks to rollback | 0 | 0 |
| Communities running their own network on the binary | 0 | 1 trial | 2 |
| Agent memory on a Pi 4 at 250k shares | 440 MB (Tahoe plus sidecars) | Under 300 MB | Under 100 MB |

### Phases

The binary ships before the engine, so the community gains something within
months and the engine never blocks it. Each phase ends with the live network
running on it. The drawn roadmap with gates is under Spike plan and
milestones.

**Phase 0, spike (2 weeks).** Decide the transport and the language with a
prototype: two Tahoe nodes on two laptops, one behind NAT, talking through the
new mesh and the loopback bridge; measure throughput against tinc and
reconnection after a network drop. Exit: a written decision with numbers.

**Phase 1, the agent on Tahoe (3 months).** One binary with identity, manifest
sync, mesh, loopback bridge, Tahoe as a bundled child process, census, usage
meter, lease sweep, backup of folders, local page, user service, self-update,
hub mode with relay. Ships as a sidecar container on existing Docker nodes
first, then as the native install for new members. Exit: a macOS or Windows
member in the live network, tinc removed from every node.

**Phase 2, the native engine (4 to 6 months).** Objects, encryption, erasure
coding, ownership, server-side quotas, roots and snapshots, repair without the
owner, deletion without leases, census from the server index. Runs as a second
grid on the same agents while Tahoe keeps serving the old one. Exit: the
maintainer's own backups run on it for a month with weekly restore tests.

**Phase 3, migration (1 to 2 months).** Members re-upload into the new grid at
their own pace; the status page shows both; Tahoe is switched off per node when
its member has moved, then removed from the binary. Exit: no Tahoe process on
the network.

**Phase 4, product polish (ongoing).** Tray application, manifest signing,
device management, restore test reports, the fork guide validated by a second
community.

Calendar time for one person in spare time is about a year to the end of
phase 3. Nothing in phase 2 is on the critical path of phase 1.

### Risks

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| The rewrite takes two years and the live network gets nothing meanwhile | High if the engine comes first | Community loses interest | Phase order: the binary ships on Tahoe first; the engine is behind an interface and never on the critical path |
| The transport library churns or dies (iroh is young; libp2p is heavy) | Medium | Rework of one layer | Transport behind an interface with a loopback-bridge fallback; the spike picks with numbers, not opinions |
| A bug in the native engine loses data | Low with care, catastrophic | Trust gone | Tahoe keeps the old grid until phase 3; integrity trees on every segment; a restore test is a product feature; the maintainer's own backups go first |
| Server-side quotas are gamed by a member with many devices | Low (vetted members) | Unfairness | Quotas are per member, devices are certified by the member key; the status page shows per-member usage from servers |
| Windows: antivirus flags the binary, service install needs admin, paths and locks differ | Medium | Support load | Signed releases (Authenticode) from the start; scheduled task at logon instead of a service; CI runs the test suite on Windows |
| Relay cost grows with members behind symmetric NAT | Medium | Hub bandwidth bill | Direct paths preferred; relay metered on the status page; a community can add relays as it adds hubs today |
| Loss of the recovery phrase loses everything | Certain for some members | Their data | The phrase is shown once and verified by typing it back; the app nags until a restore test passes; no escrow, by principle |
| One maintainer | Certain | Bus factor | Written design (this document), CI that gates releases, the fork kit so a community can carry on with the code |
| Tahoe dormant until phase 3 | Certain | Known bugs stay | Already lived with; the base-image patch and pins stay until Tahoe is removed |

### Open questions

Decisions the maintainer or the community must make; each has a recommended
answer in the Design part.

- [ ] Go or Rust (Design: Language and library choices recommends Go).
- [ ] Transport: iroh, libp2p, or tsnet with a self-hosted Headscale
      (recommends iroh if Rust, pion plus a self-hosted relay if Go; the spike
      decides).
- [ ] Keep PGP keys as an optional cross-signature for members who already have
      one, or drop them entirely (recommends drop; the manifest records the
      vouch instead).
- [ ] Grace period before a removed member's shares are deleted (recommends
      30 days).
- [ ] Default k-of-n for new networks (recommends 2-of-4 until a network has
      8 storage nodes, then 3-of-6).
- [ ] Does the hub create the join pull request through a bot, or does the app
      talk to GitHub directly (recommends the hub, so the app never needs a
      GitHub token).
- [ ] Manifest signing threshold once introduced: one maintainer key, or two of
      three (recommends one at first, two of three when a network has three
      operators).
- [ ] Segment size (recommends 1 MiB; small files are one segment).
- [ ] Should client-only members get an allocation, and how much (community
      rule, recorded in the manifest).
- [ ] Name and versioning: RedundaNet 3.0 as a new major, or a new name for the
      binary.

---

## Part 2: Design

### Architecture overview

One binary, `redundanet`, runs in three modes: **agent** on every member
device (client role, storage role, or both), **hub** on a server (relay,
manifest mirror, status page, join endpoint), and **CLI**, which only talks to
the local agent's API. Everything a node does happens inside the one agent
process; there are no containers, no sidecars and no external daemons.

![One binary, three modes: the hub introduces and relays, never stores or decides](images/0001-architecture.png)

Devices exchange data directly over QUIC and go through the hub's relay only
when NAT forbids a direct path; the manifest in Git is what admits a device,
and the hub never holds a key or a share.

The agent is six components behind interfaces, so that each can be replaced
without touching the others:

1. **Identity**: the member key, the device key and the certificate that binds
   them (see Identity and keys).
2. **Membership**: fetches the manifest, verifies it, derives the peer
   allowlist, the allocation table and the network parameters (see
   Membership).
3. **Transport**: a userspace mesh endpoint identified by the device key;
   direct paths through NAT when possible, the hub's relay otherwise;
   authenticated streams to any allowed peer, and nothing to anyone else (see
   Transport).
4. **Storage engine**: in phase 1 an adapter that runs Tahoe as a child process
   and bridges it onto the mesh through loopback ports; in phase 2 the native
   engine, a client library and a server that speak over mesh streams (see
   Storage engine interface, Native engine).
5. **Jobs**: backup, census, check and repair, rebalance, deletion, restore
   test, each a scheduled task with a budget and a resumable state in SQLite
   (see Backup and restore, Health).
6. **Surface**: the local HTTP API with its web page, the updater and the
   service integration (see Local API, Service integration).

The hub reuses the same components: transport in relay mode, membership as the
directory that serves the manifest to agents, a status collector that is
today's monitor code fed by census streams, and a join endpoint that turns an
app request into a pull request.

The data path for a backup is: scan folders, split files into segments,
encrypt each segment with a per-object key, erasure-code it into n shares, send
each share to a distinct server over a mesh stream signed by the device, write
the snapshot as an object of the same kind, then publish a signed root record
that names the new snapshot. Reading is the reverse from any k shares. Servers
never see a key, a name or a plaintext byte.

### Identity and keys

A member is a 128-bit seed, shown once as a 12-word recovery phrase.
Everything else is derived from it or certified by it, so the phrase alone
restores a member on a new device, and nothing but the phrase can.

| Key | Derived how | Used for | Lives where |
| --- | --- | --- | --- |
| Member signing key (Ed25519) | HKDF(seed, "sign") | Certifies devices, signs root records and deletion requests, signs the join request | Every device of the member, encrypted at rest |
| Member secret (32 bytes) | HKDF(seed, "secret") | Derives the convergence secret (dedup only within the member's own data) and the key that encrypts root records | Every device of the member |
| Device key (Ed25519) | Random per device | Transport identity on the mesh, signs every storage request | That device only |
| Device certificate | Signed by the member signing key over (device public key, member public key, roles, issued at, serial) | Proves to servers and peers that a device acts for a listed member | Presented on connect; cached by peers |

The manifest lists members by their signing public key. Servers accept a
request when the device certificate chains to a listed member and the member
is active; they never need to know a device in advance, which is what makes
adding a laptop a local act (F2) and not a pull request.

**Adding a device.** Phase 1: type the phrase on the new device; it re-derives
the member keys and certifies its own device key. Later: pairing, where the new
device shows a code and an existing device sends it the seed over an
authenticated mesh stream, so the phrase stays in the drawer.

**Losing a device.** The member revokes its certificate from any other device;
the revocation is a signed record the hub mirrors with the manifest and that
every peer and server honours within one sync interval. A stolen laptop
without the phrase can read what it had cached and nothing new after that.

**Losing the phrase.** The data is unreadable to everyone, by principle; there
is no escrow. The app verifies the phrase by asking for three words at first
run, and the restore test (F16) proves periodically that the phrase on paper
still works, because it runs the derivation from the words the member types,
not from the stored keys.

**Key storage at rest.** Phase 1: a file readable only by the user, encrypted
with a key held in the OS keystore where one exists (Keychain on macOS, DPAPI
on Windows, the secret service on desktop Linux) and otherwise with an optional
passphrase. A headless Pi runs without a passphrase, as today.

**Display.** A member's identity is shown as a 10-group code of the signing
public key; operators compare it against the join request. PGP keys are no
longer required; a member who has one may add a PGP signature of their member
key to the join request for continuity with RedundaNet 2, and the manifest
records it as a vouch, not as the identity.

### Membership: the manifest

The manifest stays a YAML file in a Git repository, reviewed by people, and
stays the only source of truth. Version 3 of the schema separates the three
things RedundaNet 2 mixed in one node list.

```yaml
schema: 3
network:
  name: redundanet
  encoding: {k: 2, n: 4}
  reserve_percent: 15
  grace_days: 30          # before a removed member's shares are deleted
  min_version: 3.0.0      # agents older than this update before connecting
hubs:
  - name: hub-hetzner
    relay: 2.29.46.249:4433
    status: https://status.example.org
    key: <device public key>
members:
  - name: alessandro
    key: <member signing public key>
    contribution_gb: 1800
    vouched_by: [ptlug]
    joined: 2025-03-01
    status: active        # active | leaving (grace running) | removed
storage:
  - member: alessandro
    device: <device public key>
    name: falcon
    port: 40011           # loopback port for the Tahoe phase; unused by the native engine
```

What is deliberately not in it: client devices (certified by member keys,
never listed), VPN addresses (there is no VPN), introducer furls (phase 1 keeps
them in a side file; the native engine has no introducer), and PGP fingerprints
(optional vouch records only).

**Distribution.** Agents fetch the manifest over HTTPS from the repository's
raw URL, keyed by commit, every five minutes as today, with the hub as a mirror
for agents that cannot reach GitHub. The hub also mirrors the revocation list
and the signed release index. Phase 4 adds a signature over the manifest by
operator keys listed in the manifest itself (one key at first, a threshold
later); until then the guarantee is the repository's branch protection and
review, as it is today, and the document says so.

**Joining.** The app builds a request (name, member key, contribution,
inviter) and posts it to the hub's join endpoint. The hub, holding a bot token,
opens the pull request that edits the manifest; CI validates the schema; a
human merges. The inviter is asked in the pull request to confirm, which is the
social step. Nothing changes for the operator except that the bot now receives
requests from the app instead of a web form.

**Leaving and removal.** A merge sets the member to `leaving`; servers stop
accepting their writes at once and delete their shares after `grace_days`;
their devices are refused on connect. A second merge removes the entry. A
member can reverse a `leaving` within the grace period with another merge, and
their data is still there.

**Allocation.** Unchanged in formula: allocation = contribution × k/n minus the
reserve. It is computed by every node from the manifest, so servers and clients
agree without talking, which is what lets servers enforce it (see Ownership
and quotas).

### Transport

The transport is a userspace mesh inside the agent process: one UDP socket,
QUIC with TLS 1.3, the device key as the TLS identity, and the hub as
rendezvous and relay. No TUN device, no VPN subnet, no privileges. It replaces
tinc, the VPN addresses in the manifest and the two-hub arrangement that
existed because a proxied host could not see real addresses.

**Connecting to a peer.** Each device keeps one long-lived connection to a hub
relay and publishes there the address candidates it learns from STUN-style
reflection. To reach a peer, a device asks the relay for the peer's candidates,
starts sending through the relay immediately, and attempts a direct path at the
same time (simultaneous open through both NATs). When the direct path answers,
traffic moves to it; when it dies, traffic falls back to the relay without the
stream noticing. This is the model iroh and Tailscale implement; the spike
decides whether to take it from a library or build the thin version on QUIC.

**Who may connect.** The handshake carries the device certificate. A
connection is accepted only if the certificate chains to a member whose status
is `active` in the current manifest, or if the device key is a listed hub, and
the certificate is not on the revocation list. Everyone else is refused before
any application byte. The allowlist is recomputed on every manifest sync, and
an existing connection to a newly removed member is closed.

**Streams and messages.** ALPN `redundanet/1`. One bidirectional stream per
request, framed as length-prefixed CBOR messages; large payloads (shares,
segments) stream after the header. QUIC gives flow control, multiplexing and
connection migration, so a laptop that changes network keeps its transfers.

**Relay.** The hub's relay forwards packets between two authenticated devices
and sees only QUIC ciphertext, because the QUIC session is end to end between
the devices. The relay meters bytes per device for the status page and applies
the same allowlist. A community that outgrows one relay adds another hub entry
in the manifest; devices prefer the relay with the lowest latency and fail
over.

**Bandwidth.** A token bucket per direction at the endpoint, with a schedule
(for example 2 Mbit/s up during the day, unlimited at night), configurable in
the local page (F8). The limit applies to storage traffic, never to control
messages, so a node under a tight cap still answers the census and the hub.

**The loopback bridge (phase 1 only).** Tahoe only needs TCP to a hint it was
given. The manifest assigns each storage device a loopback port; the agent
listens on `127.0.0.1:<port>` for every peer and tunnels each accepted TCP
connection to that peer's Tahoe over a mesh stream. Each Tahoe server
advertises `tub.location = tcp:127.0.0.1:<its own port>`, and the introducer
is reached the same way through the hub's port. Tahoe keeps its own tub keys,
so the storage trust is unchanged, and Tahoe needs no modification. The bridge
is deleted in phase 3.

**What the transport does not do.** It does not route IP packets, so a member
cannot reach another member's machine except through the storage protocol.
That is a feature: the attack surface of a node is the agent's request
handlers, not an operating system behind a VPN.

### Storage engine interface and the Tahoe adapter

Everything above the engine (backup, restore, census, repair scheduling, the
local page) is written against one interface, so phase 2 swaps the engine
without touching them. The interface is deliberately small and
immutable-first.

```go
type Cap string                      // opaque to callers; carries key, verifier and encoding

type Engine interface {
    Put(ctx, r io.Reader, size int64) (Cap, error)          // immutable object, deduplicated per member
    Get(ctx, cap Cap, w io.Writer, rng *Range) error
    Check(ctx, cap Cap) (Health, error)                     // which servers hold which shares
    Repair(ctx, cap Cap) (Health, error)
    Delete(ctx, cap Cap) error                              // owner-authorized, see Health
    PutRoot(ctx, root Root) error                           // this device's signed root record
    GetRoots(ctx, member Key) ([]Root, error)               // every device root of a member
    Census(ctx, since Generation) (CensusStream, error)     // what this node's server holds
}
```

`Health` names servers, share numbers, and whether placement meets n distinct
servers and distinct members. `Root` is the signed record described under
Mutable state.

**The Tahoe adapter (phase 1).** Tahoe ships inside the release as a
self-contained child process per platform (built with PyInstaller in CI from
the pinned lock file, as the Docker images are today). The agent writes
`tahoe.cfg` (storage directory, the loopback `tub.location`, the introducer
reached through the bridge, the 90-day expiry settings), starts and supervises
the process, and implements the interface over Tahoe's local web API:

- `Put` and `Get` are `PUT /uri` and `GET /uri/<cap>` with the per-member
  convergence secret Tahoe already supports.
- `Check` and `Repair` are `t=check&add-lease=true` and `repair=true`, the
  calls the lease sweep makes today.
- `PutRoot` writes the device's snapshot list into a Tahoe mutable directory
  per device; `GetRoots` reads them. Snapshots are Tahoe immutable directories
  (`t=mkdir-immutable`), so the backup job is already engine-neutral in
  phase 1.
- `Delete` is a no-op that lets the lease expire; `Census` is today's
  incremental share walk, ported.
- Quotas are computed from the manifest and enforced by the client (the backup
  stops when over), as today.

The adapter carries over the two Tahoe scars without hiding them: the lease
crawler patch stays in the bundle, and the dependency pins stay in the lock
file. Both disappear with the adapter in phase 3.

### Native engine: objects, encryption, erasure coding, shares

The native engine keeps Tahoe's good ideas (capabilities as access, convergent
encryption, k-of-n shares, integrity verified on read) and drops what the
community does not need (mutable files, leases, the introducer, Foolscap).

**Object.** An immutable byte string of any size, split into segments of 1 MiB
(the last one shorter). Files under 256 bytes are not objects; they are stored
inline in the snapshot entry.

**Key and storage index.** `key = BLAKE3_keyed(convergence_secret,
plaintext_hash)` where the convergence secret derives from the member secret,
so an identical file uploaded twice by the same member yields the same object
and the second upload is a no-op, while two members never learn they hold the
same file. `storage_index = BLAKE3("si" || key)[0:16]`: servers address shares
by it and cannot derive the key from it.

**Encryption.** Each segment is encrypted with AES-256-GCM under the object
key, nonce = segment index, additional data = (storage index, segment index,
object size). The key is unique per object, so the counter nonce can never
repeat under one key, and re-encrypting the same object produces the same
ciphertext, which repair and dedup rely on.

**Erasure coding.** Reed-Solomon over GF(2^8), systematic, k data blocks and
n minus k parity blocks per segment. Share i holds block i of every segment, so
a share is a sequence of blocks and any k shares rebuild every segment. A
segment costs n/k times its size on the grid; 2-of-4 is 2x, 3-of-6 is 2x with
a wider margin.

**Integrity.** For each share, the BLAKE3 hashes of its blocks form a tree
whose root is the share root. The object descriptor lists all n share roots and
the hash of the whole ciphertext. A reader verifies each block against the
share root before decoding and the decoded ciphertext against the ciphertext
hash, so a server that alters a share is caught on the first block.

**Descriptor.** A small CBOR record, public, stored in the header of every
share:

```
{ version, storage_index, size, segment_size, k, n,
  ciphertext_hash, share_roots[n], owner (member key), created }
+ signature by the owner's member signing key over the record
```

The signature covers all n share roots at once. That one choice makes repair
possible without the owner: any node holding k shares can regenerate a missing
share, and the receiving server accepts it because its root is in the signed
descriptor. It also gives the server what it needs for accounting: a verified
owner for every byte it stores.

**Capabilities.** `rn:r1:<key>:<descriptor_hash>:<size>` is the read
capability: whoever has it can fetch, verify and decrypt the object.
`rn:v1:<storage_index>:<descriptor_hash>` is the verify capability: enough to
check and repair, not to read. Both are base32, both carry a version prefix so
the format can change. There are no write capabilities; objects are immutable
and mutable state is the root record.

**Share on disk.** `shares/<si prefix>/<si>/<share number>`: a header (magic,
version, descriptor, signature, share number), then the blocks, then the block
hashes. The server keeps an index in SQLite (`storage_index, share_number,
owner, size, created, verified_at`), so the census and the usage per owner are
queries, not directory walks; a slow integrity job re-reads shares at a bounded
rate and records `verified_at`. The index is rebuildable from the files, and
the marker that ties a storage directory to its node (added in 2.7.29) stays.

### Ownership, accounting and quotas

A server knows the owner of every byte it holds, because the descriptor in
every share header is signed by the owner's member key. That one fact moves
quotas from trust to enforcement and makes the usage figures on the status page
come from servers instead of from a meter each member runs.

**Who may store what.** A `PutShare` arrives on a stream authenticated by a
device certificate. The server checks, in order: the device chains to an
active member; the descriptor's signature is valid and its owner is an active
member; the share root matches the descriptor; the owner is within quota on
this server; the share is not already present. A device may store shares owned
by another member (that is what repair is), and the bytes are always accounted
to the owner named in the descriptor, never to the uploader.

**The budget.** A member's grid budget is allocation × n/k bytes of shares,
where allocation = contribution × k/n minus the reserve, so the budget equals
the contribution minus the reserve: a member may occupy on the grid what they
bring to it, less the community's safety margin (15% by default, kept for
repair during outages and for snapshots that overlap during pruning).

**Enforcement per server.** Shares are not spread perfectly evenly, so each
server allows an owner up to budget / S × headroom, with S the number of
storage servers in the manifest and headroom 2 by default, configurable per
network. A write beyond that is refused with a clear error, and the client's
backup pauses with the reason on the local page. The global check is the
client's: it sums `Usage(owner)` from every server before each backup run and
stops at the budget. Both numbers are visible on the status page, so a member
can see which server is full and why.

**Free space is a separate matter.** A server also advertises its real free
space and refuses writes when it is below a floor, whatever the quota says;
placement skips such servers; the status page flags a disk smaller than the
contribution it backs, as it does today.

**Client-only members.** A member with no storage node has
`contribution_gb: 0` and an explicit `allocation_gb` that the community grants
from its reserve or from a sponsor's contribution, recorded in the manifest as
a vouch. The formula does not change; the manifest is the policy.

**Leaving.** A member marked `leaving` is refused new writes everywhere at the
next manifest sync, keeps read access during the grace period, and their
shares and roots are deleted on every server when the grace period ends. A
removed member's devices are refused at the handshake.

### Mutable state without mutable files: roots and snapshots

Tahoe's mutable files exist to let several uncoordinated writers update one
object. Backup has one writer per device, so all of that machinery is replaced
by a signed, versioned root record per device, and the rest is immutable
objects.

**The root record.** One per device: `{member, device, version, written_at,
index_cap}`, where `index_cap` is the read capability of the device's index
object, encrypted as a whole under the member root key so servers never see
capabilities. The envelope `{member, device, version, ciphertext}` is signed by
the member signing key. `PutRoot` sends it to every live server; a server keeps
the highest version per (member, device) and refuses a lower one. `GetRoots`
asks several servers and takes the highest version per device, so one stale
server cannot roll a member back. A write is confirmed when at least k servers
acknowledge it, and the job retries the others.

**The index object.** An immutable object listing the device's snapshots: for
each, the capability of its top directory object, the time, the folders it
covered, the byte counts. It is small and rewritten on every backup; old
versions become unreferenced and are deleted by the owner's cleanup job.

**Directory objects.** A snapshot is a tree of immutable directory objects, one
per folder, each listing its children: name, type, size, mtime, mode, and
either a read capability or the inline bytes of a tiny file. Because objects
are convergent, an unchanged folder produces the same directory object on every
backup and costs nothing new; only the path from a changed file up to the top
is rewritten. Partial restore walks the tree without downloading what it does
not need.

**Single writer, many devices.** Each device owns its root and never writes
another device's. A member's data is the union of their device roots; restore
on a new device lists every root of the member and picks from any of them.
Retiring a device is a manifest-free act: the new device records in its own
index which old snapshots it keeps, and the old root is deleted by the member
when they no longer want it.

**What this buys.** No write collisions, no version negotiation, no mutable
share format, no server-side lease to keep a root alive. A root's liveness is
the member's active status; everything reachable from a root is live;
everything else is garbage the owner may delete (see Health).

### Backup and restore

The backup job is the product's main feature and is engine-neutral from
phase 1. It scans, deduplicates, uploads, and publishes a root, with every step
resumable.

**Scan.** For each configured folder, walk the tree honouring excludes. For
each file, look up (path, size, mtime, file id) in the device's local database;
a hit reuses the recorded capability without reading the file. A miss hashes
the file with BLAKE3, derives the convergent key and storage index, and asks
the servers `HasShares` for it: a file moved, renamed or already uploaded by
another device of the same member is found present and costs nothing (F17).

**Chunking.** Files above 64 MiB are split into 64 MiB chunk objects, and the
directory entry holds the list of capabilities. This bounds the cost of a lost
connection: on a 0.5 Mbit/s uplink a 32 MiB share takes about nine minutes, and
`PutShare` resumes by offset for a share the server already holds partially,
so no transfer restarts from zero.

**Upload.** Shares go to n servers chosen by placement: distinct servers,
distinct members where the network has enough of them, then free space and
recent success. Fewer than n reachable servers means the object is written to
what is there, recorded as under-placed, and left to repair (see Health).
Uploads run several objects at a time within the bandwidth cap.

**Publish.** Directory objects are built bottom-up, the new index object is
written, and `PutRoot` publishes the new version. A backup that is interrupted
leaves no visible snapshot; the next run continues from the queue in the local
database.

**Retention.** A policy per device, with defaults: everything for 7 days, one
per day for 30 days, one per week for 12 weeks, one per month for 12 months.
Pruning drops snapshots from the index; deletion of what they alone referenced
is the cleanup job's work, with the rule that objects younger than 7 days are
never deleted, so a device that is mid-backup cannot have its fresh objects
removed by another device of the same member running cleanup.

**Restore.** From any device of the member: list roots, pick a device and a
snapshot, pick folders or files. Directory objects are fetched down the chosen
path only; each file is rebuilt from any k shares, verified block by block and
against the ciphertext hash, decrypted and written with its mtime. Existing
files are skipped, overwritten or kept with a suffix, as chosen. A restore
needs the recovery phrase and a manifest URL, nothing else, which is the test
of F15.

**Restore test.** Monthly by default: the app picks a sample of files across
snapshots, restores them to a temporary location, compares hashes, and reports
on the local page and to the status page as a per-member "last verified
restore" date. The derivation starts from the phrase the member types, not
from stored keys, so it also proves the paper copy.

**The local database is a cache.** If it is lost, the device rebuilds it from
its index object and by re-hashing local files; nothing on the grid depends on
it.

### Health: census, check, repair, rebalance, deletion

Durability is measured, not assumed. The pieces below replace Tahoe's lease
crawler, the client-side sweep and the usage meter with a model where the
server knows what it holds and the network repairs itself.

**Census.** Each storage node streams `(storage index, share number, owner,
size)` deltas since a generation out of its SQLite index; this is today's delta
protocol with the walk replaced by a query. The hub keeps the grid-wide
placement table and computes per-object copies, under-placed objects, usage per
member and per server. The status page keeps its shape and its honesty rules;
its usage column now comes from servers.

**Check.** For one object, `HasShares` across servers against n, plus the
integrity state servers report. In the background every server re-reads its
shares at a bounded rate (full pass per month, under 1% CPU) and records
`verified_at`; a share that fails its hashes is removed, reported, and becomes
a repair task.

**Repair without the owner.** The hub's collector turns under-placed objects
into tasks and hands them to storage nodes that volunteer as repair workers,
one task per object with an expiring lease so two workers never duplicate it. A
worker fetches the descriptor and k shares from holders, regenerates the
missing share numbers, verifies them against the signed share roots, and stores
them on servers that lack them. The receiving server accounts the bytes to the
owner, and repair writes may use the reserve above the owner's budget. The
owner is never involved, so a member away for six months stays fully placed.
If the hub is down, repair pauses and durability is unchanged in the short
term; a second hub takes over as today.

**Rebalance.** When the community changes k-of-n, objects with the old
encoding are listed by the hub from their descriptors. Re-encoding produces a
new descriptor that only the owner can sign, so rebalance runs on the owner's
devices: fetch k shares, decode, re-encode, upload, re-publish the directory
objects that reference the object. The status page shows the conversion as a
progress bar, as it did for the 1-of-2 to 2-of-4 change.

**Deletion.** There are no leases and no expiry. An object lives while a root
of an active member references it. The owner's cleanup job computes the
reachable set over all of the member's roots and deletes the rest with a signed
token `sign(member, storage index, "delete", expiry)` that every server
honours, except for objects younger than 7 days. Servers delete all shares and
roots of a member whose grace period has ended, from the manifest alone, and
run an orphan sweep for owners no longer listed. A corrupted or lost local
database cannot cause a wrong deletion, because the reachable set is computed
from the grid, not from the local cache.

**What the status page can now say.** Per member: usage as servers see it, last
backup, last verified restore, objects under-placed and whether repair is
running. Per server: shares, bytes, free space, last integrity pass, relay
traffic. Overall: the same verdict logic as today, with one fewer excuse for a
stale number.

### Hub

`redundanet hub` is the same binary on a Linux server, listed in the manifest
by its device key. It is useful and untrusted at the same time: it cannot read
data, admit a member or impersonate a device, and the design is checked against
the case where it lies.

| Role | What it does | What it sees |
| --- | --- | --- |
| Rendezvous | Holds each device's address candidates and hands them to peers that want to connect | Device keys, IP addresses, liveness |
| Relay | Forwards QUIC packets between two authenticated devices when no direct path exists; meters bytes per device | Ciphertext and volumes |
| Directory | Mirrors the manifest at its pinned commit, the revocation list and the release index for agents that cannot reach GitHub; fetches them from the repository itself | Public data |
| Status collector | Receives census streams from storage nodes and liveness from the relay; keeps the placement table; renders the page and `status.json` | Storage indices, owners, sizes, versions |
| Join endpoint | Receives a signed join request from the app, checks that the named inviter is an active member, rate-limits, and opens the pull request with a bot token | Names, member keys, contributions |

**Two hubs.** A network lists as many hubs as it likes; devices connect to all
of them and prefer the nearest for relay. The status collector runs on each, so
the page is served by whichever is up. The relay needs a public UDP port, which
a proxied platform may not offer, so a plain VPS is the reference deployment
and a platform like fly.io serves the status page only, if at all. This
retires the "a proxy cannot be a rendezvous" rule by making the rule explicit
in the manifest.

**Phase 1 extra.** The hub also supervises the Tahoe introducer as a child
process, reachable through the loopback bridge; it is removed with the adapter.

**State.** SQLite for the placement table, history and roll-ups, as today; the
bot token and the hub's device key are the only secrets. Nothing on the hub is
needed to recover any member's data.

### Local API, CLI and UI

The agent serves an HTTP API on `127.0.0.1` with a bearer token kept in the
data directory; the CLI and the web page are its only clients, and both are
thin. Binding to the LAN is an opt-in for a headless Pi managed from a laptop.

**API surface.** `/status` (peers, roles, versions, disk, last errors),
`/backups` (sets, schedule, queue, runs), `/restore` (roots, snapshots, browse,
start, progress), `/settings` (bandwidth and schedule, retention, storage
directory, roles, hubs), `/devices` (list, certify, revoke), `/join` (build and
submit the request, poll approval), `/update` (available, apply, history),
`/logs` (ring buffer), `/diagnostics` (the bundle, with no secrets).

**Web page.** Server-rendered HTML with little script, in the visual language
of the status page, usable from a phone on the LAN. First-run wizard: name,
phrase, roles, folder, join. Daily use: a backup card per set with progress and
last run, a restore browser, a "last verified restore" date, and the peers
table. It is the product's interface until a tray application earns its place.

**CLI.** `redundanet` with `agent` (runs the daemon), `service` (install,
uninstall, status), `hub`, and client commands that only call the API:
`status`, `backup add|run|list`, `restore`, `join`, `device add|revoke`,
`update`, `diagnostics`, `open` (prints a one-time link that logs the browser
into the local page). The command structure of RedundaNet 2 is dropped with its
compose plumbing; the network and storage commands that members used map one
to one.

### Service integration, packaging and self-update

**Packaging.** One static binary per target, built in CI from one commit:
`windows/amd64`, `darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`.
Release assets are an archive per target, a checksum file, and an Ed25519
signature over the checksums made with a release key whose public half is
embedded in the binary and can be rotated through the manifest. Code signing
for the operating systems is a separate decision with a cost: Apple
notarization needs a developer account, Windows needs an Authenticode
certificate; without them users click through a warning once. The
recommendation is to pay for both before inviting non-technical members,
because the warning is the first thing they see.

In phase 1 the Tahoe child process ships as a second asset per target, built
with PyInstaller on the matching CI runner from the pinned lock file,
downloaded by the agent on first start and verified the same way.

**Service.** Installed and run as the user, no administrator rights:

- macOS: a launchd user agent in `~/Library/LaunchAgents` with `RunAtLoad` and
  `KeepAlive`.
- Linux desktop: a `systemd --user` unit plus `loginctl enable-linger` so it
  runs without a login; the Raspberry Pi image installs a system unit under a
  dedicated user instead.
- Windows: a Task Scheduler task at logon with restart on failure; an always-on
  machine may install a Windows service, which is the one step that asks for
  administrator rights, and only there.

Data lives in the platform's user directories (XDG on Linux, Application
Support on macOS, `%LOCALAPPDATA%` on Windows); the storage directory is
wherever the member points it, with the on-disk marker that ties it to the
node.

**Self-update.** The agent checks the signed release index hourly (from the
hub's mirror or the repository). A release is applied when its version is above
the running one and not marked withdrawn, and when the manifest's
`min_version` or the member's channel asks for it. Sequence: download, verify
signature, write beside the current binary, atomic swap, restart, health check
within two minutes (local API up, storage index opened, at least one hub and
one peer reached, census answered). A failed check swaps the previous binary
back and restarts; the outcome travels in the next census heartbeat, so the
status page shows the version of every node and any rollback. The previous
binary is always kept. The hub updates itself the same way, one hub at a time.

### Security model and threat analysis

The model is the one RedundaNet 2 already lives by, with two upgrades: servers
can verify ownership, and the transport authenticates devices instead of
hosts. Anonymity is not a goal and the document says so: a network observer
learns who talks to whom and how much.

| Adversary | Can | Cannot | Why |
| --- | --- | --- | --- |
| A storage host, curious or malicious | See ciphertext, sizes, owners and timing; delete or corrupt the shares it holds | Read a byte of content, alter a share undetected, forge a share's owner, inflate another member's usage | Per-object keys never leave the owner's devices; hash trees are verified on read; the descriptor is signed by the owner |
| A malicious member | Fill their own quota with garbage, hammer servers with requests, open bogus join requests naming themselves as inviter | Read, delete or overwrite anyone else's objects; exceed their quota; store under another name | Capabilities are the only access; deletes carry the owner's signature; the per-device rate limit and the social process handle abuse |
| A compromised hub | See the metadata the collector sees, drop or delay relayed traffic, publish a false status page, hand out wrong address candidates (connections then fail, they are not misdirected) | Admit a member, read or write a share, impersonate a device, delete anything | Admission is the manifest in Git; devices authenticate each other by key end to end; the hub holds no key but its own |
| A compromised repository account | Change the manifest: add a member or a node, change k-of-n, mark a member leaving | Read data; impersonate an existing device | Today's guard is branch protection, review and 2FA; phase 4 adds signatures by operator keys that every agent verifies, which closes this row |
| A lost or stolen device | Read everything of that member, which is what the member's own device is for; keep writing until revoked | Act after revocation; read other members' data | The member revokes the device certificate from any other device; a stolen phrase means a new identity, as a stolen GPG key does today |
| A compromised release pipeline | Ship a malicious binary | Have it installed | Releases are signed with an offline key whose public half agents already hold; the index can withdraw a version; CI builds from tags |
| The network path | See QUIC flows to hubs and peers, their volume and timing | Read or alter anything | TLS 1.3 between device keys; the relay forwards ciphertext |

**Primitives.** Ed25519 for every signature; TLS 1.3 for every connection with
the device key as the certificate; AES-256-GCM per segment under a per-object
key; BLAKE3 for hashing and keyed derivation; HKDF for deriving keys from the
seed; BIP39 words for the phrase. All from standard or widely audited
libraries; nothing custom. Key sizes and the format version are in every
header so they can change.

**What is deliberately weaker than it could be.** Servers see owner identities
and object sizes, because accounting and repair need them; a member's number
of objects and their sizes are visible to the servers that hold them and to the
hub. Hiding that would cost the quota enforcement the community asked for.

### Migration plan: transport first, then the engine

No flag day and no moved share file. Each step is reversible on its own, and at
every step the live network keeps running on what it ran on the day before.

1. **Agent as a sidecar (start of phase 1).** The agent ships as a fourth
   container in the existing compose stack, joined to the tinc network
   namespace like the Tahoe containers. It brings the mesh and the loopback
   bridge. Each Tahoe server advertises two hints, the VPN address and its
   loopback port; the introducer does the same. Old clients still reach
   servers over tinc; nothing else changes. Rollback: remove the container.
2. **First native members.** A macOS or Windows member installs the binary and
   joins the same Tahoe grid through the bridge, because every server is now
   reachable over the mesh. This is the phase 1 exit test.
3. **Linux nodes go native.** One node at a time, the operator installs the
   binary as a service on the host, points it at the same storage directory
   and the same Tahoe node directory (the tub keys travel, so the server's
   identity and its announcements do not change), and removes the compose
   stack. The VPN hint disappears with tinc on the last node. Rollback per
   node: the compose stack is still on disk until the member deletes it.
4. **Native grid beside Tahoe (phase 2).** Agents start the native server in a
   second directory on the same disk. The backup job switches engines per
   device when the member chooses; old Tahoe snapshots stay readable through
   the adapter. The status page shows both grids. Members re-upload from their
   sources at their own pace, within their quota, which they can meet because
   the Tahoe shares still count only against the old grid.
5. **Retire Tahoe (phase 3).** A member whose native backups have passed a
   restore test stops their Tahoe client. When every member has, operators
   stop the Tahoe servers, the share directories are deleted and the space
   returns to the native grid. The next major release removes the adapter,
   the bridge, the crawler patch and the lock-file pins.

Two things are never done: converting Tahoe share files into native shares (a
bit-exact reimplementation of Tahoe's formats is the one piece of work that
could lose data), and running the two grids on the same storage accounting
(they are separate until the old one is gone).

### Language and library choices

Recommendation: Go, with the transport behind an interface so the one place
where Rust is tempting (iroh) can be revisited if the spike says so. The
reasons are about a solo maintainer shipping on three platforms, not about the
languages themselves.

| Concern | Go | Rust |
| --- | --- | --- |
| Cross-compilation to five targets | A flag per target, one CI job | Native runners or a cross toolchain per target |
| Transport | quic-go for QUIC, pion for STUN and hole punching, a thin relay to write (a few hundred lines); libp2p as the heavy alternative | iroh gives QUIC, relay and hole punching in one crate, with self-hosted relays |
| Erasure coding | klauspost/reedsolomon, SIMD on x64 and arm64, the fastest implementation in common use | reed-solomon-simd, comparable |
| Cryptography | Standard library for AES-GCM, Ed25519, TLS 1.3; x/crypto for HKDF; zeebo/blake3 | RustCrypto crates and blake3; equally standard |
| SQLite | modernc.org/sqlite, pure Go, no cgo, keeps the binary static | rusqlite with the bundled engine |
| Static binary size | 15 to 25 MB | 5 to 10 MB |
| Concurrency mistakes in storage code | Caught by tests and the race detector | Caught by the compiler |
| Learning cost for the maintainer | Weeks | Months |
| Prior art to borrow from | Syncthing (relays, discovery, device IDs, self-update), restic (chunking, repository index), rclone (backends, bandwidth limits) | iroh itself; fewer end-user tools of this shape |

What does not carry over from Python in any case: the code. The monitor's
logic and its tests are the best guide for the collector; the census delta
protocol and the manifest schema are ported as protocols, not as code. The
8,000 lines of unit tests become the specification of what must still be true.

### Spike plan and milestones

**The spike (two weeks).** A prototype of about five hundred lines in Go: a
QUIC endpoint identified by a device key, a relay and rendezvous process on the
Hetzner hub, hole punching with pion's STUN, and the loopback bridge. Two
laptops run unmodified Tahoe nodes through it, one behind a home router and one
on a phone hotspot (the symmetric NAT case), with the Raspberry Pi as a third
node. Measured against tinc on the same machines:

- Time to first connection, and the share of pairs that reach a direct path.
- Throughput direct and relayed, against tinc with compression as deployed
  today.
- CPU on the Pi 4 at 20 Mbit/s, and the klauspost Reed-Solomon rate on the Pi 4
  and on the laptop.
- Reconnection after a Wi-Fi change and after the relay restarts, with a
  transfer in flight.

Exit criteria: a direct path for the NAT types the live members have, relayed
throughput at least equal to tinc, Pi CPU under 20% at 20 Mbit/s, and a
one-page decision with those numbers. If hole punching falls short, the
fallback is iroh for the transport, which changes the language decision and is
why the spike comes first.

**Milestones and gates.** Dates assume a start in November 2026 at spare-time
pace; they are the plan, not a promise, and each gate is a test the live
network passes.

![The binary ships in February 2027; Tahoe is gone by October 2027](images/0001-roadmap.png)

The accent marks G1, the first date members on other platforms can install;
everything after it improves the network without blocking anyone.

| Gate | Passes when |
| --- | --- |
| G0 | The spike's decision page exists with the numbers above |
| G1 | A macOS or Windows member is in the live network and tinc is gone from every node |
| G2 | The maintainer's own backups have run on the native engine for a month with weekly restore tests passing |
| G3 | No Tahoe process runs on the network and the adapter is deleted from the code |

### Alternatives considered

| Alternative | Why not | What is borrowed |
| --- | --- | --- |
| Keep Python, package with PyInstaller | Gives a binary but not a small or clean one; antivirus friction on Windows; and Python has no userspace NAT-traversal library, so a Go or Rust component is needed anyway | Nothing; the Tahoe child process in phase 1 is built this way because Tahoe is Python |
| Keep Tahoe forever behind the agent (stop after phase 1) | No ownership, so quotas stay on trust; no repair without the owner; a dormant upstream carried indefinitely | It is the fallback if phase 2 stalls, and the network would still be cross-platform |
| Tailscale with a self-hosted Headscale as the mesh | Mature NAT traversal, but the control server decides who is in the network, which contradicts the no-central-authority principle | The design of rendezvous plus relay with direct-path upgrade |
| Syncthing-style full replication | Every copy costs 1x; four copies cost 4x against 2x for 2-of-4 | Relays, discovery, device identities, self-update, the local web UI |
| restic or similar over the mesh | Excellent encryption, dedup and snapshots, but a repository lives on one backend; cross-peer redundancy would be whole copies again, and there is no ownership accounting | The chunking and repository-index ideas behind the backup job |
| libp2p | A complete transport, but heavy for one protocol, and it brings nothing for storage | Candidate transport if quic-go plus pion disappoints |
| Garage, SeaweedFS, MinIO | Object stores for trusted clusters; no client-side encryption, replication rather than erasure coding across homes | Garage's experience with geo-distributed home nodes is worth reading |
| Tahoe's newer HTTP storage protocol | Replaces Foolscap but keeps the introducer, leases and mutable files; the parts that cost the most stay | Its CBOR message design |
