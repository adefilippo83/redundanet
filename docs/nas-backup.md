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
Sync is **one-way by design** — bidirectional sync means conflict resolution
(upstream's "Magic Folder" attempted it and was abandoned). Unchanged files
are skipped (`tahoe backup` keeps a local backupdb), and identical content
converges to the same capabilities, so periodic re-syncs are cheap.

## Properties

- **RPO ≈ the sync interval** (default 15 minutes): a file written to the
  share may live only on the node's disk until the next sync tick.
- **Snapshots accumulate on purpose**: every sync that changed something adds
  a timestamped snapshot under `backups:Archives/`. That is your
  oops/ransomware protection — deleting or encrypting files on the share does
  not touch already-archived snapshots. (Pruning/quota controls are a planned
  future feature.)
- The share and the node's storage contribution may live on the same disk —
  budget capacity for both.

## Setup (on the sharing node)

### 1. The share directory (on the external disk, not the SD card)

```bash
sudo mkdir -p /mnt/storage/share
sudo chown alessandro:alessandro /mnt/storage/share   # your SMB user
```

### 2. Samba

```bash
sudo apt install samba
sudo tee -a /etc/samba/smb.conf <<'EOF'

[grid-share]
   path = /mnt/storage/share
   writable = yes
   valid users = alessandro
   # Keep macOS metadata junk off the share (and out of the grid archive).
   veto files = /.DS_Store/._*/.Trashes/
   delete veto files = yes
EOF
sudo smbpasswd -a alessandro
sudo systemctl restart smbd
```

Connect from a Mac: Finder → Go → Connect to Server → `smb://<node-ip>/grid-share`.

### 3. Enable the sync

In `/opt/redundanet/.env`:

```bash
SYNC_ENABLED=true
SYNC_DIR=/mnt/storage/share
# SYNC_INTERVAL=900        # seconds; default 15 minutes
```

Then recreate the client so the settings and the bind-mount take effect:

```bash
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage --profile client up -d --force-recreate tahoe-client
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
- Large initial syncs can take a while — the loop logs duration and the
  per-run summary (`N files backed up, M reused`).
