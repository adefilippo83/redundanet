# Joining RedundaNet

This page is for a new member. A RedundaNet 2 member who already has nodes on the old grid follows [migration.md](migration.md) instead. Install the agent first: [install.md](install.md).

Joining takes an identity on your laptop, a form on the website, and a pull request the maintainer signs. Your member name and member key are public in the repository's history forever; see [../PRIVACY.md](../PRIVACY.md).

## 1. Your identity, on your laptop

```bash
redundanet identity init
```

It shows a twelve-word phrase once and asks for three of the words back. Write the phrase down and keep it like a passport: it is your member key, every device you ever certify comes from it, and nobody can recover it for you. The command prints your member key (52 characters) and this laptop's device key. Say `--passphrase` to seal the identity file on disk.

The network needs no step of yours: its name, its hub, the operator keys and where the signed manifest is served are built into the binary, and the agent fetches the manifest the first time it needs it. `redundanet network status` shows what your device believes about the network. Your device is not admitted yet: that happens when the manifest lists you.

## 2. If you contribute disk: prepare your storage node

A member who contributes disk lists at least one storage device. Prepare the node now so you have its device key for the form: one line on the node and a pairing command on your laptop, in [install.md](install.md), "Linux storage node". The node prints its device key and name once paired, and `sudo /var/lib/redundanet3/bin/redundanet status` on it shows them again. The node needs at least 10 GiB free above its floor. Your contribution in GB is what you promise the network from that disk.

## 3. The form

Fill the form at <https://redundanet.com/join.html>. It opens a GitHub issue, so you need a GitHub account; your GitHub login, lowercased, is your member name unless you choose another lowercase name. Names are lowercase letters, digits and hyphens.

The issue title is `[Join Request] Member: <name>` and the body starts with the heading `## RedundaNet 3 membership` followed by a Markdown table with exactly these rows:

| Row | Required |
| --- | --- |
| `Member name` | yes |
| `Member key` | yes, the 52-character key from step 1 |
| `Contribution GB` | yes, 0 if you contribute no disk |
| `Device key` | when Contribution GB is above 0 |
| `Device name` | when Contribution GB is above 0 |
| `UDP port` | optional: a port you forward on your router, so your node is reached directly more often |
| `Legacy GPG fingerprint` | optional, RedundaNet 2 members only |
| `Notes` | optional |

Leave the optional cells empty. The form also has a terms checkbox: the member name and key are public in the repository's history forever.

Nothing leaves the page until you open the issue on GitHub; the website loads no analytics.

## 4. What happens next

1. A workflow validates the request and opens a pull request adding you under `members:` (name, key, contribution_gb, joined, status: active, legacy_pgp when given) and your node under `storage:` (member, device, name, udp_port when given), and raising `revision` by one.
2. The maintainer reviews, signs `manifest.sig` offline with the operator key, and merges.
3. Every agent applies the new revision within five minutes. Your devices are admitted from then on; `redundanet network status --refresh` shows the new revision.

A member without a storage device needs `contribution_gb: 0` with an `allocation_gb` and a `sponsor` (another member or the reserve), which the maintainer sets.

## 5. Service and first backup

```bash
redundanet service install
redundanet backup add NAME FOLDER
```

The service starts at login and runs the first backup two minutes after it starts, then daily. `redundanet backup list` follows it; `redundanet open` shows the local page. On macOS a folder under Documents, Desktop or Downloads needs Full Disk Access for the agent; `backup add` says so. Everything about sets and restores: [backups.md](backups.md).

## Your allocation

Your allocation is derived from your contribution and the encoding overhead: contribution, less the network's reserve, times k/n. `redundanet validate --manifest manifests/manifest.yaml` on a checkout of the repository prints every member's allocation. The status page shows what you use against it.

## More devices

Every device of yours is certified from the phrase. Do not run `identity init` on a second device: that makes a new member. Pair it instead, without moving the phrase: on the new device `redundanet identity pair` shows a one-time code; on a device of yours that has its identity, on the same network, `redundanet device pair <code>` asks for the phrase, certifies the new device and hands it the identity and the network. Add `--storage` if the new device contributes disk. Devices need no manifest change: servers accept any device whose certificate chains to a listed member.

For a device reached over SSH only, the certificate file does the same in three commands:

```bash
redundanet identity device-key                              # on the new device: prints its device key
redundanet identity certify --device <KEY> > cert           # where you type the phrase
redundanet identity accept-certificate cert                 # back on the new device
```

The certificate file carries your member secret: copy it over SSH, accept it, then delete both copies.
