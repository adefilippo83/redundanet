# RedundaNet 3 agent

The one binary of [RFC 0001](../rfcs/0001-redundanet-3.md): agent, hub and
CLI. This directory is its Go module, `github.com/adefilippo83/redundanet/agent`,
beside the RedundaNet 2 Python code that keeps running the live network until
the migration ends. The spike that preceded it is under `../spike/` and is
reference material, not this code's ancestor.

The binary, package by package:

| Package | What it does | Status |
| --- | --- | --- |
| `internal/identity` | Seed and twelve-word phrase, member signing key and member secret, device keys, device certificates and revocations, the identity file at rest, and the TLS wire identity with its one verification function | Done, with tests |
| `internal/manifest` | The v3 manifest: strict parser that sets aside the RedundaNet 2 keys of a transitional file, the validation rules of the RFC's Membership section, allocations, the operator key and `manifest.sig`, invite and confirmation signatures behind `vouched_by`, and the connection policy (`Allow`, `MayWrite`) from the manifest and the revocation list | Done, with tests |
| `internal/membership` | The device's live view of the network: the network descriptor (the invite link's payload and the trust anchor), the fetch loop for `manifest.yaml` and `manifest.sig` from the repository, verification against the pinned operators, the last good copy on disk for offline starts, operator rotation only through a manifest the current keys signed, expiry (known devices stay, new ones wait for a re-sign), persisted revocations, and one `Allow` the transport keeps for good | Done, with tests |
| `internal/release` | The signed release index: versions, channels, withdrawn flags, per-target assets with hashes, and the release key's rotation chain; download with hash and size checks and extraction of the executable from tar.gz or zip | Done, with tests |
| `internal/update` | The two-stage self-update: fetch and verify the index, pick a release (channel, `min_version`, failed ones never retried), download, run the candidate on probation, rename it into place keeping the old binary, ask to be restarted | Done, with tests |
| `internal/service` | The per-user service: a launchd agent on macOS, a systemd user unit with lingering on Linux, a logon task with restart on failure on Windows | Done, definitions tested; installs tried by hand |
| `internal/api` | The local HTTP API and page on 127.0.0.1: bearer token in `api.token` for the CLI, a session cookie (HttpOnly, SameSite=Strict) that a one-time link from `redundanet open` starts for the browser, a CSRF token on every change, Host and Origin checks on every request, no GET that changes anything; update check, apply and retry as actions | Done, with tests |
| `internal/logbuf` | The last 500 log records in memory, for the page and `redundanet logs` | Done, with tests |
| `internal/wire` | The hub channel protocol: typed frames with CBOR control messages, relay payloads, the census heartbeat, the hub-signed repair task with the worker's report and the surplus release hint, the synthetic relay address, and the stream interface the protocols speak with its in-process loopback pair | Done, with tests |
| `internal/transport` | The mesh endpoint: one UDP socket owned by quic-go, the hub channel (QUIC, or TLS over TCP when UDP is blocked), relayed connections over hub datagrams, hole punching through quic-go's non-QUIC packet hook, port mapping, and a session per peer with a relay and a direct connection, new streams on the best live one; no session with oneself, that path is refused | Done, with end-to-end tests |
| `internal/hub` | Hub mode's rendezvous and relay: address reflection, sticky dialer roles, datagram relay with bounded dropping queues, a TLS carrier for UDP-blocked devices, the census hook, status | Done, tested through the transport |
| `internal/collector` | The hub's status collector: keeps each device's census heartbeat (version, uptime, platform, roles, manifest revision, free space, peers), the placement table in SQLite from the share census with the RFC's copies, dead-server, quiescence and surplus rules, the repair scheduler (phase 2, M5: signed tasks to volunteering nodes, the one-scheduler rule between hubs, expiry and retry, blocked when the live servers lack room above their floors, surplus hints), joins them with the manifest and the live channels, mirrors the RedundaNet 2 status page so today's nodes stay visible beside the new devices, and serves the status page and `status.json`. Publishes names, roles, versions, liveness, free space, relay traffic and placement and repair counts; never an address, a path or a storage index | Done, with tests |
| `internal/diskfree` | Free and total space of the filesystem that holds a path, for the census | Done, with tests |
| `internal/portmap` | PCP, NAT-PMP and UPnP port mapping, from the spike | Done |
| `internal/object` | The native engine's format (RFC 0001, phase 2, M1): segments, the convergent key and storage index, AES-256-GCM per segment, Reed-Solomon shares, BLAKE3 share roots, the signed deterministic descriptor, read and verify capabilities, the two-pass encoder, the verifying decoder and the share stream layout; pure, pinned by known-answer tests | Done, with tests |
| `internal/store` | A storage node's share store (phase 2, M2): share files in the object format, the SQLite index, partial and complete states with resume, usage per owner, the free-space floor, root records, the census delta, the scrub, the marker that ties the store to its disk, and the server's half of deletion (M5): delete tokens applied past the 7-day immunity on the server's own clock, tombstones purged after the 30-day window with the record kept, undelete within it, the per-owner listing, and membership-driven deletion after the grace period | Done, with tests |
| `internal/repair` | The node side of repair without the owner (phase 2, M5): a worker that takes the hub's signed task, fetches the descriptor and k shares from the holders (its own store for shares it holds), regenerates the missing numbers without the key, checks them against the signed share roots and stores them where the task says (its own store when the task names it), then reports; and the surplus release, given up only after confirming with the other holders directly that every share number stays complete | Done, with tests |
| `internal/shareproto` | The storage protocol between devices: one stream per request, CBOR messages, blocks streamed; the server applies the RFC's checks in order, the client resumes a share from where the server stopped; roots travel as signed envelopes; a device reaches its own server over the local opener, in process, so a storage node never dials itself | Done, with tests through a real hub |
| `internal/engine` | The client engine (phase 2, M3): the RFC's `Engine` interface, its native implementation over the storage protocol (spool and dedup, one encoding pass feeding a stream per share, placement across members with the small-network doubling and a retry on a refusal, verified reads and ranges, check and the owner's repair, roots sealed under the member's root key and confirmed by a majority of servers, delete tokens to every server, the per-member listing, reads of a tombstoned object within the window, and a cache whose word is probed on the servers once it is six days old) and a fake with the same contract for the jobs' tests | Done, with tests |
| `internal/rebalance` | The owner's side of a k-of-n change (phase 2, M5): with a transition in the manifest, each device walks its snapshots, reads every object still under the old encoding, stores it again under the new one, rewrites the directories up to the top and the index, updates the scan rows before anything is published, and publishes the root after each snapshot; a stopped run resumes where it was | Done, with tests |
| `internal/diag` | The diagnostics bundle (phase 2, M6): one zip the member sends by hand, built from the running agent when it answers and from the data directory and the service log when it does not; an allowlist of versions, the manifest revision, the service state, hubs and peers as counts, the backup sets with folders as label plus hash, restore tests, cleanup, error kinds and counts, a disk check, and the last log lines with every path, URL and address redacted | Done, with tests |
| `internal/cleanup` | The owner's cleanup job (phase 2, M5): the reachable set over every root of the member, walked on the grid, against what the servers hold for the member; a delete token for each object nothing references; refused without the root quorum, when any root or directory cannot be read, and when a run would delete more than half of what the grid holds unless forced; roots of devices revoked longer ago than the window no longer count | Done, with tests |
| `internal/localdb` | The device's local database: objects by plaintext hash and encoding, placement, the root version counter; a cache, rebuilt when lost | Done, with tests |
| `internal/snapshot` | The snapshot format (phase 2, M4): directory objects with versioned entries (file, directory, symlink, skipped; inline bytes under 256 B; chunk capabilities above 64 MiB), the index object listing a device's snapshots, and retention | Done, with tests |
| `internal/backup` | The backup job against the engine interface: scan with the per-platform change key and the rolling re-hash, default excludes, per-file failures counted and listed, dedup through the local database and the engine, chunking, directory and index objects bottom-up, the root published even after an interruption as a partial snapshot; the change-detection key with the file id and change time on every platform (a handle per file on Windows), cloud placeholders skipped and counted (iCloud's evicted files on macOS, OneDrive's online-only files on Windows), the privacy probe that reports a folder the service cannot read with the setting to change before a run, and default excludes for macOS, Windows and Linux | Done, with tests on the fake engine |
| `internal/restore` | List a member's device roots and indexes, browse a snapshot without fetching what is not asked for, restore with temporary names and renames, mtime and mode after the data, name mapping and a report that is complete with exceptions, and the unattended restore test | Done, with tests |
| `internal/jobs` | The agent's job runner: `backup.yaml` with the sets, the schedule (daily runs, monthly restore test), one run at a time, restore in the background, and the status the page and the census show | Done, with tests |
| `internal/semver` | Version strings: MAJOR.MINOR.PATCH with an optional pre-release suffix such as `-alpha.1`, ordered as Semantic Versioning orders them, shared by the release index, the manifest's `min_version` and the running-version check | Done, with tests |
| `internal/version` | The build's version string | Done |
| `cmd/redundanet` | The binary. Today: `identity`, `device` (revoke), `operator`, `manifest`, `validate`, `network`, `agent`, `hub`, `service`, `update` (check, apply, status, retry), `release`, `object` (put, get, has, check, repair, delete), `storage` (has, drop), `backup` (add, remove, list, run, verify, cleanup, rebalance), `restore` (list, browse, start, status), `status`, `open`, `logs`, `diagnostics`, `version` | Growing |

