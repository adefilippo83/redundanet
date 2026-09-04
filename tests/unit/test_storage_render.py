"""Unit tests for the tahoe.cfg renderers (storage + client)."""

from pathlib import Path

import yaml

from redundanet.storage.client import TahoeClient, TahoeClientConfig, _render_client_cfg
from redundanet.storage.storage import TahoeStorage, TahoeStorageConfig, _render_storage_cfg


class TestStorageCfg:
    def test_includes_lease_gc_settings(self):
        cfg = _render_storage_cfg(
            nickname="n1-storage",
            web_port=4457,
            tub_port=3457,
            tub_location="tcp:10.100.0.2:3457",
            introducer_furl="pb://abc@tcp:10.100.0.1:3458/swiss",
            reserved_space="1G",
            storage_dir="/data/storage",
            shares_needed=2,
            shares_happy=3,
            shares_total=3,
        )
        # GC on by default: shares whose lease lapses get collected.
        assert "expire.enabled = true" in cfg
        assert "expire.mode = age" in cfg
        assert "expire.override_lease_duration = 90 days" in cfg
        assert "storage_dir = /data/storage" in cfg
        assert "shares.happy = 3" in cfg

    def test_gc_can_be_disabled_and_duration_tuned(self):
        cfg = _render_storage_cfg(
            nickname="n1-storage",
            web_port=4457,
            tub_port=3457,
            tub_location="AUTO",
            introducer_furl="pb://abc@tcp:10.100.0.1:3458/swiss",
            reserved_space="1G",
            storage_dir=None,
            shares_needed=1,
            shares_happy=1,
            shares_total=2,
            expire_enabled=False,
            lease_duration="30 days",
        )
        assert "expire.enabled = false" in cfg
        assert "expire.override_lease_duration = 30 days" in cfg
        assert "storage_dir" not in cfg

    def test_write_config_carries_lease_settings(self, tmp_path: Path):
        config = TahoeStorageConfig(
            nickname="n1-storage",
            node_dir=tmp_path,
            introducer_furl="pb://abc@tcp:10.100.0.1:3458/swiss",
            lease_duration="45 days",
        )
        TahoeStorage(config)._write_config()
        content = (tmp_path / "tahoe.cfg").read_text()
        assert "expire.override_lease_duration = 45 days" in content


class TestClientCfg:
    def test_renders_client_config(self):
        cfg = _render_client_cfg(
            nickname="n1-client",
            web_port=4456,
            tub_port=3456,
            tub_location="tcp:10.100.0.2:3456",
            introducer_furl="pb://abc@tcp:10.100.0.1:3458/swiss",
            shares_needed=1,
            shares_happy=1,
            shares_total=2,
        )
        assert "nickname = n1-client" in cfg
        assert "introducer.furl = pb://abc@tcp:10.100.0.1:3458/swiss" in cfg
        assert "[storage]\nenabled = false" in cfg


FURL_A = "pb://aaaa@tcp:10.100.0.1:3458/swissa"
FURL_B = "pb://bbbb@tcp:10.100.0.2:3458/swissb"


def _extra_furls(node_dir: Path) -> list[str]:
    data = yaml.safe_load((node_dir / "private" / "introducers.yaml").read_text())
    return [entry["furl"] for entry in data["introducers"].values()]


class TestMultipleIntroducers:
    def test_storage_writes_extras_to_introducers_yaml(self, tmp_path: Path):
        config = TahoeStorageConfig(
            nickname="n1-storage",
            node_dir=tmp_path,
            introducer_furl=FURL_A,
            extra_introducer_furls=[FURL_B],
        )
        TahoeStorage(config)._write_config()
        # The primary stays in tahoe.cfg (Tahoe's reserved "default" petname);
        # the extra goes to private/introducers.yaml under its own petname.
        assert f"introducer.furl = {FURL_A}" in (tmp_path / "tahoe.cfg").read_text()
        assert _extra_furls(tmp_path) == [FURL_B]
        data = yaml.safe_load((tmp_path / "private" / "introducers.yaml").read_text())
        assert "default" not in data["introducers"]

    def test_update_introducers_adds_then_removes_extras(self, tmp_path: Path):
        storage = TahoeStorage(
            TahoeStorageConfig(nickname="n1-storage", node_dir=tmp_path, introducer_furl=FURL_A)
        )
        storage.update_introducers([FURL_A, FURL_B])
        assert _extra_furls(tmp_path) == [FURL_B]
        # A retired second introducer disappears from the node's config.
        storage.update_introducers([FURL_A])
        assert not (tmp_path / "private" / "introducers.yaml").exists()
        assert f"introducer.furl = {FURL_A}" in (tmp_path / "tahoe.cfg").read_text()

    def test_update_introducer_furl_keeps_extras(self, tmp_path: Path):
        storage = TahoeStorage(
            TahoeStorageConfig(
                nickname="n1-storage",
                node_dir=tmp_path,
                introducer_furl=FURL_A,
                extra_introducer_furls=[FURL_B],
            )
        )
        storage.update_introducer_furl("pb://cccc@tcp:10.100.0.3:3458/swissc")
        assert "introducer.furl = pb://cccc@" in (tmp_path / "tahoe.cfg").read_text()
        assert _extra_furls(tmp_path) == [FURL_B]

    def test_client_writes_extras_too(self, tmp_path: Path):
        config = TahoeClientConfig(
            nickname="n1-client",
            node_dir=tmp_path,
            introducer_furl=FURL_A,
            extra_introducer_furls=[FURL_B],
        )
        TahoeClient(config)._write_config()
        assert f"introducer.furl = {FURL_A}" in (tmp_path / "tahoe.cfg").read_text()
        assert _extra_furls(tmp_path) == [FURL_B]
