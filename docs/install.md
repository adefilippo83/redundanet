# Installing the RedundaNet 3 agent

RedundaNet 3 is one static binary named `redundanet`. There are no containers and nothing else to install. One line installs it on macOS and Linux, and the agent then keeps itself up to date from the signed release index. Joining the network is the next page: [join.md](join.md).

Tested on Linux (x86-64 and ARM64, including Raspberry Pi 5) and Apple silicon Macs. Windows and Intel Mac builds are provided and pass the test suite, but have not run on the live network yet.

## macOS and Linux: one line

```bash
curl -fsSL https://redundanet.com/install.sh | sh
```

The script is [docs/install.sh](install.sh) in the repository, served by the website, and short enough to read first. It finds your platform, reads the signed release index, downloads the newest stable release and refuses it unless its sha256 and size match the index. It puts the binary at `bin/redundanet` in your data directory, where the agent can replace it when it updates itself (a directory owned by root would refuse the rename), and links `/usr/local/bin/redundanet` to it; that link is the one step that asks for your password. Last, the installed binary checks the index's signature itself, with the release key built into it. A copy fetched this way carries no quarantine mark, so macOS runs it at once.

Options: `--channel early` installs the newest release of either channel, `--version 3.0.5` a given one, `--bin PATH` puts the link elsewhere. Run the line again to move to another release by hand; the agent does that on its own anyway.

Then, on a laptop or desktop:

```bash
redundanet identity init        # your recovery phrase, shown once: write it down
redundanet service install      # the agent, as a service that starts at login
redundanet open                 # the local page in your browser
```

`identity init` shows a twelve-word phrase once and never again. Write it down and keep it like a passport: your member key comes from it, and so does every device you certify. There is no reset. `--passphrase` seals the identity file on disk.

The network is built into the binary: its name, its hub, the operator keys that sign the manifest and where the manifest is served. There is nothing to paste and no `--network` flag to type; the agent fetches the signed manifest the first time it needs it, and `redundanet network status` shows what it got. A device that never joined by hand follows the built-in descriptor across releases, so a new hub or a rotated operator key reaches it with the next release. Another network, or one handed to you out of band, is joined with `redundanet network join <descriptor>` and pinned from then on; see [operators.md](operators.md).

Then send your join request: [join.md](join.md). Your devices are admitted once the maintainer has signed the manifest with you in it.

### macOS

Apple silicon is tested on the live network; the Intel build passes the test suite but has not run there yet. The service is a launchd agent that starts at login and runs `redundanet agent`. The data directory is `~/Library/Application Support/redundanet`. A service that backs up Documents, Desktop or Downloads needs Full Disk Access for the agent; `backup add` says so when a folder is guarded and names the setting to change. `redundanet service status` and `redundanet service uninstall` do what they say.

### Linux laptop or desktop

The service is a systemd user unit with lingering, so it runs while you are logged out too. The data directory is `~/.config/redundanet` (or `$XDG_CONFIG_HOME/redundanet`); the service log is in the user journal (`journalctl --user -u redundanet.service`). A headless machine that contributes disk is a storage node: next section.

## Linux storage node: one line and a pairing code

A storage node contributes disk and runs the agent as a system service, headless, under its own user. The phrase never reaches the node: a device of yours certifies it through a one-time code. With the storage disk mounted, as root on the node:

```bash
curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage
```

The same download and checks as above, then `redundanet node install`: the service user `redundanet3`, the data directory `/var/lib/redundanet3` (identity, manifest copy, census state, `versions/` for updates), the binary at `/var/lib/redundanet3/bin/redundanet` owned by the service user so the updater can rename a new version into place, the unit `redundanet-agent`, the settings in `/etc/redundanet3/agent.env` (`NETWORK`, `CHANNEL`, `STORAGE_PATH`), the share store in `/mnt/storage/redundanet3` with the unit drop-in that makes it writable for the confined service, and a pairing code on the screen. It refuses a storage path that is not a mount point, so shares never land on the system disk by accident; `--allow-root-disk` on `node install` overrides that knowingly. The node needs at least 10 GiB free above its floor. `--channel early` is accepted here too.

On your laptop, on the same network as the node, with the phrase at hand:

```bash
redundanet device pair --storage <the code>
```

The node receives its identity and the network through the code, joins and starts; nothing is copied by hand. It prints the `storage:` lines for your join request, and contributes only once the maintainer lists it under `storage:` in the manifest.

No laptop on the node's network? The runbook's script keeps the certificate-file flow over SSH: [../agent/deploy/node/README.md](../agent/deploy/node/README.md).

On a node, name the full path in every command and run the ones that talk to the running agent as root:

