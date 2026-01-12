# Architecture Overview

This document describes the architecture of RedundaNet and how its components work together.

## System Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                        RedundaNet Network                        │
│                                                                  │
│  ┌─────────────┐    ┌─────────────┐    ┌─────────────┐          │
│  │   Node A    │    │   Node B    │    │   Node C    │          │
│  │             │    │             │    │             │          │
│  │ ┌─────────┐ │    │ ┌─────────┐ │    │ ┌─────────┐ │          │
│  │ │ Tahoe   │ │    │ │ Tahoe   │ │    │ │ Tahoe   │ │          │
│  │ │ Client  │ │    │ │ Storage │ │    │ │Introducer│ │          │
│  │ └────┬────┘ │    │ └────┬────┘ │    │ └────┬────┘ │          │
│  │      │      │    │      │      │    │      │      │          │
│  │ ┌────┴────┐ │    │ ┌────┴────┐ │    │ ┌────┴────┐ │          │
│  │ │  Tinc   │ │◄──►│ │  Tinc   │ │◄──►│ │  Tinc   │ │          │
│  │ │   VPN   │ │    │ │   VPN   │ │    │ │   VPN   │ │          │
│  │ └─────────┘ │    │ └─────────┘ │    │ └─────────┘ │          │
│  └─────────────┘    └─────────────┘    └─────────────┘          │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

## Core Components

### 1. Tinc VPN Mesh

Tinc provides the secure network layer connecting all nodes.

**Key Features:**
- Full mesh topology (every node can connect to every other node)
- Automatic peer discovery
- NAT traversal
- Encrypted connections (RSA + AES)

**How it works:**
1. Each node generates an RSA keypair
2. Public keys are exchanged via the manifest
3. Tinc establishes encrypted tunnels between nodes
4. Traffic is routed through the mesh

```
Node A ◄────encrypted────► Node B
   │                          │
   │    encrypted             │    encrypted
   │                          │
   └──────────► Node C ◄──────┘
```

### 2. Tahoe-LAFS Storage

Tahoe-LAFS provides distributed, encrypted storage with erasure coding.

**Key Features:**
- Client-side encryption (data encrypted before upload)
- Erasure coding (data survives node failures)
- Capability-based security (cryptographic access control)

**Erasure Coding (3-of-10):**
```
Original File
     │
     ▼
┌─────────────┐
│  Encrypt    │
│  & Split    │
└─────┬───────┘
      │
      ▼
┌───┬───┬───┬───┬───┬───┬───┬───┬───┬───┐
│ 1 │ 2 │ 3 │ 4 │ 5 │ 6 │ 7 │ 8 │ 9 │10 │  10 shares created
└─┬─┴─┬─┴─┬─┴─┬─┴─┬─┴─┬─┴─┬─┴─┬─┴─┬─┴─┬─┘
  │   │   │   │   │   │   │   │   │   │
  ▼   ▼   ▼   ▼   ▼   ▼   ▼   ▼   ▼   ▼
 N1  N2  N3  N4  N5  N6  N7  N8  N9  N10   Distributed to nodes

Any 3 shares can reconstruct the original file
```

### 3. GPG Authentication

GPG keys provide node identity and authentication.

**Authentication Flow:**
```
1. Node generates GPG keypair
2. Public key uploaded to keyserver
3. Key ID added to manifest
4. Other nodes verify identity via keyserver
5. Manifest signed with GPG for integrity
```

### 4. Manifest System

The manifest is a YAML file defining the network configuration.

**Contents:**
- Network parameters
- Node list with IPs and roles
- Tahoe-LAFS settings
- GPG key IDs

**Distribution:**
- Stored in Git repository
- Nodes sync via `git pull`
- Changes require PR approval
- GPG signatures verify integrity

## Node Roles

### Introducer

The introducer is the rendezvous point for the storage network.

```
┌──────────────────────────────────────┐
│            Introducer                │
│                                      │
│  ┌────────────────────────────────┐  │
│  │      FURL Directory            │  │
│  │                                │  │
│  │  storage-1: pb://...           │  │
│  │  storage-2: pb://...           │  │
│  │  storage-3: pb://...           │  │
│  └────────────────────────────────┘  │
│                                      │
│  Clients and storage nodes           │
│  announce themselves here            │
└──────────────────────────────────────┘
```

**Responsibilities:**
- Maintain directory of storage nodes
- Provide FURL for clients to discover storage
- No access to stored data (encrypted by clients)

### Storage Node

Storage nodes hold encrypted data shares.

