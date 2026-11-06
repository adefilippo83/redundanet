# RedundaNet 3 on a Raspberry Pi

A ready-to-boot Raspberry Pi OS image with the RedundaNet 3 agent installed
as a storage node. Flash it, boot it, certify the key it leaves on the SD
card, put the certificate back: the Pi joins the network and keeps itself up
to date from the signed release index. Nothing of RedundaNet 2 is on it: no
Docker, no Python, no tinc.

The image is built for each stable agent release and attached to it on the
[releases page](https://github.com/adefilippo83/redundanet/releases), as
`redundanet-rpi-<version>-arm64.img.xz` with a `.sha256` beside it. It is
64-bit only: Raspberry Pi 5 is the tested board, a Pi 4 or Pi 3 boots the
same image but has not been measured.

## What is on it

- Raspberry Pi OS Lite (64-bit, Trixie), SSH enabled, hostname `redundanet`,
  login `redundanet` / `redundanet`. Change the password at the first login.
- The agent at `/var/lib/redundanet3/bin/redundanet`, the service user
  `redundanet3`, the unit `redundanet-agent`, the settings in
  `/etc/redundanet3/agent.env` (`NETWORK=redundanet`, `CHANNEL=stable`,
  `STORAGE_PATH=/mnt/storage`). The same layout `install.sh` makes on any
  Linux node; `sudo redundanet-install` is that installer.
- A first boot that makes this node's device key and writes it to the boot
  partition, and a watcher that joins the node when a certificate appears
  there.

## Steps

1. **Flash.** Raspberry Pi Imager ("Use custom") or `xzcat image.img.xz |
   sudo dd of=/dev/sdX bs=4M status=progress`. Check the download first:
   `sha256sum -c redundanet-rpi-<version>-arm64.img.xz.sha256`.
2. **Boot once**, with the network cable in. After a minute the boot
   partition of the SD card holds `redundanet-device-key.txt` with the
   node's device key. Read it by putting the card back in your computer, or
   over SSH: `ssh redundanet@redundanet.local` and `cat
   /boot/firmware/redundanet-device-key.txt`. The console shows it too.
3. **Certify** on a computer where you can type your recovery phrase:

   ```bash
   redundanet identity certify --network redundanet --device <KEY> --storage > redundanet-node.cert
   ```

   The file carries your member secret: it is for this card only.
4. **Give the Pi its disk.** Shares must not live on the SD card. With the
   disk attached, over SSH:

   ```bash
   lsblk                                   # find it; formatting destroys what is on it
   sudo mkfs.ext4 /dev/sda1
   UUID=$(sudo blkid -s UUID -o value /dev/sda1)
   echo "UUID=$UUID /mnt/storage ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab
   sudo mount /mnt/storage
   ```

   Do this before step 5 if you can. Done later, run `sudo
   redundanet-install storage` once the disk is mounted: it moves the store
   directory onto the disk and restarts the agent.
5. **Put the certificate on the boot partition** as `redundanet-node.cert`:
   copy it onto the SD card from your computer and boot the Pi again, or
   `scp redundanet-node.cert redundanet@redundanet.local:/tmp/ && ssh
   redundanet@redundanet.local sudo mv /tmp/redundanet-node.cert
   /boot/firmware/`. The node accepts it, fetches the manifest, joins and
   starts the agent; the certificate file is destroyed. The result is in
   `redundanet-status.txt` next to it.
6. **List the node.** The node appears on the status page under your name
   once the operator has merged it into `storage:` in the manifest: the
   device key and a name go in your join request, or in a pull request if
   you are already a member. Until then the hub does not admit it.

Without the boot partition: `sudo redundanet-install finish
redundanet-node.cert` over SSH does the same as step 5.

Afterwards, as on any node: `sudo /var/lib/redundanet3/bin/redundanet
status`, and the local page through `ssh -L 4470:127.0.0.1:4470
redundanet@redundanet.local` then `sudo /var/lib/redundanet3/bin/redundanet
open --print`.

## If something is off

- **No `redundanet-device-key.txt` after a boot**: `journalctl -u
  redundanet3-firstboot` on the Pi. The key is written again at every boot
  until the node has an identity.
- **`redundanet-status.txt` says the certificate was not accepted**: it
  names the reason; the usual ones are a certificate made for another key
  (certify the key in `redundanet-device-key.txt`) or no network when the
  manifest was fetched. Make a new certificate and put it there again; the
  rejected file was removed.
- **The agent runs but the status page does not show the node**: the node is
  not yet listed under `storage:`, step 6.
- **Shares on the SD card**: `df /mnt/storage` shows the SD card's root
  filesystem instead of the disk. Mount the disk by UUID (step 4), then `sudo
  redundanet-install storage`.
- **Updates**: the agent checks the signed release index every hour and
  updates itself with probation and rollback; nothing to do. `sudo
  /var/lib/redundanet3/bin/redundanet update status` shows where it stands.

## How the image is built

`.github/workflows/build-rpi-image.yml` runs after each successful "agent
release" workflow of a stable tag (`agent/vX.Y.Z` without a pre-release
suffix), or by hand for any existing release. It downloads that release's
`linux-arm64` archive, checks it against the release's `sha256sums.txt`,
runs `install.sh prepare` inside the image with the archive and without a
device key, installs the first-boot units from this directory, and attaches
the compressed image and its checksum to the release. The image is never
listed in the signed release index; the agent inside it verifies the index
on its own from the first hour.
