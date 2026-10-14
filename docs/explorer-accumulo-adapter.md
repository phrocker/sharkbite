# Shoal Explorer Accumulo Adapter Design

## Status and terminology

This document is a proposed implementation design built on the adopted
storage-neutral contract in `docs/explorer-public-contract.md`. It
distinguishes:

- **Current—implemented:** behavior directly expressed by public Shoal or
  public `accumulo` contracts.
- **Current gap:** behavior that is not defined or implemented by those public
  contracts.
- **Proposed:** required behavior of the embedded/Accumulo adapter boundary
  after the corresponding public contract is ratified.
- **Deferred:** intentionally excluded from the first Accumulo adapter.
- **Assumption:** a decision that must be verified before implementation or
  cutover.

“Explorer” below is a coarse document/graph/retrieval facade expressed in
public Shoal values. “Adapter” is its Accumulo-backed implementation. gRPC is
only an optional northbound transport. The adapter continues to use
Accumulo-native tables, mutations, scanners, iterators, visibility labels,
tablet routing, and bulk import; it does not put a Shoal storage RPC between
Explorer and Accumulo. This follows the public deployment boundary in
`docs/ai-knowledge-graph.md` and the unary facade in
`proto/knowledge.proto:KnowledgeRetrieval`.

The terms **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are normative.

## 1. Executive summary

Shoal Explorer can move from embedded storage to Accumulo without changing
the public meanings of documents, revisions, sections, spans, graph paths,
citations, evidence, scopes, ranked results, or public errors. The central
design is:

1. Preserve every caller-visible ID and ordered value exactly.
2. Store semantic history in immutable, revision- or generation-qualified
   rows, not as Accumulo cell versions.
3. Build all canonical and derived rows before publishing one immutable
   transaction fence.
4. Pin every query to a stable publication frontier and accept only committed
   rows at or below that frontier.
5. Apply Accumulo visibility before candidate generation, traversal,
   aggregation, scoring, limiting, or explanation construction.
6. Use bounded scanners, batch scanners, writers, and adapter-controlled
   iterators; explicitly sort all public results instead of relying on
   BatchScanner order.
7. Compare both adapters through one public-value conformance suite before
   migration.

Revision data is immutable because citations name an exact revision and
source range. Rewriting a committed revision would make an old citation,
quote, checksum, or graph explanation silently mean something new. Updates
therefore create a new revision or graph generation; logical deletion creates
an immutable tombstone generation.

Accumulo provides atomicity for one row mutation, not a transaction across
the rows and tables needed by a revision. The adapter closes that gap with a
bounded transaction root, row-atomic owner-bearing epoch reservation,
immutable terminal outcome, and self-contained frontier checkpoint. Every
staged row carries TXN, content epoch, stable LPART, COPYGEN,
VDIGEST, logical digest, and any physical index generation. All initial
LPART commit-copy cells are written in one TXN row mutation only after bounded
manifest chunks are verified. A reader pinned to frontier `H` and a policy
generation accepts a row only when the selected LPART/VDIGEST commit copy and
COMMITTED epoch outcome agree and `E <= H`. Before the
fence/outcome/checkpoint, none of the bundle is visible; afterward, every
logical byte is immutable. Concurrent later writes have epochs greater than
`H` and cannot enter the request. This prevents mixed cross-table reads
without pretending that Accumulo offers cross-row transactions.

### Design principles

- **Public values are authoritative.** Physical hashes, salts, row keys, and
  manifests never replace `shoal.ID` values.
- **No silent semantic widening.** Missing modes, incomplete indexes,
  exhausted graph budgets, or unsupported history return a public error; the
  adapter does not silently reduce depth, omit a requested mode, substitute a
  newer revision, or return partial results.
- **Storage timestamps are not revisions.** Publication epochs and immutable
  row identities carry semantic history. Accumulo timestamps are only
  deterministic storage metadata.
- **One selected copy per LPART and policy generation.** A document revision
  and its sections, spans, tree entries, and lexical postings share one LPART
  and active VDIGEST. Relabel may leave old/new physical copies, but policy
  mapping selects exactly one.
- **Derived data is no broader than its inputs.** Reverse edges, indexes,
  evidence, statistics, and caches inherit the same or a stricter policy.
- **Transport independence.** Direct and gRPC clients observe the same
  values, ordering, errors, and authorization decisions.
- **Bounded execution.** Every scan, expansion, merge, hydration, cursor, and
  response has configured resource limits.

## 2. Current public contract baseline

### 2.1 Capability and gap inventory

| Area | Verified current behavior | Current gap or consequence |
|---|---|---|
| Shared values | `pkg/shoal` defines opaque caller-visible IDs, raw unsigned-byte ID comparison, bounded opaque metadata, finite scores, and stable error literals. | IDs intentionally have no universal generation or Unicode rule. Metadata is not an authorization channel. |
| Documents | `pkg/document` defines UTF-8 byte offsets, half-open/empty ranges, source-boundary validation, bounded value validators, complete revision-content validation, and immutable revision ownership. | There is no storage-neutral document repository, latest/publication API, canonical source field on `Revision`, or persisted history/tombstone API. |
| Citations | `document.Citation.Validate` remains structural; `ResolveCitationQuote` and `ValidateCitationQuote` check exact retained source, ownership, containment, and quote bytes when context is supplied. | These helpers do not provide storage hydration or prove that retained source is available in an adapter. |
| Graph | `pkg/graph` validates bounded nodes/edges, finite weights, and directed connected paths while allowing cycles, self-edges, parallel edges, and optional node kind. | There is no storage-neutral graph persistence, generation, deletion, association, or path-producing traversal API. |
| Retrieval | `pkg/retrieval` defines normalization (`TopK=20`, lexical default, stable deduplication, UTC `AsOf`), scope/seed rules, hard request bounds, evidence validation, deterministic result/evidence order, uniqueness, complete-response validation, and versioned shared Unicode analysis plus lexical/tree/graph/vector fusion scoring. | Pagination, publication-frontier resolution, persisted associations, alternative scoring, and distributed vector indexing remain store-dependent or deferred. |
| gRPC | `pkg/retrieval/grpc` normalizes before delegation, preserves repeated-field order, retains wire-specific UTF-8 checks, maps public errors, and validates remote response bounds, uniqueness, finite values, and deterministic order. | gRPC contains no storage/query engine and currently establishes no authenticated principal or authorization policy. It has no pagination. |
| Errors | `pkg/shoal/errors.go` defines and tests the stable literals `invalid_argument`, `not_found`, `conflict`, `unauthorized`, `unavailable`, `canceled`, `deadline_exceeded`, and `internal`; `docs/explorer-public-contract.md` assigns condition categories. | Authorization non-disclosure cannot be claimed until an authorization boundary exists. |
| Code ingestion | `pkg/code/id.go`, `source.go`, and `ingest.go` define deterministic length-delimited SHA-256 IDs, immutable source identity, an exact idempotency key, and `applied`/`unchanged` retry behavior. `pkg/explorer/codematerializer` implements the deterministic `explorer-code-v1` public projection and additive associations. | Generic document/graph IDs remain opaque and must not be regenerated by storage. Atomic document-plus-graph publication and persisted associations are not defined. |
| Embedded Explorer | `pkg/explorer.Client` currently exposes ingest, document listing/read, graph connect/neighborhood, and retrieval operations; the embedded implementation normalizes retrieval/neighborhood requests, uses shared ranking, optionally stores span embeddings with provider/model/dimension provenance, and rejects `AsOf`. `pkg/explorer/authorized` adds trusted context resolution, policy projection before ranking, non-disclosing reads, graph reachability filtering, generation checks, and canonical backend-response verification. The reusable conformance harness covers both public values and authorization/noninterference. Its versioned lossless row envelope remains an embedded compatibility format. | The unwrapped client is intentionally unauthenticated. The M2 memory policy catalog is restart-reusable in process but not durable process recovery or proof of Accumulo enforcement. |
| Canonical logical codec | `pkg/explorer/canonical` provides bounded deterministic binary `RecordV1` serialization, source and record SHA-256 digests, opaque-byte preservation, publication-coordinate validation, and golden/corruption fixtures. | It is a logical comparison/import format, not an embedded row encoding or an Accumulo schema. |
| Authorization core | `pkg/explorer/auth` provides immutable decisions, capability-bound context resolution, canonical policy/visibility encoding, service-role ceilings, scanner-authorization derivation, generation guards, partitioned cache keys, and redacted audit values. | Durable authority/policy-copy catalogs, authenticated gRPC interceptor wiring, and Accumulo service-account deployment remain later work. |
| Embedded storage | `proto/embed.proto:ConditionalWrite` defines row-local conditional mutation semantics; `ScanRequest.as_of` defines lower-level timestamp filtering. | These are storage primitives, not a public Explorer snapshot, document, graph, citation, or authorization contract. |
| Accumulo reads | `accumulo/scanner.go` exposes scanner authorizations, columns, and iterator settings. `batch_scanner.go` documents input-range/tablet order unless multi-scan is used. `scan_stream.go` bounds memory to a scanner batch. | Accumulo scans are not a multi-row/multi-table snapshot. Batch and stream results may be accompanied by errors. |
| Transactional writes | `accumulo/mutation.go` provides one-row puts/deletes. `batch_writer.go` provides bounded buffering, durability, safe retries, and explicit ambiguous-partial-commit failure. `conditional_writer.go` exposes exact-row absence/value conditions, optional exact timestamps, and Accepted/Rejected/Unknown results over the native Accumulo conditional-update protocol. `pkg/explorer/coordination` provides bounded deterministic M3 record/key vocabulary, including transaction leases and physical `PartitionCommitCopyV1` fences. The allocator, guard, catalog, and control packages provide their documented row-CAS lifecycles. `pkg/explorer/coordination/transaction` adds absent-or-identical claims, immutable manifest chunks, sorted guard acquisition, owner-fenced allocation/takeover, injected physical write/verification, row-atomic root-plus-LPART publication, terminal outcome/checkpoint/finalization, deterministic stored results, corruption quarantine, and trusted physical mapping. `pkg/explorer/coordination/recovery` adds bounded band scanning, authoritative recheck, lease takeover, concurrency caps, cancellation, and bounded backoff. `internal/explorercoord` supplies the production embedded-engine Store, deterministic physical writer/verifier, durable canonical intent materializer, process-exclusive runtime, committed-record gate, and restart recovery composition. `cmd/shoal-explore-web` opens that composition for its embedded backend. | BatchWriter is not atomic across mutations. The live Accumulo physical sink, full document/graph row materialization, deployment schema/bootstrap, and live-cluster fault suite remain deferred. The embedded command transactionally publishes the existing immutable Explorer document-record family. A configured record exceeding the 1 MiB manifest-cell bound is rejected before any write; the unconfigured legacy `explorer.Open` path retains its historical compatibility behavior. Other Explorer record families retain their established embedded persistence path. Policy relabel and index-rebuild packages have independent recovery primitives; a single end-to-end live fault campaign spanning those operations is not yet present. |
| Accumulo ordering/history | `accumulo/key.go:Key.Compare` defines row/CF/CQ/visibility ascending, timestamp descending, tombstone-first ordering. | Scanner order is not public Explorer order, and versioning/compaction can remove cell history. |
| Accumulo security | `accumulo/authorizations.go`, `column_visibility.go`, `scanner.go`, and `security.go` expose byte-exact authorizations, boolean visibility expressions, per-scan authorizations, and user/table/namespace permissions. | They do not derive labels from a Shoal principal, prevent blank visibility, or define Explorer non-disclosure. |
| Table administration | `accumulo/table_admin.go`, `table_properties.go`, `table_add_splits.go`, and `table_bulk_import.go` expose table creation, properties, binary splits, and manager-authoritative bulk import. | Explorer-specific locality groups, iterator stacks, split plans, retention, and compaction policy do not exist today. |

### 2.2 Baseline conclusions

1. `pkg/explorer.Client` already exposes ingest, document, graph-connect,
   neighborhood, and retrieval operations. It remains a coarse embedded/product
   facade rather than a high-level storage-neutral repository; exact retained
   citation hydration, history, publication, deletion, association, and generic
   mutation operations remain additive APIs or test-harness operations.
2. `ModeVector` is an enum value, not proof that a compatible distributed
   vector implementation exists. The first adapter must not claim vector
   support.
3. `ErrorUnauthorized` is error vocabulary, not proof of current
   authentication or authorization.
4. `AsOf` is accepted by the retrieval value contract, but high-level
   revision selection and distributed snapshot behavior are not defined.
5. Current gRPC response validation protects structure and finite values; it
   does not verify stored-object relationships, quote integrity, visibility,
   ranking, or completeness.
6. The Accumulo adapter cannot safely implement claims, publication, leases,
   or frontier advancement with BatchWriter alone. A native Accumulo
   conditional-write API is a hard dependency.

### 2.3 Explicit assumptions

| Assumption | Validation required before implementation |
|---|---|
| One document-revision LPART selects one visibility expression per policy generation. | Confirm product requirements or split mixed-policy content into separate documents/LPARTs; old/new physical copies may coexist only under explicit policy mappings. |
| Exact UTF-8 source bytes remain available for every retained citation. | Prove through the embedded baseline, backfill source, and retention policy. |
| Both adapters can call one analyzer/scorer/fusion implementation. | Extract or define the shared implementation and make its version part of manifests. |
| An authorization domain is the largest unit requiring one publication frontier. | Confirm cross-domain retrieval is forbidden; otherwise define a multi-domain snapshot token. |
| A supported native Accumulo conditional writer can be added without routing storage through gRPC. | Implement and fault-test it before M3. |
| The target Accumulo deployment permits the required table properties, iterator classes, splits, and service-account authorization ceiling. | Verify during bootstrap against every supported deployment version. |

### 2.4 Normative terms

| Term | Definition |
|---|---|
| Logical identity | The public or adapter-defined identity whose state is selected: document ID, node ID, edge ID, or canonical association ID. |
| Generation | One immutable live or tombstone state for a logical identity, published at one content epoch. A document generation is its revision. |
| Winner at `H` | The visible, committed generation with the greatest content epoch `<= H`, selected **including tombstones**. The winner's state is interpreted only after selection. |
| Logical policy identity | Security-authority-owned stable policy slot. Its grants/expression are versioned by policy generation, but its identity is preserved across broadening, narrowing, and rollback. |
| Logical publication partition (`LPART`) | Stable logical access partition independent of TXN and physical visibility: `SHA256("explorer-lpart-v1" ‖ domain ‖ logicalPolicyID ‖ ownerKind ‖ ownerID ‖ logicalRole)`. Revision-owned LPARTs use `(document,revision)` as owner; shared graph objects use their graph identity. Relabeling and byte-identical reuse preserve LPART. |
| Visibility digest (`VDIGEST`) | SHA-256 of the canonical flattened Accumulo visibility expression. It identifies one physical policy copy and changes when that expression changes. |
| Physical policy copy | Cells carrying one `(LPART, COPYGEN, VDIGEST)` plus the matching commit-copy cell. `COPYGEN` is the policy generation that created those physical cells. Multiple copies may coexist. |
| LPART policy mapping | An activation at `MAPGEN` selecting one `(COPYGEN,VDIGEST)` for an LPART. A request selects the greatest active `MAPGEN <= requestPolicyGeneration`; rollback may map a new MAPGEN to an older sealed COPYGEN. |
| Content epoch | A monotonic per-domain publication ordinal reserved for one TXN. It is not `Revision.CreatedAt` or an Accumulo clock value. |
| Epoch reservation | An owner-, authority-generation-, lease-, and fencing-token-bearing slot stored on the domain allocator row. It becomes `COMMITTED`, `ABORTED`, `CONFLICTED`, or `POISONED`. |
| Epoch outcome | The immutable per-epoch copy of a terminal reservation. Frontier advancement and readers use it after active reservation cells are retired. |
| Stable frontier `H` | The greatest epoch such that every epoch at or below it is terminal. Committed slots are visible; terminal holes are skipped. |
| Frontier checkpoint | One conditional mutation on the domain allocator row that writes both current `{H,visible_at}` and an immutable history cell keyed by `{visible_at,H}`. The same row is authoritative for latest and historical lookup. |
| Physical index generation | An immutable layout/build ID for a rebuildable index. It is separate from content epochs and is pinned per index family for the request. |
| Entity guard | The authoritative control row that serializes one logical document/node/edge/association or index-family activation and records committed head, pending TXN, owner, fence, and retirement generation. |
| Expected base | The winner state a mutation requires on its entity guard: `ABSENT`, an exact revision/generation, code-document `APPEND`, or code-graph `ABSENT_OR_IDENTICAL(logicalDigest,LPART)`. |
| History floor | The smallest content epoch that readers may request and stale writers may reference. It is authoritative on the allocator row. |
| Retention generation | A monotonic allocator-row generation incremented with history-floor/retirement policy changes and checked by writers/readers. |
| Writer authority generation | A durable monotonically increasing generation naming the only writer set allowed to reserve new epochs for a domain. |
| Snapshot lease | An authoritative, expiring reader/cursor record pinning `H`, retention generation, and IGENs so GC cannot remove required data. |
| Canonical association | A versioned, visibility-protected link between a document/revision/section/span and a graph node/edge. Scope joins and graph evidence MUST use these rows, never metadata or graph labels. |

## 3. Semantic invariants

These invariants are normative for both embedded and Accumulo adapters.

### 3.1 Identity, mutation control, and serialization

1. Public document, revision, section, span, node, edge, result, and artifact
   IDs MUST round-trip byte-for-byte. Storage hashes and salts MUST NOT replace
   them.
2. Generic document and graph mutations MUST carry a nonempty idempotency
   token. The token is scoped by domain and operation:

   `TXN = SHA256("explorer-txn-v1" ‖ E(domain) ‖ E(operation) ‖ E(token))`.

   The TXN claim stores the canonical request digest. Reusing a token with a
   different digest is `conflict`.
3. `pkg/code.IngestRequest.IdempotencyKey()` is the token for code ingestion.
   Repeating a committed request MUST return `IngestUnchanged` with the exact
   stored artifact identities.
4. Create MUST use expected base `ABSENT`. Update and logical delete MUST name
   the exact committed base revision/generation. Graph bundles MUST name the
   expected base for every mutated node/edge. Entity guards are acquired in
   `(kind, unsigned ID bytes)` order; a mismatch is `conflict`.
   The existing public `pkg/code.Ingest` operation is the sole append-only
   exception: a new deterministic revision uses precondition `APPEND`, which
   serializes on the document guard but does not require it to be the current
   source-control revision. Code-owned graph identities use the row-atomic
   `ABSENT_OR_IDENTICAL` rule in Section 6.9.
