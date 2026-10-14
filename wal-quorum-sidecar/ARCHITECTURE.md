<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# WAL Quorum Replication Sidecar — Architecture

## Problem

Accumulo Write-Ahead Logs (WALs) need two properties that conflict in Kubernetes:

1. **Low latency** — WAL sync is on the critical write path. Every mutation blocks until fsync.
2. **Cross-pod durability** — if a pod dies, another pod must be able to read the WAL for recovery.

Local PVC gives (1) but not (2) — when a pod dies, its PVC is inaccessible to other pods.
GCS gives (2) but not (1) — object storage adds 50-100ms per sync.

## Solution: 3-Node Quorum Replication

A Go sidecar runs alongside each tserver. WAL writes go through the sidecar, which writes
locally AND replicates to 2 peer sidecars. A write is acked after 2-of-3 nodes confirm
(local + fastest peer), giving local-speed writes with cross-pod durability.

```
                         ┌─────────────────────┐
                         │   TServer (Java)     │
                         │                      │
                         │  DfsLogger writes to  │
                         │  qwal://local/wal/... │
                         └──────────┬───────────┘
                                    │ Unix Domain Socket gRPC
                                    │ (~50μs IPC)
                         ┌──────────▼───────────┐
                         │  WAL Quorum Sidecar   │
                         │  (Go, same pod)       │
                         │                      │
                         │  1. Write to local    │
                         │     PVC (fdatasync)   │
                         │  2. Fan out to peers  │
                         │  3. Ack after quorum  │
                         └──┬───────────────┬───┘
                            │               │
              TCP gRPC :9710│               │TCP gRPC :9710
                            │               │
                   ┌────────▼──┐      ┌─────▼───────┐
                   │ Peer      │      │ Peer        │
                   │ Sidecar 1 │      │ Sidecar 2   │
                   │ (pod 1)   │      │ (pod 2)     │
                   └───────────┘      └─────────────┘
```

## gRPC Services

Two proto services define the communication:

### WalQuorumLocal (TServer ↔ Sidecar, Unix Domain Socket)

The tserver talks to its local sidecar over a Unix domain socket at `/var/run/qwal/qwal.sock`.
This is the hot path — every WAL write goes through here.

| RPC | Pattern | Purpose |
|-----|---------|---------|
| `OpenSegment` | Unary | Create a new WAL segment file. Sidecar prepares local file + notifies peers. |
| `WriteEntries` | Bidi stream | TServer streams WAL bytes continuously. Sidecar streams acks back after quorum. No per-entry round-trip. |
| `SyncSegment` | Unary | Blocks until quorum fsync. Called by `hsync()`/`hflush()`. |
| `CloseSegment` | Unary | Seals the segment. Triggers async GCS upload. Called on WAL rotation. |

**Why bidi streaming for WriteEntries:** Unary RPCs would add ~50μs round-trip per WAL entry.
With streaming, the sidecar batches entries and acks asynchronously — the TServer keeps writing
while acks flow back. This decouples write throughput from ack latency.

**Why unary for SyncSegment:** `hsync()` is an explicit durability barrier where the TServer
must block until data is durable. A unary call naturally models this — call, wait, return.

### WalQuorumPeer (Sidecar ↔ Sidecar, TCP port 9710)

Sidecars replicate WAL data to each other over TCP gRPC.

| RPC | Pattern | Purpose |
|-----|---------|---------|
| `PrepareSegment` | Unary | Tell peer to create a replica file for an incoming segment. |
| `ReplicateEntries` | Bidi stream | Stream WAL bytes to peer. Peer acks after local fdatasync. |
| `SealSegment` | Unary | Verify replica checksum matches originator. |
| `ReadSegment` | Server stream | Recovery: stream a replica back to the requesting node. |
| `ListSegments` | Unary | Discovery: which segments does this peer have? |
| `PurgeSegment` | Unary | Delete a replica after GCS upload confirmed. |

## Quorum Protocol

### Write Path (2-of-3 consensus)

```
TServer calls hsync()
    │
    ▼
Sidecar receives SyncSegment
    │
    ├── goroutine: fdatasync local file
    ├── goroutine: send to Peer 1, wait for ack
    └── goroutine: send to Peer 2, wait for ack
    │
    ▼
Wait for: local done AND >= 1 peer ack
    │
    ▼
Return SyncSegmentResponse to TServer
```

**Invariant:** Local write always participates. Quorum = local + fastest peer.
If both peers are dead, writes succeed locally (degraded mode) with a warning.

**Timeout:** 500ms default. If the fastest peer doesn't ack within 500ms, the write
degrades to local-only. This masks tail latency — you're only as slow as your fastest peer.

### Buffer Pooling

WAL entries are copied into `sync.Pool`'d 1MB byte slices for the fan-out. After all 3
goroutines complete (or timeout), the buffer is returned to the pool. This keeps the Go GC
from waking up on the hot path, maintaining flat tail latencies.

