"""Custom exceptions for RedundaNet."""

from typing import Any


class RedundaNetError(Exception):
    """Base exception for all RedundaNet errors."""

    def __init__(self, message: str, details: dict[str, Any] | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.details = details or {}

    def __str__(self) -> str:
        if self.details:
            return f"{self.message} - Details: {self.details}"
        return self.message


class ConfigurationError(RedundaNetError):
    """Raised when there's a configuration error."""

    pass


class ManifestError(RedundaNetError):
    """Raised when there's a manifest parsing or validation error."""

    pass


class NodeError(RedundaNetError):
    """Raised when there's a node-related error."""

    pass


class VPNError(RedundaNetError):
    """Raised when there's a VPN-related error."""

    pass


class NetworkError(RedundaNetError):
    """Raised when there's a network-related error."""

    pass


class StorageError(RedundaNetError):
    """Raised when there's a storage-related error."""

    pass


class GPGError(RedundaNetError):
    """Raised when there's a GPG-related error."""

    pass


class KeyServerError(RedundaNetError):
    """Raised when there's a keyserver-related error."""

    pass


class ValidationError(RedundaNetError):
    """Raised when validation fails."""

    def __init__(
        self,
        message: str,
        errors: list[str] | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(message, details)
        self.errors = errors or []

    def __str__(self) -> str:
        if self.errors:
            error_list = "\n  - ".join(self.errors)
            return f"{self.message}:\n  - {error_list}"
        return super().__str__()
