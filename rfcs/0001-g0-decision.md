# RFC 0001, gate G0: the transport decision

- Status: Passed
- Date: 2026-11-01
- Spike: `spike/` on the branch `spike/transport`; `spike/README.md` has the
  measurement protocol and every number
- Consequences: written into version 4 of [RFC 0001](0001-redundanet-3.md)

G0 is the gate the RFC put before every other decision: a prototype of the
transport, measured on the live network, and a one-page decision with the
numbers. The spike was built and measured in one day, on 2026-11-01, rather
than the two weeks planned, because the questions it had to answer turned out
to be cheaper to ask than to argue about.

## Decisions

1. **Go.** The transport, the relay and the benches were written in Go in one
   day with quic-go, pion/stun, klauspost/reedsolomon and zeebo/blake3, and
   cross-compile to Linux, macOS and Windows without cgo. Rust is no longer
   on the table.
2. **quic-go, with a path layer of our own.** The agent keeps one QUIC
   connection per peer and moves it between the relay and a direct path with
   quic-go's path API; the multiplexer that makes this possible is about 300
   lines, the session layer about 600. iroh was not measured: the result that
   would have sent the design to iroh, hole punching falling short, happened,
   and a library cannot change it (see Direct paths below).
3. **Relay over QUIC datagrams, with TLS over TCP 443 as the fallback.** Both
   carriers measured; the fallback costs nothing where UDP works and is the
   only path where it does not.
4. **AES-256-GCM** for segment encryption, with the cipher id in the format so
   ChaCha20-Poly1305 remains possible.
5. **Wire identity as built:** a self-signed X.509 leaf whose key is the
   Ed25519 device key, the device ID as 52 lowercase base32 characters, ALPN
   `redundanet/1`. The member-signed device certificate goes in a critical
   extension, as the RFC says; the spike ran without it because it has no
   manifest.
6. **The engine comes first.** The RFC planned a phase 1 that put the binary
   on Tahoe through a loopback bridge, so that a macOS or Windows member could
   join in February 2027, and the native engine after. That phase is dropped.
   Its lasting products were the date and nothing else: the bridge, the Tahoe
   adapter, the Tahoe bundle for three platforms and the per-node migration
   away from the bridge were all to be deleted, and the data was going to be
   re-uploaded into the native grid in any case. Instead the agent ships
   first as a control plane beside today's nodes, carrying no data, which
   proves identity, the signed manifest, NAT traversal and self-update on
   real machines; then the native engine; then the old stack retires. The
   price is the first cross-platform backup moving from February to July
   2027. The reasoning is in the RFC's Phases and Migration plan.

## What was measured

Hub on hub-hetzner (Helsinki). Agents on a MacBook behind a Fritz!Box, on the
Pi 5 node falcon-1 behind another Fritz!Box, and on the x86 node falcon. All
three sites are home connections. 32 to 128 MiB per run, random bytes, and
the tinc baseline sent through the live tinc inside its namespace with the
same tool, because the live network runs tinc with `Compression = 9` and
zeros would have measured zlib.

**NAT survey.**

| Site | Mapping | Filtering | IPv6 | Reachable from a symmetric site |
| --- | --- | --- | --- | --- |
| Mac, Fritz!Box | endpoint independent | address and port dependent | yes | over IPv6 |
| falcon-1, Pi 5, Fritz!Box | depends on the destination port: two STUN servers on different ports saw two different public ports | address and port dependent | yes | over IPv6 |
| falcon, x86 | address and port dependent (symmetric), behind two NATs | address and port dependent | no | relay only |

**Connection times**, from the agent's start: hub channel up in about 120 ms;
end-to-end handshake through the relay 105 to 150 ms; first byte possible
about 280 ms after start; direct path over IPv6 about 0.5 s after start, with
the probe and switch taking 25 to 140 ms.

**Throughput**, Mbit/s, each number one run:

| Pair | Direct (IPv6) | Relay (UDP) | Relay (TCP 443) | tinc |
| --- | --- | --- | --- | --- |
| Mac to Pi | 50.4 | 6.3 to 33.9 | 17.0, 18.1 | not on tinc |
| Pi to Mac | 40.8 to 41.2 | 27.4 to 32.6 | | not on tinc |
| Mac to x86 | none possible | 27.5 | | not on tinc |
| x86 to Mac | none possible | 8.4, 9.4 | | not on tinc |
| x86 to Pi | none possible | 7.1 to 11.5 | 7.6, 10.1 | 15.1 to 24.0 |
| Pi to x86 | none possible | 20.6 to 29.1 | | 14.3 to 21.7 |

Every home link here swings by a factor of two to three within minutes, so
only runs made back to back were compared.

**CPU on the Pi 5**, percent of one core at about 40 Mbit/s on the direct
path: 27 to 30 receiving, 13 sending. **Primitives on the Pi 5**, MB/s on one
core: AES-256-GCM 1717, ChaCha20-Poly1305 396, BLAKE3 (pure Go on arm64) 333,
SHA-256 1488, Reed-Solomon 2-of-4 encode 4279 and reconstruct 2056. The
slowest supported node is a Pi 4, which was not measured and has no AES
instructions; Go's software AES-GCM on it is still far above a home uplink.

