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
| `internal/wire` | The hub channel protocol: typed frames with CBOR control messages, relay payloads, the synthetic relay address | Done |
| `internal/transport` | The mesh endpoint: one UDP socket owned by quic-go, the hub channel (QUIC, or TLS over TCP when UDP is blocked), relayed connections over hub datagrams, hole punching through quic-go's non-QUIC packet hook, port mapping, and a session per peer with a relay and a direct connection, new streams on the best live one | Done, with end-to-end tests |
| `internal/hub` | Hub mode's rendezvous and relay: address reflection, sticky dialer roles, datagram relay with bounded dropping queues, a TLS carrier for UDP-blocked devices, status | Done, tested through the transport |
| `internal/portmap` | PCP, NAT-PMP and UPnP port mapping, from the spike | Done |
| `internal/version` | The build's version string | Done |
| `cmd/redundanet` | The binary. Today: `identity`, `operator`, `manifest`, `validate`, `agent`, `hub`, `version` | Growing |

Next, in order: membership at runtime (fetch the manifest from the
repository and the hub mirror, pin operators from the network descriptor,
revocations), the service and self-update, the local page, the census and
status collector.

## Running it today

```
redundanet hub   --network NAME --manifest manifests/manifest.yaml [--udp :4433] [--tcp :4443]
redundanet agent --network NAME --manifest manifests/manifest.yaml [--connect DEVICEKEY,...] [--portmap] [--tcp-only]
```

Both need this device's identity file and a signed manifest on disk; the hub
must be listed in the manifest by its device key, and every connecting
device must chain to an active or leaving member. Until the network
descriptor exists, the operators trusted are the ones the manifest lists,
which the commands say out loud. The agent answers every stream with its
own bytes, since nothing speaks over the mesh yet, and logs a status
snapshot every minute.

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
Linux, macOS and Windows and cross-compiles every target; a tag `agent/vX.Y.Z`
builds a draft release with checksums (`agent-release.yml`).

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