Not built: the hub's manifest mirror and join endpoint (the issue form on
the website is the join path), bandwidth caps, a settings page, device
pairing. No container was ever shipped; the agent is a host service.

## Running it today

```
redundanet network descriptor --manifest manifests/manifest.yaml --repository https://raw.githubusercontent.com/OWNER/REPO/main
    An operator prints the network descriptor: name, hubs, operator keys,
    where the signed manifest is served. It goes into invite links.

redundanet device revoke --device KEY [--serial N]
    Any device of the member, with the phrase: signs a revocation record
    and hands it to the running agent, which applies it here and sends it
    to every storage server and hub within seconds; the record is printed
    too, for `revocations:` in the manifest, which carries it to whoever
    was not reachable. Serial 0, the default, covers every certificate of
    the device, the case of a lost one.

redundanet network join [descriptor]
    Joins the network built into the binary (version.Network) or the one a
    descriptor names: pins its operator keys, fetches the manifest and its
    signature, verifies them and keeps the pair on disk. Nobody runs it for
    the built-in network: a device that never joined by hand is seeded from
    the built-in descriptor when the agent, `network status` or pairing
    first needs it, and follows that descriptor across releases; a
    descriptor given here is pinned. Every --network flag defaults to the
    built-in network's name.

redundanet network status [--refresh]

redundanet hub   [--network NAME] [--udp :4433] [--tcp :4443] [--status 127.0.0.1:4480] [--legacy-status URL]
                 [--publish URL --publish-token-file FILE] [--channel stable|early] [--no-update]
redundanet agent [--network NAME] [--connect DEVICEKEY,...] [--portmap] [--tcp-only] [--storage-path DIR] [--storage-floor-gb N]
    Without an identity it serves the local page alone, with the setup
    wizard, and continues as the agent once the page or the CLI made one.
redundanet object put|get|has|check|repair|delete [--network NAME] FILE|CAP
redundanet storage has|drop [--storage-path DIR] STORAGE_INDEX [SHARE_NUMBER]
redundanet backup add NAME FOLDER... | remove NAME | list | run [NAME] | verify | cleanup [--dry-run] [--force] | rebalance
redundanet backup limit [--upload KBPS] [--download KBPS]   # bandwidth caps on this device's mesh traffic, 0 for none
redundanet backup schedule [--every 24h] [--verify-every 168h] [--cleanup-every 168h]
redundanet backup retention [--days N] [--daily N] [--weekly N] [--monthly N]
redundanet backup exclude NAME --exclude a,b                # replaces the set's own excludes
redundanet update channel stable|early                       # chosen on this device, over the service's flag
redundanet diagnostics [--out FILE]
redundanet restore list | browse DEVICE SNAPSHOT [PATH] | start DEVICE SNAPSHOT [PATH] --to DIR | status
```

