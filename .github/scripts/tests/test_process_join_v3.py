"""Unit tests for the RedundaNet 3 membership request script (.github/scripts/process_join_v3.py)."""

from __future__ import annotations

import base64
import datetime
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO_ROOT / ".github" / "scripts"))

import process_join_v3 as v3  # noqa: E402

MEMBER_KEY = "wekrreaqlio4j3m4vzxo6hn5be2sa2gxxv7gx3mq54m4xffztv2a"
FPR = "1331C835A2B446B61A34959CD5294DA9303C084B"  # emiliano-giusti's node, below
ALESSIO_FPR = "7664298803B2711BA33B17CBCE29808CC14DED64"  # AlessioCappe's node, below

FAKE_SIGNATURE = """-----BEGIN PGP SIGNATURE-----

iQIzBAABCgAdFiEEiY2fFtSERod98h8sGPjLfLrhuc0FAmronM0ACgkQGPjLfLrh
uc3U2Q//RiUyr7QM9xln/YlIGJ5D0wbsf6hpKYfaDHc4ya67c9Yx4uCGX4VsqKUl
=ai+a
-----END PGP SIGNATURE-----
"""

# The shape of manifests/manifest.yaml: header comment, the v3 half with a
# member whose signature is a block scalar, two-space lists, then the v2
# half with column-0 `nodes` items and GitHub-cased `member:` fields.
MANIFEST = f"""# RedundaNet 3 keys live beside the RedundaNet 2 ones during the
# migration. Edit, then re-sign: redundanet manifest sign --key <operator.key>.
schema: 3
revision: 4
network:
  name: redundanet
  encoding: {{k: 2, n: 3}}
  version: 2.0.0
  domain: redundanet.local
  vpn_network: 10.100.0.0/16
  tahoe:
    shares_needed: 2
    shares_happy: 4
    shares_total: 4
    reserved_space: 1G
operators:
  - name: adefilippo83
    key: qlsi3aptqpe73pu4m4b6ahlaa5bwse2op7m7hjgzvxe7dxgp7i2q
hubs:
  - name: hub-hetzner
    relay: 2.29.46.249:8443
    tcp: 2.29.46.249:8443
    status: https://hub.redundanet.com/
    key: bm4ld4ezgwnvup4654wd6su7h3id2p4nvhfq6a7krn3qfgfsfkna
# Phase 1: the agent beside the stack stores nothing yet.
members:
  - name: adefilippo83
    key: {MEMBER_KEY}
    contribution_gb: 1800
    joined: 2026-11-02
    status: active
    legacy_pgp:
      fingerprint: 898D9F16D48446877DF21F2C18F8CB7CBAE1B9CD
      signature: |
        -----BEGIN PGP SIGNATURE-----

        iQIzBAABCgAdFiEEiY2fFtSERod98h8sGPjLfLrhuc0FAmronM0ACgkQGPjLfLrh
        =ai+a
        -----END PGP SIGNATURE-----
storage:
  - member: adefilippo83
    device: ne6ftzx2ygt53rmixrdknd7abvtu2774ejyrntv3kf3hnyy445fa
    name: falcon
  - member: adefilippo83
    device: f3tvhbkoldiobpjza7uocjt62a5onkcuthrpjbatoryuibcbue5a
    name: falcon-1
introducer_furl: pb://iufhp7il422sofdh5hkpc36l3famhg7x@tcp:10.100.0.1:3458/fjogqdpcjjbuug7gs6tp7aylmi6lq627
nodes:
- name: node-bae1b9cd
  member: adefilippo83
  internal_ip: 10.100.0.11
  vpn_ip: 10.100.0.11
  gpg_key_id: 898D9F16D48446877DF21F2C18F8CB7CBAE1B9CD
  region: europe
  status: active
  roles:
  - tinc_vpn
  - tahoe_storage
  storage_contribution: 900GB
  is_publicly_accessible: false
- name: node-303c084b
  member: emiliano-giusti
  internal_ip: 10.100.0.12
  vpn_ip: 10.100.0.12
  gpg_key_id: {FPR}
  region: europe
  status: active
  roles:
  - tinc_vpn
  - tahoe_storage
  is_publicly_accessible: false
- name: node-c14ded64
  internal_ip: 10.100.0.17
  vpn_ip: 10.100.0.17
  gpg_key_id: {ALESSIO_FPR}
  region: europe
  status: pending
  roles:
  - tinc_vpn
  - tahoe_storage
  storage_contribution: 250GB
  is_publicly_accessible: false
  member: AlessioCappe
"""


