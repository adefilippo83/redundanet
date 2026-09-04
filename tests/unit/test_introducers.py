"""Unit tests for multi-introducer support (redundanet.storage.introducers)."""

from __future__ import annotations

from pathlib import Path

import yaml

from redundanet.storage.introducers import (
    introducer_furls_from_manifest,
    petname,
    render_introducers_yaml,
    write_introducers_yaml,
)

FURL_A = "pb://aaaa@tcp:10.100.0.1:3458/swissa"
FURL_B = "pb://bbbb@tcp:10.100.0.2:3458/swissb"
FURL_C = "pb://cccc@tcp:10.100.0.3:3458/swissc"


def manifest(top: str | None = FURL_A, nodes: list[dict] | None = None) -> dict:
    data: dict = {"nodes": nodes or []}
    if top is not None:
        data["introducer_furl"] = top
    return data


class TestFromManifest:
    def test_primary_first_then_introducer_nodes_in_order(self):
        data = manifest(
            nodes=[
                {"name": "hub", "roles": ["tahoe_introducer"], "introducer_furl": FURL_B},
                {
                    "name": "vps",
                    "roles": ["tinc_vpn", "tahoe_introducer"],
                    "introducer_furl": FURL_C,
                },
            ]
        )
        assert introducer_furls_from_manifest(data) == [FURL_A, FURL_B, FURL_C]

    def test_dedupes_and_ignores_furls_on_non_introducer_nodes(self):
        data = manifest(
            nodes=[
                # Same FURL as the top-level primary: counted once.
                {"name": "hub", "roles": ["tahoe_introducer"], "introducer_furl": FURL_A},
                # A storage-only node carrying a FURL is a mistake: ignored.
                {"name": "n1", "roles": ["tahoe_storage"], "introducer_furl": FURL_B},
                # Whitespace is stripped.
                {"name": "vps", "roles": ["tahoe_introducer"], "introducer_furl": f"  {FURL_C} "},
            ]
        )
        assert introducer_furls_from_manifest(data) == [FURL_A, FURL_C]

    def test_nothing_declared_is_empty(self):
        assert introducer_furls_from_manifest(manifest(top=None)) == []
        assert introducer_furls_from_manifest({}) == []


class TestRender:
    def test_petnames_are_stable_and_never_default(self):
        # Derived from the tub id, so reordering the manifest does not rename
        # the cache files Tahoe keeps per introducer.
        assert petname(FURL_B, 1) == "intro-bbbb"
        assert petname(FURL_B, 7) == "intro-bbbb"
        assert petname("garbage", 3) == "intro3"
        assert "default" not in (petname(FURL_A, 1), petname("garbage", 1))

    def test_yaml_has_the_shape_tahoe_reads(self):
        data = yaml.safe_load(render_introducers_yaml([FURL_B, FURL_C, FURL_B]))
        assert set(data) == {"introducers"}
        assert data["introducers"] == {
            "intro-bbbb": {"furl": FURL_B},
            "intro-cccc": {"furl": FURL_C},
        }


class TestWrite:
    def test_writes_then_removes_when_no_extras(self, tmp_path: Path):
        private = tmp_path / "private"
        path = write_introducers_yaml(private, [FURL_B])
        assert path == private / "introducers.yaml"
        assert path.exists()
        assert oct(path.stat().st_mode & 0o777) == "0o600"

        # A retired second introducer must not linger in the node's config.
        assert write_introducers_yaml(private, []) is None
        assert not path.exists()

    def test_no_extras_and_no_file_is_a_noop(self, tmp_path: Path):
        assert write_introducers_yaml(tmp_path / "private", []) is None
        assert not (tmp_path / "private").exists()
