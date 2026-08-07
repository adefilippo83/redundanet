"""Unit tests for FURL parsing."""

import pytest

from redundanet.core.exceptions import StorageError
from redundanet.storage.furl import parse_furl, validate_furl


class TestParseFurl:
    def test_parses_components(self):
        furl = "pb://abc234def@tcp:10.100.0.1:3458/introducerswiss"
        parsed = parse_furl(furl)
        assert parsed == {
            "tubid": "abc234def",
            "location": "tcp:10.100.0.1:3458",
            "swissnum": "introducerswiss",
        }

    def test_multiple_connection_hints(self):
        furl = "pb://abc@10.100.0.1:3458,example.com:3458/swiss"
        assert parse_furl(furl)["location"] == "10.100.0.1:3458,example.com:3458"

    def test_whitespace_is_tolerated(self):
        assert validate_furl("  pb://abc@host:1/swiss\n")

    @pytest.mark.parametrize(
        "bad",
        [
            "",
            "http://abc@host:1/swiss",  # wrong scheme
            "pb://ABC@host:1/swiss",  # tubid must be lowercase base32
            "pb://abc@host:1/",  # missing swissnum
            "pb://abc/swiss",  # missing location
            "pb://abc@host:1/swiss/extra",  # trailing path
        ],
    )
    def test_invalid_furls(self, bad: str):
        assert not validate_furl(bad)
        with pytest.raises(StorageError):
            parse_furl(bad)