def new_key() -> str:
    return base64.b32encode(os.urandom(32)).decode().lower().rstrip("=")


def issue_body(
    name: str = "alice",
    key: str | None = None,
    contribution: str = "500",
    device_key: str | None = None,
    device_name: str = "nas-1",
    udp_port: str = "",
    fingerprint: str = "",
    notes: str = "",
    signature: str | None = None,
    bold: bool = False,
    newline: str = "\n",
) -> str:
    """The body docs/join.html produces (the facts sheet's rows, in order)."""
    key = key or new_key()
    if device_key is None and contribution not in ("", "0"):
        device_key = new_key()
    rows = [
        ("Member name", name),
        ("Member key", f"`{key}`"),
        ("Contribution GB", contribution),
        ("Device key", f"`{device_key}`" if device_key else ""),
        ("Device name", device_name if device_key else ""),
        ("UDP port", udp_port),
        ("Notes", notes),
    ]
    if fingerprint:  # a row of the earlier form, which the script ignores
        rows.insert(-1, ("Legacy GPG fingerprint", fingerprint))
    lines = ["## RedundaNet 3 membership", "", "| Field | Value |", "|-------|-------|"]
    for field, value in rows:
        label = f"**{field}**" if bold else field
        lines.append(f"| {label} | {value} |")
    if signature is not None:
        lines += ["", "## Legacy signature", "", signature.rstrip("\n")]
    return newline.join(lines) + newline


@pytest.fixture
def manifest_path(tmp_path: Path) -> Path:
    path = tmp_path / "manifests" / "manifest.yaml"
    path.parent.mkdir()
    path.write_text(MANIFEST)
    return path


def run(body: str, path: Path, **kwargs: object) -> v3.JoinResult:
    kwargs.setdefault("author", "Alice")
    kwargs.setdefault("today", lambda: "2026-11-05")
    return v3.process(body, path, **kwargs)  # type: ignore[arg-type]


class TestParseTable:
    def test_plain_rows(self):
        rows = v3.parse_table(issue_body(key="k" * 52, udp_port="4471", notes="hi there"))
        assert rows["member name"] == "alice"
        assert rows["member key"] == "k" * 52  # backticks removed
        assert rows["contribution gb"] == "500"
        assert rows["udp port"] == "4471"
        assert rows["notes"] == "hi there"

    def test_bold_names_and_crlf(self):
        rows = v3.parse_table(issue_body(key="k" * 52, bold=True, newline="\r\n"))
        assert rows["member name"] == "alice"
        assert rows["member key"] == "k" * 52

    def test_empty_cell_spellings(self):
        for empty in ("", "-", "none", "N/A"):
            rows = v3.parse_table(issue_body(udp_port=empty))
            assert rows["udp port"] == ""

    def test_missing_marker(self):
        with pytest.raises(v3.JoinError, match="heading"):
            v3.parse_table("## New Node Application\n| **GPG Key ID** | `abc` |\n")

    def test_marker_without_table(self):
        with pytest.raises(v3.JoinError, match="no table"):
            v3.parse_table("## RedundaNet 3 membership\n\nfree text\n")

    def test_table_ends_at_next_heading(self):
        body = issue_body(signature=FAKE_SIGNATURE) + "| Member name | mallory |\n"
        assert v3.parse_table(body)["member name"] == "alice"

    def test_first_row_wins(self):
        body = issue_body() + "| Member name | mallory |\n"
        assert v3.parse_table(body)["member name"] == "alice"