Both long-lived commands need this device's identity file and a joined
network; they refresh the manifest every five minutes, apply a newer
signed revision on the spot, and close connections to devices the new
policy no longer admits. Every agent sends each hub a census heartbeat when
the channel comes up and every minute after: version, uptime, platform,
roles, the manifest revision it applied, free space on `--storage-path` for
a storage device, and which peers it reaches directly or through a relay.
A storage device also opens its store in the `redundanet3` directory of
`--storage-path`, serves the storage protocol on the mesh, scrubs its shares
at a rate that covers them monthly, and sends each hub its share census as
deltas since the generation that hub last received, with the date its
oldest integrity check dates from and its free-space floor.
Pairing (RFC 0001, "Adding a device") is how a member adds a laptop without
typing the phrase on it: `identity pair` on the new device shows a code,
`rn3:pair:` and base32 of the device's addresses and a 160-bit secret, and
listens on a TCP port; `device pair CODE` on a device that has the
identity, after the phrase, connects to one of those addresses, and the
two exchange four messages sealed with AES-GCM under a key HKDF derives
from the secret: open, the new device's key, the offer (the identity as
`identity certify` makes it, plus the network descriptor), done. Only the
holders of the code can take part, a wrong code opens nothing, three
failed connections end the code, and it is good for ten minutes and one
device. The two reach each other directly on the same network or, failing
that, through a hub: the code names the hubs' status pages, each hub keeps
a mailbox per pairing under `/pair/` (a room derived from the code's secret,
a queue per direction, ten minutes), and the same sealed frames travel
through it, so the hub learns only that a pairing happened and from which
addresses. The new device then holds its identity and the network, and
installs its service. The headless flows (`install.sh`, the Pi image) stay
for machines without a second device of the member at all.

