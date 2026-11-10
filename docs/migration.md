# Moving from RedundaNet 2 to RedundaNet 3

This page is for a member of the RedundaNet 2 grid: the Python command line tool, Docker Compose, the Tinc VPN and Tahoe-LAFS. The old grid stays live until a stop date the maintainer announces, because its Tahoe shares still hold members' data; six members and eight storage nodes are on it today. Nothing changes for your Tahoe node until you choose to retire it. The steps below add your RedundaNet 3 identity, put the agent on each node, back up natively, prove a restore, and only then retire the Docker stack.

On a RedundaNet 2 node both programs exist: the Python `redundanet` on PATH is the legacy tool, the Go one is at `/var/lib/redundanet3/bin/redundanet`. Every command meant for a node below names the full path.

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

Your contribution in GB is what your nodes contribute. The workflow verifies the signature, opens the pull request adding you under `members:` with `legacy_pgp` and your nodes under `storage:`, the maintainer signs and merges, and every agent applies the new revision within five minutes. Your RedundaNet 2 node entries stay as they are.

## 5. Native backup sets

The old grid received what `backup-sync` pushed from your node. Make a native backup set for each of those folders, on the device that holds them. On a node:

```bash
sudo /var/lib/redundanet3/bin/redundanet backup add NAME FOLDER
sudo /var/lib/redundanet3/bin/redundanet backup run NAME
sudo /var/lib/redundanet3/bin/redundanet backup list
```

Runs are daily from then on. The old Tahoe shares count only against the old grid, so your native allocation starts empty; a member who needs room during the overlap can draw on the reserve or a sponsor, recorded in the manifest by the maintainer. Both grids share one disk: the native store refuses writes below its floor, and the status page shows free space per server.

## 6. The restore test

A restore test reads a sample of a snapshot back and compares it to the source. The agent runs one weekly; ask for one now and read the result:

```bash
sudo /var/lib/redundanet3/bin/redundanet backup verify
sudo /var/lib/redundanet3/bin/redundanet backup list
```

`migrate-node` refuses to retire the stack until the folder `backup-sync` pushed is held by a native set with a complete run and a passed restore test on that device. Doing a real restore of something you care about is a good habit too: [backups.md](backups.md).

## 7. Retire the Docker stack

Not before the stop date is announced: your node's Tahoe shares hold other members' data too, and the old grid is 2-of-4. Until then, a drill is fine: `migrate-node` followed by `--rollback` within a few minutes, inside the old grid's tolerance of two servers down.

```bash
sudo /var/lib/redundanet3/bin/redundanet migrate-node --dry-run   # the plan, and what is not yet on the native grid
sudo /var/lib/redundanet3/bin/redundanet migrate-node             # stops and removes the containers; files and shares stay
sudo /var/lib/redundanet3/bin/redundanet migrate-node --rollback  # every service again, together, tinc first
```

What it does: the plan lists the stack's containers by service and every compose profile, then one `docker compose down` through the installed compose file, its override and `.env`, with every profile named, so the Tahoe services go too, every container at once and tinc last. A container that survives puts the stack back and fails the command, since it would run on tinc's dead network namespace. The compose files, the `.env` and the Tahoe share directories stay on disk. The agent keeps running through all of it and the node stays on the status page. `--force` retires the stack although something is not yet on the native grid, and the plan names what is lost. `--rollback` recreates every recorded service in one `up`, tinc first, never one container at a time. The Python CLI stays installed but idle until you remove it.

## What the stop date means

The wind-down is announced by hand; no countdown and no automatic deletion exist yet. When the maintainer announces the date, the plan is the RFC's: on that date the remaining Tahoe servers stop and their share directories are deleted, and the space returns to the native grid. What a member never moved to a native backup set is gone from the grid, not from their own disks. Until the date, every member's Tahoe shares stay where they are, and the old hard rules in [legacy/README.md](legacy/README.md) still apply to the old stack.

## Lost something?

Lost the phrase: a new identity is a new member entry; the maintainer retires the old one. Lost a node: certify a new one, and revoke the old device with `redundanet device revoke --device KEY` on any device of yours ([security.md](security.md)).
