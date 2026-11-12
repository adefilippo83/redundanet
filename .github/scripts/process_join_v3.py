#!/usr/bin/env python3
"""Process a RedundaNet 3 membership request (a GitHub issue) into manifest entries.

Invoked by .github/workflows/process-join-request.yml with the issue body in
ISSUE_BODY, the issue number in ISSUE_NUMBER and the applicant's GitHub login
in ISSUE_AUTHOR. The parsing, the checks and the manifest edit live here so
they are unit-tested (.github/scripts/tests/test_process_join_v3.py).

The issue body is what the join form (docs/join.html) produces. A new
member's request: the heading `## RedundaNet 3 membership`, a Markdown table
with the rows Member name, Member key, Contribution GB, Device key, Device
name, UDP port, Legacy GPG fingerprint and Notes, and, for a RedundaNet 2
member, a section `## Legacy signature` with an armored PGP detached
signature over the member key string. The script appends a `members:` entry
and, when the member contributes disk, a `storage:` entry, and raises
`revision` by one.

A listed member's request to add a storage node: the heading
`## RedundaNet 3 storage device`, a table with Member name, Device key,
Device name, UDP port, Contribution GB (the member's new total, optional)
and Notes, and a section `## Device certificate` with the node's certificate
as `redundanet identity show --certificate` prints it (one line, base64url).
The certificate is the proof: `redundanet identity verify-certificate`
checks the member's signature over it, and the script checks that the
member key is the one the manifest lists under that name, that the device
key is the one in the table and that the certificate grants the storage
role. The script appends the `storage:` entry, rewrites the member's
`contribution_gb` when a new total was given, and raises `revision` by one.

SECURITY: the body is attacker-controlled (anyone can open an issue). Every
value is validated against a closed charset before it is written anywhere:
names against the agent's rule, keys as 52-character base32 that decodes to
32 bytes, numbers as integers in range, the fingerprint as 40 hex digits, the
signature as printable ASCII inside an armored block. The manifest is edited
textually, never re-serialised, so the header comment and the existing
signature blocks survive and manifest.sig stays the only thing the maintainer
has to redo; the result must parse back to exactly the entries written. A
legacy attestation is checked fail-closed: the fingerprint must belong to a
RedundaNet 2 node of the same member and the signature must verify with the
key the keyservers serve.
"""

from __future__ import annotations

import base64
import json
import os
import re
import secrets
import subprocess
import sys
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import validate_v3_members as legacy
import yaml

MARKER = "## RedundaNet 3 membership"
DEVICE_MARKER = "## RedundaNet 3 storage device"
ROW_NAME = "member name"
ROW_KEY = "member key"
ROW_CONTRIBUTION = "contribution gb"
ROW_DEVICE_KEY = "device key"
ROW_DEVICE_NAME = "device name"
ROW_UDP_PORT = "udp port"
ROW_FINGERPRINT = "legacy gpg fingerprint"
REQUIRED_ROWS = (ROW_KEY, ROW_CONTRIBUTION)
# What a form writes into an optional cell it leaves out.
EMPTY_CELLS = frozenset({"", "-", "none", "n/a"})

# The agent's rule for member and device names (agent/internal/manifest).
NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,62}$")
# A key is a 32-byte Ed25519 public key: base32, no padding, lowercase.
KEY_RE = re.compile(r"^[a-z2-7]{52}$")
FINGERPRINT_RE = re.compile(r"^[0-9A-F]{40}$")
PRINTABLE_RE = re.compile(r"^[\x20-\x7e]*$")
LEGACY_HEADING_RE = re.compile(r"^#+\s*legacy signature\s*$", re.IGNORECASE)
CERT_HEADING_RE = re.compile(r"^#+\s*device certificate\s*$", re.IGNORECASE)
# A certificate as `identity show --certificate` prints it: base64url, one line.
CERT_RE = re.compile(r"^[A-Za-z0-9_-]{64,4096}$")
AGENT_DIR = Path(__file__).resolve().parents[2] / "agent"
MAX_CONTRIBUTION_GB = 1_000_000
# The agent's rule: a forwarded port is never a privileged one.
MIN_UDP_PORT, MAX_UDP_PORT = 1024, 65535
MAX_SIGNATURE_LINES = 200
SIGNATURE_BEGIN = "-----BEGIN PGP SIGNATURE-----"
SIGNATURE_END = "-----END PGP SIGNATURE-----"
STATUS_PAGE = "https://hub.redundanet.com/"


class JoinError(Exception):
    """A request that cannot be processed; the message goes back to the applicant."""


class EditError(Exception):
    """The manifest text could not be edited the way the script expects."""


@dataclass
class JoinRequest:
    """The validated content of a membership request."""

    name: str
    key: str
    contribution_gb: int
    device_key: str | None = None
    device_name: str | None = None
    udp_port: int | None = None
    legacy_fingerprint: str | None = None
    legacy_signature: str | None = None


@dataclass
class JoinResult:
    """Outcome of processing a membership request."""

    success: bool
    error: str = ""
    request: JoinRequest | None = None
    revision: int = 0
    legacy: str = ""


@dataclass
class DeviceRequest:
    """The validated content of a listed member's request to add a storage node."""

    member: str
    device_key: str
    device_name: str
    certificate: str
    udp_port: int | None = None
    contribution_gb: int | None = None  # the member's new total; None keeps it


