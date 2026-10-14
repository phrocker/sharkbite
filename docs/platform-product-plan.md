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
# Shoal graph and document platform plan

> **Status:** accepted direction, August 2026.
>
> This document supersedes the gRPC-sidecar-only deployment decision in
> `ai-knowledge-graph.md`. It does not change Shoal's Accumulo compatibility
> contract.

## North star

**Shoal is the local-to-Accumulo knowledge plane for agent systems.** It turns
source data into versioned, attributable, queryable evidence and exposes the
same graph, document, and retrieval semantics embedded or at cluster scale.

Agent harnesses own model invocation, prompts, tool loops, workflow scheduling,
approvals, and token budgets. Shoal owns the durable knowledge those harnesses
read and write. Agent-facing APIs therefore operate on documents, sections,
spans, graph neighborhoods, evidence, and explained retrieval plans—not raw
cells, iterator stacks, storage files, or tablet locations.

## Decision

Shoal is the independently deployable data and query platform. Hosted control
planes and reference applications are optional consumers, not runtime
dependencies.

Shoal owns:

- the sorted key/value engine, tablets, WAL, RFiles, compaction, and storage
  backends;
- Accumulo-compatible discovery, Thrift, RFile, visibility, and iterator
  behavior;
- schema-agnostic term, vector, graph traversal, aggregation, scoring, and
  time-travel pushdowns;
- stable graph and document data contracts;
- coarse graph/document query services;
- local and distributed execution behind one client API.

Shoal does not require an external provisioner, Java service, web application,
or managed-cluster lifecycle. Deployments that need governance, budgets,
multi-tenant hosting, extraction workers, billing, or an operator UI may run
those features above Shoal.

This boundary is a roadmap filter: model execution and generic agent
orchestration do not enter the Shoal engine or its required services.

## Deployment and language boundaries

Python applications receive one logical API with two transports:

```text
                        Python Shoal SDK
                         /             \
                shoal.open(path)   shoal.connect(endpoint)
                       |                    |
                gopy CPython module       gRPC
                       |                    |
                embedded Go engine    Shoal service/router
                         \             /
                    shared Go facade and semantics
```

### Embedded mode

`gopy` generates a CPython extension over a narrow, public Go facade. It is for
single-process local use: notebooks, desktop tools, tests, and hierarchical
local document workflows.

The binding must expose coarse, concrete operations. It must not expose
`context.Context`, channels, callbacks, interfaces, protobuf messages,
iterators, or internal cell representations. Those types make generated
bindings fragile and couple Python callers to implementation details.

The generated extension is wrapped by handwritten Python code that provides
Pythonic models, exceptions, context managers, and asynchronous convenience
methods. The generated API is not itself the supported public Python API.

### Remote and clustered mode

gRPC is Shoal's northbound service contract for applications. Calls are
streaming or batched and operate on documents, graph neighborhoods, query
plans, or mutation batches rather than making one RPC per cell.

gRPC does not create distributed correctness by itself. Scale comes from
tablet placement, metadata consistency, split/merge behavior, WAL durability,
failure detection, compaction scheduling, object-storage coordination, and
request routing.

### Accumulo integration

Shoal retains Accumulo-native protocols at the storage boundary:

- Accumulo Thrift for compatible scan and write paths;
- ZooKeeper and metadata-table discovery where required;
- byte-compatible RFiles for direct reads, import/export, and compaction;
- Accumulo authorizations and column-visibility semantics.

An application may use gRPC to call Shoal while Shoal uses Accumulo-native
protocols underneath. Replacing the Accumulo-facing boundary with gRPC would
reduce interoperability and is not part of this plan.

## Public package boundary

Most current capabilities live under `internal/`. Before generating Python
bindings or promising a Go API, introduce deliberately small public packages:

```text
pkg/shoal/       engine lifecycle, tables, transactions, and shared errors
pkg/code/        source identity, parser provenance, ASTs, symbols, and ingestion
pkg/graph/       nodes, edges, graph mutation, expansion, and query requests
pkg/document/    documents, sections, spans, trees, and page retrieval
pkg/retrieval/   lexical, vector, tree, and hybrid retrieval plans
bindings/python/ gopy input facade and generated build configuration
python/src/shoal/ supported Python package and transport selection
```

