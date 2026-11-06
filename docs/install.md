# Installing the RedundaNet 3 agent

RedundaNet 3 is one static binary named `redundanet`. There are no containers and nothing else to install. This page covers the download, the check against the signed release index, and the install on each platform. Joining the network is the next page: [join.md](join.md).

Tested on Linux (x86-64 and ARM64, including Raspberry Pi 5) and Apple silicon Macs. Windows and Intel Mac builds are provided and pass the test suite, but have not run on the live network yet.

## Download and verify

Release 3.0.0 is at <https://github.com/adefilippo83/redundanet/releases/tag/agent%2Fv3.0.0>. The archives are named `redundanet-3.0.0-<os>-<arch>.tar.gz`, with a zip on Windows:

| Platform | Archive | Binary inside |
| --- | --- | --- |
| Linux x86-64 | `redundanet-3.0.0-linux-amd64.tar.gz` | `redundanet-3.0.0-linux-amd64/redundanet` |
| Linux ARM64, Raspberry Pi 5 | `redundanet-3.0.0-linux-arm64.tar.gz` | `redundanet-3.0.0-linux-arm64/redundanet` |
| Apple silicon Mac | `redundanet-3.0.0-darwin-arm64.tar.gz` | `redundanet-3.0.0-darwin-arm64/redundanet` |
| Intel Mac | `redundanet-3.0.0-darwin-amd64.tar.gz` | `redundanet-3.0.0-darwin-amd64/redundanet` |
| Windows x86-64 | `redundanet-3.0.0-windows-amd64.zip` | `redundanet-3.0.0-windows-amd64\redundanet.exe` |

Every release's sha256 and size are in the signed release index, `releases/index.yaml` on the main branch (raw: <https://raw.githubusercontent.com/adefilippo83/redundanet/main/releases/index.yaml>), signed by the maintainer's offline release key in `releases/index.sig`. The release page also carries `sha256sums.txt`. Compare the hash of what you downloaded with the index entry for your platform:

```bash
shasum -a 256 redundanet-3.0.0-darwin-arm64.tar.gz     # macOS
sha256sum redundanet-3.0.0-linux-amd64.tar.gz          # Linux
```

```powershell
Get-FileHash redundanet-3.0.0-windows-amd64.zip         # Windows
```

The index is the way to verify a download on every platform today: Windows signing through SignPath and macOS notarization are set up in the release workflow but not yet active, so every 3.0.0 binary is unsigned. Linux binaries are never code-signed. Once installed, the agent only ever updates to what the index names, after verifying the signature on the index.

## macOS

Apple silicon is tested on the live network; the Intel build passes the test suite but has not run there yet.

```bash
tar -xzf redundanet-3.0.0-darwin-arm64.tar.gz
sudo install -m 0755 redundanet-3.0.0-darwin-arm64/redundanet /usr/local/bin/redundanet
redundanet version
```

Since the binary is not yet notarized, macOS refuses a copy the browser downloaded, because the browser marks it quarantined. Either allow it once in System Settings, Privacy and Security, after the first refusal, or clear the mark: `xattr -d com.apple.quarantine ./redundanet`. A copy fetched with `curl` carries no such mark and runs at once.

The data directory is `~/Library/Application Support/redundanet`. After joining ([join.md](join.md)), install the service:

```bash
redundanet service install --network redundanet
```

That is a launchd agent that starts at login and runs `redundanet agent`. A service that backs up Documents, Desktop or Downloads needs Full Disk Access for the agent; `backup add` says so when a folder is guarded and names the setting to change. `redundanet service status` and `redundanet service uninstall` do what they say.

## Linux laptop or desktop

```bash
tar -xzf redundanet-3.0.0-linux-amd64.tar.gz
sudo install -m 0755 redundanet-3.0.0-linux-amd64/redundanet /usr/local/bin/redundanet
redundanet version
```

The data directory is `~/.config/redundanet`. After joining:

```bash
redundanet service install --network redundanet
```

That is a systemd user unit with lingering, so it runs while you are logged out too. The service log is in the user journal (`journalctl --user -u redundanet.service`).

## Linux storage node

A storage node contributes disk and runs the agent as a system service, headless. The phrase never reaches the node: the node makes its own device key and a device of yours certifies it, by pairing when your laptop is on the node's network, or through a certificate file over SSH. The node needs at least 10 GiB free above its floor. Everything is in the node runbook, [../agent/deploy/node/README.md](../agent/deploy/node/README.md).

