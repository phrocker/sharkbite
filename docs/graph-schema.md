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
# Agent knowledge graph schema

`internal/graphschema` defines shared byte conventions for modeling an
agent knowledge graph on top of shoal's generic `row -> (cf,cq,cv,ts) -> value`
cells. It is a consumer library only: the engine and `internal/iterrt` remain
schema-agnostic.

## Cell layout

| Purpose | Row | Column family | Column qualifier | Value |
| --- | --- | --- | --- | --- |
| Event node | `evt:<id>` | `content:` | consumer-defined | event text |
| Event/entity embedding | `evt:<id>` or `ent:<name>` | `vec:` | consumer-defined | packed big-endian `float32` vector |
| Attribute | any node row | `attr:` | attribute key | attribute value |
| Temporal edge | source node row | `edge.temp:` | neighbor/target id | empty or big-endian `float32` weight |
| Causal edge | source node row | `edge.causal:` | neighbor/target id | empty or big-endian `float32` weight |
| Semantic edge | source node row | `edge.sem:` | neighbor/target id | empty or big-endian `float32` weight |
| Entity edge | source node row | `edge.ent:` | neighbor/target id | empty or big-endian `float32` weight |
| Term posting | `idx:<term>` | consumer-defined | primary id | optional posting payload |

## Four disentangled graph views

The edge families are separate on purpose:

- `edge.temp:` is the temporal backbone between adjacent or nearby events.
- `edge.causal:` captures cause/effect links.
- `edge.sem:` captures semantic similarity or inferred affinity.
- `edge.ent:` captures event/entity and entity/entity relationships.

Because each view has its own column family, a scan or pushdown request for one
view can seek only that family. With shoal locality groups and sorted keys, this
keeps single-view scans cheap and avoids mixing temporal, causal, semantic, and
entity traversals in one large edge namespace.

## Mapping to pushdown requests

- Exact ShoalQL vector search: set `VectorOptions.EmbeddingSpace` to the full
  stable identity that produced the supplied raw vector. The planner carries
  it as the `vectorKNN` iterator's `embeddingSpace` option. The embedded engine
  validates every participating immutable file before scoring and rejects
  unknown, `no_embeddings`, or incompatible spaces.
- `VectorSearch`: set `embedding_cf` to `graphschema.VectorCF()`. Query vectors
  and embedding values use packed big-endian `float32`, matching
  `proto/embed.proto`. The lower-level gRPC request does not currently expose
  the per-file identity snapshot needed for the ShoalQL exact contract.
- `EdgeExpand`: set `edge_cf` to `graphschema.EdgeCF(kind)`. This schema stores
  the neighbor id in the column qualifier, so use the default `edge_field`
  (`"qualifier"`), no field separator, and set `primary_prefix` to the target
  row namespace when targets are ids rather than full rows.
- `TermFilter`: build posting rows with `graphschema.TermRow(term)`. This schema
  uses the posting cell's column qualifier as the primary id, matching the
  default `id_source` (`"qualifier"`). Set `primary_prefix` to
  `[]byte("evt:")` for event postings, or leave it empty if postings store full
  row keys.

## Temporal backbone via sorted ULIDs

Event row keys are `evt:<id>`, and event ids are expected to be lexically and
temporally sortable, such as ULIDs. That makes the primary event rowspace itself
a coarse temporal index: range scans over `evt:` return events in time order
while `edge.temp:` cells add explicit local jumps across the temporal graph.
