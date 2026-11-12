# Moving from RedundaNet 2 to RedundaNet 3

This page is for a former member of the RedundaNet 2 grid: the Python command line tool, Docker Compose, the Tinc VPN and Tahoe-LAFS. RedundaNet 2 ended on 13 November 2026: its two hubs, which were the VPN entry points and the Tahoe introducers, were shut down. The old grid no longer connects, `backup-sync` no longer uploads, and what was stored only there can no longer be read through it. Your own files are on your own disks as before. The steps below add your RedundaNet 3 identity, put the agent on each node, back up natively, prove a restore, and retire what is left of the Docker stack.

On a node that ran RedundaNet 2 both programs exist: the Python `redundanet` on PATH is the old tool, the RedundaNet 3 command is `sudo redundanet3`, a copy of the program that root owns at `/usr/local/bin/redundanet3` (the service runs its own at `/var/lib/redundanet3/bin/redundanet`).

## 1. Your identity, on your laptop

Install the agent on your laptop ([install.md](install.md), one line):

```bash
curl -fsSL https://redundanet.com/install.sh | sh
```

In the page it opens (or in `redundanet setup` in the terminal), choose the first case, a new identity. It shows a twelve-word phrase once. Write it down and keep it like a passport: it is your member key, and every device you ever certify comes from it. It prints your member key (52 characters) and this laptop's device key, installs the service and opens the join form of step 4 with the key filled in; close it for now, the attestation of step 2 goes in too. The network is built into the binary; there is nothing to join by hand.

## 2. The attestation that you are you

Your RedundaNet 2 nodes are listed in the manifest with GPG keys. Sign your new member key with one of them; that ties the new identity to the person the network already trusts:

```bash
printf '%s' <your member key> | gpg --armor --detach-sign -u <fingerprint of one of your nodes' keys>
```

If the key lives on the node, run the command there over SSH. The workflow verifies the signature against the fingerprint the manifest lists for your nodes, so it has to be one of those keys, published on a keyserver with its user IDs (keyserver.ubuntu.com keeps them; keys.openpgp.org strips them unless the e-mail is verified, and a key without them cannot be imported). Fingerprints are the full 40 characters; short ids are rejected everywhere.

## 3. The agent on each node

Follow [install.md](install.md), "Linux storage node": one line as root on the node lays the agent out and shows a pairing code, and `redundanet device pair --storage` on your laptop, on the node's network or anywhere (the hub relays the exchange), asks for that code and certifies it; the node joins and starts `redundanet-agent.service` by itself. For a node your laptop cannot reach, the runbook's certificate file over SSH does the same ([../agent/deploy/node/README.md](../agent/deploy/node/README.md)). The agent runs on the host beside the Docker stack, on the host's network, with no access to the containers. From then on it updates itself from the signed release index with probation and rollback, and the node appears on the RedundaNet 3 status page under your name once the manifest lists it.

Do this on every node before the join request, so each node's device key and name can go in it.

## 4. The join request

Fill the form at <https://redundanet.com/join.html> as a new member would ([join.md](join.md), step 4), with two additions:

- the `Legacy GPG fingerprint` row carries the 40-character fingerprint you signed with;
- after the table, a section `## Legacy signature` carries the armored PGP detached signature from step 2.

Your contribution in GB is what your nodes contribute. The workflow verifies the signature, opens the pull request adding you under `members:` with `legacy_pgp` and your nodes under `storage:`, the maintainer signs and merges, and every agent applies the new revision within five minutes. Your RedundaNet 2 node entries stay in the manifest as they are: the attestation is checked against them.

## 5. Native backup sets

The old grid received what `backup-sync` pushed from your node, and receives nothing any more. Make a native backup set for each of those folders, on the device that holds them. On a node:

```bash
sudo redundanet3 backup add NAME FOLDER
sudo redundanet3 backup run NAME
sudo redundanet3 backup list
```

Runs are daily from then on. Your native allocation starts empty. The old Tahoe shares still take room on the node's disk until you delete them (step 7): the native store refuses writes below its floor, and the status page shows free space per server.

## 6. The restore test

A restore test reads a sample of a snapshot back and compares it to the source. The agent runs one weekly; ask for one now and read the result:

```bash
sudo redundanet3 backup verify
sudo redundanet3 backup list
```

`migrate-node` refuses to retire the stack until the folder `backup-sync` pushed is held by a native set with a complete run and a passed restore test on that device. Doing a real restore of something you care about is a good habit too: [backups.md](backups.md).

## 7. Retire the Docker stack

The old grid is off, so nothing reads your node's Tahoe shares any more and the containers only take memory. Retire them:

```bash
sudo redundanet3 migrate-node --dry-run   # the plan, and what is not yet on the native grid
sudo redundanet3 migrate-node             # stops and removes the containers; files and shares stay
sudo redundanet3 migrate-node --rollback  # every service again, together, tinc first
```

What it does: the plan lists the stack's containers by service and every compose profile, then one `docker compose down` through the installed compose file, its override and `.env`, with every profile named, so the Tahoe services go too, every container at once and tinc last. A container that survives puts the stack back and fails the command, since it would run on tinc's dead network namespace. The compose files, the `.env` and the Tahoe share directories stay on disk until you delete them by hand; the shares are other members' ciphertext from the old grid, and the room they take returns to the native store once they are gone. The agent keeps running through all of it and the node stays on the status page. `--force` retires the stack although something is not yet on the native grid, and the plan names what is lost. `--rollback` recreates every recorded service in one `up`, tinc first, never one container at a time. The Python CLI stays installed but idle until you remove it.

## What the end of RedundaNet 2 means

RedundaNet 2 ended on 13 November 2026, when its two hubs were shut down and removed. Nothing was deleted on any member's node: the Tahoe shares are still on the disks that held them, but with no introducer and no VPN entry point the nodes cannot find each other, so the old grid cannot be read or written. What a member never moved to a native backup set is gone from the grid, not from their own disks. No automatic deletion exists: each member retires their own stack with `migrate-node` and deletes the old share directories when they choose. The Python package `redundanet` on PyPI (2.7.40, the last one) and its source, containers and documentation are no longer in this repository; they are in its history, at the tag `v2.7.40`.

## Lost something?

Lost the phrase: a new identity is a new member entry; the maintainer retires the old one. Lost a node: certify a new one, and revoke the old device with `redundanet device revoke --device KEY` on any device of yours ([security.md](security.md)).