Public types are transport-neutral. Embedded and gRPC implementations must
pass the same contract suite. The public API must not leak row keys, column
families, iterator options, or protobuf-generated types unless a caller opts
into an explicitly low-level package.

## Current product gaps and improvements

Shoal already has the underlying graph, document-index, vector, term,
visibility, provenance, and time-travel primitives. The missing product layer
is a stable, explainable model that composes those primitives around how people
and agents explore source material.

| Current gap | Improvement |
| --- | --- |
| Source code lacks a parser-neutral ingestion boundary | Add exact source identity, AST, symbol, relationship, diagnostic, and idempotency contracts |
| Documents are addressable as indexed fields but lack a natural navigation contract | Add typed document, section, span, entity, and event objects |
| Graph traversal does not preserve an author's document hierarchy | Represent the document tree inside the wider graph |
| Index construction is not a public workflow | Define pluggable Python/Go indexers that write one versioned contract |
| Retrieval mechanisms are exposed separately | Compose tree, lexical, vector, graph, and hybrid retrieval plans |
| Results do not consistently identify exact source locations | Return page/span citations and the complete traversal path |
| Cross-document and within-document navigation are separate concerns | Link sections and entities across documents without losing parent/child order |
| Temporal, causal, semantic, and entity edges lack one public query model | Expose optional typed edge views through the graph facade |
| Security and history are low-level storage features | Carry visibility, revision, provenance, and as-of semantics through document APIs |
| Local and cluster clients use different operational shapes | Provide one client contract over native embedded and remote gRPC transports |
| Query execution is difficult to inspect | Return bounded query traces with anchors, pushdowns, scores, and hydration steps |

A document outline gives an agent a small, human-readable search space:
titles, summaries, hierarchy, and page ranges. A multi-relational graph adds
relationships, history, entities, causal/temporal views, and evidence across
many sources. The target product combines these into a hierarchical document
graph:

```text
Document
  -> Section
       -> Section
            -> PageSpan
                 -> source text/image coordinates

Section -> Entity
Section -> Section       cross-reference or semantic relation
Entity  -> Entity
Event   -> Section       evidence
Event   -> Event         temporal or causal relation
```

Tree edges preserve the author's organization. Graph edges add cross-document
and temporal context. Neither replaces the other.

## Code and AST ingestion contract

Source code ingestion is a first-class public workflow, not a parser-specific
extension of document indexing. `pkg/code` defines immutable, transport-neutral
values for:

- repository, ref, repository-relative path, immutable revision, exact content
  hash, and byte length;
- language identity plus parser name, version, and configuration provenance;
- half-open source ranges with zero-based byte offsets and one-based line and
  byte-column coordinates;
- ordered syntax nodes, semantic symbols, external entities, diagnostics, and
  typed `import`, `call`, `reference`, and `contains` relationships;
- zero-based occurrences assigned by actual declared root/child preorder, with
  an independent counter for each source, kind, and exact-range group;
- parse results constructed and validated against exact parse-request bytes;
- idempotent ingestion requests bound to that validated request/result pair.

Parser implementations remain adapters outside the contract package. The
contract does not select a parser library, require one AST shape, or expose
parser-native objects. Materialization adapters may later map the same parse
result into `pkg/document` source evidence and `pkg/graph` nodes and edges
without changing parser output.

Code ingestion is accepted only when all of these gates pass:

1. **Identity gate:** source, syntax, symbol, external-entity, relationship,
   and ingestion IDs use reserved namespaces and typed canonical derivation;
   callers cannot assert IDs in those namespaces. IDs are deterministic and
   collision-checked within a result. Syntax-node occurrences must equal the
   current source/kind/range counter during actual declared root/child preorder
   traversal. Semantic-symbol occurrences remain contiguous from zero within
   each source/kind/range group.
2. **Source gate:** repository, ref, path, revision, content hash, and byte
   length are required; canonical source identity includes both hash and byte
   length, and parse bytes must match both.
