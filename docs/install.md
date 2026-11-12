# Installing the RedundaNet 3 agent

RedundaNet 3 is one static binary named `redundanet`. There are no containers and nothing else to install. One line installs it on macOS, Linux and Windows, and the agent then keeps itself up to date from the signed release index. Joining the network is the next page: [join.md](join.md).

Tested on Linux (x86-64 and ARM64, including Raspberry Pi 5) and Apple silicon Macs. Windows and Intel Mac builds are provided and pass the test suite, but have not run on the live network yet.

## One line

In a terminal on macOS and Linux:

```bash
curl -fsSL https://redundanet.com/install.sh | sh
```

In PowerShell on Windows (Windows PowerShell 5.1, which every Windows 10 and 11 has, or PowerShell 7):

```powershell
irm https://redundanet.com/install.ps1 | iex
```

The scripts are [docs/install.sh](install.sh) and [docs/install.ps1](install.ps1) in the repository, served by the website, and short enough to read first. Each finds your platform, reads the release index, downloads the newest stable release and refuses it unless its sha256 and size match the index. It puts the binary at `bin/redundanet` in your data directory (`bin\redundanet.exe` on Windows), where the agent can replace it when it updates itself (a directory owned by root would refuse the rename), and puts it on your PATH: on macOS and Linux a link at `/usr/local/bin/redundanet`, the one step that asks for your password; on Windows the folder is added to your user PATH, with nothing to approve, and terminals opened from then on find it. The installed binary then checks the index's signature itself, with the release key built into it; the script does not, and with `--node` that step is not run. A first install therefore trusts GitHub and the website, and the signature protects the updates that follow: [security.md](security.md), "The manifest and the repository". A copy fetched this way carries no quarantine mark, so macOS runs it at once. Last, on a laptop or desktop, it installs the agent as a service that starts at login and opens the agent's page in your browser.

Options: `--channel early` installs the newest release of either channel and puts the service on it, `--version 3.0.9` a given release, `--bin PATH` puts the link elsewhere. On Windows a piped script takes no arguments, so the first two go in the environment before the line, `$env:REDUNDANET_CHANNEL = "early"` or `$env:REDUNDANET_VERSION = "3.0.9"`; or run it with parameters: `& ([scriptblock]::Create((irm https://redundanet.com/install.ps1))) -Channel early`. Run the line again to move to another release by hand; the agent does that on its own anyway. A service installed earlier is left alone; `redundanet service install` points it at the new binary.

The page walks you through the rest. With no identity yet it asks which case you are in: new here (a twelve-word phrase, shown once and never again: write it down and keep it like a passport, your member key comes from it and so does every device you certify, and there is no reset; then three of the words are asked back), an existing member with the recovery phrase at hand (typed into the page, this device gets a certificate of its own), or a member with another device nearby to pair with (the page shows a code, the phrase stays on the other device). The phrase appears only there, on a page served on 127.0.0.1 and reached through a one-time link, never over the network. Once the identity exists the same page becomes the device's page: it says whether you are listed in the manifest yet and, until you are, links to the join form filled in from this device.

The terminal does the same: `redundanet setup` walks the device through identity, service, join request and page, step by step, and can be run again at any time; `--storage` gives this device the storage role, `--print` prints the links instead of opening the browser. By hand, the same is `redundanet identity init` (`--from-phrase` for an existing member, `--passphrase` to seal the identity file on disk), `redundanet service install`, `redundanet open` and `redundanet join-request`. An agent started before the identity exists, by the service or by hand, serves the page alone with the setup and continues as the agent once the identity is there; `redundanet status` says "no identity yet" until then.

The network is built into the binary: its name, its hub, the operator keys that sign the manifest and where the manifest is served. There is nothing to paste and no `--network` flag to type; the agent fetches the signed manifest the first time it needs it, and `redundanet network status` shows what it got. A device that never joined by hand follows the built-in descriptor across releases, so a new hub or a new set of operator keys reaches it with the next release (operator keys normally change through the signed manifest itself; the descriptor is what replaces them when a key was stolen). Another network, or one handed to you out of band, is joined with `redundanet network join <descriptor>` and pinned from then on; see [operators.md](operators.md).

