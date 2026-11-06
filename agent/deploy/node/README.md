# The RedundaNet 3 agent on a storage node

One static binary under a system unit, no container: the agent runs beside
the RedundaNet 2 stack on the host's network, reports the census, keeps its
sessions, and updates itself from the signed release index with probation
and rollback. A storage node also serves shares of the native grid from
`STORAGE_PATH/redundanet3`.

A Raspberry Pi can boot the ready-made image instead, which ships this
layout with a first boot that hands the device key over on the SD card:
[rpi-image/README.md](../../../rpi-image/README.md).

Two ways to certify the node, both keeping the phrase on the laptop. With
the laptop on the node's network, pairing is two commands after `prepare`:

```bash
sudo bash install.sh pair                                        # on the node: shows a code and waits
redundanet device pair --network redundanet --storage <code>     # on the laptop, with the phrase
```

The node joins and starts by itself. For a node reached over SSH only, the
certificate file flow, three steps with the middle one on the laptop (the
RFC's headless flow):

```bash
# 1. on the node, as root: user, directories, binary, unit; prints the device key
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/install.sh
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/redundanet-agent.service
sudo STORAGE_PATH=/mnt/storage CHANNEL=stable bash install.sh prepare 3.0.0

# 2. on the laptop: certify the node's device key with the member key
redundanet identity certify --network redundanet --device <KEY> --storage > node.cert
scp node.cert node:

# 3. on the node: accept, join, start; the certificate file is destroyed afterwards
sudo bash install.sh finish node.cert
```

`prepare` checks the download against the sha256 in `releases/index.yaml`
on main. `finish` joins the network with the descriptor derived from the
manifest on main and enables `redundanet-agent.service`.

The store lives in `STORAGE_PATH/redundanet3`, owned by the service user,
and a unit drop-in (`redundanet-agent.service.d/storage.conf`) makes it
writable for the confined service; `prepare` sets both up. A node installed
before the store existed gets them with `sudo bash install.sh storage`.

Layout: `/var/lib/redundanet3` (identity, manifest copy, census state,
`versions/` for updates), `/var/lib/redundanet3/bin/redundanet` (the binary,
owned by the service user so the updater can rename a new version into
place), `/etc/redundanet3/agent.env` (`NETWORK`, `CHANNEL`, `STORAGE_PATH`).
Change the channel there and `systemctl restart redundanet-agent`.

No inbound firewall rule is needed: the agent reaches the hub outbound and
peers through hole punching or the relay. The local page is on
`127.0.0.1:4470`; over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then
`sudo /var/lib/redundanet3/bin/redundanet open --print`. As root, the
commands that talk to the running agent (`status`, `backup`, `restore`,
`open`, `logs`, `update`) find the node's data directory on their own;
the identity and join commands run as the service user with `--data-dir`,
as the installer does.

After the node is up, the member asks the operator to list it under
`storage:` in the manifest, which is what makes its contribution count.

## Leaving RedundaNet 2 (phase 3)

Once the native grid holds what this node pushed to the old one, the
Docker stack can go. Not before the network's stop date is announced: a
node's Tahoe shares hold other members' data too, and the old grid is
2-of-4. The agent does the retirement and refuses while anything is
missing:

```bash
sudo /var/lib/redundanet3/bin/redundanet migrate-node --dry-run   # the plan, and what is not yet on the native grid
sudo /var/lib/redundanet3/bin/redundanet migrate-node             # stops and removes the containers; files and shares stay
sudo /var/lib/redundanet3/bin/redundanet migrate-node --rollback  # every service again, together, tinc first
```

The agent keeps running through all of it and the node stays on the
status page. The Tahoe services sit behind compose profiles and share
tinc's network namespace, so the stack is only ever stopped and started
whole: the plan names every profile on the way down, fails and puts the
stack back if any container survives, and the rollback recreates every
recorded service in one `up`, never tinc alone. A drill is `migrate-node` followed by `--rollback` within a
few minutes, inside the old grid's tolerance of two servers down.