class TestParseRequest:
    def test_full_request(self):
        key, device = new_key(), new_key()
        req = v3.parse_request(
            issue_body(key=key, device_key=device, udp_port="4471", notes="x"), author="Alice"
        )
        assert req == v3.JoinRequest("alice", key, 500, device, "nas-1", 4471)

    def test_name_defaults_to_login_lowercased(self):
        req = v3.parse_request(issue_body(name=""), author="Bob-Builder")
        assert req.name == "bob-builder"

    def test_name_required_without_usable_login(self):
        with pytest.raises(v3.JoinError, match="Member name"):
            v3.parse_request(issue_body(name=""), author=None)

    def test_explicit_name_wins_over_login(self):
        assert v3.parse_request(issue_body(name="carol"), author="Bob").name == "carol"

    @pytest.mark.parametrize(
        "bad", ["Alice", "-alice", "al ice", "alice_1", "a" * 64, "x${{ evil }}", "a.b"]
    )
    def test_bad_names_rejected(self, bad: str):
        with pytest.raises(v3.JoinError, match="Member name"):
            v3.parse_request(issue_body(name=bad))

    def test_device_name_same_rule(self):
        with pytest.raises(v3.JoinError, match="Device name"):
            v3.parse_request(issue_body(device_name="NAS 1"))

    def test_uppercase_key_is_lowercased(self):
        key = new_key()
        assert v3.parse_request(issue_body(key=key.upper())).key == key

    @pytest.mark.parametrize("bad", ["k" * 51, "k" * 53, "1" * 52, "k" * 51 + "!", "k" * 51 + "b"])
    def test_bad_keys_rejected(self, bad: str):
        """Wrong length, wrong alphabet (base32 has no 0/1/8/9), or a
        non-canonical last character (its low bits are not zero)."""
        with pytest.raises(v3.JoinError, match="Member key"):
            v3.parse_request(issue_body(key=bad))

    def test_contribution_forms(self):
        assert v3.parse_request(issue_body(contribution="500 GB")).contribution_gb == 500
        assert v3.parse_request(issue_body(contribution="0")).contribution_gb == 0
        for bad in ("", "lots", "1.5", "-1", "10000000"):
            with pytest.raises(v3.JoinError, match="Contribution GB"):
                v3.parse_request(issue_body(contribution=bad, device_key=new_key()))

    def test_contribution_needs_a_device(self):
        with pytest.raises(v3.JoinError, match="needs a storage device"):
            v3.parse_request(issue_body(contribution="500", device_key=""))

    def test_device_needs_a_contribution(self):
        body = issue_body(contribution="0", device_key=new_key())
        with pytest.raises(v3.JoinError, match="contribution above 0"):
            v3.parse_request(body)

    def test_device_key_and_name_go_together(self):
        # A contributing member with half a device is told to fill both rows.
        with pytest.raises(v3.JoinError, match="fill Device key and Device name"):
            v3.parse_request(issue_body(device_name=""))
        with pytest.raises(v3.JoinError, match="go together"):
            v3.parse_request(issue_body(contribution="0", device_key=new_key(), device_name=""))

    def test_device_key_must_differ_from_member_key(self):
        key = new_key()
        with pytest.raises(v3.JoinError, match="equals the member key"):
            v3.parse_request(issue_body(key=key, device_key=key))

    def test_port_range_and_needs_device(self):
        for bad in ("0", "80", "1023", "65536", "abc"):
            with pytest.raises(v3.JoinError, match="UDP port"):
                v3.parse_request(issue_body(udp_port=bad))
        assert v3.parse_request(issue_body(udp_port="1024")).udp_port == 1024
        with pytest.raises(v3.JoinError, match="UDP port applies"):
            v3.parse_request(issue_body(contribution="0", udp_port="4471"))

    def test_rows_of_the_earlier_form_are_ignored(self):
        key, device = new_key(), new_key()
        plain = v3.parse_request(issue_body(key=key, device_key=device))
        req = v3.parse_request(
            issue_body(key=key, device_key=device, fingerprint=FPR, signature=FAKE_SIGNATURE)
        )
        assert req == plain

    def test_missing_required_rows(self):
        body = "## RedundaNet 3 membership\n\n| Field | Value |\n|--|--|\n| Member name | a |\n"
        with pytest.raises(v3.JoinError, match="member key, contribution gb"):
            v3.parse_request(body)


