# Transport spike for RedundaNet 3

The two-week prototype that RFC 0001 puts before every other decision
(`rfcs/0001-redundanet-3.md`, "Spike plan and milestones"). It exists to
produce numbers, not code that survives: nothing here is meant to be merged
into a product tree, and the gate it serves is G0, a one-page decision at the
end of November 2026.

What it contains:

| Binary | What it does |
| --- | --- |
| `hub` | Rendezvous and relay. Reflects each device's public address, introduces pairs, forwards end-to-end QUIC packets it cannot read. QUIC on UDP 4433, optional TLS on TCP for UDP-blocked networks. |
| `agent` | The device side. One UDP socket, a channel to the hub, one QUIC connection per peer that moves between the relay and a direct path, and the phase 1 loopback bridge for Tahoe. |
| `natprobe` | Classifies a host's NAT (mapping and filtering behaviour, RFC 5780) and whether IPv6 works. |
| `cryptobench` | AES-256-GCM, ChaCha20-Poly1305, BLAKE3, SHA-256 and Reed-Solomon 2-of-4 throughput on one core and on four. |

Everything is Go 1.27 with quic-go 0.64, pion/stun, klauspost/reedsolomon and
zeebo/blake3. `./build.sh` cross-compiles all four binaries for Linux
(amd64, arm64), macOS (arm64, amd64) and Windows (amd64) into `build/`, with
no cgo.

## How the agent works

The design follows the RFC's Transport section, with the details the RFC left
to the spike filled in.

**Identity.** An Ed25519 key per device, stored as a PEM seed in `agent.key`
(created on first run). The device ID is the public key, 52 lowercase base32
characters. Every connection is TLS 1.3 with ALPN `redundanet-spike/0`, both
sides presenting a self-signed X.509 leaf whose key is the device key; the
verifier checks the self-signature and pins the expected ID. There are no
member keys and no manifest: the hub allows any device unless given
`-allow FILE`, and agents accept any peer. This is the spike's biggest
shortcut and must not be copied.

**One socket, several views.** The agent binds one UDP socket. Because
quic-go addresses packets by `net.Addr` and ties a connection to one
`Transport`, the socket is wrapped by a multiplexer that hands quic-go several
`net.PacketConn` views of it, one per path a connection may use: the default
view (listener, hub channel, and the accepting side of every peer connection),
a relay view per dialed peer, and a direct view per dialed peer. A relayed
peer gets a synthetic address, `fd72:6564:756e:6461::/64` plus the first eight
bytes of its ID, so the relay looks like any other path to quic-go.

**Hub channel.** One QUIC stream to the hub, or one TLS connection over TCP
when UDP gets no answer (`-hub-tcp`, forced with `-tcp-only`). Both carry the
same framing: type, length, payload; JSON for control, raw QUIC packets for
relay frames tagged with the peer ID. Relaying over a stream means head-of-line
blocking across every relayed peer, which the product would avoid with one
stream per peer; the throughput numbers below are per pair and unaffected.

**Finding a peer.** The smaller ID dials. Both sides learn the other's
candidates from the hub: the address the hub observed, every interface
address with the socket's port, and with `-loopback-candidates` the loopback
too. Both punch every candidate five times, 300 ms apart, with a small
unauthenticated packet the multiplexer intercepts; an answer confirms a direct
address. The dialer opens the QUIC connection through the relay at once, and
as soon as a direct address is confirmed migrates the live connection onto it
with quic-go's path API (`AddPath`, `Probe`, `Switch`): the accepting side sees
the client arrive from a new address and validates it, as RFC 9000 describes.
A keepalive every 2 s guards the direct path; 6 s of silence migrates the
connection back to the relay, and the next rendezvous (every 15 s while not
direct) tries again. Streams survive a migration.

