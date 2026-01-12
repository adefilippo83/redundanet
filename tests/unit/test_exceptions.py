"""Unit tests for exceptions module."""

import pytest

from redundanet.core.exceptions import (
    ConfigurationError,
    GPGError,
    ManifestError,
    NetworkError,
    RedundaNetError,
    StorageError,
    ValidationError,
    VPNError,
)


class TestExceptions:
    """Tests for RedundaNet exceptions."""

    def test_base_exception(self):
        """Test base RedundaNet exception."""
        exc = RedundaNetError("test error")
        assert "test error" in str(exc)
        assert isinstance(exc, Exception)

    def test_configuration_error(self):
        """Test configuration error."""
        exc = ConfigurationError("invalid config")
        assert "invalid config" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_manifest_error(self):
        """Test manifest error."""
        exc = ManifestError("manifest invalid")
        assert "manifest invalid" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_vpn_error(self):
        """Test VPN error."""
        exc = VPNError("vpn failed")
        assert "vpn failed" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_storage_error(self):
        """Test storage error."""
        exc = StorageError("storage failed")
        assert "storage failed" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_gpg_error(self):
        """Test GPG error."""
        exc = GPGError("gpg failed")
        assert "gpg failed" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_network_error(self):
        """Test network error."""
        exc = NetworkError("network failed")
        assert "network failed" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_validation_error(self):
        """Test validation error with error list."""
        exc = ValidationError("validation failed", errors=["error1", "error2"])
        assert "validation failed" in str(exc)
        assert "error1" in str(exc)
        assert "error2" in str(exc)
        assert isinstance(exc, RedundaNetError)

    def test_exception_inheritance(self):
        """Test that all exceptions inherit from RedundaNetError."""
        exceptions = [
            ConfigurationError,
            ManifestError,
            VPNError,
            StorageError,
            GPGError,
            NetworkError,
            ValidationError,
        ]

        for exc_class in exceptions:
            exc = exc_class("test")
            assert isinstance(exc, RedundaNetError)

    def test_raise_and_catch_specific(self):
        """Test raising and catching specific exceptions."""
        with pytest.raises(VPNError):
            raise VPNError("vpn error")

    def test_raise_and_catch_base(self):
        """Test catching specific exception with base class."""
        with pytest.raises(RedundaNetError):
            raise StorageError("storage error")

    def test_exception_with_details(self):
        """Test exception with details dictionary."""
        exc = RedundaNetError("test", details={"key": "value"})
        assert "key" in str(exc)
        assert "value" in str(exc)
