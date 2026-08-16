"""Unit tests for the join-request processing script (.github/scripts/process_join.py)."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest
import yaml
from hypothesis import given
from hypothesis import strategies as st

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / ".github" / "scripts"))

import process_join  # noqa: E402

# A syntactically valid (fake) full fingerprint for parse-level tests.
FPR = "333BEC68DD2BE971333BEC68DD2BE971333BEC68"


def issue_body(
    key_id: str = FPR,
    storage: str = "100GB",
    region: str = "europe",
    public_ip: str | None = None,
) -> str:
    rows = [
        "## New Node Application",
        "",
        "| Field | Value |",
        "|-------|-------|",
        f"| **GPG Key ID** | `{key_id}` |",
        f"| **Storage Contribution** | {storage} |",
        f"| **Region** | {region} |",
        f"| **Public IP** | {public_ip or 'None'} |",
    ]
    return "\n".join(rows)


def no_key(_key_id: str) -> None:
    """fetch_key stub: key not on any keyserver."""
    return None


@pytest.fixture(scope="module")
def real_key() -> tuple[str, str]:
    """A real OpenPGP key: (armored public key, full fingerprint)."""
    pgpy = pytest.importorskip("pgpy")
    from pgpy.constants import (
        CompressionAlgorithm,
        HashAlgorithm,
        KeyFlags,
        PubKeyAlgorithm,
        SymmetricKeyAlgorithm,
    )

    key = pgpy.PGPKey.new(PubKeyAlgorithm.RSAEncryptOrSign, 2048)
    uid = pgpy.PGPUID.new("Join Test", email="join@test.local")
    key.add_uid(
        uid,
        usage={KeyFlags.Sign},
        hashes=[HashAlgorithm.SHA256],
        ciphers=[SymmetricKeyAlgorithm.AES256],
        compression=[CompressionAlgorithm.ZLIB],
    )
    fingerprint = str(key.fingerprint).replace(" ", "").upper()
    return str(key.pubkey), fingerprint


class TestParsing:
    def test_full_fingerprint_accepted_and_normalized(self):
        assert process_join.parse_gpg_key_id(issue_body(FPR)) == FPR
        spaced = " ".join(FPR[i : i + 4] for i in range(0, 40, 4))
        assert process_join.parse_gpg_key_id(issue_body(f"0x{spaced}")) == FPR
        assert process_join.parse_gpg_key_id(issue_body(FPR.lower())) == FPR

    def test_short_key_ids_rejected(self):
        """8/16-char ids are Evil32-collision-prone: reject at parse time."""
        for short in ("ABCD1234", "DEADBEEFCAFE1234", FPR[-16:]):
            assert process_join.parse_gpg_key_id(issue_body(short)) is None

    def test_invalid_key_ids_rejected(self):
        for bad in ("nothex", "ABCD123", "A" * 41, "`; rm -rf /`", "ABCD1234\nEVIL=1"):
            assert process_join.parse_gpg_key_id(issue_body(bad)) is None

    def test_missing_key_row(self):
        assert process_join.parse_gpg_key_id("no table here") is None

    def test_region_sanitized(self):
        assert process_join.parse_region(issue_body(region="eu-west 2")) == "eu-west 2"
        assert process_join.parse_region(issue_body(region="Not specified")) == "unknown"
        # Anything outside the conservative charset is discarded, not passed on.
        assert process_join.parse_region(issue_body(region="x${{ evil }}")) == "unknown"

    def test_public_ip_must_be_literal_ip(self):
        assert process_join.parse_public_ip(issue_body(public_ip="203.0.113.7")) == "203.0.113.7"
        assert process_join.parse_public_ip(issue_body(public_ip="evil.example.com")) is None
        assert process_join.parse_public_ip(issue_body(public_ip="None")) is None

    @given(st.text(alphabet="0123456789ABCDEF", min_size=1, max_size=50))
    def test_key_id_regex_accepts_exactly_40(self, hex_str: str):
        parsed = process_join.parse_gpg_key_id(issue_body(hex_str))
        if len(hex_str) == 40:
            assert parsed == hex_str
        else:
            assert parsed is None


class TestIPAllocation:
    def test_first_ip_skips_infrastructure_range(self, tmp_path: Path):
        manifest = process_join.default_manifest()
        assert process_join.allocate_ip(manifest) == "10.100.0.10"

    def test_skips_used_ips(self):
        manifest = process_join.default_manifest()
        manifest["nodes"] = [
            {"name": "a", "internal_ip": "10.100.0.10", "vpn_ip": "10.100.0.10"},
            {"name": "b", "internal_ip": "10.100.0.11", "vpn_ip": "10.100.0.11"},
        ]
        assert process_join.allocate_ip(manifest) == "10.100.0.12"

    def test_reserves_first_ten_of_every_subnet(self):
        manifest = process_join.default_manifest()
        manifest["nodes"] = [
            {"name": f"n{i}", "internal_ip": f"10.100.0.{i}"} for i in range(10, 256)
        ]
        # .0.255 is a host in a /16; the allocator must jump to .1.10, skipping
        # .1.0-.1.9 (reserved range of the next /24).
        assert process_join.allocate_ip(manifest) == "10.100.1.10"

    def test_exhaustion_returns_none(self):
        manifest = process_join.default_manifest()
        manifest["network"]["vpn_network"] = (
            "10.200.0.0/28"  # hosts .1-.14, all < .10 skipped except none
        )
        manifest["nodes"] = [
            {"name": f"n{i}", "internal_ip": f"10.200.0.{i}"} for i in range(10, 15)
        ]
        assert process_join.allocate_ip(manifest) is None


class TestDuplicateKey:
    def test_exact_match_is_duplicate(self):
        manifest = {"nodes": [{"gpg_key_id": FPR}]}
        assert process_join.is_duplicate_key(manifest, FPR)
        assert process_join.is_duplicate_key(manifest, FPR.lower())

    def test_suffix_overlap_is_not_a_duplicate(self):
        """Exact comparison only: a different fingerprint sharing a suffix must
        NOT alias an existing node (suffixes are craftable)."""
        manifest = {"nodes": [{"gpg_key_id": FPR}]}
        other = "F" * 24 + FPR[-16:]  # different key, same 16-char suffix
        assert len(other) == 40
        assert not process_join.is_duplicate_key(manifest, other)


class TestProcess:
    def test_creates_manifest_and_node(self, tmp_path: Path, real_key):
        armored, fingerprint = real_key
        manifest_path = tmp_path / "manifest.yaml"
        result = process_join.process(
            issue_body(fingerprint, public_ip="203.0.113.7"),
            manifest_path,
            fetch_key=lambda _kid: armored,
        )

        assert result.success, result.error
        assert result.node_name == f"node-{fingerprint[-8:].lower()}"
        assert result.vpn_ip == "10.100.0.10"
        assert result.warnings == []  # verified key -> nothing advisory

        manifest = yaml.safe_load(manifest_path.read_text())
        node = manifest["nodes"][0]
        assert node["gpg_key_id"] == fingerprint
        assert node["status"] == "pending"
        assert node["is_publicly_accessible"] is True
        assert node["public_ip"] == "203.0.113.7"

    def test_key_not_on_keyserver_fails_closed(self, tmp_path: Path):
        """No keyserver copy -> the join FAILS (peers could never fetch the key
        to authenticate the node; and admitting unverified ids invites
        attacker-uploaded stand-ins)."""
        manifest_path = tmp_path / "manifest.yaml"
        result = process_join.process(issue_body(FPR), manifest_path, fetch_key=no_key)
        assert not result.success
        assert "not found" in result.error
        assert "publish" in result.error.lower()
        assert not manifest_path.exists()  # nothing was written

    def test_unparseable_keyserver_response_fails_closed(self, tmp_path: Path):
        result = process_join.process(
            issue_body(FPR), tmp_path / "manifest.yaml", fetch_key=lambda _kid: "garbage"
        )
        assert not result.success
        assert "could not be parsed" in result.error

    def test_duplicate_key_rejected(self, tmp_path: Path, real_key):
        armored, fingerprint = real_key
        manifest_path = tmp_path / "manifest.yaml"
        fetch = lambda _kid: armored  # noqa: E731
        assert process_join.process(issue_body(fingerprint), manifest_path, fetch_key=fetch).success
        result = process_join.process(issue_body(fingerprint), manifest_path, fetch_key=fetch)
        assert not result.success
        assert "already exists" in result.error

    def test_keyserver_returning_wrong_key_is_an_error(self, tmp_path: Path, real_key):
        """Evil32 defense: the fetched key's fingerprint must exactly equal the
        submitted one, or the join is rejected."""
        armored, _fingerprint = real_key
        result = process_join.process(
            issue_body("0" * 40),
            tmp_path / "manifest.yaml",
            fetch_key=lambda _kid: armored,
        )
        assert not result.success
        assert "does not match" in result.error

    def test_short_id_submission_reports_fingerprint_requirement(self, tmp_path: Path, real_key):
        _armored, fingerprint = real_key
        result = process_join.process(
            issue_body(fingerprint[-16:]), tmp_path / "m.yaml", fetch_key=no_key
        )
        assert not result.success
        assert "40-character" in result.error

    def test_invalid_body_reports_error(self, tmp_path: Path):
        result = process_join.process("free-form text", tmp_path / "m.yaml", fetch_key=no_key)
        assert not result.success
        assert "fingerprint" in result.error