```
┌──────────────────────────────────────┐
│           Storage Node               │
│                                      │
│  ┌────────────────────────────────┐  │
│  │        Share Storage           │  │
│  │                                │  │
│  │  share_abc123: [encrypted]     │  │
│  │  share_def456: [encrypted]     │  │
│  │  share_ghi789: [encrypted]     │  │
│  └────────────────────────────────┘  │
│                                      │
│  Cannot decrypt data                 │
│  Only stores and serves shares       │
└──────────────────────────────────────┘
```

**Responsibilities:**
- Store encrypted data shares
- Serve shares on request
- Report capacity to introducer

### Client

Clients upload and download data.

```
┌──────────────────────────────────────┐
│              Client                  │
│                                      │
│  ┌────────────────────────────────┐  │
│  │         Upload Flow            │  │
│  │                                │  │
│  │  1. Read file                  │  │
│  │  2. Encrypt with random key    │  │
│  │  3. Split into shares          │  │
│  │  4. Distribute to storage      │  │
│  │  5. Return capability (URI)    │  │
│  └────────────────────────────────┘  │
│                                      │
│  ┌────────────────────────────────┐  │
│  │        Download Flow           │  │
│  │                                │  │
│  │  1. Parse capability           │  │
│  │  2. Fetch shares from storage  │  │
│  │  3. Reconstruct file           │  │
│  │  4. Decrypt with embedded key  │  │
│  └────────────────────────────────┘  │
└──────────────────────────────────────┘
```

## Data Flow

### Upload Process

```
User File
    │
    ▼
┌─────────────┐
│   Client    │─────────┐
│             │         │
│ 1. Encrypt  │         │
│ 2. Encode   │         │
│ 3. Split    │         │
└──────┬──────┘         │
       │                │
       ▼                ▼
  ┌────────┐      ┌────────┐
  │Share 1 │      │Share n │
  └────┬───┘      └────┬───┘
       │               │
       ▼               ▼
  ┌────────┐      ┌────────┐
  │Storage │      │Storage │
  │ Node 1 │      │ Node n │
  └────────┘      └────────┘
```

### Download Process

```
  ┌────────┐      ┌────────┐
  │Storage │      │Storage │
  │ Node 1 │      │ Node 3 │
  └────┬───┘      └────┬───┘
       │               │
       ▼               ▼
  ┌────────┐      ┌────────┐
  │Share 1 │      │Share 3 │
  └────┬───┘      └────┬───┘
       │               │
       └───────┬───────┘
               │
               ▼
        ┌─────────────┐
        │   Client    │
        │             │
        │ 1. Collect  │
        │ 2. Decode   │
        │ 3. Decrypt  │
        └──────┬──────┘
               │
               ▼
          User File
```

## Security Model

### Encryption Layers

1. **Transport**: TLS over Tinc VPN
2. **Storage**: AES-128 (Tahoe-LAFS convergent encryption)
3. **Capability**: Cryptographic read/write capabilities

### Threat Model

| Threat | Mitigation |
|--------|------------|
| Network eavesdropping | Tinc VPN encryption |
| Storage node compromise | Client-side encryption |
| Single node failure | Erasure coding (3-of-10) |
| Rogue node joining | GPG authentication |
| Manifest tampering | GPG signatures |

### What Storage Nodes Can See

- Encrypted data blobs
- Share identifiers
- Access patterns (which shares are requested)

### What Storage Nodes Cannot See

- File contents
- File names
- File metadata
- Who uploaded what

## Scalability

### Adding Nodes

1. New node generates keys
2. Submit PR to manifest
3. Existing nodes verify GPG key
4. PR merged, manifest updated
5. All nodes sync new manifest
6. New node joins mesh

### Network Growth

```
Nodes: 10   → Storage: ~5TB   → Capacity: ~500GB effective
Nodes: 100  → Storage: ~50TB  → Capacity: ~5TB effective
Nodes: 1000 → Storage: ~500TB → Capacity: ~50TB effective

(Assuming 500GB contribution per node, 3-of-10 encoding)
```

## Failure Scenarios

### Single Node Failure

- Data remains accessible (need only 3 of 10 shares)
- Tahoe-LAFS can repair by recreating missing shares
- Network continues operating

### Multiple Node Failures

- Data accessible if ≥3 shares survive
- Automatic repair when nodes return
- New nodes can receive repairs

### Introducer Failure

- Existing connections continue working
- New connections fail until introducer returns
- Consider multiple introducers for resilience
