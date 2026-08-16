#!/usr/bin/env python3
"""Process a RedundaNet join-request issue into a manifest entry.

Invoked by .github/workflows/process-join-request.yml with the issue body in
the ISSUE_BODY env var. Extracted from the workflow YAML so the parsing and
allocation logic is unit-testable (tests/unit/test_process_join.py).

SECURITY: the issue body is fully attacker-controlled (anyone can open an
issue). Every parsed value is strictly validated before it is used or written
anywhere, so it can contain no newlines or quotes that could inject
GITHUB_OUTPUT entries or break out of a later github-script string. Values are
additionally written with the random-delimiter heredoc form (defense in depth).

The applicant must submit the full 40-character GPG fingerprint (short 8/16
char ids are brute-forceable Evil32-style suffix collisions and are rejected).
The key MUST be fetchable from the public keyservers and its fingerprint MUST
exactly match the submitted one, or the join fails — peers authenticate the
node by fetching exactly this key, so an unverifiable key would either brick
the node or, worse, let an attacker-uploaded key take its place. Fail closed.
"""

from __future__ import annotations

import ipaddress
import os
import re
import secrets
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

KEYSERVERS = [
    "keys.openpgp.org",
    "keyserver.ubuntu.com",
]

# Only the full 40-char fingerprint is accepted as a node identity; short
# 8/16-char ids are collision-prone and rejected outright.
GPG_KEY_ID_RE = re.compile(r"[A-F0-9]{40}")


@dataclass
class JoinResult:
    """Outcome of processing a join request."""

    success: bool
    error: str = ""
    node_name: str = ""
    vpn_ip: str = ""
    gpg_key_id: str = ""
    storage: str = ""
    warnings: list[str] = field(default_factory=list)


def set_output(**kwargs: str) -> None:
    """Write step outputs with the random-delimiter heredoc form so a value can
    never inject extra output lines, even if it somehow contained newlines."""
    output_path = os.environ.get("GITHUB_OUTPUT")
    if not output_path:
        return
    with Path(output_path).open("a") as f:
        for name, value in kwargs.items():
            delim = f"__EOF_{secrets.token_hex(16)}__"
            f.write(f"{name}<<{delim}\n{value}\n{delim}\n")


def parse_gpg_key_id(issue_body: str) -> str | None:
    """Extract and strictly validate the GPG fingerprint from the issue body.

    Only a full 40-character hex fingerprint is accepted (spaces and an ``0x``
    prefix are tolerated and stripped). Returns the normalized fingerprint or
    None if missing/short/invalid.
    """
    match = re.search(r"\*\*GPG Key ID\*\*\s*\|\s*`([^`]+)`", issue_body)
    if not match:
        return None
    key_id = match.group(1).strip().replace(" ", "").upper()
    key_id = key_id.removeprefix("0X")
    if not GPG_KEY_ID_RE.fullmatch(key_id):
        return None
    return key_id


def parse_storage(issue_body: str) -> str:
    match = re.search(r"\*\*Storage Contribution\*\*\s*\|\s*(\d+\s*[GMTP]B)", issue_body)
    return match.group(1) if match else "100GB"


def parse_region(issue_body: str) -> str:
    match = re.search(r"\*\*Region\*\*\s*\|\s*([^\n|]+)", issue_body)
    region = match.group(1).strip() if match else "unknown"
    if region in ("Not specified", ""):
        return "unknown"
    # The region lands verbatim in YAML and in a github-script comment; keep it
    # to a conservative charset.
    if not re.fullmatch(r"[A-Za-z0-9 _.\-]{1,64}", region):
        return "unknown"
    return region


def parse_public_ip(issue_body: str) -> str | None:
    match = re.search(r"\*\*Public IP\*\*\s*\|\s*([^\n|]+)", issue_body)
    public_ip = match.group(1).strip() if match else None
    if public_ip in (None, "None", "Not specified", ""):
        return None
    # Must be a literal IP address — anything else is discarded.
    try:
        ipaddress.ip_address(public_ip)
    except ValueError:
        return None
    return public_ip


def fetch_key_from_keyservers(key_id: str) -> str | None:
    """Fetch the armored public key for key_id, or None if not found."""
    import httpx

    search = f"0x{key_id}"
    for server in KEYSERVERS:
        try:
            response = httpx.get(
                f"https://{server}/pks/lookup",
                params={"op": "get", "search": search, "options": "mr"},
                timeout=10.0,
            )
            if response.status_code == 200 and "BEGIN PGP PUBLIC KEY BLOCK" in response.text:
                return response.text
        except Exception as e:  # network errors: try the next server
            print(f"  Could not check {server}: {e}")
            continue
    return None


def armored_fingerprint(armored: str) -> str | None:
    """Primary-key fingerprint (40-char uppercase hex) of an armored key."""
    try:
        import pgpy

        key, _ = pgpy.PGPKey.from_blob(armored)
        return str(key.fingerprint).replace(" ", "").upper()
    except Exception:
        return None


def default_manifest() -> dict[str, Any]:
    return {
        "network": {
            "name": "redundanet",
            "version": "2.0.0",
            "domain": "redundanet.local",
            "vpn_network": "10.100.0.0/16",
            "tahoe": {
                "shares_needed": 3,
                "shares_happy": 7,
                "shares_total": 10,
                "reserved_space": "1G",
            },
        },
        "introducer_furl": None,
        "nodes": [],
    }


