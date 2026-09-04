# NAS share with async grid backup

Turn a node (e.g. a Raspberry Pi) into a small NAS whose contents are
asynchronously replicated into the grid:

```
LAN devices ──SMB──▶ Samba share on the node (real disk: fast, normal files)
                              │  read-only bind-mount into tahoe-client
                              ▼
                  tahoe backup, every 15 min (incremental)
                              ▼
            grid: backups:Archives/<timestamp>/  +  backups:Latest/
```

The share is the **source of truth**; the grid is the **replicated archive**.
Sync is **one-way by design**: bidirectional sync means conflict resolution
(upstream's "Magic Folder" attempted it and was abandoned). Unchanged files
are skipped (`tahoe backup` keeps a local backupdb), and identical content
converges to the same capabilities, so periodic re-syncs are cheap.

## Properties

- **RPO ≈ the sync interval** (default 15 minutes): a file written to the
  share may live only on the node's disk until the next sync tick.
- **Snapshots accumulate on purpose**: every sync that changed something adds
  a timestamped snapshot under `backups:Archives/`. That is your
  oops/ransomware protection: deleting or encrypting files on the share does
  not touch already-archived snapshots. (Pruning/quota controls are a planned
  future feature.)
- The share and the node's storage contribution may live on the same disk:
  budget capacity for both.

## Setup (on the sharing node)

> The examples use `youruser` as the account that will access the share
> over SMB: replace it with your own Unix user (on the prebuilt
> Raspberry Pi image that is `redundanet` by default).

### 1. The share directory (on the external disk, not the SD card)

```bash
sudo mkdir -p /mnt/storage/share
```

**Permissions matter**: Samba writes as the authenticated Unix user, so a
`root:root` directory with mode 755 makes the share read-only for everyone
(writes fail with access denied). The grid sync is unaffected either way (it
reads through the container as root), but pick one of these:

Single SMB user:

```bash
sudo chown youruser:youruser /mnt/storage/share   # your SMB user
sudo chmod 755 /mnt/storage/share    # 700 if other local users shouldn't read it
```

Multiple SMB users sharing one folder:

```bash
sudo groupadd -f gridshare
sudo usermod -aG gridshare youruser                 # repeat for each user
sudo chown root:gridshare /mnt/storage/share
sudo chmod 2775 /mnt/storage/share                    # setgid: new files inherit the group
```

### 2. Samba

`youruser` below is a placeholder: replace it with your Unix username, or use
`@gridshare` (the group) for the multi-user layout. A `valid users` entry that
names a user who does not exist denies everyone, and Finder reports that as
"the original item for grid-share can't be found" rather than as a login error.

```bash
sudo apt install samba
sudo tee -a /etc/samba/smb.conf <<'EOF'

[grid-share]
   path = /mnt/storage/share
   writable = yes
   valid users = youruser
   # Keep macOS metadata junk off the share (and out of the grid archive).
   veto files = /.DS_Store/._*/.Trashes/
   delete veto files = yes
EOF
sudo smbpasswd -a youruser
sudo systemctl restart smbd
```

For the multi-user layout, also add `force group = gridshare`,
`create mask = 0664` and `directory mask = 2775` to the section, so a file
created by one member stays writable by the others.

macOS asks the server for DFS referrals using its Bonjour name, which Samba
logs as `parse_dfs_path_strict: Hostname ... is not ours`. It is harmless, but
`host msdfs = no` in `[global]` silences it.

Connect from a Mac: Finder → Go → Connect to Server → `smb://<node-ip>/grid-share`.

### 3. Enable the sync

First make sure the node's compose file actually maps the sync settings into
the container. Nodes that joined before the NAS feature shipped carry an older
`/opt/redundanet/docker/docker-compose.yml` that has no `SYNC_*` passthrough,
so `SYNC_ENABLED=true` in `.env` does nothing and the log says `disabled`.
`redundanet update` only pulls new images, not compose files, so refresh it
from the repo clone that `network join` maintains:

```bash
grep -q REDUNDANET_SYNC_ENABLED /opt/redundanet/docker/docker-compose.yml || \
  cp /var/lib/redundanet/repo/docker/docker-compose.yml \
     /opt/redundanet/docker/docker-compose.yml
```

Then, in `/opt/redundanet/.env`:

```bash
SYNC_ENABLED=true
SYNC_DIR=/mnt/storage/share
# SYNC_INTERVAL=900        # seconds; default 15 minutes
# SYNC_TIMEOUT=21600       # per-run ceiling; default 6h (first syncs are slow)
```

Then recreate the client so the settings and the bind-mount take effect. Use
`--no-deps` so only the client is recreated: without it, compose also cycles
the `tinc` container, and on a node that also runs storage that strands the
storage container on the old network namespace ("0 shares connected"). `tinc`
must already be running (it is, on a node that is up):

```bash
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage --profile client up -d --force-recreate --no-deps tahoe-client
```

Watch it work:

```bash
docker logs -f redundanet-tahoe-client 2>&1 | grep backup-sync
```

## Restoring

List snapshots and pull files back (on any node with a client):

```bash
redundanet storage ls backups:Latest/
redundanet storage ls backups:Archives/
redundanet storage download backups:Latest/photo.jpg ./photo.jpg
# whole-tree restore:
docker exec redundanet-tahoe-client tahoe -d /var/lib/tahoe-client \
  cp -r backups:Latest/ /tmp/restore
```

The `backups:` alias is created automatically on the syncing node. To browse
or restore from a *different* node, share the alias capability with that node
(`tahoe list-aliases` on the syncing node → `tahoe add-alias` on the other).

## Notes

- The sync only ever **reads** the share (the bind-mount is read-only).
- If the grid is unreachable, the share keeps working; the sync retries every
  cycle and catches up.
- Large initial syncs can take a while: the loop logs duration and the
  per-run summary (`N files backed up, M reused`). Progress survives an
  interrupted run: already-uploaded files are recorded in the backupdb and
  skipped on the next cycle.
- A file still being written (e.g. a large copy over SMB in progress) when a
  sync fires may be archived **truncated in that snapshot**; the next cycle
  archives the complete version. Snapshots make this self-healing, but for a
  guaranteed-consistent snapshot, pause writes for one sync interval.