5. A lost response MUST be resolved through the token/TXN claim and commit
   fence. The adapter MUST NOT allocate a replacement epoch or duplicate
   artifacts.
6. Ordered slices remain ordered. Metadata remains opaque; canonical
   encoding sorts map keys by unsigned UTF-8 bytes. Scores/weights MUST be
   finite. Deterministic protobuf tests MUST use deterministic marshaling.

### 3.2 Document winners, source bytes, sections, and spans

1. `(domain, document ID, revision ID)` identifies one immutable revision.
   Revision claims MUST use all three components.
2. Latest selection MUST enumerate visible committed document generations
   with epoch `<= H`, choose the greatest epoch **without filtering
   tombstones**, and then interpret the winner:
   - live winner: return that revision;
   - tombstone winner: the document is absent for latest/retrieval reads.
3. Explicit retained revision and citation reads MAY resolve an older live
   revision even when the latest document winner is a tombstone. Ranked
   retrieval MUST use only the latest winner at `H`.
4. `Revision.CreatedAt` and `SourceVersion` are source metadata, not
   publication order.
5. Every revision MUST store the complete immutable canonical UTF-8 source
   byte sequence, its byte length, SHA-256 digest, and fixed-size chunks.
   `SourcePosition.Offset` is normatively a zero-based UTF-8 **byte offset**;
   ranges remain half-open `[Start, End)`. Offsets MUST land on UTF-8
   boundaries when a string is materialized. Existing one-based page and
   page-zero-unavailable semantics are unchanged.
   Proposed generic `RevisionBundle` writes MUST supply these source bytes;
   they are never reconstructed by concatenating spans. A tombstone carries
   the prior source digest but no new source payload.
6. Section-only citations MUST reconstruct quotes from revision source
   chunks. Span text MUST equal the exact source-byte slice for its range;
   indexes or submitted quote strings are never authoritative.
7. The root has empty `ParentID`; all other sections have one same-revision
   parent. Trees MUST be connected, acyclic, range-contained, and free of
   duplicate sibling orders. Children/spans order by `Order`; invalid
   duplicates fail rather than being repaired.
8. Update publishes a new revision. Logical delete publishes a tombstone
   generation. Physical GC never acts as logical deletion.

### 3.3 Graph winners, endpoint replacement, and traversal order

1. Node and edge generation selection MUST include tombstones, select the
   greatest committed epoch `<= H`, and only then interpret state.
2. Adjacency rows are candidate indexes, not edge authority. Before any
   degree, node, edge, frontier, or global limit is applied, the coordinator
   MUST:
   1. resolve the seed/source-node winner for this adjacency candidate;
   2. require that seed/source winner to be live, visible, committed, above
      the history floor, not retired, and consistent with the canonical edge
      endpoint for the requested direction;
   3. resolve the canonical edge winner;
   4. discard it if that winner is a tombstone;
   5. require the adjacency tuple, TXN, epoch, endpoints, type, and physical
      index generation to match that winner;
   6. resolve the other endpoint-node winner;
   7. require that other endpoint winner to be live, visible, committed,
      above the history floor, not retired, and consistent with the edge.
3. Replacing an edge's endpoint or type is one expected-base mutation. It
   writes the new canonical edge generation, new forward/reverse entries, and
   old-adjacency retirement entries under the same TXN. Canonical-winner
   validation makes any stale old adjacency ineligible even if cleanup is
   delayed. Concurrent replacements from the same base cannot both acquire
   the edge guard; one returns `conflict`.
4. Hidden, retired, absent, or tombstone endpoint/edge winners never consume
   degree/global limits. Hidden intermediate nodes are never skipped.
5. Seeds are deduplicated and sorted by unsigned node ID. Direction rank is
   `out=0`, `in=1`; path-representation rank is `directed=0`,
   `unrepresentable=1`. After canonical/liveness validation:
   - per-source degree order is `(directionRank, targetID, edgeType, edgeID)`;
   - global edge admission order is
     `(depth, rootSeedID, pathRepresentationRank, directionRank, sourceID, targetID, edgeType, edgeID, pathTuple)`;
   - global node admission order is
     `(depth, rootSeedID, pathRepresentationRank, pathTuple, targetID)`;
   - equal-depth directed-path winner order is
     `(rootSeedID, nodeID sequence, edgeID sequence)`;
   - returned path order is `(depth, rootSeedID, pathTuple)`.
   For a neighborhood-only incoming/mixed discovery with no representable
   directed path, `pathTuple` in admission ordering is the fixed empty
   sentinel. The representation rank ensures an equal-depth directed path is
   retained over an unrepresentable discovery; the remaining fields provide
   a total order.
6. Limits are applied only after the complete current BFS level has been
   merged and sorted by these tuples.
7. A node is expanded only at its minimum admitted depth. If the same node or
   edge is reached from multiple seeds/paths at that depth, retain one
   occurrence using the relevant global/path tuple above; parallel edges with
   different edge IDs remain distinct.
8. **Directed-path rule (Option A).** A `graph.Path` MAY be extended only by
   an edge whose `From` is the current path tail and whose `To` is the next
   node. Every returned/evidence path MUST pass `graph.Path.Validate`.
   Incoming-only or mixed-direction neighborhood expansion may return bounded
   nodes and edges in the existing neighborhood result shape, but MUST NOT
   claim a rooted `graph.Path` for those reverse steps. Once a route includes
   an incoming step, that route and all of its descendants remain
   path-unrepresentable even if later steps are outgoing. If paths are optional,
   omit the unrepresentable path. If an operation requires paths while
   requesting incoming or mixed-direction expansion, return deterministic
   `unavailable` before scanning. No replacement public walk type is proposed.

### 3.4 Associations, citations, and evidence

1. Canonical associations MUST support both directions for:
   document/revision, section, or span ↔ graph node or edge.
2. Association IDs are deterministic:

   `SHA256("explorer-association-v1" ‖ domain ‖ document ‖ revision ‖ sourceKind ‖ sourceID ‖ graphKind ‖ graphID ‖ relation)`.

3. An association's visibility is the conjunction of both endpoints. Both
   directional rows MUST carry the same ID, TXN, content epoch, digest, and
   association index generation.
4. Association selection follows the same winner rule: select the greatest
   committed generation including tombstones, then require a live winner.
5. A request with both document and node scopes accepts a candidate only when
   a live canonical association connects the document winner/target revision
   and graph winner at `H`. Missing associations MUST NOT be inferred from
   metadata, labels, matching strings, or IDs.
6. Every citation names a real committed revision and section/span. If both
   section and span are present, the span belongs to the section. The cited
   range is contained by the referenced object.
7. `Evidence.Quote` MUST be sliced from canonical revision source bytes.
   Evidence never retargets to the latest revision.
8. Evidence/path authorization uses one request decision and snapshot.
   Evidence order is score descending, then
   `(document, revision, section, span, start offset, end offset)`, then the
   graph path tuple.

### 3.5 Retrieval, winner filtering, modes, ranking, and errors

1. A request pins `H`, authorization fingerprint/generation, history-floor
   generation, writer-authority generation, and one physical generation for
   every rebuildable index family.
2. Every lexical/tree/association posting or candidate MUST be joined to the
   document winner at `H`. A posting whose revision is not the live winner is
   discarded before scoring, counting, fusion, or top-K. Superseded and
   deleted revisions therefore cannot surface.
3. Visibility, commit, winner, target-liveness, association, and physical
   generation checks precede all scoring and limits.
   Graph checks include both canonical endpoint-node winners.
   Policy-generation mapping selects one `(COPYGEN,VDIGEST)` per LPART and
   logical deduplication occurs before winner selection, counts, scoring, or
   limits.
4. Ranking statistics and every ranking input MUST be computed strictly from
   the caller-authorized projection, or be fixed authorization-independent
   constants. Hidden data MUST NOT affect visible scores or order.
5. Results order by score descending and result ID bytes ascending. Evidence
   uses Section 3.4 order. Duplicate results merge through one shared
   deterministic function.
6. Scope IDs are ORed within each list. Nonempty document and node lists are
   conjunctive through canonical associations. Empty scope is unconstrained
   only for modes with a bounded seed source.
7. Shared normalization is:
   - `TopK == 0` becomes 20;
   - empty modes become lexical;
   - duplicate modes and scope IDs collapse, preserving first occurrence;
   - normalized `TopK` and scope sizes remain subject to hard bounds.
8. Initial standalone tree/graph behavior is deliberately bounded:
   - standalone tree requires nonempty document scope, or bounded document
     seeds supplied by lexical mode earlier in the fixed planner;
   - standalone graph requires nonempty node scope, bounded nodes reached
     through document-scope associations, or bounded seeds supplied by
     lexical/tree mode earlier in the fixed planner;
   - if no such seed source exists, the adapter returns deterministic
     `unavailable` before scanning. It MUST NOT perform a corpus-wide
     tree/graph seed scan.
   Path-producing graph retrieval uses outgoing directed expansion only.
   Incoming/mixed neighborhood discoveries may contribute bounded
   neighborhood candidates but never an invalid evidence path; an evidence
   item requiring an unrepresentable path is omitted and refill continues.
9. Existing `Retrieve` is all-or-error and has no cursor.
10. Error outcomes are normative:

| Condition | Public outcome | Retryability |
|---|---|---|
| Invalid value, static size/cardinality bound, malformed UTF-8/range/tree/path | `invalid_argument` | No; change request |
| Hidden or absent individual object; expired cursor; requested epoch below history floor | `not_found` | No, except with a new cursor/time |
| Idempotency token digest mismatch, expected-base mismatch, stale writer authority/fence, retired identity resurrection | `conflict` | No; refresh base/authority or use a new token |
| Authentication or whole-operation/domain denial | `unauthorized` | Only after credentials/policy change |
| Transient storage failure, admission/runtime budget exhaustion, read-side authorization/retention-generation change during execution, required index generation unavailable, unsupported implemented-mode shape | `unavailable` | Yes when the cause/configuration changes |
| Context canceled | `canceled` | Caller decision |
| Deadline elapsed | `deadline_exceeded` | Yes with a new deadline |
| Detectable corruption after a committed fence | `internal` | No automatic retry; quarantine/repair |

Object-level authorization remains indistinguishable from absence.

### 3.6 Stable frontier and `AsOf`

1. An epoch is visible only when its TXN fence exists, its immutable outcome
   is `COMMITTED` and matches the owner-bearing reservation that created it,
   and `epoch <= H`. The active reservation cell may later be retired.
2. Terminal `ABORTED`, `CONFLICTED`, and `POISONED` reservations are holes
   that permit frontier advancement but publish no data.
3. A frontier checkpoint is one conditional mutation on the allocator row. It
   requires the previous frontier/checkpoint digest and atomically updates
   `q:{frontier,lastVisibleAt,checkpointDigest}` plus appends
   `f:{INV_TIME(visibleAt),frontier} -> FrontierCheckpointV1`, including the
   digest of the contiguous terminal outcomes it covers. There is no separate
   timeline row or cross-row pointer.
4. No `AsOf` reads `q:frontier`. Explicit `AsOf=A` scans the same row's
   `f` cells and selects the newest `visibleAt <= A`. `visibleAt` is strictly
   increasing at nanosecond precision.
5. Within a document/node/edge/association, winner selection then follows
   Sections 3.2–3.4. Authorization is current, not historical.
6. Readers MUST read the authoritative history floor after pinning and
   recheck its generation before serialization. If `H` is below the floor,
   return `not_found`; never substitute a newer snapshot.

## 4. Accumulo physical design

### 4.1 Common byte encoding

All row keys are binary. The notation below is normative:

| Symbol | Encoding |
|---|---|
| `a ‖ b` | Byte concatenation. `‖` is notation only and is not stored. |
| `V1` | Physical schema byte `0x01`. |
| `E(x)` | Order-preserving, prefix-free bytes: encode input `0x00` as `0x00 0xff`, then terminate with `0x00 0x00`. No Unicode normalization. |
| `B8(tag, parts...)` | First byte of SHA-256 over length-delimited `tag` and parts; physical partition only. |
| `U32(x)`, `U64(x)` | Unsigned big-endian integer. |
| `INV64(e)` | `U64(^e)`, so larger publication epochs sort first. |
| `TIME(t)` | Fixed UTC tuple `U16(year) ‖ U8(month) ‖ U8(day) ‖ U8(hour) ‖ U8(minute) ‖ U8(second) ‖ U32(nanosecond)`, preserving full public timestamp precision and chronological order for years 1–9999. |
| `INV_TIME(t)` | Bytewise complement of `TIME(t)`, so newer checkpoints sort first and a seek at `INV_TIME(A)` finds the newest checkpoint at or before `A`. |
| `K` | One byte: `0x01` section, `0x02` span. |
| `SK` / `GK` | Association endpoint kind: source `0x01` document/revision, `0x02` section, `0x03` span; graph `0x01` node, `0x02` edge. |
| `D` | Adapter-internal authorization-domain ID obtained only from trusted request context. It is not added to public Shoal values. |
| `EPOCH` | Monotonic per-domain publication epoch, limited to positive `int64`. |
| `TXN` | Deterministic transaction/idempotency ID. |
| `LPART` | Stable logical publication-partition ID from Section 2.4. It is independent of policy generation and visibility bytes. |
| `COPYGEN` / `MAPGEN` | Physical-copy creation generation / LPART mapping activation generation. Usually equal; rollback may use a newer MAPGEN selecting an older COPYGEN. |
| `VDIGEST` | SHA-256 digest of the canonical flattened visibility expression for one physical copy. |
| `IGEN` | Immutable physical index-generation ID. |

Values use versioned deterministic envelopes. Variable fields are
length-prefixed, integers are big-endian, ordered slices retain order,
metadata keys are sorted, and every value repeats enough identity to detect a
malformed key/value pairing.

In schema envelopes, `digest`/`canonicalDigest` means the logical content
digest, which includes LPART but excludes MAPGEN, COPYGEN, VDIGEST, and
physical visibility. `physicalCopyDigest` is non-self-referential:
it hashes row/CF/CQ/timestamp/visibility and encoded value bytes with any copy
digest field omitted. It is stored in transaction/policy-copy manifests and
commit-copy cells, not required inside every data value.

Canonical data/index row keys intentionally omit LPART, MAPGEN, COPYGEN, and
VDIGEST so logical range/order semantics do not change during relabel.
Physical copies occupy the same row/CF/CQ/timestamp with distinct Accumulo
column visibilities; their values carry LPART/COPYGEN/VDIGEST.
Commit-copy and policy-catalog keys include those fields explicitly.

All final data cells use an explicit timestamp equal to their publication
epoch. Transaction state cells use an explicit monotonically increasing
state generation. The adapter never relies on `MutationLatestTimestamp`.
Epoch/state values are also present in keys or values; Accumulo timestamps
are diagnostic and duplicate-detection metadata, not the semantic history
mechanism.

Tables use `maxVersions=1`. Semantic revisions and generations have distinct
rows, so ordinary version pruning cannot remove `AsOf` history. Logical
deletion uses tombstone envelopes, not Accumulo delete mutations. Physical
deletes are reserved for garbage collection and use the exact original
visibility.

### 4.2 Proposed table schema

The locality groups and iterator/property configuration are bootstrap
requirements applied through public table-property administration such as
`accumulo.Connector.SetTableProperty`. Exact property names and supported
iterator classes must be verified against the deployment before table
creation.