class TestCheckManifest:
    manifest = yaml.safe_load(MANIFEST)

    def check(self, body: str, author: str | None = "Alice") -> None:
        v3.check_manifest(v3.parse_request(body, author), self.manifest, author)

    def test_duplicate_member_name(self):
        with pytest.raises(v3.JoinError, match="already exists"):
            self.check(issue_body(name="adefilippo83"))

    def test_member_key_already_used(self):
        for used, who in (
            ("bm4ld4ezgwnvup4654wd6su7h3id2p4nvhfq6a7krn3qfgfsfkna", "hub hub-hetzner"),
            (MEMBER_KEY, "member adefilippo83"),
            ("ne6ftzx2ygt53rmixrdknd7abvtu2774ejyrntv3kf3hnyy445fa", "storage device falcon"),
        ):
            with pytest.raises(v3.JoinError, match=f"Member key is already .* {who}"):
                self.check(issue_body(key=used))
            with pytest.raises(v3.JoinError, match=f"Device key is already .* {who}"):
                self.check(issue_body(device_key=used))

    def test_device_name_already_used(self):
        for taken in ("falcon", "hub-hetzner"):
            with pytest.raises(v3.JoinError, match="already exists; pick another device name"):
                self.check(issue_body(device_name=taken))

    def test_v2_member_name_needs_the_account(self):
        with pytest.raises(v3.JoinError, match="member name of RedundaNet 2 nodes"):
            self.check(issue_body(name="alessiocappe"), author="mallory")
        self.check(issue_body(name="alessiocappe"), author="AlessioCappe")  # same login
        self.check(issue_body(name="emiliano-giusti"), author="emiliano-giusti")
        # A GPG fingerprint and signature, as the earlier form sent them, no
        # longer stand in for the account.
        with pytest.raises(v3.JoinError, match="member name of RedundaNet 2 nodes"):
            self.check(
                issue_body(name="alessiocappe", fingerprint=ALESSIO_FPR, signature=FAKE_SIGNATURE),
                author="mallory",
            )

    def test_not_a_v3_manifest(self):
        with pytest.raises(v3.JoinError, match="schema: 3"):
            v3.check_manifest(v3.parse_request(issue_body()), {"network": {}, "nodes": []})


class TestEditing:
    def test_append_keeps_the_comment_after_the_list(self):
        text = "hubs:\n  - name: h\n    key: k\n# about members\nmembers:\n  - name: a\n"
        out = v3.append_items(text, "hubs", lambda indent: [f"{indent}- name: h2"])
        assert (
            out
            == "hubs:\n  - name: h\n    key: k\n  - name: h2\n# about members\nmembers:\n  - name: a\n"
        )

    def test_append_detects_column_zero_items(self):
        text = "revision: 1\nnodes:\n- name: a\n  ip: 1\n"
        out = v3.append_items(text, "nodes", lambda indent: [f"{indent}- name: b"])
        assert out.endswith("- name: a\n  ip: 1\n- name: b\n")

    def test_append_to_empty_flow_list(self):
        out = v3.append_items("nodes: []\nx: 1\n", "nodes", lambda indent: [f"{indent}- name: a"])
        assert out == "nodes:\n  - name: a\nx: 1\n"

    def test_append_to_null_list_uses_default_indent(self):
        out = v3.append_items("members:\nstorage:\n", "members", lambda i: [f"{i}- name: a"])
        assert out == "members:\n  - name: a\nstorage:\n"

    def test_append_without_trailing_newline(self):
        out = v3.append_items("nodes:\n- name: a", "nodes", lambda i: [f"{i}- name: b"])
        assert yaml.safe_load(out) == {"nodes": [{"name": "a"}, {"name": "b"}]}

    def test_append_needs_exactly_one_key(self):
        with pytest.raises(v3.EditError, match="found 0"):
            v3.append_items("x: 1\n", "nodes", lambda _i: [])
        with pytest.raises(v3.EditError, match="found 2"):
            v3.append_items("nodes:\nnodes:\n", "nodes", lambda _i: [])

    def test_bump_revision(self):
        out, rev = v3.bump_revision("schema: 3\nrevision: 9  # nine\nnetwork:\n  revision: 1\n")
        assert rev == 10 and out == "schema: 3\nrevision: 10  # nine\nnetwork:\n  revision: 1\n"
        with pytest.raises(v3.EditError, match="found 0"):
            v3.bump_revision("schema: 3\n")

    def test_yaml_scalar_quotes_what_yaml_would_misread(self):
        assert v3.yaml_scalar("alice") == "alice"
        assert v3.yaml_scalar("1234") == '"1234"'
        assert v3.yaml_scalar("no") == '"no"'
        assert yaml.safe_load("name: " + v3.yaml_scalar("1234")) == {"name": "1234"}


