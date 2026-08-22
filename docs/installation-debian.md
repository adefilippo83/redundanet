# Installing a node on Debian 13 (Trixie) — from scratch

A complete walkthrough from a fresh Debian 13 install to a running RedundaNet
node. Applies to x86 and ARM machines (including a Raspberry Pi running
Debian/Raspberry Pi OS Trixie — if you use the prebuilt RPi image instead,
see `rpi-image/README.md`).

You will need:

- A machine running Debian 13, on 24/7, wired Ethernet recommended
- A **dedicated storage disk** (USB or internal) — grid data must NOT live on
  the system disk / SD card
- A non-root user with `sudo`
- ~30 minutes

---

## 1. Base system

```bash
sudo apt update && sudo apt full-upgrade -y
sudo apt install -y curl git gnupg pipx ca-certificates
```

Give the machine a **unique hostname** (avoids mDNS collisions when several
nodes share a LAN):

```bash
sudo hostnamectl set-hostname my-node
```

## 2. Docker + user privileges

```bash
sudo apt install -y docker.io
sudo systemctl enable --now docker
```

Add your user to the `docker` group so docker commands work without `sudo`.
The package normally creates the group, but not always — create it first
(`-f` makes this safe if it already exists), and restart the daemon so its
socket is owned by the group:

```bash
sudo groupadd -f docker
sudo usermod -aG docker $USER
sudo systemctl restart docker
```

⚠️ Group membership takes effect on your **next login**: log out and back in
(over SSH: reconnect), or `newgrp docker` for the current shell. Verify:

```bash
id -nG                         # must list 'docker'
ls -l /var/run/docker.sock     # must be root:docker
docker ps                      # must answer without "permission denied"
```

### Docker Compose v2 (IMPORTANT)

Debian's `docker-compose` apt package is the **legacy v1 tool** and does not
work with RedundaNet (it doesn't understand `-p`, `--env-file`, or
`--profile`). **Do not install it.** Install the official v2 CLI plugin
instead:

```bash
# x86_64 machines → x86_64 ; 64-bit Raspberry Pi → aarch64
ARCH=$(uname -m)
sudo mkdir -p /usr/local/lib/docker/cli-plugins
sudo curl -fSL "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${ARCH}" \
  -o /usr/local/lib/docker/cli-plugins/docker-compose
sudo chmod +x /usr/local/lib/docker/cli-plugins/docker-compose

docker compose version    # must print "Docker Compose version v..."
```

Do not continue until `docker compose version` answers correctly — the
`redundanet` CLI drives `docker compose` for everything.

## 3. The RedundaNet CLI

```bash
pipx install redundanet
pipx ensurepath           # then open a new shell
redundanet --version
```

(Debian 13 ships Python 3.13 — fully supported, nothing extra needed.)

For future upgrades always use:

```bash
pipx upgrade redundanet --pip-args="--no-cache-dir"
```

(`--no-cache-dir` prevents a stale pip HTTP cache from silently serving you
an old version.)

## 4. GPG key (the node's identity)

Your GPG key IS the node's identity — it is also used as the VPN transport
key. Generate it and publish it to a keyserver:

```bash
redundanet node keys generate --name my-node --email you@example.com
gpg --fingerprint          # copy the FULL 40-character hex FINGERPRINT
redundanet node keys publish --key-id <FULL_FINGERPRINT>
```

⚠️ The **full 40-character fingerprint** is required (e.g.
`36E3DFE7C1A3EC2ECC84F5F9CE7AF4CFD52801B9`). Short 8/16-character key ids are
rejected everywhere. If `keys publish` reports the key cannot be fetched
back, follow the instructions it prints (keys.openpgp.org may require an
email confirmation) and retry before moving on — a key that peers cannot
fetch cannot authenticate your node.

## 5. Apply to join the network

Go to **https://redundanet.com/join.html** and submit:

- your full 40-character fingerprint
- the disk space you contribute
- your region

This opens a GitHub issue; a maintainer approves the pull request that adds
your node to the network manifest. You will be assigned a node name
(`node-xxxxxxxx`) and a VPN IP. Wait for approval before continuing.

## 6. Join

Prepare the directories (owned by your user):

```bash
sudo install -d -o $USER -g $USER /opt/redundanet /var/lib/redundanet
```

Then join, using the node name assigned in step 5:

```bash
redundanet network join \
  --repo https://github.com/adefilippo83/redundanet.git \
  --name node-xxxxxxxx
```

The join syncs the manifest, installs the docker files under
`/opt/redundanet`, and generates `/opt/redundanet/.env` for your node. It
ends by **printing the exact start command** — don't run it yet: set up the
storage disk first (step 7).

## 7. Dedicated storage disk (do NOT skip)

Without this step, grid data lands on the system disk inside a Docker
volume — and after a reboot with a missing mount, the node silently appears
empty.

```bash
lsblk                                  # identify the disk (e.g. /dev/sda) — CAREFUL: formatting is destructive
sudo mkfs.ext4 /dev/sda1
sudo mkdir -p /mnt/storage
sudo mount /dev/sda1 /mnt/storage
sudo mkdir -p /mnt/storage/redundanet

# persist the mount across reboots — always by UUID
UUID=$(sudo blkid -s UUID -o value /dev/sda1)
echo "UUID=$UUID /mnt/storage ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab

# point the storage container at the disk
cat <<'EOF' | tee /opt/redundanet/docker/docker-compose.override.yml
services:
  tahoe-storage:
    volumes:
      - /mnt/storage/redundanet:/data/storage
EOF
```

## 8. Start the node

The join printed the exact command; in general:

```bash
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage up -d
```

- `-p redundanet` is mandatory: it is the project name the `redundanet` CLI
  uses to find the containers.
- Add `--profile client` if this node should also upload/download files.
- Containers use `restart: unless-stopped` and come back on their own after a
  reboot.

Verify the storage container really reads from the external disk:

```bash
docker inspect redundanet-tahoe-storage \
  --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' | grep /data/storage
# expected: /mnt/storage/redundanet -> /data/storage
```

## 9. Verify

```bash
redundanet status                      # node status
docker compose -p redundanet ps        # containers up (tinc healthy)
redundanet network peers               # VPN reachability of other nodes
```

The VPN takes ~1 minute to converge on first start. Network-wide status is
also visible on the public status page.

### Firewall (if you run one)

A node behind NAT works without opening any ports (it connects out to the
hub). If your node is publicly reachable and you want direct peer
connections, open **655/tcp and 655/udp** (tinc) inbound.

## 10. Maintenance

```bash
# upgrade the CLI
pipx upgrade redundanet --pip-args="--no-cache-dir"

# update containers (check first, then apply with a safe service restart)
redundanet update --check
redundanet update
```

⚠️ Never restart only the `tinc` container (`docker restart ...`): the tahoe
containers would be stranded on its dead network namespace. Use
`redundanet update`, or the full force-recreate:

```bash
cd /opt/redundanet/docker && docker compose -p redundanet --env-file /opt/redundanet/.env \
  --profile storage up -d --force-recreate tinc tahoe-storage tahoe-client
```

### Back up your key (important!)

The GPG private key is the node's identity: lose it and the node must be
re-registered from scratch. Copy it off the machine:

```bash
cp /opt/redundanet/docker/secrets/gpg_private_key.asc /somewhere/safe/
```

## Optional features

- **SFTP** (file access via sftp/sshfs): set `SFTP_ENABLED=true` in
  `/opt/redundanet/.env` — see the SFTP section of the docs.
- **NAS share with async grid backup** (Samba + one-way sync): see
  [nas-backup.md](nas-backup.md).