@dataclass
class DeviceResult:
    """Outcome of processing a storage device request."""

    success: bool
    error: str = ""
    request: DeviceRequest | None = None
    revision: int = 0
    member_key: str = ""


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


def utc_today() -> str:
    """Today's date in UTC, the manifest's date form."""
    return datetime.now(UTC).date().isoformat()


def shown(value: str, limit: int = 64) -> str:
    """A value quoted for an error message, cut so a flood cannot bloat the comment."""
    return repr(value if len(value) <= limit else value[:limit] + "...")


# --- parsing the issue body -------------------------------------------------


def _lines(body: str) -> list[str]:
    return body.replace("\r\n", "\n").replace("\r", "\n").split("\n")


def is_device_request(body: str) -> bool:
    """Whether the body carries the storage device heading rather than the
    membership one; the device heading wins when both are present."""
    return any(line.strip() == DEVICE_MARKER for line in _lines(body))


def parse_table(body: str, marker: str = MARKER) -> dict[str, str]:
    """Field -> value of the table under the marker.

    Field names are lowercased with any bold markers or backticks removed;
    values lose one pair of surrounding backticks. The table ends at the
    next heading. The first row of a repeated field wins.
    """
    lines = _lines(body)
    try:
        start = next(i for i, line in enumerate(lines) if line.strip() == marker)
    except StopIteration:
        raise JoinError(f"the issue body has no `{marker}` heading") from None
    rows: dict[str, str] = {}
    for line in lines[start + 1 :]:
        stripped = line.strip()
        if stripped.startswith("#"):
            break
        if not stripped.startswith("|"):
            continue
        cells = [cell.strip() for cell in stripped.strip("|").split("|")]
        if len(cells) < 2:
            continue
        name = cells[0].strip("*` ").lower()
        if name == "field" or re.fullmatch(r":?-+:?", name) or not name:
            continue
        value = cells[1]
        if len(value) >= 2 and value.startswith("`") and value.endswith("`"):
            value = value[1:-1].strip()
        if value.lower() in EMPTY_CELLS:
            value = ""
        rows.setdefault(name, value)
    if not rows:
        raise JoinError(f"no table found under the `{marker}` heading")
    return rows


def parse_certificate(body: str) -> str:
    """The one-line certificate under the `## Device certificate` heading,
    with code fences and whitespace removed; refused unless it is base64url
    of a plausible length. Verification is the agent's job."""
    lines = _lines(body)
    try:
        start = next(i for i, line in enumerate(lines) if CERT_HEADING_RE.match(line.strip()))
    except StopIteration:
        raise JoinError(
            "the issue body has no `## Device certificate` section; "
            "`redundanet identity show --certificate` on the node prints it"
        ) from None
    parts: list[str] = []
    for line in lines[start + 1 :]:
        stripped = line.strip()
        if stripped.startswith("#"):
            break
        if not stripped or stripped.startswith("```"):
            continue
        parts.append(stripped.strip("`"))
    certificate = "".join(parts)
    if not CERT_RE.fullmatch(certificate):
        raise JoinError(
            "the device certificate is not the one-line base64url text "
            "`redundanet identity show --certificate` prints"
        )
    return certificate


def parse_signature(body: str) -> str | None:
    """The armored detached signature in the body, or None when there is none.

    Lines are kept as they are (trailing whitespace removed), restricted to
    printable ASCII so the block scalar they land in cannot be broken out of.
    """
    lines = _lines(body)
    text = "\n".join(lines)
    begin = text.find(SIGNATURE_BEGIN)
    if begin < 0:
        if any(LEGACY_HEADING_RE.match(line) for line in lines):
            raise JoinError(
                "the Legacy signature section has no armored PGP signature block "
                f"({SIGNATURE_BEGIN} ... {SIGNATURE_END})"
            )
        return None
    end = text.find(SIGNATURE_END, begin)
    if end < 0:
        raise JoinError(f"the PGP signature block has no {SIGNATURE_END} line")
    block = [line.rstrip() for line in text[begin : end + len(SIGNATURE_END)].split("\n")]
    if len(block) > MAX_SIGNATURE_LINES:
        raise JoinError(f"the PGP signature block is longer than {MAX_SIGNATURE_LINES} lines")
    for line in block:
        if len(line) > 1024 or not PRINTABLE_RE.fullmatch(line):
            raise JoinError("the PGP signature block contains characters outside printable ASCII")
    return "\n".join(block) + "\n"


def default_name(author: str | None) -> str | None:
    """The applicant's GitHub login lowercased, when it is a valid member name."""
    if author and NAME_RE.fullmatch(author.strip().lower()):
        return author.strip().lower()
    return None


def parse_name(value: str, what: str) -> str:
    if not NAME_RE.fullmatch(value):
        raise JoinError(
            f"{what} {shown(value)}: lowercase letters, digits and hyphens only, "
            "starting with a letter or digit, at most 63 characters"
        )
    return value


