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


def issue_body(
    key_id: str = "333BEC68DD2BE971",
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


class TestParsing:
    def test_valid_key_ids(self):
        assert process_join.parse_gpg_key_id(issue_body("ABCD1234")) == "ABCD1234"
        assert (
            process_join.parse_gpg_key_id(issue_body("0xdead beef cafe 1234")) == "DEADBEEFCAFE1234"
        )
        assert process_join.parse_gpg_key_id(issue_body("A" * 40)) == "A" * 40

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
    def test_key_id_regex_accepts_exactly_8_16_40(self, hex_str: str):
        parsed = process_join.parse_gpg_key_id(issue_body(hex_str))
        if len(hex_str) in (8, 16, 40):
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


class TestProcess:
    def test_creates_manifest_and_node(self, tmp_path: Path):
        manifest_path = tmp_path / "manifest.yaml"
        result = process_join.process(
            issue_body("333BEC68DD2BE971", public_ip="203.0.113.7"),
            manifest_path,
            fetch_key=no_key,
        )

        assert result.success, result.error
        assert result.node_name == "node-dd2be971"
        assert result.vpn_ip == "10.100.0.10"
        assert result.warnings  # key not on keyserver -> advisory warning

        manifest = yaml.safe_load(manifest_path.read_text())
        node = manifest["nodes"][0]
        assert node["gpg_key_id"] == "333BEC68DD2BE971"
        assert node["status"] == "pending"
        assert node["is_publicly_accessible"] is True
        assert node["public_ip"] == "203.0.113.7"

    def test_duplicate_key_rejected(self, tmp_path: Path):
        manifest_path = tmp_path / "manifest.yaml"
        assert process_join.process(
            issue_body("333BEC68DD2BE971"), manifest_path, fetch_key=no_key
        ).success
        result = process_join.process(
            issue_body("333BEC68DD2BE971"), manifest_path, fetch_key=no_key
        )
        assert not result.success
        assert "already exists" in result.error

    def test_duplicate_detected_across_id_lengths(self, tmp_path: Path):
        """A short id matching an existing full fingerprint is still a duplicate."""
        manifest_path = tmp_path / "manifest.yaml"
        full = "1234567890ABCDEF1234567890ABCDEF12345678"
        assert process_join.process(issue_body(full), manifest_path, fetch_key=no_key).success
        result = process_join.process(issue_body(full[-16:]), manifest_path, fetch_key=no_key)
        assert not result.success

    def test_key_found_on_keyserver_pins_full_fingerprint(self, tmp_path: Path):
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
        armored = str(key.pubkey)

        manifest_path = tmp_path / "manifest.yaml"
        result = process_join.process(
            issue_body(fingerprint[-16:]), manifest_path, fetch_key=lambda _kid: armored
        )
        assert result.success, result.error
        # The manifest stores the full fingerprint, not the submitted short id.
        assert result.gpg_key_id == fingerprint
        manifest = yaml.safe_load(manifest_path.read_text())
        assert manifest["nodes"][0]["gpg_key_id"] == fingerprint

    def test_keyserver_returning_wrong_key_is_an_error(self, tmp_path: Path):
        pgpy = pytest.importorskip("pgpy")
        from pgpy.constants import (
            CompressionAlgorithm,
            HashAlgorithm,
            KeyFlags,
            PubKeyAlgorithm,
            SymmetricKeyAlgorithm,
        )

        key = pgpy.PGPKey.new(PubKeyAlgorithm.RSAEncryptOrSign, 2048)
        uid = pgpy.PGPUID.new("Join Test 2", email="join2@test.local")
        key.add_uid(
            uid,
            usage={KeyFlags.Sign},
            hashes=[HashAlgorithm.SHA256],
            ciphers=[SymmetricKeyAlgorithm.AES256],
            compression=[CompressionAlgorithm.ZLIB],
        )
        armored = str(key.pubkey)

        result = process_join.process(
            issue_body("0000000000000000"),
            tmp_path / "manifest.yaml",
            fetch_key=lambda _kid: armored,
        )
        assert not result.success
        assert "does not match" in result.error

    def test_invalid_body_reports_error(self, tmp_path: Path):
        result = process_join.process("free-form text", tmp_path / "m.yaml", fetch_key=no_key)
        assert not result.success
        assert "GPG Key ID" in result.error
