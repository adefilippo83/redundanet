"""Unit tests for redundanet.storage.disk (the shares disk's marker and record)."""

from __future__ import annotations

from pathlib import Path

import pytest

from redundanet.storage.disk import (
    MARKER_FILE,
    RECORD_FILE,
    DiskCheck,
    establish,
    explain,
    read_token,
    verify,
)


@pytest.fixture
def paths(tmp_path: Path) -> tuple[Path, Path]:
    disk = tmp_path / "data" / "storage"
    node = tmp_path / "node"
    disk.mkdir(parents=True)
    node.mkdir()
    return disk / MARKER_FILE, node / RECORD_FILE


class TestVerify:
    def test_first_start_has_no_record(self, paths):
        marker, record = paths
        check = verify(marker, record)
        assert check == DiskCheck("first")
        assert check.refused is False

    def test_the_same_disk_is_ok(self, paths):
        marker, record = paths
        token = establish(marker, record)
        assert verify(marker, record) == DiskCheck("ok", expected=token, found=token)

    def test_a_missing_marker_is_a_mismatch(self, paths):
        """The disk is not mounted: the bind shows the empty directory under
        the mountpoint, which has no marker."""
        marker, record = paths
        token = establish(marker, record)
        marker.unlink()
        check = verify(marker, record)
        assert check == DiskCheck("mismatch", expected=token, found=None)
        assert check.refused

    def test_another_disk_is_a_mismatch(self, paths):
        marker, record = paths
        token = establish(marker, record)
        marker.write_text("someone-elses-token\n")
        assert verify(marker, record) == DiskCheck(
            "mismatch", expected=token, found="someone-elses-token"
        )

    def test_an_unreadable_marker_is_an_error(self, paths):
        marker, record = paths
        token = establish(marker, record)
        marker.unlink()
        marker.mkdir()  # reading a directory raises, and not FileNotFoundError
        check = verify(marker, record)
        assert check.status == "error" and check.refused
        assert check.expected == token
        assert str(marker) in check.error

    def test_an_empty_record_counts_as_no_record(self, paths):
        marker, record = paths
        record.write_text("\n")
        assert verify(marker, record).status == "first"


class TestEstablish:
    def test_writes_one_token_to_both_files(self, paths):
        marker, record = paths
        token = establish(marker, record)
        assert len(token) == 32
        int(token, 16)  # hex
        assert read_token(marker) == token == read_token(record)

    def test_adopts_a_marker_already_on_the_disk(self, paths):
        """The node directory was recreated around an intact disk (or the disk
        came from another node): its marker becomes this node's record."""
        marker, record = paths
        marker.write_text("kept\n")
        assert establish(marker, record) == "kept"
        assert read_token(record) == "kept"

    def test_creates_missing_directories(self, tmp_path: Path):
        marker = tmp_path / "disk" / MARKER_FILE
        record = tmp_path / "node" / RECORD_FILE
        establish(marker, record)
        assert marker.is_file() and record.is_file()


class TestExplain:
    def test_a_missing_marker_names_the_files_and_the_way_out(self, paths):
        marker, record = paths
        text = explain(DiskCheck("mismatch", expected="abc", found=None), marker, record)
        assert f"The shares directory {marker.parent} is not the disk" in text
        assert f"the marker {marker} is missing" in text
        assert f"the record {record} expects abc" in text
        assert "findmnt" in text
        assert "redundanet storage stop && redundanet storage start" in text
        assert f"docker exec redundanet-tahoe-storage rm {record}" in text

    def test_another_token_is_shown(self, paths):
        marker, record = paths
        text = explain(DiskCheck("mismatch", expected="abc", found="xyz"), marker, record)
        assert "holds token xyz" in text and "expects abc" in text

    def test_a_read_error_is_quoted(self, paths):
        marker, record = paths
        text = explain(DiskCheck("error", error="cannot read it: EIO"), marker, record)
        assert "the disk cannot be read (cannot read it: EIO)" in text
        assert "expects" not in text