class TestProcess:
    def test_entries_appended_textually(self, manifest_path: Path):
        key, device = new_key(), new_key()
        body = issue_body(key=key, device_key=device, udp_port="4471", notes="hello")
        result = run(body, manifest_path, author="Alice")
        assert result.success, result.error
        assert result.revision == 5

        new_text = manifest_path.read_text()
        member_block = "\n".join(v3.render_member(result.request, "2026-11-05", "  ")) + "\n"
        storage_block = "\n".join(v3.render_storage(result.request, "  ")) + "\n"
        assert member_block in new_text and storage_block in new_text
        # Everything else is byte-for-byte what it was: the header comment,
        # the inline comment, the existing signature block, the v2 half.
        untouched = new_text.replace(member_block, "").replace(storage_block, "")
        assert untouched == MANIFEST.replace("revision: 4", "revision: 5")

        data = yaml.safe_load(new_text)
        assert data["revision"] == 5
        assert data["members"][-1] == {
            "name": "alice",
            "key": key,
            "contribution_gb": 500,
            "joined": datetime.date(2026, 11, 5),
            "status": "active",
        }
        assert data["storage"][-1] == {
            "member": "alice",
            "device": device,
            "name": "nas-1",
            "udp_port": 4471,
        }
        assert data["members"][0]["legacy_pgp"]["signature"].endswith(
            "-----END PGP SIGNATURE-----\n"
        )

    def test_member_without_disk(self, manifest_path: Path):
        result = run(issue_body(contribution="0"), manifest_path)
        assert result.success, result.error
        data = yaml.safe_load(manifest_path.read_text())
        assert data["members"][-1]["contribution_gb"] == 0
        assert [s["name"] for s in data["storage"]] == ["falcon", "falcon-1"]
        assert "udp_port" not in yaml.safe_load(manifest_path.read_text())["members"][-1]

    def test_failure_leaves_the_file_untouched(self, manifest_path: Path):
        result = run(issue_body(name="adefilippo83"), manifest_path)
        assert not result.success and "already exists" in result.error
        assert manifest_path.read_text() == MANIFEST

    def test_missing_manifest(self, tmp_path: Path):
        result = run(issue_body(), tmp_path / "nope.yaml")
        assert not result.success and "could not update the manifest" in result.error

    def test_live_manifest_shape(self, tmp_path: Path):
        """The script against a copy of the live manifest."""
        path = tmp_path / "manifest.yaml"
        shutil.copy(REPO_ROOT / "manifests" / "manifest.yaml", path)
        before = yaml.safe_load(path.read_text())
        result = run(issue_body(udp_port="4471"), path)
        assert result.success, result.error
        after = yaml.safe_load(path.read_text())
        assert after["revision"] == before["revision"] + 1
        assert len(after["members"]) == len(before["members"]) + 1
        assert len(after["storage"]) == len(before["storage"]) + 1
        assert after["nodes"] == before["nodes"]
        assert after["members"][:-1] == before["members"]
        assert path.read_text().startswith("# RedundaNet 3 (RFC 0001) keys live beside")


