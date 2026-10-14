# Shoal Explorer synthetic evaluation fixture

This directory contains an entirely original corpus and graph about fictional Aster Relay and color-named systems. It is intended for deterministic retrieval, navigation, ranking, temporal, and authorization-isolation evaluation. It contains no copied product, customer, or operational material.

## Compatibility status

Expectation records use `semantics: "proposed-evaluation-v2"`. They define an evaluation-harness contract, not a current public API contract.

The current public retrieval `Response` is all-or-error and does not expose partial-result or authorization-oracle metadata. In particular, these are proposed fixture controls or evaluator fields:

- `request.strategy`, `request.modes`, `request.top_k`, `request.principal`, `request.filters`, and `request.cache_probe`;
- `applicability`, `expected.execution_state`, `expected.authorization_oracle`, `expected.results_exact`, and `expected.ranking`;
- evaluator-only forbidden IDs, required scope names, and canary tokens.

An adapter may map a current public error or response into this harness model, but tests must not assert that public runtime responses contain these fields.

UTF-8 byte offsets now match the public contract. A fixture citation uses a
zero-based, half-open byte range: `bytes[byte_start:byte_end]`. Both endpoints
must be UTF-8 boundaries, and decoding that byte slice must produce `quote`
exactly.

## Files and immutable document revisions

| Document ID | Revision ID | Type | Path | Effective interval | Proposed scopes | UTF-8 bytes | SHA-256 |
| --- | --- | --- | --- | --- | --- | ---: | --- |
| `aster-relay-protocol` | `r1` | `technical` | `corpus/aster-relay-protocol-r1.md` | `[2026-01-10T00:00:00Z, 2026-03-15T00:00:00Z)` | none | 729 | `29ec1ce44d2924c62129d67a6180290dcb2d1f77712d1e341503da087d40f34c` |
| `aster-relay-protocol` | `r2` | `technical` | `corpus/aster-relay-protocol-r2.md` | `[2026-03-15T00:00:00Z, open)` | none | 913 | `d13eae991ff306e0a8d9b6447bf9adadda66f847f2570d1cc782c61a36399790` |
| `amber-lag-runbook` | `r1` | `runbook` | `corpus/amber-lag-runbook.txt` | `[2026-03-15T00:00:00Z, open)` | `ops:read` | 868 | `195b386809b991714f4c0be16d84ee3cabfc34c8d25b58fa94a66db47880304a` |
| `adr-004-quartz-ring` | `r1` | `decision` | `corpus/adr-004-quartz-ring.md` | `[2025-12-18T00:00:00Z, open)` | none | 746 | `5db5176b7da75ed7786316fa95926d4f591ce4deb3ff3fd2881464583e9b27d8` |

Corpus files are UTF-8 without a BOM, use LF line endings, and end with one LF. `document_id` is logical and stable across revisions; `revision_id` selects one immutable fixture file.

Other fixture files are:

- `relationships.jsonl`: citation-supported relationships over corpus entities and revisions;
- `graph.jsonl`: a graph snapshot with graph-native nodes, edges, paths, authorization canaries, and cache-probe metadata;
- `hierarchy.jsonl`: document/section hierarchy metadata used only by tree-mode locality evaluation;
- `expectations.jsonl`: query requests and exact evaluator oracles.

Current fixture counts are 4 document revisions, 10 citation-backed relationships, 1 hierarchy with 5 nodes, 1 graph snapshot with 18 nodes, 15 edges, and 2 paths, and 19 expectations. Of those expectations, 12 are currently evaluable and 7 are future/not-evaluable contract probes.

## Shared record types

The notation below is normative. `T[]` means an array of `T`, `T?` is optional, `null` is JSON null, and objects reject unlisted fields.