| Table and purpose | Row-key byte layout | CF / CQ / value | Accumulo timestamp | Visibility placement | Locality groups | Partitioning and hot-row strategy | Retention/version policy |
|---|---|---|---|---|---|---|---|
| `explorer_documents` — canonical document generations, including tombstones | `V1 ‖ 'D' ‖ B8('D',D,doc) ‖ E(D) ‖ E(doc) ‖ INV64(EPOCH) ‖ E(rev) ‖ E(TXN)` | `o:document -> DocumentEnvelopeV3{state,Document,revisionDigest,txn,epoch,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Physical visibility whose digest equals `VDIGEST` | `object={o}` | One row per generation across 256 bands. Winner selection scans the logical-identity prefix and includes tombstones. | Retain live/tombstone generations to the authoritative history floor; `maxVersions=1`. |
| `explorer_revisions` — immutable revision object and `(domain,document,revision)` claim | `V1 ‖ 'R' ‖ B8('R',D,doc,rev) ‖ E(D) ‖ E(doc) ‖ E(rev)` | `c:claim -> RevisionClaimV2{state,digest,txn,owner,fence,retentionGeneration}`; `o:revision -> RevisionEnvelopeV3{Revision,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}` | Claim generation for `c`; `EPOCH` for `o` | Claim uses fixed control visibility; object uses the active physical visibility copy | `claim={c}`, `object={o}` | Exact claim contention is limited to one document/revision. Partial claims transition to `ABORTED`/`POISONED`; staged rows are cleaned but the digest claim remains. | Claims outlive idempotency/history retention and prevent ID reuse; object retained with revision. |
| `explorer_revision_source` — authoritative full source bytes for citations | Header: `V1 ‖ 'H' ‖ B8('B',D,doc,rev) ‖ E(D) ‖ E(doc) ‖ E(rev)`; chunk: `V1 ‖ 'C' ‖ B8('B',D,doc,rev) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ U32(chunk)` | `h:source -> SourceHeaderV2{utf8=true,offsetUnit=UTF8_BYTE,byteLength,sha256,chunkSize,chunkCount,txn,epoch,lpart,copyPolicyGeneration,vdigest}`; `b:data -> SourceChunkV2{bytes,sha256,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active physical visibility copy on header and every chunk | `header={h}`, `bytes={b}` | Payload is at most 1,000,000 bytes so the encoded cell remains below 1 MiB; one row per chunk permits splitting and bounded citation reads. | Retained with revision/citation history; header checked before chunk hydration. |
| `explorer_sections` — canonical sections and non-rebuildable ordered membership | Exact: `V1 ‖ 'I' ‖ B8('S',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ E(section)`; child: `V1 ‖ 'C' ‖ B8('S',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ E(parent) ‖ U32(order) ‖ E(section)` | `o:section -> SectionEnvelopeV3{Section,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}`; `x:child -> SectionRefV3{...,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active physical visibility copy | `object={o}`, `membership={x}` | One row per object/ref. | Retained with revision; exact and child rows retire together. |
| `explorer_spans` — canonical span values and ordered section membership | Exact: `V1 ‖ 'I' ‖ B8('P',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ E(span)`; ordered: `V1 ‖ 'O' ‖ B8('P',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ E(section) ‖ U32(order) ‖ E(span)` | `o:span -> SpanEnvelopeV3{Span,sourceSliceDigest,txn,epoch,lpart,copyPolicyGeneration,vdigest}`; `x:span -> SpanRefV3{...,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active physical visibility copy | `object={o}`, `membership={x}` | Span text remains bounded; source table is quote authority. | Retained with revision. |
| `explorer_graph_nodes` — canonical node generations/tombstones | `V1 ‖ 'N' ‖ B8('N',D,node) ‖ E(D) ‖ E(node) ‖ INV64(EPOCH) ‖ E(TXN)` | `o:node -> NodeEnvelopeV3{state,Node,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active physical visibility copy | `object={o}` | One row per generation, 256 bands. | Retain to graph history floor. |
| `explorer_graph_edges` — canonical edge generations/tombstones | `V1 ‖ 'E' ‖ B8('E',D,edge) ‖ E(D) ‖ E(edge) ‖ INV64(EPOCH) ‖ E(TXN)` | `o:edge -> EdgeEnvelopeV3{state,Edge,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active copy of `Vedge AND Vfrom AND Vto` | `object={o}` | One row per generation, 256 bands. | Retain to graph history floor. |
| `explorer_graph_out` — rebuildable outgoing adjacency | `V1 ‖ 'O' ‖ E(IGEN) ‖ bucket ‖ E(D) ‖ E(from) ‖ E(to) ‖ E(type) ‖ E(edge) ‖ INV64(EPOCH) ‖ E(TXN)` | `a:edge -> EdgeRefV3{state,edge,from,to,type,txn,epoch,canonicalDigest,lpart,copyPolicyGeneration,vdigest,igen}` | `EPOCH` | Same active physical copy as canonical edge | `adjacency={a}` | One row per generation; versioned bucket count in IGEN manifest. | Retain while IGEN is selectable; stale rows never win without canonical match. |
| `explorer_graph_in` — rebuildable reverse adjacency | Same as outgoing with tag `'I'` and `E(to) ‖ E(from)` | Identical logical `EdgeRefV3` | `EPOCH` | Same as outgoing | `adjacency={a}` | Same IGEN/bucket plan. | Same as outgoing; parity checked for manifest-declared rows. |
| `explorer_lexical` — rebuildable lexical postings | `V1 ‖ 'L' ‖ E(IGEN) ‖ bucket ‖ E(D) ‖ E(analyzerVersion) ‖ E(term) ‖ E(doc) ‖ INV64(EPOCH) ‖ E(rev) ‖ K ‖ E(target) ‖ E(TXN)` | `p:posting -> PostingV3{field,tf,positions,sourceOffsets,lengthInputs,doc,rev,txn,epoch,lpart,copyPolicyGeneration,vdigest,igen}` | `EPOCH` | Active target-revision physical copy | `postings={p}` | One row per posting; bucket count from IGEN manifest. | Retain while IGEN and source revisions are selectable. No global term-partition completeness claim is made. |
| `explorer_tree` — rebuildable preorder/path/range index | `V1 ‖ kind ‖ E(IGEN) ‖ B8('T',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ suffix` | `p:path -> TreePathV3{...,txn,epoch,lpart,copyPolicyGeneration,vdigest,igen}`; `r:range -> RangeRefV3{...,txn,epoch,lpart,copyPolicyGeneration,vdigest,igen}`; `o:outline -> OutlineRefV3{...,txn,epoch,lpart,copyPolicyGeneration,vdigest,igen}` | `EPOCH` | Active revision physical copy | `paths={p}`, `ranges={r}`, `outline={o}` | One row per index entry; canonical sections/spans remain authoritative. | Retain while IGEN and revision are selectable. |
| `explorer_associations` — canonical association generations/tombstones plus rebuildable forward/reverse joins | Canonical: `V1 ‖ 'C' ‖ B8('A',D,assoc) ‖ E(D) ‖ E(assoc) ‖ INV64(EPOCH) ‖ E(TXN)`; document direction: `V1 ‖ 'D' ‖ E(IGEN) ‖ B8('A',D,doc) ‖ E(D) ‖ E(doc) ‖ E(rev) ‖ SK ‖ E(sourceID) ‖ E(assoc) ‖ INV64(EPOCH) ‖ E(TXN)`; graph direction: analogous `'G' ‖ E(IGEN) ‖ GK ‖ E(graphID) ‖ E(assoc) ‖ INV64(EPOCH) ‖ E(TXN)` | `o:association -> AssociationV3{state,endpoints,relation,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest}`; `x:ref -> AssociationRefV3{state,assoc,txn,epoch,digest,lpart,copyPolicyGeneration,vdigest,igen}` | `EPOCH` | Active conjunction physical copy | `object={o}`, `index={x}` | Canonical row hashes association ID; directional rows hash lookup endpoint. Winner validation precedes scope/evidence joins. | Canonical generations follow endpoint history; index rows follow IGEN. |
| `explorer_entity_heads` — authoritative expected-base/activation guard | `V1 ‖ 'H' ‖ B8('H',D,kind,id) ‖ E(D) ‖ kind ‖ E(id)` | `s:head -> EntityHeadV2{winnerState,winnerID,winnerEpoch,logicalDigest,lpart,logicalPolicyID,retirementGeneration}`; `s:pending -> PendingMutationV2{txn,preconditionMode,expectedBase,intendedLogicalDigest,intendedLpart,owner,lease,fence,writerAuthorityGeneration,reuse}` | Monotonic state generation | Fixed control visibility | `state={s}` | One row per logical document/node/edge/association or index family. Multi-entity bundles acquire rows in canonical order. | Retained beyond entity/index data so stale writers cannot recreate retired state, fork activation, or bypass `ABSENT_OR_IDENTICAL`. |
| `explorer_commits` — bounded TXN root, allocator/reservation/frontier authority, and immutable epoch outcomes | TXN: `V1 ‖ 'T' ‖ B8('C',D,TXN) ‖ E(D) ‖ E(TXN)`; allocator/frontier: `V1 ‖ 'Q' ‖ B8('C',D) ‖ E(D)`; outcome: `V1 ‖ 'O' ‖ B8('C',D) ‖ E(D) ‖ U64(EPOCH)` | TXN `s:root -> TxnRootV3{state,logicalDigest,tokenHash,epoch,owner,fence,manifestRoot,chunkCount,lparts,result}` and commit copy `p:E(LPART) ‖ U64(COPYGEN) ‖ VDIGEST -> PartitionCommitCopyV1{COMMITTED,txn,epoch,lpart,copyPolicyGeneration,vdigest,logicalDigest,physicalCopyDigest,requiredIndexFamilies}`; allocator `q:{headGeneration,next,retiredThrough,frontier,lastVisibleAt,checkpointDigest,historyFloor,retentionGeneration,writerAuthorityGeneration,writerMode,writerHolder,writerFence,importPlanDigest,importMaxEpoch}`, `r:U64(epoch) -> ReservationV1{reservationGeneration,txn,owner,lease,fence,authorityGeneration,state}`, and `f:INV_TIME(visibleAt) ‖ U64(frontier) -> FrontierCheckpointV1{frontier,visibleAt,predecessorDigest,outcomesDigest,digest}`; outcome `o:terminal -> EpochOutcomeV1{epoch,txn,state,ownerFence,authorityGeneration,digest}` | Allocator head uses its strictly increasing `headGeneration`; reservations use strictly increasing `reservationGeneration`; commit copies/outcomes use epoch; checkpoint history uses frontier | Each `p:*` coordinate uses the physical visibility whose digest is VDIGEST; control cells use service visibility | `root={s}`, `partitionCopies={p}`, `allocator={q,r,f}`, `outcome={o}` | One intentional allocator/frontier row per domain with bounded active reservations; TXN/outcome rows split normally. Checkpoint history is batched and pruned only below history floor; domain partitioning is the scale boundary. | Logical root/outcomes are immutable; commit copies follow retained policy generations; terminal reservations retire only after checkpoint. |
| `explorer_transaction_manifests` — bounded manifest chunks | `V1 ‖ 'M' ‖ B8('M',D,TXN) ‖ E(D) ‖ E(TXN) ‖ U32(chunk)` | `m:chunk -> ManifestChunkV2{index,entryCount,encodedBytes,logicalEntriesDigest,physicalEntriesDigest,previousDigest}`; each entry includes LPART, COPYGEN, VDIGEST, logical digest, and physical-copy digest | TXN state generation | Control visibility; VDIGEST is stored, not raw expressions | `manifest={m}` | At most 4,096 entries and 1 MiB encoded bytes per row. Root row contains only chunk count/totals/logical root digest. | Retain with TXN through repair/idempotency horizon. Verification requires contiguous chunk indices, chained digests, root totals, and every manifest-declared row/copy. |
| `explorer_policy_copies` — sealed physical-copy manifests, per-LPART mapping history, and relabel-generation fence | Copy/fence: `V1 ‖ 'C' ‖ B8('Y',D,LPART) ‖ E(D) ‖ E(LPART) ‖ U64(COPYGEN) ‖ VDIGEST`; map: `V1 ‖ 'M' ‖ B8('Y',D,LPART) ‖ E(D) ‖ E(LPART) ‖ INV64(MAPGEN) ‖ VDIGEST`; relabel root: `V1 ‖ 'G' ‖ B8('Y',D) ‖ E(D) ‖ U64(MAPGEN)` | `c:copy -> PolicyCopyManifestV1{SEALED,lpart,copyPolicyGeneration,vdigest,logicalDigest,physicalCopyDigest,rowCount,manifestRoot}`; `c:fence -> PolicyFence{owner,lease,fence,authorityGeneration,retentionGeneration,publicationMarker?,retirementMarker?}` where the publication marker embeds and digests the exact active `PolicyCopyMapV3`, and retirement binds that publication plus predecessor/successor root digests; `m:active -> PolicyCopyMapV3{lpart,mapPolicyGeneration,copyPolicyGeneration,vdigest,copyDigest,activationKind,activationRef}`; `p:commit -> PolicyGenerationCommitV2{COMMITTED,predecessorGeneration,mode,changedMapRoot,changedLpartCount,digest}` | Fence record generation / COPYGEN / MAPGEN | Control visibility; copied data/commit cells retain their data visibility | `copy={c}`, `mapping={m}`, `generation={p}` | Map rows are immutable but non-selectable until the same-row fence CAS publishes an exact marker. Takeover preserves committed markers. Retirement is selectable-as-retired as soon as its same-row authorization marker commits, and any reconciler may finish the bound root transition after a crash. Missing or mismatched markers fail closed. | Retain mapping history/copies while requests may pin MAPGEN; delete old physical data copies only after leases drain. |
| `explorer_index_generations` — build/seal manifests, durable delta journal, and activation checkpoints | Manifest: `V1 ‖ 'G' ‖ B8('G',D,family,IGEN) ‖ E(D) ‖ E(family) ‖ E(IGEN)`; delta: `V1 ‖ 'D' ‖ B8('G',D,family,IGEN) ‖ E(D) ‖ E(family) ‖ E(IGEN) ‖ U64(EPOCH) ‖ E(TXN)`; activation: `V1 ‖ 'A' ‖ B8('G',D,family) ‖ E(D) ‖ E(family) ‖ INV64(activationEpoch) ‖ E(IGEN)` | `m:manifest -> IndexGenerationV2{state=BUILDING/SEALED,family,igen,schema,buckets,buildThrough,deltaThrough,policyCopyCoverageDigest,digest}`; `d:delta -> IndexDeltaV1{epoch,txn,requiredRowsDigest,state}`; `a:active -> IndexActivationV2{activationEpoch,igen,manifestDigest,txn,lpart,copyPolicyGeneration,vdigest}` | Build generation / content epoch / activation epoch | Control visibility | `manifest={m}`, `delta={d}`, `activation={a}` | Generation-specific delta rows split by epoch. No mutable active pointer; request at H chooses newest committed activation `<=H` and verifies SEALED manifest/copy coverage. | Keep deltas through seal/activation recovery; retain generation until no snapshot/cursor selects it. |
| `explorer_snapshot_leases` — authoritative active reader/cursor pins | `V1 ‖ 'L' ‖ B8('L',D,lease) ‖ E(D) ‖ E(lease)` | `l:lease -> SnapshotLeaseV3{owner,fence,frontier,retentionGeneration,policyGeneration,policyCopyPins,policyCopyPinDigest,indexPins,expiresAt,state}` | Lease generation | Control visibility | `lease={l}` | One row per process-level reader lease or cursor; process leases publish their minimum active H and bounded canonical unions of exact policy-copy/IGEN pins. | Expire only by authenticated lease clock/heartbeat rules; retained briefly for audit. |
| `explorer_retirements` — authoritative per-identity retirement records | `V1 ‖ 'R' ‖ B8('X',D,kind,id) ‖ E(D) ‖ kind ‖ E(id)` | `r:retired -> RetirementV1{retirementGeneration,retiredThroughEpoch,lastDigest,reason,writerAuthorityGeneration}` | Retirement generation | Fixed control visibility | `retirement={r}` | One row per retired identity; checked with entity guard before mutation. | Retained permanently or beyond all token/claim reuse horizons. |
| `explorer_status` — non-authoritative recovery/GC/snapshot hints | `V1 ‖ kind ‖ bucket ‖ E(D) ‖ stateOrDeadline ‖ E(resource)` | `i:ref -> StatusRefV1{authoritativeRow,state,deadline,generation}`; `d:summary -> redacted diagnostic` | Status generation | Control visibility | `index={i}`, `diagnostic={d}` | Time/state buckets. Every entry is rechecked against allocator, TXN, entity-head, or retirement authority. | Hints expire; never used alone for publication, floor, retirement, or writer authority. |
| `explorer_vector_exact` — **deferred** exact embeddings | `V1 ‖ 'V' ‖ B8('V',D,doc) ‖ E(D) ‖ E(model) ‖ E(doc) ‖ E(rev) ‖ K ‖ E(target)` | `v:embedding -> ExactVectorV2{dimension,precision,metric,bytes,txn,epoch,lpart,copyPolicyGeneration,vdigest}` | `EPOCH` | Active target physical copy | `vectors={v}` | One row per target/model. | Not created initially. |
| `explorer_vector_postings` — **deferred** approximate postings | `V1 ‖ 'P' ‖ E(IGEN) ‖ bucket ‖ E(D) ‖ E(model) ‖ U32(centroid) ‖ E(doc) ‖ E(rev) ‖ K ‖ E(target)` | `p:code -> VectorPostingV2{code,exactRef,txn,epoch,lpart,copyPolicyGeneration,vdigest,igen}` | `EPOCH` | Active target physical copy | `postings={p}` | Generation-specific centroid/target buckets. | Not created initially; follows IGEN retention. |
| `explorer_vector_manifests` — **deferred** vector model assets | `V1 ‖ 'M' ‖ B8('V',D,model,IGEN) ‖ E(D) ‖ E(model) ‖ E(IGEN)` | `m:manifest -> VectorManifestV1{metric,dimension,precision,codebookDigest,state}` | Activation epoch | Control visibility; no protected corpus statistics exposed | `manifest={m}` | One row per model generation. | Not created initially. |

The catalog implementation uses bounded `ECA2` internal envelopes for
generation heads, owner/fence metadata (including versioned optional
publication/retirement marker extensions), and policy-copy manifest-set roots.
They include a kind byte, payload length, and SHA-256 checksum. These are
pre-GA V1 coordination formats and may change before the first compatibility
commitment; public object and canonical logical codecs are unaffected.

### 4.3 Why no Accumulo-version revision model

Using one coordinate per logical object and treating cell timestamps as
revisions would couple correctness to versioning iterators, compaction, clock
behavior, equal-timestamp conflicts, and scanner visibility. A standard
versioning policy could permanently remove data still named by a citation or
needed by `AsOf`. Explicit revision/generation rows make retention visible,
testable, and independent of Accumulo's cell-version pruning.

## 5. Operation-by-operation paths

`pkg/explorer.Client` currently names `Ingest`, `Documents`, `Document`,
`Connect`, `Neighborhood`, and `Retrieve`. More exact operations named below
are proposed additive storage-neutral APIs. Their request/response types must
use public document/graph values and opaque cursors, never Accumulo rows or
iterator settings.

| Operation | Read/write path and Accumulo primitives | Deterministic result |
|---|---|---|
| `GetDocument(document, revision?)` | Authenticate; pin checkpoint `H`, floor generation, and index pins. For explicit revision, read `(D,doc,rev)`, source header, and commit partition. For latest, scan document generations in descending epoch, consider committed visible live and tombstone rows, choose the first winner, then interpret state. `Scanner.Stream` is used only for a known single-tablet range; otherwise use `BatchScanner.Stream`. | Explicit retained live revision returns exactly; latest tombstone winner returns `not_found`. |
| Revision history / `AsOf` | Read current `q` frontier or seek the newest allocator-row `f` history cell at or before `AsOf`; read authoritative history floor; scan generation rows at `<=H`. Recheck floor generation before serialization. | Epoch descending, including tombstones. Below-floor request is `not_found`. |
| `GetOutline` | Resolve an explicit revision or live document winner; use the pinned policy-copy map and tree IGEN for candidate order; batch-hydrate canonical sections/spans and verify full tree constraints and active LPART commit copies. | Preorder; siblings by `Order`. Index/canonical detectable mismatch after commit is `internal`. |
| `GetSection` / `ListChildren` / `GetSpans` | Resolve retained revision, point-read canonical section, scan canonical membership rows, batch-hydrate canonical spans, and verify source-slice digests. | Children/spans by `Order`; duplicate order is committed corruption. |
| `ResolveCitation` | Validate citation; resolve retained revision and section/span relationships; read source header and only intersecting source chunks; validate byte boundaries/digest; slice `[Start.Offset,End.Offset)` from canonical UTF-8 bytes. Section-only citations use the same path. | Exact citation plus canonical quote; hidden/absent is `not_found`; digest/range corruption is `internal`. |
| Association lookup | Pin association IGEN; scan the requested document or graph direction; batch-hydrate canonical association objects; validate both endpoint winners/explicit revision, TXN, epoch, visibility, and IGEN. | Deterministic association-ID order after endpoint validation. |
| Evidence hydration | Deduplicate citation/association/path tuples; batch-read canonical revision/source/section/span, association, edge, and node rows; validate active LPART/VDIGEST copies, winners, and directed paths; reconstruct quotes; replay in candidate rank order. | No stale revision/copy/edge, hidden endpoint, unassociated result, or reverse walk can produce evidence. |
| `GetNode` | Scan committed visible node generations `<=H`, select winner including tombstones, then interpret. | Live node or `not_found` for a tombstone winner. |
| `GetEdge` | Scan canonical edge generations `<=H`, select the committed active-copy winner including tombstones, then validate **both** endpoint-node winners are live, visible, committed, above floor, unretired, and endpoint-consistent. | Live edge only when both endpoints pass; otherwise `not_found`. |
| `Neighbors` | Resolve the seed-node winner before scanning. Generate all ranges for pinned outgoing/incoming IGEN buckets. Adjacency rows only nominate edge IDs. Batch-resolve canonical edge winners and **both** endpoint-node winners; reject any hidden, retired, absent, tombstone, uncommitted, below-floor, or endpoint-inconsistent candidate; then merge/sort and apply limits. | Canonical, live, deterministic nodes/edges in the neighborhood result shape. Incoming results do not fabricate rooted paths. |
| Bounded multi-hop neighborhood | Perform level-synchronous expansion with both endpoint winners validated at every hop. Seeds, directions, candidates, per-source limits, and global limits use Section 3.3 total orders. Hydrate a complete level before admission. Directed `graph.Path` state advances only on outgoing canonical edges; incoming/mixed discoveries remain neighborhood-only. | Same nodes/edges across scanner/tablet/bucket order; only direction-connected paths are emitted. |
| Lexical retrieval | Pin lexical IGEN/analyzer. Scan term buckets; batch-resolve document winners; discard postings whose revision is not the live winner at `H`; validate source target and association requirements; compute authorized-projection statistics/scores; bounded oversample/refill; hydrate. | Superseded/deleted postings never score or displace live results. |
| Standalone tree retrieval | Requires normalized document scope. Pin tree IGEN, bound scoped document winners, scan tree entries, validate canonical rows, and score. Without document seeds it fails before scanning. | Exact bounded scoped behavior; empty-seed shape is deterministic `unavailable`. |
| Standalone graph retrieval | Requires normalized node scope or document scope yielding bounded association seeds. Validate seed and every subsequent source/target winner before limits. Path-producing retrieval uses outgoing directed expansion only. Without seeds, or when a path-required shape requests incoming/mixed expansion, fail before scanning. | Empty/unrepresentable path-required shape is deterministic `unavailable`; no invalid evidence path. |
| Hybrid tree/graph retrieval | Planner order is lexical candidate generation, then tree refinement, then graph expansion. A prior implemented mode MAY provide at most the configured seed budget; every seed and both edge endpoints are winner/association validated. Incoming/mixed neighborhood candidates may be used only without claiming a path. | No corpus-wide seed scan; same pinned policy copies, IGENs, and frontier across modes; evidence paths remain directed. |
| Hybrid retrieval | Each requested implemented mode produces a deterministic ranked list at the same snapshot. A shared fusion function combines them; result/evidence deduplication occurs once. | Adapter-independent fusion and tie rules. Missing requested mode is `unavailable`, not omission. |
| Vector retrieval | **Deferred.** Initial adapter detects `ModeVector` before scanning and returns `unavailable`, unless a later ratified Accumulo-native manifest/scorer implementation is enabled in both adapters. | No external vector DB and no silent lexical substitute. |
| `Ingest(pkg/code.IngestRequest)` | Use the public idempotency key; materialize Section 6.9 records; claim `(D,doc,rev)`; acquire entity guards with `APPEND`/`ABSENT_OR_IDENTICAL`; build chunked LPART/VDIGEST manifest; reserve epoch under writer authority; write initial physical copies and pinned IGEN rows; verify; atomically write all initial commit copies on TXN row; terminally commit reservation; wait for checkpoint. | First success `IngestApplied`; retry `IngestUnchanged` with identical document/graph artifact IDs and shared graph-ID reuse decisions. |
| Generic document create/update/delete | Require token and expected base. Claim token/TXN and revision; acquire document guard; check retirement/floor/authority; reserve/write/verify/publish; finalize guard head; release. | Lost response reads TXN. Base mismatch, retired retry, or token mismatch is `conflict`. |
| Generic graph mutation | Require token and expected base for each entity. Acquire guards in canonical order; write canonical generations, pinned adjacency/association generations, and stale-adjacency retirement entries; publish once; finalize all heads. | Concurrent endpoint replacements serialize; partial guard acquisition is aborted/released. |
| Logical delete | Publish a tombstone revision or graph generation through the normal fence. Remove derived current visibility through tombstone entries, not physical deletes. | Latest reads return `not_found`; earlier retained `AsOf` remains exact. |
| Policy relabel / rollback | Acquire each LPART policy guard; copy manifest-declared cells/commit copy with unchanged logical bytes/LPART and new `(COPYGEN,VDIGEST)`; write `MAPGEN` mappings; commit one changed-map root; switch authorization decisions only afterward. Rollback creates a newer MAPGEN selecting a prior sealed COPYGEN/VDIGEST. | Before root: prior mapping only. After root: exactly one selected copy per LPART. Temporary duplicates never count. |
| Rebuild/activate index | Register BUILDING IGEN before `B`; make later publications write durable delta rows and both generations; replay through `W`; seal; acquire family guard; publish activation at `A>W`; retain old IGEN for pinned leases. | Requests pin old complete IGEN before A and new complete IGEN at/after A; no publication falls between journal, seal, and activation. |
| Physical purge / GC | Acquire affected entity guards; publish authoritative retirements; atomically advance allocator history floor/retention generation before deletion; wait for readers/cursors; exact-visibility-delete data and retire IGENs. Writers/readers check floor and retirement authority. | Crash after floor but before delete leaves extra inaccessible data; stale retry cannot resurrect. |
| Batch hydration | Group exact ranges by table/locality group, use bounded BatchScanner streams, decode into maps keyed by full public identity, batch-check commits, and replay in requested rank/order. | Never one RPC per candidate; never BatchScanner return order. |

### 5.1 Iterator boundary

Adapter-controlled server iterators may perform:

- visibility/deletion/version filtering in the configured Accumulo stack;
- content-epoch and pinned-IGEN checks encoded in rows/values;
- exact term/field predicates within a tablet range;
- projection;
- bounded tablet-local scoring only when every input is already authorized,
  committed, winner-filtered, and authorization-independent or computed from
  that authorized projection.

They must not perform network calls, writes, cross-table joins, multi-hop
traversal, unbounded regex/path/posting buffering, authorization after
aggregation, or claim tablet-local counts/top-K are globally final.
Transaction-fence, winner, target-liveness, association, history-floor, and
cross-table hydration checks remain in the bounded query coordinator.

## 6. Distributed correctness

### 6.1 Exact responsibility split

| Accumulo responsibility | Explorer coordination responsibility |
|---|---|
| Atomic application of updates within one row mutation. | Define one authoritative transaction row and never infer cross-row atomicity. |
| Row-local conditional compare-and-mutate once the required native API is exposed. | Claims, fencing tokens, leases, state transitions, epoch allocation, and commit publication. |
| Durable acknowledged mutations according to selected BatchWriter durability. | Treat ambiguous BatchWriter outcomes as “may have committed”; scan and reconcile deterministic coordinates. |
| Key ordering, visibility filtering, tablet routing, scans, splits, compaction, and bulk import. | Pin snapshots, merge ranges, impose public order, verify manifests, and discard non-cleanup partial failures. |
| Per-cell versions/tombstones. | Keep semantic history in explicit rows and enforce retention. |

### 6.2 Ingestion state machine

Authoritative transaction state is:

`ABSENT -> CLAIMED -> PLANNED -> GUARDS_ACQUIRED -> EPOCH_RESERVED -> WRITING -> VERIFIED -> PREPARED -> COMMITTED`

Terminal alternatives are `ABORTED`, `CONFLICTED`, and `POISONED`.
`RETRYABLE` is a diagnostic classification, not a publication state.

1. **Validate.** Validate every public value, source digest, tree, range,
   graph endpoint, expected base, token, UTF-8 source bytes, bounds,
   authorization decision, association plan, and artifact plan.
2. **Claim TXN and revision.** Conditionally create the bounded TXN root using
   deterministic TXN. Conditionally create
   `(domain,document,revision)` claims with owner/fence/digest. Same digest may
   resume; different digest conflicts. Recovery after a crash before planning
   transitions owned claims to `ABORTED` and retains the claim digest. Claim
   rows use control visibility and are never read by object APIs.
3. **Plan and chunk durably.** Produce ordered manifest templates for every
   intended row/cell, logical digest, LPART, initial COPYGEN,
   VDIGEST, symbolic content epoch slot, IGEN, and entity guard. The logical
   root digest covers public/logical bytes and LPARTs but excludes MAPGEN,
   COPYGEN, VDIGEST, and physical visibility. Each physical-copy digest
   covers the expanded coordinate/value, LPART, COPYGEN, VDIGEST,
   and visibility bytes.
   After reservation, verification deterministically expands the one epoch slot;
   manifest chunks are not rewritten. Write bounded chained chunks. The TXN
   root stores only chunk count, total entries/bytes, LPART IDs, result
   identities, and the Merkle/chained root digest.
4. **Acquire entity guards.** Acquire the manifest-declared
   document/node/edge/association rows in canonical order, checking exact
   expected bases, retirement generation, writer authority, and no
   incompatible pending TXN. Partial acquisition is released in reverse
   order on abort. Takeover first increments the TXN fencing token; stale TXN
   transitions and guard updates then fail.
5. **Reserve epoch atomically.** One conditional mutation on the allocator
   row requires the expected `next`, history/retention generation, and writer
   authority. It writes both `next+1` and owner-bearing reservation
   `r:epoch`. Thus allocation cannot advance without a recoverable slot.
6. **Write.** Materialize the reserved epoch into final keys/values and use
   deterministic explicit timestamps. BatchWriter ambiguous failures are
   reconciled against manifest-declared coordinates; conflicting bytes mark
   `POISONED`. New LPARTs also write initial `PolicyCopyMapV3` rows whose
   activation reference is this content TXN; those maps remain inactive until
   publication/outcome.
7. **Verify.** Require contiguous manifest chunks and root totals, then verify
   every manifest-declared row/cell/digest, source digest/chunks, public tree
   constraints, association endpoints, and forward/reverse entries.
   Verification is intentionally scoped to declared artifacts and detectable
   canonical/index inconsistencies; it does not claim a global
   term-partition completeness fence for planner omissions.
8. **Prepare.** Recheck TXN fence, held entity guards, expected bases,
   retirement generation, writer authority, and sealed IGEN manifests, then
   conditionally transition to `PREPARED`.
9. **Publish logical partitions atomically.** One conditional mutation on the
   TXN row changes the root to `COMMITTED` and writes every initial commit
   copy at `p:<LPART,COPYGEN,VDIGEST>`. A TXN may therefore contain
   per-cell visibilities while remaining one publication unit. Commit copies
   expose no hidden LPART counts or visibility labels. Published logical
   bytes/LPARTs are immutable; later policy-generation copies MUST preserve
   them exactly. The content TXN/outcome activates its initial LPART maps.
10. **Commit the epoch outcome.** After TXN publication, conditionally change
    the allocator reservation to terminal `COMMITTED`, then conditionally
    create the immutable epoch-outcome row as absent-or-identical. For
    abort/conflict/poison, write the matching terminal reservation and outcome
    without a commit fence.
11. **Checkpoint.** The frontier worker advances only over contiguous
    immutable outcome rows. One conditional allocator-row mutation updates
    current `H/visible_at/checkpointDigest` and appends the matching historical
    checkpoint cell.
12. **Finalize.** Idempotently update entity heads to the committed winner and
    release pending guards. Public write success waits for both finalization
    and a checkpoint covering the epoch. A crash after publication may delay
    future writers but cannot hide or mix the committed generation once
    checkpointed.

### 6.3 Idempotency and concurrency

- Deterministic TXNs/coordinates exclude wall time, process ID, retry count,
  lease expiry, and Accumulo-assigned timestamps.
- Generic token claims are permanent through the idempotency horizon. Same
  token/same digest resumes or returns stored result; same token/different
  digest is `conflict`.
- Revision claims are keyed by `(D,document,revision)`. `ABORTED` claims keep
  the digest, owner history, and retirement generation; only the same logical
  request may resume. `POISONED` claims require operator repair.
- Writer identity includes process and boot/session identity. Leases are
  liveness hints; fencing tokens and conditional rows provide correctness.
- Concurrent identical requests converge. Concurrent updates from the same
  expected base contend on entity guards; only one publishes and others get
  `conflict`. Concurrent updates from different current bases are rejected.
- Distinct document revisions may serialize and both commit when each names
  the then-current expected base. Publication epochs preserve all history.
- A caller without write authority is rejected before claims are probed.
  Conflict/not-found messages contain no existing digest, owner, epoch, or
  identity details.

### 6.4 Row-atomic allocation, outcomes, checkpoints, and ambiguous responses

The allocator row is the authority for `next`, active owner-bearing
reservations, history floor, retention generation, and writer authority. One
row mutation performs allocation plus reservation. The active window is
bounded; allocation returns `unavailable` rather than exceed it.
The mutable `q:head` record starts at `headGeneration=1`; every accepted
logical head mutation increments it exactly once and writes the head cell at
that explicit timestamp. Epoch and frontier values are never reused as head
timestamps, because a later retirement or low-frontier checkpoint must not be
shadowed by an older allocation with a larger epoch timestamp.
Bootstrap calls `EnsureInitialized` with trusted domain configuration and
conditionally creates exactly `q:head` at explicit timestamp 1. An identical
existing initial head is success; a different valid head conflicts, and an
invalid record/timestamp pair is corruption. There is no external
implicit-timestamp seeding requirement.

Each active reservation starts at `reservationGeneration=1`. Every mutable
reservation transition increments that generation exactly once and uses it as
the reservation cell timestamp; the epoch remains in the qualifier and value.
This avoids same-coordinate/same-timestamp differing values and leaves a
monotonic version axis for later lease renewal or fenced takeover work.

Each terminal reservation is copied to an immutable outcome row:

- `COMMITTED` outcome requires the matching TXN root and every initial LPART
  commit-copy cell to exist first;
- hole outcomes (`ABORTED`, `CONFLICTED`, `POISONED`) require no commit cell;
- the frontier worker accepts only an outcome matching reservation TXN,
  owner/fence, authority generation, epoch, and terminal state.

A frontier checkpoint mutation is row-atomic. Latest reads fetch
`q:{frontier,lastVisibleAt}`. `AsOf=A` seeks the first `f` cell at
`INV_TIME(A)` on the same row. Because the mutable head and immutable history
cell are written in one conditional row mutation, there is no state where
the frontier and historical timeline disagree or fork.

After a checkpoint covers terminal epochs, a conditional allocator mutation
may advance `retiredThrough` and remove their active reservation cells.
Immutable outcomes and retained `f` history cells remain authoritative.

Every ambiguous conditional response is resolved by rereading the exact
authoritative coordinate:

| Mutation | Readback decision |
|---|---|
| Allocation/reservation | If `r:epoch` matches TXN/owner/fence/authority, use it. If neither `next` nor reservation changed, retry. The atomic row mutation cannot produce `next` without its reservation. |
| TXN/claim/guard transition | Matching state and fence means accepted; unchanged expected state means retry; different fence/owner means stale writer stops. |
| LPART publication | All expected initial LPART/VDIGEST commit-copy cells plus committed root/logical digest means committed; absence means retry only while the same fence is held. |
| Terminal reservation/outcome | Matching terminal state is success; contradictory state is `internal`. |
| Frontier checkpoint | Matching `q` head and `f` history cell is success. If both remain at the predecessor, retry the same conditional mutation. Any one-sided state is impossible for a valid row mutation and is `internal`. |
| History floor/writer authority | Read the allocator generation; any mismatch fences the old operation and is `conflict`. |

### 6.5 Snapshot and publication fence

At request start the coordinator pins:

```text
Snapshot = {
  domain,
  snapshotLease,
  stableFrontier,
  visibleAt,
  authorizationFingerprint,
  policyGeneration,
  policyCopyPins,
  historyFloor,
  retentionGeneration,
  writerAuthorityGeneration,
  requiredIndexGenerations
}
```

Before data scans, the coordinator verifies the pinned policy-generation
authority. For each encountered LPART it selects the greatest mapping
generation `<=` the request generation and verifies its activation:
`CONTENT_TXN` mappings require the original TXN commit/outcome; `POLICY_ROOT`
mappings require the referenced committed relabel root and changed-map digest.
`policyCopyPins` is the authenticated digest/cache handle for the resolved
LPART mappings and may be filled lazily.

All candidate rows contain
`{TXN,EPOCH,LPART,COPYGEN,VDIGEST,logicalDigest}` and rebuildable
rows also contain `IGEN`. The coordinator accepts a row only if:

1. the greatest active LPART mapping at or below the pinned request policy
   generation selects exactly one VDIGEST;
2. the row matches the mapping's
   `(LPART, COPYGEN, VDIGEST)` physical-copy tuple;
3. the matching commit-copy cell is visible and agrees with TXN, epoch,
   LPART, logical/physical-copy digests, and required logical index families;
4. the immutable epoch outcome is `COMMITTED` for that TXN/authority;
5. `historyFloor <= EPOCH <= stableFrontier`;
6. a rebuildable row's IGEN equals the request pin;
7. no authoritative retirement record excludes the requested logical or
   exact identity at the pinned retention generation;
8. canonical winner, liveness, and association checks succeed.

If old and new physical visibility copies are both returned by Accumulo, the
coordinator first rejects any copy not selected by the pinned policy map,
then deduplicates by
`(logical row identity, TXN, EPOCH, LPART, logicalDigest, IGEN)`. Temporary
copies therefore cannot double count, displace candidates, or consume limits.

The coordinator creates/refreshes an authoritative snapshot lease before
data scans. Short unary requests MAY share a process lease containing the
minimum active H and union of active policy-copy/IGEN pins; cursors require
their own lease. Policy-copy pins are bounded, canonically sorted, duplicate
free `(LPART, MAPGEN, COPYGEN, VDIGEST)` tuples. Their aggregate digest is a
canonical commitment for cache keys, not a membership oracle. Index pins are
bounded, canonically sorted, duplicate-free `(family, IGEN)` tuples.
Retirement tests exact tuple membership in active, unexpired leases and fails
closed on malformed records or scan bounds. Failure to establish/refresh a
required lease is `unavailable`.

Snapshot lease wire version 3 adds the exact policy-copy tuple list and is
intentionally incompatible with version 2. M3 is pre-GA and has no supported
persisted live lease records; deployments using development version-2 records
must discard them rather than attempting an ambiguous migration.

Accumulo may return data written after the request started, but its epoch is
greater than the pin and is rejected. Data at or below the pin is immutable.
Therefore separate scans of revisions, sections, graph indexes, postings,
hydration rows, and commit cells produce one logical snapshot even though
the physical scans are not transactional.

The allocator history/retention generation and authorization generation are
rechecked before serialization. If a new floor or retirement excludes the
pin/identity, return `not_found`. Any other read-side retention-generation or
authorization-generation change returns `unavailable` so a retry repins.
A changed writer authority does not invalidate a read but is included in
diagnostics/cursor binding.

### 6.6 Index generations and detectable consistency

Each request resolves one activation checkpoint `<=H` for lexical, tree,
outgoing adjacency, incoming adjacency, and association indexes. It then
verifies the referenced sealed manifest. All scans and cursor state use those
IGENs.

The IGEN manifest covers every `(LPART,COPYGEN,VDIGEST)` still selectable by
an unexpired policy-generation lease. A request rejects the IGEN as
`unavailable` if its selected policy copy is absent from that coverage.
Concurrent policy-copy work dual-writes active and BUILDING IGENs and records
their deltas, or delays its MAPGEN root until coverage is verified.

Index activation itself is a normal control TXN with an owner-bearing content
epoch reservation and writer-authority generation; it publishes no public
document/graph object.

Rebuild protocol:

1. create a `BUILDING` IGEN registration **before** capturing frontier `B`;
   writers reserving later epochs record the building generation in their
   manifest and, before their content commit, write both its index rows and
   manifest-declared `IndexDeltaV1` row;
2. build canonical rows through `B`, then replay the durable generation delta
   journal in epoch order;
3. close a short generation-seal barrier, capture delta watermark `W`, and
   require every publication `B < E <= W` terminal and present in the new
   generation;
4. verify manifest-declared rows, canonical references, forward/reverse
   parity, visibility equality, and sampled/full checks required by the
   family;
5. seal the manifest with `{buildThrough=B,deltaThrough=W}`;
6. acquire the index-family entity guard with expected current IGEN, reserve
   unique activation epoch `A > W`, and write the committed activation row;
7. release the barrier. Publications reserving `E >= A` use the new active
   generation; any publication reserved before `A` was covered through `W`
   or remains barrier-blocked until its new-generation rows are verified;
8. keep the old generation until no retained snapshot/cursor selects it.

An activation row is written only after the sealed manifest. A crash before
activation leaves an unused generation; a crash after activation is safe
because the manifest already exists and the family guard blocks a competing
activation until recovery finalizes it. Arbitrarily omitted lexical postings
that were never in the transaction/build manifest are not magically
detectable; the design makes no unsupported completeness claim. Detected
missing manifest rows, dangling targets, winner mismatches, digest failures,
or forward/reverse disagreement after commit are `internal`.

### 6.7 Partial failure, recovery, and reconciliation

- Recovery finds expired nonterminal TXNs through `explorer_status`, then
  rechecks TXN, allocator reservation/outcome, entity guards, claims,
  retirements, and manifest chunks.
- A takeover increments the TXN fence before changing guards/reservations.
- Recovery verifies the manifest, replays missing deterministic rows, and
  resumes at the first incomplete phase.
- Ambiguous conditional results are resolved by rereading the row, never by
  blindly incrementing an epoch or fence.
- Every reservation becomes an immutable terminal outcome so the frontier
  cannot remain blocked indefinitely.
- Derived index entries without a valid fence are invisible and eventually
  deleted. Detectable corruption after a committed fence is quarantined and
  returns `internal`, never `unavailable` or partial success.
- Reconciliation checks manifest root/chunks, declared rows, revision claims,
  canonical winners, source hashes, associations, forward/reverse adjacency,
  outcome/frontier gaps, index activation/manifests, visibility equality,
  guards/retirements, and stale status hints.

### 6.8 Retirement, history floor, and stale-writer prevention

Published citation-bearing revisions are retained indefinitely by default.
For finite retention:

1. acquire affected entity guards under a GC owner/fence;
2. scan authoritative snapshot leases and ensure no active reader/cursor,
   legal hold, nonterminal TXN, IGEN, or
   idempotency retry needs the candidate epochs;
3. write authoritative per-identity retirement rows;
4. atomically advance allocator `historyFloor` and `retentionGeneration`
   **before** physical deletion;
5. wait for pre-floor readers to drain;
6. exact-visibility-delete and compact;
7. retain heads, claims, retirements, outcomes, and checkpoints long enough
   to reject stale retries.

Snapshot leases use coordinator-authenticated UTC expiry plus a configured
clock-skew/grace interval and owner fencing. GC treats an ambiguous heartbeat
as active until readback proves the lease expired or a newer fence owns it.

Retirement identities are explicit: document logical identity,
`(document,revision)`, node/edge logical identity or generation, association,
and IGEN. A document-level retirement does not implicitly erase a retained
revision citation; each retained/purged unit has its own record.

A crash after step 4 leaves extra data that readers reject, which is safe. A
crash before step 4 leaves history readable, so data MUST NOT yet be deleted.
Writers check retirement rows and allocator generations during guard
acquisition/reservation/prepare. An old process cannot publish after GC
because guard/TXN fencing or authority/retention mismatch prevents terminal
`COMMITTED`; even a stray old commit cell fails the immutable outcome check.

### 6.9 Code-ingestion materialization

Materializer version `explorer-code-v1` maps public `pkg/code` values:

- document ID:
  `code.NewStableID("shoal.document", repository locator, canonical source path)`;
- revision ID:
  `code.NewStableID("shoal.revision", document ID, Source.ID(), IngestRequest.IdempotencyKey())`;
- root section ID:
  `code.NewStableID("shoal.section", revision ID, "root")`;
- syntax section/span IDs:
  `code.NewStableID("shoal.section" or "shoal.span", revision ID, SyntaxNode.ID())`;
- graph node IDs: existing syntax, semantic-symbol, and external-entity
  canonical `pkg/code` IDs converted byte-for-byte to `shoal.ID`;
- graph edge IDs: existing public relationship IDs;
- graph artifact ID:
  `code.NewStableID("shoal.graph-publication", IngestRequest.IdempotencyKey())`.

Every `code.ID` above is exposed as `shoal.ID(id.String())`; its bytes are not
rehashed by storage.

The document title/path metadata is deterministic. The root covers the full
source. Syntax nodes become range-preserving ordered sections; each directly
attributable syntax occurrence has a span whose text is sliced from the exact
parse-request source bytes. Parent/child and sibling order follow validated
public parse-result order. Syntax sections/spans associate to syntax graph
nodes; symbols associate to their declared syntax occurrence; relationships
materialize as graph edges. External entities without source association
cannot independently produce citation evidence.

Because Explorer document text and quotes are UTF-8, a code-ingestion source
that is not valid UTF-8 returns `invalid_argument` before claims.

`IngestResult` returns exactly one `ArtifactDocument` with the document ID and
one `ArtifactGraph` with the graph artifact ID when those surfaces are
materialized. The TXN root stores those refs before publication, so retries
return identical values. The graph artifact manifest stores the exact ordered
node/edge IDs and whether each was created or byte-identically reused; reuse
never changes a returned ID. A materializer-version change cannot reinterpret
an already claimed ingestion key; it requires migration or a new public
materialization operation.

One code ingest MAY span multiple visibility classes. All classes remain one
TXN/content epoch: each semantic subset receives a stable LPART, its initial
physical cells carry `COPYGEN=currentPolicyGeneration` plus VDIGEST, and one
row-atomic TXN mutation writes all initial commit-copy cells. The
revision/source LPART uses its source policy; graph/association LPARTs may be
stricter conjunctions. Public success waits for every LPART and the one epoch
outcome.

Code ingestion acquires the document guard with `APPEND`. Concurrent distinct
ingest requests serialize and may both commit; the later content epoch is the
latest Explorer revision, while both exact revision IDs remain retained.

Code-owned graph node/edge guards use
`ABSENT_OR_IDENTICAL(intendedLogicalDigest,intendedLPART)` atomically:

1. if the guard head is absent, the mutation installs a pending create;
2. if the live head has byte-identical canonical graph bytes, the same graph
   ID, and the same LPART/logical access-policy identity, it installs a
   pending `REUSE` marker and performs no canonical graph generation or
   adjacency rewrite;
3. a tombstone, different canonical bytes, different LPART, or incompatible
   logical visibility semantics returns `conflict`;
4. an existing pending owner is waited on or fenced through normal takeover;
   the decision is made only after the guard is acquired, never from an
   unprotected pre-read.

Thus two concurrent ingests sharing an external entity serialize at its guard:
one may create it and the other then reuses the identical ID/bytes, while each
still publishes its own revision associations. A lost response returns the
stored artifact manifest. Physical VDIGEST differences caused solely by an
authorized policy relabel do not make logical visibility incompatible,
because LPART and logical policy identity remain stable.

## 7. Security and authorization

### 7.1 Current gap and proposed boundary

`retrieval.Request` and `proto/knowledge.proto:RetrieveRequest` carry no
principal, tenant, source, or Accumulo authorization labels. The base gRPC
server passes `context.Context` through but does not authenticate it. M2
therefore implements authorization as a trusted resolver plus
`pkg/explorer/authorized.Client`; an unwrapped client or an untrusted
interceptor remains unauthenticated.

Northbound deployments authenticate with a trusted interceptor and inject an
immutable `pkg/explorer/auth.Decision` containing:

- subject and actor/client (including an on-behalf-of chain when applicable);
- authorization domain;
- permitted sources/policies and operation;
- policy generation and authentication expiry;
- request/correlation ID and optional audit purpose.

Embedded callers use an equally trusted host resolver. Capability-bound
binders prevent one authority instance from forging another's context.
Untrusted
`context.WithValue` data is not an authentication boundary. Missing context
fails closed, except for explicit migration/recovery service identities.

### 7.2 Label model

`pkg/explorer/auth.Policy` uses a stricter grammar than the low-level Accumulo API, which
accepts arbitrary byte labels:

- `d:<encoded-domain>` — immutable domain boundary;
- `s:<encoded-source>` — immutable source boundary;
- `g:<encoded-policy>:e:<epoch>` — policy grant and policy epoch;
- `svc:<role>` — control/background role, never issued by a caller.

Encoded components are lowercase base32 (or another injective ASCII encoding)
and at most 128 bytes; a flattened expression is at most 4 KiB and 64 terms.
Operators, whitespace, quotes, empty components, Unicode alternatives, and
caller-provided expressions are rejected.

A normal object visibility is generated from structured policy, parsed with
`accumulo.NewColumnVisibility`, normalized with `Flatten`, and stored in
canonical form. No user-data cell has blank visibility. Graph `Labels`,
`Metadata`, retrieval `Scope`, claimed ownership, and protobuf fields never
increase authority.

The security authority also supplies the stable logical policy identity used
in LPART derivation. Policy generation changes grants/visibility and VDIGEST,
not that logical identity.

All document revision components share one revision LPART and the active
VDIGEST selected for the request policy generation. Property-level or
section-level ACLs are not representable without changing current
whole-object semantics; such content must be split into separate
documents/LPARTs.

### 7.3 Scanner authorizations

For every operation the adapter:

1. authenticates and authorizes the operation/domain;
2. intersects requested scope with authorized sources;
3. derives only the required domain/source/current-policy labels;
4. intersects them with the configured Accumulo service-account ceiling;
5. supplies the resulting byte labels through
   `accumulo.ScannerOptions.Authorizations` on every data and commit scan.

Northbound users never receive Accumulo credentials or submit labels,
tables, rows, column families, iterator classes, or visibility expressions.
Separate least-privilege accounts are used for data reads, writes,
coordination, background derivation, migration, and security administration.

### 7.4 Derived/index/edge/evidence rules

- A locator, tree entry, lexical posting, vector entry, cache entry, or
  derived fact is exactly as restrictive as its target or stricter.
- An edge and both adjacency copies use the conjunction of the edge policy
  and both endpoint policies.
- A derived result uses the conjunction of all input visibilities, never
  their union.
- Counts, document frequencies, bucket populations, degree, frontier size,
  top-K, and ranking statistics are computed only over the exact
  caller-authorized projection. The only alternative is a fixed
  authorization-independent constant that cannot change with hidden corpus
  contents.
- A quote is read only from the authorized cited revision.
- A path is emitted only when every node and edge is live/visible and every
  edge is directed from `Nodes[i]` to `Nodes[i+1]`. Redacting a hidden middle
  node or reversing an incoming edge into a rooted path is forbidden.
- An LPART has one logical access-policy identity, but MAY have multiple
  physical policy copies over time. One TXN may contain multiple LPARTs; all
  initial commit copies are written atomically on the TXN row, so
  mixed-visibility code ingestion remains one publication unit without
  broadening any LPART.

### 7.5 Non-disclosure

The adapter must not disclose unauthorized data through:

- existence/not-found differences for individual IDs;
- counts, order gaps, degrees, candidate totals, or traversal-limit messages;
- result displacement, scores, explanations, or corpus statistics;
- positive or negative caches;
- error text, table/row names, visibility expressions, or iterator plans;
- logs, metrics, traces, manifests, recovery queues, or status APIs;
- timing paths that are needlessly different for absent and unauthorized
  probes.

Collection calls omit hidden objects. Direct hidden objects return the same
`not_found` shape as absent ones. Entire-request authentication/authority
failure returns `unauthorized`.

Cache keys include domain, authorization fingerprint, policy generation,
policy-copy pin digest, snapshot frontier, history-floor/retention generation,
query/modes/scope/limits, and every IGEN. Positive and negative caches use
identical partitioning. Raw queries, quotes, source text, IDs, labels,
credentials, and serialized responses are not logged.

### 7.6 Writes and confused-deputy controls

The write path authenticates subject and actor, authorizes the exact
operation/domain/source, derives IDs/rows/visibility server-side, and checks
the expected policy generation. Effective authority is the intersection of:

```text
caller grants
AND operation permission
AND domain/source policy
AND durable writer-authority generation/mode
AND matching embedded and Accumulo authority mirrors during migration
AND Accumulo account permission/authorization ceiling
```

Write requests cannot provide physical coordinates or broaden authority via
scope, metadata, graph labels, or claimed owner IDs. Cross-domain edges are
forbidden. Same-domain cross-source edges require both endpoint grants and
the conjunctive edge visibility. A direct backend writer MUST NOT accept its
local authority mirror when the durable authority or other backend mirror
differs; migration barriers remain closed until all agree.

### 7.7 Policy-copy evolution, activation, rollback, and revocation

- Removing a subject from a stable grant stops issuing that label on future
  scans. Authorization is resolved per request.
- The policy generation is rechecked before response serialization. A change
  discards the in-flight result and returns `unavailable`; a retried request
  is evaluated under the new decision.
- Revocation invalidates auth sessions, scanner pools, positive/negative
  caches, and result caches.
- Relabeling preserves TXN, content epoch, LPART, logical digest, public
  identity, and IGEN. It computes a new VDIGEST and physical-copy digest.
  The copied values and matching commit-copy cell carry
  `(LPART,newCOPYGEN,newVDIGEST)`; normal relabel uses `MAPGEN=COPYGEN`.
  No content reservation/outcome/frontier is created.
- A policy-copy worker acquires the LPART policy guard, writes every
  manifest-declared canonical/index/commit copy using the same bounded
  manifest-chunk format under a maintenance TXN, verifies physical-copy
  digests, writes a sealed `PolicyCopyManifestV1`, and writes the LPART mapping
  row with activation reference to the pending relabel root. These rows are
  inert until that root is committed.
- The final `PolicyGenerationCommitV2` fences the changed-LPART map set. Only
  after it exists may the security authority issue an authorization decision
  pinned to that generation. For each LPART, a reader selects the greatest
  active mapping at or below its pin; LPARTs not changed by this root continue
  to use their earlier content-TXN or relabel mapping. Uncommitted partial
  copies/maps are ignored.
- **Broadening:** finish and commit all new mappings/copies before issuing the
  broader labels. **Narrowing:** build/commit the narrower copies, atomically
  switch the security authority to the new generation and revoke old label
  issuance/sessions, then cancel old-generation requests according to the
  revocation policy.
- A crash before the generation root leaves inert copies/maps that recovery
  resumes or deletes. A crash after the root but before old-copy deletion is
  safe: new requests select only the new VDIGEST and old pinned requests only
  the old mapping. Accumulo visibility returning both copies cannot double
  count because policy-pin filtering and logical deduplication precede all
  counts, scores, and limits.
- Rollback never decrements policy generation. It creates a new monotonically
  increasing generation whose changed-LPART mappings point back to a
  previously sealed VDIGEST/copy (or a verified recreated copy), commits its
  root, then switches label issuance. An ambiguous activation/rollback
  response is resolved from the generation root plus every changed mapping
  and copy; no partial map is accepted.
- Old physical copies and commit-copy cells are exact-visibility deleted only
  after all snapshot leases/caches pinned to their policy generation drain.
- Accumulo visibility cannot revoke bytes already returned to a client, and
  physical old-label removal is not instantaneous until deletion/compaction.

## 8. Query and scaling design

### 8.1 Bounded graph expansion

Each graph request has client-visible requested limits and deployment hard
caps for seed count, depth, frontier size, expanded nodes, scanned/emitted
edges, per-node degree, bucket/range count, hydrated nodes, path count/path
length, scanner cells/bytes, wall time, and heap.

Initial shared defaults are depth 2, 100 edges per node, 1,000 nodes, and
10,000 edges. Both adapters use the same versioned configuration. Deployments
MAY lower hard caps but MUST NOT silently lower one request below its accepted
normalized limits.

For each level:

1. sort and deduplicate the frontier;
2. calculate all bucket ranges and reject before scanning if the range budget
   is exceeded;
3. stream the ranges;
4. merge every pinned-IGEN bucket for every source/seed/direction;
5. batch-resolve the seed/source-node winners and canonical edge winners,
   including tombstones;
6. reject rows whose source winner is dead/hidden/retired/below-floor or whose
   adjacency tuple does not exactly match the edge winner;
7. batch-resolve the other endpoint-node winners and reject dead, hidden,
   retired, below-floor, uncommitted, or endpoint-inconsistent candidates;
8. construct Section 3.3 complete neighborhood ordering tuples and only
   outgoing direction-connected path tuples;
9. sort the whole level, then apply per-source degree and global edge/node
   limits;
10. add admitted targets to the next frontier and record the canonical path
    winner.

Previously visited nodes are not re-expanded. Self-loops/back-edges may be
returned if requested but do not create new frontier entries.

### 8.2 Lexical/tree/hybrid execution

- Analyzer version is part of every posting key and commit manifest.
- Posting rows retain term frequency, positions, offsets, and every input
  needed by the shared scorer.
- Common terms are target-bucketed and pre-split.
- Candidate sets, intersections/unions, and oversampling are bounded.
- Every posting is joined to the live document winner at `H` before it can
  contribute a count, score, seed, refill, or top-K position.
- Hydration occurs in batches, not per candidate.
- Candidates removed by authorization, stale indexes, or residual checks
  trigger bounded refill. If exact top-K cannot be established within budget,
  return `unavailable`.
- Tree indexes accelerate selection, but canonical sections/spans decide
  validity.
- Standalone tree/graph requests use only the bounded seed sources in Section
  3.5. Empty-seed shapes return `unavailable`; they never fall back to a
  corpus-wide scan.
- Hybrid fusion is one shared library/function; the Accumulo adapter does not
  invent a new weighting scheme.

### 8.3 Future vector boundary

Vector implementation is deferred. Acceptable future choices remain
Accumulo-backed:

1. exact authorized-vector scan with bounded tablet-local top-K and global
   merge;
2. versioned Accumulo-native IVF/PQ postings plus exact-vector rerank;
3. another ratified Accumulo-resident index with explicit recall/freshness
   semantics.

An external vector database, an unversioned service-local index, or gRPC as a
replacement storage engine is out of scope. Approximate behavior must be
explicit in the public contract and explanation; it cannot silently replace
an exact request. Unauthorized vectors must not affect candidate limits or
top-K displacement.

### 8.4 Scanner, batching, and ordering

- Use `Scanner.Stream` for a range known to fit one tablet.
- Use `BatchScanner.Stream` for multiple point/range reads and tablet-spanning
  work.
- `UseMultiScan` is a throughput option only; all public output is explicitly
  merged and sorted afterward.
- Scanner batches bound memory. Existing unary `Retrieve` is assembled only
  within response budgets.
- Non-cleanup scan errors discard all accumulated public results. A documented
  `CleanupError` after a complete scan may preserve the complete result while
  recording an operational warning.
- Every batch hydration map is replayed in caller/candidate order.

### 8.5 Splits, locality, compaction, and placement

- Pre-split every salted table by tag/bucket. Verify actual splits before
  bulk import.
- Keep one row per section, span, posting, edge generation, and adjacency
  entry; no whole revision, supernode, or common term occupies one row.
- Version bucket counts and IGENs. Rebucket/rebuild only by sealing a new
  generation and publishing one activation checkpoint. Requests/cursors pin
  lexical, tree, association, outgoing, and incoming IGENs independently.
- Use locality groups only for stable CF sets shown in the schema. Large span
  text, metadata, indexes, manifests, and vectors stay physically separable.
- Use Accumulo block/index caches and Bloom filters on point-lookup tables
  where deployment measurements justify them.
- Keep `maxVersions=1`; semantic history is in row keys. Do not age off data
  above the advertised history/citation floor.
- Separate table compaction cadences. Rate-limit major compactions and monitor
  file count, read amplification, queue depth, and retention tombstones.
- Let Accumulo metadata, manager, and balancer remain tablet-placement
  authorities. The adapter observes skew; it does not pin tablet internals.

### 8.6 Static bounds, query budgets, and overload

The following are initial shared hard maxima. Deployments MAY lower them but
MUST NOT raise them without a versioned conformance/configuration change.

| Item | Hard maximum |
|---|---:|
| Public ID or idempotency operation name | 1,024 bytes |
| Generic idempotency token | 128 bytes |
| Metadata per object | 256 entries; key 256 bytes; value 4 KiB; total 256 KiB |
| Graph labels | 64 labels; 256 bytes each |
| Visibility expression | 64 terms; 128-byte encoded component; 4 KiB flattened |
| Kind/type/source-version/title/heading string | 4 KiB each |
| Explanation summary | 16 KiB |
| Canonical revision source | 512 MiB; 1,000,000-byte maximum chunk payload |
| Sections per revision | 100,000 |
| Spans per revision | 1,000,000 |
| Graph nodes or edges per TXN | 1,000,000 each |
| Associations per TXN | 2,000,000 |
| Logical publication partitions (`LPART`) per TXN | 64 |
| Manifest | 32,000,000 entries; 4,096 entries and 1 MiB encoded bytes per chunk; 64 KiB root |
| Accumulo cell value / row mutation | 1 MiB / 8 MiB encoded |
| Updates per row mutation | 10,000 |
| Retrieval query text / scope IDs / normalized TopK | 16 KiB / 10,000 / 1,000 |
| Retrieval modes / encoded non-source request payload | 4 / 64 MiB |
| Scanner ranges / cells / bytes per request | 50,000 / 1,000,000 / 256 MiB |

IDs and static metadata are checked before claims. A source/manifest may be
stream-validated, but no final writes occur until all cardinality bounds are
known. Exceeding a static bound is `invalid_argument`. Exceeding dynamic
admission, scan, memory, refill, graph, or time budget is `unavailable`; this
is the public equivalent of transport `RESOURCE_EXHAUSTED`.

A weighted admission semaphore estimates:

```text
term count * posting buckets
+ frontier nodes * adjacency buckets
+ hydration rows
+ requested modes
+ expected response bytes
```

Requests exceeding syntactic hard limits fail `invalid_argument` before I/O.
Admission exhaustion, unavailable required indexes, or exact-result budget
exhaustion is `unavailable`. Cancellation/deadline remains distinct. The
adapter never lowers depth, top-K, or requested modes without an explicit
public truncation contract.

### 8.7 Pagination and stable cursors

Existing `Retrieve` remains a complete unary response and gains no cursor.
Additive outline/history/neighborhood APIs may be paginated. Their opaque,
authenticated cursor contains:

- API/schema version;
- domain and authorization fingerprint;
- policy generation;
- request policy generation and resolved LPART→(MAPGEN,COPYGEN,VDIGEST) pin
  digest;
- history-floor/retention generation and writer-authority generation;
- normalized request fingerprint;
- pinned frontier and index generations;
- last complete public sort tuple or full logical key;
- expiry.

Exact/key-ordered pages use seek-after. Ranked pages use
`(score DESC, ID ASC)` search-after. Cursors never contain credentials, raw
queries, visibilities, tablet addresses, or internal plans. Expired,
authorization-changed, history-pruned, or generation-incompatible cursors
fail explicitly rather than restart against a newer view.

### 8.8 Observability

Emit bounded-dimension metrics/traces for operation/mode latency and outcome,
admission queue/rejection, ranges/tablets/cells/bytes, candidates before/after
authorized filtering, hydration/refill, graph counts by depth, iterator
seek/output ratios, cache behavior, bucket/tablet skew, writer retries and
ambiguous failures, frontier lag/gaps, recovery age, compaction debt, bulk
import, manifest chunk/root verification, IGEN activation, history-floor
changes, writer-authority generation/mismatch, cursor age, and
budget-exhaustion reason.

Use request IDs and opaque domain tokens. Do not record raw queries, content,
quotes, public object IDs, labels, visibilities, credentials, or response
payloads. Authorization mismatch and commit corruption are high-severity
signals.

## 9. Failure and recovery semantics

| Failure point | Persisted state | Retry behavior | Recovery owner | Client-visible outcome |
|---|---|---|---|---|
| Public validation/authentication before claim | Nothing | No storage retry | Request handler | `invalid_argument` or request-level `unauthorized` |
| Token/TXN claim rejected, same digest committed | Existing committed TXN/result | Read stored result; do not allocate | Request handler | Exact prior success; code ingestion returns `IngestUnchanged` |
| Token/TXN/revision claim rejected, different digest | Immutable control claim | Never retry unchanged | Request handler/audit | `conflict` with nondisclosing message |
| Partial revision/guard claim then crash | TXN/claim/guard rows name owner/fence; no commit | Fence old owner, mark claim `ABORTED`, release acquired guards, clean manifest-declared staging | Recovery | `unavailable` while recovering; retry resumes same digest |
| Expected-base mismatch, including winner tombstone | Guard head differs from request | No automatic retry | Request handler | `conflict` |
| Code graph `ABSENT_OR_IDENTICAL` sees identical bytes/LPART | Guard records pending `REUSE` | Reuse same ID; no canonical graph rewrite | Ingest worker | Normal success/`IngestUnchanged` on retry |
| Code graph `ABSENT_OR_IDENTICAL` sees different bytes, tombstone, or incompatible LPART | Guard remains unchanged | No automatic retry | Ingest worker | `conflict` |
| Writer authority or retention generation stale | Allocator/guard has newer generation | Old writer stops; no new reservation | Request/migration recovery | `conflict` |
| Planner fails before epoch | `CLAIMED`/`GUARDS_ACQUIRED`/`PLANNED` | Same token may resume; takeover fences prior owner | Ingest recovery | `unavailable`, `canceled`, or `deadline_exceeded` |
| Allocation conditional response lost | Atomic allocator row may contain both increment and owner reservation | Reread exact reservation/next; use matching slot or retry unchanged condition | Coordinator recovery | `unavailable` only if readback cannot complete |
| Safe BatchWriter failure before accepted submission | TXN `WRITING`; known rows absent | Retry within bounded policy | Ingest worker | No partial visibility; eventual success or `unavailable` |
| Ambiguous BatchWriter apply/close failure | Unknown deterministic subset may exist | Writer is discarded; scan manifest coordinates and replay only missing identical rows | Recovery/reconciler | `unavailable`; never claim applied from writer result |
| Process crash during writes | Nonterminal TXN, reserved epoch, partial invisible rows | Lease takeover, manifest verification, deterministic replay | Recovery worker | Retry waits/assists; deadline may expire |
| Manifest-declared coordinate has different bytes after claim | Nonterminal TXN plus conflicting cell | Do not overwrite; mark `POISONED` | Reconciler/operator | `internal` |
| Missing manifest chunk or declared row before commit | No commit; incomplete plan/write | Rebuild chunk/row from canonical request or abort | Ingest/reconciler | `unavailable`; no visible revision |
| Transient verification scan/storage failure before commit | No commit | Retry same TXN within budget | Ingest/reconciler | `unavailable` |
| Deterministic source/index/association digest or structural mismatch before commit | No commit; TXN becomes `POISONED` | No automatic retry until repair | Ingest/reconciler | `internal` |
| Conditional PREPARED transition loses response | Row may be `PREPARED` | Reread exact state/fence | Ingest/recovery | Retry continues |
| Commit mutation rejected by stale fence/authority | Data remains invisible | Stale writer stops; owner recovers/aborts | Recovery | `conflict` |
| Initial multi-LPART commit response lost | Root and all initial LPART commit-copy cells may exist atomically | Reread exact root/LPART/logical and physical-copy digest set; never allocate again | Request handler | Retry returns prior success/unchanged |
| TXN committed, reservation not terminal | Commit invisible because no COMMITTED outcome/frontier | Recovery terminally commits matching reservation and writes outcome | Coordinator recovery | Write waits or returns `unavailable`; retry discovers TXN |
| Reservation terminal, outcome response lost | Allocator terminal cell exists; outcome may exist | Read outcome; write same immutable row if absent | Watermark recovery | No read visibility until outcome/checkpoint |
| Earlier reservation nonterminal | Later outcomes exist; frontier remains old | Take over and commit or write terminal hole | Watermark/recovery | Reads continue at old checkpoint |
| Checkpoint response lost | Allocator row may atomically contain both new `q` head and `f` history cell | Reread both; retry only if both remain at predecessor | Watermark | No frontier/timeline disagreement; write may wait |
| Scanner returns values plus non-cleanup error | No write-state change | Discard all values; retry whole logical operation at same pin if safe | Query coordinator | `unavailable`, `canceled`, or `deadline_exceeded`; never partial success |
| Scanner cleanup-only error after complete read | Complete values usable by public scanner contract | Return complete result; record warning | Query coordinator | Success |
| Committed manifest-declared row/source chunk/index ref missing or digest-mismatched | Commit/outcome/checkpoint exists; detectable corruption | Quarantine and operator repair; no transparent retry | Reconciler/operator | `internal` |
| Posting belongs to superseded/tombstone document winner | Normal historical index row | Discard before scoring/limits | Query coordinator | Not returned; no error |
| Adjacency disagrees with canonical edge winner or dead target | Stale/detectably corrupt index row | Discard; alert/reconcile if manifest-declared current row | Query/reconciler | Not returned; current committed inconsistency is `internal` if exact result cannot be established |
| Required IGEN unavailable/unsealed/mixed | Snapshot valid; index pin unusable | Do not mix/fallback silently | Query coordinator | `unavailable` |
| Index rebuild crashes around `B`, delta `W`, seal, or activation `A` | BUILDING/SEALED manifest, delta rows, and/or activation may exist | Replay manifest-declared deltas through W; verify seal; read activation TXN/outcome; old IGEN remains active until committed A | Index recovery | Serving stays on old complete IGEN or returns `unavailable`; never mixes |
| Policy-copy crash before generation root | Partial copied cells/maps/manifests, no active generation fence | Resume or delete; readers keep prior generation | Policy-copy recovery | No client-visible change |
| Policy-copy crash after generation root before old deletion | Both physical copies may exist; one mapping is active per pinned generation | Deduplicate/filter by policy pin; resume old-copy cleanup after leases drain | Policy-copy recovery | Normal result with no duplicate/count/rank change |
| Policy rollback activation response lost | New monotonic generation root may exist | Read generation root and every changed LPART mapping/copy; never decrement or accept partial map | Security authority | `unavailable` only while pin cannot be established |
| Snapshot lease cannot be created/refreshed | No safe GC pin | Do not start/continue data scans | Query coordinator | `unavailable` |
| Standalone tree/graph has no bounded seed source | No I/O | Do not corpus-scan | Query coordinator | `unavailable` |
| Path-required incoming/mixed traversal | No representable directed `graph.Path` | Do not scan or synthesize a walk | Query coordinator | `unavailable`; optional neighborhood path is omitted |
| New history floor/retirement excludes pinned snapshot or identity | New authoritative retention state | Do not restart at current view | Query coordinator | `not_found` |
| Authorization or read-side retention generation changes but pin remains valid | Stale decision/generation; immutable data | Discard and retry under new generation | Query coordinator | `unavailable` |
| Cursor expired or snapshot below history floor | Authoritative expiry/floor | Do not restart at current view | API handler | `not_found` |
| GC advances floor then crashes before delete | New floor/retirements authoritative; old data remains | Resume exact deletes | GC worker | Old reads `not_found`; current reads unaffected |
| Stale writer attempts after retirement/GC | Old guard/floor/authority generation | Conditional publish/outcome fails; staging cleaned | Recovery | `conflict`; no resurrection |
| Cutover/rollback authority response lost or one mirror is stale | Durable authority and either/both backend mirrors may differ | Keep barrier closed; read durable authority plus embedded guard and Accumulo allocator; repair both mirrors from durable generation/mode/fence; never accept a backend-local value | Migration controller | Writes remain `unavailable` until all three records and routing agree |
| Stale direct embedded writer after cutover | Embedded guard carries newer `ACCUMULO_PRIMARY` generation | Conditional guard check rejects before source apply | Embedded write path | `conflict`; no write/journal bypass |
| Stale direct Accumulo writer after rollback | Accumulo allocator carries newer `EMBEDDED_PRIMARY` generation | Reservation/authority condition rejects before epoch allocation | Accumulo write path | `conflict`; no target write |
| Bulk import FATE outcome unknown | Durable FATE ID and TXN manifest | Use public allocate/submit/wait/finish lifecycle and persisted ID | Migration/ingest recovery | `unavailable` until resolved |

## 10. Embedded/Accumulo conformance

### 10.1 Harness

Create one test-only high-level harness implemented by:

1. embedded direct adapter;
2. embedded loopback gRPC adapter where an RPC exists;
3. Accumulo direct adapter;
4. Accumulo loopback gRPC adapter.

Fixtures contain only public document, graph, retrieval, ingestion, error, and
authorization-context values. They never contain rows, CFs, iterators, or
scanner types. Both backends receive the same IDs, source bytes, timestamps,
policy decisions, scorer/analyzer versions, fake clock, and fault script.

Canonical comparison hashes logical values, not physical cells:

1. source-byte SHA-256;
2. canonical record digest;
3. revision/bundle digest over sorted logical leaves;
4. LPART logical digest plus selected physical-copy digest
   `(MAPGEN,COPYGEN,VDIGEST)`;
5. watermark manifest digest including codec/schema version.

### 10.2 Test matrix

| Area / case | Existing coverage | Proposed shared assertions |
|---|---|---|
| Range/citation validation | **Existing:** `pkg/document/document_test.go` covers invalid offsets/pages and citation requirements. | Both adapters reject before I/O; valid half-open boundaries and empty ranges resolve identically. |
| Graph path validation | **Existing:** `pkg/graph/graph_test.go` covers path structure/connectivity. | Stored/hydrated paths remain connected, ordered, authorized, and byte-identical. |
| Retrieval request validation | **Existing:** `pkg/retrieval/retrieval_test.go` covers text/mode/scope/as-of validation. | Shared normalization fixes zero TopK, empty/duplicate modes/scopes, limits, and scope combination. |
| gRPC round trip/order/errors | **Existing:** `pkg/retrieval/grpc/retrievalgrpc_test.go` covers rich request/response conversion, nil normalization, cancellation, and public error mapping. | Direct and loopback adapters produce identical ordered values and deterministic protobuf bytes. |
| Response integrity | **Existing:** public gRPC validation rejects malformed citations/paths, invalid UTF-8, and non-finite scores. | Backends additionally verify object relationships, range containment, quote bytes, commit fence, and visibility. |
| Code deterministic IDs | **Existing:** `pkg/code/code_test.go` covers stable typed IDs/source identity. | Accumulo storage preserves every ID; retries/restarts/compaction do not change artifacts. |
| Code ingestion retry | **Existing:** public `Ingest` contract requires applied then unchanged with identical artifacts. | Fault injection at every state transition converges to exactly one committed bundle. |
| Concurrent code append | Public ingest has no expected-base field. | Distinct deterministic ingest requests for one document serialize under `APPEND`, both remain exact, and later content epoch is latest; identical requests still collapse. |
| Concurrent shared external graph ID | No shared coverage. | Two ingests sharing one byte-identical external entity and LPART race at `ABSENT_OR_IDENTICAL`: one creates, one reuses, both return the same graph ID, each publishes its associations, and no duplicate generation/count appears. Different bytes or logical policy identity conflict. |
| Generic opaque IDs | No high-level adapter coverage. | NUL-containing/direct-Go IDs, UTF-8 gRPC IDs, long IDs, and hash collisions in physical buckets round-trip without normalization. |
| Immutable revision conflict | No public repository coverage. | Same revision/same digest is unchanged; same revision/different digest is `conflict`; original remains exact. |
| Revision claim key/non-disclosure | No shared coverage. | Same revision ID under different document IDs is independent; `(D,doc,rev)` collision is enforced; unauthorized writer cannot probe claim existence; aborted/poisoned partial claims recover deterministically. |
| Concurrent same revision | No shared coverage. | 32 identical writers yield one applied/rest unchanged; ambiguous acknowledgements reconcile. |
| Concurrent conflicting revision | No shared coverage. | One digest can own the revision claim; no mixed rows or indexes. |
| Distinct revision sequence / concurrent stale base | No shared coverage. | Sequential revisions naming the current base are all retained; concurrent revisions naming the same base yield one commit and one `conflict`; latest/as-of follows publication epoch. |
| Tombstone winner selection | No shared coverage. | Newest committed generation is selected including tombstones; latest read becomes `not_found`; earlier live generation is never resurrected by pre-filtering tombstones. |
| Tree integrity/order | Public values exist; no public repository suite. | Out-of-storage-order writes return preorder/sibling order; duplicate order, orphan, cycle, disconnection, and containment violations never publish. |
| Revision/as-of | Retrieval carries `AsOf`; high-level selection is undefined. | Before-first, exact boundary, between publications, after tombstone, equal source `CreatedAt`, and retained-history floor agree. |
| Section-only citation/source bytes | Public source-aware range, aggregate revision, and citation-quote validation exists; storage integrity is untested. | Full UTF-8 source bytes/chunks round-trip; byte offsets and UTF-8 boundaries agree; section-only and span citations reconstruct exact source slices after restart/compaction. |
| Citation/quote/evidence association | Public contextual quote validation exists; association storage is new. | IDs resolve to one revision, canonical associations join both scope dimensions, quote equals source interval, evidence/path visibility agrees, missing association cannot be inferred. |
| Superseded/deleted postings | No high-level coverage. | Postings for nonwinner revisions never affect counts, authorized ranking, refill, or top-K; winner tombstone removes the document. |
| Graph one-hop/multi-hop | Public graph validators and embedded both-direction neighborhood exist; durable generation semantics do not. | Forward/reverse parity, winner-including-tombstone selection, cycles, parallel edges, depth/degree/frontier caps, and bucket-count variations agree. |
| Source endpoint liveness | No shared coverage. | A tombstoned/hidden/retired/uncommitted seed or edge source removes the direct neighbor before degree/global limits, even when target and adjacency rows remain live. |
| Concurrent endpoint replacement | No shared coverage. | Two replacements from one base serialize; stale adjacency never matches canonical winner or consumes a limit; old/new endpoint tombstones and reverse rows agree. |
| Graph global ordering | No shared coverage. | Cross-seed, depth, path-representation rank, direction, source, target, type, edge, node, and path tuples produce byte-identical limited results across tablet/multi-scan order. |
| Directed path representability | Existing `graph.Path.Validate` requires directed adjacency. | Fixture `B <- A -> C`: incoming expansion from `B` may return `A`/edge `A->B` as neighborhood data but never path `[B,A]`; mixed walk `B<-A->C` never becomes a path. Incoming-only path-required requests return `unavailable`; retrieval emits only directed connected evidence paths. |
| Retrieval ordering | Public result/evidence total orders, finite scores, unique IDs, and TopK response validation exist; fusion scoring is undefined. | Exact score bits, result ID tie-break, evidence order, deduplication, TopK, and deterministic fusion agree. |
| Scope | Public normalization defines OR-within/AND-between association semantics and bounded opaque IDs; canonical association storage is absent. | Empty/document/node/both scopes use the normative association rule and cannot affect authorization. |
| Standalone tree/graph seeds | Public fixed-planner validation and embedded pre-scan rejection exist. | Scoped/association/hybrid seed sources are bounded; standalone empty-seed tree/graph returns deterministic `unavailable` before I/O in every adapter/transport. |
| Errors | **Existing:** `pkg/shoal/errors_test.go` and gRPC tests cover categories/mapping. | Missing, hidden, conflict, corrupt, overload, cancellation, deadline, history-pruned, and mode-unavailable cases match. |
| Safe retries | **Existing lower-level:** `accumulo/batch_writer_test.go` covers bounded safe retries and ambiguous terminal failures. | High-level retry never duplicates artifacts or epochs and never exposes staged rows. |
| Epoch allocation/frontier crash windows | The allocator package covers accepted/rejected/unknown reservation, terminal outcome, checkpoint, retirement, one-sided corruption, bounded batches, and concurrent unique allocation without live Accumulo. Entity-guard tests cover exact-row acquisition/finalization readback, mixed-state corruption, deterministic multi-row ordering/rollback, renewal, and takeover without live Accumulo. End-to-end ingestion/recovery-worker coverage is not yet present. | Extend the conformance harness through publication, worker restart, authority cutover/rollback, and checkpoint wait boundaries. |
| Partial writes/crash recovery | Guard-row accepted-before/after and contradictory partial-row outcomes are covered at the injected store seam. High-level TXN claims, manifest/materialization writes, commit-copy/outcome orchestration, and worker restart coverage remain absent. | Fault after each claim/manifest chunk/mutation/verification/LPART commit-copy/outcome boundary and prove convergence to committed or terminal. |
| Manifest chunking | No shared coverage. | Missing, duplicate, reordered, oversized, and digest-broken chunks fail before commit; large valid manifests keep TXN root bounded. |
| Mid-read failures | Public scanners can return values plus errors. | Accumulated partial values are never serialized; retry stays on the same snapshot. |
| Authorization | Accumulo scan authorizations exist; no Explorer bridge. | Public/empty/restricted grants, mixed scopes, hidden intermediates, counts, scores, explanations, caches, and logs show noninterference. |
| Authorized ranking projection | No shared coverage. | Adding/removing inaccessible documents, edges, terms, or vectors cannot change visible scores/order; fixed constants remain bit-identical. |
| Revocation | No Explorer coverage. | Policy-generation change invalidates in-flight work/caches; `AsOf` does not restore old authorization. |
| LPART/VDIGEST broadening | No shared coverage. | New COPYGEN/maps are inert before MAPGEN root; after root/label issue exactly the mapped `(COPYGEN,VDIGEST)` is selected; simultaneous old/new visible cells deduplicate by logical identity and never change counts/rank. |
| LPART/VDIGEST narrowing/crash/rollback | No shared coverage. | Narrowing revokes old issuance only after committed copies; crashes before/after root recover without gaps/double counts; rollback uses a newer MAPGEN mapped to a prior sealed COPYGEN/VDIGEST and never decrements generation. |
| Mixed visibility code ingestion | Public ingest allows document/graph artifacts; storage atomicity is undefined. | One TXN publishes all initial LPART commit copies atomically; each caller selects authorized VDIGEST copies; result waits for all LPARTs and retries return stable artifact IDs. |
| Mixed index generations | No shared coverage. | Every request/cursor pins one IGEN per family; activation crashes are safe; lexical/tree/out/in/association generations are never mixed. |
| Index rebuild publication race | No shared coverage. | Fault/concurrent content or policy-copy publication around build frontier `B`, delta journal/dual-write, seal, and activation epoch `A` yields either old or new complete IGEN with required policy-copy coverage; no missed delta, early activation, or mixed family generation. |
| Snapshot lease, retirement, and stale retry | No shared coverage. | Active/ambiguous leases block GC; history floor advances before deletion; crash leaves safe extra data; old token/guard/authority cannot resurrect compacted data. |
| Writer authority cutover/rollback | No shared coverage. | Backfill/dual-write/cutover/rollback verify durable authority plus **both** backend mirrors before routing. Stale direct embedded writer after cutover and stale direct Accumulo writer after rollback are rejected; ambiguous transitions remain write-closed. |
| Bounds | M0 public ID/metadata/document/graph/retrieval static bounds are shared; storage/manifest bounds remain future. | Every ID/metadata/source/cardinality/manifest/mutation/request bound has identical `invalid_argument`; runtime exhaustion is `unavailable`. |
| Deterministic serialization | Partial order/normalization coverage exists. | Canonical record bytes, checksums, and deterministic protobuf bytes match across adapter, transport, restart, tablet layout, and compaction. |
| Vector request | Enum exists; implementation is not established. | Initial adapters both return deterministic `unavailable`; future vector suites are separately gated. |

The suite runs before and after flush, restart, split changes, compaction,
cache-cold restart, tablet-server disruption, and a sustained fault/soak
workload. Exact operations require zero semantic mismatches. Authorization
mismatches are release blockers.

## 11. Incremental promotion from embedded to Accumulo

### 11.1 Prerequisites

- Expose and test native Accumulo conditional writes.
- Use the implemented shared canonical codec, scorer/analyzer versions, and
  conformance harness; implement the trusted auth resolver before promotion.
- Define the embedded extraction/change-journal boundary and historical
  publication sequence/time export.
- Bootstrap tables, visibility policy, splits, locality groups, iterator
  classes, retention, service accounts, dashboards, and a test environment.
- Prove source bytes are available for quote verification.
- Create a durable migration-authority record:

  `MigrationAuthorityV1{generation,primary,phase,holder,fence,journalWatermark}`.

  It lives in the durable migration journal/control store and is mirrored in
  **both** the embedded write guard and Accumulo allocator row. Every direct
  writer, replication writer, and router requires the durable generation/mode
  to equal both backend mirrors; a backend-local stale match is never enough.

### 11.2 Durable journal and shadow writes

Do not make application success depend on two synchronous stores. Introduce a
durable migration journal with:

`planned -> embedded_applied -> accumulo_applied -> verified`

Each record contains sequence, schema/codec/materializer versions,
idempotency token/TXN, expected base, exact source publication order/time,
LPARTs, `MAPGEN -> (COPYGEN,VDIGEST)` mappings, logical and physical copy
digests, operation kind, tombstone state, and writer-authority generation.

The journal is persisted before source application. Recovery can apply a
journaled operation to either backend idempotently. During the initial
promotion, authority mode is `EMBEDDED_PRIMARY`; Accumulo accepts only the
fenced migration-replica identity and is non-serving. A stale application
writer cannot bypass the journal because its authority generation is checked
by the embedded guard. Sequence gaps, authority mismatches, digest conflicts,
visibility drift, and lag page operators.

### 11.3 Historical epoch import protocol

1. Under writer authority generation `G`, capture journal high-watermark
   `H0` and a consistent export manifest of every committed publication at or
   below it, including tombstones, exact publication order, `visible_at`,
   IDs, source bytes, LPARTs, `MAPGEN -> (COPYGEN,VDIGEST)` mappings, and
   digests.
   Exported `visible_at` values MUST be nondecreasing in publication order;
   otherwise the source cannot implement the normative `AsOf` contract and
   migration is blocked. `H0` MUST end a complete equal-`visible_at` group;
   discovery of a later journal record with the same time requires recapturing
   `H0`.
2. Let the export contain `N` historical publications in source order.
   Assign target content epochs `1..N` exactly in that order. Initialize the
   target allocator's `next=N+1`, `importMaxEpoch=N`, signed
   `importPlanDigest`, history floor, retention generation, and mirrored
   authority `G` in one row mutation before target writes.
3. Post-`H0` writes continue only in the journal/embedded primary. They MUST
   NOT reserve target epochs yet; this prevents them from overtaking the
   historical band or exhausting the bounded active reservation window.
4. Import history in bounded epoch windows. For each fixed historical epoch,
   conditionally create an owner-bearing import reservation authorized by the
   allocator's matching plan digest/range, write/verify/publish the TXN,
   terminalize the outcome, and checkpoint contiguous progress.
5. Publications sharing the same source `visible_at` are imported in source
   order and receive one checkpoint at the group's maximum epoch. This
   preserves the source `AsOf` boundary without inventing timestamp order.
6. Preserve public IDs, expected bases, source bytes, LPARTs and their active
   `MAPGEN -> (COPYGEN,VDIGEST)` mappings, tombstones, logical/physical
   digests, and logical times. Migration wall time is never stored as content
   publication time.
7. Large sorted windows MAY use manager-authoritative bulk import after split
   and load-map verification, but their TXN/outcome/checkpoint protocol is
   unchanged.
8. After epoch `N` is terminal and checkpointed, replay journal records after
   `H0` strictly by journal publication sequence. They reserve epochs
   `N+1...` and retain original publication times/group boundaries.

If embedded storage cannot supply a high-level publication order consistent
with this protocol, migration is blocked rather than guessing from
`Revision.CreatedAt`, storage timestamps, or export order.

### 11.4 Verification and shadow reads

At fixed watermarks compare:

- missing/extra logical records;
- canonical digests and timestamps;
- epoch outcomes, checkpoint `visible_at` groups, history floor, and writer
  authority generation/mode/fence equality across durable authority,
  embedded guard, and Accumulo allocator;
- LPART stability, per-generation active VDIGEST mappings, logical/physical
  copy digests, and absence of duplicate logical results while old/new copies
  coexist;
- physical IGEN activations and pinned-family checks;
- tree reachability/order/ranges;
- graph endpoints and neighborhoods;
- citation and quote bytes;
- retrieval IDs, scores, evidence, explanations, and exact ordering;
- authorization/non-disclosure projections.

After two full clean passes separated by live traffic, mirror sampled
production reads to Accumulo with the same request, authorization, and
snapshot. Embedded responses continue serving. Comparison telemetry is
secured and records categories/digests, never restricted content.

### 11.5 Canaries and cutover

- Select canary domains/documents by stable hash, not request randomness.
- Start with internal/non-sensitive cohorts.
- Accumulo serves canary reads; embedded remains the immediate fallback.
- Writes continue under `EMBEDDED_PRIMARY`; target replication uses the
  journal and mirrored authority generation.
- Promotion gates include zero exact semantic/authorization mismatches,
  contiguous journal, zero unresolved ambiguous writes, acceptable frontier
  lag, and SLO compliance.
- Exercise an intentional rollback before expanding.
- Cutover is a fenced authority transition:
  1. close a write-admission barrier under generation `G`;
  2. drain the journal and require every target reservation through the final
     watermark terminal/checkpointed;
  3. CAS the durable authority to
     `{generation=G+1, primary=ACCUMULO, phase=CUTOVER_PENDING}`;
  4. write and read-verify `{G+1,ACCUMULO_PRIMARY}` in the **embedded guard**,
     which disables old embedded primary/direct writers;
  5. write and read-verify `{G+1,ACCUMULO_PRIMARY}` in the **Accumulo
     allocator**, which enables only the new target primary/fenced replica
     roles;
  6. re-read the durable authority and both mirrors and require exact
     generation/mode/fence equality;
  7. flip routing with required generation `G+1`;
  8. CAS the durable phase to `ACTIVE` and reopen writes only after routing
     and both mirrors agree.

An ambiguous CAS/mirror/routing response is resolved by reading the durable
authority plus **both** backend mirrors. Missing/mismatched mirrors are
repaired from the durable authority while the barrier remains closed. Neither
backend's local generation is accepted as authoritative. The safe
intermediate state is write-unavailable, never two primaries.

### 11.6 Rollback and dual-write hazards

After cutover, reverse-replicate writes to embedded under the current authority
generation until decommission. Rollback uses the same barrier/drain protocol:
CAS durable authority to `{G+2,EMBEDDED_PRIMARY,ROLLBACK_PENDING}`, then write
and verify that exact generation/mode in **both** mirrors. The Accumulo mirror
disables old target primary/direct writers; the embedded mirror enables only
the new embedded primary. Routing changes to `G+2` only after durable
authority and both mirrors agree; phase becomes `ACTIVE` and the barrier opens
last. Reusing or decrementing a generation is forbidden.

| Hazard | Required control |
|---|---|
| Embedded succeeds, Accumulo fails | Durable journal and replay; embedded remains authoritative before cutover. |
| Accumulo succeeds, acknowledgement is lost | TXN idempotency and commit readback. |
| Different timestamps/order | Carry the canonical logical publication mapping; never use migration wall time. |
| Mixed visibility / relabel | LPART, MAPGEN, COPYGEN, VDIGEST, logical digest, and physical-copy digest are part of manifests and parity checks. |
| Partial multi-table bundle | Final commit fence; readers ignore staging. |
| Index leads canonical data | Commit check and hydration validation. |
| Canonical data leads required index | No commit until manifest is complete. |
| Tombstone omitted | Tombstones are first-class journal/checksum records. |
| Retry-policy differences | High-level adapter owns retry classification. |
| Equal-score drift | Shared total order independent of scanner order. |
| Schema/version skew | Version journal and codec; reject unknown versions. |
| Rollback loses post-cutover writes | Reverse journal remains active and verified. |
| Shadow comparison leaks data | Secured digest/category telemetry only. |
| Stale/in-flight writer after route change | Monotonic durable authority generation plus checks against both backend mirrors; cutover fences embedded writers and rollback fences Accumulo writers before routing. |
| Authority update response lost | Read durable authority plus embedded and Accumulo mirrors; keep writes closed and repair mirrors from durable state until one exact generation/mode/fence exists everywhere. |

### 11.7 Decommissioning

After the agreed rollback window:

1. take a final signed logical manifest and journal watermark;
2. verify reverse replication, authority generation, and rollback obligations
   are closed;
3. disable reverse replication and shadow reads;
4. revoke migration credentials;
5. remove temporary routing flags/tooling;
6. retain audit/journal/manifests per policy;
7. delete embedded storage only through a separately approved, recoverable
   operation.

## 12. Implementation milestones

| Milestone | Dependencies | Deliverable | Exit criteria |
|---|---|---|---|
| M0 — Public contract adoption | Public API owners | `docs/explorer-public-contract.md` plus shared public validators/tests adopt UTF-8 byte offset, normalization, scope, mode-seed, ranking, error, future mutation-token/precondition, and traversal rules; winner/tombstone vocabulary remains additive because current values contain no such fields | Public-value semantics are shared; persistence/publication/authorization decisions remain explicitly deferred to later milestones |
| M1 — Canonical conformance foundation | M0 | Public-value harness, materializer/codec/checksums, deterministic serialization, source-byte fixtures, scorer/analyzer interfaces, fault/authority model | Embedded reference passes winner, citation, association, ordering, bounds, and error cases |
| M2 — Authorization foundation | M0–M1 | Trusted context resolver, partition/label codec, service accounts, authorized-projection ranking, non-disclosure/caching/logging | Embedded reference passes authorization/noninterference/revocation tests |
| M3 — Atomic coordination and recovery | M0–M2 | Native ConditionalWriter; bounded TXN/manifest chunks; allocator reservations/outcomes; allocator-row checkpoint mutations; entity guards/claims; LPART/VDIGEST policy-copy catalog/fences; index-generation catalog; snapshot leases; history floor/retirements; durable authority plus embedded/Accumulo mirrors; recovery/reconciliation workers | Fault/ambiguous-response injection covers LPART broadening/narrowing/crash/rollback with no duplicate counts, both authority mirrors, and index rebuild `B`/journal/seal/`A` races, in addition to claim/guard/allocation/commit/outcome/checkpoint/lease/floor recovery |
| M4 — Schema/bootstrap and document adapter | M1–M3 | Tables/properties/splits, documents/revisions/source chunks/sections/spans, outline, citation/quote hydration, associations, tombstones | Tombstone winner, superseded revision, section-only citation, AsOf, GC-stale-writer, restart/compaction parity pass |
| M5 — Graph adapter | M1–M4 | Canonical nodes/edges, `ABSENT_OR_IDENTICAL` code-owned identity reuse, IGEN forward/reverse adjacency, expected-base endpoint replacement, association joins, bounded neighborhood traversal, directed evidence paths only | Concurrent shared-external reuse, both-endpoint liveness (including source tombstones), replacement, mixed IGEN, global ordering, `B<-A->C`, incoming-only no-path, auth, and supernode tests pass |
| M6 — Lexical/tree/hybrid retrieval | M1–M5 | IGEN postings/tree indexes, winner filtering, bounded seed planner, approved iterators, shared scoring/fusion, budgets | Superseded/deleted postings, standalone mode errors, mixed scopes, authorized ranking, deterministic order, and load targets pass; vector remains explicit `unavailable` |
| M7 — Migration/import tooling | M1–M6 | Durable journal/authority, embedded and Accumulo authority mirrors, historical epoch-band import, bulk load, checksums, shadow comparator, dashboards | Exact publication order/time preserved; both mirrors verified before every route transition; stale direct writers against each backend fail; ambiguous cutover/rollback stays closed; two clean watermarks and zero auth mismatch |
| M8 — Canary and cutover | M7 | Generation-bound routing, reverse replication, fenced cutover/rollback, SLO gates | Exactly one writer authority throughout; all promotion gates green through rollback window |
| M9 — Vector evaluation (**deferred**) | M0, M6–M8 plus separate vector contract | Accumulo-native exact/ANN prototype and conformance/recall/security suite | Explicit accuracy/freshness semantics, no unauthorized displacement, and operational approval |

The M3 control slice is implemented in
`pkg/explorer/coordination/control`: authoritative snapshot leases, atomic
allocator-row history-floor/head updates, retirement decisions, durable
writer authority synchronized with the allocator head, backend observations,
and a fail-closed routing/cutover barrier. The package uses injected exact
read/prefix-scan/row-CAS, UTC clocks, pin/object/migration verifiers, and an
optional trusted exact deleter. Catalog and entity-guard adapters consume the
agreed durable state; allocator's Accumulo store provides matching bounded
prefix/seek scans. Physical object deletion, background scheduling/status
indexes, and live-cluster fault tests remain later integration work.

The M3 transaction slice is implemented in
`pkg/explorer/coordination/transaction` and
`pkg/explorer/coordination/recovery`. Transaction roots and leases use exact
row reads/CAS and monotonic generations; manifest rows are immutable; initial
LPART commit copies and the committed root are one bounded row mutation.
Recovery rechecks roots, manifests, guards, reservations, outcomes, and
frontiers rather than trusting queue hints. The reusable
`internal/explorerconformance` checks the durable in-memory fault model and
trusted Accumulo physical-cell mapping. Live Accumulo integration, a concrete
Explorer document/graph physical sink, and combined policy/index rebuild
fault campaigns remain explicitly deferred.

M1 now has concrete package surfaces in `pkg/explorer/canonical`,
`pkg/explorer/codematerializer`, `pkg/retrieval/ranking.go`, and
`internal/explorerconformance`. The embedded reference runs the harness with
exact source-time controls and restart coverage. Later milestones extend the
same harness for authorization, fault injection, publication recovery, and
Accumulo direct/loopback adapters rather than redefining these logical values.

M2 now has concrete package surfaces in `pkg/explorer/auth`,
`pkg/explorer/authorized`, and the authorization suite in
`internal/explorerconformance`. The embedded reference proves
non-disclosing direct reads, authorized-projection ranking, hidden-intermediary
graph behavior, generation revocation/reissue, backend-response validation,
and restart with a reused in-process policy catalog. M3/M4 must replace that
catalog with atomic durable policy/publication state before cross-process or
Accumulo restart guarantees are claimed.

The dependency order is intentional. M3 supplies the reusable transaction,
recovery, retirement, frontier, manifest, and writer-authority machinery
before a document/graph adapter may claim restart or fault tolerance.
Migration cannot begin before the deferred concrete adapter and live
integration paths pass.

## 13. Explicit non-goals

- Replacing Accumulo internals, metadata authority, tablet routing, scans,
  mutations, compaction, visibility, RFiles, or bulk import with gRPC.
- Exposing rows, cells, CFs, iterators, scanners, or protobuf internals through
  public document/graph APIs.
- Breaking or reinterpreting existing public document, graph, citation,
  evidence, retrieval, ingestion, or error values.
- Regenerating caller-supplied opaque public IDs. The versioned code
  materializer generates only new artifact IDs under Section 6.9.
- Claiming cross-row ACID transactions; the publication fence provides
  logical bundle atomicity.
- Blind retry of ambiguous writes.
- Property/section-level ACLs inside one public document revision.
- Cross-domain graph traversal.
- An external vector database, service-local authoritative vector index, or
  initial vector implementation.
- Redesigning analyzers, scoring quality, hybrid weights, parsers,
  summarization, OCR, or embedding generation.
- Retrofitting pagination into existing unary `Retrieve`.
- Adding a replacement public graph-walk/path type. Incoming/mixed expansion
  remains neighborhood-only when it cannot satisfy `graph.Path.Validate`.
- Aggressive deletion of citation-bearing history.
- Decommissioning embedded storage before a verified rollback window.

### 13.1 Embedded transaction runtime

`internal/explorercoord.Open` is the production in-process composition for the
embedded engine. Callers provide a domain, a recovery owner, a current
embedded-primary authority generation/fence, and the physical tables that the
intent may address. `Runtime.Publish` accepts a bounded logical intent
containing cells, expected entity states, and result identities. It:

1. canonicalizes and stores the intent absent-or-identical;
2. derives the TXN from domain, operation, and token;
3. invokes the existing allocator/guard/transaction protocol;
4. writes physical cells in deterministic table/row order;
5. independently reads exact physical versions before publication;
6. marks completion only after outcome, checkpoint, and guard finalization.

Recovery pages the canonical live-pending transaction index and then
authoritatively loads each referenced intent and inspects its transaction root.
The cursor is derived from ordered pending transaction IDs, never
`Snapshot.Frontier`; repeated recovery is idempotent. An unavailable or
canceled call after intent persistence is an indeterminate publication, never
an implicit rollback.

The embedded implementation maintains a row-atomic pending marker beside each
new durable intent and a canonical, generation-fenced index containing only
live pending transaction IDs. Index registration precedes intent persistence,
so a crash can create only a safe false positive that recovery removes; it
cannot hide a durable pending intent. Completion or a non-committed terminal
state removes the transaction from the index. Startup and steady-state pending
checks therefore inspect only outstanding work; completed corpus history does
not consume the recovery page budget or make each ingest scan old tombstones.
A record publication also stores its stable record-key binding in the canonical
intent, allowing recovery to restore or verify an ambiguous alias before any
physical publication. A non-committed terminal attempt is released and its TXN
is folded into the next deterministic retry token before the record key is
rebound. A retry of an active or committed Explorer document publication
reloads the exact encoded attempted record, including its original publication
time, sequence, and expected entity head, instead of regenerating transient
values under the same TXN. Before the first transactional Explorer open, the
runtime records bounded, per-row grandfather markers for existing legacy
document rows and then seals migration with a domain-bound marker in the
Explorer table. Once sealed, a physical document without a transaction binding
is readable only when its exact legacy marker exists; recreating coordination
metadata cannot reclassify later transactional residue as legacy data.

The transaction runtime accepts only `localwal.SyncFull`. `SyncNormal` and
`SyncOff` are rejected before the engine is opened because coordination and
physical tables have independent WALs; allowing a commit/checkpoint WAL to
survive while a physical WAL is lost would violate the publication fence.

`Runtime.ScanCommitted` is the read-only physical event/registry seam. It pins
one allocator frontier, scans only a configured table and exact
family/qualifier/visibility under a bounded row prefix, and returns the newest
committed epoch-stamped version per row. Admission requires the immutable epoch
outcome, durable intent completion, committed root, frontier inclusion, and an
exact matching cell in the reconstructed manifest. A newer nonterminal,
aborted, or poisoned version cannot hide an older committed version.
`Runtime.Committed` exposes the same completion proof for a known TXN and
logical digest. Raw engine scans and a second engine open remain unsupported.

`Runtime.PruneCommitted` is the bounded retention seam for product-local
streams such as fleet events. Callers page at most 256 exact
`CommittedCell` values, identify each owning live entity guard, and provide one
domain-local readable-floor checkpoint. The runtime independently rehydrates
every committed proof, commits domain-separated physical tombstones, retires
the owning guards, and advances the checkpoint in one normal transaction.
Tombstones remain visible at older pinned frontiers and hide the value only at
and after the prune epoch. The checkpoint is an ordinary committed cell and
guard chosen by the product adapter; pruning never advances the shared
allocator `HistoryFloor`. Replays with the same operation/token are
idempotent, divergent targets conflict, and unavailable outcomes remain
indeterminate and recoverable through the existing pending-intent worker.

`internal/explorercoord.OpenExplorer` shares the same engine handle with
`pkg/explorer`, and `cmd/shoal-explore-web` uses it for the embedded backend.
Immutable document-revision records are published with a stable document-level
guard and optimistic expected epoch/digest. Concurrent divergent revisions
that start from one head cannot both commit. Explorer load consults durable
intent completion and the committed transaction root before decoding a
transactional record, so physical-stage residue from a nonterminal, aborted,
or poisoned transaction is not observable. A configured document envelope
larger than the 1 MiB manifest-cell limit fails closed before physical write;
only the unconfigured legacy open path preserves direct-write compatibility.
Snapshot-anchor, change-cursor, edge, interaction, fold, extraction, and
ontology-proposal records still use the established embedded path. Fleet
registry/dispatch code may use `Runtime.Publish` for bounded multi-row bundles,
but those domain routes are not implemented by this adapter change.

The v1 embedded runtime supports many concurrent agents in one process. It
holds a nonblocking operating-system file lock for the data directory and
rejects a second runtime open. Authority is injected and must exactly match the
durable allocator head. This is not a claim that multiple processes can share
one embedded engine directory; cross-process authority rotation and routing
remain separate work.

## 14. Risks, open questions, and decision points

These items affect feasibility, performance, or public adoption. They do not
relax the normative behavior above.

| Item | Why it matters / required decision |
|---|---|
| Native Accumulo conditional writer | The M3 slices expose a public exact-row API over Accumulo's native conditional-update RPC, the bounded coordination vocabulary, a production allocator client with allocator-head fencing, row-atomic reservations/checkpoints/retirement, immutable outcomes, and a production entity-guard client with canonical acquisition ordering, APPEND/ABSENT_OR_IDENTICAL decisions inside row CAS, renewal/takeover fencing, publication-authority-gated finalization, retention checks, and authoritative ambiguous-response readback. The embedded Shoal-engine runtime now composes those pieces with durable intents and recovery. A live Accumulo physical sink, full product materializers, durable catalogs/mirrors, deployment routing, and a live-cluster fault campaign remain incomplete. These slices do not make full M3 complete. |
| Pre-GA allocator wire compatibility | `AllocatorHeadV1` and `ReservationV1` currently include explicit monotonic record generations. This branch has not shipped, so their V1 fixtures were updated in place and no migration from the earlier branch-only bytes is provided. |
| Public contract adoption | M0 adopts byte-offset, normalization, scope, error, and traversal rules in `docs/explorer-public-contract.md`. Winner/tombstone persistence, publication frontiers, authorization, and mutation shapes remain later additive work and must not be inferred from the value helpers. |
| Embedded historical export | Migration requires exact publication order/time and canonical source bytes. If embedded storage cannot export them, promotion is blocked rather than approximated. |
| Shared ranking implementation | Exact analyzer, scorer, fusion, and authorized-projection statistics must be shared or proven byte-equivalent; storage-local approximations are not acceptable. |
| Authorization vocabulary | Domain/source/policy authority, principal resolver, service ceilings, and migration mapping need ownership and operational approval. |
| LPART ownership and policy-copy scale | LPART must remain stable across relabel and shared graph reuse, while VDIGEST tracks physical visibility. Policy-copy manifests/maps may be large; benchmark generation activation and retain old copies for pinned leases without weakening exact selection. |
| Dual backend authority mirrors | Cutover safety depends on durable authority plus embedded and Accumulo mirrors. The barrier must remain durable/write-closed while any mirror differs; operational tooling must never “pick” a backend-local value. |
| Uniform revision LPART | The design requires one logical policy identity and one selected physical copy per policy generation for a complete document revision. Mixed ACLs require changed public semantics or split content. |
| Allocator-row throughput | Row-atomic allocation/reservation is deliberately serialized per domain. Benchmark the active-window rate and split authorization domains rather than weakening frontier correctness. |
| Frontier-history row growth | Latest and `AsOf` checkpoints share the allocator row for atomicity. Batch frontier advances and prune checkpoint cells only below the authoritative history floor; indefinite history requires capacity planning. |
| Long epoch reservation | Large final writes can block frontier advancement. Prepare manifests/RFiles before reservation, enforce bounded active windows/recovery deadlines, and benchmark bulk paths. |
| Materializer product mapping | Section 6.9 is deterministic, but product owners must confirm that syntax-to-section/span and artifact-ref meanings are the intended public Explorer presentation. |
| Shared graph logical policy identity | `ABSENT_OR_IDENTICAL` reuse requires byte-identical graph content and the same LPART/logical policy identity. Ingests with incompatible policies intentionally conflict rather than broadening visibility. |
| Association production | Parsers/ingesters must provide enough deterministic source-to-graph relationships. Missing associations reduce eligible mixed-scope/evidence results and cannot be inferred. |
| Incoming/mixed path limitation | Current `graph.Path` is directed. Incoming/mixed traversal can return neighborhood nodes/edges but not rooted paths; product UX must tolerate omitted paths or deterministic `unavailable` for path-required shapes. |
| Historical retention versus citations | Default is indefinite published evidence. A deployment choosing finite retention must publish its history floor/SLA and accept `not_found` for older citations/cursors. |
| Iterator deployment compatibility | Required iterator classes, priorities, options, and table properties must be verified on target Accumulo versions and tested so visibility precedes aggregation. |
| Stable cursor API | Only additive APIs may use cursors. Signature/encryption and maximum lifetime remain API/deployment decisions; expiry outcome and snapshot binding are fixed here. |
| Embedded baseline availability | No public high-level document/graph store interface exists today. The conformance harness and embedded reference must be created before Accumulo implementation can prove “no semantic change.” |
| Vector semantics | Exactness, approximation, precision, metric, freshness, explanation, and recall are unresolved. Vector remains unavailable until separately approved. |

## Public contract references

- `pkg/shoal/types.go` — `ID`, `Metadata`, `Score`
- `pkg/shoal/errors.go` — public error categories and wrapping
- `pkg/document/document.go` — ranges, immutable revisions, documents,
  sections, spans, citations
- `pkg/graph/graph.go` — nodes, directed edges, ordered path validation
- `pkg/retrieval/retrieval.go` — modes, scope, request validation, evidence,
  results, `Retriever`
- `pkg/retrieval/grpc/retrievalgrpc.go` — context delegation and error mapping
- `pkg/retrieval/grpc/conversion.go` — public/protobuf conversion and slice
  order
- `pkg/retrieval/grpc/validation.go` — structural/UTF-8/finite-score response
  validation
- `pkg/code/id.go`, `pkg/code/source.go`, `pkg/code/model.go`,
  `pkg/code/parse.go`, `pkg/code/ingest.go` — deterministic code identities,
  immutable sources, parser-neutral model/ranges, and idempotent ingestion
- `proto/knowledge.proto` — current unary retrieval wire contract
- `proto/embed.proto` — public embedded conditional-write and lower-level
  as-of storage contracts
- `accumulo/mutation.go`, `accumulo/batch_writer.go` — row mutations,
  durability, safe retries, and ambiguous write outcomes
- `accumulo/scanner.go`, `accumulo/batch_scanner.go`,
  `accumulo/scan_stream.go` — scanners, authorizations, iterators, streaming,
  order, and partial-error behavior
- `accumulo/key.go` — Accumulo key ordering
- `accumulo/authorizations.go`, `accumulo/column_visibility.go`,
  `accumulo/security.go` — authorization and visibility primitives
- `accumulo/table_admin.go`, `accumulo/table_properties.go`,
  `accumulo/table_add_splits.go`, `accumulo/table_bulk_import.go`,
  `accumulo/table_bulk_import_fate.go` — table lifecycle, properties, splits,
  and manager-authoritative bulk import
- `docs/ai-knowledge-graph.md` — public transport/storage boundary
