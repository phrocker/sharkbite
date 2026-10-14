# Authorized bounded graph analytics

Shoal analytics are computed only over a caller-authorized, explicitly bounded
graph neighborhood. They are not whole-corpus ranks and are never sourced from
stored compaction rank cells.

## Operations

- HTTP: `POST /api/v1/analytics`
- MCP: `shoal.analytics`
- Authorization: `analytics_read` for the analytics operation and `retrieve`
  for its recorded evidence, plus the source and policy grants required by
  every returned node and edge

The MCP tool is advertised only when its provider and validated runtime limits
are installed and the launcher identity explicitly includes `analytics_read`
and `retrieve`.
The stdio identity keeps its configured source and policy ID bounds unchanged.
The HTTP metadata response reports the provider capability and limits.

## Request scope

Every request supplies:

- one or more opaque seed node IDs;
- traversal depth and direction;
- per-node fanout;
- maximum materialized nodes and edges;
- maximum scanned edges per expanded node; and
- an optional edge-type allow-list.

All graph work bounds are required and nonzero. Shoal fails the request if the
authorized neighborhood cannot be completely materialized within them. A
returned result always has `scope.complete: true`.

An optional analytics snapshot ID pins a retry to the same authorized
projection. The snapshot is derived from the filtered subgraph, authorization
fingerprint, policy generation, selected ontology, and requested bounds. It
does not expose the whole-corpus frontier, so changes confined to hidden graph
material do not alter it.

## Results

The response contains:

- converged PageRank with damping, tolerance, and iteration metadata;
- directed in-degree, out-degree, and total degree per node; and
- weakly connected component summaries.

PageRank redistributes dangling mass within the materialized subgraph and fails
if it does not converge within the requested iteration bound. Nodes are ordered
by descending PageRank with opaque node ID as the tie-breaker. Component
members and components are canonically ordered. PageRank is structural and
unweighted: each directed edge is one transition, including each parallel edge.
Total degree is in-degree plus out-degree, so a self-edge contributes to both.

The selected read-time ontology identity is part of the authorization
fingerprint and analytics snapshot. Unresolved assertion interpretations remain
explicit in scope metadata with their reading and reason; they are never
silently presented as resolved.

The shipped embedded HTTP and stdio MCP providers require the shared durable
`interaction.ResultSink`. They are unavailable when no sink is installed and
never return a successful analytics result until the interaction is durably
captured. The record retains every materialized node and exact source edge,
the selected ontology, authorization fingerprint and expiry, and trusted
actor/delegation/reason metadata. Its visibility is the conjunction of the
current workspace policies for every recorded node and edge. A failure after
the durable write is returned with indeterminate-commit semantics.

The interaction admission is authorized and recorded with `analytics_read`.
The underlying graph evidence is independently reauthorized with `retrieve`,
so evidence-bearing analytics identities require both operations. The
analytics service role permits this bounded pair.
