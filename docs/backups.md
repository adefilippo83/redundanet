# Backups and restores

The agent does the work; `backup` and `restore` ask it through the local API, so the service must be running. On a laptop the command is `redundanet`; on a storage node it is `sudo redundanet3`, the copy root owns, which finds the node's data directory on its own.

## Backup sets

A backup set is a name and one or more folders:

```bash
redundanet backup add NAME FOLDER [FOLDER...] [--exclude a,b]
redundanet backup list
redundanet backup remove NAME
```

`--exclude` adds base-name globs or relative path prefixes beside the defaults. The defaults are caches, the systems' own folders and the cloud providers' databases, on every platform at once: `.DS_Store`, `.Trash`, `.Trashes`, `Library/Caches`, `.Spotlight-V100`, `.fseventsd`, `.TemporaryItems`, `.DocumentRevisions-V100`, `Thumbs.db`, `desktop.ini`, `$RECYCLE.BIN`, `System Volume Information`, `pagefile.sys`, `hiberfil.sys`, `swapfile.sys`, `~$*`, `AppData/Local/Temp`, `AppData/Local/Microsoft/Windows/INetCache`, `.cache`, `.dropbox.cache`, `*.partial`, `*.crdownload`.

On macOS, `backup add` probes each folder as the service and refuses one the system guards (Documents, Desktop, Downloads) until the agent has Full Disk Access; it names the setting to change. A folder that becomes unreadable later shows the same line in `backup list` and on the local page.

## Schedule

Each set runs daily, the first time two minutes after the agent starts. `redundanet backup run [NAME]` starts a run now; `backup list` follows it. The interval is `every` in `backup.yaml` ([configuration.md](configuration.md)). One run at a time.

## Settings

The local page has a Settings card, and every setting has a command:

```
redundanet backup schedule --every 24h --verify-every 168h --cleanup-every 168h
redundanet backup retention --days 7 --daily 30 --weekly 12 --monthly 12
redundanet backup exclude NAME --exclude "*.iso,cache/"
redundanet backup limit --upload 2000 --download 0
redundanet update channel stable
```

Backups run at most hourly, restore tests and cleanups at most daily; a changed schedule takes effect at once. Retention keeps everything for the first days, then one snapshot a day, a week, a month. Excludes are base-name globs or relative prefixes beside the defaults, per set. The channel choice is kept on the device and wins over the way the service was installed.

## Your allocation

Before every run the agent asks the storage servers what they hold for you and compares it, as the data it stands for, with the allocation the manifest gives you. The local page and `backup list` show "allocation: X of Y GB used". When the allocation is used up a run is refused and says so: prune snapshots with a shorter retention, or ask the operator for a larger allocation. The servers enforce a bound with headroom as well; this is the exact budget on your side. A server's answer counts for at most what the manifest lets that server hold for you, so one server that reports too much cannot use up your allocation by itself, unless it is a large part of the whole network's storage.

## Bandwidth

The agent can cap what the device sends and receives over the mesh, in kilobytes per second, so a backup never takes more than its share of a home connection:

```
redundanet backup limit --upload 2000 --download 0
```

Caps apply at once, survive restarts, and show in `backup list` and on the local page; 0 means no cap. A short burst after idle time is allowed, one second's worth. Restores, restore tests and repair traffic are paced too, since the cap is on the device's mesh streams, not on the backup job.

## What a run does

The agent walks the folders, honouring the excludes. A file it has seen (same path, size, modification time, change time and file id) costs nothing; a changed or new file is hashed, and content the member already stored on any device costs no upload. Whether it is stored is not taken on a server's word: a server must produce the object's descriptor, signed by one of the member's devices, and each server is believed for no more shares than placement gives one server; where that does not add up to enough shares, the file is uploaded. Each run also re-hashes a rolling share of unchanged files, so a file that defeats every timestamp is still caught. Every object is encrypted on the device, erasure coded and sent as shares to storage servers across members. The run ends by publishing a signed root that names the new snapshot. It starts by reading the device's current root, and it does not start on an unsure reading: when too few storage servers answer to be certain of the newest root, or when they return an older root than the device itself last confirmed, the run stops with that reason and the next one tries again. Building on an older root would publish a snapshot list without the snapshots made since. The weekly cleanup applies the same rule.

A run that is interrupted still publishes what it had as a partial snapshot, marked as such, and continues next time.

## Skipped files

A file that cannot be read (locked, permission denied, vanished mid-read) is skipped, counted, listed with the reason and retried next run; it never fails the snapshot. Cloud placeholders are skipped and counted, never pulled down: OneDrive online-only files on Windows and evicted iCloud Drive files on macOS are listed as "not on this computer". Devices, sockets and FIFOs are skipped too. Symlinks are stored as their target text and never followed. `backup list` counts skipped files by reason, the local page shows the same, and `restore browse` shows each one's reason.

## Snapshots and retention

Every complete run is a snapshot of the set. Retention keeps everything for 7 days, one per day for 30 days, one per week for 12 weeks and one per month for 12 months. A partial snapshot is kept like any other until a complete one of the same set and day exists. The policy is edited in `backup.yaml` under `retention`; there is no settings page in 3.0.0. Pruning drops a snapshot from the index; the objects only it referenced go with the next cleanup.