```text
DocumentRef = {
  document_id: non-empty string,
  revision_id: non-empty string,
  path: "corpus/"-relative path
}

SectionRef = {
  document_id: non-empty string,
  revision_id: non-empty string,
  section_id: non-empty string
}

Citation = {
  document_id: non-empty string,
  revision_id: non-empty string,
  path: "corpus/"-relative path,
  section_id: non-empty string,
  byte_start: integer >= 0,
  byte_end: integer > byte_start,
  quote: non-empty string
}

EntityRef = {
  entity_id: non-empty string,
  entity_type: one of
    alert | component | decision | queue | revision |
    runbook | service | system | team
}
```

Every citation contains both logical `document_id` and explicit `revision_id`; neither may be inferred from the path. Section IDs are lowercase heading slugs. The plain-text runbook uses `header`, `trigger`, `triage`, `mitigation`, and `escalation`.

## Retrieval modes and `top_k`

Every expectation request has an integer `top_k > 0`. Modes are exact: no evaluator may infer a backend default from an unspecified or accidentally empty array.

The valid strategy-to-mode declarations are:

| `strategy` | Exact `modes` |
| --- | --- |
| `lexical-only` | `["lexical"]` |
| `vector-only` | `["vector"]` |
| `tree-only` | `["tree"]` |
| `graph-only` | `["graph"]` |
| `declared-hybrid` | `["lexical","vector"]` |
| `public-default-semantics` | `[]` |

Exactly one case, `q14-public-default-semantics`, may use `modes: []`; the
public request normatively normalizes it to lexical. The full fixture case
remains FUTURE because it also requests explicit `AsOf`, requires observable
effective modes, and has no current result oracle. Empty modes in every other
fixture strategy remain invalid schema even though the public request value
would normalize them.

The other strategy labels are harness controls, not a model, storage, scoring, or current public request guarantee.

## `relationships.jsonl` schema

Each non-empty line is one object:

```text
Relationship = {
  relationship_id: unique non-empty string,
  relationship_type: one of
    buffers_on | consumed_by | delivers_to | mitigates | monitors |
    owned_by | part_of | receives_from | selects | supersedes,
  source: EntityRef,
  target: EntityRef,
  valid_from: RFC 3339 UTC timestamp,
  valid_to_exclusive: RFC 3339 UTC timestamp | null,
  evidence: Citation[1..]
}
```

Relationship validity is `[valid_from, valid_to_exclusive)`. Citation evidence must refer to a revision whose interval overlaps the relationship interval.

## `hierarchy.jsonl` schema

Each non-empty line is one immutable hierarchy over one document revision:

```text
Hierarchy = {
  hierarchy_id: unique non-empty string,
  document_id: existing logical document ID,
  revision_id: existing revision ID,
  path: matching corpus path,
  nodes: HierarchyNode[1..]
}

HierarchyNode = {
  node_id: globally unique non-empty string,
  node_type: document | section,
  heading: exact non-empty heading text,
  parent_node_id: existing HierarchyNode.node_id | null,
  depth: integer >= 0,
  byte_start: integer >= 0,
  byte_end: integer > byte_start
}
```

The document root has null parent, depth zero, and spans the whole revision. Every section node has a parent whose byte range contains it and has depth exactly one less. Sibling section ranges do not overlap.

`q05-section-hierarchy` is genuinely tree-only: it asks for the heading containing an exact citation and its document ancestor. Its results and evidence reference `hierarchy.jsonl`, while `relationships_exact` is empty. The answer therefore cannot be satisfied by graph-relationship retrieval alone.

## `graph.jsonl` schema

The first record is exactly one snapshot. Remaining records are nodes, edges, or paths in that snapshot.

