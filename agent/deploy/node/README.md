# The RedundaNet 3 agent on a storage node

One static binary under a system unit, no container: the agent runs beside
the RedundaNet 2 stack on the host's network, reports the census, keeps its
sessions, and updates itself from the signed release index with probation
and rollback. Nothing is stored under RedundaNet 3 until phase 2.

Three steps, the middle one on the member's laptop because that is where the
phrase lives (the RFC's headless flow):

```bash
# 1. on the node, as root: user, directories, binary, unit; prints the device key
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/install.sh
curl -fsSLO https://raw.githubusercontent.com/adefilippo83/redundanet/main/agent/deploy/node/redundanet-agent.service
sudo STORAGE_PATH=/mnt/storage CHANNEL=stable bash install.sh prepare 3.0.0-alpha.2

# 2. on the laptop: certify the node's device key with the member key
redundanet identity certify --network redundanet --device <KEY> --storage > node.cert
scp node.cert node:

# 3. on the node: accept, join, start; the certificate file is destroyed afterwards
sudo bash install.sh finish node.cert
```

`prepare` checks the download against the sha256 in `releases/index.yaml`
on main. `finish` joins the network with the descriptor derived from the
manifest on main and enables `redundanet-agent.service`.

Layout: `/var/lib/redundanet3` (identity, manifest copy, census state,
`versions/` for updates), `/var/lib/redundanet3/bin/redundanet` (the binary,
owned by the service user so the updater can rename a new version into
place), `/etc/redundanet3/agent.env` (`NETWORK`, `CHANNEL`, `STORAGE_PATH`).
Change the channel there and `systemctl restart redundanet-agent`.

No inbound firewall rule is needed: the agent reaches the hub outbound and
peers through hole punching or the relay. The local page is on
`127.0.0.1:4470`; over SSH: `ssh -L 4470:127.0.0.1:4470 node`, then
`sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet open --print --data-dir /var/lib/redundanet3`.

After the node is up, the member asks the operator to list it under
`storage:` in the manifest, which is what makes its contribution count.
