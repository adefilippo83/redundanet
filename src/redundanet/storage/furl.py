"""FURL parsing and validation for Tahoe-LAFS introducers."""

from __future__ import annotations

import re

from redundanet.core.exceptions import StorageError

# FURL format: pb://<tubid>@<location>/<swissnum>
# The location may be a comma-separated list of connection hints
# (e.g. "tcp:10.100.0.1:3458" or "10.100.0.1:3458,example.com:3458").
FURL_PATTERN = re.compile(r"^pb://([a-z2-7]+)@([^/]+)/([a-z2-7]+)$")


def parse_furl(furl: str) -> dict[str, str]:
    """Parse a FURL into its components.

    Args:
        furl: The FURL string

    Returns:
        Dictionary with tubid, location, and swissnum
    """
    match = FURL_PATTERN.match(furl.strip())
    if not match:
        raise StorageError(f"Invalid FURL format: {furl}")

    return {
        "tubid": match.group(1),
        "location": match.group(2),
        "swissnum": match.group(3),
    }


def validate_furl(furl: str) -> bool:
    """Validate a FURL format.

    Args:
        furl: The FURL string

    Returns:
        True if valid
    """
    try:
        parse_furl(furl)
        return True
    except StorageError:
        return False
