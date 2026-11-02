#!/usr/bin/env python3
"""Check the RedundaNet 3 members' GPG attestations (RFC 0001, migration step 0).

A member migrating from RedundaNet 2 signs their new member key with a GPG
key the v2 manifest lists under one of their nodes, and records it in their
`members:` entry as `legacy_pgp: {fingerprint, signature}`. For each such
record this script checks that the fingerprint belongs to one of that
member's v2 nodes, fetches the key from the keyservers, and verifies the
armored detached signature over the member key string. Any failure exits 1.
A v2-only manifest, or a member without the record, passes with a note: the
attestation is what ties the new key to the person, so a missing one is
visible in the log, not fatal.

Usage: validate_v3_members.py [manifests/manifest.yaml]
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path

import yaml

KEYSERVERS = ("keys.openpgp.org", "keyserver.ubuntu.com")
FINGERPRINT = re.compile(r"^[0-9A-F]{40}$")


def normalize(fingerprint: str) -> str:
    """Uppercase hex, no spaces, no 0x prefix."""
    return fingerprint.replace(" ", "").upper().removeprefix("0X")


def fetch_keys(fingerprint: str) -> list[str]:
    """The armored public key for a fingerprint from every keyserver that has it.

    keys.openpgp.org strips the user IDs of a key whose e-mail is not
    verified, and GnuPG refuses to import a key without any, so every
    answer is kept and the importable one wins.
    """
    found: list[str] = []
    failures: list[str] = []
    for server in KEYSERVERS:
        query = urllib.parse.urlencode({"op": "get", "search": "0x" + fingerprint, "options": "mr"})
        url = f"https://{server}/pks/lookup?{query}"
        try:
            with urllib.request.urlopen(url, timeout=20) as response:
                body = response.read().decode("utf-8", "replace")
        except OSError as err:
            failures.append(f"{server}: {err}")
            continue
        if "BEGIN PGP PUBLIC KEY BLOCK" in body:
            found.append(body)
        else:
            failures.append(f"{server}: no key block in the answer")
    if not found:
        raise LookupError("; ".join(failures) or "no keyserver answered")
    return found


def verify(member_key: str, armored_keys: list[str], signature: str, fingerprint: str) -> bool:
    """Whether signature is a good detached PGP signature over member_key by the key with fingerprint."""
    gpg = shutil.which("gpg") or shutil.which("gpg2")
    if gpg is None:
        raise RuntimeError("gpg is not installed")
    with tempfile.TemporaryDirectory() as home:
        Path(home).chmod(0o700)
        env = {**os.environ, "GNUPGHOME": home}
        base = [gpg, "--batch", "--no-tty", "--quiet"]
        imported = 0
        for armored in armored_keys:
            if (
                subprocess.run(
                    [*base, "--import"],
                    input=armored.encode(),
                    env=env,
                    capture_output=True,
                    check=False,
                ).returncode
                == 0
            ):
                imported += 1
        if imported == 0:
            raise RuntimeError(
                "no keyserver served an importable copy of the key (one with user IDs)"
            )
        message = Path(home) / "member.key"
        message.write_bytes(member_key.encode())
        sig = Path(home) / "member.sig"
        sig.write_text(signature if signature.endswith("\n") else signature + "\n")
        result = subprocess.run(
            [
                *base,
                "--status-fd",
                "1",
                "--trust-model",
                "always",
                "--verify",
                str(sig),
                str(message),
            ],
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
    for line in result.stdout.splitlines():
        # VALIDSIG <signing key fpr> <date> <timestamp> <expiry> <version> <reserved> <pk algo> <hash> <class> <primary fpr>
        if line.startswith("[GNUPG:] VALIDSIG "):
            fields = line.split()
            if fingerprint in (fields[2].upper(), fields[-1].upper()):
                return True
    return False


def validate(
    manifest: object,
    fetch: Callable[[str], list[str]] = fetch_keys,
    verify_signature: Callable[[str, list[str], str, str], bool] = verify,
) -> tuple[list[str], list[str]]:
    """Check every legacy_pgp record; returns (errors, notes)."""
    errors: list[str] = []
    notes: list[str] = []
    if not isinstance(manifest, dict) or manifest.get("schema") != 3:
        notes.append("not a RedundaNet 3 manifest; nothing to check")
        return errors, notes

    v2_keys: dict[str, set[str]] = {}
    for node in manifest.get("nodes") or []:
        if isinstance(node, dict) and node.get("member") and node.get("gpg_key_id"):
            v2_keys.setdefault(str(node["member"]), set()).add(normalize(str(node["gpg_key_id"])))

    for member in manifest.get("members") or []:
        if not isinstance(member, dict):
            continue
        name, key, record = (
            str(member.get("name")),
            str(member.get("key")),
            member.get("legacy_pgp"),
        )
        if record is None:
            if name in v2_keys:
                notes.append(
                    f"{name}: a RedundaNet 2 member without legacy_pgp; the attestation is still to come"
                )
            continue
        if (
            not isinstance(record, dict)
            or not record.get("fingerprint")
            or not record.get("signature")
        ):
            errors.append(f"{name}: legacy_pgp needs fingerprint and signature")
            continue
        fingerprint = normalize(str(record["fingerprint"]))
        if not FINGERPRINT.match(fingerprint):
            errors.append(f"{name}: legacy_pgp.fingerprint is not a 40-character fingerprint")
            continue
        if fingerprint not in v2_keys.get(name, set()):
            errors.append(f"{name}: {fingerprint} is not a GPG key of {name}'s RedundaNet 2 nodes")
            continue
        try:
            armored = fetch(fingerprint)
        except LookupError as err:
            errors.append(f"{name}: key {fingerprint} not found on the keyservers ({err})")
            continue
        try:
            good = verify_signature(key, armored, str(record["signature"]), fingerprint)
        except (OSError, RuntimeError, subprocess.CalledProcessError) as err:
            errors.append(f"{name}: could not verify the signature: {err}")
            continue
        if good:
            notes.append(f"{name}: member key attested by GPG key {fingerprint}")
        else:
            errors.append(
                f"{name}: the legacy_pgp signature does not verify over the member key with {fingerprint}"
            )
    return errors, notes


def main() -> None:
    path = sys.argv[1] if len(sys.argv) > 1 else "manifests/manifest.yaml"
    try:
        with Path(path).open() as f:
            manifest = yaml.safe_load(f)
    except (OSError, yaml.YAMLError) as err:
        print(f"❌ {path}: {err}")
        sys.exit(1)
    errors, notes = validate(manifest)
    for note in notes:
        print(f"note: {note}")
    if errors:
        print(f"❌ RedundaNet 3 member attestations failed for {path}:")
        for error in errors:
            print(f"  - {error}")
        sys.exit(1)
    print(f"✅ {path}: member attestations ok")


if __name__ == "__main__":
    main()