```text
GraphNativeAnchor = {
  kind: "graph_native",
  anchor_id: unique non-empty string
}

Snapshot = {
  record_type: "snapshot",
  snapshot_id: unique non-empty string,
  as_of: RFC 3339 UTC timestamp,
  evidence_contract: {
    graph_native: "evaluable",
    grpc_citation_shape: "not_evaluable_future_anchor_contract"
  },
  canary_origin_rule: {
    included_record_fields: [
      "node.canary_token",
      "edge.canary_token",
      "path.canary_token"
    ],
    excluded_request_fields: [
      "query",
      "request.top_k",
      "request.modes",
      "request.principal",
      "request.filters",
      "request.scope",
      "request.neighborhood"
    ]
  },
  cache_probes: CacheProbeDefinition[1..],
  deterministic_oracles: DeterministicGraphOracle[1..]
}

CacheProbeDefinition = {
  probe_id: unique non-empty string,
  first_case_id: existing case_id,
  second_case_id: existing case_id,
  ordering:
    authorized_then_unauthorized |
    unauthorized_then_authorized,
  principal_partition_required: true,
  required_outcomes: unique (
    no_leak |
    no_poisoning |
    no_authorized_result_suppression
  )[1..]
}

DeterministicGraphOracle = {
  oracle_id: unique non-empty string,
  oracle_type: "directed_fan_out",
  hub_node_id: existing Node.node_id,
  direction: "outgoing",
  edge_ids: existing Edge.edge_id[1..],
  leaf_node_ids: existing Node.node_id[1..],
  expected_edge_count: integer >= 1,
  expected_leaf_count: integer >= 1,
  deterministic_order: "edge_id_ascending"
}

Node = {
  record_type: "node",
  snapshot_id: Snapshot.snapshot_id,
  node_id: unique non-empty string,
  node_type: component | service | system,
  label: non-empty string,
  required_scopes_all: unique string[],
  valid_from: RFC 3339 UTC timestamp,
  valid_to_exclusive: RFC 3339 UTC timestamp | null,
  evidence: GraphNativeAnchor,
  canary_token?: unique non-empty string
}

Edge = {
  record_type: "edge",
  snapshot_id: Snapshot.snapshot_id,
  edge_id: unique non-empty string,
  edge_type: links_to | part_of | routes_to,
  source_node_id: existing Node.node_id,
  target_node_id: existing Node.node_id,
  required_scopes_all: unique string[],
  valid_from: RFC 3339 UTC timestamp,
  valid_to_exclusive: RFC 3339 UTC timestamp | null,
  evidence: GraphNativeAnchor,
  canary_token?: unique non-empty string
}

Path = {
  record_type: "path",
  snapshot_id: Snapshot.snapshot_id,
  path_id: unique non-empty string,
  node_ids: existing Node.node_id[2..],
  edge_ids: existing Edge.edge_id[1..],
  required_scopes_all: unique string[],
  valid_from: RFC 3339 UTC timestamp,
  valid_to_exclusive: RFC 3339 UTC timestamp | null,
  evidence: GraphNativeAnchor,
  canary_token?: unique non-empty string
}
```

For each path, `len(node_ids) == len(edge_ids) + 1`, and each edge must connect the corresponding adjacent node pair.

The Violet Gate, Celadon Hub, and Sable Sink component is graph-native and has no document citation. Future graph-only query `q11` defines the desired navigation and graph-native evidence oracle.

Snapshot oracle `oracle:graph-fan-out-10` declares one visible hub, exactly 10 visible leaves, and exactly 10 directed outgoing hub-to-leaf edges. Its edge and leaf arrays are paired by index and ordered by ascending edge ID. No other edge may have `node:fanout-hub` as its source.

The graph snapshot explicitly marks retrieval evaluation over the current Citation-shaped gRPC evidence as `not_evaluable_future_anchor_contract`. Fixture-file citations remain mechanically evaluable, but comparing a runtime gRPC citation to a graph-native anchor requires a future anchor contract.

## Evidence union and stable results

