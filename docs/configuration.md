# Configuration

RedundaNet 3 has little to configure. The network's parameters are in the signed manifest; a device has a data directory, a `backup.yaml` and, on a storage node, `agent.env`. This page lists all of it. The legacy RedundaNet 2 configuration is in [legacy/configuration.md](legacy/configuration.md).

## The manifest (schema 3)

`manifests/manifest.yaml` in the repository is the source of truth for membership. The agent follows only a revision whose `manifests/manifest.sig` an operator key signed, fetches it every five minutes, and applies a newer signed revision at once. During the migration the same file also carries the RedundaNet 2 keys (`nodes`, `introducer_furl`, and `version`, `domain`, `vpn_network`, `tahoe`, `quota` under `network`); the agent sets exactly those aside and refuses any other key it does not know, so a typo is caught.

Top level:

| Key | Meaning |
| --- | --- |
| `schema` | `3`; the agent refuses any other value. |
| `revision` | A whole number, at least 1, raised by one with every change; an agent applies only a revision above the last one it applied. |
| `network` | The network's parameters, below. |
| `operators` | The keys whose signature makes a manifest take effect: a list of `name` and `key`. |
| `hubs` | The hubs: `name`, `key` (the hub's device key), `relay` (host:port of the QUIC relay), `tcp` (host:port of the TLS carrier; without it, port 443 on the relay's host), `status` and `join` (https URLs, optional). |
| `members` | One entry per member, below. |
| `storage` | One entry per storage device: `member`, `device` (its device key), `name`, and optionally `udp_port`, a port between 1024 and 65535 the member forwards on their router. |
| `revocations` | Signed device revocation records, added by the operator from the output of `redundanet device revoke`; every agent and server refuses a revoked device from then on. New in 3.0.0. |

`network`:

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | required | The network name: lowercase letters, digits and dashes. Devices are certified for it. |
| `encoding` | required | `{k: K, n: N}`: every object is split into N shares of which any K rebuild it. 2-of-3 on the live network today. Applies to new uploads. |
| `encoding_transition` | none | `{from, to, deadline}`: a k-of-n change in progress. `to` must equal `encoding`; the deadline is a date. Until it, servers account shares under `from` at zero when a member stores under `to`, so members convert without deleting first (a new share under `from` is still measured against everything the member holds); `validate` warns once it has passed. |
| `reserve_percent` | 15 | The share of contributions held back for repair, 0 to 50. Allocations come out of the rest. |
| `grace_days` | 30 | How long servers keep a leaving or unlisted member's shares and roots, on their own clocks, before purging them. |
| `min_version` | none | The lowest agent version admitted; the self-updater installs a release that satisfies it. MAJOR.MINOR.PATCH with an optional pre-release suffix. |
| `signature_max_age_days` | 90 | Past this age of `manifest.sig`, a node keeps working for devices it already admitted and refuses new ones until an operator re-signs. |
| `quota_headroom` | 2 | Servers refuse a member's writes past this multiple of the member's budget; the exact budget is the client's. |
| `objects_per_gb` | 1000 | The object-count bound per GB of allocation. |
| `repair_after_hours` | 72 | A server silent this long is dead: its shares count no more and repair is scheduled. Down is ten minutes without a heartbeat. |

A member entry:

| Key | Meaning |
| --- | --- |
| `name` | Lowercase letters, digits and dashes, unique. The GitHub login for members who joined through the form. |
| `key` | The member key, 52 characters. |
| `contribution_gb` | The disk the member contributes. A member above 0 lists at least one storage device; a member at 0 is client-only. |
| `allocation_gb` | Client-only members: the allocation the community granted. A contributing member's allocation is computed, never declared. |
| `sponsor` | Client-only members: `{member, gb}`, whose contribution pays for the allocation; `reserve` for the network's reserve. |
| `vouched_by` | Invite and confirmation signatures, when a member was invited by another. |
| `legacy_pgp` | RedundaNet 2 members: `fingerprint` (the full 40 characters) and `signature`, the PGP detached signature over the member key. |
| `joined` | A date, YYYY-MM-DD. |
| `status` | `active` or `leaving`. |
| `leaving_since` | A date, required when leaving, forbidden when active. |

The allocation of a contributing member is `contribution_gb` less the reserve, times k/n. `redundanet validate --manifest manifests/manifest.yaml` applies every rule, warns on capacity, and prints the allocation per member.

## agent.env (storage node)

A Linux storage node installed with `install.sh` reads `/etc/redundanet3/agent.env`:

| Variable | Meaning |
| --- | --- |
| `NETWORK` | The network name, `redundanet`. |
| `CHANNEL` | The release channel the self-updater follows: `stable` or `early`. |
| `STORAGE_PATH` | The disk the node contributes. The share store lives in `STORAGE_PATH/redundanet3`. |

Change a value and `systemctl restart redundanet-agent`. The unit runs the agent as the system user `redundanet3` with its data under `/var/lib/redundanet3`.

