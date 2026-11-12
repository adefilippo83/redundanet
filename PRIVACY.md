# Privacy policy

This page says what the RedundaNet software sends, to whom, and what stays on
your device. It describes what the code does today. The code is the
reference; this page is kept beside it and changes with it, and git shows
what changed and when.

RedundaNet is run by its members, not by a company. There are no accounts, no
analytics, no crash reporting and no telemetry: the project collects nothing
about you. The software talks only to the network you joined and to the
places it fetches its own updates from. The rest of this page lists exactly
those exchanges.

## The project collects nothing

- No account is created anywhere to use RedundaNet.
- Nothing in the software reports usage, errors or statistics to the
  maintainers.
- The local page and the command line talk only to the agent on the same
  computer: it listens on 127.0.0.1 and asks for a token kept in the data
  directory.
- The diagnostics bundle (`redundanet diagnostics`) is written to a file on
  your computer with every path, URL and address replaced. It is never
  uploaded. You send it by hand, to whom you choose.
- The website redundanet.com is a static site on GitHub Pages. It loads no
  analytics and no third-party scripts, and stores only your language choice
  in your browser. Its join form builds a GitHub issue in your browser;
  nothing leaves the page until you open that issue on GitHub. GitHub serves
  the site and GitHub's privacy statement applies to its server logs.

## Who the software talks to

A RedundaNet network has three kinds of parties, all of them members or
volunteers of that network:

- Hubs: a few always-on machines listed in the manifest, run by the network's
  operators. They introduce devices to each other, relay traffic between
  devices that cannot reach each other directly, collect the census and
  publish the public status page.
- Storage servers: members' devices that contribute disk. They hold encrypted
  shares of other members' backups.
- The source repository on GitHub: the manifest (who is a member) and the
  release index (what the current version is) live there.

### What a device sends to the hubs

Every minute the RedundaNet 3 agent sends each hub a heartbeat with:

- the software version, operating system and architecture, uptime, roles and
  the manifest revision in force;
- for a storage server: free and total bytes of the contributed disk, its
  state, the number and bytes of shares held, the date of the last integrity
  check and the write floor;
- which other devices it currently reaches, by device key, and whether
  directly or through a relay;
- the self-updater's last word;
- the date of the last backup and whether it was partial, the date of the
  last restore test, whether it passed, and how many weeks in a row tests
  have passed;
- whether it volunteers as a repair worker.

A storage server also sends the hubs its share census: for every share it
holds, the storage index, share number, owner member key, size, encoding and
descriptor hash, plus the tombstones of deleted objects. The hub uses it to
compute placement health and to schedule repairs.

Because the device connects to the hub, the hub also sees the public IP
address and port the connection comes from. It uses them to introduce the
device to its peers and writes them in its own log. When two devices of a
member pair through the hub, the hub relays their sealed frames for a few
minutes and sees only the two addresses and that a pairing happened, not
the keys or the identity inside. Hub logs and the hub's
placement table (which server holds which share, with sizes and times) stay
on the hub operator's machine, under that operator's retention.

### What the hubs publish

Each hub publishes a public status page and a `status.json` beside it. Per
device they show: the device name from the manifest, the device key, the
member name, online state, how it connects and since when, when it was last
seen, software version, operating system and architecture, uptime, manifest
revision, free and total bytes, storage state, number of shares and bytes
stored, how many peers it reaches directly or through a relay, and relayed
byte counts. Per member: name, status, contribution and allocation, devices
and how many are online, bytes stored, object counts, the day of the last
backup and the restore-test streak. Plus grid totals and repair counters.
IP addresses are not published.

The status page is served by the hub itself at hub.redundanet.com, a
server rented from Hetzner in Germany. Its web server sees the IP address
of whoever opens the page, as any web server does, and keeps no access
log. The RedundaNet 2 page stays on fly.io until the old grid's stop date.

### What a storage server receives

Backups never leave your device in the clear. Each file is split into
objects; each object is encrypted on your device with a key derived from your
member secret and the object's own content; the ciphertext is erasure coded
into shares, and only shares go to storage servers. File names, folder
structure and the index of your snapshots are themselves stored inside
encrypted objects.