class TestOutputs:
    req = v3.JoinRequest("alice", "k" * 52, 500, "d" * 52, "nas-1", 4471)

    def test_pr_text(self):
        pr = v3.compose_pr(self.req, "123", 5)
        assert pr["pr_title"] == "Add member alice (Issue #123)"
        assert "revision 5" in pr["commit_message"]
        body = pr["pr_body"]
        assert "git checkout join-request/123" in body
        assert "`scripts/sign.sh manifest join-request/123`" in body
        assert (
            "cd agent && go run ./cmd/redundanet manifest sign --key operator.key "
            "--manifest ../manifests/manifest.yaml"
        ) in body
        assert "Closes #123" in body
        assert "`nas-1` (`" + "d" * 52 + "`), UDP port 4471" in body
        assert "allocation_gb" not in body

    def test_pr_text_for_a_client_only_member(self):
        pr = v3.compose_pr(v3.JoinRequest("alice", "k" * 52, 0), "7", 5)
        assert "allocation_gb" in pr["pr_body"] and "sponsor" in pr["pr_body"]
        assert "| **Storage device** | none |" in pr["pr_body"]

    def test_next_steps(self):
        text = v3.compose_next_steps(self.req, 5)
        assert "{PR_URL}" in text
        assert "`redundanet service install`" in text
        assert "redundanet backup add NAME FOLDER" in text
        assert "`nas-1`" in text
        assert "docker" not in text.lower()

    def test_main_writes_outputs(self, manifest_path: Path, tmp_path: Path, monkeypatch):
        outputs = tmp_path / "outputs"
        monkeypatch.chdir(manifest_path.parent.parent)
        monkeypatch.setenv("GITHUB_OUTPUT", str(outputs))
        monkeypatch.setenv("ISSUE_NUMBER", "123")
        monkeypatch.setenv("ISSUE_AUTHOR", "Alice")
        monkeypatch.setenv("ISSUE_BODY", issue_body(name=""))
        assert v3.main() == 0
        text = outputs.read_text()
        assert "success<<" in text and "\ntrue\n" in text
        assert "member_name<<" in text and "\nalice\n" in text
        assert "pr_body<<" in text and "join-request/123" in text
        assert "revision<<" in text and "\n5\n" in text

    def test_main_fails_with_a_message(self, manifest_path: Path, tmp_path: Path, monkeypatch):
        outputs = tmp_path / "outputs"
        monkeypatch.chdir(manifest_path.parent.parent)
        monkeypatch.setenv("GITHUB_OUTPUT", str(outputs))
        monkeypatch.setenv("ISSUE_BODY", "no marker here")
        assert v3.main() == 1
        text = outputs.read_text()
        assert "\nfalse\n" in text and "heading" in text
        assert manifest_path.read_text() == MANIFEST


# --- a listed member adds a storage node ---------------------------------------

FALCON_KEY = "ne6ftzx2ygt53rmixrdknd7abvtu2774ejyrntv3kf3hnyy445fa"
CERT = "Q" * 96


def device_body(
    member: str = "adefilippo83",
    device_key: str | None = None,
    device_name: str = "nas-2",
    udp_port: str = "",
    contribution: str = "",
    notes: str = "",
    cert: str | None = CERT,
) -> str:
    body = f"""## RedundaNet 3 storage device

| Field | Value |
|-------|-------|
| Member name | {member} |
| Device key | {device_key or new_key()} |
| Device name | {device_name} |
| UDP port | {udp_port} |
| Contribution GB | {contribution} |
| Notes | {notes} |
"""
    if cert is not None:
        body += f"\n## Device certificate\n\n```\n{cert}\n```\n"
    return body


def verifier(
    member: str = MEMBER_KEY,
    device: str | None = None,
    roles: tuple[str, ...] = ("client", "storage"),
    network: str = "redundanet",
):
    """A stand-in for the agent's verify-certificate: what a valid certificate
    for the given facts would say. `device` None means: the Device key row."""
    seen: list[tuple[str, str]] = []

    def verify(certificate: str, net: str) -> dict:
        seen.append((certificate, net))
        rows = verify.rows  # type: ignore[attr-defined]
        return {
            "network": network,
            "member": member,
            "device": device or rows,
            "roles": list(roles),
            "serial": 7,
            "issued_at": "2026-11-08T12:00:00Z",
        }

    verify.seen = seen  # type: ignore[attr-defined]
    verify.rows = ""  # type: ignore[attr-defined]
    return verify


