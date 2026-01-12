"""Unit tests for node module."""

from redundanet.core.node import Node, NodeHealth, NodeRole, NodeStatus


class TestNodeRole:
    """Tests for NodeRole enum."""

    def test_role_values(self):
        """Test NodeRole enum values."""
        assert NodeRole.TINC_VPN.value == "tinc_vpn"
        assert NodeRole.TAHOE_INTRODUCER.value == "tahoe_introducer"
        assert NodeRole.TAHOE_STORAGE.value == "tahoe_storage"
        assert NodeRole.TAHOE_CLIENT.value == "tahoe_client"

    def test_role_from_string(self):
        """Test creating NodeRole from string."""
        assert NodeRole("tinc_vpn") == NodeRole.TINC_VPN
        assert NodeRole("tahoe_storage") == NodeRole.TAHOE_STORAGE


class TestNodeStatus:
    """Tests for NodeStatus enum."""

    def test_status_values(self):
        """Test NodeStatus enum values."""
        assert NodeStatus.ONLINE.value == "online"
        assert NodeStatus.OFFLINE.value == "offline"
        assert NodeStatus.CONNECTING.value == "connecting"
        assert NodeStatus.ERROR.value == "error"
        assert NodeStatus.UNKNOWN.value == "unknown"


class TestNodeHealth:
    """Tests for NodeHealth dataclass."""

    def test_default_values(self):
        """Test NodeHealth default values."""
        health = NodeHealth()
        assert health.vpn_connected is False
        assert health.storage_available is False
        assert health.introducer_reachable is False
        assert health.last_seen is None
        assert health.uptime_seconds == 0
        assert health.errors == []

    def test_is_healthy(self):
        """Test is_healthy property."""
        health = NodeHealth(vpn_connected=True)
        assert health.is_healthy is True

        health_with_errors = NodeHealth(vpn_connected=True, errors=["some error"])
        assert health_with_errors.is_healthy is False

        health_no_vpn = NodeHealth(vpn_connected=False)
        assert health_no_vpn.is_healthy is False


class TestNode:
    """Tests for Node dataclass."""

    def test_node_creation(self):
        """Test creating a Node instance."""
        node = Node(
            name="test-node",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            public_ip="1.2.3.4",
            gpg_key_id="ABCD1234",
            roles=[NodeRole.TAHOE_STORAGE, NodeRole.TAHOE_CLIENT],
        )

        assert node.name == "test-node"
        assert node.internal_ip == "192.168.1.10"
        assert node.vpn_ip == "10.100.0.1"
        assert node.public_ip == "1.2.3.4"
        assert NodeRole.TAHOE_STORAGE in node.roles

    def test_node_minimal(self):
        """Test node with minimal fields."""
        node = Node(
            name="minimal-node",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
        )

        assert node.name == "minimal-node"
        assert node.public_ip is None
        assert node.roles == []

    def test_node_has_role(self):
        """Test checking if node has a role."""
        node = Node(
            name="test",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            roles=[NodeRole.TAHOE_STORAGE, NodeRole.TAHOE_CLIENT],
        )

        assert node.has_role(NodeRole.TAHOE_STORAGE)
        assert node.has_role(NodeRole.TAHOE_CLIENT)
        assert not node.has_role(NodeRole.TAHOE_INTRODUCER)

    def test_node_is_introducer(self):
        """Test is_introducer property."""
        introducer = Node(
            name="intro",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            roles=[NodeRole.TAHOE_INTRODUCER],
        )
        storage = Node(
            name="storage",
            internal_ip="192.168.1.11",
            vpn_ip="10.100.0.2",
            roles=[NodeRole.TAHOE_STORAGE],
        )

        assert introducer.is_introducer
        assert not storage.is_introducer

    def test_node_is_storage(self):
        """Test is_storage property."""
        storage = Node(
            name="storage",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            roles=[NodeRole.TAHOE_STORAGE],
        )
        client = Node(
            name="client",
            internal_ip="192.168.1.11",
            vpn_ip="10.100.0.2",
            roles=[NodeRole.TAHOE_CLIENT],
        )

        assert storage.is_storage
        assert not client.is_storage

    def test_node_is_client(self):
        """Test is_client property."""
        client = Node(
            name="client",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            roles=[NodeRole.TAHOE_CLIENT],
        )
        storage = Node(
            name="storage",
            internal_ip="192.168.1.11",
            vpn_ip="10.100.0.2",
            roles=[NodeRole.TAHOE_STORAGE],
        )

        assert client.is_client
        assert not storage.is_client

    def test_node_fqdn(self):
        """Test fqdn property."""
        node = Node(
            name="test-node",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
        )

        assert node.fqdn == "test-node.redundanet.local"

    def test_node_to_dict(self):
        """Test converting node to dictionary."""
        node = Node(
            name="test",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
            roles=[NodeRole.TAHOE_STORAGE],
        )

        data = node.to_dict()
        assert data["name"] == "test"
        assert data["internal_ip"] == "192.168.1.10"
        assert data["vpn_ip"] == "10.100.0.1"
        assert "tahoe_storage" in data["roles"]

    def test_node_default_status(self):
        """Test node default status is unknown."""
        node = Node(
            name="test",
            internal_ip="192.168.1.10",
            vpn_ip="10.100.0.1",
        )

        assert node.status == NodeStatus.UNKNOWN