3. **Range gate:** every node, symbol, relationship location, and diagnostic
   range is valid and source-bounded. Validation derives line starts from the
   exact bytes and verifies every byte/line/UTF-8-byte-column coordinate before
   checking child order, parent containment, or symbol containment.
4. **Tree gate:** roots are complete, each non-root node has exactly one parent,
   children are non-overlapping and source-ordered, and cycles or unreachable
   nodes fail explicitly. Identical ranges are ordered by the canonical
   semantic tuple of kind and occurrence, never by an ID digest.
5. **Relationship gate:** only the four declared kinds are accepted, endpoint
   kinds must match the matrix below, and every typed endpoint resolves to the
   declared source, syntax node, semantic symbol, or external entity.
6. **Idempotency gate:** ingestion first runs `ParseResult.ValidateFor` against
   its retained `ParseRequest`. Equal request/result provenance produces the
   same ingestion key; retries never create duplicate document or graph
   artifacts, while a content, byte-length, revision, language, parser, or
   parser-configuration change produces a different key.
7. **Boundary gate:** the public contract has no concrete parser, storage,
   protobuf, or transport dependency. Extensible constructors use functional
   options instead of public aggregate specification structs.

The initial relationship endpoint matrix is closed:

| Relationship | Allowed source endpoints | Allowed target endpoints |
| --- | --- | --- |
| `import` | source, syntax | external |
| `call` | syntax, symbol | symbol, external |
| `reference` | syntax, symbol | symbol, external |
| `contains` | source, syntax, symbol | syntax, symbol |

Custom relationship kinds require a future contract revision and are rejected
by this version. An unresolved or not-yet-materialized import is represented by
an `ExternalEntity`; a source endpoint is not a valid import target.

## Document-tree contract

The first document model should remain ordinary Shoal cells and should be
expressible through public types rather than hard-coded engine behavior.

Required logical objects:

- `Document`: id, title, media type, source URI, checksum, metadata, and
  visibility;
- `Section`: stable id, document id, parent id, ordinal, depth, title, summary,
  and optional token count;
- `Span`: document id, section id, page start/end, character or geometry
  bounds, extracted text, and source coordinates;
- typed edges: `child`, `next`, `references`, `mentions`, `semantic`, and
  consumer-defined relations;
- index records for title terms, body terms, summaries, embeddings, and page
  ranges.

Stable section ids must survive summary regeneration. Re-indexing the same
document checksum must be idempotent. A new source version creates a new
document revision and remains queryable with Shoal's as-of semantics.

Shoal stores and queries this contract. PDF parsing, OCR, layout detection,
summary generation, and model-provider integration remain pluggable indexing
components. The initial implementation may use a Python-based pipeline without
porting that ecosystem into the Go engine.

## Retrieval model

Expose retrieval as a plan with independently selectable stages:

1. **Scope:** tenant, corpus, documents, visibility, revision/as-of time.
2. **Anchor:** tree root, terms, field predicates, vectors, entities, or ids.
3. **Navigate:** child/parent traversal, graph expansion, references, or
   temporal/causal edges.
4. **Rank:** structural relevance, lexical score, vector score, graph score,
   freshness, or a caller-supplied blend.
5. **Hydrate:** section summaries, source spans, neighboring context, and page
   images/text.
6. **Explain:** return selected nodes, scores, traversal path, source
   coordinates, and revision identifiers.

LLM-guided tree search belongs in the retrieval layer, not in the storage
iterator runtime. It should choose among coarse operations such as
`ListChildren`, `SearchSections`, `GetSection`, `Expand`, and `GetSpans`.
Shoal executes each operation close to the data and returns bounded results.

## Graph and document explorer

The explorer is an evidence-first interface, not a generic node-link
visualizer. Its initial workspace has coordinated views:

- **Document tree:** titles, summaries, page ranges, search hits, and current
  traversal path.
- **Source viewer:** page text or rendered page with exact cited spans.
- **Graph neighborhood:** entities, cross-references, semantic links,
  temporal/causal edges, and configurable hop limits.
