# The RedundaNet 3 hub on hub-hetzner

The hub runs on the host beside the RedundaNet 2 containers, as the RFC's
migration step 0 says: relay on UDP and TCP 8443, census collector, status
page on loopback, served to the world as <https://hub.redundanet.com/> by
Caddy on the same host. One static binary, one system user, one systemd
unit, no Docker.

What it needs before the first start, in this order:

1. **A release that has the hub features**: `agent/v3.0.0` or later
   in `releases/index.yaml`.
2. **The name and the web server**: `hub.redundanet.com` resolves to the
   host; ports 80 and 443 TCP are open in the Hetzner cloud firewall and
   in nftables (`tcp dport { 80, 443 } accept`); Caddy is installed from
   Debian's package with `Caddyfile` from this directory at
   `/etc/caddy/Caddyfile` (`systemctl reload caddy`). Caddy fetches and
   renews the certificate by itself and proxies to the loopback page.
3. **The manifest lists the hub**: its device key under `hubs:` with
   `relay: hub.redundanet.com:8443`, `tcp: hub.redundanet.com:8443` and
   `status: https://hub.redundanet.com/`, in a manifest
   signed by the operator key (`manifest.sig` on main). The hub refuses to
   start otherwise.

## Install

As root on the host:

```bash
# user and directories
useradd --system --home /var/lib/redundanet3 --shell /usr/sbin/nologin redundanet3
install -d -o redundanet3 -g redundanet3 -m 0750 /var/lib/redundanet3

# binary: the asset named in releases/index.yaml, hash checked against it
version=3.0.0
curl -fsSL -o /tmp/rn.tgz "https://github.com/adefilippo83/redundanet/releases/download/agent%2Fv$version/redundanet-$version-linux-amd64.tar.gz"
echo "<sha256 from releases/index.yaml>  /tmp/rn.tgz" | sha256sum -c
install -d -o redundanet3 -g redundanet3 -m 0750 /var/lib/redundanet3/bin
tar -xzf /tmp/rn.tgz -C /tmp && install -o redundanet3 -g redundanet3 -m 0755 "$(find /tmp -path '*linux-amd64/redundanet' | head -1)" /var/lib/redundanet3/bin/redundanet
# a copy root owns, for the commands you run as root: the one above is the
# hub's own, which its user can replace
install -o root -g root -m 0755 "$(find /tmp -path '*linux-amd64/redundanet' | head -1)" /usr/local/bin/redundanet3
redundanet3 version

# the hub's identity: prints the device key that goes into the manifest
sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet identity init --yes --data-dir /var/lib/redundanet3
```

Put the device key in the manifest's `hubs:` entry, sign, merge. Then join
the network (the descriptor is built into the binary; a second hub needs a
release whose descriptor lists it first, see `redundanet network
descriptor`):

```bash
sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet network join --data-dir /var/lib/redundanet3
install -m 0644 redundanet-hub.service /etc/systemd/system/redundanet-hub.service
systemctl daemon-reload && systemctl enable --now redundanet-hub
journalctl -u redundanet-hub -f
```

The status page also answers `/pair/`: the mailbox through which two
devices of a member pair when they cannot reach each other (a room per
pairing, derived from the code; sealed frames the hub cannot read; ten
minutes). Caddy proxies every path, so nothing to configure.

The page is public, so the hub bounds what a caller costs it. The status is
computed once in five seconds however many ask. One source address opens at
most eight pairing rooms at a time, and a room holds at most two full frames
nobody has read. The hub tells sources apart by the address Caddy writes in
`X-Forwarded-For`, which it reads only on a connection from loopback, and
of which it takes the last address, the one Caddy itself saw, so a caller
cannot choose it. Keep the page on `127.0.0.1` behind Caddy. With another
proxy in front of Caddy every caller would share that proxy's address and
its bound of eight. Without Caddy, every caller counts by its own address.

## Firewall

Both layers of the host firewall need 8443, UDP and TCP:

- Hetzner Cloud Firewall `hub`: two inbound rules, `udp 8443` and
  `tcp 8443`, source `0.0.0.0/0` and `::/0`
  (`hcloud firewall add-rule hub --direction in --protocol udp --port 8443 --source-ips 0.0.0.0/0 --source-ips ::/0`,
  and the same with `tcp`).
- `/etc/nftables.conf`, in `chain input` next to the tinc rules:
  `tcp dport 8443 accept` and `udp dport 8443 accept`, then
  `nft -f /etc/nftables.conf`.

The hub serves the status page on loopback (`127.0.0.1:4480`) and Caddy
publishes it as <https://hub.redundanet.com/> with `/status.json` beside
it. Over SSH it is `ssh -L 4480:127.0.0.1:4480 root@hub-hetzner` as well.

The hub depends on nothing outside its host for the page. The unit no
longer publishes a copy to the fly.io app of RedundaNet 2 (`--publish`,
`--publish-token-file`) and no longer mirrors that product's status
(`--legacy-status`). A host installed before that change still has the
three lines: replace `/etc/systemd/system/redundanet-hub.service` with
the file here, remove `/etc/redundanet3/publish.token`,
`systemctl daemon-reload`, and restart the hub at a quiet moment.

## Check

- `journalctl -u redundanet-hub` shows `hub up`, `status page up`, and a `device up` line per device that connects.
- `curl -s https://hub.redundanet.com/status.json | head -c 300` names the
  network and the hub, with `generated_at` within the last minute, and
  `curl -sI https://hub.redundanet.com/ | head -1` answers 200 over TLS.
- `/var/lib/redundanet3/census.json` appears after the first heartbeat,
  `placement.sqlite` with the first share census; the repair tasks the
  hub issues are in its `tasks` table, and `repair task issued` lines
  in the journal name the worker and the object.

## Update

The hub updates itself like a node: every hour it checks the signed release
index on its channel (`--channel early` in the unit), downloads the asset,
checks its hash, hands the relay, the status listener and the collector to
the candidate, which runs `hub --probation` on the same ports and reports
healthy once the devices that were connected are back on a channel: all of
them, or after thirty seconds at least half of all and at least half of
the storage devices among them, since a laptop may have gone to sleep
meanwhile and one storage device that stays away must not decide the hub's
update (most of them staying away is the candidate's fault until proven
otherwise).
Then the new binary is renamed into place beside `redundanet.old` and the
hub exits for systemd to restart it. Devices see two reconnects of a few seconds. A
candidate that fails is recorded in `/var/lib/redundanet3/updates/state.yaml`.
One that did not report healthy in time is tried again after 24 hours, and
once more 24 hours after that; after a third failure, or when the candidate
exited by itself, it waits for `redundanet update retry VERSION`. Each
attempt is two reconnects for every device again. The journal line from the
candidate names the devices that did not come back, and the line of the
running hub says when the next attempt is.

By hand, for an emergency: download the asset, check its hash, `install` it
over `/var/lib/redundanet3/bin/redundanet` owned by `redundanet3`, and
`systemctl restart redundanet-hub`.