def parse_key(value: str, what: str) -> str:
    """A 52-character base32 key that decodes to exactly 32 bytes, lowercased."""
    value = value.strip().lower()
    if not KEY_RE.fullmatch(value):
        raise JoinError(
            f"{what} {shown(value)} is not a key: expected the 52-character lowercase key "
            "that `redundanet identity show` prints"
        )
    raw = base64.b32decode(value.upper() + "====")
    if len(raw) != 32 or base64.b32encode(raw).decode().lower().rstrip("=") != value:
        raise JoinError(f"{what} {shown(value)} does not decode to a 32-byte key")
    return value


def parse_contribution(value: str) -> int:
    match = re.fullmatch(r"(\d{1,7})\s*(?:gb)?", value, flags=re.IGNORECASE)
    if not match:
        raise JoinError(
            f"Contribution GB {shown(value)} must be a whole number of gigabytes "
            "(0 if you contribute no disk)"
        )
    gb = int(match.group(1))
    if gb > MAX_CONTRIBUTION_GB:
        raise JoinError(
            f"Contribution GB {gb} is above the {MAX_CONTRIBUTION_GB} GB the form accepts"
        )
    return gb


def parse_port(value: str) -> int:
    if not re.fullmatch(r"\d{1,5}", value) or not MIN_UDP_PORT <= int(value) <= MAX_UDP_PORT:
        raise JoinError(
            f"UDP port {shown(value)} must be a number between {MIN_UDP_PORT} and {MAX_UDP_PORT}"
        )
    return int(value)


def parse_fingerprint(value: str) -> str:
    fingerprint = legacy.normalize(value)
    if not FINGERPRINT_RE.fullmatch(fingerprint):
        raise JoinError(
            f"Legacy GPG fingerprint {shown(value)} must be the full 40-character fingerprint "
            "(`gpg --fingerprint <key>`)"
        )
    return fingerprint


def parse_request(body: str, author: str | None = None) -> JoinRequest:
    """Parse and validate a request body; raises JoinError with the reason."""
    rows = parse_table(body)
    missing = [row for row in REQUIRED_ROWS if row not in rows]
    if missing:
        raise JoinError("the table is missing the row(s): " + ", ".join(missing))

    name = rows.get(ROW_NAME, "") or default_name(author)
    if not name:
        raise JoinError("Member name is empty and the GitHub login cannot be used; fill the row")
    req = JoinRequest(
        name=parse_name(name, "Member name"),
        key=parse_key(rows[ROW_KEY], "Member key"),
        contribution_gb=parse_contribution(rows[ROW_CONTRIBUTION]),
    )

    device_key = rows.get(ROW_DEVICE_KEY, "")
    device_name = rows.get(ROW_DEVICE_NAME, "")
    port = rows.get(ROW_UDP_PORT, "")
    if req.contribution_gb > 0 and not (device_key and device_name):
        raise JoinError(
            "a contribution above 0 GB needs a storage device: fill Device key and Device name "
            "(`install.sh prepare` on the node prints its key)"
        )
    if device_key or device_name:
        if not (device_key and device_name):
            raise JoinError("Device key and Device name go together: fill both or neither")
        if req.contribution_gb == 0:
            raise JoinError(
                "a storage device needs a contribution above 0 GB; without disk to contribute, "
                "leave Device key and Device name empty"
            )
        req.device_key = parse_key(device_key, "Device key")
        req.device_name = parse_name(device_name, "Device name")
        if req.device_key == req.key:
            raise JoinError(
                "Device key equals the member key; the device key is the one the storage "
                "node's installer printed"
            )
    if port:
        if not req.device_key:
            raise JoinError("UDP port applies to a storage device: fill Device key and Device name")
        req.udp_port = parse_port(port)

    fingerprint = rows.get(ROW_FINGERPRINT, "")
    signature = parse_signature(body)
    if fingerprint or signature:
        if not fingerprint:
            raise JoinError("a Legacy signature needs the Legacy GPG fingerprint row")
        if not signature:
            raise JoinError(
                "a Legacy GPG fingerprint needs the `## Legacy signature` section with the "
                "armored detached signature over the member key"
            )
        req.legacy_fingerprint = parse_fingerprint(fingerprint)
        req.legacy_signature = signature
    return req


# --- checks against the manifest --------------------------------------------


def _entries(manifest: dict[str, Any], key: str) -> list[dict[str, Any]]:
    value = manifest.get(key) or []
    if not isinstance(value, list):
        raise JoinError(f"manifest: `{key}` is not a list")
    return [entry for entry in value if isinstance(entry, dict)]


