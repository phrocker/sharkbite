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

A store can also carry a descriptor of a table's layout so a reader does not
have to be told these conventions out of band; see
[Describing a table in the store](#describing-a-table-in-the-store). The
engine stores the descriptor and never interprets it.

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

## Describing a table in the store

`proto/schema.proto` (`shoal.schema.v1.TableSchema`, generated into
`internal/schemapb`) is the well-known, versioned descriptor of how one
table's cells lay out a graph: node row namespaces (prefix, how the kind is
cut out of the row, attribute family, qualifier role, value encoding), edge
families, embedding families, and term posting namespaces. The edge, vector,
and posting entries carry the `EdgeExpand`, `VectorSearch`, and `TermFilter`
parameters of `proto/embed.proto` under the same names, so a reader builds a
scan request straight from the descriptor and the descriptor cannot say
something the engine cannot do. `internal/tableschema` validates, encodes,
records, and reads descriptors; `tableschema.ForGraphSchema` is the
descriptor of the layout in this document.

Where it lives:

- The reserved table `_shoal_schema`, created by the first write and never by
  `init`: row = the described table's name, column family `schema`, column
  qualifier = the version zero-padded to 20 digits (so versions sort), value
  = the `TableSchema` in proto3 JSON with proto field names, compact. The
  JSON is the wire form and reads back with `shoal-embed scan --table
  _shoal_schema`; the proto is the contract. Writes are append-only and
  versions are monotonic: a put must carry a version greater than the newest
  recorded one, or the same version with a byte-identical document (a no-op
  that only repairs the pointer). Any other reuse of a version is refused.
- The described table's manifest (`table.json`) gains an optional
  `"schema": {"id": "...", "version": N}` pointer to the newest version, so
  `shoal-embed status` reports described tables without a scan. The manifest
  version is unchanged: older binaries ignore the field and keep opening newer
  stores. The pointer is an index; the reserved table holds the document.

The descriptor is kept in a table rather than in the RFiles because RFiles
are immutable and per tablet (a schema per file is a schema per flush, and
readers would have to reconcile several), the `rfile` package has no
metadata-block API, a table reads over the existing `Scan` RPC from Go and
Python alike, and a schema change is one row write rather than a compaction.

CLI:

```
shoal-embed schema put  --data ~/.shoal/data --table graph --file graph-schema.json
shoal-embed schema get  --data ~/.shoal/data --table graph [--version N]
shoal-embed schema list --data ~/.shoal/data
shoal-embed status      --data ~/.shoal/data      # SCHEMA column: id@version or -
```

A descriptor of a table laid out as `ent:<kind>|<id>` and `evt:<kind>|<id>`
rows, `cf = <record type>`, `cq = <field>`, text values, and edges as
`cf = edge`, `cq = <relationship>\0<target row>` (the empty `attribute_cfs`
means every family not claimed by the edge entry holds attributes):

```json
{
  "table": "graph",
  "schema_id": "foreman.graph",
  "version": 1,
  "nodes": [
    {"name": "entity", "row_prefix": "ent:", "kind_sep": "|", "kind_index": 0,
     "attribute_qualifier": "QUALIFIER_ROLE_FIELD", "attribute_encoding": "VALUE_ENCODING_TEXT"},
    {"name": "event", "row_prefix": "evt:", "kind_sep": "|", "kind_index": 0,
     "attribute_qualifier": "QUALIFIER_ROLE_FIELD", "attribute_encoding": "VALUE_ENCODING_TEXT"}
  ],
  "edges": [
    {"name": "edge", "edge_cf": "edge", "edge_field": "qualifier", "field_sep": "\u0000",
     "id_index": 1, "id_index_set": true, "rel_index": 0}
  ]
}
```

Empty `attribute_cfs` means every column family not claimed by an edge,
vector, or posting entry holds attributes and names the record type; node
namespaces may share attribute families (this document's layout puts
`attr:` on every node row). Descriptors are read through the newest cell of
each version only, a deleted version is absent, and a version is recorded
with a conditional write that requires it absent, so two writers cannot
both record one version.
Validation refuses a descriptor whose node or posting row prefixes overlap,
whose entries claim the same column family twice, or whose pushdown
parameters the engine would not accept (an unknown `edge_field`, `id_source`,
or `metric`; `id_index`, `rel_index`, or `relationships` without a
`field_sep`).

