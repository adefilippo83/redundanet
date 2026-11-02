# RedundaNet 3 agent

The one binary of [RFC 0001](../rfcs/0001-redundanet-3.md): agent, hub and
CLI. This directory is its Go module, `github.com/adefilippo83/redundanet/agent`,
beside the RedundaNet 2 Python code that keeps running the live network until
the migration ends. The spike that preceded it is under `../spike/` and is
reference material, not this code's ancestor.

Phase 1, the control plane, is under construction. What exists:

| Package | What it does | Status |
| --- | --- | --- |
| `internal/identity` | Seed and twelve-word phrase, member signing key and member secret, device keys, device certificates and revocations, the identity file at rest, and the TLS wire identity with its one verification function | Done, with tests |
| `internal/manifest` | The v3 manifest: strict parser that sets aside the RedundaNet 2 keys of a transitional file, the validation rules of the RFC's Membership section, allocations, the operator key and `manifest.sig`, invite and confirmation signatures behind `vouched_by`, and the connection policy (`Allow`, `MayWrite`) from the manifest and the revocation list | Done, with tests |
| `internal/membership` | The device's live view of the network: the network descriptor (the invite link's payload and the trust anchor), the fetch loop for `manifest.yaml` and `manifest.sig` from the repository, verification against the pinned operators, the last good copy on disk for offline starts, operator rotation only through a manifest the current keys signed, expiry (known devices stay, new ones wait for a re-sign), persisted revocations, and one `Allow` the transport keeps for good | Done, with tests |
| `internal/release` | The signed release index: versions, channels, withdrawn flags, per-target assets with hashes, and the release key's rotation chain; download with hash and size checks and extraction of the executable from tar.gz or zip | Done, with tests |
| `internal/update` | The two-stage self-update: fetch and verify the index, pick a release (channel, `min_version`, failed ones never retried), download, run the candidate on probation, rename it into place keeping the old binary, ask to be restarted | Done, with tests |
| `internal/service` | The per-user service: a launchd agent on macOS, a systemd user unit with lingering on Linux, a logon task with restart on failure on Windows | Done, definitions tested; installs tried by hand |
| `internal/api` | The local HTTP API and page on 127.0.0.1: bearer token in `api.token` for the CLI, a session cookie (HttpOnly, SameSite=Strict) that a one-time link from `redundanet open` starts for the browser, a CSRF token on every change, Host and Origin checks on every request, no GET that changes anything | Done, with tests |
| `internal/logbuf` | The last 500 log records in memory, for the page and `redundanet logs` | Done, with tests |
| `internal/wire` | The hub channel protocol: typed frames with CBOR control messages, relay payloads, the synthetic relay address | Done |
| `internal/transport` | The mesh endpoint: one UDP socket owned by quic-go, the hub channel (QUIC, or TLS over TCP when UDP is blocked), relayed connections over hub datagrams, hole punching through quic-go's non-QUIC packet hook, port mapping, and a session per peer with a relay and a direct connection, new streams on the best live one | Done, with end-to-end tests |
| `internal/hub` | Hub mode's rendezvous and relay: address reflection, sticky dialer roles, datagram relay with bounded dropping queues, a TLS carrier for UDP-blocked devices, status | Done, tested through the transport |
| `internal/portmap` | PCP, NAT-PMP and UPnP port mapping, from the spike | Done |
| `internal/semver` | Version strings: MAJOR.MINOR.PATCH with an optional pre-release suffix such as `-alpha.1`, ordered as Semantic Versioning orders them, shared by the release index, the manifest's `min_version` and the running-version check | Done, with tests |
| `internal/version` | The build's version string | Done |
| `cmd/redundanet` | The binary. Today: `identity`, `operator`, `manifest`, `validate`, `network`, `agent`, `hub`, `service`, `update`, `release`, `status`, `open`, `logs`, `version` | Growing |

Next, in order: the census heartbeat and the hub's status collector, the
hub's manifest mirror and the join endpoint, the container build for
today's nodes.

## Running it today

```
redundanet network descriptor --manifest manifests/manifest.yaml --repository https://raw.githubusercontent.com/OWNER/REPO/main
    An operator prints the network descriptor: name, hubs, operator keys,
    where the signed manifest is served. It goes into invite links.

redundanet network join <descriptor>
    A device pins the descriptor's operator keys, fetches the manifest and
    its signature, verifies them and keeps the pair on disk.

redundanet network status [--refresh]

redundanet hub   --network NAME [--udp :4433] [--tcp :4443]
redundanet agent --network NAME [--connect DEVICEKEY,...] [--portmap] [--tcp-only]
```

Both long-lived commands need this device's identity file and a joined
network; they refresh the manifest every five minutes, apply a newer
signed revision on the spot, and close connections to devices the new
policy no longer admits. `--manifest FILE` runs them from a signed file
instead, trusting the operators that file lists, for tests and for the hub
that serves the repository it is listed in. The agent answers every stream
with its own bytes, since nothing speaks over the mesh yet, and logs a
status snapshot every minute.

```
redundanet status [--json]      # what the running agent sees, through its local API
redundanet open                 # the local page, through a one-time link that logs the browser in
redundanet logs [-n 100]

redundanet service install --network NAME     # this binary, as a user service that starts at login
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

Go 1.27 or later, no cgo. `.github/workflows/agent.yml` runs the tests on
Linux, macOS and Windows and cross-compiles every target; a tag `agent/vX.Y.Z`,
or `agent/vX.Y.Z-alpha.N` for a pre-release, builds a draft release with
checksums (`agent-release.yml`).

## Identity commands

```
redundanet identity init --network NAME [--storage] [--passphrase]
    New member on this device: shows the phrase, checks three words, writes
    the identity file (device key, member secret, certificate).

redundanet identity show --network NAME
    Member key, its ten-group display code, device key, roles.

redundanet identity device-key --network NAME            (on a headless node)
redundanet identity certify --network NAME --device KEY [--storage] > file   (where the phrase is typed)
redundanet identity accept-certificate --network NAME file   (back on the node)
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
```

## Encodings, fixed here

- Keys and device IDs: 32 bytes as 52 lowercase base32 characters.
- Signed records: deterministic CBOR (RFC 8949 core), signature by Ed25519
  over `"redundanet/3/<domain>\0" || record`, one domain per record kind.
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
  leaf with the device certificate in a critical extension under a
  provisional OID (see `tls.go`; it moves under an IANA enterprise number
  before 3.0.0).