def check_manifest(req: JoinRequest, manifest: Any, author: str | None = None) -> None:
    """Uniqueness of the names and keys, and the RedundaNet 2 name rule."""
    if not isinstance(manifest, dict) or manifest.get("schema") != 3:
        raise JoinError("the manifest is not a RedundaNet 3 manifest (schema: 3)")
    members = _entries(manifest, "members")
    storage = _entries(manifest, "storage")
    hubs = _entries(manifest, "hubs")
    operators = _entries(manifest, "operators")

    taken = {str(m.get("name")) for m in members} | {str(o.get("name")) for o in operators}
    if req.name in taken:
        raise JoinError(f"a member named {req.name} already exists")

    keys: dict[str, str] = {}
    for what, entries, field in (
        ("operator", operators, "key"),
        ("hub", hubs, "key"),
        ("member", members, "key"),
        ("storage device", storage, "device"),
    ):
        for entry in entries:
            keys[str(entry.get(field, "")).strip().lower()] = f"{what} {entry.get('name')}"
    for label, key in (("Member key", req.key), ("Device key", req.device_key)):
        if key and key in keys:
            raise JoinError(f"{label} is already in the manifest as the key of {keys[key]}")

    device_names = {str(s.get("name")) for s in storage} | {str(h.get("name")) for h in hubs}
    if req.device_name and req.device_name in device_names:
        raise JoinError(
            f"a storage device or hub named {req.device_name} already exists; "
            "pick another device name"
        )

    # A RedundaNet 2 member's name is claimed by that GitHub account or by the
    # legacy attestation, which validate_v3_members ties to the member's nodes.
    v2_members = {
        str(node.get("member")).lower()
        for node in _entries(manifest, "nodes")
        if node.get("member")
    }
    if (
        req.name in v2_members
        and req.legacy_fingerprint is None
        and req.name != (author or "").strip().lower()
    ):
        raise JoinError(
            f"{req.name} is the member name of RedundaNet 2 nodes: file the request from "
            "that GitHub account, or add the Legacy GPG fingerprint and the Legacy signature"
        )


# --- a listed member's storage device -----------------------------------------


def parse_device_request(body: str) -> DeviceRequest:
    """Parse and validate a storage device request; raises JoinError with the reason."""
    rows = parse_table(body, DEVICE_MARKER)
    missing = [row for row in (ROW_NAME, ROW_DEVICE_KEY, ROW_DEVICE_NAME) if not rows.get(row)]
    if missing:
        raise JoinError("the table is missing the row(s): " + ", ".join(missing))
    req = DeviceRequest(
        member=parse_name(rows[ROW_NAME], "Member name"),
        device_key=parse_key(rows[ROW_DEVICE_KEY], "Device key"),
        device_name=parse_name(rows[ROW_DEVICE_NAME], "Device name"),
        certificate=parse_certificate(body),
    )
    if rows.get(ROW_UDP_PORT):
        req.udp_port = parse_port(rows[ROW_UDP_PORT])
    if rows.get(ROW_CONTRIBUTION):
        req.contribution_gb = parse_contribution(rows[ROW_CONTRIBUTION])
    return req


def verify_certificate_with_go(certificate: str, network: str) -> dict[str, Any]:
    """Have the agent verify the certificate: it decodes the one-line form,
    checks the member's signature and prints what the certificate says. The
    workflow has Go set up; the checkout's own agent is the verifier, so a
    format change ships with its check."""
    with tempfile.NamedTemporaryFile("w", suffix=".cert", delete=False) as f:
        f.write(certificate)
        path = f.name
    try:
        run = subprocess.run(
            [
                "go",
                "run",
                "./cmd/redundanet",
                "identity",
                "verify-certificate",
                "--network",
                network,
                path,
            ],
            cwd=AGENT_DIR,
            capture_output=True,
            text=True,
            timeout=600,
            check=False,
        )
    finally:
        Path(path).unlink(missing_ok=True)
    if run.returncode != 0:
        reason = run.stderr.strip().splitlines()[-1] if run.stderr.strip() else "no reason given"
        raise JoinError(
            "the device certificate does not verify: " + reason.removeprefix("redundanet: ")
        )
    try:
        info = json.loads(run.stdout)
    except json.JSONDecodeError as err:
        raise JoinError(f"the certificate check printed no usable result: {err}") from None
    return info


def check_device_request(req: DeviceRequest, manifest: Any, info: dict[str, Any]) -> str:
    """The member exists, the certificate is theirs and for this device with
    the storage role, and the device is new. Returns the member key."""
    if not isinstance(manifest, dict) or manifest.get("schema") != 3:
        raise JoinError("the manifest is not a RedundaNet 3 manifest (schema: 3)")
    members = _entries(manifest, "members")
    member = next((m for m in members if str(m.get("name")) == req.member), None)
    if member is None:
        raise JoinError(
            f"no member named {req.member} is in the manifest; a new member sends the "
            "membership request instead, with the node in it"
        )
    key = str(member.get("key", "")).strip().lower()
    network = str((manifest.get("network") or {}).get("name", ""))
    if str(info.get("network", "")) != network:
        raise JoinError(
            f"the certificate is for network {shown(str(info.get('network', '')))}, not {network}"
        )
    if str(info.get("member", "")).lower() != key:
        raise JoinError(
            f"the certificate was issued by another member key, not the one the manifest "
            f"lists for {req.member}; pair the node from a device of that member"
        )
    if str(info.get("device", "")).lower() != req.device_key:
        raise JoinError("the certificate is for another device than the Device key row names")
    roles = info.get("roles") or []
    if "storage" not in roles:
        raise JoinError(
            "the certificate grants no storage role; pair the node again with "
            "`redundanet device pair --storage`"
        )
    keys: dict[str, str] = {}
    for what, entries, field in (
        ("operator", _entries(manifest, "operators"), "key"),
        ("hub", _entries(manifest, "hubs"), "key"),
        ("member", members, "key"),
        ("storage device", _entries(manifest, "storage"), "device"),
    ):
        for entry in entries:
            keys[str(entry.get(field, "")).strip().lower()] = f"{what} {entry.get('name')}"
    if req.device_key in keys:
        raise JoinError(
            f"Device key is already in the manifest as the key of {keys[req.device_key]}"
        )
    taken = {str(s.get("name")) for s in _entries(manifest, "storage")} | {
        str(h.get("name")) for h in _entries(manifest, "hubs")
    }
    if req.device_name in taken:
        raise JoinError(
            f"a storage device or hub named {req.device_name} already exists; pick another device name"
        )
    return key


