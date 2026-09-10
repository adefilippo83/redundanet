"""Unit tests for docker/entrypoints/tahoe_storage.py helpers."""

from __future__ import annotations

import sys
from pathlib import Path

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
