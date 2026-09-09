"""Unit tests for the contribution-based allocation model (core/quota.py)."""

from __future__ import annotations

from redundanet.core.quota import (
    allocation_bytes,
    compute_quotas,
    footprint_bytes,
    format_size,
    node_allocation,
    parse_size,
    quota_settings,
    resolve_encoding,
)

GB = 10**9


def manifest(nodes: list[dict], needed: int = 1, total: int = 2, quota: dict | None = None) -> dict:
    network: dict = {
        "name": "t",
        "tahoe": {"shares_needed": needed, "shares_happy": total, "shares_total": total},
    }
    if quota is not None:
        network["quota"] = quota
    return {"network": network, "nodes": nodes}


def storage(name: str, contribution: str, member: str | None = None) -> dict:
    node: dict = {
        "name": name,
        "roles": ["tinc_vpn", "tahoe_storage"],
        "storage_contribution": contribution,
    }
    if member:
        node["member"] = member
    return node


class TestSizes:
    def test_parse_size_units(self):
        assert parse_size("500GB") == 500 * GB
        assert parse_size("1TB") == 10**12
        assert parse_size("1G") == 10**9
        assert parse_size("250 GiB") == 250 * 2**30
        assert parse_size("garbage") is None
        assert parse_size(None) is None
        assert parse_size("") is None

    def test_format_size(self):
        assert format_size(250 * GB) == "250.0 GB"
        assert format_size(1200 * GB) == "1.20 TB"
        assert format_size(800 * 10**6) == "800 MB"


class TestAllocation:
    def test_examples_from_the_design(self):
        # 500 GB contributed: 1-of-2 and 2-of-4 both expand 2x -> 250 GB; 3-of-10 -> 150 GB.
        assert allocation_bytes(500 * GB, 1, 2, 0.0) == 250 * GB
        assert allocation_bytes(500 * GB, 2, 4, 0.0) == 250 * GB
        assert allocation_bytes(500 * GB, 3, 10, 0.0) == 150 * GB

    def test_reserve_reduces_allocation(self):
        assert allocation_bytes(500 * GB, 1, 2, 0.15) == int(250 * GB * 0.85)

    def test_footprint_is_size_times_expansion(self):
        assert footprint_bytes(100, 1, 2) == 200
        assert footprint_bytes(100, 3, 10) == 334  # ceil(333.3)
        assert footprint_bytes(100, 2, 4) == 200


class TestSettings:
    def test_defaults_when_absent(self):
        settings = quota_settings({"network": {}})
        assert settings.reserve == 0.15
        assert settings.enforce is False

    def test_manifest_values_and_clamping(self):
        settings = quota_settings(manifest([], quota={"reserve": 0.2, "enforce": True}))
        assert settings.reserve == 0.2
        assert settings.enforce is True
        assert quota_settings(manifest([], quota={"reserve": 5})).reserve == 0.9
        assert quota_settings(manifest([], quota={"reserve": "x"})).reserve == 0.15


