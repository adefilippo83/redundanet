# RedundaNet

[![Agent CI](https://github.com/adefilippo83/redundanet/actions/workflows/agent.yml/badge.svg)](https://github.com/adefilippo83/redundanet/actions/workflows/agent.yml)
[![License](https://img.shields.io/github/license/adefilippo83/redundanet)](LICENSE)

Community-run encrypted backup. Members give each other disk; everyone's data is encrypted on their own devices and spread over the others' machines.

**Website**: [https://redundanet.com](https://redundanet.com). **Status page**: [https://hub.redundanet.com/](https://hub.redundanet.com/).

## What it is

RedundaNet 3 is a backup network run by its members. Each member runs one program, the `redundanet` agent, on the devices they want backed up and on the machines they contribute disk from. The agent scans your folders every day, encrypts every file on your device, splits it into erasure-coded shares, and places the shares on other members' storage servers. Nobody but you can read your data: not the storage servers, not the hub, not the maintainer. You get an allocation on the network in proportion to the disk you contribute.

Version 3.0.0 is one static binary, written in Go, with no containers, no Python and no Docker. It runs as a user service on a laptop and as a system service on a storage node. Membership is a signed file in this repository; releases are listed in a signed index; the agent updates itself from that index and rolls back on its own if a new version does not come up. The network is small, with one maintainer, and this README says so where it matters.

## How it works

A backup set is one or more folders. Once a day the agent scans them, encrypts every object with a key derived from your member secret and the object's content, splits it with erasure coding k-of-n (2-of-3 on the live network today; the number is set by the signed manifest and can change through a declared transition), and places the shares on other members' storage servers, across members. Snapshots follow a retention policy; restores are per file, folder or snapshot. A weekly restore test reads a sample back and compares it to the source. Deleted data stays recoverable for 30 days.

Storage servers scrub their shares monthly. The hub schedules repair of under-placed objects without involving the owner, and the status page shows it.

Your identity is a 12-word phrase shown once by `redundanet identity init`. Every device gets a certificate signed by the member key derived from it; devices authenticate each other over QUIC with TLS 1.3. Member and storage lists live in `manifests/manifest.yaml`, signed offline by the maintainer's operator key; agents fetch it every five minutes.

The agent reaches the hub outbound. Devices connect to each other directly through hole punching when they can, otherwise through the hub's relay. No inbound firewall rule is needed.

More in [docs/architecture.md](docs/architecture.md).

## Platforms

Tested on Linux (x86-64 and ARM64, including Raspberry Pi 5) and Apple silicon Macs. Windows and Intel Mac builds are provided and pass the test suite, but have not run on the live network yet.

## Install

On macOS and Linux, one line installs the newest release, checked against the signed release index:

```bash
curl -fsSL https://redundanet.com/install.sh | sh
```

- macOS and Linux laptops: `redundanet service install` then installs a user service that starts at login (launchd on macOS, a systemd user unit on Linux).
- Windows: take the zip from the [download page](https://redundanet.com/download.html); the same command installs a logon task.
- Linux storage node: `curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage` lays the agent out under `/var/lib/redundanet3` as a system service and shows a pairing code for your laptop. The phrase never reaches the node.

Step by step, per platform, and the way by hand: [docs/install.md](docs/install.md).

## Join

Install the agent on your laptop, create your identity, then fill the form at [redundanet.com/join.html](https://redundanet.com/join.html). The network itself is built into the binary, so there is nothing to join by hand. The form opens a GitHub issue; a workflow turns it into a pull request; the maintainer reviews, signs the manifest and merges. Your devices are admitted within five minutes of the merge.

```bash
redundanet identity init
```

The whole flow, including what a storage node needs: [docs/join.md](docs/join.md).

## Backups and restores

```bash
redundanet backup add NAME FOLDER [FOLDER...] [--exclude a,b]
redundanet backup list
redundanet restore list
redundanet restore start DEVICE SNAPSHOT [PATH] --to DIR
```

Sets, schedule, excludes, skipped files, restore tests, cleanup and the 30-day window: [docs/backups.md](docs/backups.md).

## Status page

[https://hub.redundanet.com/](https://hub.redundanet.com/) shows members, devices, grid health, repair, and per member the day of the last backup and the restore-test streak. It never shows an IP address, a path or a storage index. Your own device's view is the local page: `redundanet open`.

## Security and privacy

RedundaNet collects nothing: no accounts, no analytics, no telemetry. What the software sends to hubs, storage servers and GitHub, and what stays on your device, is spelled out in [PRIVACY.md](PRIVACY.md). The threat model in short, what the hub can and cannot do, what a lost device or a lost phrase means: [docs/security.md](docs/security.md).

## Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

The Windows binary of a release is signed by SignPath Foundation after the maintainer approves the request. The signature says the binary was built by the release workflow of this repository, from the tagged source, on GitHub's runners, and has not changed since; the certificate names SignPath Foundation as the publisher, not RedundaNet. The macOS binaries are signed with the maintainer's Apple Developer ID and notarized by Apple. Linux binaries carry no signature of their own. On every platform the agent installs only what the [release index](releases/README.md) names, after verifying the maintainer's signature on the index, so a self-update never rests on the platform's signing alone. Each release page states whether its binaries are signed.

Status at 3.0.0: Windows signing through SignPath and macOS notarization are set up in the release workflow but not yet active (accounts pending); until then every binary is unsigned and the signed release index is the way to verify a download on every platform. Linux binaries are never code-signed; the index covers them.

Team:

- Authors, who commit without a further review: Alessandro De Filippo ([@adefilippo83](https://github.com/adefilippo83)).
- Reviewers, who review every change proposed by anyone else before it is merged: the same.
- Approvers, who approve each signing request: the same.

Team members use two-factor authentication on GitHub and SignPath. Every change, AI-assisted ones included, is reviewed by the maintainer before it reaches `main`, and releases are built only from tags on `main`.

Privacy: see the section above and [PRIVACY.md](PRIVACY.md). The agent installs a service only when asked (launchd on macOS, systemd on Linux, a scheduled task at logon on Windows) and says what it did; `redundanet service uninstall` removes it.

## AI assistance policy

RedundaNet is designed and maintained by Alessandro De Filippo. Substantial
parts of the code are written with AI assistance (Claude); every change is
human-reviewed, gated by CI (lint, types, 250+ unit tests, end-to-end grid
tests), and proven on the live production network before release. Bugs, like
credit, land on the human.

Project instructions for AI coding tools live in [CLAUDE.md](CLAUDE.md). AI-assisted contributions are welcome under the same rule as everything else: you review, test, and own what you submit.

## Release and update policy

Releases are tagged `agent/vX.Y.Z` and built by the release workflow. Every release's sha256 and size go into `releases/index.yaml`, which the maintainer signs offline with the release key (`releases/index.sig`); the public half is embedded in every build. The agent installs only what the index names. There are two channels: `stable` (the default; 3.0.0 is on it) and `early` (the alpha builds that preceded it, and the next ones). The running agent checks the index hourly, downloads the asset, verifies hash and size, runs the new version in probation for up to two minutes, keeps the old binary as `redundanet.old`, and rolls back by itself if the new one does not reconnect. A release can be withdrawn from the index. Policy and the maintainer's steps: [releases/README.md](releases/README.md).

## RedundaNet 2 (legacy)

RedundaNet 2 is the earlier product: a Python command line tool, Docker Compose, a Tinc VPN and Tahoe-LAFS. The Python package `redundanet` on PyPI (version 2.7.x) is that legacy tool, not the RedundaNet 3 binary; on a RedundaNet 2 node both exist, the Python one on PATH and the Go one at `/var/lib/redundanet3/bin/redundanet`. The old grid stays live until a stop date the maintainer announces, because its Tahoe shares still hold members' data. Its documentation is under [docs/legacy/README.md](docs/legacy/README.md). Existing members move over with [docs/migration.md](docs/migration.md).

## Documentation

The index of every page is [docs/README.md](docs/README.md). How the network is run, who signs what and how to run your own: [COMMUNITY.md](COMMUNITY.md). Design: [rfcs/0001-redundanet-3.md](rfcs/0001-redundanet-3.md).

## Development

```bash
cd agent && go test -race ./...     # the agent's gate; CI runs it on Linux, macOS and Windows
cd agent && go build ./cmd/redundanet
```

The legacy Python code keeps its own gate (`poetry run pytest tests/unit -q`, `ruff`, `mypy`). Contributions go through a pull request; the template lists the checks.

## License

GNU General Public License v3.0; see [LICENSE](LICENSE).