## Restores

From any device of the member, including a fresh one certified from the phrase:

```bash
redundanet restore list                                   # every device's snapshots, from the servers' roots
redundanet restore browse DEVICE SNAPSHOT [PATH]          # a directory listing, fetching only what is asked
redundanet restore start DEVICE SNAPSHOT [PATH] --to DIR [--overwrite]
redundanet restore status
```

A restore runs in the background. Files are written under temporary names and renamed when complete, with modification time and permission bits set after the data; ownership and the set-user-ID, set-group-ID and sticky bits are not restored. Nothing is written outside the folder given: an entry whose name would lead out of it (a `..`, or a separator in a name below the top) is not restored and is listed as an exception, and a symlink in the restored tree is recreated as a link but never written through. Without `--overwrite`, a file that exists is left alone and the restored copy is written beside it with `.restored` added to its name, listed as an exception. Names the target cannot hold (invalid UTF-8 on Windows, reserved names, forbidden characters) are mapped to an escaped form and each one is reported; a restore ends "complete with exceptions" when something could not be restored as it was, and `restore status` lists each exception. Restores read any k shares of each object and verify them before writing. A storage server that is off does not hold a restore, a listing or a backup up: a device that has had no connection to a server for 45 seconds gives each request to it two seconds, then goes on with the others, and the list says how many servers answered. A server it was connected to until a moment ago, as after the laptop wakes, still gets the full wait of 30 seconds.

## Restore tests

Every week the agent restores a sample of a snapshot to a scratch location, verifies it, and compares what it read with sources that have not changed since. `redundanet backup verify` asks for one now. `backup list` shows the last verified restore, every test kept, and how many weeks in a row have passed; the status page shows the streak per member. The test never asks for the phrase.

## Cleanup and the 30-day window

Deleting a file from a folder does not delete it from the grid: the snapshots that hold it keep it, under retention. What nothing references any more is removed by the cleanup job, weekly on its own or by hand:

```bash
redundanet backup cleanup [--dry-run] [--force]
```

Cleanup reads every root of the member, walks the snapshots on the grid to the set of reachable objects, lists what every server holds for the member, and sends a signed delete token for each object nothing references. It refuses to run when a root or a directory cannot be read, and refuses to delete more than half of what the grid holds unless `--force`.

A server tombstones an object only when the owner has not touched it for 7 days on the server's own clock. Within the 30-day window after that the owner's devices still read the object and a new upload of the same content clears the tombstone. After the window the shares go and the record stays, so repair never fills a deleted object back in. So anything deleted stays recoverable for 30 days, from any device of the member.

## Encoding changes

When the manifest declares a k-of-n transition, each device converts the objects its own snapshots reference, on its own every six hours and on `redundanet backup rebalance`, and publishes the root after each snapshot. Until the deadline the old shares do not count against your allocation, so you convert without deleting first. The status page shows the conversion as a bar.

## The local page

`redundanet open` opens the local page on `127.0.0.1:4470` in the browser through a one-time link that works for two minutes. It is never on the LAN; a headless node is reached over an SSH tunnel. It has six views:

- **Overview**: one sentence on how the backup stands (protected, backing up, needs attention, first backup still to run, nothing backed up yet), then what needs you, each with its button or link: a folder the system does not let the agent read, a failed backup, a failed restore test, a used-up allocation, no connection to a hub, a membership not signed yet, an update to install or one that failed. Below that, the last backup, the last restore test, your space against your allocation, how the network spreads a file (the manifest's k-of-n), and recent activity.
- **Backups**: one card per set with its folders, its last run and its progress, `Back up now`, its exclusions, and `Remove this folder`; a form that adds a folder under a new name (the form never replaces a set, `redundanet backup add` does); the restore tests with `Run a test now`.
- **Restore**: the versions of each of your devices, newest first; a version opens as its folders, link by link down to the folder or the file you want; the form at the end takes the folder to put it in, as a full path, and an option to replace files that exist there. The folder or file keeps its name inside that folder. The view shows the restore running, then its outcome with every exception. It is `redundanet restore list`, `browse` and `start` behind links, and reads only the directory shown.
- **Devices and network**: the devices paired from this one, with how each is reached now, and the link to the join form for a paired storage node the manifest does not list yet; a form that pairs a new device from the code it shows and your twelve words, the same as `redundanet device pair` (the words must be this member's, certify the new device and are kept nowhere); this device's keys and roles, the member list's revision, the hubs and the connections to other devices.
- **Settings**: schedule, versions to keep, bandwidth caps, update channel, `Check now`.
- **Advanced**: the diagnostics bundle, the service, repair, cleanup, conversion, the data folder and the log.

The page is in English, German, Spanish, French, Italian, Japanese, Korean, Portuguese, Russian and Chinese: it follows the browser's language until you choose another in its menu, which a cookie on `127.0.0.1` remembers. Reasons the agent gives for a failure are shown as the agent words them, in English. The parts that change (progress, status, the log) refresh by themselves every few seconds; with scripts off the page works the same and is refreshed by hand.
