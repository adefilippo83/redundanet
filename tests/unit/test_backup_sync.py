"""Unit tests for the backup-sync loop (docker/entrypoints/backup_sync.py)."""

from __future__ import annotations

import json
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
        self.timeouts: list[int] = []

    def __call__(self, args, timeout=3600):
        self.calls.append(list(args))
        self.timeouts.append(timeout)
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
                "REDUNDANET_SYNC_TIMEOUT": "7200",
            }
        )
        assert config.enabled is True
        assert config.interval == 300
        assert config.sync_dir == "/srv/share"
        assert config.alias == "nas"
        assert config.timeout == 7200

    def test_bad_integers_fall_back_to_defaults(self):
        """A typo in .env must degrade gracefully, not crash-loop the program."""
        config = backup_sync.parse_config(
            {"REDUNDANET_SYNC_INTERVAL": "abc", "REDUNDANET_SYNC_TIMEOUT": ""}
        )
        assert config.interval == 900
        assert config.timeout == 21600

    def test_exclude_and_reencode_defaults(self):
        config = backup_sync.parse_config({})
        assert config.exclude == []
        assert config.reencode is True

    def test_exclude_patterns_split_and_trimmed(self):
        config = backup_sync.parse_config(
            {
                "REDUNDANET_SYNC_EXCLUDE": " .DS_Store, *.tmp ,, photos-link ",
                "REDUNDANET_SYNC_REENCODE": "false",
            }
        )
        assert config.exclude == [".DS_Store", "*.tmp", "photos-link"]
        assert config.reencode is False


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
            enabled=True, interval=900, sync_dir=sync_dir, alias="backups", timeout=1234
        )

    def test_backs_up_nonempty_dir(self, tmp_path: Path):
        (tmp_path / "file.txt").write_text("data")
        run = FakeRun({"backup": completed(stdout=" reused 2 files\n backed up 1 files\n")})
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is True
        assert run.calls == [["backup", str(tmp_path), "backups:"]]
        # The configured ceiling must reach the subprocess (a big first sync
        # can far outlast the old 1h default).
        assert run.timeouts == [1234]

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

    def test_exclude_patterns_reach_tahoe(self, tmp_path: Path):
        (tmp_path / "file.txt").write_text("data")
        config = self.config(str(tmp_path))
        config.exclude = [".DS_Store", "*.tmp"]
        run = FakeRun({"backup": completed(stdout=" done\n")})
        assert backup_sync.run_backup(config, run=run) is True
        assert run.calls == [
            ["backup", "--exclude=.DS_Store", "--exclude=*.tmp", str(tmp_path), "backups:"]
        ]

    def test_skipped_entries_are_a_success_and_named(self, tmp_path: Path, capsys):
        """tahoe backup exits 2 after making the snapshot when it skipped
        something (symlinks, special files). That is not a failed backup."""
        (tmp_path / "file.txt").write_text("data")
        run = FakeRun(
            {
                "backup": completed(
                    returncode=2,
                    stdout=" 1 files uploaded (3 reused), 2 files skipped, 1 directories created (0 reused), 0 directories skipped\n",
                    stderr="WARNING: cannot backup symlink '/data/sync/photos'\nWARNING: cannot backup special '/data/sync/pipe'\n",
                )
            }
        )
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is True
        out = capsys.readouterr().out
        assert "backup ok" in out and "2 skipped" in out and "FAILED" not in out
        assert "skipped: cannot backup symlink '/data/sync/photos'" in out
        assert "skipped: cannot backup special '/data/sync/pipe'" in out

    def test_skipped_list_is_capped(self, tmp_path: Path, capsys):
        (tmp_path / "file.txt").write_text("data")
        warnings = "".join(f"WARNING: cannot backup symlink 'l{i}'\n" for i in range(25))
        run = FakeRun({"backup": completed(returncode=2, stdout=" done\n", stderr=warnings)})
        assert backup_sync.run_backup(self.config(str(tmp_path)), run=run) is True
        out = capsys.readouterr().out
        assert out.count("  skipped:") == backup_sync.MAX_SKIPPED_LOGGED
        assert "and 5 more" in out