```text
CitationEvidence = {
  evidence_id: unique non-empty string within an expectation,
  evidence_type: "citation",
  evaluation_state: "evaluable",
  citation: Citation
}

GraphNativeEvidence = {
  evidence_id: unique non-empty string within an expectation,
  evidence_type: "graph_native",
  evaluation_state: "evaluable",
  graph_record_id: existing graph node_id | edge_id | path_id
}

HierarchyNativeEvidence = {
  evidence_id: unique non-empty string within an expectation,
  evidence_type: "hierarchy_native",
  evaluation_state: "evaluable",
  hierarchy_id: existing Hierarchy.hierarchy_id,
  hierarchy_node_id: existing HierarchyNode.node_id
}

Evidence =
  CitationEvidence |
  GraphNativeEvidence |
  HierarchyNativeEvidence

ExpectedResult = {
  result_id: unique non-empty string within an expectation,
  result_type:
    document_section | hierarchy_node |
    graph_node | graph_edge | graph_path,
  target_id: stable non-empty string,
  evidence_ids: existing Evidence.evidence_id[1..],
  relevance_grade: integer 0..3,
  tie_group: integer >= 1
}
```

`result_id` is the unit used for `top_k`, relevance, tie grouping, and deterministic ranking. This removes ambiguity between documents, sections, graph records, and synthesized paths.

## `expectations.jsonl` schema

```text
Expectation = {
  case_id: unique "qNN-kebab-case" string,
  semantics: "proposed-evaluation-v2",
  query: non-empty string,
  request: {
    strategy:
      lexical-only | vector-only | tree-only |
      graph-only | declared-hybrid | public-default-semantics,
    modes: unique (lexical | vector | tree | graph)[],
    top_k: integer >= 1,
    as_of: RFC 3339 UTC timestamp,
    scope?: {
      document_ids: unique non-empty string[1..]
    },
    principal: {
      scopes: unique non-empty string[]
    },
    filters: {
      revision_mode: effective | all,
      document_types?: unique (technical | runbook | decision)[],
      revision_ids?: unique non-empty string[],
      entity_ids_all?: unique non-empty string[],
      graph_snapshot_id?: non-empty string,
      graph_record_types?: unique (node | edge | path)[]
    },
    cache_probe?: {
      probe_id: existing Snapshot.cache_probes.probe_id,
      sequence_position: 1 | 2,
      paired_case_id: existing case_id
    },
    neighborhood?: NeighborhoodRequest
  },
  expected: CurrentExpected | FutureExpected,
  applicability: Applicability
}

Applicability = {
  surface: "public_retrieval_grpc",
  state:
    "current_evaluable" |
    "future_not_evaluable_current_public_contract",
  current_gate_eligibility: {
    conformance: boolean,
    embedded_remote_parity: boolean,
    release: boolean,
    relevance: boolean
  },
  activation: {
    requires_all_capabilities: unique Capability[]
  }
}

Capability =
  public_graph_native_anchor_v1 |
  public_graph_navigation_v1 |
  public_authorization_filter_v1 |
  public_principal_partitioned_cache_v1 |
  public_default_mode_contract_v1 |
  observable_effective_modes_v1 |
  public_bounded_graph_neighborhood_v1

CurrentExpected = {
    execution_state: complete | partial | error,
    authorization_oracle: AuthorizationOracle,
    documents_exact: unique DocumentRef[],
    sections_exact: unique SectionRef[],
    evidence_exact: unique Evidence[],
    relationships_exact: unique relationship_id[],
    evidence_paths_exact: unique EvidencePath[],
    facts_exact: unique Fact[],
    results_exact: unique ExpectedResult[],
    ranking: Ranking,
    error_exact: ErrorOracle | null
}

FutureExpected = {
  execution_state: null,
  authorization_oracle: null,
  documents_exact: [],
  sections_exact: [],
  evidence_exact: [],
  relationships_exact: [],
  evidence_paths_exact: [],
  facts_exact: [],
  results_exact: [],
  ranking: null,
  error_exact: null,
  future_contract: {
    status: "not_evaluable_current_public_contract",
    current_release_gate: false,
    relevance_gate: false,
    ranking_gate: false,
    activation: {
      requires_target_advertisement:
        "versioned_default_mode_contract" |
        "versioned_bounded_graph_neighborhood_contract",
      requires_observable_effective_modes: true
    },
    snapshot_oracle_id?: existing DeterministicGraphOracle.oracle_id
  }
}

EvidencePath = {
  path_id: unique non-empty string within an expectation,
  path_type: relationship | graph_native | hierarchy,
  step_ids:
    relationship_id[1..] |
    graph edge_id[1..] |
    hierarchy node_id[2..],
  evidence_ids: existing Evidence.evidence_id[1..]
}

Fact = {
  subject_id: non-empty string,
  predicate: non-empty string,
  value: string | number | boolean,
  qualifier?: non-empty string
}

Ranking = {
  tiers: {
    result_ids: unique ExpectedResult.result_id[1..],
    tie_group: integer >= 1
  }[],
  same_input_order_must_repeat: true
}

ErrorOracle = {
  code: "authorization_denied",
  details_evaluation: "not_evaluable_future_t0"
}
```

