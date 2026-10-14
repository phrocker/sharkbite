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
# Shoal architecture

## Coordination authority

Shoal's accepted architecture assigns one coordination authority per logical
table: local process/PVC fencing for embedded mode, Kubernetes Leases for
Shoal-only pod groups, and Accumulo's ZooKeeper ServiceLocks plus
manager/coordinator authority when attached to Accumulo. These authorities
must never be active concurrently for the same table. The adapters and fencing
are tracked implementation work, not behavior supplied by the current
binaries. The quorum, fencing, failure, and local-to-Accumulo handoff model is
documented in
[`docs/coordination-authority.md`](docs/coordination-authority.md) and tracked
by [#128](https://github.com/phrocker/shoal-oss/issues/128).

Hosted Accumulo tablets use the separate, fenced WAL lifecycle described in
[`docs/hosted-tablet-wal.md`](docs/hosted-tablet-wal.md). It is not the
embedded `localwal` path. Its WAL retirement is gated on the crash-resumable
native-RFile metadata commit described in
[`docs/hosted-tablet-minc.md`](docs/hosted-tablet-minc.md).

## Goal
Lightweight read-only Accumulo replica. JVM-free pods that open RFiles
directly from object storage, run a minimal iterator stack, and serve
hedged single-shot `scan()` calls.

## V0 boundary

**In V0:**
- ZK + metadata-table bootstrap (Option A: from-scratch Go client, no JVM).
- RFile reader port from `core/.../file/rfile/RFile.java`, with two
  sharkbite-cribbed optimizations: async block readahead, in-block visibility
  filtering (skip cells before decompression).
- VisibilityFilter only — no other iterators.
- Thrift `TabletScanClientService.startScan` only — no `startMultiScan`,
  `continueMultiScan`, or session state. Each scan is self-contained.
- Per-replica LRU block cache. Per-replica is non-negotiable; shared cache
  defeats hedged reads.
- Java-side hedge coordinator (lives in the calling application; the
  shoal pod itself is unaware of hedging).

**Out of V0:** all other iterators (V1–V4 in the staging plan), FRESH-tier
reads, server-side BatchScanner (range fan-out happens client-side in the
SDK), HTTP frontend, full Prometheus integration, sharkbite's Python
lambda-iterator path.

## Bootstrap chain (Option A)

1. ZK → `/accumulo/instances/<name>` → instance UUID.
2. ZK → `/accumulo/<uuid>/root_tablet` → JSON `RootTabletMetadata` →
   tserver host:port hosting the root tablet.
3. Thrift `scan()` against root tablet → metadata-tablet locations + their
   files.
4. Thrift `scan()` against metadata tablets → user-table tablet→file map.
5. Cache; refresh by exception (sharkbite-style), not TTL.

## Wire protocol

Apache Thrift, generated with compiler **0.17.0** from the vendored Accumulo 4
IDL snapshot in `internal/thrift/idl`. The source and compiler revisions are
documented there, `make thrift-check` enforces the compiler version, and
`make thrift-verify` checks the internal generated bindings for drift.

Transport: `TFramedTransport`. Protocol: `TCompactProtocol` wrapped in a
custom `AccumuloProtocol` that prepends/validates a header on every
message. Header (encoded through TCompactProtocol's writers, not raw
bytes): `i32` magic `0x41434355` ("ACCU") + `byte` protocol version (1) +
`string` Accumulo version (major.minor must match server) + `string`
instance ID (must match server's). Server validates all four fields and
errors on any mismatch. See `core/.../rpc/AccumuloProtocolFactory.java`.
Sharkbite predates this header and does not implement it.

## Caching strategy

Tablet→location and tablet→file maps cached in-process. Invalidate on
Thrift errors that indicate stale state (NotServingTabletException,
transport-level failures) — same approach as sharkbite's `LocatorCache` +
`CachedTransport`. No TTL.

## Locked decisions (no re-litigation)

- Language: Go (qwal precedent).
- Wire: existing Accumulo Thrift, not new gRPC.
- Tablet→RFile source: metadata table (pulled, with exception-driven cache
  invalidation), NOT a ZK watch — corrected from the original v0 spec.
- Hedge coordinator: client SDK in Java provisioner.
- Cache: per-replica LRU.
- No FRESH tier.

# Shoal compactors

Shoal's second-half deliverable is a JVM-free compaction implementation.
Read-fleet replicas (above) handle scan-path traffic; compactors handle
the write path — merging input RFiles through an iterator stack and
emitting a single output RFile, byte-equivalent to what Java's
`FileCompactor` would have produced.

Compactors share read-fleet infrastructure (`internal/rfile`,
`internal/storage`, `internal/iterrt`) but expose different surface: a
goroutine that pulls work from the manager's compaction queue rather
than a Thrift listener that answers scans.

## Iterator runtime (`internal/iterrt`)

`SortedKeyValueIterator` is a near-1:1 Go port of Accumulo's
`org.apache.accumulo.core.iterators.SortedKeyValueIterator`. Same
contract — `Seek`/`HasTop`/`GetTopKey`/`GetTopValue`/`Next`/`Init`/
`DeepCopy` — so a Go iterator can be parity-tested against its Java
reference with no impedance mismatch. Ported iterators:

- `VersioningIterator` — newest-N per (row, cf, cq, cv).
- `DeletingIterator` — tombstone suppression; uses
  `PartialKey.ROW_COLFAM_COLQUAL_COLVIS` to skip a deleted
  coordinate's older versions.
- `VisibilityFilter` — column-visibility evaluation (also used in V0
  scan path).
- `MergingIterator` — heap-merge over N source RFiles, the bottom of
  every compaction stack.
- `LatentEdgeDiscoveryIterator` — example of a non-system, table-
  specific majc iterator that emits new cells (the "link" cells in
  the graph-vidx pattern). Carries the deterministic-emission-
  timestamp contract described below.

Stack composition mirrors Accumulo's
`IteratorConfigUtil.loadIterators`: specs are sorted ascending by
priority, then each is `Init`'d on top of the previous, so lower
priority = lower in stack = closer to source.

## Compaction stack shape

For a compactor invocation the stack is built bottom-up:

```
N source RFiles → MergingIterator → DeletingIterator → user iterators (priority asc) → output writer
```

`DeletingIterator` is auto-wrapped between the merge and the
user-configured iterators (matching Java's
`FileCompactor.compactLocalityGroup`). Without it, tombstones leak
into user iterators as live cells and divergence from Java becomes
inevitable.

## Deterministic emission contract

Any iterator that emits *new* cells during compaction (as opposed to
filtering or passing through) MUST use a deterministic timestamp
derived from its input — typically `max(source cell ts in the same
emission boundary) + 1`. Wall-clock timestamps (`time.Now()`,
`System.currentTimeMillis()`) break reproducibility: two compactions
of the same input bytes at different times would produce different
outputs, which makes parity validation impossible.

The `+1` keeps emissions strictly newer than every source cell in
the same boundary, preserving the "newest version first" invariant
the stacked `VersioningIterator` expects.

## Iterator-config resolver (`internal/shadow/itercfg`)

Reads the table's `table.iterator.<scope>.*` properties out of ZK
and materializes them into an ordered `[]IterSpec`. Layout (Accumulo
4.0):

```
/accumulo/<uuid>/config                       → site/system props blob
/accumulo/<uuid>/namespaces/<ns-id>/config    → namespace props blob
/accumulo/<uuid>/tables/<table-id>/config     → table props blob
```

Each blob is `VersionedPropGzipCodec`-encoded: `int32 version || bool
compressed || writeUTF timestamp || [gzip(int32 count, (writeUTF
key, writeUTF value)*)]`. Properties merge in inheritance order
(system → namespace → table), and ZK ACLs require digest auth with
`accumulo:<instance-secret>` for reads. Iterators referencing
classes not on the resolver's allowlist are reported as `Skipped`
rather than dropping the entire stack — operators see which iterator
a table needs ported before parity is meaningful.

# Shadow oracle

The shadow oracle is shoal's correctness gate: it runs alongside
Java compactions, runs shoal's compactor on the same inputs, and
asserts byte-equivalent output. Until the oracle is green for 24h
on a workload, the shoal compactor doesn't replace Java's.

## Tiers

- **T1**: Java's `accumulo file rfile-info` reads shoal's output.
  Tests the wire-format / RFile-writer side. Env-gated; absent
  validator = T1 not attempted, not a failure.
- **T2**: streaming SHA-256 over a canonical encoding of every cell
  (length-prefixed row || cf || cq || cv || int64-le ts || del-flag
  || value). Both shoal and Java fold cells into the hash in a
  single pass — O(1) memory per side regardless of input size.
  Equal digests prove cell-sequence equivalence by construction
  (RFile cells are sorted, so set-equal under canonical encoding ⇒
  sequence-equal).
- **T3**: vestigial. Originally a random point-lookup probe to
  catch block-index / bloom divergence the linear walk might miss.
  Subsumed by T2: if every cell at every position matches, no
  alternate read path can yield a different result. Field is
  preserved in the Report struct for wire compatibility but always
  `Attempted=false`.
- **T4**: per-file `CellsByCF` count map. Folded into the T2 pass.
  Informational — used by operators to localize divergences when
  T2 fails.

## Service shape

`cmd/shoal-compactor-shadow` runs in two modes:

- **Single-shot CLI**: explicit `(inputs, iterator-stack, optional
  java-output)` args; one Compare call; structured report to stdout
  or object storage. Used for debugging a specific divergence.
- **Service**: long-running daemon. A metadata poller watches one
  or more tables; when a tablet's `file:` set changes (a compaction
  committed), the event flows into a bounded oracle worker pool.
  Each worker fetches inputs + Java output from object storage,
  runs Compare, and uploads a JSON report under
  `<reportPrefix>/<table-id>/<unix-nanos>.json`. Prometheus-style
  metrics on `:9810/metrics` and `/metrics-json`.

## Memory shape

Shadow's per-event cost is dominated by the GCS-fetched byte
buffers (input RFiles + Java output) held in memory for re-seekable
reads. The streaming hash itself is O(1). Memory grows linearly
with the sum of input file sizes — so the service applies a
configurable cap (`maxInputBytes`, default 500 MiB) and skips
pathologically large events, surfacing them via
`shadow_inputs_oversized_total`. A future refactor that teaches
`bcfile.Reader` to ranged-read from object storage on demand would
drop the byte-buffer cost.

See [`docs/shadow-runbook.md`](docs/shadow-runbook.md) for the
operator-facing version: how to enable, what reports look like,
how to triage T1/T2 failures.