Then send the join request `setup` opened: [join.md](join.md). Your devices are admitted once the maintainer has signed the manifest with you in it; `redundanet status` and the local page say "listed in the manifest" from then on.

### macOS

Apple silicon is tested on the live network; the Intel build passes the test suite but has not run there yet. The service is a launchd agent that starts at login and runs `redundanet agent` through a launcher, a copy of the binary kept at `launcher/redundanet` in the data directory and never replaced. macOS keys the permissions it grants a program, such as access to Documents, to the program's identity, and for an unsigned binary that identity changes with every update; because the agent runs as the launcher's child, a permission granted once to the launcher survives the agent's self-updates. The data directory is `~/Library/Application Support/redundanet`. A service that backs up Documents, Desktop or Downloads needs Full Disk Access for the agent; `backup add` says so when a folder is guarded and names the setting to change, and the grant goes to the launcher, once. `redundanet service status` and `redundanet service uninstall` do what they say.

### Linux laptop or desktop

The service is a systemd user unit with lingering, so it runs while you are logged out too. The data directory is `~/.config/redundanet` (or `$XDG_CONFIG_HOME/redundanet`); the service log is in the user journal (`journalctl --user -u redundanet.service`). A headless machine that contributes disk is a storage node: next section.

## Linux storage node: one line and a pairing code

A storage node contributes disk and runs the agent as a system service, headless, under its own user. The phrase never reaches the node: a device of yours certifies it through a one-time code. With the storage disk mounted, as root on the node:

```bash
curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage
```

The same download and checks as above, then `redundanet node install`: the service user `redundanet3`, the data directory `/var/lib/redundanet3` (identity, manifest copy, census state, `versions/` for updates), the binary at `/var/lib/redundanet3/bin/redundanet` owned by the service user so the updater can rename a new version into place, a second copy at `/usr/local/bin/redundanet3` owned by root, which is the one you run (`sudo redundanet3 ...`), the unit `redundanet-agent`, the settings in `/etc/redundanet3/agent.env` (`NETWORK`, `CHANNEL`, `STORAGE_PATH`), the share store in `/mnt/storage/redundanet3` with the unit drop-in that makes it writable for the confined service, and a pairing code on the screen. It refuses a storage path that is not a mount point, so shares never land on the system disk by accident; `--allow-root-disk` on `node install` overrides that knowingly. The node needs at least 10 GiB free above its floor. `--channel early` is accepted here too.

On your laptop, with the phrase at hand, on the same network as the node or anywhere else:

```bash
redundanet device pair --storage
```

It asks for the code the node shows, then for the phrase.

The two reach each other directly on the same network, or through the hub when they cannot: the hub relays the same sealed frames and learns nothing but that a pairing happened. The node receives its identity and the network through the code, joins and starts; nothing is copied by hand. Your laptop records the node (key, host name, roles) in `devices.yaml` in its data directory, so `redundanet setup` and `redundanet join-request` put it in the join form; the node prints its `storage:` lines too. It contributes only once the maintainer lists it under `storage:` in the manifest.

No laptop at all, or a node that cannot reach the hub over HTTPS? The runbook's script keeps the certificate-file flow over SSH: [../agent/deploy/node/README.md](../agent/deploy/node/README.md).

On a node, name the full path in every command and run the ones that talk to the running agent as root:

```bash
sudo redundanet3 status
sudo redundanet3 logs -n 100
```

The local page stays on `127.0.0.1:4470`. Over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then `sudo redundanet3 open --print` and open the link it prints. To change the channel, edit `CHANNEL` in `/etc/redundanet3/agent.env` and `systemctl restart redundanet-agent`.

## Raspberry Pi