**Path loss with data in flight.** The direct path was cut mid-transfer
(packets to and from its address dropped in the agent). The agent noticed in
6 s through its keepalives, validated the relay path in 112 ms and the stream
continued; 700 MiB arrived intact. That worked only with a patch to quic-go
(next section). A forced switch from a busy fast path to a slower one is a
different case and fails by design: the receiver drops the slow path's
probes as duplicates because thousands of newer packets arrived meanwhile,
which RFC 9000 allows. The agent only leaves a path that is dead.

## Exit criteria

| Criterion | Bar | Result | Verdict |
| --- | --- | --- | --- |
| Direct path for the live NAT types | every pair of non-symmetric sites | Over IPv6, every pair that has it, at the first punch. Over public IPv4, none: of the three sites one is symmetric and one maps per destination port, so there is no non-symmetric pair to punch between | Met as far as the sites allow; see Direct paths |
| Relayed throughput | at least tinc's | Pi to x86: relay 20.6 to 29.1 against tinc 14.3 to 21.7. x86 to Pi: relay 7.1 to 11.5 against tinc 15.1 to 24.0, but the x86 site's uplink to Helsinki caps at about 10 Mbit/s over UDP and TCP alike, so tinc is not going through the hub on that pair: it has a direct path through a port forwarded by hand, which RedundaNet 2's install guide asks for | Met where both relay; the other case is a direct path, covered below |
| Pi CPU at 20 Mbit/s | under 20 % | About 15 % of one core receiving and 7 % sending, scaled from the 40 Mbit/s runs | Met, with headroom to gain: the spike's socket wrapper keeps quic-go from GSO, recvmmsg and buffer sizing |
| Resume after a path switch | within seconds | 6 s to notice a dead path, 112 ms to switch, stream intact, with the quic-go patch | Met |
| TCP 443 fallback | works, number recorded | 17 to 18 Mbit/s Mac to Pi, 7.6 to 10.1 x86 to Pi, within the UDP relay's range on the same pairs | Met |
| Cipher chosen | from the Pi table | AES-256-GCM | Met |
| Wire identity | as built, or the change recorded | As built; the certificate extension is the one addition | Met |

## Direct paths: what the live sites taught

Hole punching over IPv4 did not produce one direct path. The Mac's router is
punchable, but the Pi's site gives a different public port per destination
and the x86 site is symmetric behind two NATs. Every direct path in the table
above is IPv6, end to end, which punched at the first round. RedundaNet 2's
direct paths come from hand-made port forwards, which the install guide asks
members to make for 655/udp.

Automatic port mapping, which the RFC lists among the candidate sources, is
unavailable at all three sites as they are configured: both Fritz!Boxes refuse
PCP and UPnP until the member enables "allow independent port sharing" for
that device, and the x86 site's inner router maps onto a private address. The
agent asks for a mapping and reports the answer; whether a storage node is
reachable directly belongs on the status page.

The consequences for the design: IPv6 candidates are first-class and gathered
from day one; a storage node that wants a direct path on IPv4 forwards the
agent's fixed UDP port or enables port sharing, with the local page saying
which; and the relay is the normal path for a symmetric site, sized as such.
None of this would differ with another transport library.

## Findings for upstream and for the product

1. **quic-go cannot leave a dead path while data is in flight.** Path probes
   are sent only when the current path may send, and a path that died with a
   full congestion window never may again, so the PATH_CHALLENGE for the new
   path is never sent. A 30-line patch sends pending probes before that check;
   `spike/patches/` holds it with an integration test that fails without it
   and passes with it, and quic-go's own suites pass with it. Issue and pull
   request drafts are in `spike/patches/README.md`, not yet filed.
2. **The accepting side keeps a connection ID per path it switched to**, so a
   connection migrates twice and the third probe is never answered. The agent
   reconnects after two migrations, which breaks streams; a product that
   migrates often needs this fixed upstream too.
3. **The relay must drop, not buffer.** Relayed packets travel as QUIC
   datagrams with a bounded queue at the hub and in each agent; a congested
   leg becomes loss the end-to-end connection reacts to, instead of seconds
   of hidden queue.
4. **Give quic-go the raw socket.** The spike's multiplexer wraps the UDP
   socket to hand quic-go several views of it, which costs GSO, recvmmsg and
   buffer sizing. The product's path layer should sit beside quic-go's
   transport, not under it.
5. **Overlay networks must not pass for direct paths.** Two of the three test
   machines run Tailscale; its addresses punched at once and would have been
   measured as a direct path (at 9.9 Mbit/s, through Tailscale's own relay).
   The agent now excludes overlay ranges from its candidates.
6. **A bench must stay on the path it claims.** The first relay numbers
   published during the day were direct-path numbers: a punch landing mid-run
   migrated the connection. The corrected numbers are the ones above.

## Not measured

- An iroh endpoint beside quic-go: see decision 2.
- The Pi 4: the Pi 5 was measured. The Pi 4 numbers should be taken before
  the engine's segment size is frozen, since the Pi 4 is the slowest node the
  RFC supports.
- Two devices on one LAN, and a change of Wi-Fi network with a transfer in
  flight: the path loss was produced by dropping packets in the agent, not by
  moving a laptop. Both are cheap to run once the product's path layer exists.
- Tahoe through the loopback bridge: dropped with decision 6.

## What happens next

The control plane of phase 1, as the RFC now describes it; filing the quic-go
issue and pull request; and, when a member enables port sharing on a router,
the one measurement that would close the tinc comparison.