def run_device(body: str, path: Path, verify=None) -> v3.DeviceResult:
    if verify is None:
        verify = verifier()
    if not verify.rows:
        verify.rows = v3.parse_table(body, v3.DEVICE_MARKER)["device key"]
    return v3.process_device(body, path, verify_certificate=verify)


class TestDeviceRequest:
    def test_detects_the_kind(self):
        assert v3.is_device_request(device_body())
        assert not v3.is_device_request(issue_body())

    def test_parse(self):
        key = new_key()
        req = v3.parse_device_request(
            device_body(device_key=key, udp_port="4471", contribution="2500")
        )
        assert req == v3.DeviceRequest(
            member="adefilippo83",
            device_key=key,
            device_name="nas-2",
            certificate=CERT,
            udp_port=4471,
            contribution_gb=2500,
        )
        assert v3.parse_device_request(device_body()).contribution_gb is None

    def test_parse_refusals(self):
        with pytest.raises(v3.JoinError, match="Device certificate"):
            v3.parse_device_request(device_body(cert=None))
        with pytest.raises(v3.JoinError, match="base64url"):
            v3.parse_device_request(device_body(cert="not a certificate!"))
        with pytest.raises(v3.JoinError, match="missing the row"):
            v3.parse_device_request(device_body(device_name=""))
        with pytest.raises(v3.JoinError, match="Member name"):
            v3.parse_device_request(device_body(member="Not Valid"))

    def test_certificate_fenced_or_bare(self):
        bare = device_body(cert=None) + "\n## Device certificate\n\n" + CERT + "\n"
        assert v3.parse_device_request(bare).certificate == CERT
        split = (
            device_body(cert=None)
            + "\n## Device certificate\n\n```\n"
            + CERT[:40]
            + "\n"
            + CERT[40:]
            + "\n```\n"
        )
        assert v3.parse_device_request(split).certificate == CERT

    def test_storage_entry_appended_textually(self, manifest_path: Path):
        key = new_key()
        verify = verifier()
        result = run_device(device_body(device_key=key, udp_port="4471"), manifest_path, verify)
        assert result.success, result.error
        assert result.revision == 5 and result.member_key == MEMBER_KEY
        assert verify.seen == [(CERT, "redundanet")]
        new_text = manifest_path.read_text()
        block = "\n".join(v3.render_device_storage(result.request, "  ")) + "\n"
        assert block in new_text
        assert new_text.replace(block, "") == MANIFEST.replace("revision: 4", "revision: 5")
        data = yaml.safe_load(new_text)
        assert data["storage"][-1] == {
            "member": "adefilippo83",
            "device": key,
            "name": "nas-2",
            "udp_port": 4471,
        }
        assert data["members"][0]["contribution_gb"] == 1800

    def test_new_contribution_rewrites_the_members_line(self, manifest_path: Path):
        result = run_device(device_body(contribution="2500"), manifest_path)
        assert result.success, result.error
        new_text = manifest_path.read_text()
        assert "    contribution_gb: 2500\n" in new_text and "contribution_gb: 1800" not in new_text
        block = "\n".join(v3.render_device_storage(result.request, "  ")) + "\n"
        assert new_text.replace(block, "") == MANIFEST.replace(
            "revision: 4", "revision: 5"
        ).replace("contribution_gb: 1800", "contribution_gb: 2500")

    def test_refusals_leave_the_file_untouched(self, manifest_path: Path):
        cases = [
            (device_body(member="nobody"), verifier(), "no member named nobody"),
            (device_body(), verifier(member=new_key()), "another member key"),
            (device_body(), verifier(device=new_key()), "another device"),
            (device_body(), verifier(roles=("client",)), "no storage role"),
            (device_body(), verifier(network="othernet"), "network"),
            (device_body(device_key=FALCON_KEY), verifier(), "already in the manifest"),
            (device_body(device_name="falcon"), verifier(), "already exists"),
        ]
        for body, verify, message in cases:
            result = run_device(body, manifest_path, verify)
            assert not result.success and message in result.error, (message, result.error)
            assert manifest_path.read_text() == MANIFEST

    def test_set_member_contribution_on_the_live_manifest(self, tmp_path: Path):
        text = (REPO_ROOT / "manifests" / "manifest.yaml").read_text()
        before = yaml.safe_load(text)
        target = before["members"][-1]
        new_text = v3.set_member_contribution(text, target["name"], target["contribution_gb"] + 1)
        after = yaml.safe_load(new_text)
        assert after["members"][-1]["contribution_gb"] == target["contribution_gb"] + 1
        after["members"][-1]["contribution_gb"] = target["contribution_gb"]
        assert after == before
        with pytest.raises(v3.EditError):
            v3.set_member_contribution(text, "nobody", 1)

    def test_outputs(self):
        req = v3.parse_device_request(device_body(device_name="nas-9", contribution="900"))
        pr = v3.compose_device_pr(req, "42", 7)
        assert pr["pr_title"] == "Add storage device nas-9 for member adefilippo83 (Issue #42)"
        assert (
            "revision 7" in pr["pr_body"]
            and "now 900 GB" in pr["pr_body"]
            and "Closes #42" in pr["pr_body"]
        )
        assert "nas-9" in pr["commit_message"]
        steps = v3.compose_device_next_steps(req, 7)
        assert "{PR_URL}" in steps and "nas-9" in steps


