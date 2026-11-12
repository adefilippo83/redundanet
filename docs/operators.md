# Operating the network

This page is for the operator of a RedundaNet 3 network: the person who holds the operator key and signs the manifest. On the live network that is the maintainer, Alessandro De Filippo ([@adefilippo83](https://github.com/adefilippo83)). How the network is governed is in [../COMMUNITY.md](../COMMUNITY.md); the keys and rules themselves are in [configuration.md](configuration.md).

## The operator key

```bash
redundanet operator init --key operator.key
```

Makes the offline operator key and prints the public half for the manifest's `operators` list. The key never goes on a server or into CI. The operators list changes only through a manifest the current keys signed.

## Editing the manifest

Every change to membership is a pull request against `manifests/manifest.yaml`: a join request's workflow opens one, for a new member and for a listed member's additional storage node alike (the latter proves itself with the node's certificate, which the workflow verifies with `redundanet identity verify-certificate` against the member key in the manifest); the operator edits by hand for everything else. The rules:

1. Raise `revision` by one. An agent applies only a revision above the last one it applied, with a valid signature.
2. Run the validator before and after:

   ```bash
   redundanet validate --manifest manifests/manifest.yaml
   ```

   It parses the v3 half (the RedundaNet 2 keys are set aside, anything else unknown is an error), applies the rules, prints errors, warnings and the allocation per member, and exits 1 on errors. CI runs it on every pull request.
3. Sign offline, on the laptop with the operator key, one command for a pull request:

   ```bash
   scripts/sign.sh manifest PR        # a number or the branch, for example join-request/42; no argument signs the current branch
   ```

   It builds the signing tool from main, checks out the pull request, shows the manifest diff, refuses a revision that does not rise, validates, asks, signs, pushes `manifests/manifest.sig`, waits for the checks and merges. `--dry-run` stops before signing; the key file is `~/.redundanet/operator.key` unless `OPERATOR_KEY` names another. By hand it is:

   ```bash
   redundanet manifest sign --key operator.key --manifest manifests/manifest.yaml
   ```

   which validates, then writes `manifests/manifest.sig`: the operator's signature over the network, the revision, the time and the file's hash. Commit the manifest and the signature together. CI shows the signature check red until the signature matches the file, which is why the operator signs the branch before merging.
4. Every agent fetches the pair within five minutes and applies it at once; devices the new policy no longer admits are disconnected.

Anyone can check a signature:

```bash
redundanet manifest verify --manifest manifests/manifest.yaml [--sig manifests/manifest.sig] [--trust KEY,...]
```

Without `--trust` it uses the operators listed in the manifest itself, which proves consistency, not authenticity; agents pin the keys from the descriptor they joined with. The output also says when the signature is past `signature_max_age_days`: known devices keep working, new ones wait for a re-sign.

A manifest that is broken on main does not reach an agent: it keeps its last good copy until a valid signed revision appears.

## Admitting a member

The join form opens an issue; the workflow validates it and opens the pull request that adds the member under `members:` and their node under `storage:`. Review: the member key in the request against the issue, the storage device, the contribution. The name of a former RedundaNet 2 member, one the manifest's `nodes:` still lists, is given only to a request filed from that GitHub account; the workflow refuses any other. A client-only member gets `contribution_gb: 0`, an `allocation_gb` and a `sponsor` (a member, or `reserve`); the validator checks that the sponsor covers it. Then sign and merge.

## A member leaving

Set `status: leaving` with `leaving_since: YYYY-MM-DD`, raise the revision, sign, merge. Servers note the time and purge that member's shares and roots once `grace_days` (30 by default) have run on their own clocks with the manifest still saying so; a member who reappears as active before that clears the note. Remove the entry after the grace period. The repository's history keeps every revision.

## Revoking a device

A member revokes a lost or stolen device from any other device of theirs:

```bash
redundanet device revoke --device KEY
```

It asks for the phrase, signs a revocation record, and the running agent applies it and sends it to every storage server and every hub; the hubs keep it, send it to every connected device at once and to each device that connects later. A hub or a device keeps one record per certificate (the earliest date), only for a member the manifest lists as active or leaving, and at most 256 for one member; a record for a member the manifest does not list yet waits in memory until a manifest lists that member. The member also sends you the printed record: add it to the manifest under `revocations:`, raise the revision, sign and merge, so it survives every later manifest and reaches whoever was unreachable. Signatures the device made before the revocation stay valid, so what it wrote keeps verifying and repair keeps working.

## Changing k-of-n

Declare the transition beside the new encoding:

```yaml
network:
  encoding: {k: 2, n: 3}
  encoding_transition: {from: {k: 2, n: 4}, to: {k: 2, n: 3}, deadline: 2026-12-03}
```

`validate` refuses a transition whose `to` is not the encoding and warns once the deadline has passed. Until the deadline every server accounts shares under the old encoding at zero when it decides on a share under the new one, so a member near budget converts without deleting first. A new share under the old encoding gets no such exemption: it is measured against everything the member holds, so the old encoding is not a way around the quota. Each device converts what its own snapshots reference, every six hours on its own and on `redundanet backup rebalance`; the status page shows the conversion as a bar, with the old encoding's bytes and whether they count yet, and per member the objects still to convert. Once every member has converted, remove the transition, raise the revision, sign. Encoding is per object at upload time: nothing is re-encoded except by the owner's devices.

## Grace, minimum version, signature age

- `grace_days`: how long servers keep a leaving member's data. Deletion waits for it on every server's own clock, which is also the time a signed manifest nobody merged would be noticed.
- `min_version`: the lowest agent version admitted. The self-updater installs a release that satisfies it, so raise it only to a version the index already carries on the `stable` channel. A device on which that version failed probation tries it once more for each manifest revision signed while `min_version` requires it, also when it was waiting for `redundanet update retry`: raising the revision is how you ask every device again.
- `signature_max_age_days` (90 by default): re-sign before it runs out, even with no change, or new devices wait.

## Releases

Releases are the maintainer's: tag `agent/vX.Y.Z`; the workflow builds it and opens a draft release; `scripts/sign.sh release X.Y.Z` on the laptop with the release key checks it again, builds it again from the tag and compares every binary with the release, signs, merges and publishes the draft. To withdraw a release, set `withdrawn: true`, raise the revision, sign again. All of it: [../releases/README.md](../releases/README.md). Nothing in the manifest can name a release.

## The hub

The live network has one hub, `hub-hetzner` (relay `hub.redundanet.com:8443`, UDP and TCP), listed under `hubs:` by its device key. With it down there are no new connections, no repair scheduling and no status page; existing direct connections continue. Install, firewall, checks and the hub's self-update are in the hub runbook, [../agent/deploy/hub/README.md](../agent/deploy/hub/README.md). The hub updates itself like a node, with probation on the same ports; a candidate that does not report healthy in time is tried again after 24 hours, twice; after a third failure, or when the candidate exits by itself, it waits for `redundanet3 update retry VERSION`, run as root on the hub.

The hub serves its status page and `status.json` itself at <https://hub.redundanet.com/>, through Caddy on the same host with a certificate it renews on its own; the hub runbook has the setup. It depends on nothing outside its host for that.

The network descriptor (name, hubs, operator keys, where the signed manifest is served) is built into every binary, as `Network` in `agent/internal/version/version.go`, so members never see it: every `--network` flag defaults to its name and a device that never joined by hand is seeded from it. It is printed from the manifest:

```bash
redundanet network descriptor --manifest manifests/manifest.yaml --repository https://raw.githubusercontent.com/adefilippo83/redundanet/main
```

When the hubs or the operator keys change, put the new descriptor in `version.go` in the same change; a test in `agent/cmd/redundanet` fails until it matches the manifest. Devices on the built-in network follow it with the next release; a device that joined with a descriptor given by hand keeps that one until `redundanet network join` (no argument, back to the built-in one) or a new descriptor is run on it.

### Changing the operator keys

There are two ways, and they are for different days.

**An ordinary change** goes through the manifest: a manifest signed by a current key lists the operators of the next one. Add the new key first and remove the old one in a later revision, or do both at once; either way the device keeps the list it has across restarts, also when the key that signed it has left. Put the new descriptor in `version.go` in the same change. When a release brings a descriptor whose keys the devices already trust through the manifest, nothing more happens.

**A stolen key** cannot be removed that way, because the thief can sign manifests too. The way out is the descriptor: a device takes its operator keys from a descriptor (the one built into a release, or one given to `network join`) when the descriptor's keys differ both from the keys of the descriptor it had and from the keys it trusts now. From that moment:

- only the descriptor's keys sign for that device, and the keys it dropped stay out: a manifest that lists one of them as an operator is refused, whoever signed it;
- the member list it has stays in force, so it keeps working, and its revision stays the floor;
- the next manifest must be signed by a new key and carry a higher revision. `redundanet network status` shows `awaiting_operators` until it arrives.

So, in this order:

1. Sign a manifest with the new key, with a higher revision, listing only the keys that remain, and publish it. Devices on the old release refuse it and stay on the list they have.
2. Put the new descriptor in `version.go` and release. Each device that updates drops the stolen key and takes the manifest from step 1 at its next fetch.
3. A device that joined with a descriptor given by hand needs the new descriptor sent to it, for `redundanet network join <descriptor>`.

If step 1 is skipped the release still installs and the devices keep the old list, but nothing new reaches them until a manifest from a new key is published. Between the theft and step 2 the thief's manifests are accepted by any device they can serve them to, which takes control of the repository address as well as the key. A device the thief served a manifest with a very high revision keeps that list until a new key signs a higher one. A failed update that puts the previous binary back returns that device to the old keys until a release with the new descriptor installs on it.

## Repair, by hand

The hub schedules repair of under-placed objects without the owner: a task signed by its device key goes to a storage node that volunteers, which fetches k shares, regenerates the missing numbers without the key, checks them against the signed share roots and stores them where the task says. The status page shows objects per number of copies, under-placed, unreadable and due; "blocked: X short above the floors" means no live server has room, and the community needs to add disk.

To drill it on a live network, drop one share on purpose, as the service user beside the running agent:

```bash
sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet storage drop --storage-path /mnt/storage STORAGE_INDEX SHARE_NUMBER
```

The census reports the loss within seconds and the hub issues a task at once. `redundanet object check CAP` from any device of the owner prints the storage index and the holders to pick from.

## Diagnostics from a member

Ask the member for `redundanet diagnostics`: a zip with versions, the manifest revision, the service state, hubs and peers as counts, the backup sets with each folder as label plus hash, restore tests, error kinds and counts, a disk check and the last log lines with paths, URLs and addresses replaced by pattern (a path whole, by a short hash; the host name of a Linux journal is left out). It works with the agent down. Nothing is ever uploaded automatically; the member sends it by hand.