The settings live in `backup.yaml` of the data directory and change from
the local page's Settings card, from the commands above, or by hand: the
schedule (`every`, `verify_every`, `cleanup_every`; backups at most hourly,
the others at most daily, the scheduler re-arms at once), the retention
policy, the bandwidth caps and each set's own excludes. The update channel
is chosen with `update channel`, kept with the updater's state, and wins
over the service's `--channel` flag from the next check on.
Before every run the agent asks the storage servers what they hold for the
member (`usage`, share bytes under the current encoding) and sets that,
times k/n, against the manifest's allocation: the local page and `backup
list` show "allocation: X of Y GB used", and a run is refused once the
allocation is used up, with the way out in the message. The servers bound
the member with headroom anyway; this is the exact budget on the client
side (RFC "Quota").

The bandwidth caps of the RFC (F8) pace every mesh stream of the device,
opened or accepted, so a backup never takes more than its share of a home
uplink: `backup limit --upload 2000 --download 0` caps what the device
sends at 2000 kB/s and leaves what it receives free. The caps live in
`backup.yaml` as `upload_kbps` and `download_kbps`, apply at once and
survive restarts, and show in `backup list` and on the local page. A token
bucket grants a second's worth of burst after idle time, never more. The
in-process path to the device's own store is never paced.
The agent runs the backup sets in `backup.yaml` of its data directory
(daily by default, two minutes after start first, and on `backup run`),
the restore test weekly (a file saved by an earlier version with the
monthly default it then had moves to weekly on load), the cleanup
weekly, and restores in the background; `backup` and `restore` are thin
clients of its API. Every restore test is kept in the local database and
listed on the local page and by `backup list`, and the last one survives
a restart. The heartbeat carries the day of the last backup, the day of
the last passing restore test and how many weeks in a row had a passing
one, which the hub page shows per member: the month the G2 gate measures
reads as "4 weeks in a row" there.
On macOS a service reading Documents, Desktop or Downloads needs Full
Disk Access, and the agent says so: `backup add` probes each folder as
the service and refuses one the system guards, with the setting to
change, and the local page and `backup list` show the same line for a
folder that becomes unreadable later. A OneDrive online-only file on
Windows and an evicted iCloud file on macOS are listed as skipped, "not
on this computer", never pulled down; the local page and `backup list`
count the skipped files by reason, and `restore browse` shows each one's. The change-detection key carries
the file id and change time on Windows too, through a handle per file.
When something is wrong, `redundanet diagnostics` writes a bundle to send
by hand, and the local page has a button for it. It works with the agent
down, from the data directory and the service log, and holds only what
the RFC allows: versions, the manifest revision, the service state, hubs
and peers as counts, the backup sets with each folder as the set's label
plus a hash, the restore tests, the cleanup, warnings and errors by kind
and count, a disk check, and the last log lines with every path, URL and
address replaced. Nothing is uploaded.
Deletion follows the RFC's two phases and its one invariant. The cleanup
job (`backup cleanup`, weekly on its own) reads every root of the member
under the quorum rule, walks the indexes, snapshots and directories on
the grid to the reachable set, lists what every server holds for the
member, and sends a delete token signed by the device for each object
nothing references; it refuses to run when a root or a directory cannot
be read, and refuses to delete more than half of what the grid holds
unless forced. A server tombstones the object only when the owner has
not touched it for 7 days on the server's own clock and the token is
newer than the last touch; within the 30-day window the owner's devices
still read it, `undelete` brings it back, and a new upload clears it;
after the window the shares go and the record stays, so repair never
fills a deleted object in. Pruning a snapshot drops it from the index and
leaves the deletion of what it alone referenced to the next cleanup. Two
checks on the device keep a root from pointing at a deleted object: a
file that comes back after a run away is probed on the servers before its
cached capability is reused, and an object the cache has not confirmed
for six days is probed too; both touch the object on every server. A
server that finds a member leaving or unlisted notes the time and purges
their shares and roots only once the manifest's `grace_days` have run on
its own clock with the manifest still saying so; a member who reappears
clears the note.
A k-of-n change is declared in the manifest as `encoding_transition:
{from, to, deadline}` beside the new `encoding`; `validate` refuses a
transition whose `to` is not the encoding and warns once the deadline
has passed. Until the deadline every server accounts shares under the
old encoding at zero, so a member near budget converts without deleting
first; the hub page shows the conversion as a bar, converted of all
objects, with the old encoding's bytes and whether they count yet, and
per member the objects still to convert. Each device converts what its
own root references (`backup rebalance`, and on its own every six hours
while a transition is declared): fetch, decode, store under the new
encoding, rewrite the directories and the index, publish the root after
each snapshot. The old objects fall to the cleanup. Once every member
has converted, the operators remove the transition.
No address and no path travel in it. The hub's collector keeps the
heartbeats in `census.json` and the share census in `placement.sqlite`,
the grid-wide placement table of the RFC's "Health" section: per object
the distinct shares held on servers that are not dead (a server is down
after ten minutes without a heartbeat and still counts; dead after
`repair_after_hours`, 72 by default, and counts no more), the spread over
servers against min(n, servers not dead), surplus, tombstones, and
whether it needs repair at once (it had been fully placed) or after the
24-hour quiescence window (it never was). The page and `status.json`
carry the counts only: objects per number of copies, under-placed,
unreadable, due for repair, and per member its under-placed objects.
Every minute the hub that schedules repair (the first in manifest order
whose status page is fresh; the others take over when it has been gone
ten minutes) turns each object due into a task signed by its device key:
the missing share numbers, where each goes (a live server with room above
its floor that does not hold it, servers holding nothing of the object
first, at most ceil(n / servers not dead) per server, never a plan that
leaves the object on fewer than k servers), the holders to fetch from,
and an expiry. A storage node volunteers as a worker in its heartbeat;
the task goes to one that is not a target when there is one, at most two
tasks at a time each. The worker fetches the descriptor and k shares,
regenerates the missing numbers without the key, checks them against the
signed share roots and stores them; the receiving server checks the task
before it accepts a share from a device that is not the owner's, lets a
repair write pass the owner's cap but never its own floor, and never
crosses a tombstone. The task closes when the share census shows the
object placed, when the worker reports, or at its expiry, after which the
object waits ten minutes before another task. When no live server has
room for a share the page says "blocked: X short above the floors", which
tells the community to add disk. When a server returns from the dead and
an object has more complete shares than n, the hub hints at the holder
with the most shares; that holder asks the other holders itself and gives
its copy up only once every share number stays complete without it.
Tasks live in `placement.sqlite` with the table, so a hub restart neither
loses nor doubles them. `storage drop`, run on a node as the service user
beside the running agent, removes one share on purpose: the census
reports the loss within seconds and, since the object had been fully
placed, the hub issues a task at once. That is how repair is drilled on
a live network, and `object check CAP` from any device of the owner
prints the storage index and the holders to pick from.
The collector serves the status page and `status.json` on
`--status`, and with `--legacy-status` mirrors the RedundaNet 2 status page
so today's nodes stay visible beside the new devices until G3. With
`--publish` it also PUTs `status.json` and `index.html` under that URL every
minute with the bearer token from `--publish-token-file`, which is how the
fly.io page serves the new network under `/v3/` while the hub has no domain.
A hub entry in the manifest names its QUIC relay as `relay: host:port` and,
when its TLS carrier is not on port 443 of the same host, `tcp: host:port`.
`--manifest FILE` runs them from a signed file
instead, trusting the operators that file lists, for tests and for the hub
that serves the repository it is listed in. The agent serves the storage
protocol on the mesh and logs a status snapshot every minute.

```
redundanet status [--json]      # what the running agent sees, through its local API
redundanet open                 # the local page, through a one-time link that logs the browser in
redundanet logs [-n 100]