```bash
sudo /var/lib/redundanet3/bin/redundanet status
sudo /var/lib/redundanet3/bin/redundanet logs -n 100
```

The local page stays on `127.0.0.1:4470`. Over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then `sudo /var/lib/redundanet3/bin/redundanet open --print` and open the link it prints. To change the channel, edit `CHANNEL` in `/etc/redundanet3/agent.env` and `systemctl restart redundanet-agent`.

## Raspberry Pi

Each stable release carries a ready-to-boot image, `redundanet-rpi-<version>-arm64.img.xz`, with the agent installed as a storage node and nothing else. Flash it, boot it once, certify the device key it leaves on the SD card's boot partition, put the certificate back there, and the Pi joins: the steps are in [rpi-image/README.md](../rpi-image/README.md). A Pi running any other 64-bit Linux is a Linux storage node: the one line above picks the `linux-arm64` release on its own. Raspberry Pi 5 is the tested board; Pi 4 has not been measured.

## A second device, without the phrase

On the new device, after the one line:

```
redundanet identity pair
```

It shows a code and waits ten minutes. On a device of yours that already has its identity, on the same network, type the phrase when asked:

```
redundanet device pair <the code>
```

Add `--storage` if the new device will contribute disk. The new device now holds its identity and the network; finish with `redundanet service install`. The phrase never leaves the device you typed it on; the code is good for one device and ten minutes, and the two devices must be able to reach each other, on the same network or over public IPv6. For a machine with no second device beside it, the storage node flow above and the Raspberry Pi image remain.

## Windows

The Windows build passes the test suite but has not run on the live network yet. Take `redundanet-<version>-windows-amd64.zip` from the newest release on the [download page](https://redundanet.com/download.html) and check its sha256 as described there; the binary inside is `redundanet-<version>-windows-amd64\redundanet.exe`. Put it somewhere on your PATH, open a terminal and run `redundanet version`. The data directory is `%AppData%\redundanet`. Then:

```powershell
redundanet identity init
redundanet service install     # a logon task with restart on failure
redundanet open
```

The Windows binary carries a version resource (product name and version in its properties) and is unsigned for now; see the download page.

## By hand, without the script

Every release is at <https://github.com/adefilippo83/redundanet/releases>, as `redundanet-<version>-<os>-<arch>.tar.gz`, with a zip on Windows:

| Platform | Archive | Binary inside |
| --- | --- | --- |
| Linux x86-64 | `redundanet-<version>-linux-amd64.tar.gz` | `redundanet-<version>-linux-amd64/redundanet` |
| Linux ARM64, Raspberry Pi 5 | `redundanet-<version>-linux-arm64.tar.gz` | `redundanet-<version>-linux-arm64/redundanet` |
| Apple silicon Mac | `redundanet-<version>-darwin-arm64.tar.gz` | `redundanet-<version>-darwin-arm64/redundanet` |
| Intel Mac | `redundanet-<version>-darwin-amd64.tar.gz` | `redundanet-<version>-darwin-amd64/redundanet` |
| Windows x86-64 | `redundanet-<version>-windows-amd64.zip` | `redundanet-<version>-windows-amd64\redundanet.exe` |

Every release's sha256 and size are in the signed release index, `releases/index.yaml` on the main branch (raw: <https://raw.githubusercontent.com/adefilippo83/redundanet/main/releases/index.yaml>), signed by the maintainer's offline release key in `releases/index.sig`. The release page also carries `sha256sums.txt`. Compare the hash of what you downloaded with the index entry for your platform:

```bash
shasum -a 256 redundanet-<version>-darwin-arm64.tar.gz     # macOS
sha256sum redundanet-<version>-linux-amd64.tar.gz          # Linux
```

```powershell
Get-FileHash redundanet-<version>-windows-amd64.zip         # Windows
```

Windows signing through SignPath and macOS notarization are set up in the release workflow but not yet active, so every binary is unsigned; Linux binaries are never code-signed. The index is the way to verify a download on every platform, and once installed, the agent only ever updates to what the index names, after verifying the signature on the index.

Unpack the archive and put the binary where the agent can replace it: for a laptop that is `bin/redundanet` in the data directory, with a link on your PATH, which is what the script does; a copy owned by root in `/usr/local/bin` runs, but the agent cannot update it. A browser's download on macOS is quarantined: allow it once in System Settings, Privacy and Security, after the first refusal, or clear the mark with `xattr -d com.apple.quarantine ./redundanet`.

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

Then remove the data directory, which holds the binary too, and the link `/usr/local/bin/redundanet`; nothing else was written. A storage node has the files under `/var/lib/redundanet3`, `/etc/redundanet3` and the store in `STORAGE_PATH/redundanet3`; tell the maintainer first, so the node leaves the manifest and the shares it held are repaired elsewhere.
