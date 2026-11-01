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
| `tcpbench` | Plain TCP sink and source with random data, for the tinc baseline inside the tinc namespace. |
| `portmap` | Asks the router for a two-minute UDP mapping with PCP, NAT-PMP and UPnP in turn, reports each, releases it. |

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

**Finding a peer.** The device that asks the hub dials; when both have asked
for each other, the smaller ID dials. Both sides learn the other's candidates
from the hub: the address the hub observed, every interface
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
`/switch?peer=ID&path=relay|direct` to force a path, `/connect?peer=ID`,
`/blackhole?peer=ID&on=1|0` to make the current direct address unreachable
(the path-loss test). `SPIKE_TRACE=1` logs every relayed packet.
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
| Direct path for the live NAT types | every pair of non-symmetric sites | IPv6 yes, every pair, first attempt; public IPv4 none, and there is no non-symmetric pair: the Pi site maps per destination port and the x86 site is symmetric |
| Relayed throughput | at least tinc's | not comparable on the measured pair: the x86 site's uplink to Helsinki caps at about 10 Mbit/s over UDP and TCP alike, and tinc (16 to 24) evidently does not go through the hub there; Pi to x86 the relay wins, 23 to 29 against 14 to 22 |
| Pi CPU at 20 Mbit/s | under 20 % | Pi 5: about 15 % of one core receiving, 7 % sending (scaled from 40 Mbit/s) |
| Resume after a path switch | within seconds | yes with a 30-line quic-go patch: direct path cut mid-transfer, 6 s detection, 112 ms switch to the relay, the stream survived; without the patch quic-go never probes the new path |
| TCP 443 fallback | works, number recorded | works: 17 to 18 Mbit/s Mac to Pi (UDP relay 10 to 34 in the same hour) |
| Cipher | chosen from the Pi table | AES-256-GCM (1.7 GB/s per core on the Pi 5) |
| Wire identity | as built here, or the change recorded | self-signed Ed25519 leaf, 52-char base32 ID, ALPN `redundanet-spike/0` |

## Results, 2026-11-01

Hub on hub-hetzner (Helsinki), agents on a MacBook (Fritz!Box, Germany), the
Pi 5 node falcon-1 and the x86 node falcon. 64 or 128 MiB per run.