def render_device_storage(req: DeviceRequest, indent: str) -> list[str]:
    """The `storage:` entry for the member's new device."""
    lines = [
        f"{indent}- member: {yaml_scalar(req.member)}",
        f"{indent}  device: {yaml_scalar(req.device_key)}",
        f"{indent}  name: {yaml_scalar(req.device_name)}",
    ]
    if req.udp_port:
        lines.append(f"{indent}  udp_port: {req.udp_port}")
    return lines


def expected_device_storage(req: DeviceRequest) -> dict[str, Any]:
    entry: dict[str, Any] = {
        "member": req.member,
        "device": req.device_key,
        "name": req.device_name,
    }
    if req.udp_port:
        entry["udp_port"] = req.udp_port
    return entry


def set_member_contribution(text: str, name: str, gb: int) -> str:
    """Rewrite one member's `contribution_gb` in the text of the top-level
    `members:` list, touching nothing else."""
    lines = text.split("\n")
    try:
        start = next(i for i, line in enumerate(lines) if re.fullmatch(r"members:\s*(#.*)?", line))
    except StopIteration:
        raise EditError("no top-level `members:` list") from None
    in_member = False
    for i in range(start + 1, len(lines)):
        line = lines[i]
        if line and not line[0].isspace():
            break  # the next top-level key
        item = re.fullmatch(r"(\s*)- name:\s*(.+?)\s*", line)
        if item:
            in_member = yaml.safe_load(item.group(2)) == name
            continue
        if not in_member:
            continue
        field = re.fullmatch(r"(\s+contribution_gb:[ \t]*)(\d+)([ \t]*(?:#.*)?)", line)
        if field:
            lines[i] = f"{field.group(1)}{gb}{field.group(3)}"
            return "\n".join(lines)
    raise EditError(f"member {name} has no `contribution_gb:` line to rewrite")


def verify_device_roundtrip(data: Any, req: DeviceRequest, revision: int) -> None:
    if not isinstance(data, dict):
        raise EditError("the edited manifest is not a mapping")
    if data.get("revision") != revision:
        raise EditError(f"revision read back as {data.get('revision')!r}, not {revision}")
    devices = [s for s in _entries(data, "storage") if str(s.get("name")) == req.device_name]
    if len(devices) != 1 or dict(devices[0]) != expected_device_storage(req):
        raise EditError(f"the storage entry read back as {devices!r}")
    if req.contribution_gb is not None:
        member = next(m for m in _entries(data, "members") if str(m.get("name")) == req.member)
        if member.get("contribution_gb") != req.contribution_gb:
            raise EditError(f"contribution_gb read back as {member.get('contribution_gb')!r}")


def process_device(
    issue_body: str,
    manifest_path: Path,
    verify_certificate: Callable[[str, str], dict[str, Any]] = verify_certificate_with_go,
) -> DeviceResult:
    """Parse a storage device request and add the device to the manifest on
    disk. The file is written only after every check passed."""
    try:
        req = parse_device_request(issue_body)
        text = manifest_path.read_text()
        data = yaml.safe_load(text)
        network = (
            str(((data or {}).get("network") or {}).get("name", ""))
            if isinstance(data, dict)
            else ""
        )
        info = verify_certificate(req.certificate, network)
        key = check_device_request(req, data, info)
        new_text = append_items(text, "storage", lambda indent: render_device_storage(req, indent))
        if req.contribution_gb is not None:
            new_text = set_member_contribution(new_text, req.member, req.contribution_gb)
        new_text, revision = bump_revision(new_text)
        verify_device_roundtrip(yaml.safe_load(new_text), req, revision)
    except JoinError as err:
        return DeviceResult(success=False, error=str(err))
    except (EditError, yaml.YAMLError, OSError) as err:
        return DeviceResult(success=False, error=f"could not update the manifest: {err}")
    manifest_path.write_text(new_text)
    return DeviceResult(success=True, request=req, revision=revision, member_key=key)


