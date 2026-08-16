"""Tests for keyserver fetches and fingerprint pinning."""

from __future__ import annotations

import pgpy
import pytest
from pgpy.constants import (
    CompressionAlgorithm,
    HashAlgorithm,
    KeyFlags,
    PubKeyAlgorithm,
    SymmetricKeyAlgorithm,
)

from redundanet.auth.keyserver import (
    KeyServerClient,
    armored_key_fingerprint,
    armored_key_matches_id,
    normalize_key_id,
)


@pytest.fixture(scope="module")
def armored_key_and_fpr() -> tuple[str, str]:
    key = pgpy.PGPKey.new(PubKeyAlgorithm.RSAEncryptOrSign, 2048)
    uid = pgpy.PGPUID.new("KS Test", email="ks@test.local")
    key.add_uid(
        uid,
        usage={KeyFlags.Sign},
        hashes=[HashAlgorithm.SHA256],
        ciphers=[SymmetricKeyAlgorithm.AES256],
        compression=[CompressionAlgorithm.ZLIB],
    )
    fingerprint = str(key.fingerprint).replace(" ", "").upper()
    return str(key.pubkey), fingerprint


class TestNormalizeKeyId:
    def test_strips_spaces_prefix_and_case(self):
        assert normalize_key_id("0xdead beef cafe 1234") == "DEADBEEFCAFE1234"
        assert normalize_key_id("abcd1234") == "ABCD1234"


class TestFingerprintMatching:
    def test_full_fingerprint_exact_match(self, armored_key_and_fpr):
        armored, fpr = armored_key_and_fpr
        assert armored_key_matches_id(armored, fpr)
        assert armored_key_matches_id(armored, fpr.lower())
        # A single flipped character must fail.
        flipped = ("0" if fpr[0] != "0" else "1") + fpr[1:]
        assert not armored_key_matches_id(armored, flipped)

    def test_short_ids_never_match(self, armored_key_and_fpr):
        """Short 8/16-char ids are Evil32-collision-prone: fail closed, even
        when the suffix genuinely belongs to this key."""
        armored, fpr = armored_key_and_fpr
        assert not armored_key_matches_id(armored, fpr[-16:])
        assert not armored_key_matches_id(armored, f"0x{fpr[-8:]}")
        assert not armored_key_matches_id(armored, "0000000000000000")

    def test_unparseable_blob_never_matches(self):
        assert armored_key_fingerprint("garbage") is None
        assert not armored_key_matches_id("garbage", "DEADBEEFCAFE1234")


class FakeResponse:
    def __init__(self, text: str, status_code: int = 200) -> None:
        self.text = text
        self.status_code = status_code

    def raise_for_status(self) -> None:
        pass


class FakeHTTPClient:
    """Returns a fixed response for every GET, recording the requests."""

    def __init__(self, text: str) -> None:
        self._text = text
        self.requests: list[dict] = []

    def get(self, url, params=None):
        self.requests.append({"url": url, "params": params})
        return FakeResponse(self._text)

    def close(self) -> None:
        pass


class TestFetchKeyPinning:
    def _client_returning(self, text: str) -> KeyServerClient:
        client = KeyServerClient(gpg_manager=None, keyservers=["ks.example"])
        client._client = FakeHTTPClient(text)
        return client

    def test_matching_key_is_returned(self, armored_key_and_fpr):
        armored, fpr = armored_key_and_fpr
        client = self._client_returning(armored)
        assert client.fetch_key(fpr) == armored

    def test_short_id_lookup_refused_without_network(self, armored_key_and_fpr):
        """A short id is refused before any keyserver request is made."""
        armored, fpr = armored_key_and_fpr
        client = self._client_returning(armored)
        assert client.fetch_key(fpr[-16:]) is None
        assert client._client.requests == []  # fail-closed pre-network

    def test_mismatched_key_is_discarded(self, armored_key_and_fpr):
        """A keyserver serving the wrong key for an id must be ignored.

        This is the Evil32-style attack: an attacker uploads their own key so
        a lookup by the victim's id returns attacker material.
        """
        armored, _fpr = armored_key_and_fpr
        client = self._client_returning(armored)
        assert client.fetch_key("0" * 40) is None

    def test_non_key_response_is_ignored(self):
        client = self._client_returning("<html>rate limited</html>")
        assert client.fetch_key("DEADBEEFCAFE1234DEADBEEFCAFE1234DEADBEEF") is None
