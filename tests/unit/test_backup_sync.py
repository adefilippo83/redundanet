"""Unit tests for the backup-sync loop (docker/entrypoints/backup_sync.py)."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import backup_sync  # noqa: E402


def completed(returncode: int = 0, stdout: str = "", stderr: str = ""):
    return subprocess.CompletedProcess(
        args=["tahoe"], returncode=returncode, stdout=stdout, stderr=stderr
    )


class FakeRun:
    """Scripted tahoe-CLI stand-in recording every invocation."""

    def __init__(self, responses: dict[str, subprocess.CompletedProcess]):
        self.responses = responses
        self.calls: list[list[str]] = []

    def __call__(self, args, timeout=3600):
        self.calls.append(list(args))
        return self.responses.get(args[0], completed())


class TestParseConfig:
    def test_defaults_disabled(self):
        config = backup_sync.parse_config({})
        assert config.enabled is False
        assert config.interval == 900
        assert config.sync_dir == "/data/sync"
        assert config.alias == "backups"

    def test_enabled_with_overrides(self):
        config = backup_sync.parse_config(
            {
                "REDUNDANET_SYNC_ENABLED": "TRUE",
                "REDUNDANET_SYNC_INTERVAL": "300",
                "REDUNDANET_SYNC_DIR": "/srv/share",
                "REDUNDANET_SYNC_ALIAS": "nas",
            }
        )
        assert config.enabled is True
        assert config.interval == 300
        assert config.sync_dir == "/srv/share"
        assert config.alias == "nas"


class TestEnsureAlias:
    def test_existing_alias_not_recreated(self):
        run = FakeRun(
            {"list-aliases": completed(stdout="backups: URI:DIR2:abc\nhome: URI:DIR2:x\n")}
        )
        assert backup_sync.ensure_alias("backups", run=run) is True
        assert [c[0] for c in run.calls] == ["list-aliases"]

    def test_missing_alias_created(self):
        run = FakeRun({"list-aliases": completed(stdout="home: URI:DIR2:x\n")})
        assert backup_sync.ensure_alias("backups", run=run) is True
        assert [c[0] for c in run.calls] == ["list-aliases", "create-alias"]
        assert run.calls[1] == ["create-alias", "backups"]

    def test_client_not_ready_returns_false(self):
        run = FakeRun({"list-aliases": completed(returncode=1, stderr="no node")})
        assert backup_sync.ensure_alias("backups", run=run) is False

    def test_create_failure_returns_false(self):
        run = FakeRun(
            {
                "list-aliases": completed(stdout=""),
                "create-alias": completed(returncode=1, stderr="boom"),
            }
        )
        assert backup_sync.ensure_alias("backups", run=run) is False


class TestRunBackup:
    def config(self, sync_dir: str) -> backup_sync.SyncConfig:
        return backup_sync.SyncConfig(
            enabled=True, interval=900, sync_dir=sync_dir, alias="backups"
        )

    def test_backs_up_nonempty_dir(self, tmp_path: Path):
        (tmp_path / "file.txt").write_text("data")
        run = FakeRun({"backup": completed(stdout=" reused 2 files\n backed up 1 files\n")})
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is True
        assert run.calls == [["backup", str(tmp_path), "backups:"]]

    def test_empty_dir_skipped_without_error(self, tmp_path: Path):
        run = FakeRun({})
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is True
        assert run.calls == []  # nothing to do -> tahoe never invoked

    def test_missing_dir_skipped(self, tmp_path: Path):
        run = FakeRun({})
        assert backup_sync.run_backup(self.config(str(tmp_path / "nope")), run=run) is True
        assert run.calls == []

    def test_backup_failure_reported(self, tmp_path: Path):
        (tmp_path / "file.txt").write_text("data")
        run = FakeRun({"backup": completed(returncode=1, stderr="grid unreachable")})
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is False