def allocate_ip(manifest: dict[str, Any]) -> str | None:
    """Pick the next free VPN IP, skipping the first 10 addresses of each /24."""
    vpn_network = manifest.get("network", {}).get("vpn_network", "10.100.0.0/16")
    network = ipaddress.ip_network(vpn_network)

    used_ips = set()
    for node in manifest.get("nodes", []):
        for ip_field in ("vpn_ip", "internal_ip"):
            if node.get(ip_field):
                used_ips.add(ipaddress.ip_address(node[ip_field]))

    for ip in network.hosts():
        if int(ip) % 256 < 10:  # Reserve the first 10 IPs in each subnet
            continue
        if ip not in used_ips:
            return str(ip)
    return None


def is_duplicate_key(manifest: dict[str, Any], key_id: str) -> bool:
    """True if a node already uses this exact fingerprint.

    Exact comparison only: identities are full 40-char fingerprints, and
    suffix matching would let a crafted fingerprint alias an existing node.
    """
    kid = key_id.replace(" ", "").upper()
    for node in manifest.get("nodes", []):
        existing = str(node.get("gpg_key_id", "")).replace(" ", "").upper()
        if existing and existing == kid:
            return True
    return False


def process(
    issue_body: str,
    manifest_path: Path,
    fetch_key: Callable[[str], str | None] = fetch_key_from_keyservers,
) -> JoinResult:
    """Parse a join-request body and add the node to the manifest on disk."""
    key_id = parse_gpg_key_id(issue_body)
    if key_id is None:
        return JoinResult(
            success=False,
            error=(
                "Could not parse a valid GPG fingerprint from the issue "
                "(expected the full 40-character hex fingerprint; short "
                "8/16-character key ids are not accepted — run "
                "'gpg --fingerprint <your-key>' to get it)"
            ),
        )

    result = JoinResult(success=True, gpg_key_id=key_id)

    # FAIL CLOSED: the key must be fetchable from the keyservers and its
    # fingerprint must exactly match the submitted one. Peers authenticate the
    # node by fetching exactly this key — admitting an unverifiable key would
    # either brick the node or let attacker-uploaded material stand in for it.
    armored = fetch_key(key_id)
    if armored is None:
        return JoinResult(
            success=False,
            error=(
                "GPG key not found on the public keyservers. Publish it first "
                "('redundanet node keys publish --key-id <fingerprint>' or "
                "upload at https://keys.openpgp.org/upload), wait a few "
                "minutes, then re-open the request."
            ),
        )
    fingerprint = armored_fingerprint(armored)
    if fingerprint is None:
        return JoinResult(
            success=False,
            error=(
                "The keyserver response could not be parsed as an OpenPGP "
                "key; cannot verify the submitted fingerprint."
            ),
        )
    if fingerprint != key_id:
        return JoinResult(
            success=False,
            error="Keyserver returned a key whose fingerprint does not match the submitted one",
        )

    storage = parse_storage(issue_body)
    region = parse_region(issue_body)
    public_ip = parse_public_ip(issue_body)

    if manifest_path.exists():
        with manifest_path.open() as f:
            manifest = yaml.safe_load(f) or default_manifest()
    else:
        manifest_path.parent.mkdir(parents=True, exist_ok=True)
        manifest = default_manifest()

    if is_duplicate_key(manifest, result.gpg_key_id):
        return JoinResult(
            success=False,
            error=f"Node with GPG key {key_id} already exists",
        )

    next_ip = allocate_ip(manifest)
    if next_ip is None:
        return JoinResult(success=False, error="No available IP addresses")

    node_name = f"node-{result.gpg_key_id[-8:].lower()}"
    new_node: dict[str, Any] = {
        "name": node_name,
        "internal_ip": next_ip,
        "vpn_ip": next_ip,
        "gpg_key_id": result.gpg_key_id,
        "region": region,
        "status": "pending",
        "roles": ["tinc_vpn", "tahoe_storage"],
        "ports": {
            "tinc": 655,
            "tahoe_storage": 3457,
            "tahoe_client": 3456,
            "tahoe_introducer": 3458,
        },
        "storage_contribution": storage,
        "is_publicly_accessible": public_ip is not None,
    }
    if public_ip:
        new_node["public_ip"] = public_ip

    manifest.setdefault("nodes", []).append(new_node)
    with manifest_path.open("w") as f:
        yaml.dump(manifest, f, default_flow_style=False, sort_keys=False)

    result.node_name = node_name
    result.vpn_ip = next_ip
    result.storage = storage
    return result


def main() -> None:
    issue_body = os.environ.get("ISSUE_BODY", "")
    issue_number = os.environ.get("ISSUE_NUMBER", "")

    # Do NOT echo the untrusted issue body/title to the log: a line that begins
    # with `::` would be interpreted as a workflow command.
    print(f"Processing issue #{issue_number}")

    result = process(issue_body, Path("manifests/manifest.yaml"))

    for warning in result.warnings:
        print(f"::warning::{warning}")

    if not result.success:
        print("::error::" + result.error)
        set_output(success="false", error=result.error)
        return

    print(f"Node name: {result.node_name}")
    print(f"Assigned VPN IP: {result.vpn_ip}")
    print(f"GPG key id stored in manifest: {result.gpg_key_id}")
    set_output(
        success="true",
        node_name=result.node_name,
        vpn_ip=result.vpn_ip,
        gpg_key_id=result.gpg_key_id,
        storage=result.storage,
    )
    print("Done!")


if __name__ == "__main__":
    main()
