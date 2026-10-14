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
# Design: AI-aware local knowledge graph

> **Status:** core pushdown capabilities implemented. The product and
> deployment direction is continued in
> [platform-product-plan.md](platform-product-plan.md).

## Motivation

The embedded engine (`shoal-embed`, `internal/engine`) is today a generic
Accumulo-style sorted KV store: rows of `(cf, cq, cv, ts) -> value` cells,
served over the `ShoalEmbed` gRPC service (`proto/embed.proto`) with
`CreateTable` / `Write` / `Scan` / `Flush` / `Compact` / `Status`.

Consumers that model a **graph** on top of this — nodes as rows, edges as
cells, plus derived link cells via the `graph-vidx` pattern and
`LatentEdgeDiscoveryIterator` — currently pull large row ranges to the
client and do all filtering, ranking, and traversal there. For a graph of
tens of thousands of nodes, even a single keyword lookup streams the whole
table back, because `Scan` can only bound by **row range**, not by cell
content.

The engine already contains the two primitives needed to fix this without
changing its nature:

- `internal/iterrt` — a faithful port of Accumulo's
  `SortedKeyValueIterator` stack (`Seek`/`HasTop`/`Next`/…), already used
  for versioning, deletion, visibility, and merge.
- `internal/ivfpq` — an IVF-PQ vector index.

The proposal is to expose these as **server-side query capabilities** so
that graph-shaped and vector-shaped reads execute next to the data and
return only what the caller needs. This is the same "push the iterator
down" lever Accumulo itself relies on.

## Hard requirement: additive and backward compatible

This evolution **must not** break existing deployments. Concretely:

- The current `ShoalEmbed` RPCs keep their existing semantics. New
  capabilities are exposed either as **new optional fields** on
  `ScanRequest` or as **new RPCs** — never by changing the meaning of an
  existing field.
- The on-disk RFile format stays byte-compatible. New cell conventions
  (embeddings, postings) are ordinary cells distinguished by column
  family; an engine build without the new iterators still reads and writes
  them as opaque KV.
- A consumer that ignores the new fields/RPCs continues to behave exactly
  as it does today.
- The shadow oracle (`internal/shadow`) parity gate continues to apply:
  any new compaction-time iterator that emits cells must satisfy the
  deterministic-emission-timestamp contract and pass byte-equivalence
  before it ships.

In short: existing callers keep working unchanged; the new surface is
strictly opt-in.

## Proposed capabilities

All of the following are generic KV/graph/vector primitives. The engine
defines **mechanisms**, not any consumer's domain vocabulary.

### 1. Term-index (keyword) iterator — pushdown filtering

A consumer maintains an inverted index as ordinary cells, e.g. posting
rows `idx:<term>` with one cell per matching primary row. Recall today
means: client scans `idx:<term>`, collects ids, then issues N point
lookups.

Move the join server-side: a new iterator (configured on a scan) takes a
set of term rows, walks their postings, and emits the **referenced primary
rows** directly — so one `Scan` returns the candidate nodes, not the whole
table. This is the single highest-value change; it removes the
"transfer the entire graph per query" cost measured on large graphs.

Surface: a new optional `term_filter` message on `ScanRequest` (list of
posting-row keys + the primary-row prefix to resolve against), or a
dedicated `Lookup` RPC. Either is additive.

### 2. Vector KNN scan — semantic search

Store an embedding as a cell co-located with its node row (a reserved
column family, value = packed `float32`). `internal/ivfpq` already
provides the index; expose it as a `VectorSearch` RPC (or a scan mode)
that takes a query vector + `k` and streams back the nearest node rows
with scores. Co-locating the vector with the row (rather than a sidecar
table) keeps a node and its embedding in the same tablet, so KNN results
can hydrate node attributes without a second round trip.

### 3. Embedding cells + deterministic derivation

Treat embeddings as first-class cells (reserved cf), written through the
normal `Write` path. Any embedding *derived* during compaction (e.g.
re-quantization) follows the existing deterministic-emission-timestamp
rule so the shadow oracle can prove parity.

### 4. Graph traversal & read-time scoring iterators (later)

Once (1)–(3) land, two more iterators become natural:

- **Edge-expand**: given seed rows, emit their adjacent edge/target cells
  in one scan — server-side neighborhood expansion instead of
  client-driven BFS.
- **Read-time decay/scoring**: apply a monotonic transform (e.g. a
  time-based weight) to a salience-like attribute at scan time, so callers
  can rank without writing decayed values back. This is a pure scan-path
  iterator; it never mutates stored cells.

## Layering and boundaries

- **Engine (this repo):** generic KV + tablets + iterators (term filter,
  vector KNN, edge expand, read-time scoring) + embeddings as a cell type.
  No consumer-specific schema, no domain field names baked into the wire
  format, no JVM.
- **Consumers:** define their own node/edge schema, their own column-family
  conventions, and their own ranking/consolidation policy, and compose the
  engine's iterators to execute it.

The gRPC boundary is the contract. Because consumers talk protobuf, they
remain free to be written in any language; the engine's implementation
language is invisible to them.

## Locked decisions (no re-litigation)

- **Language: Go.** The engine's mission is a JVM-free, single-static-binary,
  Accumulo-parity store. Go fits it directly (concurrency for hedged reads
  and goroutine compactors, mature gRPC/object-storage SDKs, an existing
  Accumulo-internals port validated by a byte-equivalence shadow oracle).
  The gRPC boundary already decouples the engine's language from every
  consumer, so "easier to call from language X" is not a reason to rewrite —
  it is an integration-ergonomics task (typed client, sidecar lifecycle),
  not a language change. A rewrite (e.g. Rust) would trade a large, parity-
  validated codebase for marginal tail-latency gains; embedding the engine
  in a consumer's process would couple it to that consumer and undermine the
  standalone, JVM-free mission. Keep Go.
- **Deployment shape: embedded or gRPC.** Local Python applications may load a
  narrow public Go facade through a generated CPython extension. Remote and
  clustered applications use `shoal-embed` or a Shoal router over gRPC. Both
  transports implement the same coarse graph/document contracts. Accumulo
  integration continues to use Accumulo-native Thrift, metadata, visibility,
  and RFile formats at the storage boundary.
- **No breaking changes.** Every capability here is additive (new optional
  fields / new RPCs / new reserved column families). Existing deployments
  are unaffected.

## Non-goals

- No domain model, schema, or business logic in the engine.
- No replacement of the existing scan/write/compact contract.
- No direct binding to iterator, cell, protobuf, or other internal engine
  types. In-process bindings expose only the stable public facade.
- No JVM dependency.

## Suggested first slice

Implement capability (1), the **term-index iterator**, end to end:
an additive `ScanRequest` field (or `Lookup` RPC), an `internal/iterrt`
iterator that resolves postings to primary rows, and a parity test. It
directly removes the measured "transfer the whole graph per query"
bottleneck and validates the pushdown architecture before the vector and
traversal work builds on it.