class TestForgetStaleEncoding:
    def config(self, reencode: bool = True) -> backup_sync.SyncConfig:
        return backup_sync.SyncConfig(
            enabled=True, interval=900, sync_dir="/x", alias="backups", timeout=1, reencode=reencode
        )

    def tahoe_cfg(self, tmp_path: Path, needed: int, total: int) -> Path:
        cfg = tmp_path / "tahoe.cfg"
        cfg.write_text(
            f"[node]\nnickname = n\n\n[client]\nintroducer.furl = pb://x\n"
            f"shares.needed = {needed}\nshares.happy = {total}\nshares.total = {total}\n"
        )
        return cfg

    def backupdb(self, tmp_path: Path, caps: list[str]) -> Path:
        import sqlite3

        db = tmp_path / "backupdb.sqlite"
        conn = sqlite3.connect(db)
        conn.executescript(
            "CREATE TABLE local_files (path PRIMARY KEY, size, mtime, ctime, fileid);"
            "CREATE TABLE caps (fileid INTEGER PRIMARY KEY, filecap UNIQUE);"
            "CREATE TABLE last_upload (fileid INTEGER PRIMARY KEY, last_uploaded, last_checked);"
            "CREATE TABLE directories (dirhash PRIMARY KEY, dircap, last_uploaded, last_checked);"
        )
        for i, cap in enumerate(caps, start=1):
            conn.execute("INSERT INTO caps VALUES (?, ?)", (i, cap.encode()))
            conn.execute("INSERT INTO local_files VALUES (?, 1, 1, 1, ?)", (f"/f{i}", i))
        conn.commit()
        conn.close()
        return db

    def remaining(self, db: Path) -> int:
        import sqlite3

        conn = sqlite3.connect(db)
        try:
            return conn.execute("SELECT count(*) FROM caps").fetchone()[0]
        finally:
            conn.close()

    def test_rows_at_the_old_encoding_are_forgotten(self, tmp_path: Path, capsys):
        db = self.backupdb(tmp_path, ["URI:CHK:a:h:1:2:9", "URI:CHK:b:h:2:4:9"])
        backup_sync.forget_stale_encoding(
            self.config(), tahoe_cfg=self.tahoe_cfg(tmp_path, 2, 4), db_path=db
        )
        assert self.remaining(db) == 1
        assert "forgot 1 files" in capsys.readouterr().out

    def test_compares_with_the_running_node_not_the_manifest(self, tmp_path: Path, capsys):
        """A node whose tahoe.cfg still says 1-of-2 (not yet recreated) must
        keep its rows: re-uploading now would produce 1-of-2 caps again."""
        db = self.backupdb(tmp_path, ["URI:CHK:a:h:1:2:9"])
        backup_sync.forget_stale_encoding(
            self.config(), tahoe_cfg=self.tahoe_cfg(tmp_path, 1, 2), db_path=db
        )
        assert self.remaining(db) == 1
        assert "forgot" not in capsys.readouterr().out

    def test_disabled_leaves_the_database_alone(self, tmp_path: Path):
        db = self.backupdb(tmp_path, ["URI:CHK:a:h:1:2:9"])
        backup_sync.forget_stale_encoding(
            self.config(reencode=False), tahoe_cfg=self.tahoe_cfg(tmp_path, 2, 4), db_path=db
        )
        assert self.remaining(db) == 1

    def test_unreadable_tahoe_cfg_keeps_the_database(self, tmp_path: Path, capsys):
        db = self.backupdb(tmp_path, ["URI:CHK:a:h:1:2:9"])
        backup_sync.forget_stale_encoding(
            self.config(), tahoe_cfg=tmp_path / "missing.cfg", db_path=db
        )
        assert self.remaining(db) == 1
        assert "keeping the backupdb" in capsys.readouterr().out

    def test_damaged_database_does_not_stop_the_run(self, tmp_path: Path, capsys):
        db = tmp_path / "backupdb.sqlite"
        db.write_bytes(b"not a database at all")
        backup_sync.forget_stale_encoding(
            self.config(), tahoe_cfg=self.tahoe_cfg(tmp_path, 2, 4), db_path=db
        )
        assert "cannot prune" in capsys.readouterr().out

    def test_no_database_yet_is_quiet(self, tmp_path: Path, capsys):
        backup_sync.forget_stale_encoding(
            self.config(), tahoe_cfg=self.tahoe_cfg(tmp_path, 2, 4), db_path=tmp_path / "none"
        )
        assert capsys.readouterr().out == ""


class TestQuotaBlocks:
    def test_no_report_does_not_block(self, tmp_path: Path):
        assert backup_sync.quota_blocks(tmp_path / "none.json") is False

    def test_over_allocation_blocks_when_enforced(self, tmp_path: Path, capsys):
        report = tmp_path / "u.json"
        report.write_text(
            json.dumps(
                {"member": "ale", "enforce": True, "used_bytes": 200, "allocation_bytes": 100}
            )
        )
        assert backup_sync.quota_blocks(report) is True
        assert "over allocation" in capsys.readouterr().out

    def test_not_enforced_never_blocks(self, tmp_path: Path):
        report = tmp_path / "u.json"
        report.write_text(
            json.dumps({"enforce": False, "used_bytes": 200, "allocation_bytes": 100})
        )
        assert backup_sync.quota_blocks(report) is False
