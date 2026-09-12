"""Storage traffic limits: ``tc`` rules for the VPN interface, scoped to the
storage server's port.

Every byte written into a node's shares arrives on the storage tub port, and
every share it serves leaves from that port, so limiting that port on the
VPN interface caps the storage service alone: the node's own client (backup
uploads, restores, re-encoding), tinc, the census and the manifest sync stay
untouched. Tahoe and tinc have no bandwidth settings of their own; the VPN
interface lives in the tinc container's namespace, which every Tahoe
container shares, so the rules are applied from the generated tinc-up script
each time the interface comes up.

Directions differ in mechanism. Outgoing packets can be queued, so
``STORAGE_RATE_OUT`` is a shaper (an HTB class for the storage port, the rest
of the traffic in an unlimited default class). Incoming packets cannot be
queued on the receiving interface, only policed: ``STORAGE_RATE_IN`` drops
what exceeds the rate and the sender's TCP backs off. An upload writes its
shares to several servers in parallel and completes at the pace of the
slowest, so the storage nodes' inbound caps are the grid's effective upload
speed.

Rates use tc's units: ``500kbit``, ``20mbit``, ``1gbit`` (bits per second).
"""

from __future__ import annotations

import re

# The storage server's tub port (docker/entrypoints/tahoe_storage.py binds it).
STORAGE_TUB_PORT = 3457

_RATE = re.compile(r"^\s*(\d+)\s*([kmg])bit\s*$", re.IGNORECASE)
_UNIT_BITS = {"k": 1_000, "m": 1_000_000, "g": 1_000_000_000}
MIN_BURST_BYTES = 16_384
BURST_SECONDS = 0.025  # a burst of 25 ms of traffic keeps tbf/htb accurate at high rates


def parse_rate(value: str | None) -> str | None:
    """A rate in tc's own spelling (``20mbit``), or None for empty/invalid."""
    if not value:
        return None
    match = _RATE.match(value)
    if not match or int(match.group(1)) == 0:
        return None
    return f"{int(match.group(1))}{match.group(2).lower()}bit"


def rate_bits(rate: str) -> int:
    match = _RATE.match(rate)
    if not match:
        raise ValueError(f"not a rate: {rate!r}")
    return int(match.group(1)) * _UNIT_BITS[match.group(2).lower()]


def burst_bytes(rate: str) -> int:
    return max(int(rate_bits(rate) / 8 * BURST_SECONDS), MIN_BURST_BYTES)


def render_tc_rules(port: int, rate_in: str | None, rate_out: str | None) -> str:
    """Shell lines for tinc-up. Each limit is applied on its own and a failure
    (a kernel without the qdisc, an old iproute2) only logs: the VPN must come
    up regardless."""
    if not rate_in and not rate_out:
        return ""
    lines = [
        "",
        "# Storage traffic limits (STORAGE_RATE_IN / STORAGE_RATE_OUT in .env): only the",
        f"# storage server's port {port}; the client, tinc and the sidecars are not limited.",
        "tc qdisc del dev $INTERFACE root 2>/dev/null || true",
        "tc qdisc del dev $INTERFACE ingress 2>/dev/null || true",
    ]
    if rate_out:
        burst = burst_bytes(rate_out)
        lines += [
            "if ! (",
            "  tc qdisc add dev $INTERFACE root handle 1: htb default 20 &&",
            "  tc class add dev $INTERFACE parent 1: classid 1:20 htb rate 10gbit "
            "burst 1000000 quantum 60000 &&",
            f"  tc class add dev $INTERFACE parent 1: classid 1:10 htb rate {rate_out} "
            f"ceil {rate_out} burst {burst} quantum 60000 &&",
            "  tc filter add dev $INTERFACE parent 1: protocol ip prio 1 u32 "
            f"match ip protocol 6 0xff match ip sport {port} 0xffff flowid 1:10",
            "); then",
            f'  echo "redundanet: STORAGE_RATE_OUT={rate_out} could not be applied; '
            'shares are served unlimited" >&2',
            "else",
            f'  echo "redundanet: STORAGE_RATE_OUT={rate_out} applied to port {port}"',
            "fi",
        ]
    if rate_in:
        burst = burst_bytes(rate_in)
        lines += [
            "if ! (",
            "  tc qdisc add dev $INTERFACE handle ffff: ingress &&",
            "  tc filter add dev $INTERFACE parent ffff: protocol ip prio 1 u32 "
            f"match ip protocol 6 0xff match ip dport {port} 0xffff "
            f"police rate {rate_in} burst {burst} mtu 65535 drop flowid :1",
            "); then",
            f'  echo "redundanet: STORAGE_RATE_IN={rate_in} could not be applied; '
            'shares are accepted unlimited" >&2',
            "else",
            f'  echo "redundanet: STORAGE_RATE_IN={rate_in} applied to port {port}"',
            "fi",
        ]
    return "\n".join(lines) + "\n"


def render_tc_teardown() -> str:
    """Shell lines for tinc-down: drop the rules with the interface."""
    return (
        "tc qdisc del dev $INTERFACE root 2>/dev/null || true\n"
        "tc qdisc del dev $INTERFACE ingress 2>/dev/null || true\n"
    )
