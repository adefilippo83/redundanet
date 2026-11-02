# The RedundaNet 3 hub on hub-hetzner

The hub runs on the host beside the RedundaNet 2 containers, as the RFC's
migration step 0 says: relay on UDP and TCP 8443, census collector, status
page on loopback, published to the fly.io page under `/v3/`. One static
binary, one system user, one systemd unit, no Docker.

What it needs before the first start, in this order:

1. **A release that has the hub features**: `agent/v3.0.0-alpha.2` or later
   in `releases/index.yaml`.
2. **The fly.io side**: the `REDUNDANET_V3_PUBLISH_TOKEN` secret set and the
   2.7.41 hub deployed (`docs/bootstrap-hub.md`, "RedundaNet 3 status").
3. **The manifest lists the hub**: its device key under `hubs:` with
   `relay: 2.29.46.249:8443` and `tcp: 2.29.46.249:8443`, in a manifest
   signed by the operator key (`manifest.sig` on main). The hub refuses to
   start otherwise.

## Install

As root on the host:

```bash
# user, directories, token
useradd --system --home /var/lib/redundanet3 --shell /usr/sbin/nologin redundanet3
install -d -o redundanet3 -g redundanet3 -m 0750 /var/lib/redundanet3
install -d -m 0750 -g redundanet3 /etc/redundanet3
install -m 0640 -g redundanet3 /dev/stdin /etc/redundanet3/publish.token <<< "$TOKEN"   # the fly secret's value

# binary: the asset named in releases/index.yaml, hash checked against it
version=3.0.0-alpha.2
curl -fsSL -o /tmp/rn.tgz "https://github.com/adefilippo83/redundanet/releases/download/agent%2Fv$version/redundanet-$version-linux-amd64.tar.gz"
echo "<sha256 from releases/index.yaml>  /tmp/rn.tgz" | sha256sum -c
install -d -o redundanet3 -g redundanet3 -m 0750 /var/lib/redundanet3/bin
tar -xzf /tmp/rn.tgz -C /tmp && install -o redundanet3 -g redundanet3 -m 0755 "$(find /tmp -path '*linux-amd64/redundanet' | head -1)" /var/lib/redundanet3/bin/redundanet
/var/lib/redundanet3/bin/redundanet version

# the hub's identity: prints the device key that goes into the manifest
sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet identity init --network redundanet --yes --data-dir /var/lib/redundanet3
```

Put the device key in the manifest's `hubs:` entry, sign, merge. Then join
the network with the descriptor printed by
`redundanet network descriptor --manifest manifests/manifest.yaml --repository https://raw.githubusercontent.com/adefilippo83/redundanet/main`:

```bash
sudo -u redundanet3 /var/lib/redundanet3/bin/redundanet network join --data-dir /var/lib/redundanet3 'rn3:net:...'
install -m 0644 redundanet-hub.service /etc/systemd/system/redundanet-hub.service
systemctl daemon-reload && systemctl enable --now redundanet-hub
journalctl -u redundanet-hub -f
```

## Firewall

Both layers of the host firewall need 8443, UDP and TCP:

- Hetzner Cloud Firewall `hub`: two inbound rules, `udp 8443` and
  `tcp 8443`, source `0.0.0.0/0` and `::/0`
  (`hcloud firewall add-rule hub --direction in --protocol udp --port 8443 --source-ips 0.0.0.0/0 --source-ips ::/0`,
  and the same with `tcp`).
- `/etc/nftables.conf`, in `chain input` next to the tinc rules:
  `tcp dport 8443 accept` and `udp dport 8443 accept`, then
  `nft -f /etc/nftables.conf`.

The status page stays on loopback (`127.0.0.1:4480`); read it over SSH with
`ssh -L 4480:127.0.0.1:4480 root@hub-hetzner` or on the fly page under
`/v3/`.

## Check

- `journalctl -u redundanet-hub` shows `hub up`, `status page up`,
  `status published`, and a `device up` line per device that connects.
- `curl -s https://redundanet-hub.fly.dev/v3/status.json | head -c 300`
  names the network and the hub, with `X-Published-At` within the last
  minute.
- `/var/lib/redundanet3/census.json` appears after the first heartbeat.

## Update

The hub updates itself like a node: every hour it checks the signed release
index on its channel (`--channel early` in the unit), downloads the asset,
checks its hash, hands the relay, the status listener and the collector to
the candidate, which runs `hub --probation` on the same ports and reports
healthy once every device that was connected is back on a channel, then
renames the new binary into place beside `redundanet.old` and exits for
systemd to restart it. Devices see two reconnects of a few seconds. A
candidate that fails is recorded in `/var/lib/redundanet3/updates/state.yaml`
and never retried until `redundanet update retry VERSION`.

By hand, for an emergency: download the asset, check its hash, `install` it
over `/var/lib/redundanet3/bin/redundanet` owned by `redundanet3`, and
`systemctl restart redundanet-hub`.
