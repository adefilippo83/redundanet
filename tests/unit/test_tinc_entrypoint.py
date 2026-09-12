"""Unit tests for the tinc container entrypoint's pre-flight checks."""

from __future__ import annotations

import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import tinc as tinc_entrypoint  # noqa: E402


class TestGpgSecretError:
    """The mounted key must be diagnosed with actionable messages — a fresh
    node whose key was never exported gets a DIRECTORY at the mount point
    (Docker auto-creates missing bind sources), which used to surface as an
    IsADirectoryError traceback repeated until supervisord gave up."""

    def test_valid_key_file_passes(self, tmp_path: Path):
        key = tmp_path / "gpg_private_key"
        key.write_text("-----BEGIN PGP PRIVATE KEY BLOCK-----\n...")
        assert tinc_entrypoint.gpg_secret_error(key) is None

    def test_directory_diagnosed_with_fix_instructions(self, tmp_path: Path):
        mount = tmp_path / "gpg_private_key"
        mount.mkdir()
        message = tinc_entrypoint.gpg_secret_error(mount)
        assert message is not None
        assert "DIRECTORY" in message
        assert "rmdir" in message
        assert "export-secret-keys" in message

    def test_missing_file_diagnosed(self, tmp_path: Path):
        message = tinc_entrypoint.gpg_secret_error(tmp_path / "nope")
        assert message is not None
        assert "not found" in message
        assert "export-secret-keys" in message

    def test_empty_file_diagnosed(self, tmp_path: Path):
        key = tmp_path / "gpg_private_key"
        key.write_text("")
        message = tinc_entrypoint.gpg_secret_error(key)
        assert message is not None
        assert "EMPTY" in message


class FakeLogger:
    def __init__(self):
        self.warnings: list[dict] = []
        self.infos: list[dict] = []

    def warning(self, msg, **kw):
        self.warnings.append({"msg": msg, **kw})

    def info(self, msg, **kw):
        self.infos.append({"msg": msg, **kw})


class TestStorageLimits:
    def test_unset_means_unlimited_and_silent(self):
        log = FakeLogger()
        assert tinc_entrypoint.storage_limits({}, log) == (None, None)
        assert log.warnings == [] and log.infos == []

    def test_valid_rates_are_normalized_and_logged(self):
        log = FakeLogger()
        limits = tinc_entrypoint.storage_limits(
            {"REDUNDANET_STORAGE_RATE_IN": "10Mbit", "REDUNDANET_STORAGE_RATE_OUT": " 20mbit "}, log
        )
        assert limits == ("10mbit", "20mbit")
        assert log.infos[0]["inbound"] == "10mbit" and log.infos[0]["outbound"] == "20mbit"

    def test_invalid_value_is_ignored_with_a_warning(self):
        """A typo in .env must not take the VPN down."""
        log = FakeLogger()
        limits = tinc_entrypoint.storage_limits(
            {"REDUNDANET_STORAGE_RATE_IN": "20MB/s", "REDUNDANET_STORAGE_RATE_OUT": "5mbit"}, log
        )
        assert limits == (None, "5mbit")
        assert log.warnings[0]["variable"] == "REDUNDANET_STORAGE_RATE_IN"
        assert log.infos[0]["inbound"] == "unlimited"
