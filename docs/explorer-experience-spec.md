# Shoal Explorer experience specification

> Status: proposed product experience
>
> Audience: product, design, CLI/API, accessibility, documentation, and
> implementation teams
>
> Scope: architecture-neutral exploration of versioned documents and general
> knowledge graphs

## 1. Purpose

Shoal Explorer is an evidence-first interface for creating or opening a
knowledge corpus, observing ingestion, navigating hierarchical sources,
exploring relationships, and comparing retrieval strategies. It should help a
person answer:

1. What knowledge is visible to me?
2. How is it organized and versioned?
3. What relationships connect it?
4. Why did this query return these results?
5. Can I verify the result against exact evidence?

Explorer is not a database console or a generic node-link visualizer. It
presents documents, revisions, sections, spans, graph neighborhoods, evidence,
and explained retrieval without exposing parser, model, retrieval engine,
storage, or transport implementation.

## 2. Public grounding and claim policy

This specification is grounded in the following public Shoal material:

- `FEATURES.md` for capabilities described as shipped primitives;
- `docs/platform-product-plan.md` for the accepted evidence-first,
  local-to-remote product direction;
- `docs/distributed-vector-index.md` for freshness, explicit fallback,
  cancellation, deterministic ranking, and recall-claim behavior;
- `docs/explorer-public-contract.md` for adopted storage-neutral identity,
  byte-range, validation, retrieval normalization, ordering, seed, and error
  semantics;
- `pkg/document/document.go` for revisions, hierarchy, spans, ranges, and
  citations;
- `pkg/graph/graph.go` for schema-neutral nodes, directed edges, weights, and
  connected paths;
- `pkg/retrieval/retrieval.go` for lexical, vector, tree, and graph modes,
  hybrid requests, scope, as-of retrieval, evidence, scores, and explanations.

The following labels are normative:

- **Current - public contract:** a public type or invariant exists. This does
  not establish an end-to-end adapter or Explorer workflow.
- **Current - shipped primitive:** public status documentation describes an
  underlying mechanism as shipped. This does not establish an Explorer
  surface.
- **Current - embedded implementation:** `pkg/explorer` implements the
  operation locally. This does not establish a storage-neutral repository,
  remote parity, or Accumulo conformance.
- **Alpha - proposed surface:** required for the first Explorer product slice;
  not a claim of current implementation.
- **Future - proposed:** intentionally beyond Alpha.
- **Dependency-blocked:** the surface must not be claimed until a named public
  contract or semantic dependency exists.

When implementation evidence is absent, the specification says "proposed" or
"dependency-blocked." It never infers support from a type name, roadmap item,
or lower-level primitive.

## 3. Transferable PageIndex product research

Public PageIndex material provides a useful experience benchmark, not an
architecture prescription:

