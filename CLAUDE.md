# CLAUDE.md

Project instructions for AI-assisted development (Claude Code and compatible
tools). Humans: this file is operational config, not documentation; start
with README.md and docs/README.md.

## What this is

RedundaNet 3: community-run encrypted backup. One static Go binary,
`redundanet`, in the module `agent/` (no cgo, no containers): the agent on
every member device (client and storage roles), the hub on a server (relay,
census collector, status page), and the CLI over the agent's local API on
127.0.0.1:4470. Objects are encrypted on the device and erasure coded k-of-n
onto other members' storage servers; the hub schedules repair without the
owner. Membership is `manifests/manifest.yaml` plus `manifest.sig`, signed
offline by the maintainer's operator key; agents fetch it every five minutes.
Releases are listed in `releases/index.yaml` plus `index.sig`, signed offline
by the release key embedded in every build; the agent self-updates from it
with probation and rollback. The live network's descriptor (name, hub,
operator keys, manifest URL) is built into the binary too
(`agent/internal/version`): every `--network` flag defaults to it and a
device that never joined by hand is seeded from it, so `--network` and
`network join <descriptor>` appear in docs only for another network.
`docs/install.sh` and `docs/install.ps1`, served at redundanet.com/install.sh
and /install.ps1, are the one-line installers (sh for macOS and Linux,
PowerShell for Windows); `redundanet setup` then takes a laptop through
identity, service and join request. Design: `rfcs/0001-redundanet-3.md`; the
package map: `agent/README.md`. `spike/` is the measured prototype that
preceded it and is not edited.

There is a LIVE production network: one hub (`hub-hetzner`), storage nodes
and laptops that self-update from the signed index within an hour of a
release on their channel. Changes ship to real machines.

RedundaNet 2 (a Python CLI, Docker Compose, Tinc, Tahoe-LAFS) ended on
13 November 2026 and its code is no longer in this repository; what is
left of it here is listed in its section below.

## Commands

```bash
cd agent && go test -race ./...     # the agent's gate; CI runs it on Linux, macOS and Windows
cd agent && go build ./cmd/redundanet
cd agent && go vet ./... && test -z "$(gofmt -l .)"   # CI runs both too
```

The join pipeline's Python scripts (only when `.github/scripts/`,
`pyproject.toml` or `poetry.lock` change):

```bash
poetry install --sync                      # env: the scripts' packages, nothing else
poetry run pytest                          # the scripts' unit tests (.github/scripts/tests)
poetry run ruff check .github/scripts      # lint (also: ruff format)
```

CI gates publishing: binaries only build from green commits. Never weaken
that chain.

## Hard rules, RedundaNet 3

- **The Go module has no cgo and must stay that way** (one static binary per
  target).
- **The agent's version is set at release time** by `agent-release.yml` from
  a tag `agent/vX.Y.Z` (a pre-release suffix such as `-alpha.1` is allowed).
  It never touches `pyproject.toml`; a Go change bumps nothing there.
- **Nothing in the manifest can name a release, and nothing in the release
  index can admit a member.** Keep the operator key and the release key
  separate.
- **What a binary trusts is in source, and a release build is one script.**
  The release key, the index address and the network descriptor are in
  `agent/internal/version`; never set them from a repository variable, a
  secret or a workflow input. `scripts/build-agent.sh` is the whole build
  and takes only the version, so the maintainer's rebuild matches CI byte
  for byte: nothing in it may depend on the machine, the path or the time.
- **Agents apply only a signed manifest with a higher revision.** Every
  manifest edit raises `revision` by one; `redundanet validate --manifest
  manifests/manifest.yaml` must pass (CI runs it); the maintainer signs `manifest.sig` offline. Unknown
  keys are errors; the v2 keys are the only ones set aside.
- **`migrate-node` stops and starts the whole stack.** The Tahoe services
  share tinc's network namespace, so the stack is only ever taken down whole
  (every profile named) and brought back in one `up`, tinc first; never one
  container at a time. Keep `agent/internal/migrate` that way.
- **The local API is loopback only** with the browser guards the RFC lists
  (one-time login link, session cookie, CSRF token, Host and Origin checks,
  no GET that changes state). Never bind it to the LAN. The page lives
  under the session's own address (`/s/<key>/`) and its links are relative,
  so a new link or form action never starts with `/`; the CLI signs its
  requests and never sends the token.
- **The status page and the diagnostics bundle carry no address, path or
  storage index.** Keep the allowlists in `internal/collector` and
  `internal/diag`.
- **Deletion is two-phase** (7-day immunity on the server's clock, 30-day
  window, record kept after the purge so repair never fills a deleted
  object in). Do not shortcut it.
- **Signed records are deterministic CBOR under one domain per kind**;
  known-answer tests pin the encodings. A change to one is a format change.

## Release flow, RedundaNet 3

1. Run the gate (`go test -race ./...`); the maintainer commits and pushes;
   CI must be green.
