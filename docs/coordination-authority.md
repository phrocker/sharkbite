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
# Coordination authority and quorum model

Status: **accepted architecture direction**, 2026-08-19. Implementation is
tracked by [#128](https://github.com/phrocker/shoal-oss/issues/128).
This document is the release-gated target contract; it does not claim that the
current binaries already implement the coordinator adapters, durable fencing,
or cutover state machine.

## 1. Decision

Shoal uses **one coordination authority for each logical table at any point in
time**. It does not mirror ownership into two quorum systems and it does not
add a second consensus service beside an Accumulo deployment.

| Deployment domain | Coordination authority | Durable state |
| --- | --- | --- |
| Embedded, one process | OS/PVC exclusion plus a persisted manifest generation | WAL, immutable files, and manifests on the configured backend |
| Shoal-only Kubernetes pods | Kubernetes `coordination.k8s.io/Lease` objects for membership and election | CAS-protected manifests plus WAL and immutable files on PVC/object storage |
| Accumulo-connected Shoal | Accumulo ZooKeeper ServiceLocks and manager/coordinator authority | Accumulo metadata, FATE state, WALs, and RFiles |

Kubernetes already has a consensus system behind its API. A Shoal-only
Kubernetes deployment uses that existing service at low frequency for leases;
it does not deploy an application-visible etcd or Raft cluster. When Shoal is
connected to Accumulo, it uses the ZooKeeper ensemble Accumulo already
requires. Shoal never creates a parallel Kubernetes lease that can authorize
the same Accumulo-hosted table.

A dedicated etcd cluster is justified only if Shoal later supports a
multi-node, multi-writer standalone deployment outside both Kubernetes and
Accumulo. That is not a current requirement. A custom embedded Raft
implementation is explicitly out of scope.

## 2. Authority invariants

These invariants are release gates, not implementation preferences:

1. **One writer domain.** A logical table cannot be writable under local,
   Kubernetes, and Accumulo authority simultaneously.
2. **Immutable proof.** Every authority-sensitive work unit is bound to an
   authority token that can be rejected after lease or session loss.
   Manager/coordinator work carries it explicitly; protocols such as Accumulo
   ingest that have no token field bind the update session and hosted-tablet
   instance to the current token server-side.
3. **Monotonic epochs.** A domain activation or authority handoff cannot reuse
   a logical-table epoch previously allowed to mutate state. Process
   reconnection establishes new backend proof (lock generation and operation
   attempt), not a new table-wide epoch, and never revives work issued under
   expired proof.
4. **Fail closed.** If a process cannot prove current authority, it withdraws
   readiness, rejects new mutations, and cannot publish asynchronous results.
5. **Bounded quorum state.** Coordination stores contain membership, leases,
   service descriptors, and small fencing records. WALs, table manifests,
   files, scan state, and workflow payloads remain in their authoritative data
   stores.
6. **Authority-preserving delegation.** ZooKeeper presence does not make Shoal
   the Accumulo manager. Placement, metadata mutation, imports, and online
   compaction completion still flow through manager/coordinator/FATE APIs.
7. **No clock-only fencing.** Lease timeouts detect loss, but an API-assigned
   identity and a CAS-protected generation fence writes. Wall-clock time alone
   never proves ownership.

## 3. Common vocabulary

Implementations should share a small coordination vocabulary without forcing
all backends to pretend they have identical consistency primitives:

```go
type AuthorityToken struct {
    Domain     string
    Resource   string
    Owner      string
    LeaseID    string
    Epoch      uint64
    Generation uint64
    Attempt    string
}

type Coordinator interface {
    Acquire(context.Context, string) (Lease, error)
    Watch(context.Context, string) (<-chan Event, error)
    Members(context.Context, string) ([]Member, error)
}

type Lease interface {
    Token() AuthorityToken
    Renew(context.Context) error
    Release(context.Context) error
    Lost() <-chan struct{}
}
```

This is a design shape, not a committed public Go API. Backend-specific proof
must remain available:

- every token includes the logical table's durable `Epoch`, which is advanced
  by compare-and-swap when activating a writer domain or handing authority to
  another domain. It is independent of backend-native generations and is the
  only value ordered across coordination domains;
- an embedded token also includes the persisted manifest generation and
  lock-file identity;
- a Kubernetes token includes a tenure identity formed from the Lease UID,
  unique holder identity, and `leaseTransitions`, plus the CAS-protected
  manifest generation. The changing `resourceVersion` is used only for
  optimistic-concurrency updates and is not carried as immutable work proof;
- an Accumulo token includes the exact ServiceLock identity and sequence plus
  the manager-issued assignment or coordinator-issued job attempt.

Embedded and Kubernetes modes keep the epoch in the CAS-protected table
manifest. Accumulo mode keeps it in a manager-owned Shoal authority record in
Accumulo authoritative metadata; changing that record requires a supported
manager/FATE operation, never a direct metadata mutation. The Accumulo adapter
must verify the record's epoch when issuing assignments or jobs. A handoff
first CAS-records a source handoff intent that serializes epoch changes while
source epoch `E` may remain writable. It then fences the destination and
reserves epoch
`N = max(source high-water mark, destination high-water mark) + 1`, freezes
source epoch `E` under the same intent while verifying `N` is still greater
than the source high-water mark, and retires `E` before activating destination
epoch `N`. Native manifest generations, Kubernetes tenure identities, and
ServiceLock sequences remain backend proof and are never compared with one
another.

The `Attempt` component prevents a delayed completion from applying to a later
operation that happens to run under the same process lease. This matches the
tablet-hosting attempt fence in
[`tserver-hosting-lifecycle.md`](./tserver-hosting-lifecycle.md).

## 4. State placement

Quorum availability and durable-data availability are different concerns.
Shoal must not make a coordination service carry data-plane load.

| State | Correct home |
| --- | --- |
| Process membership and endpoint descriptors | ZooKeeper ServiceLock or Kubernetes Lease-associated discovery |
| Leader/active-writer lease | Current domain's coordinator |
| Logical-table authority epoch | CAS-protected manifest in embedded/Kubernetes mode; manager-owned Shoal authority record in Accumulo metadata |
| Terminal handoff record | Source and destination authority records, linked by epoch and immutable lineage |
| Local mutations and recovery | WAL plus manifest/file generations |
| Accumulo tablet placement and files | Accumulo manager, metadata, and FATE |
| Online compaction lifecycle | Accumulo coordinator/manager protocol |
| Promotion payload | Immutable export files, checksums, and manager FATE operation identity |
| Scan sessions and query intermediates | Owning process with bounded expiry; never ZooKeeper or Kubernetes Lease data |

## 5. Failure behavior

### 5.1 Embedded process

The process acquires exclusive OS/PVC ownership before opening a writable
engine. Every durable manifest replacement increments a generation with atomic
replacement or backend compare-and-swap. A second process cannot open the same
writer domain merely because it can read the files.

### 5.2 Shoal-only Kubernetes

Use the standard Kubernetes leader-election/Lease protocol. A pod that cannot
renew before the configured deadline:

1. withdraws readiness;
2. stops admitting mutations and authority-sensitive background work;
3. prevents completions carrying the lost token from publishing;
4. drains or aborts in-flight work within a bounded period; and
5. requires a fresh Lease observation and a new durable generation before it
   can become writable again.

An API-server partition is therefore an authority failure even if the pod can
still reach its PVC or object store. The durable manifest CAS protects against
an old pod racing a replacement after connectivity returns.

### 5.3 Accumulo-connected processes

Production Accumulo should retain its normal odd-sized ZooKeeper ensemble
(typically three or five members with failure-domain separation). A
single-member ensemble is acceptable only for local development.

ZooKeeper session expiration is an immediate fence. A Shoal tserver drops its
hosted claims, a compactor cannot commit an old result, and a reconnected
process receives a new ServiceLock generation. The exact lock generation,
manager identity, and per-operation attempt travel with work; seeing the same
tablet or job identifier later is not sufficient authority.

ZooKeeper is not used as a replacement for Accumulo metadata or FATE. In
particular, promotion never writes metadata or ZooKeeper directly, and an
online compactor never commits file references without the authoritative
manager/coordinator boundary.

## 6. Local-to-Accumulo authority handoff

Promotion is a transfer of authority, not data copying followed by eventual
guesswork. The durable state machine is:

```text
LOCAL_WRITABLE
    -> HANDOFF_INTENT
    -> DESTINATION_FENCED
    -> LOCAL_FROZEN
    -> FATE_ALLOCATED
    -> IMPORT_SUBMITTED
    -> IMPORT_VERIFIED
    -> LOCAL_RETIRED
    -> ACCUMULO_WRITABLE
```

The handoff proceeds as follows:

1. CAS-record `HANDOFF_INTENT` in the source authority record, capturing source
   epoch `E` and its high-water mark while serializing other authority
   activations and handoffs. Local mutations may continue under `E` until the
   later freeze CAS.
2. Resolve the destination table through Accumulo and establish a
   manager-authoritative write fence before freezing source authority. The
   normal gate is the table's `OFFLINE` state, changed through supported
   Accumulo table operations; the destination must remain unable to accept
   ordinary mutations for the rest of the handoff. It must either be newly
   created offline and never enabled, or have all prior writers retired and
   their lineage reconciled before this protocol begins. Persist the
   destination table ID and gated state as `DESTINATION_FENCED`, reconcile both
   epoch histories, then reserve logical authority epoch
   `N = max(source high-water mark, destination high-water mark) + 1` in the
   destination's manager-owned authority record without making the table
   writable.
3. Fence the local writer with its current lease and manifest generation.
   The freeze CAS must match the source handoff intent and epoch `E`, and verify
   `N` is still greater than the current source high-water mark. If any check
   fails, keep the destination fenced, abort its reservation, and restart
   reconciliation with a newly chosen epoch.
4. Stop admitting local writes and durably record `LOCAL_FROZEN` at epoch `E`.
5. Flush/checkpoint an immutable generation and verify its checksums.
6. Stage the export, call `beginFateOperation`, and durably record the returned
   FATE identity as `FATE_ALLOCATED` **before** calling
   `executeFateOperation(TABLE_BULK_IMPORT2, ...)`.
7. Submit that exact transaction and reconcile all timeout, restart, and
   failover outcomes by its persisted FATE identity. Recovery must never begin
   a replacement transaction merely because execution returned ambiguously,
   and must not finish the FATE transaction until its terminal result is
   durably recorded.
8. Verify the imported files and tablet state through Accumulo while the
   destination write fence remains held.
9. Revoke local authority and durably record `LOCAL_RETIRED`, linking source
   epoch `E` to the destination's reserved epoch `N`.
10. While the destination remains `OFFLINE`, durably transition its
    manager-owned authority record from reserved to active epoch `N` and verify
    that record.
11. Bring the destination online through the manager and verify the transition.
    Only then may distributed clients admit writes.
12. Retain an auditable lineage record tying local generation, export
    checksums, destination table, FATE identity, and terminal authority epoch.

`managerclient.DurableFateAdapter` and Accumulo's
`AllocateBulkImport`/`SubmitBulkImport`/`WaitBulkImport`/`FinishBulkImport`
separate allocation from submission. `promotion.Machine` persists the
allocated identity before execute, records the terminal result before finish,
and carries immutable authority tokens through the handoff. The legacy
`Promote`/`BulkImport` helper remains a one-shot staging API; concrete
embedded and manager-owned authority-record adapters are still required
before this protocol is exposed as an operational cutover.

Before import submission, a failed attempt may return to `LOCAL_WRITABLE` only
after durably marking the destination's reserved epoch `N` aborted while the
destination remains `OFFLINE`, then using a successful source CAS to activate
epoch `M = max(source high-water mark, destination high-water mark) + 1`, where
`M > N`. The destination fence is not released during rollback; it may be
released only by a later handoff that retires local authority. After FATE
allocation, local writes remain frozen and the destination remains fenced until
the persisted transaction is reconciled.
Bringing the destination online administratively during this interval is a
protocol violation and must fail readiness/recovery rather than be accepted as
a successful cutover. Guessing that the import failed could authorize both
domains.

No step grants Kubernetes and Accumulo concurrent write authority. Ongoing
fan-in is a new immutable transfer from a new source generation; it is not a
long-lived second writer.

## 7. Implementation order

The dependency order is maintained in issue #128:

1. **Architecture contract** — this document and links from existing designs.
2. **Common vocabulary and embedded fencing** — authority token, conformance
   suite, local lock, and durable manifest generation.
3. **Accumulo adapter** — complete #67 ServiceLock integration and carry its
   tokens through manager/coordinator work.
4. **Kubernetes adapter** — Lease membership/election and CAS manifest fences
   for Shoal-only pods.
5. **Promotion handoff** — extend #70 with the durable state machine and
   manager FATE reconciliation above.
6. **Operations and release gates** — extend #71 and #74 with diagnostics,
   partitions, session loss, rolling upgrades, and handoff crash tests.
7. **Dependent data planes** — unblock #69 and #72 only after their authority
   prerequisites pass.

Each phase must ship as independently tested slices. A coordinator abstraction
alone does not satisfy a phase; the loss, restart, partition, and stale-token
tests are part of the deliverable.

## 8. Rejected alternatives

### Mirror every Accumulo lock into Kubernetes

Rejected because two independently available control planes can disagree.
There is no atomic transaction spanning ZooKeeper and the Kubernetes API, so a
network partition can make both sides believe they own the table.

### Store table manifests in Lease objects

Rejected because Leases are small, high-level coordination records. Using the
Kubernetes API as a table metadata database couples data-plane progress to API
availability and creates unbounded control-plane load.

### Run an embedded Raft group in every Shoal deployment

Rejected because it adds membership changes, snapshots, backup, upgrades,
quorum-loss recovery, and split-brain handling where Kubernetes or Accumulo
already supplies a supported coordination service.

### Let promotion accept writes on both sides and reconcile later

Rejected because timestamp, delete, visibility, and derived-index semantics do
not provide a general conflict-free merge. Explicit freeze and fenced cutover
are required.