def compose_device_pr(req: DeviceRequest, issue_number: str, revision: int) -> dict[str, str]:
    """Title, body and commit message of the pull request for a storage device."""
    device = f"`{req.device_name}` (`{req.device_key}`)"
    if req.udp_port:
        device += f", UDP port {req.udp_port}"
    contribution = (
        f"now {req.contribution_gb} GB" if req.contribution_gb is not None else "unchanged"
    )
    body = f"""## RedundaNet 3 storage device

This PR adds a storage device of member `{req.member}` to the RedundaNet 3 manifest as revision {revision}: the `storage:` entry{" and the member's new `contribution_gb`" if req.contribution_gb is not None else ""}.

| Field | Value |
|-------|-------|
| **Member** | `{req.member}` |
| **Storage device** | {device} |
| **Contribution** | {contribution} |
| **Proof** | the node's certificate: issued by the member's key, for this device, with the storage role (verified by `redundanet identity verify-certificate`) |

### Maintainer checklist
- [ ] Review the entry{" and the contribution" if req.contribution_gb is not None else ""}
- [ ] On the laptop with the operator key: `scripts/sign.sh manifest join-request/{issue_number}`. It checks out this branch, shows the manifest diff, checks that the revision rises and validates, signs, pushes `manifests/manifest.sig`, waits for the checks and merges; every agent applies revision {revision} within five minutes.

The `validate` check on this PR runs the Python and Go validators inline (validate.yml does not run on bot-created PRs). The signature check is informational and fails until `manifest.sig` is signed.

Closes #{issue_number}

---
*Auto-generated from storage device request #{issue_number}*
"""
    return {
        "pr_title": f"Add storage device {req.device_name} for member {req.member} (Issue #{issue_number})",
        "pr_body": body,
        "commit_message": (
            f"Add storage device {req.device_name} of member {req.member} to the RedundaNet 3 "
            f"manifest (revision {revision})"
        ),
    }


def compose_device_next_steps(req: DeviceRequest, revision: int) -> str:
    """The closing comment on a storage device issue; the workflow fills in {PR_URL}."""
    return f"""**Storage device request processed.**

Storage device `{req.device_name}` of member `{req.member}` goes into the RedundaNet 3 manifest as revision {revision}.

**Pull Request**: {{PR_URL}}

This issue is being closed. What happens next:

1. The maintainer reviews, signs `manifest.sig` offline with the operator key, and merges. Every agent applies the new revision within five minutes.
2. The node is admitted from then on and appears on the status page under your name: {STATUS_PAGE}
3. On the node, `sudo redundanet3 status` shows it; on your laptop, `redundanet setup` shows nothing left to request.

*Thank you for contributing to RedundaNet!*"""


# --- editing the manifest text ------------------------------------------------


def yaml_scalar(value: str) -> str:
    """value as a plain YAML scalar, double-quoted when YAML would read it as
    something else (a name made of digits, for instance)."""
    if yaml.safe_load(value) == value:
        return value
    return json.dumps(value)


def append_items(
    text: str, key: str, render: Callable[[str], list[str]], default_indent: str = "  "
) -> str:
    """Append items to the top-level list `key` of a YAML document, textually.

    `render` gets the indentation of the list's existing items (or
    `default_indent` when there are none) and returns the lines to add. Items
    may sit at column 0 (`- name:`, the v2 `nodes` style) or indented (the v3
    lists); the list ends at the next non-blank column-0 line that is not an
    item. Blank lines and column-0 comments after the last item keep their
    place.
    """
    if not text.endswith("\n"):
        text += "\n"
    lines = text.split("\n")
    key_re = re.compile(rf"^{re.escape(key)}:(\s*\[\])?\s*(#.*)?$")
    hits = [i for i, line in enumerate(lines) if key_re.match(line)]
    if len(hits) != 1:
        raise EditError(f"expected one top-level `{key}:` line, found {len(hits)}")
    start = hits[0]
    match = key_re.match(lines[start])
    if match and match.group(1):  # `key: []`
        lines[start] = f"{key}:"
        end, indent = start + 1, default_indent
    else:
        end = start + 1
        found: str | None = None
        for i in range(start + 1, len(lines)):
            line = lines[i]
            if not line.strip():
                continue
            item = line.lstrip().startswith("- ")
            if not line[0].isspace() and not item:
                if line.startswith("#"):
                    continue
                break
            if found is None and item:
                found = line[: len(line) - len(line.lstrip())]
            end = i + 1
        indent = found if found is not None else default_indent
    lines[end:end] = render(indent)
    return "\n".join(lines)


def bump_revision(text: str) -> tuple[str, int]:
    """Raise the top-level `revision` by one; returns (text, new revision)."""
    pattern = re.compile(r"^(revision:[ \t]*)(\d+)([ \t]*(?:#.*)?)$", re.MULTILINE)
    hits = list(pattern.finditer(text))
    if len(hits) != 1:
        raise EditError(f"expected one top-level `revision:` line, found {len(hits)}")
    match = hits[0]
    revision = int(match.group(2)) + 1
    return text[: match.start(2)] + str(revision) + text[match.end(2) :], revision


def render_member(req: JoinRequest, joined: str, indent: str) -> list[str]:
    """The `members:` entry, in the file's two-space style."""
    i1, i2, i3 = indent + "  ", indent + "    ", indent + "      "
    lines = [
        f"{indent}- name: {yaml_scalar(req.name)}",
        f"{i1}key: {yaml_scalar(req.key)}",
        f"{i1}contribution_gb: {req.contribution_gb}",
        f"{i1}joined: {joined}",
        f"{i1}status: active",
    ]
    if req.legacy_fingerprint and req.legacy_signature:
        lines += [
            f"{i1}legacy_pgp:",
            f"{i2}fingerprint: {yaml_scalar(req.legacy_fingerprint)}",
            f"{i2}signature: |",
        ]
        lines += [
            f"{i3}{line}" if line else "" for line in req.legacy_signature.rstrip("\n").split("\n")
        ]
    return lines