@pytest.mark.skipif(shutil.which("go") is None, reason="the Go toolchain is not installed")
class TestDeviceRequestWithTheAgent:
    """The seam the workflow relies on: a certificate the agent made, checked
    by the agent's own verify-certificate, then the manifest edit."""

    def test_real_certificate(self, tmp_path: Path):
        agent = REPO_ROOT / "agent"
        data_dir = tmp_path / "node"
        env = {**os.environ, "GOFLAGS": "-mod=mod"}
        subprocess.run(
            [
                "go",
                "run",
                "./cmd/redundanet",
                "identity",
                "init",
                "--yes",
                "--storage",
                "--network",
                "redundanet",
                "--data-dir",
                str(data_dir),
            ],
            cwd=agent,
            check=True,
            capture_output=True,
            text=True,
            env=env,
            timeout=600,
        )
        shown = subprocess.run(
            [
                "go",
                "run",
                "./cmd/redundanet",
                "identity",
                "show",
                "--network",
                "redundanet",
                "--data-dir",
                str(data_dir),
            ],
            cwd=agent,
            check=True,
            capture_output=True,
            text=True,
            env=env,
            timeout=600,
        ).stdout
        member = next(
            line.split()[-1] for line in shown.splitlines() if line.startswith("member key:")
        )
        device = next(
            line.split()[-1] for line in shown.splitlines() if line.startswith("device key:")
        )
        cert = subprocess.run(
            [
                "go",
                "run",
                "./cmd/redundanet",
                "identity",
                "show",
                "--certificate",
                "--network",
                "redundanet",
                "--data-dir",
                str(data_dir),
            ],
            cwd=agent,
            check=True,
            capture_output=True,
            text=True,
            env=env,
            timeout=600,
        ).stdout.strip()

        path = tmp_path / "manifest.yaml"
        path.write_text(MANIFEST.replace(MEMBER_KEY, member))
        result = v3.process_device(device_body(device_key=device, cert=cert), path)
        assert result.success, result.error
        assert yaml.safe_load(path.read_text())["storage"][-1]["device"] == device

        tampered = cert[:-4] + ("AAAA" if not cert.endswith("AAAA") else "BBBB")
        path.write_text(MANIFEST.replace(MEMBER_KEY, member))
        result = v3.process_device(device_body(device_key=device, cert=tampered), path)
        assert not result.success and "does not verify" in result.error