For `CurrentExpected`, `results_exact` contains at most `request.top_k` records. Every result appears exactly once in ranking. A ranking tier's `tie_group` equals every member result's `tie_group`; earlier tiers use strictly increasing tie-group numbers.

`q14` uses `FutureExpected`. Empty modes normatively mean lexical, but the case
has no Finch ownership oracle or any other exact result/ranking assertion and
requests explicit `AsOf`. It contributes nothing to current release gates. A
future fixture revision may promote it only after the target supports its
publication-frontier `AsOf`, reports effective modes, and gains exact oracles.

`q19-future-fan-out-10` also uses `FutureExpected`. Its public request shape is suitable for a bounded-neighborhood evaluator:

```text
NeighborhoodRequest = {
  root_node_id: existing Node.node_id,
  direction: "outgoing",
  max_depth: 1,
  max_fan_out: integer >= 1
}
```

The current public contract cannot execute this bounded-neighborhood oracle, so q19 has no current results, relevance, or ranking gate. Its `snapshot_oracle_id` retains the deterministic fan-out-10 truth until a target advertises a versioned bounded-graph-neighborhood contract with observable effective modes.

## Applicability and capability activation

`applicability` is required and closed-schema on every expectation. It is the only field that determines whether a case participates in current conformance, embedded/remote parity, release, or relevance gates:

- `current_evaluable` requires all four gate booleans to be true and `requires_all_capabilities` to be empty;
- `future_not_evaluable_current_public_contract` requires all four gate booleans to be false and at least one required capability;
- a future case may activate only when the target advertises every named, versioned capability on `public_retrieval_grpc`.

Execution state, authorization oracle, results, evidence, relevance grades, and ranking never override applicability. This makes it mechanically impossible for a future case with a fully specified desired oracle to enter a current gate.

The future cases and activation requirements are:

| Cases | Required capabilities |
| --- | --- |
| q11 | `public_graph_native_anchor_v1`, `public_graph_navigation_v1` |
| q12, q13, q15, q16 | `public_graph_native_anchor_v1`, `public_graph_navigation_v1`, `public_authorization_filter_v1`, `public_principal_partitioned_cache_v1` |
| q14 | `public_default_mode_contract_v1`, `observable_effective_modes_v1` |
| q19 | `public_bounded_graph_neighborhood_v1`, `observable_effective_modes_v1` |

q11, q12, q13, q15, and q16 deliberately retain their desired future `execution_state`, authorization, result, evidence, and ranking oracles. Those payloads become active only after all declared capabilities are advertised; until then their all-false gate eligibility excludes them from current retrieval/gRPC conformance and current embedded/remote parity.

q14 and q19 additionally use `FutureExpected` because the current public contract cannot issue their operations at all.

`request.scope.document_ids` is the public direct-document request scope. Case `q10-unauthorized-error` contains exactly one logical document ID, `amber-lag-runbook`, and exactly one `filters.revision_ids` value, `r1`. Their Cartesian product identifies one immutable fixture revision and maps to one Request. This field is not evaluator-only authorization metadata.

