"""Unit tests for the member attestation check (.github/scripts/validate_v3_members.py)."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Iterator
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO_ROOT / ".github" / "scripts"))

import validate_v3_members as v3  # noqa: E402

MEMBER_KEY = "wekrreaqlio4j3m4vzxo6hn5be2sa2gxxv7gx3mq54m4xffztv2a"


@pytest.fixture(scope="module")
def gpg_key() -> Iterator[dict[str, str]]:
    gpg = shutil.which("gpg")
    if gpg is None:
        pytest.skip("gpg is not installed")
    # A short path: gpg-agent's socket path must fit a unix socket address,
    # which pytest's own temporary directories do not.
    home = Path(tempfile.mkdtemp(prefix="rn3gpg-"))
    home.chmod(0o700)
    env = {**os.environ, "GNUPGHOME": str(home)}
    base = [
        gpg,
        "--batch",
        "--no-tty",
        "--quiet",
        "--pinentry-mode",
        "loopback",
        "--passphrase",
        "",
    ]
    subprocess.run(
        [*base, "--quick-gen-key", "Test Member <test@example.org>", "ed25519", "sign", "never"],
        env=env,
        check=True,
    )
    listing = subprocess.run(
        [gpg, "--with-colons", "--list-keys"], env=env, check=True, capture_output=True, text=True
    ).stdout
    fingerprint = next(
        line.split(":")[9] for line in listing.splitlines() if line.startswith("fpr:")
    )
    armored = subprocess.run(
        [gpg, "--armor", "--export", fingerprint],
        env=env,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    signature = subprocess.run(
        [*base, "--armor", "--detach-sign", "-u", fingerprint],
        input=MEMBER_KEY,
        env=env,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    yield {"fingerprint": fingerprint, "armored": armored, "signature": signature}
    subprocess.run(
        [shutil.which("gpgconf") or "gpgconf", "--kill", "gpg-agent"], env=env, check=False
    )
    shutil.rmtree(home, ignore_errors=True)


def manifest(fingerprint: str, signature: str | None, node_member: str = "alice") -> dict:
    member: dict = {
        "name": "alice",
        "key": MEMBER_KEY,
        "contribution_gb": 0,
        "joined": "2026-11-02",
        "status": "active",
    }
    if signature is not None:
        member["legacy_pgp"] = {"fingerprint": fingerprint, "signature": signature}
    return {
        "schema": 3,
        "revision": 1,
        "network": {"name": "demo"},
        "members": [member],
        "nodes": [{"name": "node-1", "member": node_member, "gpg_key_id": fingerprint}],
    }


class TestAttestation:
    def test_good_signature_passes(self, gpg_key):
        errors, notes = v3.validate(
            manifest(gpg_key["fingerprint"], gpg_key["signature"]),
            fetch=lambda _f: [gpg_key["armored"]],
        )
        assert errors == [] and any("attested" in n for n in notes)

    def test_fingerprint_may_be_spaced_lowercase(self, gpg_key):
        spaced = " ".join(gpg_key["fingerprint"].lower()[i : i + 4] for i in range(0, 40, 4))
        m = manifest(gpg_key["fingerprint"], gpg_key["signature"])
        m["members"][0]["legacy_pgp"]["fingerprint"] = spaced
        errors, _ = v3.validate(m, fetch=lambda _f: [gpg_key["armored"]])
        assert errors == []

    def test_tampered_signature_fails(self, gpg_key):
        lines = gpg_key["signature"].splitlines()
        body = [
            i
            for i, line in enumerate(lines)
            if line and not line.startswith("-") and "Version" not in line
        ][1]
        lines[body] = ("A" if lines[body][0] != "A" else "B") + lines[body][1:]
        errors, _ = v3.validate(
            manifest(gpg_key["fingerprint"], "\n".join(lines)),
            fetch=lambda _f: [gpg_key["armored"]],
        )
        assert errors and "does not verify" in errors[0]

    def test_signature_over_another_key_fails(self, gpg_key):
        m = manifest(gpg_key["fingerprint"], gpg_key["signature"])
        m["members"][0]["key"] = MEMBER_KEY[:-1] + "b"
        errors, _ = v3.validate(m, fetch=lambda _f: [gpg_key["armored"]])
        assert errors and "does not verify" in errors[0]

    def test_key_of_another_members_node_is_refused(self, gpg_key):
        errors, _ = v3.validate(
            manifest(gpg_key["fingerprint"], gpg_key["signature"], node_member="bob"),
            fetch=lambda _f: [gpg_key["armored"]],
        )
        assert errors and "not a GPG key of alice" in errors[0]

    def test_missing_key_on_keyservers(self, gpg_key):
        def nowhere(_f: str) -> list[str]:
            raise LookupError("keys.openpgp.org: 404")

        errors, _ = v3.validate(
            manifest(gpg_key["fingerprint"], gpg_key["signature"]), fetch=nowhere
        )
        assert errors and "not found on the keyservers" in errors[0]

    def test_member_without_record_is_a_note(self, gpg_key):
        errors, notes = v3.validate(
            manifest(gpg_key["fingerprint"], None), fetch=lambda _f: [gpg_key["armored"]]
        )
        assert errors == [] and any("still to come" in n for n in notes)

    def test_bad_records(self, gpg_key):
        m = manifest(gpg_key["fingerprint"], gpg_key["signature"])
        m["members"][0]["legacy_pgp"] = {"fingerprint": "ABCD"}
        assert v3.validate(m, fetch=lambda _f: "")[0] == [
            "alice: legacy_pgp needs fingerprint and signature"
        ]
        m["members"][0]["legacy_pgp"] = {"fingerprint": "ABCD", "signature": "x"}
        assert "40-character" in v3.validate(m, fetch=lambda _f: [])[0][0]

    def test_unimportable_copies_are_an_error(self, gpg_key):
        stripped = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nbm90IGEga2V5\n-----END PGP PUBLIC KEY BLOCK-----\n"
        errors, _ = v3.validate(
            manifest(gpg_key["fingerprint"], gpg_key["signature"]), fetch=lambda _f: [stripped]
        )
        assert errors and "importable" in errors[0]
        # A stripped copy beside a full one is fine: the full one is imported.
        errors, _ = v3.validate(
            manifest(gpg_key["fingerprint"], gpg_key["signature"]),
            fetch=lambda _f: [stripped, gpg_key["armored"]],
        )
        assert errors == []

    def test_v2_only_manifest_passes(self):
        errors, notes = v3.validate({"network": {"name": "redundanet"}, "nodes": []})
        assert errors == [] and notes == ["not a RedundaNet 3 manifest; nothing to check"]