```go
var bufferPool = sync.Pool{
    New: func() interface{} {
        b := make([]byte, 0, 1<<20)
        return &b
    },
}
```

## Segment Lifecycle

```
OPEN → WRITING → SEALING → SEALED → UPLOADING → UPLOADED → PURGED
```

1. **OPEN:** TServer calls `OpenSegment`. Sidecar creates local file, sends `PrepareSegment` to peers.
2. **WRITING:** TServer streams entries via `WriteEntries`. Sidecar replicates to peers.
3. **SEALING:** Triggered by size limit (128MB), WAL rotation, or pod shutdown (SIGTERM).
   Sidecar fdatasync's, computes SHA-256, sends `SealSegment` to peers for checksum verification.
4. **UPLOADING:** Background goroutine uploads sealed segment to GCS.
5. **PURGED:** After GCS upload confirmed, `PurgeSegment` sent to all peers. Local file deleted.

### Graceful Shutdown

On SIGTERM (K8s pod termination):
1. Stop accepting new `OpenSegment` calls
2. Flush all active WriteEntries streams
3. Seal all open segments
4. Upload sealed segments to GCS (within 60s termination grace period)
5. Exit

## Recovery Path

When a tserver dies, the manager needs to recover its WALs.

```
Manager detects dead tserver
    │
    ▼
QuorumWalLogCloser.close(walPath)
    │
    ├── Check GCS for sealed segment → found? Ready for sort.
    │
    └── Not in GCS (unsealed segment, tserver died mid-write)
        │
        ▼
    Contact peer sidecars via ListSegments
        │
        ▼
    Peer seals its replica, uploads to GCS
        │
        ▼
    Return: ready for sort
```

**Key insight:** Because peers already have complete replicas, recovery doesn't need to
read from a dead pod's PVC. The surviving peer seals and uploads on behalf of the dead tserver.

## Hadoop FileSystem Integration

`QuorumWalFileSystem` implements Hadoop's `FileSystem` for the `qwal://` URI scheme.

```
instance.volumes = gs://bucket/tenants/xxx/accumulo, qwal://local/accumulo
```

Accumulo's `TieredVolumeChooser` routes WAL writes to `qwal://local/accumulo`:

```java
case WAL:
    String qwalVolume = chooseByPrefix(options, "qwal:");
    if (qwalVolume != null) {
        preferred = qwalVolume;  // → sidecar quorum write
    } else {
        preferred = cloudPrefix; // → fallback to GCS
    }
```

### Delegation Mode

The manager doesn't have a sidecar (only tservers do). `QuorumWalFileSystem` detects
the absence of the UDS socket and runs in **delegation mode** — all operations pass through
to GCS. This handles `accumulo init`, which writes metadata to all volumes.

```java
if (hasSidecar()) {
    // tserver: WAL writes go to sidecar
} else {
    // manager: delegate to GCS
}
```

### Volume Exclusion

Accumulo's `ServerDirs` and `Initialize` skip `qwal://` volumes when checking `instance_id`
and `version`. The qwal volume is WAL-only — it doesn't store cluster metadata.

```java
if (baseDir.startsWith("qwal://")) {
    continue; // skip init/version checks
}
```

## Peer Discovery

Peers are discovered statically from Kubernetes StatefulSet DNS conventions.
No service discovery protocol needed.

For a 3-replica StatefulSet where this pod is `tserver-N`:
- Peer 1: `tserver-((N+1)%3).<headless-service>.<namespace>.svc.cluster.local:9710`
- Peer 2: `tserver-((N+2)%3).<headless-service>.<namespace>.svc.cluster.local:9710`

The pod name and service name come from environment variables set by the Helm chart:
- `POD_NAME` (from Kubernetes downward API)
- `PEER_SERVICE_NAME` (from Helm template)
- `POD_NAMESPACE` (from Kubernetes downward API)

## Kubernetes Deployment

The sidecar runs as a container in the tserver StatefulSet pod:

```yaml
containers:
- name: tserver          # Accumulo tserver (Java)
- name: wal-quorum-sidecar  # WAL replication (Go)
```

Shared resources:
- **UDS volume** (`emptyDir: {medium: Memory}`) — `/var/run/qwal/qwal.sock`
- **PVC** — `/mnt/data` (sidecar writes WAL segments here)
- **Network** — peer port 9710 exposed via headless service

## Performance Characteristics

| Operation | Expected Latency |
|-----------|-----------------|
| TServer → Sidecar (UDS gRPC) | ~50μs |
| Local fdatasync (NVMe PVC) | ~100μs |
| Peer replication (datacenter network) | ~200-500μs |
| **Total hsync() with quorum** | **~300-600μs** |
| GCS direct write (for comparison) | 50-100ms |

The quorum write is ~100-200x faster than GCS direct writes.
