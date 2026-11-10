# RedundaNet 3 on a Raspberry Pi

A ready-to-boot Raspberry Pi OS image with the RedundaNet 3 agent installed
as a storage node. Flash it, boot it, pair it from a device of yours with
the code it shows, give it a disk labeled REDUNDANET: the Pi joins the
network and keeps itself up to date from the signed release index. Nothing
of RedundaNet 2 is on it: no Docker, no Python, no tinc.

The image is built for each stable agent release and attached to it on the
[releases page](https://github.com/adefilippo83/redundanet/releases), as
`redundanet-rpi-<version>-arm64.img.xz` with a `.sha256` beside it. It is
64-bit only: Raspberry Pi 5 is the tested board, a Pi 4 or Pi 3 boots the
same image but has not been measured. Images from 3.0.12 on pair as below;
the earlier ones used a certificate file, which still works (see "Without
pairing").

## What is on it

- Raspberry Pi OS Lite (64-bit, Trixie), SSH enabled, hostname `redundanet`,
  login `redundanet` / `redundanet`. Change the password at the first login.
- The agent at `/var/lib/redundanet3/bin/redundanet`, which the service
  runs and updates, and a copy root owns at `/usr/local/bin/redundanet3`,
  which is the command you run (`sudo redundanet3 ...`); the service user
  `redundanet3`, the unit `redundanet-agent`, the settings in
  `/etc/redundanet3/agent.env` (`NETWORK=redundanet`, `CHANNEL=stable`,
  `STORAGE_PATH=/mnt/storage`). The same layout `install.sh` makes on any
  Linux node; `sudo redundanet-install` is that installer.
- A first boot that shows a pairing code, on the boot partition and the
  console, until a device of yours pairs the node. The code stays the same
  across reboots.
- An `/etc/fstab` line that mounts the disk labeled `REDUNDANET` at
  `/mnt/storage` whenever it is attached, and a rule that the agent runs only
  while that disk is mounted, so shares never land on the SD card.
- `redundanet-status.txt` on the boot partition, written when the node is
  paired and refreshed every five minutes: paired, disk, agent.

## Steps

1. **Flash.** Raspberry Pi Imager ("Use custom") or `xzcat image.img.xz |
   sudo dd of=/dev/sdX bs=4M status=progress`. Check the download first:
   `sha256sum -c redundanet-rpi-<version>-arm64.img.xz.sha256`.
2. **Boot**, with the network cable in. After a minute the boot partition
   of the SD card holds `redundanet-pairing-code.txt` with the node's
   pairing code; the console shows it too, and so does the login message.
   Read it over SSH (`ssh redundanet@redundanet.local`, then `cat
   /boot/firmware/redundanet-pairing-code.txt`), or without SSH: power the
   Pi off, put the card in your computer, read the file, put the card back
   and boot again. The code is the same after every reboot until the node
   is paired.
3. **Pair**, on a device of yours that has its identity, with your recovery
   phrase at hand, on the same network as the Pi or anywhere else (the hub
   relays the exchange and learns nothing but that a pairing happened):

   ```bash
   redundanet device pair --storage
   ```

   It asks for the code, then for the phrase.

   The Pi receives its identity and the network, joins, and the code leaves
   the card. Your device records the Pi (key, name, certificate), so
   `redundanet join-request` there opens the join form with the Pi filled
   in. The phrase never reaches the Pi. Anyone holding the code could claim
   the node until it is paired, so keep the file to yourself.
4. **Give the Pi its disk.** Shares must not live on the SD card, and they
   cannot: the agent starts only once a disk labeled `REDUNDANET` is mounted
   at `/mnt/storage`. With the disk attached, over SSH:

   ```bash
   lsblk                                   # find it; the next line destroys what is on it
   sudo redundanet-install disk /dev/sda
   ```

   It formats the whole device as ext4 labeled `REDUNDANET`, mounts it,
   makes the store directory on it and starts the agent. A disk that already
   carries an ext4 filesystem labeled `REDUNDANET` (made anywhere with
   `mkfs.ext4 -L REDUNDANET /dev/sdX`) mounts on its own, at boot or when it
   is plugged in, and the agent starts with it. The disk can be attached
   before the first boot or after pairing; the order does not matter.
5. **List the node.** The node appears on the status page under your name
   once the maintainer has merged it into `storage:` in the manifest: on the
   device that paired it, `redundanet join-request` opens the form with the
   node's key, name and certificate in place (a new member sends the
   membership request with the node in it the same way). Until then the hub
   does not admit it.

How things stand, at any time: `redundanet-status.txt` on the boot
partition (paired, disk, agent and its version), refreshed every five
minutes; over SSH `sudo redundanet3 status`; the
local page through `ssh -L 4470:127.0.0.1:4470 redundanet@redundanet.local`
and then `sudo redundanet3 open --print`.

## Without pairing

A Pi with no device of yours to pair from, or one that cannot reach the hub
over HTTPS, takes the certificate file over SSH instead, the node
installer's `finish`: on the Pi, `sudo -u redundanet3
/var/lib/redundanet3/bin/redundanet identity device-key --data-dir
/var/lib/redundanet3` prints its device key; on a device of yours,
`redundanet identity certify --device <KEY> --storage > redundanet-node.cert`;
back on the Pi, `sudo redundanet-install finish redundanet-node.cert`. The
pairing code leaves the card once the node has its identity either way.

## If something is off

- **No `redundanet-pairing-code.txt` after a boot**: `journalctl -u
  redundanet3-firstboot` on the Pi, and the network cable. The code is
  shown again at every boot until the node is paired.
- **`device pair` cannot reach the Pi**: a code the Pi showed before it was
  online names no hub to relay through and works on the Pi's own network
  only; within ten minutes the Pi shows one that does, in the same file.
  Read the file again and try again.
- **The agent is not running**: no disk labeled `REDUNDANET` is mounted.
  `lsblk -o NAME,SIZE,FSTYPE,LABEL,MOUNTPOINT` shows the disks;
  `sudo redundanet-install disk /dev/sda` makes one. A disk labeled so but
  formatted exFAT or NTFS mounts, but the agent cannot own files on it: it
  has to be ext4.
- **`redundanet-status.txt` says paired but the status page does not show
  the node**: it is not yet listed under `storage:`, step 5.
- **Updates**: the agent checks the signed release index every hour and
  updates itself with probation and rollback; nothing to do. `sudo
  redundanet3 update status` shows where it stands.

## How the image is built

`.github/workflows/build-rpi-image.yml` runs after each successful "agent
release" workflow of a stable tag (`agent/vX.Y.Z` without a pre-release
suffix), or by hand for any existing release. It downloads that release's
`linux-arm64` archive, checks it against the release's `sha256sums.txt`,
runs `install.sh prepare` inside the image with the archive and without a
device key, installs the first-boot and status units and the two drop-ins
from this directory (`agent-disk.conf` for the agent, `mount-agent.conf`
for the mount), adds the `/etc/fstab` line for the disk labeled
`REDUNDANET`, makes the mount point immutable so nothing lands on the card
under it, and attaches the compressed image and its checksum to the
release. The image is never listed in the signed release index; the agent
inside it verifies the index on its own from the first hour. The first boot
needs the agent 3.0.12 or later (`identity pair --code-file`); an image
built by hand for an older release says so in the journal and stops.
