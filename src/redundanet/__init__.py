"""RedundaNet - Distributed encrypted storage on a mesh VPN network."""

__version__ = "2.0.0"
__author__ = "Alessandro De Filippo"
__email__ = "alessandro@example.com"

from redundanet.core.config import NetworkConfig, NodeConfig, TahoeConfig
from redundanet.core.manifest import Manifest

__all__ = [
    "Manifest",
    "NetworkConfig",
    "NodeConfig",
    "TahoeConfig",
    "__version__",
]