2. Maintainer tags `agent/vX.Y.Z` (or `-alpha.N`, `-beta.N`, `-rc.N`); the
   `agent release` workflow builds every target, opens a draft release with
   `sha256sums.txt` and the index fragment (plain version stable,
   pre-release early). It does not touch the index: the pull request
   `release/index-X.Y.Z` is opened by `scripts/sign.sh` in step 3, signed.
   Windows signing and macOS notarization run only once their
   secrets exist; until then the binaries ship unsigned and the index is
   the verification. A second run for a tag stops at its first step.
3. Maintainer runs `scripts/sign.sh release X.Y.Z` on the laptop with the
   release key: it rechecks, builds the tag again with the tag's
   `scripts/build-agent.sh` and refuses unless every binary on the release
   equals the one built there, rebuilds the revision on main with
   `redundanet release add`, signs, merges and undrafts. Manifest pull
   requests the same way with `scripts/sign.sh manifest PR`. The keys never
   go into CI or a server. See `releases/README.md`.
4. Nodes and laptops update themselves within an hour: probation up to two
   minutes, `redundanet.old` kept, automatic rollback. The hub updates
   itself the same way with probation on the same ports
   (`agent/deploy/hub/README.md`); a candidate that ran out of time is
   tried again after a backoff (agent 6 h then 24 h, hub 24 h twice), one
   that exited or failed three times waits for `redundanet update retry
   VERSION`. A withdrawn release is
   `withdrawn: true` in the index, revision raised, signed again.
5. Every user-facing change updates the docs under `docs/` in the same
   change. Docs describe what the code DOES.

## RedundaNet 2 (ended 13 November 2026)

The Python CLI (package `redundanet` on PyPI, 2.7.40 the last), Docker
Compose, Tinc mesh VPN, GPG keys as node identities, Tahoe-LAFS 2-of-4 and
its two hubs. The hubs are shut down and removed; `src/`, `tests/`,
`docker/`, `docs/legacy/`, `fly.toml` and the workflows that built and
published it are gone from the tree and live in the history at the tag
`v2.7.40`. Do not bring any of it back. What is left, on purpose:

- **The v2 half of the manifest** (`nodes`, `introducer_furl` and the v2
  keys under `network`) and the `legacy_pgp` records of two members. The
  agent's parser sets exactly those v2 keys aside and still accepts
  `legacy_pgp`; nothing checks the records any more. The join script reads
  `nodes` for one rule: the name of a former member is given only to a
  request from that GitHub account. Removing the v2 half is a manifest
  revision the maintainer signs.
- **`redundanet migrate-node`** (`agent/internal/migrate`), for members
  whose nodes still run the old containers: the hard rule above stays.
- **The name `redundanet3`** on storage nodes, because a node that ran
  RedundaNet 2 has the Python `redundanet` on PATH.

The Raspberry Pi image is built by `build-rpi-image.yml` after a stable
agent release. Images are 64-bit only.

## Conventions

- Go 1.27 or later, no cgo. CI runs `gofmt -l`, `go vet`, `go test -race`
  and `go build` on Linux, macOS and Windows; all four must be clean. Tests
  sit beside the code; the jobs are tested on the engine fake in
  `internal/engine`, which has the same contract as the native engine.
- Python 3.11+ and ruff (rules in `pyproject.toml`, line length 100) for
  the scripts in `.github/scripts`, the only Python left. Their packages
  come from `poetry.lock`, with hashes.
- The human maintainer does all commits, tags, and pushes.
- Workflows name every action by commit, with the version in a comment
  (Dependabot moves both). `process-join-request.yml` runs for any
  stranger's issue: the jobs that run scripts or packages hold a read-only
  token, the job that opens the pull request takes only the manifest from
  them, and the job that reports the `validate` check runs nothing but
  `actions/github-script`. Keep that split.
- Docs describe what the code DOES, not what it should do. When behavior and
  docs disagree, fixing the drift is part of the change.
- The agent's page (`agent/internal/api`) is in the website's ten languages: a text added to `locales/en.json` is added to the nine others in the same change (`TestDictionaries` fails otherwise), and the page takes no inline style or script (its content policy forbids them).
- No em dashes in user-facing prose (maintainer preference).
- Commands meant for a storage node are written `sudo redundanet3`, the
  copy root owns at `/usr/local/bin/redundanet3`; on a laptop, bare
  `redundanet`. Root never runs `/var/lib/redundanet3/bin/redundanet`: the
  service user owns that file so the updater can replace it. The name ends
  in 3 because a RedundaNet 2 node has the Python `redundanet` on PATH.

## AI assistance policy

Substantial parts of this codebase are written with AI assistance. Every
change is human-reviewed, CI-gated, and proven on the live network before
release. Accountability stays with the human maintainer. AI-assisted
contributions are welcome under the same rule: the submitter reviews, tests,
and owns what they submit.