## Execution state versus authorization

Execution state and authorization policy are separate:

- `complete` means the request executed successfully for that principal, even if policy filtered every otherwise relevant record;
- `partial` is reserved for an execution-level truncation or recoverable backend failure and must never be caused solely by authorization filtering;
- `error` means execution returned an error according to the proposed policy adapter.

The null execution and authorization fields in `FutureExpected` mean “not evaluated,” not a fourth execution or authorization state.

```text
AuthorizationOracle = {
  policy_id: public-or-filter | filter-unreadable | deny-direct-identity,
  decision: allow | filtered | denied,
  task_answerable: boolean,
  candidate_must_report_forbidden_ids: false,
  candidate_must_report_scope_names: false,
  evaluator_only?: {
    forbidden_record_ids?: unique string[],
    required_scope_names?: unique string[],
    canary_channels?: unique (graph_node | graph_edge | graph_path)[],
    canary_tokens?: unique string[],
    forbidden_canary_tokens?: unique string[]
  }
}
```

Authorization-filtered case `q09` and the desired future oracles for q13 and q15 are execution-`complete`. They expect no results because their tasks are not answerable from visible evidence, and `task_answerable` is false. They receive no relevance or task-success credit for returning an unrelated public fact.

`q10` is the separate unauthorized-error oracle. Candidate output is not required to contain forbidden record IDs, scope names, detailed error text, or canary values.

For q13 and q15, evaluator-only `forbidden_record_ids` is the complete restricted closure exercised by the query: `node:umber-vault`, `edge:umber-links-opal`, hidden intermediate `node:opal-bridge`, `edge:jade-routes-opal`, `edge:opal-routes-silver`, and `path:jade-via-opal-to-silver`. The closure is derived from the authorized node/edge results and every restricted node and edge referenced by the authorized path, so no hidden intermediate can evade the oracle.

## Authorization canaries and cache probe

Canaries exist only for channels backed by actual fixture records:

| Channel | Record | Token |
| --- | --- | --- |
| `graph_node` | `node:umber-vault` | `CANARY_GRAPH_NODE_7H3` |
| `graph_edge` | `edge:umber-links-opal` | `CANARY_GRAPH_EDGE_9K2` |
| `graph_path` with hidden intermediate | `path:jade-via-opal-to-silver` | `CANARY_GRAPH_PATH_4M8` |

Only the declared graph-record `canary_token` fields are canary origins. Tokens appearing in a query, `top_k`, modes, principal, filters, or other request metadata are explicitly excluded from leakage detection.

Snapshot probe `graph-canary-cross-principal` evaluates authorized then unauthorized:

1. `q12-authorized-canary-warm`, which warms results under `graph:restricted`;
2. `q13-unauthorized-canary-probe`, which repeats the request without that scope.

Reverse probe `graph-canary-cross-principal-reverse` evaluates unauthorized then authorized:

1. `q15-unauthorized-cache-prime`, which primes the request without the scope;
2. `q16-authorized-cache-verify`, which repeats it under `graph:restricted`.

Both probes require principal-partitioned cache entries and `no_leak` plus `no_poisoning`. The reverse probe additionally requires `no_authorized_result_suppression`: the unauthorized first response must not suppress the complete authorized node, edge, and path results in the second response.

Both cache sequences are paired future probes. Their desired authorization and cache outcomes remain fully specified, but they contribute to no current gate until the target advertises the graph-native anchor, graph navigation, authorization-filter, and principal-partitioned-cache capabilities.

Forbidden IDs, tokens, and scope names live in `authorization_oracle.evaluator_only`; candidates do not need to echo them.

There are no explanation-channel, runtime error-body-channel, or runtime span-rendering canaries in this fixture because no corresponding fixture records exist. Those are required future T0 expansions and must not be reported as covered. The byte-span checks here validate fixture integrity only.

## Temporal, filter, and comparison semantics