def render_storage(req: JoinRequest, indent: str) -> list[str]:
    """The `storage:` entry for the member's device."""
    assert req.device_key and req.device_name  # noqa: S101 (checked by parse_request)
    lines = [
        f"{indent}- member: {yaml_scalar(req.name)}",
        f"{indent}  device: {yaml_scalar(req.device_key)}",
        f"{indent}  name: {yaml_scalar(req.device_name)}",
    ]
    if req.udp_port:
        lines.append(f"{indent}  udp_port: {req.udp_port}")
    return lines


def expected_member(req: JoinRequest, joined: str) -> dict[str, Any]:
    entry: dict[str, Any] = {
        "name": req.name,
        "key": req.key,
        "contribution_gb": req.contribution_gb,
        "joined": joined,
        "status": "active",
    }
    if req.legacy_fingerprint:
        entry["legacy_pgp"] = {
            "fingerprint": req.legacy_fingerprint,
            "signature": req.legacy_signature,
        }
    return entry


def expected_storage(req: JoinRequest) -> dict[str, Any]:
    entry: dict[str, Any] = {"member": req.name, "device": req.device_key, "name": req.device_name}
    if req.udp_port:
        entry["udp_port"] = req.udp_port
    return entry


def verify_roundtrip(data: Any, req: JoinRequest, joined: str, revision: int) -> None:
    """The edited text must read back as exactly the entries that were written."""
    if not isinstance(data, dict):
        raise EditError("the edited manifest is not a mapping")
    if data.get("revision") != revision:
        raise EditError(f"revision read back as {data.get('revision')!r}, not {revision}")
    members = [m for m in _entries(data, "members") if str(m.get("name")) == req.name]
    if len(members) != 1:
        raise EditError(f"{len(members)} members named {req.name} read back, not one")
    got = dict(members[0])
    got["joined"] = str(got.get("joined"))
    if got != expected_member(req, joined):
        raise EditError(f"the member entry read back as {got!r}")
    if req.device_key:
        devices = [s for s in _entries(data, "storage") if str(s.get("name")) == req.device_name]
        if len(devices) != 1:
            raise EditError(f"{len(devices)} storage devices named {req.device_name} read back")
        if dict(devices[0]) != expected_storage(req):
            raise EditError(f"the storage entry read back as {devices[0]!r}")


def attest(
    req: JoinRequest,
    data: dict[str, Any],
    fetch: Callable[[str], list[str]],
    verify_signature: Callable[[str, list[str], str, str], bool],
) -> str:
    """Check the new member's legacy attestation with the shared validator.

    Returns the status line for the pull request. Fails closed: a record that
    does not verify, or names a key of another member's nodes, rejects the
    request.
    """
    member = next(m for m in _entries(data, "members") if str(m.get("name")) == req.name)
    subset = {"schema": 3, "members": [member], "nodes": data.get("nodes") or []}
    errors, notes = legacy.validate(subset, fetch=fetch, verify_signature=verify_signature)
    if errors:
        raise JoinError("the legacy attestation failed: " + "; ".join(errors))
    if req.legacy_fingerprint:
        return (
            f"member key attested by GPG key {req.legacy_fingerprint} of a RedundaNet 2 node "
            f"of {req.name} (signature verified against the keyservers)"
        )
    if any("still to come" in note for note in notes):
        return f"none; {req.name} is a RedundaNet 2 member and the attestation is still to come"
    return "none (new member)"


def process(
    issue_body: str,
    manifest_path: Path,
    author: str | None = None,
    fetch_key: Callable[[str], list[str]] = legacy.fetch_keys,
    verify_signature: Callable[[str, list[str], str, str], bool] = legacy.verify,
    today: Callable[[], str] = utc_today,
) -> JoinResult:
    """Parse a request and add the member (and device) to the manifest on disk.

    The file is written only after every check passed; on failure it is
    untouched and the result carries the reason.
    """
    try:
        req = parse_request(issue_body, author)
        text = manifest_path.read_text()
        check_manifest(req, yaml.safe_load(text), author)
        joined = today()
        new_text = append_items(text, "members", lambda indent: render_member(req, joined, indent))
        if req.device_key:
            new_text = append_items(new_text, "storage", lambda indent: render_storage(req, indent))
        new_text, revision = bump_revision(new_text)
        data = yaml.safe_load(new_text)
        verify_roundtrip(data, req, joined, revision)
        status = attest(req, data, fetch_key, verify_signature)
    except JoinError as err:
        return JoinResult(success=False, error=str(err))
    except (EditError, yaml.YAMLError, OSError) as err:
        return JoinResult(success=False, error=f"could not update the manifest: {err}")
    manifest_path.write_text(new_text)
    return JoinResult(success=True, request=req, revision=revision, legacy=status)


# --- what the workflow posts ---------------------------------------------------