redundanet service install [--network NAME] [--channel early]  # this binary, as a user service that starts at login
redundanet node install --storage-path DIR [--network NAME] [--channel stable|early] [--allow-root-disk] [--no-pair]   # Linux, as root: a storage node laid out, paired and started
redundanet node status | uninstall
redundanet service status | uninstall

redundanet update check | apply | status [--channel early]
redundanet release keygen --key release.key                 # maintainer, once, offline
redundanet release sign --key release.key --index releases/index.yaml
```

The agent serves its API and page on `127.0.0.1:4470` (`--api`), never on
the LAN; a headless node is reached over an SSH tunnel. The CLI finds the
address and the bearer token in the data directory (`api.addr`,
`api.token`, mode 0600). A browser gets in only through `redundanet open`,
whose link works once and for two minutes and sets a session cookie; every
form on the page carries a CSRF token, the API refuses any request whose
Host is not loopback and any change whose Origin is not the page, and no
GET changes anything. These are the guards the RFC's Browser safety
section lists.

Self-update needs a release key embedded at build time (`-X
.../version.ReleaseKey=`); a plain build has none and says so. The running
agent checks the signed index hourly and on start. When a newer release
fits its channel, or the manifest's `min_version` requires one, it
downloads the asset, checks its hash and size, closes its own endpoint,
starts the candidate as `agent --probation` on the same identity, and waits
up to two minutes for the candidate to report that every hub answered and
every peer the agent had came back. Only then is the new binary renamed
into place, the old one kept beside it as `redundanet.old`, and the agent
exits so the service starts the new version. A candidate that fails is
recorded and never retried; the agent restarts its endpoint and carries on.
The release key rotates only through records in the index signed by the
key being retired, so a binary that embeds an old key still verifies a
newer index; nothing in the manifest can name a release.

What the membership rules do, in the words of the RFC: a manifest takes
effect only with a valid operator signature and a revision above the last
applied; the operators list changes only through a manifest the current
keys signed; a signature older than `signature_max_age_days` keeps the
node working for devices it has already admitted and refuses new ones
until an operator re-signs; a revocation is applied at once, persisted, and
survives every later manifest.

How a session comes up: the agent asks a hub for the peer's candidates; the
device the hub names dials through the relay at once, both punch the
candidates, and the dialer opens a second, direct connection on the first
address that answers, ranked LAN, global IPv6, public IPv4. New streams take
the direct connection while it lives and the relay otherwise; a connection
that dies takes its streams with it, and the request layer retries. There
is no QUIC path migration: quic-go cannot migrate off a dead path (see the
G0 record), and the RFC only asks for a resume within seconds.

## Build and test

```
cd agent
go test -race ./...
go build ./cmd/redundanet
```

`deploy/hub/` holds the systemd unit and the install runbook for the hub on
hub-hetzner; `deploy/node/` the unit, installer and runbook for a storage node.

Go 1.27 or later, no cgo. `.github/workflows/agent.yml` runs the tests on
Linux, macOS and Windows and cross-compiles every target; a tag `agent/vX.Y.Z`,
or `agent/vX.Y.Z-alpha.N` for a pre-release, builds a draft release with
checksums (`agent-release.yml`). The Windows binary carries a version
resource (product name and version in its properties) generated at release
time from `cmd/redundanet/versioninfo.json`. The workflow signs the Windows
binary through SignPath Foundation and signs and notarizes the macOS
binaries with a Developer ID as soon as the secrets its header names exist;
until then those jobs are skipped and the binaries ship unsigned.

## Identity commands

```
redundanet setup [--storage] [--channel early] [--print]
    The device you sit at, step by step and again any time: binary where
    the agent can update it, identity (new, from the phrase, or paired),
    the manifest and whether the member is listed, the service, the join
    form filled in while not listed, the page.