class TestComputeQuotas:
    def test_groups_nodes_by_member_and_sums_contributions(self):
        m = manifest(
            [
                storage("n1", "500GB", "ale"),
                storage("n2", "500GB", "ale"),
                storage("n3", "1TB", "bob"),
            ],
            quota={"reserve": 0},
        )
        quotas = {q.member: q for q in compute_quotas(m)}
        assert quotas["ale"].nodes == ["n1", "n2"]
        assert quotas["ale"].contributed_bytes == 1000 * GB
        assert quotas["ale"].allocation_bytes == 500 * GB  # 1-of-2
        assert quotas["bob"].allocation_bytes == 500 * GB
        assert quotas["ale"].used_bytes is None
        assert quotas["ale"].usage_source == "none"

    def test_node_without_member_is_its_own_member(self):
        assert compute_quotas(manifest([storage("n1", "500GB")]))[0].member == "n1"

    def test_usage_summed_over_member_nodes_and_over_flag(self):
        m = manifest(
            [storage("n1", "500GB", "ale"), storage("n2", "500GB", "ale")], quota={"reserve": 0}
        )
        usage = {
            "n1": {"used_bytes": 300 * GB, "files": 3, "source": "live"},
            "n2": {"used_bytes": 300 * GB, "files": 2, "source": "cached"},
        }
        quota = compute_quotas(m, usage)[0]
        assert quota.used_bytes == 600 * GB
        assert quota.files == 5
        assert quota.over is True
        assert quota.percent == 120.0
        assert quota.usage_source == "cached"  # any stale report marks the total as such

    def test_overstated_contribution_counts_the_real_disk(self):
        m = manifest([storage("n1", "1TB", "ale")], quota={"reserve": 0})
        quota = compute_quotas(m, disk_totals={"n1": 400 * GB})[0]
        assert quota.overstated == ["n1"]
        assert quota.contributed_bytes == 1000 * GB
        assert quota.effective_bytes == 400 * GB
        assert quota.allocation_bytes == 200 * GB

    def test_non_storage_nodes_contribute_nothing(self):
        node = {
            "name": "hub",
            "roles": ["tahoe_introducer"],
            "member": "ale",
            "storage_contribution": "9TB",
        }
        assert compute_quotas(manifest([node]))[0].allocation_bytes == 0

    def test_encoding_change_shrinks_allocation(self):
        nodes = [storage("n1", "500GB", "ale")]
        two = compute_quotas(manifest(nodes, 2, 4, quota={"reserve": 0}))[0].allocation_bytes
        ten = compute_quotas(manifest(nodes, 3, 10, quota={"reserve": 0}))[0].allocation_bytes
        assert (two, ten) == (250 * GB, 150 * GB)


class TestNodeAllocation:
    def test_client_side_lookup(self):
        m = manifest(
            [
                storage("n1", "500GB", "ale"),
                {"name": "laptop", "roles": ["tahoe_client"], "member": "ale"},
            ],
            quota={"reserve": 0, "enforce": True},
        )
        member, allocation, settings = node_allocation(m, "laptop")
        assert (member, allocation, settings.enforce) == ("ale", 250 * GB, True)

    def test_unknown_node_has_no_allocation(self):
        member, allocation, _settings = node_allocation(manifest([]), "ghost")
        assert (member, allocation) == ("ghost", 0)


class TestResolveEncoding:
    def test_manifest_wins_over_environment(self):
        m = manifest([], needed=2, total=4)
        m["network"]["tahoe"]["shares_happy"] = 4
        env = {
            "REDUNDANET_SHARES_NEEDED": "1",
            "REDUNDANET_SHARES_HAPPY": "2",
            "REDUNDANET_SHARES_TOTAL": "2",
        }
        assert resolve_encoding(m, env) == (2, 4, 4)

    def test_environment_when_no_manifest(self):
        env = {
            "REDUNDANET_SHARES_NEEDED": "1",
            "REDUNDANET_SHARES_HAPPY": "2",
            "REDUNDANET_SHARES_TOTAL": "2",
        }
        assert resolve_encoding({}, env) == (1, 2, 2)

    def test_defaults_and_garbage(self):
        assert resolve_encoding({}, {}) == (3, 7, 10)
        assert resolve_encoding(
            {"network": {"tahoe": {"shares_needed": "x"}}}, {"REDUNDANET_SHARES_NEEDED": "y"}
        ) == (3, 7, 10)


class TestNodeEncoding:
    def test_reads_the_client_section(self, tmp_path):
        from redundanet.core.quota import node_encoding

        cfg = tmp_path / "tahoe.cfg"
        cfg.write_text(
            "# Tahoe-LAFS client configuration\n[node]\nnickname = n\n"
            "tub.location = tcp:10.100.0.5:3457\n\n[client]\nintroducer.furl = pb://x@tcp:h:1/y\n"
            "shares.needed = 2\nshares.happy = 4\nshares.total = 4\n\n[storage]\nenabled = false\n"
        )
        assert node_encoding(cfg) == (2, 4)

    def test_missing_or_broken_file_is_none(self, tmp_path):
        from redundanet.core.quota import node_encoding

        assert node_encoding(tmp_path / "absent") is None
        (tmp_path / "bad.cfg").write_text("[client]\nshares.needed = two\nshares.total = 4\n")
        assert node_encoding(tmp_path / "bad.cfg") is None
        (tmp_path / "nosection.cfg").write_text("[node]\nnickname = n\n")
        assert node_encoding(tmp_path / "nosection.cfg") is None
