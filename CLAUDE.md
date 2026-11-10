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
release on their channel, and the RedundaNet 2 grid still holding members'
data beside it. Changes ship to real machines.

RedundaNet 2 (the Python CLI in `src/`, Docker Compose, Tinc, Tahoe-LAFS,
the fly.io hub) is the legacy product, live until the stop date the
maintainer announces; its section is below.

## Commands

```bash
cd agent && go test -race ./...     # the agent's gate; CI runs it on Linux, macOS and Windows
cd agent && go build ./cmd/redundanet
cd agent && go vet ./... && test -z "$(gofmt -l .)"   # CI runs both too
```

Legacy Python (only when `src/`, `tests/`, `docker/` or `scripts/` change):

```bash
poetry install --sync                 # env
poetry run pytest tests/unit -q       # unit tests (fast; run before proposing changes)
poetry run ruff check src tests       # lint (also: ruff format)
poetry run mypy src                   # strict typing
bash scripts/grid-test.sh             # dockerized 3-node erasure-coding e2e (slow)
```

CI gates publishing: binaries, images and PyPI only build from green
commits. Never weaken that chain.

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
  manifests/manifest.yaml` must pass (CI runs it, and the legacy validator
  for the v2 half); the maintainer signs `manifest.sig` offline. Unknown
  keys are errors; the v2 keys are the only ones set aside.
- **`migrate-node` stops and starts the whole stack.** The Tahoe services
  share tinc's network namespace, so the stack is only ever taken down whole
  (every profile named) and brought back in one `up`, tinc first; never one
  container at a time. Keep `agent/internal/migrate` that way.
- **The local API is loopback only** with the browser guards the RFC lists
  (one-time login link, session cookie, CSRF token, Host and Origin checks,
  no GET that changes state). Never bind it to the LAN.
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
   pre-release early), then its `index-pr` job checks the release as it is
   on GitHub and opens `release/index-X.Y.Z` with the next index revision,
   unsigned. Windows signing and macOS notarization run only once their
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
   (`agent/deploy/hub/README.md`); a failed candidate is never retried
   until `redundanet update retry VERSION`. A withdrawn release is
   `withdrawn: true` in the index, revision raised, signed again.
5. Every user-facing change updates the docs under `docs/` in the same
   change. Docs describe what the code DOES.

## RedundaNet 2 (legacy, live until the stop date)

The Python CLI (`src/redundanet`, package `redundanet` on PyPI), Docker
Compose, Tinc mesh VPN, GPG keys as node identities, Tahoe-LAFS 2-of-4, and
the fly.io hub that also serves the RedundaNet 3 status page under `/v3/`.
Its docs are under `docs/legacy/`. Its hard rules, learned in production,
stay in force on the old grid:

- **Never restart the tinc container alone.** The Tahoe containers live in its
  network namespace; a tinc restart strands them ("0 shares connected").
  Recreates must name tinc first: see `Deployment.recreate()`.
- **Every CLI compose invocation must include the override file.** Passing
  `-f` disables Docker's auto-load of `docker-compose.override.yml`, which
  holds storage disk bind-mounts; dropping it silently detaches node disks.
  `Deployment._base()` handles this — route compose calls through it. The
  override is found next to the compose file the CLI resolved, so that file
  must be the installed one: `join` pins it (`REDUNDANET_COMPOSE_FILE`), the
  repo clone is the last candidate, and `update` refuses when the installed
  override would be dropped.
- **Compose project name is `redundanet`** (`-p redundanet`); without it the
  CLI cannot see the containers.
- **GPG identities are full 40-char fingerprints, exact match, fail closed.**
  Short 8/16-char ids are Evil32-forgeable and rejected everywhere (manifest
  validation, join bot, keyserver client). Never reintroduce suffix matching.
- **The manifest is read at runtime with `yaml.safe_load` by entrypoints** —
  a broken manifest on main reaches live nodes in ~5 minutes. `redundanet
  validate` exits non-zero on blocking errors; keep it that way.
- **`.env` files are merged, never overwritten** (`_merge_env`): operators add
  keys (SFTP_*, SYNC_*) that a re-join must preserve.
- **Tahoe upstream is dormant** (1.20.0, Dec 2024). Its transitive deps are
  pinned in the Dockerfiles (`pyopenssl<25`, `service-identity<25`) — scars,
  not suggestions. The SFTP frontend never closes sessions (known, upstream,
  routed around; do not chase it). Its lease crawler crashes after a restart
  mid-cycle; `docker/patches/tahoe_expirer_json_state.py` fixes that in the
  base image and the build fails if Tahoe moves the patched code.
- **Encoding parameters are per-file at upload time.** Changing k-of-n in the
  manifest affects only new uploads; old files need re-upload to re-encode.

Legacy release flow: bump `pyproject.toml` (version lives there only; every
user-facing Python change bumps the patch version; `readme` points at
`docs/legacy/README.md`, the PyPI long description), run the Python gate,
maintainer tags `vX.Y.Z`; the release workflow publishes PyPI and ghcr
images. The Raspberry Pi image is RedundaNet 3's since 3.0.0: built by
`build-rpi-image.yml` after a stable agent release, never for a `v*` tag.
The tahoe-* images build
FROM `redundanet-tahoe-base`, which `tahoe-base.yml` rebuilds only when
`poetry.lock` or `docker/Dockerfile.tahoe-base` changes. Images are 64-bit
only. Hub changes additionally need `fly deploy`. Nodes pick up images via
`redundanet update` (health-checked, auto-rollback).

## Conventions

- Go 1.27 or later, no cgo. CI runs `gofmt -l`, `go vet`, `go test -race`
  and `go build` on Linux, macOS and Windows; all four must be clean. Tests
  sit beside the code; the jobs are tested on the engine fake in
  `internal/engine`, which has the same contract as the native engine.
- Python 3.11+, strict mypy, ruff (rules in `pyproject.toml`). Line length
  100. Entrypoints (`docker/entrypoints/*.py`) are pure, testable functions
  with injected runners/clocks; the monitor (`src/redundanet/monitor/`) is
  pure logic. Keep both that way.
- The human maintainer does all commits, tags, and pushes.
- Docs describe what the code DOES, not what it should do. When behavior and
  docs disagree, fixing the drift is part of the change. `docs/legacy/` is
  frozen except for the banner.
- The agent's page (`agent/internal/api`) is in the website's ten languages: a text added to `locales/en.json` is added to the nine others in the same change (`TestDictionaries` fails otherwise), and the page takes no inline style or script (its content policy forbids them).
- No em dashes in user-facing prose (maintainer preference). The legacy
  rules above are quoted as written.
- Commands meant for a storage node are written with the full path,
  `sudo /var/lib/redundanet3/bin/redundanet`; on a laptop, bare `redundanet`.

## AI assistance policy

Substantial parts of this codebase are written with AI assistance. Every
change is human-reviewed, CI-gated, and proven on the live network before
release. Accountability stays with the human maintainer. AI-assisted
contributions are welcome under the same rule: the submitter reviews, tests,
and owns what they submit.