redundanet join-request [--print]
    The join form on the website, filled in from this device: member key
    and the storage node it paired, or, for a listed member, the form's
    "add a storage node" mode with the node's certificate as proof. Nothing
    is sent; the member reviews.

redundanet identity init [--storage] [--passphrase] [--from-phrase]
    New member on this device: shows the phrase, checks three words, writes
    the identity file (device key, member secret, certificate). With
    --from-phrase an existing member types the phrase instead, and the
    device gets a certificate of its own.

redundanet identity show [--certificate]
    --certificate prints this device's certificate as one line, the proof a
    listed member's storage device request carries.
redundanet identity verify-certificate [--network NAME] FILE|-
    Checks a certificate in that form and prints what it certifies as JSON;
    the join workflow runs it on a storage device request.
redundanet identity check
    Types the phrase and says whether it is the one this device was
    certified under; nothing is written. The check to run after writing
    the phrase down, and whenever in doubt.
redundanet identity pair [--wait 10m]
    The new device's side of pairing: makes its device key, shows a
    one-time code and waits. Nothing is typed here but the command.
redundanet device pair [--storage] CODE
    The existing device's side, with the phrase: reaches the new device
    through the code, certifies its key and hands over the identity and
    the network descriptor, sealed under the code. The phrase never moves.
    The new device (key, host name, roles) goes on record in devices.yaml,
    for the join request.
    Member key, its ten-group display code, device key, roles.

