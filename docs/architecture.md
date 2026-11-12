# Architecture

RedundaNet 3 as it runs today. The design is [RFC 0001](../rfcs/0001-redundanet-3.md); this page describes what the code does. The RedundaNet 2 architecture, still live on the old grid, is in [legacy/architecture.md](legacy/architecture.md).

## One binary, three modes

`redundanet` is one static Go binary with no cgo. It runs as the **agent** on every member device (client role, storage role, or both), as the **hub** on a server, and as the **CLI**, which talks to the local agent's API. Everything a device does happens inside the one agent process: there are no containers and no external daemons. On a RedundaNet 2 node the agent runs on the host beside the Docker stack, on the host's network, and never talks to Tahoe.

## The pieces

**Agent.** Holds the device's identity, keeps the manifest fresh, runs the mesh endpoint, serves the local API and page on `127.0.0.1:4470`, runs the jobs (backup daily, restore test weekly, cleanup weekly, rebalance while a transition is declared, restores in the background), sends each hub a census heartbeat every minute, and updates itself from the signed release index with probation and rollback. On a storage device it also opens the share store in the `redundanet3` directory of its storage path, serves the storage protocol on the mesh, scrubs its shares at a rate that covers them monthly, sends each hub its share census as deltas, and volunteers as a repair worker.

**Storage servers.** Members' devices with the storage role, listed under `storage:` in the manifest. They hold shares of other members' objects, each with a signed descriptor, and apply the RFC's checks before accepting a write: the device certificate chains to a listed, active member; the owner's budget with headroom; the server's own free-space floor; no write across a tombstone. They apply delete tokens past a 7-day immunity, purge tombstoned shares after 30 days and keep the record, and purge a leaving member's shares after the grace period.

**Hub.** The same binary on a Linux server, listed under `hubs:` by its device key, useful and untrusted at once. It reflects addresses so devices can hole punch, relays QUIC packets as ciphertext when no direct path exists (with a TLS carrier for UDP-blocked devices), collects the census heartbeats and the share census into a placement table (SQLite), schedules repair tasks for under-placed objects to volunteering storage nodes, and serves the status page and `status.json`. What it has for a device waits in a bounded queue with one writer per device and ten seconds per write, so a device that stops reading loses its channel and delays no other device. It holds no key but its own and no share. One hub runs today, `hub-hetzner`.

**Manifest.** `manifests/manifest.yaml` and `manifest.sig` in the GitHub repository: operators, hubs, members, storage devices and the network's parameters, signed offline by the operator key. Agents fetch the pair every five minutes, verify it against the operator keys pinned at join, apply a newer revision at once, and keep the last good copy for offline starts. Keys and rules: [configuration.md](configuration.md).

**Release index.** `releases/index.yaml` and `index.sig`: every release, its channel, per-target assets with hashes and sizes, withdrawn flags and the release key's rotation chain, signed offline by the release key whose public half is embedded in every build. Agents install only what it names.

## A backup, end to end

Scan the folders, honouring excludes. For each changed file: hash it, ask the local database and then the servers whether the member already stores that content (a server's yes counts only with the descriptor the member's device signed, and for no more shares than placement gives one server), and if not split it into segments, encrypt each with a per-object key derived from the member secret and the content, erasure code the ciphertext into n shares, and send each share to a distinct server over a mesh stream signed by the device, placing across members. Write the directory objects bottom-up and the snapshot index as objects of the same kind. Publish a signed root that names the new snapshot, sealed under the member's root key and confirmed by a majority of servers. Reading is the reverse from any k shares, verified against the signed share roots. Servers never see a key, a name or a plaintext byte.

## How devices connect

The agent reaches the hubs outbound (QUIC, or TLS over TCP when UDP is blocked). To reach a peer it asks a hub for the peer's address candidates; the device the hub names dials through the relay at once, both punch the candidates, and the dialer opens a direct connection on the first address that answers, ranked LAN, global IPv6, public IPv4. An address counts as answering only when its packet echoes the random number of a punch this device sent, so a packet from somebody else confirms nothing; a session remembers at most 32 such addresses and forgets one whose direct dial fails. A candidate is a literal address and port: the hub keeps at most 32 of the ones a device announces, a device punches at most 33 for a peer, and neither looks a name up. New streams take the direct connection while it lives and the relay otherwise. No inbound firewall rule is needed; a storage node may forward a UDP port (`udp_port` in the manifest) and the agent can ask the router for a mapping (`--portmap`). A device never dials itself: its own server is reached in process.

## Health and repair

The hub's placement table knows, per object, the distinct shares held on servers that are not dead (a server is down after ten minutes without a heartbeat and still counts; dead after `repair_after_hours`, 72 by default, and counts no more). Every minute the scheduling hub turns each object due into a task signed by its device key: the missing share numbers, where each goes, the holders to fetch from, an expiry. A volunteering storage node fetches the descriptor and k shares, regenerates the missing numbers without the key, checks them against the signed share roots and stores them; the receiving server checks the task before accepting a share from a device that is not the owner's. When no live server has room above its floor, the page says so. The page and `status.json` carry counts only: never an address, a path or a storage index.

## The package map

The Go module is `agent/`; the table in [../agent/README.md](../agent/README.md) is the authoritative map. In short:

| Package | What it does |
| --- | --- |
| `internal/identity` | Seed and phrase, member and device keys, certificates and revocations, the identity file, the TLS wire identity |
| `internal/manifest` | The v3 manifest: parser, validation rules, allocations, operator key and `manifest.sig`, the connection policy |
| `internal/membership` | The device's live view: the descriptor, the fetch loop, verification, the last good copy, expiry, persisted revocations |
| `internal/release`, `internal/update` | The signed release index and the two-stage self-update with probation and rollback |
| `internal/service` | The per-user service: launchd, a systemd user unit, a Windows logon task |
| `internal/api`, `internal/logbuf` | The local HTTP API and page, with the browser guards; the last 500 log records |
| `internal/wire`, `internal/transport`, `internal/hub`, `internal/portmap` | The hub channel protocol, the mesh endpoint (QUIC, relay, hole punching), hub mode, port mapping |
| `internal/collector` | The hub's status collector: census, placement table, repair scheduler, status page and `status.json`. The page opens with one line (everything stored is fully placed; repairing or waiting for a device; some objects need attention), then devices, storage, members and objects; it is one self-contained document with the look of the agent's page, no script, in English |
| `internal/object`, `internal/store`, `internal/shareproto` | The object format (encryption, erasure coding, descriptors, capabilities), the share store, the storage protocol |
| `internal/engine` | The client engine over the storage protocol: put, get, has, check, repair, roots, delete tokens |
| `internal/snapshot`, `internal/backup`, `internal/restore`, `internal/jobs` | Snapshots and retention, the backup job, restores and the restore test, the scheduler |
| `internal/cleanup`, `internal/rebalance`, `internal/repair` | Deletion of unreferenced objects, k-of-n conversion, repair without the owner |
| `internal/localdb`, `internal/diskfree`, `internal/diag`, `internal/semver`, `internal/version` | The local database, free space, the diagnostics bundle, version ordering, the build's version |
| `cmd/redundanet` | The binary: every command in [cli.md](cli.md) |

## Encodings, fixed

Keys and device ids are 32 bytes as 52 lowercase base32 characters. Signed records are deterministic CBOR signed by Ed25519 under one domain per record kind. TLS is 1.3 only, with a self-signed Ed25519 leaf carrying the device certificate under the project's OID arc 1.3.6.1.4.1.67508.1. The full list is in [../agent/README.md](../agent/README.md), "Encodings, fixed here".
