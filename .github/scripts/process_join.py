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

The manifest also carries the RedundaNet 3 half (schema 3: a signed revision,
members, storage devices). The node is appended to `nodes:` textually and
`revision` is raised by one, with the editor the v3 script
(process_join_v3.py) uses: re-serialising the whole file would drop the
header comment and reflow the members' signature blocks, and any change to
the file invalidates manifest.sig, so the agents only follow a higher,
re-signed revision.
"""

from __future__ import annotations

import ipaddress
import os
import re
import sys
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml
from process_join_v3 import EditError, append_items, bump_revision, set_output

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
    revision: int | None = None  # the manifest's new revision, when it has one
    warnings: list[str] = field(default_factory=list)


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


# A member handle is the GitHub login of the applicant (the issue author):
# GitHub's own rules, so it can never carry YAML or shell metacharacters.
MEMBER_RE = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$")


def valid_member(handle: str | None) -> str | None:
    """The handle if it is a valid GitHub login, else None."""
    if handle and MEMBER_RE.fullmatch(handle.strip()):
        return handle.strip()
    return None


def process(
    issue_body: str,
    manifest_path: Path,
    fetch_key: Callable[[str], str | None] = fetch_key_from_keyservers,
    member: str | None = None,
) -> JoinResult:
    """Parse a join-request body and add the node to the manifest on disk.

    ``member`` groups this node with the applicant's other nodes for the
    contribution-based allocation (docs/legacy/quotas.md).
    """
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

    text: str | None = None
    if manifest_path.exists():
        text = manifest_path.read_text()
        manifest = yaml.safe_load(text) or default_manifest()
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
    if valid_member(member):
        new_node["member"] = valid_member(member)

    try:
        if text is None or not text.strip():
            # A fresh file: nothing to preserve, serialise the default shape.
            manifest.setdefault("nodes", []).append(new_node)
            new_text = yaml.dump(manifest, default_flow_style=False, sort_keys=False)
        else:
            new_text, result.revision = insert_node(text, new_node)
    except (EditError, yaml.YAMLError) as err:
        return JoinResult(success=False, error=f"could not update the manifest: {err}")
    manifest_path.write_text(new_text)

    result.node_name = node_name
    result.vpn_ip = next_ip
    result.storage = storage
    return result


def render_node(node: dict[str, Any], indent: str) -> list[str]:
    """The node as YAML list-item lines at the list's indentation."""
    dumped = yaml.dump([node], default_flow_style=False, sort_keys=False).rstrip("\n")
    return [indent + line if line else line for line in dumped.split("\n")]


def insert_node(text: str, node: dict[str, Any]) -> tuple[str, int | None]:
    """Append the node to `nodes:` textually and raise `revision` when there is one.

    Returns (new text, new revision or None for a manifest without one). The
    result must read back with the node as the last entry; otherwise EditError.
    """
    new_text = append_items(text, "nodes", lambda indent: render_node(node, indent), "")
    revision: int | None = None
    if re.search(r"^revision:", new_text, re.MULTILINE):
        new_text, revision = bump_revision(new_text)
    data = yaml.safe_load(new_text)
    nodes = data.get("nodes") if isinstance(data, dict) else None
    if not isinstance(nodes, list) or not nodes or nodes[-1] != node:
        raise EditError("the node did not read back as the last entry of `nodes`")
    if revision is not None and data.get("revision") != revision:
        raise EditError(f"revision read back as {data.get('revision')!r}, not {revision}")
    return new_text, revision


def compose_pr(result: JoinResult, issue_number: str) -> dict[str, str]:
    """Title, body and commit message of the pull request."""
    revision = (
        f" and raises the RedundaNet 3 `revision` to {result.revision}, which the maintainer re-signs"
        if result.revision is not None
        else ""
    )
    body = f"""## New Node Addition

This PR adds a new node to the RedundaNet network manifest{revision}.

| Field | Value |
|-------|-------|
| **Node Name** | `{result.node_name}` |
| **VPN IP** | `{result.vpn_ip}` |
| **GPG Key ID** | `{result.gpg_key_id}` |
| **Storage** | {result.storage} |

### Checklist
- [ ] Verify GPG key is valid
- [ ] Confirm storage allocation
- [ ] Review node configuration
- [ ] Sign the manifest: `cd agent && go run ./cmd/redundanet manifest sign --key operator.key --manifest ../manifests/manifest.yaml`, commit `manifests/manifest.sig`, push

Closes #{issue_number}

---
*Auto-generated from join request*
"""
    return {
        "pr_title": f"Add node {result.node_name} (Issue #{issue_number})",
        "pr_body": body,
        "commit_message": f"Add node {result.node_name} to network manifest",
    }


def compose_next_steps(result: JoinResult) -> str:
    """The closing comment on the issue; the workflow fills in {PR_URL}."""
    return f"""**Application processed successfully!**

Your node `{result.node_name}` has been assigned IP `{result.vpn_ip}`.

**Pull Request**: {{PR_URL}}

This issue is being closed. The PR will be reviewed by a maintainer. Once merged, you can configure your node with:

```bash
# Clone the repository
git clone https://github.com/adefilippo83/redundanet.git
cd redundanet

# Initialize your node
pip install redundanet
redundanet init --name {result.node_name}

# Pull the latest manifest
redundanet sync

# Start services with Docker
docker compose up -d

# Check status
redundanet status
```

*Thank you for contributing to RedundaNet!*"""


def main() -> int:
    issue_body = os.environ.get("ISSUE_BODY", "")
    issue_number = os.environ.get("ISSUE_NUMBER", "")

    # Do NOT echo the untrusted issue body/title to the log: a line that begins
    # with `::` would be interpreted as a workflow command.
    print(f"Processing issue #{issue_number}")

    member = valid_member(os.environ.get("ISSUE_AUTHOR"))
    result = process(issue_body, Path("manifests/manifest.yaml"), member=member)

    for warning in result.warnings:
        print(f"::warning::{warning}")

    if not result.success:
        print("::error::" + result.error)
        set_output(success="false", error=result.error)
        return 1

    print(f"Node name: {result.node_name}")
    print(f"Assigned VPN IP: {result.vpn_ip}")
    print(f"GPG key id stored in manifest: {result.gpg_key_id}")
    if result.revision is not None:
        print(f"Manifest revision: {result.revision}")
    set_output(
        success="true",
        node_name=result.node_name,
        vpn_ip=result.vpn_ip,
        gpg_key_id=result.gpg_key_id,
        storage=result.storage,
        revision=str(result.revision or ""),
        next_steps=compose_next_steps(result),
        **compose_pr(result, issue_number),
    )
    print("Done!")
    return 0


if __name__ == "__main__":
    sys.exit(main())
