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
# Shoal Explorer evaluation plan

> **Status:** proposed release methodology.
>
> **Fixture root:** `testdata/explorer-eval`.
>
> This plan evaluates public document, graph, and retrieval behavior. It is
> backend-, storage-, transport-, parser-, and model-neutral. It does not
> depend on private indexes, embeddings, prompts, storage layouts, or Explorer
> implementation details.

## 1. Conformance language and capability labels

`MUST`, `MUST NOT`, `SHOULD`, and `MAY` are normative in this document.
Every assertion carries one of these capability labels:

| Label | Meaning |
| --- | --- |
| **CURRENT-CONTRACT** | Directly testable through the current public contracts in `pkg/document`, `pkg/graph`, or `pkg/retrieval`. |
| **CURRENT-BLACK-BOX** | Testable today through a product adapter without assuming internal implementation. |
| **PROPOSED-V2** | Defined by the delivered `proposed-evaluation-v2` fixture and evaluator contract, but not necessarily exposed by the current public API. |
| **FUTURE** | Requires a new public capability, fixture channel, or scale pack before it can be a release gate. |

An implementation MUST NOT report a **FUTURE** check as passing by substituting
an internal field, a model judgment, or a weaker proxy.

### 1.1 Public-contract boundary

The current public contracts support these direct assertions:

- **CURRENT-CONTRACT:** document and revision identity; half-open source ranges;
  UTF-8 byte boundaries; document hierarchy/ownership validation; graph node,
  edge, and directed-path structure; request normalization, modes, top-K,
  document/node scope, and as-of value normalization; deterministic result and
  evidence ordering; citations and graph paths; stable errors, cancellation,
  and deadlines.
- **CURRENT-CONTRACT:** `document.SourceRange` is half-open `[Start, End)` and
  `document.SourcePosition.Offset` is a zero-based UTF-8 byte offset.
- **CURRENT-CONTRACT:** empty modes normalize to lexical, `TopK == 0`
  normalizes to 20, and duplicate modes/scope IDs preserve first occurrence.
- **CURRENT-CONTRACT:** retrieval is all-or-error. A public `Response` has no
  partial-result status and no authorization-oracle metadata.
- **CURRENT-CONTRACT:** current gRPC retrieval evidence is citation-shaped.
  A graph-only result with only graph-native evidence is therefore
  `not_evaluable_future_anchor_contract` through current gRPC until a public
  graph-native anchor exists.

The fixture deliberately defines additional semantics:

- **CURRENT-CONTRACT:** fixture zero-based, half-open UTF-8 byte ranges may be
  checked directly with the public source-aware range and quote helpers.
- **PROPOSED-V2:** execution state, authorization policy, strategy, filters,
  principal scopes, stable result IDs, relevance grades, and cache-probe
  controls are evaluator fields.
- **FUTURE:** partial execution, runtime authorization metadata, and a public
  graph-native remote evidence anchor.

### 1.2 Current public `Request` mapping

Each currently applicable expectation maps to one public retrieval request.
The exceptions are the explicitly labelled **FUTURE** public principal and
filter capabilities, which require proposed adapter behavior today, and cases
whose applicability requires capabilities the target has not advertised.

| Fixture field | Public mapping | Status |
| --- | --- | --- |
| `query` | `retrieval.Request.Text` | **CURRENT-CONTRACT** |
| `request.modes` | Concrete modes map to `retrieval.Request.Modes`; empty modes normalize to lexical | **CURRENT-CONTRACT** |
| `request.top_k` | `retrieval.Request.TopK` | **CURRENT-CONTRACT** |
| `request.as_of` | `retrieval.Request.AsOf` | **CURRENT-CONTRACT** |
| `request.scope.document_ids` | `retrieval.Request.Scope.DocumentIDs` | **CURRENT-CONTRACT** |
| `request.scope.node_ids` | `retrieval.Request.Scope.NodeIDs` | **CURRENT-CONTRACT** |
| `request.strategy` | Harness declaration used to select and verify modes | **PROPOSED-V2** |
| `request.principal` | Authorization context supplied by the adapter | **PROPOSED-V2**, **FUTURE** public API |
| `request.filters` | Adapter filter controls | **PROPOSED-V2**, **FUTURE** public API |
| `request.cache_probe` | Evaluator sequencing metadata, not request payload | **PROPOSED-V2** |
| `request.neighborhood` | No retrieval request field; `pkg/explorer` has an embedded both-direction neighborhood operation without direction/fan-out fields or remote/storage-neutral conformance | **CURRENT-BLACK-BOX** only for the embedded shape; fixture request remains **FUTURE** |

`q14-public-default-semantics` now has a **CURRENT-CONTRACT** lexical default.
The delivered `proposed-evaluation-v2` case remains **FUTURE** as a release
gate because it also requests explicit `AsOf`, requires observable effective
modes, and has no current result oracle. A target may separately conformance
test normalization without claiming that the full q14 fixture is evaluable.

`q19-future-fan-out-10` is also **FUTURE** and
`not_evaluable_current_public_contract`. The current public retrieval request
can scope graph node IDs, but it cannot request a bounded one-hop neighborhood.
The case activates only when a target advertises a versioned bounded graph
neighborhood contract and exposes the effective modes used.

`q11`, `q12`, `q13`, `q15`, and `q16` retain complete desired graph-native,
authorization, and cache oracles but are transport-conditional and **FUTURE**
for current retrieval/gRPC evaluation. They activate only after the target
advertises every capability listed by their applicability records. Until then
they contribute to fixture-oracle validation only, not current conformance,
embedded/remote parity, relevance, or release decisions.