- **Inspector:** properties, visibility, revision history, provenance,
  embeddings/index status, and raw query scores.
- **Query trace:** anchor sources, pushed-down operations, ranking stages,
  selected evidence, latency, and result counts.

Selecting an item in any view highlights the same object and evidence in all
other views. Every generated answer or reasoning path can be inspected back to
document revision and source coordinates.

The explorer consumes the public graph/document API. It must not issue raw
cell scans or know physical column-family conventions.

## Capability placement

Place capabilities according to their abstraction level:

| Capability | Destination |
| --- | --- |
| Source identity, ASTs, symbols, diagnostics, and code ingestion | `pkg/code` |
| Code-to-document and code-to-graph materialization | Adapters over `pkg/code`, `pkg/document`, and `pkg/graph` |
| Generic graph mutations and reads | `pkg/graph` |
| Neighbor expansion and aggregation | Shoal query facade over existing pushdowns |
| Document/section ingestion contract | `pkg/document` |
| Graph/vector/term query composition | `pkg/retrieval` and ShoalQL |
| Snapshot/distillation algorithms | Optional graph analytics package or worker |
| PDF/OCR/entity/summary workers | Pluggable indexer processes, initially Python |
| Governance, budgets, agent authority | Optional higher-level package/control plane |
| Hosted tenant provisioning and billing | Optional deployment product |
| Document and graph exploration | New explorer over the public Shoal API |

Do not port an existing large HTTP handler surface verbatim. Start from use
cases and define coarse contracts. Preserve algorithms and behavioral tests
where they add value, but remove assumptions about a mandatory managed
deployment.

## Relationship to the existing roadmap

This plan extends the existing execution-plane roadmap; it does not replace or
duplicate it.