- Its public project describes a simple sequence of submit a document, inspect
  its hierarchy, and ask questions. The hierarchy exposes section titles,
  ranges, and summaries, making structure inspectable before retrieval
  ([official project](https://github.com/VectifyAI/PageIndex)).
- Its public design article explains navigation through a document table of
  contents and iterative selection of relevant sections. The transferable
  pattern is progressive, hierarchy-aware exploration with explicit source
  references
  ([public introduction](https://pageindex.ai/blog/pageindex-intro)).
- Its public SDK describes local and cloud use through similar high-level
  client methods, while clearly listing capability differences such as
  citation granularity and OCR. The transferable pattern is one semantic
  workflow with honest target capability differences
  ([official project](https://github.com/VectifyAI/PageIndex)).
- Its public material offers chat, API, MCP, and agent-tool entry points. The
  transferable pattern is a small task-oriented surface that can serve both
  people and agents
  ([developer page](https://pageindex.ai/developer),
  [public documentation](https://docs.pageindex.ai/quickstart)).

Shoal does **not** adopt PageIndex's retrieval architecture. In particular,
Shoal Explorer does not require PDFs, a tree-only index, model-driven tree
search, a vectorless design, a particular extraction technique, or a hosted
service. Shoal retains independently selectable lexical, vector, tree, graph,
and hybrid retrieval. It supports non-document knowledge as graph-native data
without claiming that graph-only retrieval evidence is currently representable.

The transferable quality bar is:

- hierarchy is directly inspectable;
- source references are first-class;
- local and remote workflows use the same product language;
- capability differences are explicit;
- a one-line task surface expands into inspectable evidence and explanation.

## 4. Adversarial review controls (RC-01 through RC-09)

### RC-01: four-state capability ledger

Every advertised capability has exactly one primary state:

| Capability | State | Horizon | Evidence or blocking dependency |
| --- | --- | --- | --- |
| Document, revision, section, span, range, citation values | Public contract | Current | `pkg/document` |
| Node, edge, weight, and connected path values | Public contract | Current | `pkg/graph` |
| Retrieval request/result/evidence/explanation values | Public contract | Current | `pkg/retrieval` |
| Retrieval normalization, bounds, deterministic order, and seed-shape rules | Public contract | Current | `pkg/retrieval`; `docs/explorer-public-contract.md` |
| Document predicates, graph traversal, lexical, exact vector, approximate vector | Shipped primitive | Current | `FEATURES.md`; not an Explorer claim |
| Embedded corpus create/open and direct ingestion | Embedded implementation | Current | `pkg/explorer`; not a storage-neutral or remote repository contract |
| Ingestion job and readiness experience | Dependency-blocked | Alpha | No reviewed public ingestion/status contract |
| Embedded document hierarchy and span navigation | Embedded implementation | Current | `pkg/explorer`; canonical source-byte hydration and remote/storage-neutral conformance remain dependency-blocked |
| Embedded both-direction graph neighborhood workspace | Embedded implementation | Current | `pkg/explorer`; depth is 1..16 and final values order by ID |
| Embedded citation-backed retrieval | Embedded implementation | Current | `pkg/explorer`; vector requires an explicitly configured embedder and complete compatible stored span embeddings; explicit `AsOf` returns `unavailable` |
| Ask with generated prose over citation-backed evidence | Proposed surface | Alpha | Optional composition outside required Shoal runtime |
| Explicit partial-result UI | Dependency-blocked | Future | Current response has no completeness envelope |
| Authorization-aware result semantics | Dependency-blocked | Future | Current retrieval contract has no authorization/restriction envelope |
| Embedded/remote workspace continuity | Dependency-blocked | Alpha | Requires public capability and identity negotiation |
| General non-document graph-native browsing | Proposed surface | Alpha | Graph contract supports nodes, edges, properties, and paths |
| Graph-only retrieval evidence and Ask | Dependency-blocked | Future | Requires a public graph-native evidence anchor/provenance variant |

The ledger is updated when public contracts or status evidence changes.
Marketing text and UI labels must derive from this ledger.

### RC-02: parser and model neutrality

- A source adapter may expose source-specific options, but corpus and ingestion
  concepts must not require a particular parser, OCR system, layout detector,
  summarizer, embedding model, or generator.
- Ingestion stages use neutral names: accepted, discovering, materializing,
  indexing, validating, and ready.
- Parser and model details appear only as optional provenance.
- Search works without answer generation.
- Ask works in evidence-only mode when no model is configured.
- Generated prose is an Explorer or client composition; it is not a required
  storage or retrieval capability.

### RC-03: retrieval, storage, and transport neutrality

- Lexical, vector, tree, graph, and hybrid modes remain independently
  selectable. Explorer must not encode one strategy as the architecture.
- A corpus is a logical knowledge boundary, not a table, file, bucket, index,
  or directory.
- Saved workspaces contain corpus, object, revision, request, and filter
  identities, not storage or transport messages.
- Embedded and remote targets expose the same coarse product operations.
- Target-specific implementation details may be diagnosed outside the normal
  workflow but are not primary navigation or copied evidence.

### RC-04: graph-only evidence limitation

`retrieval.Evidence` currently contains a value-shaped `document.Citation`.
`document.Citation.Validate` requires a document ID, revision ID, a section or
span ID, and a valid range. A graph-only path therefore has no valid way to
express "citation absent" in the current Citation-shaped contract.

Alpha rules:

- Never invent a document, revision, range, page, or quote for graph-only
  knowledge.
- Graph-native browsing may display nodes, edges, properties, weights, and
  paths outside the retrieval evidence surface.
- Do not accept or render an invalid or absent citation as successful
  retrieval evidence, even when a graph path is present.
- Do not call graph metadata a quote or source citation.
- Retrieval evidence and Ask over graph-only knowledge are dependency-blocked
  until a public graph-native anchor/provenance variant exists.

Future contract direction:

```text
EvidenceAnchor =
    DocumentCitation
  | GraphPathAnchor
  | ExternalReference
```

This should be a discriminated or optional shape, not a zero-value citation
convention.

### RC-05: current all-or-error semantics and future partial results

The current retriever returns `(Response, error)`. `Response` has no
completeness field, missing-stage list, or partial-ranking semantics. Public
vector lifecycle documentation also states that cancellation prevents partial
results from being returned.

Current and Alpha rules:

- Treat retrieval as all-or-error.
- Do not infer partial success from a non-empty response accompanied by an
  error.
- Do not stream provisional ranked results unless a future contract defines
  their ordering and completeness.
- Timeout and cancellation are failures, not implicit partial responses.
- Stale approximate retrieval fails or uses an explicitly enabled exact
  fallback; it never silently returns stale or partial data.

Future partial-result envelope:

```text
completeness: complete | partial
reasons: [structured reason]
completed_stages: [...]
missing_stages: [...]
global_ranking_valid: true | false
retryable: true | false
```

Explorer may show a partial result only when all applicable fields are
explicitly supplied. Execution completeness is independent of authorization:
a response fully executed for one principal is complete even when future
authorization semantics omit items that principal cannot access. Such
omission is not an execution-partial response. `partial` is reserved for the
future execution-completeness envelope above.

### RC-06: authorization is future semantics, not UI filtering

Shoal public documentation describes lower-level visibility behavior, but the
reviewed retrieval request has no principal or authorization field and the
response has no restriction summary.

Therefore:

- Alpha filter controls are query filters, not security controls.
- Hiding rows in the UI is never described as authorization enforcement.
- Local execution is labeled **Local process context; Explorer authorization
  semantics unavailable** unless a target advertises otherwise.
- Remote authentication state may be shown, but Explorer must not claim that
  result counts are authorization-filtered without a public response semantic
  that says so.
- "No visible results," hidden-count messaging, restricted path segments, and
  authorization-safe sharing remain dependency-blocked until the server
  supplies enforceable semantics and restriction metadata.

Future authorization must be enforced at the serving boundary. The response
should state the effective authorization context and whether counts,
documents, nodes, paths, or evidence were restricted without leaking
prohibited identities. Authorization restriction metadata is orthogonal to
execution completeness: a response may be complete for the principal while
containing fewer items than another principal would receive.

### RC-07: normalized public defaults

`retrieval.Request.Normalize` now defines stable shorthand:

- empty `Modes` becomes exactly lexical;
- `TopK == 0` becomes 20;
- duplicate modes and scope IDs collapse by first occurrence.

The visual surface MAY accept that shorthand but SHOULD display the normalized
effective mode and limit. Saved requests SHOULD persist normalized values so
reviews and replays remain explicit. A target still must not claim support for
vector, `AsOf`, tree, or graph shapes that it cannot execute; unsupported
requested behavior returns `unavailable` rather than fallback.

### RC-08: capability negotiation

The reviewed contracts do not define a capability handshake, so the Alpha
surface is dependency-blocked on one.

A target capability response should report semantic support for:

- corpus list/create/open;
- ingestion start/status/retry/cancel;
- document list/revisions/tree/spans/source hydration;
- graph node/edge/neighborhood/path operations and bounds;
- lexical, vector, tree, graph, and hybrid retrieval;
- exact and approximate retrieval, freshness checks, and fallback;
- as-of time;
- explanations and bounded traces;
- citation validation and quote hydration;
- partial-result semantics;
- authorization/restriction semantics;
- maximum top-K, hop, page, span, and request limits;
- contract versions and compatible identity semantics.

Unknown capabilities fail closed. The UI disables unsupported actions with a
reason instead of probing support through destructive operations.

### RC-09: non-document knowledge

A corpus may contain entities, events, code symbols, records, media artifacts,
or consumer-defined nodes with no document root.

- Graph-native browsing and inspection work without a Documents view.
- Node kind, labels, properties, directed edges, weights, and paths remain
  available.
- Empty document state says **No document evidence supplied**, not **Corpus
  empty**.
- Retrieval and Ask over graph-only knowledge remain dependency-blocked until
  a public graph-native evidence anchor/provenance variant exists.
- Page, section, span, and quote controls disappear when inapplicable.
- No non-document object is forced into a fake document citation.

## 5. Target users and use cases

### Knowledge investigator

Searches a corpus, follows cross-document relationships, compares strategies,
and verifies findings against immutable evidence.

### Document analyst

Navigates long sources by revision, ordered section hierarchy, span, source
offset, and optional page.

### Graph investigator

Explores entities, events, and relationships; filters neighborhoods; and
inspects bounded graph paths without requiring documents. These paths are
graph-native data, not retrieval evidence under the current public contract.

### Retrieval evaluator

Runs lexical, vector, tree, graph, and hybrid strategies under identical
scope, as-of time, filters, and top-K.

### Corpus curator

Creates or opens a corpus, submits sources through configured adapters, and
observes readiness, warnings, failures, and revisions.

### Agent or application developer

Reproduces visual workflows through coarse CLI/API operations and consumes
structured citations, scores, paths, explanations, and request IDs.

## 6. Product principles

1. **Evidence before visualization.** Every retrieval result leads to valid,
   inspectable evidence. Graph-native browsing may show uncited graph data but
   must not present it as retrieval evidence.
2. **Hierarchy and graph are complementary.** Tree edges preserve source
   organization; graph edges add cross-document and non-document context.
3. **Exact identity over display labels.** Documents, revisions, sections,
   spans, nodes, edges, and requests retain stable IDs.
4. **No silent fallback.** Unsupported modes, stale indexes, missing evidence,
   and failed hydration remain visible.
5. **Semantic continuity.** Local and remote workspaces use the same product
   concepts while capability differences remain explicit.
6. **Progressive disclosure.** A simple search or open action expands into
   evidence, provenance, scores, and trace without requiring those details
   upfront.

## 7. Information architecture

### Global shell

- Target: embedded or remote profile
- Corpus: current logical corpus
- Time context: latest or selected as-of time/revision
- Capability status: compatible, limited, incompatible, or unknown
- Connection identity: informational until authorization semantics exist
- Global Search/Ask entry
- Activity indicator
- Help and maturity legend

### Primary navigation

1. **Overview**
   - corpus identity and description;
   - safely available counts;
   - capability/readiness summary;
   - warnings and recent activity.
2. **Ingestion**
   - sources, operations, stages, failures, retries, and produced artifacts.
3. **Documents**
   - document list, revision selector, hierarchy, source, and citations.
4. **Graph**
   - node lookup, neighborhoods, relationship filters, paths, and table view.
5. **Explore**
   - Search, optional Ask, strategy comparison, filters, evidence, and trace.
6. **Activity**
   - semantic request and operation history, not raw engine logs.

Storage and cluster administration are not Alpha navigation.

## 8. End-to-end journeys

### 8.1 Create or open a corpus

**Maturity:** Alpha - proposed surface; dependency-blocked on corpus and
capability contracts.

1. Select a target profile.
2. Run capability negotiation.
3. Create a corpus with name, optional description, and neutral metadata, or
   open a visible corpus by immutable ID.
4. Show effective capabilities and unknowns before the user proceeds.
5. Land on an explicit empty state. Creation does not imply ingestion or index
   readiness.
6. Preserve workspace state only when corpus and object identities are
   compatible.

### 8.2 Ingest and observe readiness

**Maturity:** dependency-blocked.

1. Select **Add source**.
2. Choose a configured source adapter, not a parser or storage engine.
3. Preview source identity and whether this is a new source version.
4. Submit an idempotent operation.
5. Show accepted, discovering, materializing, indexing, validating, and ready
   stages.
6. Link completion to document revisions, graph artifacts, retrieval
   readiness, warnings, and exclusions.
7. Retry unchanged work with the same idempotency identity.
8. Materialize a changed source as a new immutable revision.

### 8.3 Navigate a document and revision

**Maturity:** Alpha - proposed surface over public document contracts.

1. Open a document.
2. Pin a revision or as-of time.
3. Expand its ordered section tree.
4. Select a section to highlight its source range and attributable spans.
5. Display `[start offset, end offset)` exactly.
6. Display one-based pages when known; page zero means **Page unavailable**.
7. Copy a citation containing document, revision, section/span, range, and
   verified quote when available.

Following a relationship must not silently move evidence to a newer revision.

### 8.4 Explore a neighborhood and cross-document relationship

**Maturity:** Alpha - proposed surface over public graph contracts and shipped
traversal primitives.

1. Select a section, entity, event, or arbitrary node.
2. Show one hop by default; require explicit action for additional hops.
3. Filter by direction, relationship type, node kind/labels, weight, and as-of
   time when supported.
4. Inspect edge ID, source, target, type, weight, properties, and supporting
   citation-backed evidence when supplied.
5. Coordinate a document-backed target with its pinned revision and source.
6. Present graph-only relationships as graph-native data, not retrieval
   evidence, and do not fabricate citations.

### 8.5 Search and inspect evidence

**Maturity:** Alpha - proposed surface over public retrieval contracts.

1. Enter query text.
2. Select explicit modes and positive top-K.
3. Optionally scope to document and node IDs and an as-of time.
4. Submit one all-or-error request.
5. Show rank, result ID, best label, result score, evidence count, evidence
   type, and request ID.
6. Inspect each evidence score, citation, quote, path, and explanation.
7. Treat scores as ranking values, not probability or confidence, unless the
   service defines otherwise.
8. Use graph mode only for citation-backed retrieval evidence; graph-only
   retrieval remains dependency-blocked under RC-04.

### 8.6 Ask and verify

**Maturity:** Alpha - optional proposed surface.

1. Run retrieval first.
2. Display generated prose only after evidence is available.
3. Link factual answer segments to evidence cards.
4. Distinguish direct quote, citation-backed graph context, generated
   synthesis, and unsupported claim.
5. Offer **Evidence only** when no model is configured.
6. Mark an answer **Unverified draft** if citation verification fails.
7. Do not run Ask over graph-only knowledge until RC-04 is resolved.

### 8.7 Compare strategies

**Maturity:** Alpha - proposed surface.

Run each strategy with identical query, corpus, effective modes, authorization
semantics when available, as-of time, scope, filters, and top-K. Compare:

- completion status;
- ordered results and overlap;
- unique evidence;
- cited documents/revisions;
- raw strategy-specific scores;
- latency when reported;
- freshness, fallback, and benchmark warnings.

Do not declare the strategy with the largest raw score "best." Configuration
is not a recall claim. When benchmark evidence is absent, show
**Unbenchmarked; no recall claim** if the target reports that state.

### 8.8 Continue from embedded to remote

**Maturity:** dependency-blocked.

1. Save semantic workspace state.
2. Select another target.
3. Compare corpus identity/lineage, contract versions, revisions, modes,
   limits, and evidence support.
4. Reopen only when the target asserts compatible identities.
5. If data must move, present an explicit transfer or promotion operation.
6. Never imply that switching a target transfers data.
7. Keep unsupported saved filters visible but disabled so intent is not lost.

### 8.9 Explore graph-only knowledge

**Maturity:** Alpha - proposed surface.

1. Open a node by ID or browse through supported graph-native property
   filters.
2. Use graph and inspector views without a document tree.
3. Inspect properties, relationships, paths, and weights as graph-native data,
   not retrieval evidence.
4. Disable retrieval and Ask with a dependency message until a public
   graph-native evidence anchor/provenance variant exists.

## 9. Surface specification

### 9.1 CLI

**Maturity:** Alpha - proposed surface.

Examples use `shoal explorer` as product syntax, not a packaging requirement:

```text
shoal explorer capabilities --target <profile>

shoal explorer corpus create --target <profile> --name <name>
shoal explorer corpus list --target <profile>
shoal explorer corpus open <corpus-id>

shoal explorer ingest start --corpus <id> --source <adapter-input>
shoal explorer ingest status <operation-id> --watch
shoal explorer ingest retry <operation-id>

shoal explorer documents list --corpus <id>
shoal explorer document show <document-id> --revision <revision-id>
shoal explorer document tree <document-id> --as-of <timestamp>

shoal explorer graph neighbors <node-id> --direction both --hops 1
shoal explorer graph path <from-id> <to-id> --max-hops 4

shoal explorer search "query" --mode lexical,tree --top-k 10 --explain
shoal explorer ask "question" --mode lexical,tree --top-k 10 --evidence-only
shoal explorer compare "query" --mode lexical --mode vector --top-k 10
shoal explorer explain <request-id>
```

Human and `--format json` output must preserve:

- target, corpus, and as-of time;
- explicit modes and top-K;
- capability decisions;
- request ID and all-or-error status;
- result, evidence, and component scores;
- exact citations/ranges and path data;
- fallback, freshness, and benchmark warnings.

The CLI MAY accept empty modes or zero top-K as public shorthand, but its
human and JSON output must show the normalized lexical mode and top-K 20.
Credentials should not be supplied through command-history-visible arguments.
Graph commands are graph-native browsing. Search or Ask in graph mode is
available only for citation-backed evidence until RC-04 is resolved.

### 9.2 API

**Maturity:** Alpha - proposed coarse surface.

The API should expose semantic operations, independent of streaming, polling,
batching, or transport:

- `GetCapabilities`
- `CreateCorpus`, `ListCorpora`, `GetCorpus`
- `StartIngestion`, `GetIngestion`, `RetryIngestion`
- `ListDocuments`, `GetDocument`, `ListRevisions`
- `GetDocumentTree`, `GetSection`, `GetSpans`, `ResolveCitation`
- `GetNode`, `GetEdge`, `ExpandNeighborhood`, `FindPaths`
- `Retrieve`
- optional Explorer-level `Ask`

Common response metadata should eventually include:

- request/operation ID;
- effective corpus and as-of time;
- effective modes, top-K, and filters;
- capability decisions;
- warnings;
- freshness/fallback decisions;
- completeness and restriction metadata only when those future contracts
  exist.

No operation exposes raw storage, iterator, or transport types.

### 9.3 Alpha visual workbench

The desktop-oriented workbench coordinates:

- document tree;
- source viewer;
- graph neighborhood and equivalent relationship table;
- result list;
- evidence/properties inspector;
- query explanation.

Selecting an object in one view highlights the same stable object in the other
applicable views.

### 9.4 Future visual surfaces

- Notebook widget for search, citations, and evidence paths
- IDE view for exact source revision/range and related symbols
- Read-only evidence packet with request, results, citations, paths, and
  limitations
- Revision comparison and moved-section mapping
- Authorization-safe sharing after authorization semantics exist

## 10. Textual wireframes

### Corpus chooser

```text
+ Shoal Explorer -----------------------------------------------------+
| Target: local-dev v   Capabilities: Limited   Context: Local       |
+--------------------------------------------------------------------+
| Corpora                                           [Create corpus]   |
|                                                                    |
| Research Notes     corpus_01     Ready              [Open]          |
| Event Knowledge    corpus_02     Graph only         [Open]          |
|                                                                    |
| Counts are descriptive only; authorization semantics unavailable.  |
+--------------------------------------------------------------------+
```

### Empty corpus

```text
+ Research Notes / Overview -----------------------------------------+
| This corpus has no knowledge artifacts.                            |
|                                                                    |
| [Add source]   [Open graph guide]                                  |
|                                                                    |
| Supported primitives: lexical, graph                              |
| Product readiness: ingestion status unavailable                    |
+--------------------------------------------------------------------+
```

### Ingestion

```text
+ Ingestion ing_2041 ------------------------------------------------+
| Source: handbook.pdf          State: Indexing                      |
|                                                                    |
| [x] Accepted                                                     |
| [x] Discovering                                                  |
| [x] Materializing     1 document, 74 sections                    |
| [>] Indexing          lexical ready; vector pending               |
| [ ] Validating                                                   |
| [ ] Ready                                                        |
|                                                                    |
| Warning: 3 spans have no page coordinate                           |
+--------------------------------------------------------------------+
```

### Coordinated document workspace

```text
+ Target: team | Corpus: Research | As of: rev_7f2 ------------------+
| Document tree | Source                      | Inspector             |
|---------------|-----------------------------|-----------------------|
| Handbook      | Page 7                      | Section sec_12        |
| +- Overview   | ... highlighted source ...  | Revision rev_7f2      |
| +- Operations | [offset 1042, 1288)         | Evidence paths: 2     |
|    +- Recovery|                             | [Copy citation]       |
|    +- Rollback|                             |                       |
+---------------+-----------------------------+-----------------------+
| Neighborhood | Relationships table                                 |
+--------------------------------------------------------------------+
```

### Search and evidence

```text
+ Explore -----------------------------------------------------------+
| [How is recovery authorized?________________________] [Search]     |
| Modes: [x] Lexical [x] Tree [ ] Vector [ ] Graph   Top K: 10      |
+------------------------+-------------------------------------------+
| Results                | Evidence                                  |
| 1 Recovery authority   | Result score: 0.8241                      |
|   rev_7f2, page 7      | Evidence score: 0.7912                    |
|   3 evidence items     | Range: [1042, 1288)                       |
|                        | Path: section -> references -> section     |
| 2 Rollback procedure   | Citation-backed relationship context      |
+------------------------+-------------------------------------------+
| Complete | No fallback | Request req_91a                           |
+--------------------------------------------------------------------+
```

### Ask

```text
+ Ask ---------------------------------------------------------------+
| Answer status: Verified against 3 evidence items                   |
|                                                                    |
| Recovery requires a single active authority.[1]                    |
| A transition preserves lineage.[2][3]                              |
|                                                                    |
| [1] Direct quote | document d1 | revision r7 | page 7             |
| [2] Citation-backed relationship | document d2 | revision r3       |
| [3] Direct quote | page unavailable                               |
|                                                                    |
| [Evidence only] [Compare strategies] [View explanation]            |
+--------------------------------------------------------------------+
```

## 11. Evidence and explainability

### Citations and ranges

A valid citation identifies document, immutable revision, section and/or span,
and a half-open source range. Explorer preserves `[start, end)` in copied and
advanced representations. Pages are one-based when known; zero means
unavailable.

### Quotes

Explorer distinguishes:

- verified exact quote hydrated from the cited revision and range;
- supplied quote whose verification is unavailable;
- quote mismatch;
- no quote supplied.

Display truncation must not appear as source text.

### Scores

The public contracts distinguish result score, evidence score, and named
explanation scores. Explorer:

- identifies each score's level and mode;
- shows raw values at stable precision;
- does not call a score probability or confidence without defined semantics;
- does not compare raw values across strategies without a declared common
  scale;
- treats an absent score as unavailable, not zero.

### Paths

A valid path contains ordered nodes with one connecting directed edge between
each consecutive pair. Explorer validates continuity, shows direction and
type, and never fills missing segments through visual proximity.

### Explanations

Current public explanation supports modes, a summary, and named score
components. Alpha renders only supplied detail.

A future bounded trace may add semantic stages:

1. scope;
2. anchor;
3. tree or graph navigation;
4. rank;
5. hydrate;
6. restrict;
7. fallback;
8. complete.

It must describe what happened, not expose storage or execution internals.

## 12. States

| Surface | Empty | Loading | Failure | Authorization | Partial |
| --- | --- | --- | --- | --- | --- |
| Target/capabilities | No target selected | Capability check in progress | Incompatible or unreachable | Connection identity only | Not applicable |
| Corpus list | No corpora returned | Skeleton rows | List failed | Semantics unavailable until RC-06 resolves | Not supported |
| Corpus overview | No artifacts | Independent readiness cards | Readiness failed | Do not claim filtered counts | Future envelope only |
| Ingestion | No operations | Stage, no fake percentage | Stage reason and retryability | Future | Explicit incomplete artifacts only |
| Documents | No document artifacts | Preserve filter and position | Service/revision failure | Future | Future envelope only |
| Hierarchy | No tree supplied | Branch busy state | Failed branch is retryable | Future | Loaded branches not declared complete |
| Source | No source evidence | Range placeholder | Hydration or range failure | Future | Quote cannot be verified |
| Graph | No graph nodes returned | Keep center node interactive | Expansion failure | Future | No frontier unless explicit |
| Search | No matches | Whole-request busy state | All-or-error | Future | Not supported currently |
| Ask | Insufficient evidence | Retrieval then generation | Failures separated | Future | Draft only under future envelope |
| Compare | No completed strategy | Per-strategy busy state | Per-strategy failure | Future | Future partial columns distinct |

Authorization restriction and execution completeness are separate dimensions.
Once authorization semantics exist, a response fully executed for the active
principal remains complete even if policy makes fewer items available.
`partial` is reserved for a future explicit execution-completeness envelope.

Additional required states:

- **Unsupported mode:** retain the requested mode, disable execution, show the
  target's reason.
- **Unknown capability:** fail closed and offer capability refresh.
- **Stale approximate index:** fail or use an explicitly permitted exact
  fallback.
- **Unbenchmarked approximate mode:** no recall claim.
- **Citation validation failure:** evidence remains inspectable but unverified.
- **Revision unavailable:** keep the requested revision identity; do not jump
  to latest.
- **Connection lost:** label cached content with its request time and do not
  present it as fresh.
- **Authentication expired:** preserve unsent work, stop requests, and avoid
  mixing cached and refreshed results.

Progress uses percentages only when a denominator is known.

## 13. Embedded-to-remote continuity

The workspace identity consists of:

- corpus ID and optional lineage;
- document/node/revision selections;
- query text;
- explicit modes and top-K;
- scope, filters, and as-of time;
- applicable request IDs.

Changing targets runs compatibility negotiation. Outcomes are:

- compatible;
- compatible with limitations;
- incompatible;
- unknown.

Reopening requires asserted identity compatibility. Matching names are
insufficient. Data transfer is a separate explicit operation that reports
lineage, validation, and whether old citations remain resolvable.

This follows the transferable local/cloud UX pattern documented publicly by
PageIndex while retaining Shoal's own contracts and multi-strategy
architecture.

## 14. Accessibility

Shoal Explorer targets WCAG 2.2 AA.

- All primary operations work without a pointer.
- Trees use standard tree or treegrid keyboard behavior.
- Every graph has an equivalent relationship list or table.
- Graph paths have textual narration.
- Panes can be resized by keyboard.
- Focus remains visible after coordinated-view updates.
- Loading and completion use restrained live regions.
- Status never relies on color alone.
- Edge types use labels or patterns in addition to color.
- Scores include numeric text and mode labels.
- Citation highlights expose document, revision, range, and page text.
- Zoom to 200 percent preserves primary workflows.
- Reduced-motion preferences disable graph rearrangement animation.
- Advanced provenance is collapsed by default.
- Stable IDs remain available in copied references.

## 15. Explicit non-goals

Shoal Explorer does not:

- select or standardize a parser, OCR system, embedding provider, or model;
- make generation part of Shoal's required runtime;
- manage prompts, agent loops, approvals, or token budgets;
- require a tree-only, vector-only, or model-driven retrieval architecture;
- expose raw cells, storage files, physical indexes, or transport messages;
- act as a database or cluster administration console;
- serve as an unconstrained graph visualization product;
- guarantee that a high score means factual correctness;
- label ranking scores as confidence without defined semantics;
- fabricate citations, quotes, pages, documents, or path segments;
- silently substitute retrieval strategies or use stale indexes;
- imply that switching targets transfers data;
- collapse immutable revisions into one mutable source;
- require document hierarchy for graph-only knowledge;
- force non-document knowledge into a fake citation;
- present graph-native data as retrieval evidence before a public graph-native
  anchor/provenance variant exists;
- treat UI filtering as authorization;
- provide authorization-safe sharing before RC-06 is resolved;
- edit original source content;
- claim a capability without ledger evidence.

## 16. Alpha acceptance criteria

Alpha is ready only when:

1. Capability negotiation gates every target-dependent action.
2. The UI and documentation use the four-state ledger.
3. Empty modes and zero top-K are never sent implicitly.
4. Corpus create/open is available only through a public coarse contract.
5. Ingestion status is not claimed before its contract exists.
6. Documents remain pinned to immutable revisions while navigating.
7. Exact half-open ranges and page-unavailable semantics are preserved.
8. Graph-native browsing is available without being labeled retrieval
   evidence; graph-only retrieval and Ask remain disabled until RC-04 is
   resolved.
9. Search is all-or-error until a partial-result envelope exists.
10. Strategy comparison holds query, scope, as-of time, filters, and top-K
    constant.
11. Fallback and freshness decisions are explicit.
12. Ask remains separable from evidence retrieval.
13. Authorization is not simulated with client-side filters.
14. Graph-only corpora remain navigable through graph-native browsing while
    retrieval and Ask remain dependency-blocked.
15. Embedded-to-remote reopening verifies identity and capabilities.
16. All primary workflows are keyboard and screen-reader accessible.

## 17. Future contract priorities

1. Capability and compatibility negotiation
2. Coarse corpus lifecycle
3. Ingestion operation and readiness status
4. Document list/tree/span hydration and graph expansion
5. Observable normalized/effective modes and limits across targets
6. Discriminated evidence anchors for document, graph, and external evidence
7. Explicit partial-result envelope
8. Authorization context and restriction metadata
9. Workspace lineage for embedded-to-remote continuity
