"""Unit tests for docker/entrypoints/tahoe_storage.py helpers."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from redundanet.storage.disk import MARKER_FILE, RECORD_FILE, establish, read_token

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "entrypoints"))

import tahoe_storage  # noqa: E402


class TestExistingShareCount:
    def test_counts_storage_index_dirs_not_incoming(self, tmp_path: Path):
        shares = tmp_path / "shares"
        for si in ("aaindex1", "aaindex2", "bbindex3"):
            (shares / si[:2] / si).mkdir(parents=True)
        (shares / "incoming" / "cc" / "ccindex9").mkdir(parents=True)
        assert tahoe_storage.existing_share_count(shares) == 3

    def test_missing_or_empty_disk_is_zero(self, tmp_path: Path):
        assert tahoe_storage.existing_share_count(tmp_path / "none") == 0
        (tmp_path / "shares").mkdir()
        assert tahoe_storage.existing_share_count(tmp_path / "shares") == 0

    def test_stops_at_the_limit(self, tmp_path: Path):
        shares = tmp_path / "shares"
        for i in range(30):
            (shares / "aa" / f"aaindex{i:02}").mkdir(parents=True)
        assert tahoe_storage.existing_share_count(shares, limit=10) == 10


class TestStorageDisk:
    def test_first_start_marks_the_disk_after_node_creation(self, tmp_path: Path):
        data, node = tmp_path / "data", tmp_path / "node"
        data.mkdir()
        node.mkdir()
        check = tahoe_storage.check_storage_disk(data, node)
        assert check.status == "first"
        assert not (node / RECORD_FILE).exists()  # tahoe create-node wants it empty
        tahoe_storage.remember_storage_disk(check, data, node)
        token = read_token(data / MARKER_FILE)
        assert token and read_token(node / RECORD_FILE) == token
        assert tahoe_storage.check_storage_disk(data, node).status == "ok"

    def test_refuses_to_start_on_another_filesystem(self, tmp_path: Path):
        data, node = tmp_path / "data", tmp_path / "node"
        establish(data / MARKER_FILE, node / RECORD_FILE)
        (data / MARKER_FILE).unlink()  # the disk is not mounted: an empty directory
        with pytest.raises(SystemExit) as exit_info:
            tahoe_storage.check_storage_disk(data, node)
        assert exit_info.value.code == 1

    def test_remember_does_nothing_after_the_first_start(self, tmp_path: Path):
        data, node = tmp_path / "data", tmp_path / "node"
        token = establish(data / MARKER_FILE, node / RECORD_FILE)
        check = tahoe_storage.check_storage_disk(data, node)
        tahoe_storage.remember_storage_disk(check, data, node)
        assert read_token(data / MARKER_FILE) == token