def compose_pr(
    req: JoinRequest, issue_number: str, revision: int, legacy_status: str
) -> dict[str, str]:
    """Title, body and commit message of the pull request."""
    if req.device_key:
        device = f"`{req.device_name}` (`{req.device_key}`)"
        if req.udp_port:
            device += f", UDP port {req.udp_port}"
        adds = "the `members:` entry and the `storage:` entry"
    else:
        device = "none"
        adds = "the `members:` entry"
    review = "Review the entries"
    if req.contribution_gb == 0:
        review += (
            "; this member contributes no disk, so set `allocation_gb` and `sponsor` "
            "(another member or `reserve`) before signing"
        )
    body = f"""## RedundaNet 3 membership

This PR adds a member to the RedundaNet 3 manifest as revision {revision}: {adds}.

| Field | Value |
|-------|-------|
| **Member name** | `{req.name}` |
| **Member key** | `{req.key}` |
| **Contribution** | {req.contribution_gb} GB |
| **Storage device** | {device} |
| **Legacy attestation** | {legacy_status} |

### Maintainer checklist
- [ ] {review}
- [ ] On the laptop with the operator key: `scripts/sign.sh manifest join-request/{issue_number}`. It checks out this branch, shows the manifest diff, checks that the revision rises and validates, signs, pushes `manifests/manifest.sig`, waits for the checks and merges; every agent applies revision {revision} within five minutes.

By hand instead: `git fetch origin && git checkout join-request/{issue_number}`, then `cd agent && go run ./cmd/redundanet manifest sign --key operator.key --manifest ../manifests/manifest.yaml` (the operator key stays offline; writes `manifests/manifest.sig`), commit the signature, push, merge.

The `validate` check on this PR runs the Python and Go validators inline (validate.yml does not run on bot-created PRs). The signature check is informational and fails until `manifest.sig` is signed.

Closes #{issue_number}

---
*Auto-generated from join request #{issue_number}*
"""
    return {
        "pr_title": f"Add member {req.name} (Issue #{issue_number})",
        "pr_body": body,
        "commit_message": f"Add member {req.name} to the RedundaNet 3 manifest (revision {revision})",
    }


def compose_next_steps(req: JoinRequest, revision: int) -> str:
    """The closing comment on the issue; the workflow fills in {PR_URL}."""
    device = f" with storage device `{req.device_name}`" if req.device_name else ""
    storage = (
        f"\n5. Your storage node `{req.device_name}` appears on the status page once its agent connects."
        if req.device_name
        else ""
    )
    return f"""**Membership request processed.**

Member `{req.name}` goes into the RedundaNet 3 manifest as revision {revision}{device}.

**Pull Request**: {{PR_URL}}

This issue is being closed. What happens next:

1. The maintainer reviews, signs `manifest.sig` offline with the operator key, and merges. Every agent applies the new revision within five minutes. Your devices are admitted from then on.
2. On your laptop, install the service: `redundanet service install --network redundanet`
3. Add a backup set: `redundanet backup add NAME FOLDER`
4. `redundanet status` shows your device; the network page is {STATUS_PAGE}{storage}

*Thank you for contributing to RedundaNet!*"""


def main() -> int:
    issue_body = os.environ.get("ISSUE_BODY", "")
    issue_number = os.environ.get("ISSUE_NUMBER", "")
    author = os.environ.get("ISSUE_AUTHOR")

    # Do NOT echo the untrusted issue body to the log: a line that begins
    # with `::` would be interpreted as a workflow command.
    if is_device_request(issue_body):
        print(f"Processing issue #{issue_number} as a RedundaNet 3 storage device request")
        device = process_device(issue_body, Path("manifests/manifest.yaml"))
        if not device.success or device.request is None:
            print("::error::" + device.error)
            set_output(success="false", error=device.error)
            return 1
        dreq = device.request
        print(f"Member: {dreq.member} ({device.member_key})")
        print(f"Storage device: {dreq.device_name} ({dreq.device_key})")
        print(f"Manifest revision: {device.revision}")
        set_output(
            success="true",
            member_name=dreq.member,
            member_key=device.member_key,
            contribution_gb=str(dreq.contribution_gb if dreq.contribution_gb is not None else ""),
            device_key=dreq.device_key,
            device_name=dreq.device_name,
            udp_port=str(dreq.udp_port or ""),
            legacy="",
            revision=str(device.revision),
            next_steps=compose_device_next_steps(dreq, device.revision),
            **compose_device_pr(dreq, issue_number, device.revision),
        )
        print("Done!")
        return 0

    print(f"Processing issue #{issue_number} as a RedundaNet 3 membership request")

    result = process(issue_body, Path("manifests/manifest.yaml"), author=author)
    if not result.success or result.request is None:
        print("::error::" + result.error)
        set_output(success="false", error=result.error)
        return 1

    req = result.request
    print(f"Member: {req.name} ({req.key}), {req.contribution_gb} GB")
    if req.device_key:
        print(f"Storage device: {req.device_name} ({req.device_key})")
    print(f"Legacy attestation: {result.legacy}")
    print(f"Manifest revision: {result.revision}")
    set_output(
        success="true",
        member_name=req.name,
        member_key=req.key,
        contribution_gb=str(req.contribution_gb),
        device_key=req.device_key or "",
        device_name=req.device_name or "",
        udp_port=str(req.udp_port or ""),
        legacy=result.legacy,
        revision=str(result.revision),
        next_steps=compose_next_steps(req, result.revision),
        **compose_pr(req, issue_number, result.revision, result.legacy),
    )
    print("Done!")
    return 0


if __name__ == "__main__":
    sys.exit(main())