## Serving a described table through the Explorer

`internal/graphtable` reads a described table as a graph, and
`shoal-explore-web -graph-table <name>` serves it at `/api/v1/graph-table`
beside the workspace's own graph: bounded record browsing, nodes by row key,
neighborhoods, directed paths, and term lookups over the table's postings,
plus the descriptor projected as the table's ontology (namespaces and their
kinds, edge families and their relationships, postings, embedding spaces).
The Graph panel has a live record picker. Readers choose a namespace and
optionally a kind and identifier prefix, then select a named record to load
its first hop. A link to the selected record uses namespace, kind, and
identifier in the URL; storage row keys stay out of the visible controls.
Term lookup, hop count, and source switching remain under Graph options.

`POST /api/v1/graph-table/browse` accepts `namespace`, optional `kind`,
`identifier_prefix`, `after`, and `limit` (1–40). It returns `nodes` with
display labels and an optional `next` cursor. The server scans at most 4096
cells per page; clients pass `next` as `after` for another page. A kind filter
uses the descriptor's kind separator when the kind is the first key field.

The reader never opens the table. Every read is the `ScanRequest` of
`proto/embed.proto`, so the rows are served by whichever process hosts
them:

- With no `-graph-stores`, the table is read from the workspace's own
  engine (the `-data` or `-state-dir/corpus` directory), which is where a
  consumer that shares the store with the Explorer wrote it.
- With `-graph-stores address[=start..end],...`, the rows are read from
  those `shoal-embed serve` processes over gRPC, each owning the half-open
  row range it names. The ranges must tile the keyspace with no gap or
  overlap (one address with no range owns every row). Every store must
  carry the same newest descriptor of the table, read from its own
  `_shoal_schema`; a disagreement refuses to start.

A neighborhood is expanded hop by hop. Each hop reads the frontier's rows
through the store (concurrently, each row routed to the shard that owns
it), parses the edge cells by the descriptor exactly as `EdgeExpand`
would (qualifier or value, split by `field_sep`, id at `id_index` or the
last field, relationship at `rel_index`, neighbor row `primary_prefix` +
id, a `float32_be` value as the weight), follows up to `fanout` edges per
node in (family, relationship, target) order, and stops adding nodes at
`max_nodes`. For a qualifier such as `owns\u0000service|api` whose target stores
only the id, set `field_sep` to `\u0000`, `rel_index` to `0`, `id_index` to
`1`, and `primary_prefix` to `ent:`. The reader then follows
`ent:service|api`; without the prefix it looks for `service|api`.
The rows the last hop reached are read for their kinds and
attributes. The Explorer therefore holds only the walk's frontier; the
cell reads, merges, and versioning run in the store processes, and a
table split across several stores is walked without any one process
holding it. Edges are followed outward only: the layout records no
reverse index, and none is invented. Pushdowns (`EdgeExpand`,
`TermFilter`, `VectorSearch`) are not used for the walk, because a
pushdown resolves rows inside one store and the rows it resolves to may
be owned by another; `internal/graphtable`'s parity test checks that the
descriptor reading of a `graphschema` table returns the neighbor rows the
engine's `EdgeExpand` returns under the mapping above.

A described table is trusted ingest exposed under one label,
`-graph-table-label`: a reader must hold that label's grant (for
`-dev-auth`, `-dev-auth-labels shoal-explore-web/workspace=<label>`), and a
reader who does not is told no such route exists. Per-cell visibility from
`cv` is not applied. Writes through the workspace to a described table,
documents, and citations are out of scope: a described table has nodes
and edges.

Bounds: at most 64 seeds, 8 hops, 256 edges per node, 2048 nodes per
expansion, and 16 terms per lookup; a request past them is refused. One
row is read up to 8192 cells; a row past that (a hub, or one carrying
large embedding cells) is cut there and the expansion reports
`truncated`, as it does for a fanout or node-budget cut. Node ids are
JSON strings, so the reader names only row keys that are UTF-8, at most
4096 bytes, and inside a described namespace; an edge cell whose target
is none of those is not followed and is counted in `dropped_edges`. The
engine's `EdgeExpand` follows any byte string, which is the one place the
two readings differ, and the reader says when it happened.