`q10-unauthorized-error` maps `scope.document_ids:
["amber-lag-runbook"]` and `filters.revision_ids: ["r1"]` to exactly one
immutable revision and one public `Request`. It is not an evaluator-only hidden
identity.

For current all-or-error adapters:

- a successful response maps to `execution_state: complete`;
- an error maps to `execution_state: error`;
- no current response may be synthesized as `partial`;
- authorization filtering may still be `complete` for that principal.

## 2. Evaluation principles

1. **Falsifiable before executable.** Every metric MUST define a population,
   numerator, denominator, comparison rule, and empty-population behavior.
2. **Exact oracles before similarity.** IDs, ranges, revisions, authorization,
   path continuity, and ordering use deterministic comparison.
3. **No hidden implementation assumptions.** Only normalized inputs, public
   outputs, fixture records, and declared run metadata may affect a verdict.
4. **No-model baseline first.** Lexical, tree, graph, temporal, and policy
   behavior MUST have a deterministic baseline that invokes no generative
   model.
5. **Independent modes.** Lexical, vector, tree, graph, and declared hybrid are
   evaluated separately. Hybrid success cannot mask a failing constituent
   mode. The future default-mode probe is excluded until its activation
   contract is satisfied.
6. **Two independent parsers.** Candidate ingestion and reference validation
   MUST use parsers with different implementations and no shared parsing
   lineage. Shared libraries for byte reading or hashing are allowed; shared
   AST, sectionization, range, or hierarchy logic is not.
7. **Visible output only.** Hidden oracle data is evaluator input and MUST
   never be required candidate output.
8. **No security averages.** One authorization leak fails the release,
   regardless of aggregate quality.
9. **Pairwise coverage is a floor.** Every supported pair of declared matrix
   values MUST occur; specifically risky combinations also require explicit
   higher-order cases.
10. **Reproducibility.** Dataset hash, parser identities, configuration hashes,
    build identity, random seed, clock, machine profile, and warm/cold state
    MUST be recorded.

## 3. Canonical fixture and sole normative schema

The delivered fixture inventory is:

```text
testdata/explorer-eval/
  README.md
  corpus/
    adr-004-quartz-ring.md
    amber-lag-runbook.txt
    aster-relay-protocol-r1.md
    aster-relay-protocol-r2.md
  expectations.jsonl
  graph.jsonl
  hierarchy.jsonl
  relationships.jsonl
```

`README.md` is the schema authority. `expectations.jsonl` contains only
`semantics: "proposed-evaluation-v2"`. The following is the one normative
schema used by this plan; objects are closed and reject every unlisted field.
`T[]` is an array, `T?` is optional, and `null` is JSON null.

The delivered fixture contains 4 document revisions, 10 citation-backed
relationships, 1 hierarchy with 5 nodes, 1 graph snapshot with 18 nodes,
15 edges, 2 paths, and 19 expectations. Twelve expectations are currently
evaluable and seven are future/not-evaluable under the current public
retrieval/gRPC contract (specifically
`q11`, `q12`, `q13`, `q14`, `q15`, `q16`, and `q19`).

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
  entity_type:
    alert | component | decision | queue | revision |
    runbook | service | system | team
}

Relationship = {
  relationship_id: unique non-empty string,
  relationship_type:
    buffers_on | consumed_by | delivers_to | mitigates | monitors |
    owned_by | part_of | receives_from | selects | supersedes,
  source: EntityRef,
  target: EntityRef,
  valid_from: RFC 3339 UTC timestamp,
  valid_to_exclusive: RFC 3339 UTC timestamp | null,
  evidence: Citation[1..]
}

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