redundanet identity device-key            (on a headless node)
redundanet identity certify --device KEY [--storage] > file   (where the phrase is typed)
redundanet identity accept-certificate file   (back on the node)
    The headless flow of the RFC: the node makes its own key, the member
    certifies it on a laptop, the certificate travels back as a file. The
    phrase never reaches the node; the file carries the member secret and is
    deleted after acceptance.
```

The identity file lives in the platform's user config directory under
`redundanet/` (`--data-dir` or `REDUNDANET_DATA_DIR` override it). With
`--passphrase` it is sealed with XChaCha20-Poly1305 under an Argon2id key;
without, as on a headless node, the file mode is its protection, as the RFC
says. OS keystores come later and change only that file.

## Operator commands

```
redundanet operator init --key operator.key
    Makes the offline operator key and prints the public half for the
    manifest's operators list.

redundanet validate --manifest manifests/manifest.yaml
    Parses the v3 half of the manifest (v2 keys are set aside, anything else
    unknown is an error), applies the RFC's rules, prints errors, warnings
    and the allocation per member; exits 1 on errors.

redundanet manifest sign --key operator.key --manifest manifests/manifest.yaml
    After a merge: validates, then writes manifests/manifest.sig, the
    operator's signature over the network, the revision, the time and the
    file's hash. Nodes apply only a signed manifest.

