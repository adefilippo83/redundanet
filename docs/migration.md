# Moving from RedundaNet 2 to RedundaNet 3

This page is for a former member of the RedundaNet 2 grid: the Python command line tool, Docker Compose, the Tinc VPN and Tahoe-LAFS. RedundaNet 2 ended on 13 November 2026: its two hubs, which were the VPN entry points and the Tahoe introducers, were shut down. The old grid no longer connects, `backup-sync` no longer uploads, and what was stored only there can no longer be read through it. Your own files are on your own disks as before. The steps below add your RedundaNet 3 identity, put the agent on each node, back up natively, prove a restore, and retire what is left of the Docker stack.

On a node that ran RedundaNet 2 both programs exist: the Python `redundanet` on PATH is the old tool, the RedundaNet 3 command is `sudo redundanet3`, a copy of the program that root owns at `/usr/local/bin/redundanet3` (the service runs its own at `/var/lib/redundanet3/bin/redundanet`).

## 1. Your identity, on your laptop

Install the agent on your laptop ([install.md](install.md), one line):

```bash
curl -fsSL https://redundanet.com/install.sh | sh
```

In the page it opens (or in `redundanet setup` in the terminal), choose the first case, a new identity. It shows a twelve-word phrase once. Write it down and keep it like a passport: it is your member key, and every device you ever certify comes from it. It prints your member key (52 characters) and this laptop's device key, installs the service and opens the join form of step 3 with the key filled in; close it for now, the nodes of step 2 go in too. The network is built into the binary; there is nothing to join by hand.

## 2. The agent on each node

Follow [install.md](install.md), "Linux storage node": one line as root on the node lays the agent out and shows a pairing code, and `redundanet device pair --storage` on your laptop, on the node's network or anywhere (the hub relays the exchange), asks for that code and certifies it; the node joins and starts `redundanet-agent.service` by itself. For a node your laptop cannot reach, the runbook's certificate file over SSH does the same ([../agent/deploy/node/README.md](../agent/deploy/node/README.md)). The agent runs on the host beside the Docker stack, on the host's network, with no access to the containers. From then on it updates itself from the signed release index with probation and rollback, and the node appears on the RedundaNet 3 status page under your name once the manifest lists it.

Do this on every node before the join request, so each node's device key and name can go in it.

## 3. The join request

Fill the form at <https://redundanet.com/join.html> as a new member does ([join.md](join.md), step 4), from the GitHub account your old nodes were listed under: the workflow gives the name of a former RedundaNet 2 member only to a request filed from that account. Nothing else tells you apart from a new member; the GPG signature the earlier form asked for is no longer used.

Your contribution in GB is what your nodes contribute. The workflow opens the pull request adding you under `members:` and your nodes under `storage:`, the maintainer signs and merges, and every agent applies the new revision within five minutes.

## 4. Native backup sets

The old grid received what `backup-sync` pushed from your node, and receives nothing any more. Make a native backup set for each of those folders, on the device that holds them. On a node:

```bash
sudo redundanet3 backup add NAME FOLDER
sudo redundanet3 backup run NAME
sudo redundanet3 backup list
```

Runs are daily from then on. Your native allocation starts empty. The old Tahoe shares still take room on the node's disk until you delete them (step 6): the native store refuses writes below its floor, and the status page shows free space per server.

## 5. The restore test

A restore test reads a sample of a snapshot back and compares it to the source. The agent runs one weekly; ask for one now and read the result:

```bash
sudo redundanet3 backup verify
sudo redundanet3 backup list
```

`migrate-node` refuses to retire the stack until the folder `backup-sync` pushed is held by a native set with a complete run and a passed restore test on that device. Doing a real restore of something you care about is a good habit too: [backups.md](backups.md).

## 6. Retire the Docker stack

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