**A quic-go limit, found here.** The accepting side reserves one of the
peer's connection IDs for every path it validates and never releases the one
it switches to (`path_manager.go`, `SwitchToPath` clears the list without
retiring the chosen path's ID). With the default budget of four IDs a
connection migrates exactly twice; the third probe is skipped with "no
connection ID is available". The agent counts migrations and, past two, opens
a fresh connection on the wanted path instead; streams on the old connection
break, which is the weaker property the RFC accepts (resume within seconds at
segment granularity). Worth an upstream issue; the product should not rely on
unlimited migrations until it is fixed.

**Bridge.** `-bridge 127.0.0.1:PORT=PEERID:TARGET` listens locally and
tunnels each accepted TCP connection over one QUIC stream to `127.0.0.1:TARGET`
on the peer, which must list TARGET in `-expose`. Half-closes propagate.

**Status.** `http://127.0.0.1:4481/status` on each agent (hub channel,
sessions with timestamped events, bridged connections, dropped packets),
`/switch?peer=ID&path=relay|direct` to force a path, `/connect?peer=ID`.
The hub serves `http://127.0.0.1:4480/status` with relayed bytes per device.

## Running it

### 1. NAT survey

The exit criterion is a direct path for the NAT types the live members have,
so first find out what they are:

```
./survey.sh alessandro@falcon alessandro@falcon-1 ptlug@redundanet ...
```

It cross-compiles `natprobe`, copies it to `/tmp` on each host, runs it on
the host (not in a container) and keeps `survey/<host>.json`. The two numbers
that matter: mapping "endpoint independent" with any filtering is
hole-punchable; mapping "address dependent" or "address and port dependent"
(symmetric NAT) means that site will relay unless the other side is open.
This Mac behind a Fritz!Box: endpoint-independent mapping, address-and-port
dependent filtering, which punches fine.

### 2. Hub on hub-hetzner

Not inside the existing stack: a spare port and a throwaway key.

```
scp build/linux-amd64/hub root@2.29.46.249:/usr/local/bin/spike-hub
ssh root@2.29.46.249 'spike-hub -key /root/spike-hub.key -udp :4433 -tcp :4443 -http 127.0.0.1:4480'
```

Open UDP 4433 and TCP 4443 in the firewall. The first log line prints the hub
ID, which every agent needs.

### 3. Agents on the two laptops and the Pi

```
./build/darwin-arm64/agent id -key agent.key          # prints this device's ID; once
./build/darwin-arm64/agent -key agent.key -hub HUBID@2.29.46.249:4433 -hub-tcp 2.29.46.249:4443 \
    -connect PEERID -expose 5201 -http 127.0.0.1:4481
```

Give each machine the other machines' IDs with `-connect`. Within a few
seconds `/status` shows the hub channel up, a session per peer with
`active: relay` and then `active: direct`, and the events give the two times
the RFC asks for: session created to "connected via relay", and to
"switched to direct". Record both for every pair, in both directions.

### 4. Throughput, relay and direct, against tinc

Each pair, each direction, three runs each:

```
agent ... -bench PEERID -bench-size 256 -bench-path relay
agent ... -bench PEERID -bench-size 256 -bench-path direct
```

The bench streams zeros over one QUIC stream and the peer counts them; the
result line gives Mbit/s and the path used. For the tinc baseline run
`iperf3 -s` on one node and `iperf3 -c <vpn address> -t 20` on the other,
inside the tinc namespace, the same pairs, the same hours. The RFC's bar is
relayed throughput at least equal to tinc's.

For the TCP 443 case start one agent with `-tcp-only` and bench both
directions; that is the hotel network number. For "same LAN" run two agents
on one network and check `/status` shows a direct address in the LAN range,
not the public one. For IPv6-only, `natprobe` says whether the site has it;
the candidates include global IPv6 addresses and a pair with only IPv6 between
them is a bench like any other.

### 5. Path switch with a transfer in flight

Start a long bench or a bridged transfer, then move the laptop from Wi-Fi to
the phone hotspot (the symmetric NAT case). The events show "direct path
silent", "switched back to relay", and later "switched to direct" again; the
bench either finishes (migration) or fails with the time it ran. Repeat with
the hub restarted mid-transfer. `/switch` forces the same transitions
without touching the network.

### 6. Tahoe through the bridge

Unmodified Tahoe nodes, each reaching the others only through loopback. Give
every device a port, say 40001 for the first laptop, 40002 for the second,
40003 for the Pi; the manifest will do this in phase 1. On every machine
bridge each other device to its port:

```
agent ... -expose 3457 \
    -bridge 127.0.0.1:40002=ID_LAPTOP2:3457 -bridge 127.0.0.1:40003=ID_PI:3457
```

In each node's `tahoe.cfg`:

```
[node]
tub.port = tcp:3457:interface=127.0.0.1
tub.location = tcp:127.0.0.1:40001       # this device's own port
web.port = tcp:3456:interface=127.0.0.1
[storage]
force_foolscap = true
```

A throwaway introducer runs on one laptop the same way (its own port,
`-expose` its tub). `tahoe put` and `tahoe get` through it are the
end-to-end test; `/status` shows the bridged connections and the path they
took.

### 7. CPU on the Pi

```
scp build/linux-arm64/cryptobench pi:/tmp/ && ssh pi /tmp/cryptobench
```

plus `top` on the Pi during a 20 Mbit/s bench (`-bench-size` large enough to
run a minute). The bar is under 20 % CPU at 20 Mbit/s; the cipher table picks
AES-GCM or ChaCha20 for the native engine and shows whether 2-of-4 encoding
is ever the bottleneck.

### 8. iroh beside it

The RFC wants iroh measured on the same pairs so the fallback is judged on
numbers. Nothing here does that; `iroh-doctor` from the iroh releases
(`iroh-doctor accept` on one machine, `iroh-doctor connect <id>` on the other)
reports whether the pair went direct and the throughput, with the same
hole-punching problem and a hosted relay. Run it right after the agent bench
on each pair.

## Exit criteria, to fill in

| Criterion | Bar | Result |
| --- | --- | --- |
| Direct path for the live NAT types | every pair of non-symmetric sites | |
| Relayed throughput | at least tinc's | |
| Pi CPU at 20 Mbit/s | under 20 % | |
| Resume after a path switch | within seconds | |
| TCP 443 fallback | works, number recorded | |
| Cipher | chosen from the Pi table | |
| Wire identity | as built here, or the change recorded | self-signed Ed25519 leaf, 52-char base32 ID, ALPN `redundanet-spike/0` |

The decision page goes in `rfcs/` as the G0 record.

## What the code already tells us

From one machine, before any network test: the relay handshake takes about
200 ms on loopback (hub channel, then the end-to-end handshake through it);
punching confirms a direct address in the first round; probe and switch to
the direct path take under a millisecond locally; 20 MB through the bridge
arrive intact on every path and across forced switches; the migration budget
of two per connection is real and the reconnect fallback works. On an M-series
Mac, AES-256-GCM seals at about 5 GB/s per core, ChaCha20-Poly1305 at
0.9 GB/s, BLAKE3 (pure Go on arm64) at 0.6 GB/s, SHA-256 at 2.2 GB/s, and
Reed-Solomon 2-of-4 encodes at 8.7 GB/s. The Pi numbers are the ones that
count.

## Not here on purpose

No manifest, no member keys, no allowlist, no revocation, no bandwidth caps,
no port mapping (UPnP, NAT-PMP, PCP), no relay failover, no self-update. Punch
packets are unauthenticated and the bridge exposes whatever `-expose` lists
to any peer. Run it on throwaway keys and ports, then delete it.
