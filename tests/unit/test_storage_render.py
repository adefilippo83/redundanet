"""Unit tests for the tahoe.cfg renderers (storage + client)."""

from pathlib import Path

from redundanet.storage.client import _render_client_cfg
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