Each stable release carries a ready-to-boot image, `redundanet-rpi-<version>-arm64.img.xz`, with the agent installed as a storage node and nothing else. Flash it, boot it, and pair it from a device of yours with the code it writes to the SD card's boot partition (`redundanet device pair --storage`, which asks for the code, from anywhere: the hub relays the exchange; the code is the same after every reboot until the Pi is paired). Give it a disk labeled `REDUNDANET`: the Pi mounts it on its own and runs the agent only while it is mounted, so shares never land on the SD card; `sudo redundanet-install disk /dev/sda` over SSH formats one. The steps are in [rpi-image/README.md](../rpi-image/README.md). A Pi running any other 64-bit Linux is a Linux storage node: the one line above picks the `linux-arm64` release on its own. Raspberry Pi 5 is the tested board; Pi 4 has not been measured.

## A second device, without the phrase

On the new device, after the one line, `redundanet setup` offers it as the third choice; by hand:

```
redundanet identity pair
```

It shows a code and waits ten minutes. On a device of yours that already has its identity, anywhere, run this, paste the code when asked, then type the phrase:

```
redundanet device pair
```

Add `--storage` if the new device will contribute disk. The new device now holds its identity and the network; finish with `redundanet service install`. The phrase never leaves the device you typed it on; the code is good for one device and ten minutes. The two devices reach each other directly on the same network, or through the hub from anywhere: the code names the hub, and the hub relays the sealed frames without being able to read them. With the phrase at hand and no other device nearby, `setup`'s second choice, or `redundanet identity init --from-phrase`, puts your identity on the new device directly. For a machine with no second device beside it, the storage node flow above and the Raspberry Pi image remain.

## Windows

The Windows build passes the test suite but has not run on the live network yet. The one line above, in PowerShell, needs no administrator rights: the binary goes to `%AppData%\redundanet\bin\redundanet.exe`, that folder is added to your user PATH (terminals opened from then on find `redundanet`; the one you ran it in is told too), the service is a logon task that restarts on failure, and the page opens. The data directory is `%AppData%\redundanet`.

By hand instead: take `redundanet-<version>-windows-amd64.zip` from the newest release on the [download page](https://redundanet.com/download.html) and check its sha256 as described there; the binary inside is `redundanet-<version>-windows-amd64\redundanet.exe`. Put it at `%AppData%\redundanet\bin\redundanet.exe`, where the agent can replace it when it updates itself, add that folder to your PATH, open a terminal and run `redundanet version`. Then:

```powershell
redundanet service install     # a logon task with restart on failure; the agent serves its page with the setup
redundanet open                # the page walks you through the rest; or redundanet setup here
```

The Windows binary carries a version resource (product name and version in its properties) and is unsigned for now, so Windows may warn before it runs; the script checks the sha256 against the index, and so can you, as the download page shows.

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

Unpack the archive and put the binary where the agent can replace it: for a laptop that is `bin/redundanet` in the data directory, with a link on your PATH, which is what the script does (on Windows `bin\redundanet.exe` in `%AppData%\redundanet`, with that folder on your PATH); a copy owned by root in `/usr/local/bin` runs, but the agent cannot update it. A browser's download on macOS is quarantined: allow it once in System Settings, Privacy and Security, after the first refusal, or clear the mark with `xattr -d com.apple.quarantine ./redundanet`.

## Channels and updates

The service follows the `stable` channel unless installed with `--channel early`. The running agent checks the signed index on start and every hour, downloads a newer release that fits its channel (or one that the manifest's `min_version` requires), verifies hash and size, runs the candidate in probation for up to two minutes, keeps the old binary as `redundanet.old`, and rolls back by itself if the new one does not reconnect. A candidate that does not report healthy in time (a hub or a peer did not come back) is recorded and tried again by itself after 6 hours, then after 24 hours; after a third failure, or when the candidate exits by itself, the version waits for `redundanet update retry VERSION`. A probation cut short because the service was stopping counts for nothing. The candidate passes when every hub answers and the devices the agent had sessions with are back: all of them, or after 30 seconds at least half of all and at least half of the storage devices among them. A failed version that the manifest's `min_version` requires is tried once more for each new manifest revision. `redundanet update status` and the page say when the next attempt is.

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
