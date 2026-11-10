# The RedundaNet 3 agent on a storage node

One static binary under a system unit, no container: the agent runs beside
the RedundaNet 2 stack on the host's network, reports the census, keeps its
sessions, and updates itself from the signed release index with probation
and rollback. A storage node also serves shares of the native grid from
`STORAGE_PATH/redundanet3`.

A Raspberry Pi can boot the ready-made image instead, which ships this
layout with a first boot that shows a pairing code on the SD card and a
storage disk found by its label: [rpi-image/README.md](../../../rpi-image/README.md).

The short way is one line as root on the node and one command on the
laptop. The line downloads the newest release, checks it against the
signed index and runs `redundanet node install`, which lays the node out
and shows a pairing code; the laptop, with the phrase at hand, on the
node's network or anywhere (the hub relays the exchange when the two
cannot reach each other), certifies the node through the code:

```bash
curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage   # on the node
redundanet device pair --storage                                                   # on the laptop: asks for the code the node shows
```

`node install` refuses a storage path that is not a mount point unless
told `--allow-root-disk`, keeps `agent.env` and the identity on a second
run, and prints the `storage:` lines to send for the manifest once paired;
`node status` and `node uninstall` go with it (the data directory and the
store stay). The network is built into the binary: the node joins it on
its own, and `--network` is only for another one. `install.sh` below makes
the same layout from a script, which the Pi image uses, and keeps the
certificate-file flow for a node the laptop cannot reach.

With a laptop of the member anywhere, pairing is two commands after
`prepare`:

```bash
sudo bash install.sh pair                        # on the node: shows a code and waits
redundanet device pair --storage                 # on the laptop: asks for the code, then the phrase
```

The node joins and starts by itself. For a node with no laptop of the
member at all, or one that cannot reach the hub over HTTPS, the
certificate file flow, three steps with the middle one on the laptop (the
RFC's headless flow):

```bash
# 1. on the node, as root: user, directories, binary, unit; prints the device key
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/install.sh
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/redundanet-agent.service
sudo STORAGE_PATH=/mnt/storage CHANNEL=stable bash install.sh prepare 3.0.6

# 2. on the laptop: certify the node's device key with the member key
redundanet identity certify --device <KEY> --storage > node.cert
scp node.cert node:

# 3. on the node: accept, join, start; the certificate file is destroyed afterwards
sudo bash install.sh finish node.cert
```

`prepare` checks the download against the sha256 in `releases/index.yaml`
on main. `finish` joins the network built into the binary and enables
`redundanet-agent.service`.

The store lives in `STORAGE_PATH/redundanet3`, owned by the service user,
and a unit drop-in (`redundanet-agent.service.d/storage.conf`) makes it
writable for the confined service; `prepare` sets both up. A node installed
before the store existed gets them with `sudo bash install.sh storage`.
`sudo bash install.sh disk /dev/sdX` makes a storage disk of a whole
device: ext4 labeled `REDUNDANET`, an `/etc/fstab` line that mounts it at
`STORAGE_PATH` whenever it is attached, the store directory, and the agent
started. Everything on the device is destroyed, so the device name is asked
back unless `--yes`.

Layout: `/var/lib/redundanet3` (identity, manifest copy, census state,
`versions/` for updates), `/var/lib/redundanet3/bin/redundanet` (the
service's binary, owned by the service user so the updater can rename a new
version into place), `/usr/local/bin/redundanet3` (a copy root owns: the
command you run, `sudo redundanet3 ...`), `/etc/redundanet3/agent.env` (`NETWORK`, `CHANNEL`, `STORAGE_PATH`).
Change the channel there and `systemctl restart redundanet-agent`.

No inbound firewall rule is needed: the agent reaches the hub outbound and
peers through hole punching or the relay. The local page is on
`127.0.0.1:4470`; over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then
`sudo redundanet3 open --print`. As root, the
commands that talk to the running agent (`status`, `backup`, `restore`,
`open`, `logs`, `update`) find the node's data directory on their own;
the identity and join commands run as the service user with `--data-dir`,
as the installer does.

Root does not run the service's binary. The service user owns that file,
so a fault in the agent could replace it, and the next `sudo` of it would
run whatever is there as root. `sudo redundanet3` is the copy root owns;
it does not follow the service's updates. An older copy keeps talking to a
newer agent as long as the local API it uses is still there; run the node's
one-line installer again to refresh it. A node installed before 3.0.16 has no such copy until
the installer is run once more: until then its documented commands were
`sudo /var/lib/redundanet3/bin/redundanet ...`. `migrate-node --rollback`
builds its command from the stack's own files and takes only the service
names from the marker in the data directory, each checked.

After the node is up, the member asks the operator to list it under
`storage:` in the manifest, which is what makes its contribution count.

## Leaving RedundaNet 2 (phase 3)

Once the native grid holds what this node pushed to the old one, the
Docker stack can go. Not before the network's stop date is announced: a
node's Tahoe shares hold other members' data too, and the old grid is
2-of-4. The agent does the retirement and refuses while anything is
missing:

```bash
sudo redundanet3 migrate-node --dry-run   # the plan, and what is not yet on the native grid
sudo redundanet3 migrate-node             # stops and removes the containers; files and shares stay
sudo redundanet3 migrate-node --rollback  # every service again, together, tinc first
```

The agent keeps running through all of it and the node stays on the
status page. The Tahoe services sit behind compose profiles and share
tinc's network namespace, so the stack is only ever stopped and started
whole: the plan names every profile on the way down, fails and puts the
stack back if any container survives, and the rollback recreates every
recorded service in one `up`, never tinc alone. A drill is `migrate-node` followed by `--rollback` within a
few minutes, inside the old grid's tolerance of two servers down.