GraphNativeAnchor = {
  kind: "graph_native",
  anchor_id: unique non-empty string
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

NeighborhoodRequest = {
  root_node_id: existing Node.node_id,
  direction: "outgoing",
  max_depth: 1,
  max_fan_out: integer >= 1
}
```

### 3.1 Required schema invariants

A strict validator MUST accept every delivered non-empty JSONL record and MUST
reject missing fields, extra fields, wrong types, duplicate identities, invalid
references, or any strategy/mode combination not listed below:

| Strategy | Exact modes |
| --- | --- |
| `lexical-only` | `["lexical"]` |
| `vector-only` | `["vector"]` |
| `tree-only` | `["tree"]` |
| `graph-only` | `["graph"]` |
| `declared-hybrid` | `["lexical","vector"]` |
| `public-default-semantics` | `[]` |

Every case has `top_k > 0`. `q14-public-default-semantics` is the sole permitted
empty-modes case and normatively normalizes to lexical. It remains a
`FutureExpected` fixture case because its explicit `AsOf`, effective-mode
observability, and result oracle are not currently available. Its null current
oracles mean "not evaluated," not another execution or authorization state.

`q19-future-fan-out-10` also uses `FutureExpected`. Its
`snapshot_oracle_id: "oracle:graph-fan-out-10"` references a deterministic
fixture oracle with exactly 10 outgoing edges from `node:fanout-hub`, exactly
10 paired leaf nodes, and ascending edge-ID order. Fixture structure and oracle
determinism are currently evaluable. Target neighborhood results, relevance,
ranking, latency, and release behavior are not: the embedded public operation
has no direction or fan-out fields and no remote/storage-neutral conformance.
Those checks activate only with the versioned bounded-graph-neighborhood
contract and observable effective limits.
Its declared request is `top_k: 10` with root `node:fanout-hub`, direction
`outgoing`, `max_depth: 1`, and `max_fan_out: 10`.

`applicability` is the sole authority for current gate participation:

- `current_evaluable` requires all four gate-eligibility booleans to be true
  and no activation capabilities;
- `future_not_evaluable_current_public_contract` requires all four booleans to
  be false and at least one named, versioned capability; and
- a future case activates only after the target advertises every required
  capability on `public_retrieval_grpc`.

Desired execution, authorization, result, evidence, relevance, and ranking
oracles never override applicability. `q11` requires
`public_graph_native_anchor_v1` and `public_graph_navigation_v1`. `q12`, `q13`,
`q15`, and `q16` additionally require `public_authorization_filter_v1` and
`public_principal_partitioned_cache_v1`. Their populated `CurrentExpected`
payloads preserve future oracle value but do not make them currently evaluable.

`q05-section-hierarchy` is the required genuine tree-only case. It evaluates a
section heading and document ancestor from `hierarchy.jsonl`, has no graph
relationship expectation, and cannot be passed by retrieving a `part_of`
relationship.

For `CurrentExpected`, `results_exact` is capped by `top_k`. Every result has a
stable ID, type, target, one or more evidence memberships, grade, and tie group.
Ranking tiers exactly partition results. Multi-document evidence remains
attached to its result; retrieval metrics deduplicate by `result_id`, never by
document alone.

### 3.2 Fixture mechanical gate

The validator MUST fail on any violation listed by the fixture README,
including:

- malformed JSONL, unknown fields, blank records, duplicate IDs, or missing
  references;
- a missing, extra, or open-ended applicability field, unknown capability, or
  gate booleans inconsistent with applicability state;
- a future case counted in current conformance, embedded/remote parity,
  relevance, or release, or activated without every required capability;
- corpus path escape, BOM, CR byte, invalid UTF-8, missing final LF, length or
  hash mismatch;
- citation slice/quote mismatch or citation assigned to the wrong section;
- hierarchy containment, depth, sibling overlap, or parent discontinuity;
- graph and relationship temporal or path discontinuity;
- result/evidence/ranking inconsistency;
- effective revision selection outside `[valid_from, valid_to_exclusive)`;
- an invalid one-second fixture tick or wrong `q17`, `q03`, or `q18` revision;
- an authorization-filtered case labelled partial;
- positive results when `task_answerable` is false;
- visible evidence requiring scopes absent from the principal;
- `q13` or `q15` omitting a restricted node, edge, or path from the complete
  authorized-query closure;
- a fan-out oracle with a count other than 10, a discontinuous edge/leaf pair,
  an additional outgoing hub edge, or nondeterministic edge order;
- `q19` referencing an oracle other than `oracle:graph-fan-out-10` or lacking
  its future bounded-neighborhood activation contract;
- `q14` or `q19` containing a current result, relevance, ranking, execution,
  authorization, or target release-gate oracle;
- either cache ordering absent or mispaired; or
- evaluator-only data made mandatory in candidate output.

The validator MUST also run mutation tests that independently remove, add,
rename, and mistype each field family. A validator that only accepts the
positive fixture without rejecting these mutations does not pass.

## 4. Dataset construction and parser independence

### 4.1 Delivered gold provenance

The synthetic corpus, hashes, ranges, relationships, hierarchy, graph snapshot,
and expectations are adjudicated gold. A change to any gold record requires:

1. a human-readable rationale;
2. regeneration by a reference tool independent of candidate ingestion;
3. review of the exact byte diff;
4. successful strict validation; and
5. a new dataset content hash in run metadata.

Gold MUST NOT be generated from the candidate system under test. Model output
MUST NOT silently rewrite deterministic gold.

### 4.2 Source format and parser are separate dimensions

Source format and parser identity MUST NOT be conflated. The delivered corpus
contains Markdown and plain text. The support relation is versioned and records
each `(source_format, candidate_parser)` combination as `supported`,
`unsupported`, or `experimental`. Unsupported combinations are excluded from
quality denominators but MUST be listed; silently dropping them fails fixture
completeness.

Run metadata is external to the normative fixture schema. It MUST record:

| Field | Requirement |
| --- | --- |
| Dataset | Fixture semantic version and whole-tree content hash |
| Source | Format, encoding, newline policy, byte length, and content hash |
| Candidate parser | ID, version, configuration hash, and implementation lineage |
| Reference parser | ID, version, configuration hash, and implementation lineage |
| Gold | Adjudication revision, adjudicator role, and provenance hash |
| Support relation | Version and status for every format/parser pair |
| Execution | Build ID, adapter ID, transport, storage profile, mode, seed, clock, and machine profile |

The candidate and reference parser MUST have no common AST, heading extraction,
sectionization, source-range, or hierarchy implementation lineage. Acceptance
requires both:

- candidate output equals adjudicated gold; and
- independent reference output equals adjudicated gold.

Candidate/reference agreement alone is insufficient because both may be wrong.
Disagreement is a hard ingestion failure until adjudicated.

### 4.3 Dataset splits

| Split | Purpose | Visibility |
| --- | --- | --- |
| Fixture | Fast deterministic contract and regression checks | Public |
| Development | Tuning and diagnostics | Public to implementers |
| Blind release | Unbiased release gate | Queries hidden until evaluation |
| Challenge | Adversarial parsing, temporal, authorization, and fan-out cases | Partially hidden |
| Scale | Performance and capacity only; quality gold sampled | Generated from fixed seed |

Logical document or revision families MUST remain in one split. Near-duplicate
content MUST NOT cross development and blind splits.

## 5. Coverage matrix

### 5.1 Dimensions

The run manifest MUST enumerate these independent dimensions:

| Dimension | Minimum values |
| --- | --- |
| Source format | Markdown, plain text, each newly supported format |
| Parser | Every supported candidate parser; one independent reference parser |
| Revision | Single, multiple, before boundary, exact boundary, after boundary |
| Mode | Lexical, vector, tree, graph, declared hybrid; public default is FUTURE/non-gating |
| Evidence | Citation, hierarchy-native, multi-document; graph-native is FUTURE/transport-conditional |
| Authorization | Public, authorized restricted, filtered, direct-identity denied |
| Execution | Complete, error; partial once publicly supported |
| Transport | Embedded, remote |
| Storage | Every advertised storage adapter/profile |
| Cache state | Cold, same-principal warm, both cross-principal orderings |
| Scale | T0, T1, T2; T3 when release-supported |
| Graph fan-out | 1, 10, 100, 1,000 or declared supported maximum |

Pairwise coverage MUST include every pair among supported values. A
machine-generated pairwise coverage report MUST list covered and missing pairs.
No unsupported pair may be silently counted as covered. All seven
future/not-evaluable cases are excluded from current target pairwise coverage
until their applicability requirements are satisfied.

### 5.2 Mandatory higher-order cells

Pairwise coverage does not replace these explicit cases:

- revision boundary x direct scope x unauthorized principal;
- tree-only x hierarchy-native evidence x Markdown;
- graph-only x graph-native evidence x no document citation (**FUTURE**);
- graph-only x hidden restricted intermediate x unauthorized principal
  (**FUTURE**);
- authorized warm x unauthorized probe x shared cache (**FUTURE**);
- unauthorized first x authorized control x shared cache (**FUTURE**);
- embedded x remote x each independently tested mode;
- each source format x each supported parser x citation boundary;
- multi-document evidence x deduplication x tied ranking; and
- high fan-out x as-of filter x authorization filter.

The graph-native, graph-authorization, cache, and last fan-out cells retain
fixture oracles but are **FUTURE** target cells until their declared
capabilities activate.

## 6. Metric definitions

All metrics are computed per case and macro-averaged by case unless a weighted
average is explicitly named. Reports MUST include raw counts and per-mode,
per-format, per-transport, per-storage, and per-authorization slices. All seven
future/not-evaluable cases are excluded from every current target metric
population, denominator, relevance metric, and per-mode slice.

Unless a metric below defines a stronger empty-set rule, a zero denominator is
reported as `not_applicable` and excluded from macro-averages; it is never
coerced to a passing value. For exact set comparisons, two empty sets score
`1`, and exactly one empty set scores `0`.

Let `G_q` be expected stable result IDs for query `q`, `R_q@k` the first `k`
deduplicated returned result IDs, and `rel_q(r)` the gold grade in `[0,3]`.

### 6.1 Ingestion correctness

```text
Document acceptance = accepted supported inputs / supported inputs
Document identity accuracy = exact document IDs / gold document IDs
Revision identity accuracy = exact revision IDs / gold revision IDs
Content integrity = exact normalized content hashes / gold hashes
Section extraction F1 = 2 * section_precision * section_recall
Unsupported-format accuracy =
  correctly rejected unsupported inputs / unsupported inputs
```

Normalization MUST be declared and MUST NOT alter fixture bytes used by
citations. Duplicate input tests verify idempotent identity rather than merely
matching counts.

### 6.2 Hierarchy quality

```text
Node precision = matched hierarchy nodes / candidate hierarchy nodes
Node recall = matched hierarchy nodes / gold hierarchy nodes
Parent accuracy = nodes with exact parent / matched non-root nodes
Depth accuracy = nodes with exact depth / matched nodes
Ancestor-path accuracy = exact root-to-node paths / gold root-to-node paths
Range containment = nodes contained by parent / non-root nodes
Sibling non-overlap = non-overlapping adjacent sibling pairs / sibling pairs
```

Matching uses stable hierarchy node identity, then exact heading, range, type,
and revision. `q05-section-hierarchy` is the fixture gate.

### 6.3 Citation byte-range accuracy

For fixture bytes `B` and citation `[s,e)`:

```text
Exact byte accuracy = count(B[s:e] == UTF8(quote)) / citations
Boundary exactness = count(s == gold_s and e == gold_e) / citations
Range IoU = |[s,e) intersect [gs,ge)| / |[s,e) union [gs,ge)|
Identity accuracy =
  exact (document_id, revision_id, section_id, path) / citations
```

Malformed UTF-8, `s < 0`, `e <= s`, out-of-bounds ranges, or quote mismatch
score zero. Exact byte accuracy is **PROPOSED-V2**, not a current
`SourcePosition.Offset` claim.

### 6.4 Retrieval relevance

```text
Precision@k = |R_q@k intersect G_q| / |R_q@k|
Recall@k = |R_q@k intersect G_q| / |G_q|
Reciprocal rank = 1 / rank(first result with grade > 0), else 0
DCG@k = sum_i=1..k ((2^rel_i - 1) / log2(i + 1))
nDCG@k = DCG@k / IDCG@k, when IDCG@k > 0
```

When `IDCG@k == 0`, `nDCG@k` is `1` only when the deduplicated result list is
empty; otherwise it is `0`. An empty returned list has precision `1` only when
gold is also empty, otherwise `0`. Recall is `1` when both sets are empty.

Across distinct implementations or baselines, permutations within one gold tie
group are relevance-equivalent. That equivalence does not relax deterministic
replay: repeated identical inputs to the same build and configuration MUST
produce the exact same canonical normalized order, including within tie groups.
Results from a lower-quality tier MUST NOT precede a higher-quality tier.
Duplicate `result_id` values after the first are discarded for relevance and
separately counted as a defect:

```text
Duplicate rate = duplicate result occurrences / raw result occurrences
```

### 6.5 Evidence membership and path validity

```text
Evidence membership precision =
  returned (result_id, evidence_id) memberships in gold / returned memberships
Evidence membership recall =
  returned gold memberships / gold memberships
Path validity =
  structurally continuous, temporally valid paths / returned paths
Path evidence coverage =
  path steps supported by required evidence / returned path steps
Endpoint accuracy =
  paths with exact source and target / returned paths
```

Relationship paths require joining adjacent endpoints. Graph paths require each
edge to connect adjacent nodes. Hierarchy paths require each next node's parent
to equal the previous node. A path that crosses an invisible or temporally
ineligible intermediate is invalid even when both endpoints are visible.

### 6.6 Cross-document graph usefulness

For paired graph-enabled and graph-disabled runs with identical visible corpus:

```text
Cross-document success =
  correctly completed multi-document graph tasks / graph-eligible tasks
Graph evidence gain =
  task_success_graph_enabled - task_success_graph_disabled
Useful path rate =
  valid returned paths contributing to a correct result / returned paths
```

The graph-enabled run MUST add supported evidence, not merely different prose.
`q06-cross-document-path` tests citation-supported cross-document relationships.
`q11-graph-native-navigation` tests graph-only evidence. Current gRPC scoring of
the latter remains `not_evaluable_future_anchor_contract`, and q11 is excluded
from current conformance, parity, relevance, and release counts until
`public_graph_native_anchor_v1` and `public_graph_navigation_v1` are advertised.

### 6.7 Deterministic ordering

Normalize each response to a canonical ordered sequence of stable result IDs,
ordered evidence IDs, ordered path steps, and tie groups. Canonicalization
normalizes representation only; it MUST NOT sort away or otherwise conceal the
order returned by the target.

```text
Replay mismatch rate =
  identical-input replays whose exact canonical normalized sequence differs
  / identical-input replay comparisons
Cross-process mismatch rate =
  identical-input process pairs with differing exact canonical normalized order
  / identical-input process comparisons
Tie violation rate =
  lower-tier-before-higher-tier pairs / comparable result pairs
```

Run each deterministic case 100 times in one process, 20 fresh processes, and
with at least three input enumeration permutations. Every repeated run of each
identical input must match exactly, including within ties. Tie permutations may
be relevance-equivalent only when comparing distinct implementations or
baselines; they are always replay mismatches for identical runs. Scores alone
are not stable identities and MUST NOT be used as final tie-breakers.

### 6.8 Revision and as-of correctness

Eligibility is:

```text
valid_from <= as_of
and (valid_to_exclusive is null or as_of < valid_to_exclusive)
```

```text
As-of selection accuracy =
  queries returning only the exact eligible revision set / temporal queries
Boundary accuracy =
  exact boundary cases selecting the inclusive successor / boundary cases
Temporal leakage rate =
  ineligible revision or graph records returned / returned temporal records
```

Tests include one tick before, exactly at, and one tick after every boundary,
open-ended intervals, all-revision mode, and revision-specific direct scope.

The delivered fixture defines one tick as exactly one second and covers the
protocol boundary `2026-03-15T00:00:00Z` explicitly:

- `q17-one-tick-before-boundary` uses `2026-03-14T23:59:59Z` and selects `r1`;
- `q03-revision-boundary` uses exactly `2026-03-15T00:00:00Z` and selects `r2`;
- `q18-one-tick-after-boundary` uses `2026-03-15T00:00:01Z` and selects `r2`.

### 6.9 Authorization isolation

Execution completeness and visibility are separate:

- `complete` means successful execution over everything visible to the
  principal, including a filtered empty result;
- `partial` means an execution-level recoverable failure or truncation;
- `error` means request failure;
- authorization `allow`, `filtered`, and `denied` are policy decisions.

A failed mode or shard is an execution failure and is never reclassified as
authorization filtering.

```text
Forbidden record leaks = visible forbidden IDs in delivered output
Canary leaks = delivered canary tokens with no allowed origin
Cross-principal contamination =
  responses differing from isolated-cache control / cross-principal probes
Authorized suppression =
  authorized controls missing expected results after unauthorized priming
```

Every applicable count MUST be zero. Under the current public retrieval/gRPC
surface, only currently applicable authorization cases contribute to release
counts; graph-native canary, contamination, and suppression counts remain
future until their capability sets activate.

Delivered future-oracle canary coverage is strictly limited to:

| Delivered channel | Record kind |
| --- | --- |
| `graph_node` | Restricted graph node |
| `graph_edge` | Restricted graph edge |
| `graph_path` | Restricted path with hidden intermediate |

Canary origins are only declared `node.canary_token`, `edge.canary_token`, and
`path.canary_token` fields. Query text, `top_k`, modes, principal, filters, and
scope are excluded input origins. A token copied from excluded request metadata
is not evidence of protected-corpus leakage.

The desired future cache probes are:

1. `q12-authorized-canary-warm` then
   `q13-unauthorized-canary-probe`: no leak and no poisoning.
2. `q15-unauthorized-cache-prime` then
   `q16-authorized-cache-verify`: no leak, no poisoning, and no suppression of
   the authorized node, edge, or hidden-intermediate path.

The second run in each pair MUST share the candidate cache with the first.
Each case also runs against an isolated cache as its control.

These sequences preserve precise future expected behavior, but neither is a
current retrieval/gRPC, embedded/remote parity, relevance, or release gate.
They activate only after the target advertises
`public_graph_native_anchor_v1`, `public_graph_navigation_v1`,
`public_authorization_filter_v1`, and
`public_principal_partitioned_cache_v1`.

For both unauthorized cases, `q13` and `q15`, the complete forbidden-record
closure is:

- `node:umber-vault` and `edge:umber-links-opal`;
- hidden intermediate `node:opal-bridge`;
- `edge:jade-routes-opal` and `edge:opal-routes-silver`; and
- `path:jade-via-opal-to-silver`.

The leak oracle covers every restricted node and edge referenced by the
authorized path, not only the three records carrying canary tokens or returned
as top-level authorized results.

**FUTURE required T0 expansion:** add independent canaries for document,
revision, section, span, source, explanation, and error-body channels.
Until those fixture origins exist, no release report may claim those channels
are covered.

### 6.10 Embedded, remote, and storage parity

Normalize away only documented transport metadata. Compare:

```text
Response parity =
  exact normalized matches / cases successful in both configurations
Error parity =
  matching public error category / cases failing in both configurations
Availability parity =
  cases with the same success-or-error disposition / all parity cases
```

Exact normalized response parity includes result IDs, order/tie groups,
document and revision IDs, evidence membership, citations, graph paths, nil
versus empty normalization, finite scores, and UTF-8. Error parity includes
invalid input, authorization denial, cancellation, and deadline behavior.

The current embedded/remote parity population is exactly the 12
`current_evaluable` cases. `q11`, `q12`, `q13`, `q15`, and `q16` are excluded
along with `q14` and `q19`; populated desired graph-native/cache oracles do not
override their all-false gate eligibility. Each excluded case joins parity only
after every capability in its applicability record is advertised.

Every mode MUST be tested independently in embedded and remote configurations;
one hybrid parity test is insufficient. Every advertised storage adapter MUST
run the same conformance pack. Storage-specific ordering, stale revisions, or
authorization behavior is a parity failure, not an allowed implementation
difference.

### 6.11 Performance and scale

All latency is client-observed wall time from request dispatch to the observed
terminal success, error, cancellation, or timeout. The latency population
contains every measured request; outcome is not a latency filter. Ingestion
throughput intervals begin before the first accepted byte and end after
durable/queryable completion.

```text
Ingestion throughput = successfully queryable source MiB / elapsed seconds
Terminal query throughput = requests reaching any terminal outcome
  / steady-state seconds
Successful query throughput = successful requests / steady-state seconds
p95/p99 latency = empirical percentile over all measured request durations
Success rate = successful requests / measured requests
Error rate = non-timeout errors / measured requests
Timeout rate = timed-out requests / measured requests
Scale correctness = exact passing sampled cases / sampled scale cases
Fan-out amplification =
  candidate graph records examined or returned / gold contributing records
```

Reports MUST separate cold start, cold cache, and warm cache; concurrency;
corpus size; graph size; fan-out; result limit; transport; storage; and machine
profile. Reliability rates are reported separately from latency percentiles.
Errors retain their observed duration. A request timeout uses the observed
duration from dispatch until its deadline/cancellation outcome. A request still
nonterminal at the declared hard harness cutoff `H` is forcibly cancelled,
classified as a timeout, and assigned duration `H`; the same declared `H` MUST
be used for every compared configuration. No error or timeout may be deleted,
zeroed, or replaced with a successful-request duration to improve p95 or p99.

### 6.12 User task completion and explainability

Deterministic tasks use exact expected facts and evidence:

```text
Task completion =
  tasks with all required facts and no contradictory fact / tasks
Evidence-supported fact rate =
  correct facts traceable to valid visible evidence / returned factual claims
Unsupported claim rate =
  factual claims lacking valid visible evidence / returned factual claims
Explanation coverage =
  results with traceable mode, result identity, and evidence / returned results
```

Model-assisted answer fluency or synthesis is evaluated separately. It cannot
override failed retrieval, citation, temporal, path, or authorization gates.

## 7. Release gates

All seven future/not-evaluable cases contribute no current target hard-gate,
relevance, ranking, parity, regression, performance, or release-decision
outcome. Closed-schema and deterministic-oracle validation of their desired
records is fixture-integrity validation only, not a target behavior gate.

### 7.1 T0 deterministic correctness and security gates

All are hard pass/fail gates:

| Gate | Threshold |
| --- | ---: |
| Strict fixture validation | 100% delivered records accepted; 100% mutation classes rejected |
| Supported document acceptance | 100% |
| Document/revision/content identity | 100% exact |
| Unsupported-format handling | 100% correct, with explicit diagnostic |
| Independent parser agreement with adjudicated gold | 100% for both parsers |
| Hierarchy node/parent/depth/containment | 100% on fixture |
| Fixture citation byte and identity accuracy | 100% |
| Evidence membership precision and recall | 100% on currently applicable target cases; future oracles validated structurally |
| Relationship and hierarchy path validity | 100% on currently applicable target cases |
| Graph-native evidence/path validity | FUTURE; 100% after required capabilities activate |
| Temporal and revision boundary accuracy | 100% |
| Temporal leakage | 0 |
| Deterministic replay/cross-process mismatch | 0 |
| Tie violations and duplicate result rate | 0 |
| Current authorization isolation (`q09`, `q10`) | 0 forbidden-record leaks |
| Graph-native canary leak, contamination, or authorized suppression | FUTURE; 0 after all required capabilities activate |
| Both graph-native cache orderings | FUTURE; 100% exact after all required capabilities activate |
| Current execution-state mapping | 100%; no synthesized partial response |
| Embedded/remote normalized response and error parity | 100% across the 12 currently applicable cases |
| Storage conformance parity | 100% for every release-supported adapter |

Current T0 authorization coverage is limited to applicable `q09` and `q10`.
The graph-native canary/cache oracles in `q12`, `q13`, `q15`, and `q16` remain
future and transport-conditional until graph-native anchor, graph navigation,
authorization-filter, and principal-partitioned-cache capabilities are all
advertised. Document, revision, section, span, source, explanation, and
error-body canaries remain additional future T0 expansions. Release notes MUST
disclose all of these gaps.

### 7.2 Relevance and usefulness gates

Apply to applicability-current cases in the blind release set, with per-mode
results. All seven future/not-evaluable cases are excluded:

| Metric | Overall gate | Per-mode floor |
| --- | ---: | ---: |
| Macro nDCG@k | >= 0.90 | >= 0.80 |
| Macro Recall@k | >= 0.90 | >= 0.80 |
| Mean reciprocal rank | >= 0.90 | >= 0.80 |
| Task completion | >= 0.95 | >= 0.85 |
| Evidence-supported fact rate | >= 0.98 | >= 0.95 |
| Unsupported claim rate | <= 0.01 | <= 0.03 |
| Cross-document graph task success | >= 0.90 | >= 0.85 for graph mode |
| Useful graph path rate | >= 0.95 | >= 0.90 |
| Graph evidence gain on graph-eligible pairs | >= +0.10 absolute | no negative per-task regression |

Report exact 95% bootstrap confidence intervals with 10,000 resamples.
For every minimum or "at least" gate, use the lower confidence bound; passing
requires the point estimate to meet the gate and the lower bound to be no more
than 0.03 below it. For every maximum or "at most" gate, use the upper
confidence bound; passing requires the point estimate to meet the gate and the
upper bound to be no more than 0.03 above it. Thus unsupported-claim rate uses
its upper bound, never its lower bound. Authorization and deterministic
exactness never use confidence allowances.

### 7.3 Regression gates

Against the last accepted release on the identical blind dataset:

- no hard-gate regression;
- no overall nDCG, recall, MRR, or task-completion drop greater than 0.02
  absolute;
- no per-mode drop greater than 0.03 absolute for current concrete modes;
- no p95 or p99 latency regression greater than 10%;
- no throughput regression greater than 10%; and
- no new unsupported format/parser pair.

Define each degradation as a positive regression magnitude (quality or
throughput drop, or latency increase). Because these are maximum-allowed gates,
their upper confidence bounds determine passing; a lower bound MUST NOT be used
to approve a possible regression.

If a quality improvement intentionally changes gold ordering, the gold revision
must be reviewed independently and both releases rerun on old and new gold.

## 8. Deterministic and model-assisted boundaries

### 8.1 Deterministic release path

The following MUST run without a generative model:

- fixture validation, hashing, range slicing, hierarchy, and parser comparison;
- lexical baseline;
- explicit tree and graph traversal;
- revision/as-of filtering;
- authorization and cache fixture probes, with target verdicts conditioned on
  applicability;
- stable result normalization, deduplication, path validation, and metrics;
- embedded/remote/storage parity over applicability-current cases; and
- latency, throughput, scale, and fan-out measurement.

A vector implementation MAY require a fixed embedding artifact, but the
artifact version and hash MUST be pinned. It cannot serve as the no-model
baseline.

The minimum no-model baseline is:

1. lexical exact/token retrieval;
2. deterministic hierarchy traversal;
3. deterministic graph traversal;
4. deterministic authorization filtering; and
5. stable fusion by declared rule when a combined baseline is reported.

### 8.2 Model-assisted evaluation

Model-assisted synthesis is a separate, non-security layer. Runs MUST pin model
identifier, provider-neutral configuration, prompt hash, sampling parameters,
tool policy, and seed where supported. Each task runs at least five times.

Human adjudication uses two blinded raters and a third for disagreements.
Report agreement (Cohen's kappa for two raters or Krippendorff's alpha for
variable raters); `>= 0.80` is required before a rubric becomes release-gating.

Model-assisted pass gates are:

- task completion `>= 0.85`;
- evidence-supported fact rate `>= 0.95`;
- unsupported claim rate `<= 0.03`; and
- run-to-run task verdict agreement `>= 0.90`.

No model judgment may excuse an authorization leak, wrong revision, invalid
path, or citation mismatch.

## 9. Regression-test layering

| Layer | Scope | Trigger | Required outcome |
| --- | --- | --- | --- |
| L0 | Schema, hashes, bytes, references, mutations | Every change | Exact |
| L1 | Public document/graph/retrieval contract units | Every change | Exact |
| L2 | Candidate/reference parser and adapter components | Parser/adapter change | Exact |
| L3 | Embedded black-box applicability-current cases by independent mode | Every change | Current T0 exact gates |
| L4 | Remote black-box applicability-current cases by independent mode | Every change | Current T0 exact gates and parity |
| L5 | Each storage adapter; future cache orderings after capability activation | Storage/auth/cache change; nightly | Current gates now; cache gates when activated |
| L6 | Applicability-current blind relevance and task suite | Pull request sample; full nightly/release | Relevance gates |
| L7 | T0-T2 performance, scale, and fan-out | Nightly/release | Performance gates |
| L8 | Model-assisted synthesis and human audit | Scheduled/release | Separate model gates |

Failures MUST retain normalized input, output, expected record IDs, adapter
metadata, and seed. Protected oracle values MUST be redacted from candidate
logs while remaining available to the evaluator.

## 10. Benchmark scale tiers

Generated scale corpora use a fixed generator version and seed, preserve the
same schema invariants, and include sampled exact gold.

| Tier | Documents | UTF-8 corpus | Revisions | Graph nodes/edges | Max tested fan-out | Purpose |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| T0 fixture | 3 (4 revision files) | < 1 MiB | 4 | 18 / 15 | 10 (fixture oracle; target q19 FUTURE) | Per-change correctness |
| T1 small | 10,000 | 1 GiB | 20,000 | 100,000 / 500,000 | 100 | Pull request/nightly |
| T2 medium | 100,000 | 10 GiB | 250,000 | 1,000,000 / 5,000,000 | 1,000 | Release |
| T3 large | 1,000,000 | 100 GiB | 2,500,000 | 10,000,000 / 50,000,000 | Declared maximum | Qualification/report-only until supported |

On the declared reference machine and storage profile:

| Tier | Warm query p95 | Warm query p99 | Minimum query throughput | Scale correctness |
| --- | ---: | ---: | ---: | ---: |
| T0 | <= 250 ms | <= 500 ms | >= 20 requests/s | 100% |
| T1 | <= 750 ms | <= 1.5 s | >= 50 requests/s | >= 99.9% |
| T2 | <= 2.5 s | <= 5 s | >= 25 requests/s | >= 99.9% |
| T3 | Report | Report | Report | >= 99.9% sampled |

The p95/p99 cells use all measured durations, including errors and timeouts
under the timeout rule in Section 6.11. Their upper 95% confidence bounds MUST
meet the at-most thresholds. Minimum throughput and scale-correctness cells use
their lower 95% bounds. Reliability remains a separately reported distribution
of success, error, and timeout outcomes.

T1 and T2 ingestion MUST sustain at least 2 MiB/s and complete with zero lost
or duplicate logical revisions. These absolute targets are provisional until a
reference machine manifest is checked in; the 10% release-to-release regression
gate applies regardless.

At T1 through T3, graph tests MUST separately measure fan-outs 1, 10, 100, and
the tier maximum, deduplicating repeated values. Query latency and records
examined MUST be reported by fan-out; averaging low and high fan-out cases is
prohibited.

At T0, the value 10 is grounded by `oracle:graph-fan-out-10`: the validator
checks the hub, 10 paired outgoing edges and leaves, absence of any other
outgoing hub edge, and deterministic edge-ID order. It is not a current target
performance or release gate because `q19` cannot be executed through the
current public request contract.

## 11. Explainability output

A backend-neutral evaluation trace SHOULD contain:

- case and stable result IDs;
- declared request modes and adapter mapping;
- selected document/revision, hierarchy node, or graph record IDs;
- evidence IDs and path step IDs;
- authorization disposition without hidden scope or forbidden IDs;
- deterministic ordering/tie rationale; and
- elapsed stage timings.

The trace is evaluated for identity and evidence coverage, not wording.
Explanations MUST NOT expose protected records, scope names, cache keys, hidden
intermediates, prompts, credentials, or proprietary internals.

## 12. Prioritized implementation backlog

### P0 - required before the deterministic gate is credible

1. Implement the closed-schema validator and mutation pack; accept every
   delivered record and reject every unlisted or mistyped field.
2. Implement corpus hash, UTF-8 byte-slice, section ownership, hierarchy,
   relationship, graph, deterministic fan-out-10 oracle, ranking, and temporal
   validators.
3. Implement the one-request adapter, including `q10` direct document scope,
   current all-or-error mapping, and explicit unsupported handling.
4. Implement stable result/evidence normalization, multi-document evidence
   membership, deduplication, tie-group comparison, and nDCG zero-IDCG behavior.
5. Run lexical, vector, tree, graph, and declared hybrid independently.
   Apply target gates only to applicability-current cases; preserve all seven
   future cases and their desired oracles until activation.
6. Implement candidate/reference parser lineage recording and the versioned
   format/parser support relation.
7. Implement current authorization isolation with `q09` and `q10`. Preserve
   `q12`-`q13` and `q15`-`q16` as future shared-cache,
   hidden-intermediate, zero-leak oracles without adding them to current gates.
8. Add negative fixture packs for malformed bytes, invalid ranges, duplicate
   IDs, broken hierarchy, broken paths, invalid temporal intervals, bad mode
   declarations, authorization leaks, and poisoned caches.
9. Add future T0 canary fixtures for document, revision, section, span, source,
   explanation, and error-body delivery channels.
10. Publish exact embedded and remote conformance commands in CI.

### P1 - required for release qualification

1. Add blind and challenge datasets with adjudicated gold and leak-resistant
   family splits.
2. Add all advertised storage adapters to the same conformance and parity
   matrix.
3. Generate pairwise coverage and fail CI on uncovered supported pairs.
4. Add negative scale packs for truncated ingestion, unavailable shards,
   corrupt indexes, retry duplication, and high fan-out authorization.
5. Implement T1/T2 performance runs, cold/warm separation, and regression
   baselines.
6. Add versioned public graph-native anchor and navigation capabilities, plus
   authorization-filter and principal-partitioned-cache capabilities for
   `q12`-`q16`; activate each case only when its complete capability set is
   advertised, then add it to gRPC conformance and parity.
7. Add public partial-execution metadata and cases that distinguish failed
   modes/shards from authorization filtering.

### P2 - quality expansion

1. Add model-assisted synthesis and blinded human adjudication.
2. Add multilingual, non-ASCII, very large section, and additional source
   format packs with independent reference parsers.
3. Add longitudinal user-task studies and calibrated explainability rubrics.
4. Qualify T3 and adversarial graph topologies.

## 13. Release report and decision rule

A release report MUST include:

- source commit and build identity;
- fixture version and whole-tree hash;
- candidate/reference parser and lineage metadata;
- mode, transport, storage, authorization, cache, and scale matrix;
- raw per-case outcomes and metric numerators/denominators;
- confidence intervals where allowed;
- isolated and shared-cache authorization results;
- embedded/remote/storage parity diffs;
- latency/throughput distributions and machine manifest;
- unsupported combinations and all **FUTURE** gaps; and
- comparison with the last accepted release.

The release passes only when every applicable hard gate passes, every supported
matrix pair is covered, relevance and performance gates pass, and no result is
silently omitted. `not_evaluable` is an explicit disclosed state, never a pass.
All seven future/not-evaluable case statuses are disclosed but do not
participate in the current release decision.
Any authorization leak, wrong as-of revision, invalid evidence path, strict
schema failure, or deterministic parity mismatch is an unconditional failure.