redundanet manifest verify --manifest ... [--sig ...] [--trust KEY,...]
    Checks the signature against the file. Without --trust it uses the
    operators listed in the manifest itself, which proves consistency, not
    authenticity; agents pin the keys from the network descriptor they
    joined with.

sudo redundanet migrate-node [--dry-run] [--force] [--rollback]
    Phase 3, on a node that runs the agent beside the RedundaNet 2 stack:
    retires the stack. Refuses while the folder backup-sync pushed to the
    old grid is not held by a native set with a complete run and a passed
    restore test on this device, or while no hub is connected; --force
    overrides, and the plan names what is lost. The plan lists the stack's
    containers by service (asked of Docker itself, since compose does not
    see a service whose profile is off) and every profile the compose file
    defines. Then one `docker compose down` through the installed compose
    file, its override and .env, with every profile named, so the Tahoe
    services go too, every container at once and tinc last by its
    dependencies; a container that survives puts the stack back and fails
    the command, since it would run on tinc's dead network namespace.
    Then a marker in the data directory. The compose files, the .env and
    the Tahoe share directories stay on disk; the shares go on the
    network's stop date, not here. --rollback is `compose up -d
    --force-recreate` naming every service the marker recorded, tinc
    first by its dependencies, and removes the marker: the stack is never
    restarted one container at a time. The agent is untouched throughout.

A device revocation takes the direct path first, the RFC's "Losing a
device": the agent applies the member's signed record, delivers it to every
storage server over the storage protocol (`revoke`) and to every hub over
the channel; a hub keeps it, sends it to every connected device at once and
to each device that connects later, so a server that was offline learns it
on its next connection. The manifest carries it for good: the operator
appends the printed record under `revocations:` (member key, device key,
serial, revoked_at, signature), raises the revision and signs; `validate`
checks every record's signature against the member's key and warns when
one names a listed storage device.
```

## Encodings, fixed here

- Keys and device IDs: 32 bytes as 52 lowercase base32 characters.
- Signed records: deterministic CBOR (RFC 8949 core), signature by Ed25519
  over `"redundanet/3/<domain>\0" || record`, one domain per record kind.
- Protocol messages (channel frames, share messages): the same CBOR, but
  a receiver skips fields it does not know, so devices and hubs may run
  different versions; signed records stay strict.
- Key derivation: HKDF-SHA256 from the seed with the labels in
  `internal/identity/member.go`; the test `TestKnownAnswers` pins them.
- Display code: ten groups of four base32 characters of a hash of the key.
- `manifest.sig`: YAML with network, revision, issued_at, sha256 of the
  file, the operator key and a base64 Ed25519 signature over the
  deterministic CBOR of the first four, under the domain `manifest`.
- Invites and confirmations: deterministic CBOR records signed by the
  inviter's device under the domains `invite` and `invite-confirmation`; the
  manifest keeps the nonce, the expiry and both signatures in `vouched_by`.
- TLS: 1.3 only, ALPN `redundanet/1`, no resumption, a self-signed Ed25519
  leaf with the device certificate in a critical extension under the
  project's OID 1.3.6.1.4.1.67508.1.1 (IANA private enterprise number
  67508, arc .1 for RedundaNet 3, .1.1 for this extension). Since 3.0.0 only
  that arc is read; the provisional arc 2.25.1409274851 of the alphas is
  refused (`tls.go`).