The declared fixture tick precision is exactly one second. The protocol revision boundary is `2026-03-15T00:00:00Z`:

- q17 is exactly one tick before at `2026-03-14T23:59:59Z` and selects r1;
- q03 remains the exact-boundary case and selects r2;
- q18 is exactly one tick after at `2026-03-15T00:00:01Z` and selects r2.

For `revision_mode: "effective"`, a revision or graph record is eligible when:

```text
valid_from <= request.as_of
and
(valid_to_exclusive is open or request.as_of < valid_to_exclusive)
```

`revision_mode: "all"` permits all document revisions after other filters. Filter families are conjunctive. `entity_ids_all` requires every listed entity. Graph snapshot and record-type filters apply only to graph records.

Fields ending in `_exact` are exact, order-insensitive sets unless stated otherwise. Relationship, graph, and hierarchy path steps are ordered. For relationship paths, adjacent relationship endpoints must join. For graph paths, adjacent graph edges must join through the declared nodes. For hierarchy paths, each next node's `parent_node_id` equals the prior node ID.

## Required mechanical validation

A fixture validator must fail on any of the following:

1. malformed JSON, blank JSONL records, missing fields, extra fields, or incorrect field types;
2. duplicate case, relationship, graph record, hierarchy node, anchor, evidence, result, path, cache-probe, or canary-token IDs;
3. a missing, extra, or open-ended applicability field; an unknown capability; or gate booleans inconsistent with applicability state;
4. any future case counted in current conformance, embedded/remote parity, release, or relevance gates, or activated without every required capability;
5. a missing/non-positive `top_k`, a strategy-to-modes mismatch, more than one empty-mode case, or an empty-mode case other than `q14-public-default-semantics`;
6. q14 or q19 having any current execution, authorization, exact result, evidence, fact, relevance, ranking, or release-gate oracle, or lacking its future activation requirements;
7. a path outside this fixture, missing file, BOM, CR byte, invalid UTF-8, missing final LF, length mismatch, or SHA-256 mismatch;
8. a document header whose logical document ID, revision ID, or temporal interval disagrees with the inventory;
9. a citation outside its file, assigned to the wrong section, or whose UTF-8 slice differs from `quote`;
10. an unknown or temporally invalid relationship, graph record, hierarchy node, evidence, result, or path reference;
11. relationship, graph, or hierarchy path discontinuity;
12. graph-native evidence that points to a citation, or hierarchy-native evidence that points outside `hierarchy.jsonl`;
13. a result with missing evidence, invalid grade/tie group, duplicate identity, or a count above `top_k`;
14. a ranking that does not exactly partition results, disagrees with result tie groups, or orders tie groups non-monotonically;
15. an effective revision or graph record outside the request's `as_of` interval;
16. an invalid one-second fixture tick, an invalid q17/q18 timestamp or revision selection, or an invalid exact-boundary result at `2026-03-15T00:00:00Z`;
17. an authorization-filtered case marked execution-`partial`, or a denied case not marked execution-`error`;
18. positive relevance/results when `task_answerable` is false;
19. a visible result or evidence requiring scopes absent from the principal;
20. a public direct-document scope that maps to zero or multiple revisions instead of exactly one Request;
21. either cache ordering missing, mispaired, principal-equal, inconsistent with `graph.jsonl`, leaking results, poisoning results, or suppressing the authorized second response;
22. q13 or q15 omitting any restricted node, edge, or path in the authorized query closure;
23. a fan-out oracle whose hub has other outgoing edges, whose edge/leaf pairing is discontinuous, whose order is nondeterministic, or whose count is not exactly 10;
24. q19 referencing any other snapshot oracle, contributing to current result/ranking gates, or lacking its future bounded-neighborhood activation contract;
25. a canary origin, channel, record, or token inconsistent with `graph.jsonl`;
26. any evaluator-only forbidden ID, scope name, or canary token made mandatory in candidate output;
27. any claim that span, explanation, or error-body canaries are covered.
