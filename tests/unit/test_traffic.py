"""Unit tests for redundanet.vpn.traffic (storage traffic limits via tc)."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from redundanet.vpn.tinc import TincConfig, TincManager
from redundanet.vpn.traffic import (
    MIN_BURST_BYTES,
    STORAGE_TUB_PORT,
    burst_bytes,
    parse_rate,
    render_tc_rules,
    render_tc_teardown,
)


class TestParseRate:
    @pytest.mark.parametrize(
        ("raw", "expected"),
        [
            ("20mbit", "20mbit"),
            (" 500KBIT ", "500kbit"),
            ("1 gbit", "1gbit"),
            ("", None),
            (None, None),
            ("0mbit", None),  # zero would block the port entirely
            ("20", None),
            ("20mbps", None),
            ("fast", None),
            ("20MB/s", None),
        ],
    )
    def test_tc_units_only(self, raw, expected):
        assert parse_rate(raw) == expected


class TestBurst:
    def test_scales_with_rate_with_a_floor(self):
        assert burst_bytes("500kbit") == MIN_BURST_BYTES
        assert burst_bytes("100mbit") == 312_500  # 25 ms of 12.5 MB/s
        assert burst_bytes("1gbit") == 3_125_000


class TestRenderRules:
    def test_nothing_when_unlimited(self):
        assert render_tc_rules(STORAGE_TUB_PORT, None, None) == ""

    def test_inbound_polices_the_storage_port_only(self):
        rules = render_tc_rules(3457, "10mbit", None)
        assert "ingress" in rules and "police rate 10mbit" in rules
        assert "match ip dport 3457 0xffff" in rules
        assert "htb" not in rules  # no outbound shaper requested
        assert "STORAGE_RATE_IN=10mbit could not be applied" in rules  # the failure path logs

    def test_outbound_shapes_the_storage_port_in_its_own_class(self):
        rules = render_tc_rules(3457, None, "20mbit")
        assert "htb default 20" in rules  # everything else: the unlimited class
        assert "classid 1:10 htb rate 20mbit ceil 20mbit" in rules
        assert "match ip sport 3457 0xffff flowid 1:10" in rules
        assert "ingress" not in rules.replace("qdisc del dev $INTERFACE ingress", "")

    def test_both_start_by_clearing_old_rules(self):
        rules = render_tc_rules(3457, "10mbit", "20mbit")
        assert rules.index("qdisc del dev $INTERFACE root") < rules.index("qdisc add")
        assert "qdisc del dev $INTERFACE ingress" in render_tc_teardown()


class TestTincScripts:
    def config(self, tmp_path: Path, **kw) -> TincConfig:
        return TincConfig(
            config_dir=tmp_path, network_name="testnet", node_name="n", vpn_ip="10.100.0.5", **kw
        )

    def test_unlimited_node_scripts_are_unchanged(self, tmp_path: Path):
        tinc = TincManager(self.config(tmp_path))
        tinc._write_tinc_up()
        tinc._write_tinc_down()
        assert "tc " not in (tinc.config.network_dir / "tinc-up").read_text()
        assert "tc " not in (tinc.config.network_dir / "tinc-down").read_text()

    def test_limited_node_gets_rules_and_teardown(self, tmp_path: Path):
        tinc = TincManager(
            self.config(tmp_path, storage_rate_in="10mbit", storage_rate_out="20mbit")
        )
        tinc._write_tinc_up()
        tinc._write_tinc_down()
        up = (tinc.config.network_dir / "tinc-up").read_text()
        down = (tinc.config.network_dir / "tinc-down").read_text()
        assert up.startswith("#!/bin/bash")
        assert "ip route add 10.100.0.0/16 dev $INTERFACE" in up
        assert f"dport {STORAGE_TUB_PORT} 0xffff police rate 10mbit" in up
        assert f"sport {STORAGE_TUB_PORT} 0xffff flowid 1:10" in up
        assert down.index("qdisc del") < down.index("ip route del")

    @pytest.mark.skipif(shutil.which("bash") is None, reason="bash needed to syntax-check")
    def test_generated_scripts_are_valid_bash(self, tmp_path: Path):
        tinc = TincManager(
            self.config(tmp_path, storage_rate_in="10mbit", storage_rate_out="20mbit")
        )
        tinc._write_tinc_up()
        tinc._write_tinc_down()
        for name in ("tinc-up", "tinc-down"):
            result = subprocess.run(
                ["bash", "-n", str(tinc.config.network_dir / name)], capture_output=True, text=True
            )
            assert result.returncode == 0, result.stderr
