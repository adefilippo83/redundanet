"""Unit tests for the SFTP frontend config and helpers."""

from __future__ import annotations

import pytest

from redundanet.cli.storage import parse_pubkey
from redundanet.storage.client import _render_client_cfg


class TestClientSftpConfig:
    def _cfg(self, **kw):
        return _render_client_cfg(
            nickname="n1-client",
            web_port=4456,
            tub_port=3456,
            tub_location="AUTO",
            introducer_furl="pb://abc@tcp:10.100.0.1:3458/swiss",
            shares_needed=1,
            shares_happy=2,
            shares_total=2,
            **kw,
        )

    def test_sftp_absent_by_default(self):
        cfg = self._cfg()
        assert "[sftpd]" not in cfg

    def test_sftp_stanza_when_enabled(self):
        cfg = self._cfg(sftp_enabled=True, sftp_port=8022)
        assert "[sftpd]" in cfg
        assert "enabled = true" in cfg
        assert "port = tcp:8022" in cfg
        assert "accounts.file = private/sftp_accounts" in cfg
        assert "host_privkey_file = private/ssh_host_rsa_key" in cfg

    def test_sftp_custom_port(self):
        assert "port = tcp:2222" in self._cfg(sftp_enabled=True, sftp_port=2222)


class TestParsePubkey:
    def test_extracts_type_and_blob_dropping_comment(self):
        assert parse_pubkey("ssh-rsa AAAAB3Nza... alice@laptop") == "ssh-rsa AAAAB3Nza..."

    def test_ed25519_and_ecdsa_accepted(self):
        assert parse_pubkey("ssh-ed25519 AAAAC3 x").startswith("ssh-ed25519 ")
        assert parse_pubkey("ecdsa-sha2-nistp256 AAAAE2 y").startswith("ecdsa-sha2-nistp256 ")

    @pytest.mark.parametrize("bad", ["", "not-a-key", "hello world", "AAAAB3 onlyblob"])
    def test_rejects_non_pubkeys(self, bad):
        with pytest.raises(ValueError):
            parse_pubkey(bad)
