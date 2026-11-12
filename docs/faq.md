# Frequently asked questions

**Does it run on Windows?**
A Windows build is provided and passes the test suite, but it has not run on the live network yet. The same holds for the Intel Mac build. Tested on Linux (x86-64 and ARM64, including Raspberry Pi 5) and Apple silicon Macs. The Windows binary is unsigned for now; the one-line installer for Windows checks it against the signed release index, and so can you ([install.md](install.md)).

**What happens if the hub is down?**
There is one hub today. With it down there are no new connections, no repair scheduling and no status page; existing direct connections between devices continue. Your data is on the storage servers, not on the hub, and nothing on the hub is needed to recover it.

**Where is my data?**
On other members' storage servers, as encrypted, erasure-coded shares: every object is split into n shares of which any k rebuild it (2-of-3 on the live network today), placed across members. The status page shows the servers and the grid's health; `redundanet object check --network redundanet CAP` from one of your devices prints which servers hold an object's shares.

**Can the maintainer, or a storage server, read my files?**
No. Every object is encrypted on your device with a key derived from your member secret and the content. Servers and the hub see ciphertext, sizes, your member key and timing; never names or contents. What exactly they see is in [../PRIVACY.md](../PRIVACY.md).

**Is my phrase still right?** Run `redundanet identity check` on any of your devices and type it: the command says whether it derives the member key this device was certified under, and writes nothing. Do it once after writing the phrase down.

**How do I add a second laptop?** `redundanet identity pair` on the new one shows a code; `redundanet device pair` on a device that already has your identity asks for that code and your phrase, certifies it and hands over the network. The phrase is never typed on the new device. See [install.md](install.md).

**What if I lose the phrase?**
The data is unreadable to everyone, by design; there is no escrow and nobody can recover it for you. A new identity is a new member entry, and the maintainer retires the old one. Keep the phrase like a passport. Seal the identity file with `--passphrase` if the device is shared.

**What if I lose a device?**
Revoke it from any other device of yours: `redundanet device revoke --device KEY`. The agent sends the signed record to every storage server and hub within seconds, and the maintainer adds it to the manifest for good; the device is refused from then on. Until then it can read what your devices can read and write as that device, but anything it deletes stays recoverable for 30 days. Revoking stops the device; it does not change the secret that device held, which reads your data and is the same on all your devices. What that leaves open, and when a new identity is the answer: [security.md](security.md), "What a lost device can do".

**How much space do I get?**
The local page and `redundanet backup list` show "allocation: X of Y GB used"; a run is refused once the allocation is used up, and says so.
Your allocation is your contribution, less the network's reserve, times k/n. With the live network's 15 percent reserve and 2-of-3, 1000 GB contributed gives 566 GB. `redundanet validate --manifest manifests/manifest.yaml` prints every member's allocation; the status page shows what you use against it. A member without a storage device gets an allocation from a sponsor or the reserve, set by the maintainer.

**How do I add a second laptop?**
Certify it from the phrase with the three identity commands in [join.md](join.md), "More devices". No manifest change is needed. There is no pairing without the phrase in 3.0.0.

**How do updates work?**
The agent checks the signed release index every hour, installs only what the index names after verifying the hash, runs the new version in probation for up to two minutes, keeps the old binary as `redundanet.old` and rolls back on its own if the new one does not reconnect. `redundanet update check` and `redundanet update apply` do it by hand. Channels: `stable` (the default) and `early`.

**How do I leave?**
Tell the maintainer. You are marked leaving in the manifest; servers keep your shares for the grace period (30 days by default) and then drop them. Then `redundanet service uninstall` and remove the data directory. Your member name and key stay in the repository's history.

**Something is wrong. How do I report it?**
Run `redundanet diagnostics`. It writes a zip with versions, the manifest revision, the service state, hubs and peers as counts, your backup sets with folders as label plus hash, restore tests, error kinds and counts, a disk check and the last log lines with paths, URLs and addresses replaced (by pattern: have a look at `log.txt` in the zip first). It works with the agent down. Open an issue on GitHub and attach it by hand; nothing is ever uploaded automatically.

**How do I know my backups are good?**
Every week the agent restores a sample and compares it to the source; `redundanet backup list` shows the result and the streak, and so does the status page. `redundanet backup verify` runs a test now. A real restore to a scratch folder with `redundanet restore start` is the proof that counts.

**Is there a bandwidth cap or a settings page?**
Not in 3.0.0. The schedule and the retention policy are edited in `backup.yaml` by hand ([configuration.md](configuration.md)).

**I was a RedundaNet 2 member. What must I do?**
Create your identity on your laptop, put the agent on each node, send the join request, make native backup sets for what `backup-sync` pushed to the old grid, pass a restore test, and retire the Docker stack with `migrate-node`. Step by step: [migration.md](migration.md). RedundaNet 2 ended on 13 November 2026: its hubs are shut down and the old grid can no longer be read or written.

**Why are there two programs called `redundanet`?**
The Python package `redundanet` on PyPI is the RedundaNet 2 command line tool, which ended with that product. RedundaNet 3 is one Go binary from the GitHub releases. On a node that ran RedundaNet 2 both exist: the Python one on PATH as `redundanet`, the Go one as `redundanet3` (`/usr/local/bin/redundanet3` for you, `/var/lib/redundanet3/bin/redundanet` for the service).
