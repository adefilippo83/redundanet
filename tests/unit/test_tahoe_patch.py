"""Unit tests for the Tahoe lease-crawler patch applied in the base image
(docker/patches/tahoe_expirer_json_state.py)."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).parent.parent.parent
sys.path.insert(0, str(REPO_ROOT / "docker" / "patches"))

import tahoe_expirer_json_state as patch_module  # noqa: E402

# The shape of LeaseCheckingCrawler.add_initial_state in Tahoe-LAFS 1.20.0.
TAHOE_SNIPPET = """
class LeaseCheckingCrawler:
    def __init__(self, state):
        self.state = state

    def create_empty_cycle_dict(self):
        return {"corrupt-shares": [], "lease-age-histogram": {}, "leases-per-share-histogram": {}}

    def add_initial_state(self):
        # we fill ["cycle-to-date"] here (even though they will be reset in
        # self.started_cycle) just in case someone grabs our state before we
        # get started: unit tests do this
        so_far = self.create_empty_cycle_dict()
        self.state.setdefault("cycle-to-date", so_far)
        # in case we upgrade the code while a cycle is in progress, update
        # the keys individually
        for k in so_far:
            self.state["cycle-to-date"].setdefault(k, so_far[k])

    def increment(self, d, k, delta=1):
        if k not in d:
            d[k] = 0
        d[k] += delta

    def add_lease_age_to_histogram(self, age):
        k = (0, 86400)
        self.increment(self.state["cycle-to-date"]["lease-age-histogram"], k, 1)
"""


def crawler_class(source: str):
    namespace: dict = {}
    exec(compile(source, "expirer.py", "exec"), namespace)  # noqa: S102 - the test's own snippet
    return namespace["LeaseCheckingCrawler"]


def loaded_mid_cycle_state() -> dict:
    """What the JSON state file holds mid-cycle: the histogram as a list."""
    return {"cycle-to-date": {"lease-age-histogram": [[0, 86400, 3], [86400, 172800, 2]]}}


class TestPatch:
    def test_unpatched_tahoe_crashes_on_the_first_lease(self):
        crawler = crawler_class(TAHOE_SNIPPET)(loaded_mid_cycle_state())
        crawler.add_initial_state()
        with pytest.raises(TypeError, match="list indices"):
            crawler.add_lease_age_to_histogram(10)

    def test_patched_tahoe_restores_the_histogram(self):
        crawler = crawler_class(patch_module.patch(TAHOE_SNIPPET))(loaded_mid_cycle_state())
        crawler.add_initial_state()
        crawler.add_lease_age_to_histogram(10)
        assert crawler.state["cycle-to-date"]["lease-age-histogram"] == {
            (0, 86400): 4,
            (86400, 172800): 2,
        }

    def test_fresh_state_is_untouched(self):
        crawler = crawler_class(patch_module.patch(TAHOE_SNIPPET))({})
        crawler.add_initial_state()
        crawler.add_lease_age_to_histogram(10)
        assert crawler.state["cycle-to-date"]["lease-age-histogram"] == {(0, 86400): 1}

    def test_idempotent_and_loud_when_tahoe_changes(self):
        once = patch_module.patch(TAHOE_SNIPPET)
        assert patch_module.patch(once) == once
        with pytest.raises(ValueError, match="anchor"):
            patch_module.patch("def add_initial_state(self): pass\n")

    def test_cli(self, tmp_path: Path, capsys):
        path = tmp_path / "expirer.py"
        path.write_text(TAHOE_SNIPPET)
        assert patch_module.main(["patch", str(path)]) == 0
        assert patch_module.MARKER in path.read_text()
        assert patch_module.main(["patch", str(path)]) == 0
        assert "already patched" in capsys.readouterr().out
        assert patch_module.main(["patch"]) == 2
