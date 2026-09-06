# Quotas: contribution-based allocation

RedundaNet has no central operator selling space. Members bring disks, and
what each member may store follows from what they contribute. This page
describes the model, how it is measured, and exactly what is and is not
enforced.

## The model

Tokens are gigabytes contributed. Every storage node declares a
`storage_contribution` in the manifest; a member's tokens are the sum over
their nodes. Under k-of-n erasure coding each stored byte occupies n/k bytes
of grid capacity, so

    allocation = contribution x k/n x (1 - reserve)

| Contribution | Encoding | Expansion | Allocation (reserve 15%) |
|---|---|---|---|
| 500 GB | 1-of-2 | 2.0x | 212.5 GB |
| 500 GB | 2-of-4 | 2.0x | 212.5 GB |
| 500 GB | 3-of-10 | 3.3x | 127.5 GB |

The reserve leaves room for what the grid holds beyond live files: shares of
deleted files until their leases expire (up to the lease duration, 90 days by
default), and the rebalancer's transition when the encoding changes and old
and new shares coexist for a while.

Changing the encoding changes every allocation. Moving from 1-of-2 to 2-of-4
keeps the expansion at 2x, so allocations stay the same; moving to 3-of-10
shrinks them, and a member above the new allocation has to delete data or
contribute more.

## Members

Allocation and usage are per person, not per node. A node entry names its
owner with `member:` (the applicant's GitHub login, set automatically by the
join bot); a node without one is its own member. Several nodes with the same
member pool their contributions and share one allocation.

## What is measured, and by whom

**Contribution** is declared in the manifest and verified by the hub: every
storage node's census reports the size of its storage disk, and a node whose
disk is smaller than its declared contribution counts as what it actually has
(the status page flags it).

**Usage** is measured on the client side. Tahoe storage servers cannot tell
whose bytes they hold: shares carry no owner and lease secrets are hashed per
file and per server, by design. But every client knows exactly what it stores,
and every capability string carries the file's size and its k and n. The
usage meter in the client container walks the client's aliases every 15
minutes and sums, per file, size x n/k with the file's own encoding. That sum
is the member's real footprint on the grid. It is served over the VPN to the
hub, which shows every member's contribution, allocation and usage on the
public status page, and written locally for the enforcement points below.

Files held only as bare capabilities, outside any alias, are invisible to the
meter, the same way they are invisible to lease renewal and repair.

Backup snapshots are counted correctly: `tahoe backup` links a new
`Archives/<timestamp>` on every run, but unchanged files keep the same
capability, and the meter counts each capability once, which is what the grid
actually stores.

## What is enforced

With `enforce: true` in the manifest's `network.quota` section:

- `redundanet storage upload` refuses an upload that would push the member
  over its allocation (`--ignore-quota` overrides; the overuse stays visible).
- The backup sync pauses its runs while the member is over allocation and
  resumes on its own once usage drops. The Samba share keeps working; only the
  copy into the grid waits.

What is not enforced, and cannot be with Tahoe as it is: writes that bypass
the RedundaNet client, i.e. the SFTP frontend (Tahoe's own, it writes straight
into the grid) and raw `tahoe` commands. A member who does that is not
stopped, only seen: their usage is on the status page for everyone. In a
community of vetted members that visibility is the enforcement, and it is
stated openly rather than pretended otherwise.

With `enforce: false` (the default for a manifest without the section)
everything is measured and shown, nothing is refused.

## Commands and files

```bash
redundanet storage quota            # this member's allocation, usage, remaining
redundanet storage quota --fresh    # measure now instead of the meter's last report
```

The meter's last report is `/var/lib/tahoe-client/redundanet-usage.json`
inside the client container, and `GET http://<vpn-ip>:3460/usage` over the
VPN. The hub aggregates the reports per member into `status.json` under
`quotas`.

## Manifest fields

```yaml
network:
  quota:
    reserve: 0.15      # fraction of the contribution kept back (0 to 0.9)
    enforce: true      # client-side refusal over allocation
nodes:
  - name: node-2680cd08
    member: adefilippo83          # groups this node's contribution with the member's others
    storage_contribution: 900GB   # tokens: decimal GB/TB like disk labels
```

Environment overrides on a client node (`/opt/redundanet/.env`):
`USAGE_INTERVAL` (seconds between measurements, default 900) and
`QUOTA_ENFORCE=true|false` to override the manifest's setting locally.

## Known limits

- Tahoe spreads shares evenly across servers regardless of their size, so a
  grid of unequal disks fills its smallest server first. Summed contributions
  slightly overstate the usable total; the reserve absorbs some of it.
- Usage is at most one measurement interval stale; a burst of uploads within
  15 minutes can overshoot the allocation before the meter notices.
- Real server-side enforcement (storage servers demanding tokens per byte)
  would need Tahoe's accounting or a spending-pass plugin; that is a separate
  project, deliberately not started.
