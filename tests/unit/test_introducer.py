"""Unit tests for redundanet.monitor.introducer (announcements per identity)."""

from __future__ import annotations

from datetime import UTC, datetime

from redundanet.monitor.introducer import (
    Announcement,
    identity_notes,
    parse_announcements,
    summarize_storage,
)
from redundanet.monitor.status import collect_status


def announcement_row(nick: str, key: str, when: str, service: str = "storage") -> str:
    """One row of the introducer's 'Service Announcements' table (Tahoe 1.20)."""
    return (
        '<tr> <td class="nickname-and-peerid"> '
        f'<div class="nickname">{nick}</div> '
        f'<div class="nodeid data-chars">v0-{key}</div></td> '
        f'<td class="service-announced" title="connection hints: tcp:10.100.0.10:3457">{when}</td> '
        '<td class="service-version">tahoe-lafs/1.20.0</td> '
        f'<td class="service-service-name">{service}</td> </tr>'
    )


def subscriber_row(nick: str, when: str) -> str:
    """One row of the 'Subscribed Clients' table: same nickname markup, a
    different time column, no announcement."""
    return (
        '<tr> <td class="nickname-and-peerid"> '
        f'<div class="nickname">{nick}</div> '
        '<div class="nodeid data-chars">rsxxcdowosamdlprey366gemfbgzcbsm</div></td> '
        "<td>10.100.0.14:42022</td> "
        f'<td class="service-since">{when}</td> '
        '<td class="service-version">tahoe-lafs/1.20.0</td> '
        '<td class="service-service-name">storage</td> </tr>'
    )


PAGE = "\n".join(
    [
        "<html><body><h2>Service Announcements</h2><table>",
        announcement_row("node-2680cd08-storage", "aaaa", "2026-09-10 09:38:32"),
        announcement_row("node-bc1232bc-storage", "bbb1", "2026-09-08 09:28:30"),
        announcement_row("node-bc1232bc-storage", "bbb2", "2026-09-09 17:05:39"),
        announcement_row("node-bc1232bc-storage", "bbb3", "2026-09-10 07:32:42"),
        announcement_row("stranger-storage", "cccc", "2026-09-01 00:00:00"),
        "</table><h2>Subscribed Clients</h2><table>",
        subscriber_row("node-2680cd08-client", "2026-09-10 09:38:33"),
        subscriber_row("node-bc1232bc-storage", "2026-09-10 07:32:42"),
        "</table></body></html>",
    ]
)


class TestParseAnnouncements:
    def test_reads_announcements_and_skips_subscribers(self):
        found = parse_announcements(PAGE)
        assert len(found) == 5
        assert found[0] == Announcement(
            "node-2680cd08-storage", "v0-aaaa", "2026-09-10 09:38:32", "storage"
        )
        assert found[0].node_name == "node-2680cd08"
        assert {a.nickname for a in found} == {
            "node-2680cd08-storage",
            "node-bc1232bc-storage",
            "stranger-storage",
        }

    def test_empty_or_foreign_page(self):
        assert parse_announcements("") == []
        assert parse_announcements("<html><table><tr><td>x</td></tr></table></html>") == []


class TestSummarizeStorage:
    def test_counts_distinct_servers_and_finds_churn(self):
        summary = summarize_storage(parse_announcements(PAGE), ["node-2680cd08", "node-bc1232bc"])
        assert summary.servers == 3  # 2 known nicknames + the stranger
        assert summary.identities == 5  # what the introducer's JSON would say
        assert list(summary.churned) == ["node-bc1232bc"]
        assert [a.server_id for a in summary.churned["node-bc1232bc"]] == [
            "v0-bbb1",
            "v0-bbb2",
            "v0-bbb3",
        ]
        assert summary.unknown == ["stranger"]

    def test_other_services_are_ignored(self):
        anns = [Announcement("n-storage", "v0-a", "2026-09-10 00:00:00", "helper")]
        assert summarize_storage(anns, ["n"]).servers == 0

    def test_notes(self):
        notes = identity_notes(
            summarize_storage(parse_announcements(PAGE), ["node-2680cd08", "node-bc1232bc"])
        )
        assert len(notes) == 2
        assert notes[0].startswith(
            "node-bc1232bc has announced 3 storage identities since 2026-09-08 09:28 UTC "
            "(last 2026-09-10 07:32)"
        )
        assert "volumes were recreated" in notes[0]
        assert notes[1] == "storage server 'stranger' is announced but is not in the manifest"

    def test_healthy_network_has_no_notes(self):
        anns = parse_announcements(announcement_row("node-a-storage", "k", "2026-09-10 00:00:00"))
        assert identity_notes(summarize_storage(anns, ["node-a"])) == []


def manifest() -> dict:
    return {
        "network": {"tahoe": {"shares_needed": 2, "shares_happy": 4, "shares_total": 4}},
        "nodes": [
            {"name": "node-2680cd08", "internal_ip": "10.100.0.10", "roles": ["tahoe_storage"]},
            {"name": "node-bc1232bc", "internal_ip": "10.100.0.15", "roles": ["tahoe_storage"]},
        ],
    }


class TestCollectStatusWithAnnouncements:
    def collect(self, announcements):
        return collect_status(
            manifest=manifest(),
            self_name="hub",
            ping=lambda _ip: 1.0,
            storage_connected=5,  # the JSON count: 5 announcements
            furl_present=True,
            manifest_synced_at=None,
            now=datetime(2026, 9, 10, 8, 0, tzinfo=UTC),
            announcements=announcements,
        )

    def test_distinct_servers_replace_the_announcement_count(self):
        status = self.collect(parse_announcements(PAGE))
        assert status.grid.storage_connected == 3
        assert status.grid.storage_identities == 5
        assert any("node-bc1232bc has announced 3 storage identities" in n for n in status.notes)
        assert any("'stranger'" in n for n in status.notes)
        assert status.to_dict()["grid"]["storage_identities"] == 5

    def test_without_announcements_the_count_is_used_as_is(self):
        status = self.collect(None)
        assert status.grid.storage_connected == 5
        assert status.grid.storage_identities is None