**NAT survey.** Mac: endpoint-independent mapping, address-and-port-dependent
filtering, global IPv6. falcon-1 (Pi 5): a mapping that depends on the
destination port (two STUN servers on different ports saw two different
public ports; the RFC 5780 test, which keeps the port, called it endpoint
independent and `natprobe` now reports the simple test's verdict instead),
address-and-port-dependent filtering, global IPv6. falcon (x86):
address-and-port-dependent mapping and filtering (symmetric), no IPv6: it can
only ever relay.

**Connection times.** Hub channel about 120 ms; relay handshake 105 to 150 ms;
first byte possible about 280 ms after the agent starts; direct path (IPv6)
about 0.5 s after start, probe and switch 25 to 140 ms.

| Pair, direction | Path | Mbit/s |
| --- | --- | --- |
| Mac to Pi | direct (IPv6) | 50.4 |
| Pi to Mac | direct (IPv6) | 41.2, 40.8, 41.2 |
| Mac to Pi | relay | 10.3, 12.0, 33.9, 20.8, 6.3 |
| Pi to Mac | relay | 29.3, 27.4, 29.2, 32.6 |
| Mac to Pi | relay over TLS/TCP (`-tcp-only`) | 18.1, 17.0 |
| Mac to x86 | relay (symmetric NAT) | 27.5 |
| x86 to Mac | relay | 9.4, 8.4 |
| x86 to Pi | relay (stream, then datagrams) | 8.2, 11.4, 7.1 / 11.5, 9.6, 10.0 |
| x86 to Pi | relay over TLS/TCP | 7.6, 10.1 |
| x86 to Pi | tinc, interleaved with the line above | 19.6, 15.1, 22.5 / 24.0, 21.0, 16.2 |
| Pi to x86 | relay | 21.7, 22.2, 20.6, 29.0, 29.1, 23.0, 25.6 |
| Pi to x86 | tinc | 15.3, 14.4, 14.3, 21.7 |

The first "relay" numbers published this afternoon (Mac to Pi 34.4, Pi to
Mac 37.6, TCP 34.8) were wrong: a relay bench could still be migrated to the
direct path by a punch landing mid-run. `-bench-path relay` now disables
direct paths for the bench, and the rows above are relay only. The tinc
baseline sends random bytes: the live network runs tinc with
`Compression = 9`, and zeros would have measured zlib.

Every number moves by a factor of two to three within minutes on these home
links (Mac to Pi relay: 6.3 to 33.9). Compare only interleaved runs.

**CPU on the Pi 5** (top, percent of one core, at about 40 Mbit/s over the
direct path): 27 to 30 % receiving, 13 % sending. The mux wraps the socket,
so quic-go cannot use GSO, recvmmsg or set buffer sizes ("connection doesn't
allow setting of receive buffer size"); the product should give quic-go the
raw socket or reimplement those.

**cryptobench, Pi 5** (MB/s, one core / four): AES-256-GCM 1717 / 6641,
ChaCha20-Poly1305 396 / 1556, BLAKE3 333 / 1321 (pure Go), SHA-256 1488 /
5963, Reed-Solomon 2-of-4 encode 4279 / 5196, reconstruct 2056 / 2803.

**Findings to carry into G0.**

1. quic-go cannot leave a dead path while data is in flight. Path probes are
   only sent from `sendPackets`, which runs only when the current path may
   send (`SendAny`); a path that died with a full congestion window never gets
   there, so the PATH_CHALLENGE for the new path is never sent. Reproduced on
   a laptop through the Hetzner hub (direct path blackholed mid-transfer with
   `/blackhole`), fixed by sending pending probes before the send-mode check:
   `patches/quic-go-v0.64.1-probe-when-congestion-limited.patch`, about 30
   lines plus an integration test that fails without it (3 of 3) and passes
   with it (5 of 5); quic-go's own suites still pass. With it, 700 MiB
   arrived intact across the loss: 6 s to notice, 112 ms to switch, then
   about half the fresh relay rate while congestion control recovers. Issue
   and pull request drafts are in `patches/README.md`, not filed.
2. A forced switch from a busy fast path to a slower one fails for another
   reason: the receiver drops the probes as "potentially duplicate", because
   thousands of newer packets arrived on the fast path while they crossed the
   relay. RFC 9000 allows that drop. In practice the agent leaves a path only
   when it is dead, which is case 1; a deliberate switch should pause sending
   for one RTT of the slower path.
3. The accepting side keeps the connection ID of every path it switched to,
   so after two migrations the pool of four is empty and the third probe is
   never answered. The agent reconnects after two migrations; upstream issue
   material too.
4. Relay throughput is set by the slower uplink, not by the relay design here.
   The x86 site manages about 10 Mbit/s to Helsinki whether the relay runs
   over QUIC streams, QUIC datagrams or TLS over TCP. Tinc does 16 to 24
   between the same two nodes, so for that pair it must not be going through
   the hub: likely a port forward for UDP 655 on falcon-1's router, which the
   spike cannot use because its socket port is random. RedundaNet 2's
   install docs ask operators to open 655/udp, so its direct paths come from
   hand-made forwards. Automatic port mapping is unavailable at all three
   sites today: both Fritz!Boxes refuse PCP ("not authorized") and UPnP until
   "allow independent port sharing" is enabled for the device, and the x86
   site is double-NATed (PCP maps on the inner router, onto 192.168.1.2).
   The agent has `-portmap`; enabling port sharing for falcon-1 on its
   Fritz!Box is the one router change that would let the x86 node reach the
   Pi directly and settle the comparison with tinc.
5. Relayed packets now travel as QUIC datagrams, with a bounded queue at the
   hub and in each agent that drops instead of buffering. No throughput gain
   measured (the links are the limit), but a congested leg now shows up as
   loss to the end-to-end connection instead of seconds of hidden queue.
   Inner packets are capped at 1200 bytes so one fits a datagram.
6. Public IPv4 punching between the Mac and the Pi failed with only the
   reflexive candidate. The survey explains it: the Pi site's mapping depends
   on the destination port, so the port the hub sees is not the port the Mac
   must hit; `natprobe` had called it punchable because the RFC 5780 tests
   keep the destination port. IPv6 punched at once.
7. Overlay addresses (Tailscale) are left out of the candidates by default
   (`-include-overlay` brings them back), otherwise an overlay's relay passes
   for a direct path.

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