| Existing work | Role in this plan |
| --- | --- |
| [#59](https://github.com/phrocker/shoal-oss/issues/59) client and Python compatibility | Completes the Accumulo connector deliverable and its release-quality language bindings |
| [#65](https://github.com/phrocker/shoal-oss/issues/65) online compactor | Completes an Accumulo execution-plane role |
| [#70](https://github.com/phrocker/shoal-oss/issues/70) local-to-Accumulo promotion | Supplies the authority-safe transition behind one local/distributed client API |
| [#72](https://github.com/phrocker/shoal-oss/issues/72) unified ShoalQL execution | Shipped foundation for format-neutral local and Accumulo query semantics |
| [#73](https://github.com/phrocker/shoal-oss/issues/73) RFile/Parquet policy | Shipped current boundary: Parquet is local or derived; Accumulo tablet metadata remains RFile-only |
| [#74](https://github.com/phrocker/shoal-oss/issues/74) replacement conformance | Release gate for every distributed capability advertised by this plan |
| [#75](https://github.com/phrocker/shoal-oss/issues/75) execution-plane roadmap | Parent program for Accumulo-compatible runtime roles |
| [#96](https://github.com/phrocker/shoal-oss/issues/96) cluster status API | Supplies connector and operator observability needed by remote clients |
| [#128](https://github.com/phrocker/shoal-oss/issues/128) coordination authority | Defines fencing and the single-authority invariant for local, Kubernetes, and Accumulo modes |
| [#218](https://github.com/phrocker/shoal-oss/issues/218) trusted publishing | Existing release gate for the compatibility Python distribution |

Closed roadmap issues remain architectural foundations and test assets; they
must not be re-created under new names. An issue being closed does not by
itself permit a stronger public claim than its acceptance evidence supports.

### Dependency gates

Work may proceed in parallel, but public promises are gated:

1. **Code ingestion gate:** all identity, source, range, tree, relationship,
   idempotency, and boundary gates in the code ingestion contract must pass
   before parser output is advertised as a stable public input.
2. **Local library gate:** the public facade and embedded contract suite must
   pass before `shoal.open()` is supported.
3. **Remote API gate:** coarse gRPC contracts and transport parity must pass
   before `shoal.connect()` is supported.
4. **Cluster correctness gate:** #128 authority invariants and #74 role
   conformance must pass before a multi-node deployment is advertised as
   production-ready.
5. **Promotion gate:** #70 must pass graph, document, term, vector, visibility,
   delete, timestamp, retry, and cutover fixtures before one client may switch
   a logical table from local to Accumulo-backed execution.
6. **Connector gate:** #59 and its compatibility matrix determine which
   Accumulo client surfaces are advertised; missing rows cannot be hidden by
   the higher-level graph/document API.
7. **Parquet gate:** the #73 policy remains authoritative until Accumulo gains
   an explicit file-format capability. Parquet must not appear in Accumulo
   tablet metadata under the current protocol.

### Parquet and Accumulo evolution

Shoal's public graph/document API is format-neutral. The storage layer exposes
an explicit authoritative or derived role for every RFile and Parquet
materialization.

Native Parquet-backed Accumulo tablets are a separate compatibility program,
not an implicit extension of the current connector. That program requires:

- an Accumulo file-format capability and metadata contract;
- format-aware scan, bulk import, split estimation, minor/major compaction,
  recovery, upgrade, and rollback behavior;
- mixed-version assignment rules that prevent an unsupported server from
  receiving a Parquet-backed tablet;
- cell-equivalence fixtures for visibility, versions, deletes, ranges,
  locality, and iterator behavior;
- an update to #73's policy only after the Accumulo boundary is accepted and
  its conformance gate passes.

Until then, promotion converts Parquet authoritative data to RFile or rebuilds
the required distributed indexes, retaining lineage and checksums.

## Actionability contract

The roadmap is actionable only when documentation, GitHub issues, tests, and
release claims agree. Apply these rules:

1. Every delivery phase has one `roadmap` or `agent:epic` issue.
2. Every implementation slice is a separate issue small enough for one pull
   request and names its parent epic.
3. Every issue states current behavior, exact scope, dependencies, non-goals,
   acceptance criteria, and required validation.
4. Dependencies are written in both parent and child issue bodies. A blocked
   item receives `agent:blocked`; a parallel-safe item receives `agent:ready`.
5. Pull requests close implementation issues, not umbrella epics. An epic
   closes only after all required children and release evidence are complete.
6. Contract tests run unchanged against embedded and gRPC transports. Cluster
   tests add Accumulo and mixed-fleet backends rather than replacing local
   coverage.
7. Format-neutral query fixtures run against RFile, Parquet, mixed local
   materializations, and—where supported—Accumulo.
8. CI emits machine-readable capability verdicts. Documentation and binaries
   may advertise only capabilities with passing verdicts.
9. Any unsupported iterator, format, authority mode, or compatibility surface
   fails explicitly with a stable reason; no success-shaped fallback is
   allowed.
10. The platform plan is reviewed whenever an epic closes or a storage,
    authority, protocol, or public-API contract changes.

### Product work-item decomposition

Create implementation issues from this plan in the following dependency order.
The titles are intentionally concrete so issue creation does not require
reinterpreting the architecture:

| Work item | Depends on | Exit evidence |
| --- | --- | --- |
| Public transport-neutral facade and error model | Existing embedded engine and connector APIs | Go compile tests; no `internal`, protobuf, iterator, or cell types in the public contract |
| Parser-neutral code and AST ingestion contract | Public facade | Typed-ID, source/hash/length/revision, request-bound exact-coordinate, tree-order, endpoint-matrix, and idempotency fixtures; no concrete parser dependency |
| Code materialization adapters | Code contract plus graph/document contracts | The same parse result produces revision-specific document evidence and graph entities without duplicate artifacts on retry |
| Embedded graph/document adapter | Public facade | Shared contract suite passes against an in-process engine |
| Coarse graph/document gRPC service | Public facade | Shared contract suite passes against loopback gRPC; cancellation and streaming limits covered |
| Versioned document/section/span persistence | Embedded adapter | Idempotent ingest and revision/as-of fixtures |
| Tree navigation and bounded source hydration | Document contract | Parent/child/order and exact page/span citation fixtures |
| Unified retrieval plan and explanation model | #72 foundation plus document tree | Tree, lexical, vector, graph, hybrid, and `EXPLAIN` fixtures |
| Local Python native facade | Stable public facade | Pinned `gopy`; CPython 3.11/3.12 Windows/Linux lifecycle and memory tests |
| Python remote transport | Coarse gRPC service | Same Python behavioral suite against native and gRPC transports |
| Local-to-cluster logical client transition | #70, #74, #128 | Crash-safe cutover and cell-equivalence fixtures |
| Evidence-first graph/document explorer | Stable graph/document API and explanation model | Coordinated tree, source, graph, inspector, and query-trace acceptance tests |
| Accumulo native Parquet design and prototype | #73 policy and upstream Accumulo design acceptance | Separate ADR, compatibility matrix, mixed-version fixtures, and no weakening of the current RFile gate |

## Delivery sequence

### Phase 0: contracts and parity

1. Create the public Go facade and shared error model.
2. Define transport-neutral code, graph, and document request and response
   types.
3. Implement embedded and gRPC adapters.
4. Run one contract suite against both transports.

This phase may proceed while execution-plane work continues. It cannot claim
production clustered operation until the remote, coordination, and conformance
gates above pass.

### Phase 1: code and AST ingestion

1. Establish `pkg/code` source, provenance, range, AST, symbol, relationship,
   diagnostic, parse, and ingestion contracts.
2. Add parser adapters in separate packages only after the public contract and
   acceptance fixtures are stable.
3. Materialize revision-specific source evidence through `pkg/document` and
   semantic entities and relationships through `pkg/graph`.
4. Run the same idempotency and structural validation fixtures against every
   ingestion adapter.

### Phase 2: document tree

1. Implement `Document`, `Section`, and `Span` persistence.
2. Add idempotent document revision ingestion.
3. Build tree navigation and bounded span hydration.
4. Add a Python indexer adapter for hierarchical PDF structure output.

### Phase 3: unified retrieval

1. Add tree, lexical, vector, and graph anchors to one retrieval plan.
2. Return explainable scores and traversal paths.
3. Add LLM-guided tree navigation as an optional Python strategy.
4. Benchmark retrieval quality, latency, token use, and bytes transferred
   against vector-only, tree-only, and multi-relational graph baselines.

Reuse #72's format-neutral planning and execution work. Add only the public
retrieval and explanation contracts that are not already covered there.

### Phase 4: Python distribution

1. Create the narrow `gopy` facade.
2. Pin a tested `gopy` revision rather than relying on an unbounded latest
   build.
3. Test Go 1.25 with CPython 3.11 and 3.12 on Windows and Linux.
4. Stress repeated open/close, concurrent calls, exceptions, large byte
   buffers, interpreter shutdown, and memory ownership.
5. Publish wheels only after the native and gRPC implementations pass the same
   behavior suite.

Python 3.13 free-threading is not an initial support target. The SDK can always
use gRPC when a supported native wheel is unavailable.

This is a separate high-level Shoal package. It does not replace the
Accumulo-compatibility Python distribution tracked by #59 and #218; the two
packages may share release infrastructure but have different compatibility
contracts.

### Phase 5: explorer

1. Ship document tree and source views.
2. Add graph neighborhood and evidence-path views.
3. Add query traces and revision/provenance inspection.
4. Add administrative views only after the retrieval workflow is coherent.

## Success criteria

- The same Python retrieval code works with `shoal.open()` and
  `shoal.connect()`.
- Local mode requires no separate service.
- Cluster mode makes no cell-at-a-time network calls.
- Existing Accumulo RFiles and visibility rules remain compatible.
- A code parse identifies the exact repository revision and content hash,
  parser provenance, byte/line/column ranges, symbols, diagnostics, and
  relationship endpoints.
- Repeating one code ingestion key returns the same document and graph artifact
  identities without duplicate materialization.
- A document answer identifies document revision, section, pages/spans, and
  the retrieval path that selected them.
- Tree-only, vector-only, and graph-only retrieval remain available so hybrid
  behavior can be measured rather than assumed.
- No external managed control plane is required to install, run, or scale
  Shoal.
