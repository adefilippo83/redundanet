"""RedundaNet - Distributed encrypted storage on a mesh VPN network."""

from importlib.metadata import PackageNotFoundError, version

try:
    __version__ = version("redundanet")
except PackageNotFoundError:  # running from a source tree without an install
    __version__ = "0.0.0+dev"

__author__ = "Alessandro De Filippo"
__email__ = "alessandro@defilippo.me"

from redundanet.core.config import NetworkConfig, NodeConfig, TahoeConfig
from redundanet.core.manifest import Manifest

__all__ = [
    "Manifest",
    "NetworkConfig",
    "NodeConfig",
    "TahoeConfig",
    "__version__",
]