The short way, since 3.0.4: download the `linux-amd64` or `linux-arm64` archive as in [Download and verify](#download-and-verify), then, with the storage disk mounted:

```bash
sudo ./redundanet node install --storage-path /mnt/storage       # on the node: lays it out, shows a pairing code
redundanet device pair --network redundanet --storage <the code>  # on your laptop, with the phrase
```

The node starts by itself once paired and prints the `storage:` lines for your join request. It refuses a storage path that is not a mount point, so shares never land on the system disk by accident; `--allow-root-disk` overrides that knowingly. The script below makes the same layout and keeps the certificate-file flow for a node reached over SSH only. On the node:

```bash
# 1. on the node, as root: user, directories, binary, unit; prints the device key
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/install.sh
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/redundanet-agent.service
sudo STORAGE_PATH=/mnt/storage bash install.sh prepare 3.0.0
```

Then, with your laptop on the same network, pairing:

```bash
# 2. on the node: show a one-time code and wait
sudo bash install.sh pair
# on your laptop, where the phrase is typed
redundanet device pair --network redundanet --storage <the code>
```

The node receives its identity and the network through the code, joins and starts; nothing is copied by hand. Or, for a node you reach over SSH only, the certificate file:

```bash
# 2. on your laptop, where the phrase is typed: certify the node's device key
redundanet identity certify --network redundanet --device <KEY> --storage > node.cert
scp node.cert node:
# 3. on the node: accept the certificate, join, start; the certificate file is destroyed
sudo bash install.sh finish node.cert
```

`prepare` checks the download against the sha256 in `releases/index.yaml` on main. `pair` and `finish` both join the network and enable `redundanet-agent.service`.

What `prepare` sets up:

- the binary at `/var/lib/redundanet3/bin/redundanet`, owned by the service user `redundanet3` so the updater can rename a new version into place;
- the data directory `/var/lib/redundanet3` (identity, manifest copy, census state, `versions/` for updates);
- `/etc/redundanet3/agent.env` with `NETWORK`, `CHANNEL` and `STORAGE_PATH`;
- the share store in `STORAGE_PATH/redundanet3`, owned by the service user, and the unit drop-in that makes it writable for the confined service.

The node's device key and name go into your join request as the storage entry; the node contributes only once the maintainer lists it under `storage:` in the manifest. On a node, name the full path in every command and run the ones that talk to the running agent as root:

```bash
sudo /var/lib/redundanet3/bin/redundanet status
sudo /var/lib/redundanet3/bin/redundanet logs -n 100
```

The local page stays on `127.0.0.1:4470`. Over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then `sudo /var/lib/redundanet3/bin/redundanet open --print` and open the link it prints. To change the channel, edit `CHANNEL` in `/etc/redundanet3/agent.env` and `systemctl restart redundanet-agent`.

## Raspberry Pi

Each stable release carries a ready-to-boot image, `redundanet-rpi-<version>-arm64.img.xz`, with the agent installed as a storage node and nothing else. Flash it, boot it once, certify the device key it leaves on the SD card's boot partition, put the certificate back there, and the Pi joins: the steps are in [rpi-image/README.md](../rpi-image/README.md). A Pi running any other 64-bit Linux is a Linux storage node: `install.sh prepare` picks the `linux-arm64` archive on its own. Raspberry Pi 5 is the tested board; Pi 4 has not been measured.

## A second device, without the phrase

On the new device, after installing the binary:

```
redundanet identity pair --network redundanet
```

It shows a code and waits ten minutes. On a device of yours that already has its identity, on the same network, type the phrase when asked:

```
redundanet device pair --network redundanet <the code>
```

Add `--storage` if the new device will contribute disk. The new device now holds its identity and the network; finish with `redundanet service install --network redundanet`. The phrase never leaves the device you typed it on; the code is good for one device and ten minutes, and the two devices must be able to reach each other, on the same network or over public IPv6. For a machine with no second device beside it, the storage node flow above and the Raspberry Pi image remain.

## Windows

The Windows build passes the test suite but has not run on the live network yet. Unzip `redundanet-3.0.0-windows-amd64.zip`; the binary is `redundanet-3.0.0-windows-amd64\redundanet.exe`. Put it somewhere on your PATH, open a terminal and run `redundanet version`. The data directory is `%AppData%\redundanet`. After joining:

```powershell
redundanet service install --network redundanet
```

That is a logon task with restart on failure. The Windows binary carries a version resource (product name and version in its properties) and is unsigned at 3.0.0; see the download section.

## Channels and updates

The service follows the `stable` channel unless installed with `--channel early`. The running agent checks the signed index on start and every hour, downloads a newer release that fits its channel (or one that the manifest's `min_version` requires), verifies hash and size, runs the candidate in probation for up to two minutes, keeps the old binary as `redundanet.old`, and rolls back by itself if the new one does not reconnect. A candidate that fails is recorded and never retried until `redundanet update retry VERSION`.

```bash
redundanet update check
redundanet update apply
redundanet update status
```

## Uninstall

```bash
redundanet service uninstall
```

Then remove the data directory; nothing else was written. A storage node has the files under `/var/lib/redundanet3`, `/etc/redundanet3` and the store in `STORAGE_PATH/redundanet3`; tell the maintainer first, so the node leaves the manifest and the shares it held are repaired elsewhere.
