# Architecture Overview

This document describes the architecture of RedundaNet and how its components work together.

## System Overview

RedundaNet is a distributed encrypted storage system with three main layers:

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           Application Layer                             │
│                    (CLI, Web Interface, APIs)                           │
├─────────────────────────────────────────────────────────────────────────┤
│                           Storage Layer                                 │
│                  (Tahoe-LAFS: Encryption + Erasure Coding)             │
├─────────────────────────────────────────────────────────────────────────┤
│                           Network Layer                                 │
│                    (Tinc VPN: Encrypted Mesh Network)                   │
├─────────────────────────────────────────────────────────────────────────┤
│                           Identity Layer                                │
│                    (GPG Keys + Public Keyservers)                       │
└─────────────────────────────────────────────────────────────────────────┘
```

## Network Architecture

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

**Why GPG?**
- Decentralized identity (no central authority)
- Proven cryptographic security
- Public keyserver infrastructure already exists
- Users control their own keys

**Authentication Flow:**
```
1. Node generates GPG keypair
2. Public key uploaded to keyserver (keys.openpgp.org)
3. Key ID added to network manifest
4. Other nodes verify identity via keyserver
5. Manifest can be GPG-signed for integrity
```

### 4. Manifest System

The manifest is a YAML file defining the network configuration.

**Contents:**
- Network parameters (name, version, VPN range)
- Tahoe-LAFS settings (shares needed/total)
- Node list with IPs, GPG keys, and roles
- Introducer FURL

**Distribution via Git:**
```
┌─────────────────────────────────────────────────────────────┐
│                    GitHub Repository                         │
│                                                             │
│  manifests/manifest.yaml                                    │
│  ├── network configuration                                  │
│  ├── node list (auto-updated by GitHub Actions)            │
│  └── introducer FURL                                        │
│                                                             │
│  On join request:                                           │
│  1. User submits issue via join.html                       │
│  2. GitHub Action parses request                           │
│  3. PR created to add node to manifest                     │
│  4. Maintainer reviews and merges                          │
│  5. All nodes sync updated manifest                        │
└─────────────────────────────────────────────────────────────┘
```

## Node Joining Process

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Node Joining Flow                               │
│                                                                         │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐          │
│  │ Generate │    │ Publish  │    │  Submit  │    │  Wait    │          │
│  │ GPG Key  │───►│ to Key   │───►│  Join    │───►│  for     │          │
│  │          │    │ Server   │    │ Request  │    │ Approval │          │
│  └──────────┘    └──────────┘    └──────────┘    └──────────┘          │
│       │                                               │                 │
│       │ redundanet node                              │ Maintainer       │
│       │ keys generate                                │ reviews PR       │
│       │                                               │                 │
│       ▼                                               ▼                 │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐          │
│  │  Clone   │    │  Init    │    │  Sync    │    │  Start   │          │
│  │  Repo    │◄───│  Node    │◄───│ Manifest │◄───│ Services │          │
│  │          │    │          │    │          │    │          │          │
│  └──────────┘    └──────────┘    └──────────┘    └──────────┘          │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

**Automated Processing:**
1. User fills form at redundanet.com/join.html
2. Form creates GitHub issue with `join-request` label
3. GitHub Action triggers on issue creation
4. Action parses GPG key ID, storage, region
5. Action verifies GPG key exists on keyserver
6. Action assigns next available VPN IP
7. Action creates PR to update manifest
8. Maintainer reviews and merges PR
9. Issue is closed with setup instructions

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

| Layer | Technology | Purpose |
|-------|------------|---------|
| Transport | Tinc VPN (RSA + AES) | Encrypts all network traffic |
| Storage | Tahoe-LAFS (AES-128) | Encrypts data at rest |
| Capability | Cryptographic URIs | Access control |
| Identity | GPG (RSA-4096) | Node authentication |

### Threat Model

| Threat | Mitigation |
|--------|------------|
| Network eavesdropping | Tinc VPN encryption |
| Storage node compromise | Client-side encryption |
| Single node failure | Erasure coding (3-of-10) |
| Rogue node joining | GPG authentication via keyserver |
| Manifest tampering | Git history + GPG signatures |
| Man-in-the-middle | VPN certificates + GPG verification |

### What Storage Nodes Can See

- Encrypted data blobs (meaningless without keys)
- Share identifiers
- Access patterns (which shares are requested when)
- IP addresses of requesters (within VPN)

### What Storage Nodes Cannot See

- File contents
- File names
- File metadata
- Who uploaded what
- Relationship between shares

## Scalability

### Adding Nodes

1. New node generates GPG key and publishes to keyserver
2. User submits join request via web form
3. GitHub Action creates PR to add node
4. Maintainer reviews and merges
5. All nodes sync new manifest via `git pull`
6. New node joins Tinc mesh
7. New node announces to Tahoe introducer

### Network Growth

```
Nodes: 10   → Storage: ~5TB   → Effective Capacity: ~500GB
Nodes: 100  → Storage: ~50TB  → Effective Capacity: ~5TB
Nodes: 1000 → Storage: ~500TB → Effective Capacity: ~50TB

(Assuming 500GB contribution per node, 3-of-10 encoding)
```

**Note:** Effective capacity is ~10% of total due to 3-of-10 erasure coding overhead.

## Failure Scenarios

### Single Node Failure

- Data remains accessible (need only 3 of 10 shares)
- Tahoe-LAFS can repair by recreating missing shares
- Network continues operating normally

### Multiple Node Failures

- Data accessible if ≥3 shares survive
- Automatic repair when nodes return
- New nodes can receive repair shares

### Introducer Failure

- Existing connections continue working
- New connections fail until introducer returns
- Recommendation: Run multiple introducers for resilience

### Network Partition

- Nodes in each partition continue working
- Cross-partition requests fail
- Automatic recovery when partition heals

## Container Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         Docker Compose Stack                            │
│                                                                         │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐         │
│  │   tinc          │  │ tahoe-storage   │  │  tahoe-client   │         │
│  │                 │  │                 │  │                 │         │
│  │ • VPN daemon    │  │ • Storage svc   │  │ • Client svc    │         │
│  │ • Port 655      │  │ • Port 3457     │  │ • Port 3456     │         │
│  │ • Mesh network  │  │ • Data storage  │  │ • FUSE mount    │         │
│  └────────┬────────┘  └────────┬────────┘  └────────┬────────┘         │
│           │                    │                    │                   │
│           └────────────────────┴────────────────────┘                   │
│                                │                                        │
│                    ┌───────────┴───────────┐                           │
│                    │      Shared Volumes    │                           │
│                    │  • tinc-config         │                           │
│                    │  • tahoe-storage       │                           │
│                    │  • storage-data        │                           │
│                    └───────────────────────┘                           │
└─────────────────────────────────────────────────────────────────────────┘
```
