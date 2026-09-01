# CLAUDE.md

Project instructions for AI-assisted development (Claude Code and compatible
tools). Humans: this file is operational config, not documentation — start
with README.md.

## What this is

RedundaNet: community-run encrypted storage. Tahoe-LAFS (erasure coding) over
a Tinc mesh VPN; GPG keys are node identities (also reused as the Tinc key);
a GitHub-hosted YAML manifest (`manifests/manifest.yaml`) is the source of
truth for membership; Docker Compose runs each node; a fly.io hub relays
NAT-to-NAT peers, runs the Tahoe introducer, and serves the public status
page. There is a LIVE production network — changes ship to real machines that
auto-pull `:latest`.

## Commands

```bash
poetry install --sync                 # env
poetry run pytest tests/unit -q       # unit tests (fast; run before proposing changes)
poetry run ruff check src tests       # lint (also: ruff format)
poetry run mypy src                   # strict typing
bash scripts/grid-test.sh             # dockerized 3-node erasure-coding e2e (slow)
```

CI gates publishing: images/PyPI only build from green commits. Never weaken
that chain.

## Hard rules (learned in production)

- **Never restart the tinc container alone.** The Tahoe containers live in its
  network namespace; a tinc restart strands them ("0 shares connected").
  Recreates must name tinc first: see `Deployment.recreate()`.
- **Every CLI compose invocation must include the override file.** Passing
  `-f` disables Docker's auto-load of `docker-compose.override.yml`, which
  holds storage disk bind-mounts; dropping it silently detaches node disks.
  `Deployment._base()` handles this — route compose calls through it.
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
  routed around; do not chase it).
- **Encoding parameters are per-file at upload time.** Changing k-of-n in the
  manifest affects only new uploads; old files need re-upload to re-encode.

## Conventions

- Python 3.11+, strict mypy, ruff (rules in `pyproject.toml`). Line length 100.
- Entrypoints (`docker/entrypoints/*.py`) are written as pure, testable
  functions with injected runners/clocks; tests import them via
  `sys.path.insert` (see `tests/unit/test_backup_sync.py`).
- The monitor (`src/redundanet/monitor/`) is pure logic: every external input
  (pings, censuses, clock, files) is a parameter. Keep it that way.
- Version lives in `pyproject.toml` only. Every user-facing change bumps the
  patch version; the human maintainer does all commits, tags, and pushes.
- Docs describe what the code DOES, not what it should do. When behavior and
  docs disagree, fixing the drift is part of the change.
- No em dashes in user-facing prose (maintainer preference).

## Release flow

1. Bump `pyproject.toml`, run the full local gate (pytest+ruff+mypy).
2. Maintainer commits and pushes; CI must be green.
3. Maintainer tags `vX.Y.Z` → release workflow publishes PyPI + ghcr images
   (+ Raspberry Pi image built under QEMU).
4. Hub changes additionally need `fly deploy` (the hub is built by fly, not
   from ghcr images).
5. Nodes pick up images via `redundanet update` (health-checked, auto-rollback).

## AI assistance policy

Substantial parts of this codebase are written with AI assistance. Every
change is human-reviewed, CI-gated, and proven on the live network before
release. Accountability stays with the human maintainer. AI-assisted
contributions are welcome under the same rule: the submitter reviews, tests,
and owns what they submit.