With each share a server receives a descriptor signed by your device: the
storage index, size, segment size, encoding parameters, cipher, hashes of the
ciphertext and of the shares, and the owner member key. The server records
those fields per share, together with when the share was created, last
touched and last verified, and the delete tombstones you issue. So a storage
server, and the hub, learn that your member stores an object of a given size
at a given time, and how much you store in all. They cannot read names or
contents.

Your device's root, the pointer to your latest snapshot index, is stored on
the servers as an encrypted envelope.

Other nodes may repair your objects without you: they rebuild and move shares
from ciphertext alone, never with a key.

### Connections between devices

Devices talk to each other over QUIC with TLS 1.3, authenticated by their
device keys. When two devices connect directly, each sees the other's IP
address. When they cannot, a hub relays the packets; the relay forwards
end-to-end encrypted packets it cannot read, and counts the bytes.

### The manifest and the repository

Membership is a signed file in the public git repository,
`manifests/manifest.yaml`. Joining means publishing in it: your member name
(a GitHub handle), your member key, your contribution and allocation, the day
you joined, your status, and, for members migrated from RedundaNet 2, the PGP
fingerprint and the signature attesting the migration; your device names and
keys; the hubs' names, keys and addresses. RedundaNet 2 node entries also
carry host names, VPN addresses, public IP addresses, GPG fingerprints, ports
and regions. The repository is public and git keeps every revision: a member
can be removed from the current manifest, but earlier revisions remain in the
history.

Devices fetch the manifest and its signature from the repository every five
minutes, check the release index and its signature on GitHub every hour, and
download new releases from GitHub releases. GitHub sees the device's IP
address and the user agent `redundanet/3`; no identifier is sent.

Applications to join are made on GitHub, in issues, discussions and pull
requests, and are public there under GitHub's terms.

## RedundaNet 2, the Docker stack

Nodes of the RedundaNet 2 stack:

- join a Tinc mesh VPN: every node learns the public IP addresses of the
  nodes it connects to, which are also listed in the manifest;
- store files with Tahoe-LAFS: storage servers hold ciphertext shares and see
  share sizes and lease times; the hub runs the introducer that tells nodes
  where the servers are;
- serve two endpoints over the VPN that the hub reads for the status page: a
  share census (the opaque storage indexes the node holds) and a usage report
  (bytes the member occupies against the allocation);
- pull the manifest from GitHub every few minutes, and container images from
  ghcr.io when updating.

The RedundaNet 2 status page shows per node: name, VPN address, manifest
status, reachability, round-trip time, uptime over 24 hours, 7 days and 30
days, roles and whether storage is listening, plus grid, replication and
quota figures. The hub keeps minute samples for about three days and hourly
rollups for 90 days. That hub runs on fly.io, whose logs and terms apply to
the machine.

## What stays on your device

- Your identity, sealed with a passphrase, your member secret and the
  network descriptor.
- A local database of the files in your backup sets: paths, sizes,
  modification times, hashes and the capabilities to read the objects back;
  and the record of your restore tests.
- The service log, with operational messages that include peer addresses.
- The backup configuration: sets, folders, excludes, schedule.

None of it is sent anywhere. Removing the data directory removes it all; the
`redundanet service uninstall` command removes the service.

## Retention and deletion

- An object you delete is tombstoned on every server. The tombstone takes
  effect after 7 days of immunity on the server's clock, the share data is
  purged 30 days after the tombstone, and the record is dropped 90 days
  after the purge.
- A member who leaves is removed from the manifest; servers keep that
  member's shares for the grace period the manifest sets, then drop them.
- Hub placement tables and logs follow the hub operator's retention.
- The manifest's git history is permanent.

## Children

RedundaNet is a tool for people running their own machines. It is not
directed at children and collects no data from anyone.

## Contact and changes

Questions about this policy go to the maintainers through the repository's
issues. This page is kept with the code: every change is a commit, and the
history shows what changed and when.