## backup.yaml

The agent's job file lives in the data directory. `backup add` and `backup remove` edit `sets`; the rest is edited by hand (there is no settings page in 3.0.0) and read when the agent starts:

```yaml
version: 2
sets:
  - name: documents
    folders:
      - /home/me/Documents
    excludes:
      - "*.iso"
every: 24h            # between runs of each set
verify_every: 168h    # between restore tests; 7 days
cleanup_every: 168h   # between cleanups of unreferenced objects; 7 days
upload_kbps: 0      # bandwidth caps on the device's mesh traffic, kilobytes per second; 0 is none
download_kbps: 0
retention:
  days: 7             # keep everything this long
  daily: 30           # then one per day
  weekly: 12          # then one per week
  monthly: 12         # then one per month
```

`excludes` are base-name globs or relative path prefixes beside the defaults listed in [backups.md](backups.md). Durations are Go durations (`24h`, `168h`). A file saved by an earlier version with the old monthly restore test moves to weekly on load.

## Data directories

| Platform | Data directory |
| --- | --- |
| macOS | `~/Library/Application Support/redundanet` |
| Linux | `~/.config/redundanet` |
| Windows | `%AppData%\redundanet` |
| Linux storage node | `/var/lib/redundanet3`, binary at `/var/lib/redundanet3/bin/redundanet` |

`--data-dir` or the `REDUNDANET_DATA_DIR` environment variable override it. On a node, root without an agent of its own talks to the node's agent without flags; the identity and join commands run as the service user with `--data-dir`, as the installer does.

What is in it:

| File | What |
| --- | --- |
| `identity` | The device key, the member secret and the device certificate. Sealed with XChaCha20-Poly1305 under an Argon2id key when made with `--passphrase`; otherwise the file mode is its protection, as on a headless node. The member signing key is never in it. |
| `network.yaml` | The network descriptor the device joined with: the pinned operator keys, the hubs, where the manifest is served. |
| `manifest.yaml`, `manifest.sig` | The last good signed manifest, for offline starts. |
| `backup.yaml` | Above. |
| `local.sqlite` | The device's local database: files by scan key and hash, capabilities, placement, restore tests. A cache, rebuilt when lost. |
| `api.addr`, `api.token` | Where the local API listens and the token the CLI proves itself with, mode 0600. The CLI signs its requests with the token and never sends it. |
| `service.log` | The service's output on macOS; Linux uses the journal. |
| `spool/` | Staging for uploads. |
| `versions/`, `updates/state.yaml` | Downloaded releases and the self-updater's record, including candidates that failed probation. |
| `device-key.pending` | On a headless node between `identity device-key` and `identity accept-certificate` only. |

A storage node also has its store in `STORAGE_PATH/redundanet3`: share files, a SQLite index and a marker that ties the store to its disk. A hub keeps `census.json` and `placement.sqlite`.

Removing the data directory removes everything the agent knows; `redundanet service uninstall` removes the service.

## Channels

`stable` is the default and where 3.0.0 lives; `early` gets pre-releases first. A laptop chooses at `redundanet service install [--channel early]`; a node sets `CHANNEL` in `agent.env`. `redundanet update check|apply|status [--channel early]` query and drive the updater by hand.

## The local API

The agent serves its API and page on `127.0.0.1:4470` (`redundanet agent --api 127.0.0.1:4470`), never on the LAN. The CLI reads `api.addr` and `api.token` from the data directory. A browser gets in only through `redundanet open`, whose link works once and for two minutes, sets an HttpOnly, SameSite=Strict session cookie and leads to the session's own address (`/s/<key>/`), the only path the cookie is sent to; every form carries a CSRF token; the API refuses any request whose Host is not loopback and any change whose Origin is not the page; no GET changes anything; the page loads its stylesheet and its one script from the agent itself and nothing from anywhere else. Its language is the browser's until one is chosen in its menu (cookie `redundanet_lang`). A headless node is reached over an SSH tunnel: `ssh -L 4470:127.0.0.1:4470 node`, then `sudo /var/lib/redundanet3/bin/redundanet open --print`.

## Agent flags

The service passes `agent --network NAME --data-dir DIR [--channel early]`. The other flags of `redundanet agent` (`--storage-path`, `--storage-floor-gb`, `--portmap`, `--tcp-only`, `--no-update`, `--api`) are listed in [cli.md](cli.md); the node unit sets the storage ones from `agent.env`.

## Settings that the page and the commands change

`backup.yaml` holds the schedule, the retention policy, the bandwidth caps and each set's excludes, all changeable from the local page (Settings, and each set's card under Backups) or `redundanet backup schedule|retention|exclude|limit`. The update channel chosen on a device is kept in `updates/state.yaml` as `chosen_channel` and wins over the service's `--channel` flag from the next check on; `redundanet update channel stable|early` sets it, from the agent or from another process.
