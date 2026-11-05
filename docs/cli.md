# Command reference

Every command of the `redundanet` binary, version 3.0.0. `redundanet help` prints the list. On a laptop the command is `redundanet`; on a Linux storage node name the full path, `sudo /var/lib/redundanet3/bin/redundanet`, which finds the node's data directory on its own for the commands that talk to the running agent.

Two flags are accepted by most commands:

- `--data-dir DIR`: the data directory (default: the platform's user config directory under `redundanet/`, or `$REDUNDANET_DATA_DIR`).
- `--network NAME`: the network name, as in the manifest.

Exit status is 0 on success, 1 on an error, 2 for a usage error.

## Member commands

```
redundanet identity init --network NAME [--storage] [--passphrase] [--yes]
```
New member on this device: shows the twelve-word phrase, checks three words back, writes the identity file (device key, member secret, certificate) and prints the member key, its display code and the device key. `--storage` adds the storage role. `--passphrase` seals the file. `--yes` skips the three words (scripts only).

```
redundanet identity show --network NAME
```
Member key, its ten-group display code, device key, roles, certificate date and serial.

```
redundanet network join <descriptor>
```
Pins the descriptor's operator keys, fetches the manifest and its signature, verifies them and keeps the pair on disk.

```
redundanet network status [--refresh]
```
What the device believes about its network, as JSON; `--refresh` fetches first.

```
redundanet service install --network NAME [--channel early]
redundanet service status
redundanet service uninstall
```
Installs this binary as a user service that starts at login: a launchd agent on macOS, a systemd user unit with lingering on Linux, a logon task on Windows. The service runs `agent --network NAME --data-dir DIR [--channel early]`.

```
redundanet status [--json]
```
What the running agent sees: version, uptime, member and device, manifest revision, hubs and their carrier, peers and their paths, updates, service state.

```
redundanet open [--print]
```
Opens the local page in the browser through a one-time link that works for two minutes; `--print` only prints the link, for an SSH tunnel.

```
redundanet logs [-n 100]
```
The running agent's recent log lines.

```
redundanet backup add NAME FOLDER [FOLDER...] [--exclude a,b]
redundanet backup remove NAME
redundanet backup list
redundanet backup run [NAME]
redundanet backup verify
redundanet backup cleanup [--dry-run] [--force]
redundanet backup rebalance
```
Backup sets of the running agent. `list` shows each set's last run, skipped files by reason, the schedule, the restore tests and the streak. `run` starts a run now. `verify` asks for a restore test. `cleanup` deletes what nothing references: `--dry-run` counts and deletes nothing, `--force` lifts the bound on how much one run may delete. `rebalance` converts the device's objects to the new encoding while the manifest declares a transition.

```
redundanet restore list
redundanet restore browse DEVICE SNAPSHOT [PATH]
redundanet restore start DEVICE SNAPSHOT [PATH] --to DIR [--overwrite]
redundanet restore status
```
Restore from the running agent. `list` shows every device's snapshots from the servers' roots. `browse` lists a directory without fetching what is not asked for. `start` restores a snapshot or a path under it into `DIR`, in the background; `--overwrite` replaces files that exist. `status` reports progress, the result and each exception.

```
redundanet update check [--channel early]
redundanet update apply [--channel early]
redundanet update status [--channel early]
redundanet update retry VERSION
```
The self-updater by hand. `check` names the newest release that fits the channel; `apply` installs it (restart the agent or the service to run it); `status` is the updater's record; `retry` allows a version that failed probation to be installed again.

```
redundanet diagnostics [--out FILE]
```
Writes the bundle to send when something is wrong (default `redundanet-diagnostics-DATE.zip` in the current directory). Works with the agent down. Every path, URL and address is redacted; nothing is uploaded.

```
redundanet version
```
The version, platform and Go version of this binary.

## Device commands

```
redundanet identity device-key --network NAME
```
On a headless node: makes the device key, keeps it pending, prints it and the two commands that follow.

```
redundanet identity certify --network NAME --device KEY [--storage] [--serial N] > file
```
Where the phrase is typed: asks for the phrase, certifies the device key, writes the certificate and the member secret to standard output. `--storage` grants the storage role. Copy the file to the device over SSH and delete both copies after acceptance.

```
redundanet identity accept-certificate --network NAME <file>
```
Back on the device: checks the certificate against the pending key and the network, writes the identity file, removes the pending key.

```
redundanet device revoke --device KEY
```
New in 3.0.0. On any device of the member: asks for the phrase and prints a signed revocation record to add to the manifest under `revocations:`. The operator signs and merges; every agent and server refuses the device from then on.

## Operator commands

```
redundanet operator init [--key operator.key]
```
Makes the offline operator key and prints the public half for the manifest's `operators` list.

```
redundanet validate [--manifest manifests/manifest.yaml]
```
Parses the v3 half of the manifest (v2 keys set aside, anything else unknown is an error), applies the rules, prints errors, warnings and the allocation per member; exits 1 on errors.

```
redundanet manifest sign [--key operator.key] [--manifest manifests/manifest.yaml] [--out FILE]
```
After a merge: validates, then writes `manifest.sig` beside the manifest (or `--out`), the operator's signature over the network, the revision, the time and the file's hash.

```
redundanet manifest verify [--manifest FILE] [--sig FILE] [--trust KEY,...]
```
Checks the signature against the file. Without `--trust` it uses the operators listed in the manifest itself, which proves consistency, not authenticity. Reports the age and whether `signature_max_age_days` has passed.

```
redundanet network descriptor [--manifest manifests/manifest.yaml] --repository URL
```
Prints the network descriptor (name, hubs, operator keys, where the manifest is served) for invite links and the join page. `URL` is the base under which `manifests/manifest.yaml` and `manifest.sig` are served, for example `https://raw.githubusercontent.com/OWNER/REPO/main`.

```
redundanet hub --network NAME [--udp :4433] [--tcp :4443] [--status 127.0.0.1:4480] [--legacy-status URL]
               [--publish URL --publish-token-file FILE] [--channel stable|early] [--no-update]
               [--manifest FILE --sig FILE] [--status-every 1m] [--probation] [--probation-peers KEYS] [-v]
```
Runs hub mode: rendezvous and relay on `--udp` (QUIC) and `--tcp` (TLS carrier; empty disables), the status page and `status.json` on `--status` (empty disables), a RedundaNet 2 `status.json` mirrored beside the mesh census with `--legacy-status`, and with `--publish` the page and `status.json` PUT under that URL every minute with the bearer token in `--publish-token-file`. `--manifest` runs from a signed file instead of the joined network. `--probation` and `--probation-peers` are used by the updater. Runbook: [../agent/deploy/hub/README.md](../agent/deploy/hub/README.md).

```
redundanet storage has [--storage-path DIR] STORAGE_INDEX
redundanet storage drop [--storage-path DIR] [--reason TEXT] STORAGE_INDEX SHARE_NUMBER
```
This node's own share store, beside the running agent; run as the service user. `has` reports the owner, descriptor and complete and partial shares held. `drop` removes one share on purpose, with the reason recorded in the log; the census tells the hub within seconds, which is how repair is drilled. `--storage-floor-gb N` (default 10) is accepted for symmetry with the agent.

```
redundanet object put|get|has|check|repair|delete --network NAME [--out FILE] [--timeout 30s] FILE|CAP
```
Drives the engine by hand on a standalone endpoint with the device's identity and manifest. `put FILE` stores a file and prints its read capability; `get CAP` writes it to standard output or `--out`; `has FILE` says whether the member already stores that content; `check CAP` prints the storage index, the health and the holders; `repair CAP` is the owner's repair; `delete CAP` sends delete tokens to every server. `--timeout` is per server operation.

## Maintainer commands

```
redundanet release keygen [--key release.key]
```
Once, offline: makes the release key and prints the public half to embed in builds. CI never holds the key.

```
redundanet release sign [--key release.key] [--index releases/index.yaml]
```
Signs the release index; writes `index.sig` beside it. Flow: [../releases/README.md](../releases/README.md).

## Node commands

```
redundanet agent --network NAME [--data-dir DIR] [--api 127.0.0.1:4470] [--storage-path DIR] [--storage-floor-gb 10]
                 [--channel stable|early] [--no-update] [--portmap] [--tcp-only] [--connect KEY,...] [--listen :0]
                 [--manifest FILE --sig FILE] [--status-every 1m] [--loopback-candidates] [--probation] [--probation-peers KEYS] [-v]
```
Runs the agent; the service and the node unit run this. `--api` is the local API and page, loopback only. `--storage-path` is the disk a storage device stores on (the store lives in its `redundanet3` directory) and `--storage-floor-gb` the free gigabytes below which the store refuses writes. `--portmap` asks the router for a port mapping; `--tcp-only` reaches hubs over TCP only; `--connect` keeps sessions with the named devices; `--listen` is the UDP address to bind. `--manifest` runs from a signed file instead of the joined network, for tests. `--probation`, `--probation-peers` and `--loopback-candidates` are for the updater and tests.

```
sudo /var/lib/redundanet3/bin/redundanet migrate-node [--dry-run] [--force] [--rollback] [--compose-dir DIR] [--env-file FILE]
```
Linux, as root, on a node that runs the agent beside the RedundaNet 2 stack: retires the stack. Refuses while the folder `backup-sync` pushed to the old grid is not held by a native set with a complete run and a passed restore test on this device, or while no hub is connected; `--force` overrides and the plan names what is lost. `--dry-run` prints the plan and changes nothing. The stack is stopped whole, every profile named, tinc last; files and Tahoe shares stay on disk. `--rollback` starts every recorded service again in one `up`, tinc first, and forgets the migration. `--compose-dir` and `--env-file` override where the compose file and the `.env` are installed. Guide: [migration.md](migration.md).
