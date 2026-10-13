# Shoal as the Sentrius knowledge/decision backend

Whether Sentrius should implement `KnowledgeGraphPort` against Shoal. Written
against Shoal at `test/disclosure-graph-surface` and Sentrius at its current
checkout; every claim cites code I read, and where I could not verify something
I say so.

`KnowledgeGraphPort` has exactly one production implementation,
`UnavailableKnowledgeGraph`
(`dataplane/.../core/services/documents/UnavailableKnowledgeGraph.java:29`),
which returns `false`, `null` and empty for everything and is bound by
`@ConditionalOnMissingBean`
(`dataplane/.../knowledgegraph/autoconfigure/KnowledgeGraphAutoConfiguration.java:38`).
Every graph-derived feature — behavioral routines, DRI lifecycle, agent
forensics — is unreachable at runtime today. This is not a migration; it is
choosing a first backend, with no incumbent to match and no data to move. And
the interface was shaped by a document database Sentrius no longer has:
`createNode` / `deleteNode` / `updateNodeProperties` / `getAllNodes` /
`getStatistics` are CRUD, and Shoal is not a CRUD store. About half the
interface maps well, a quarter awkwardly, and a quarter not at all.

## 1. Method-by-method mapping

**`storeDocumentAsNode(Document, username)`** — clean, onto
`authorized.Client.Ingest` (`pkg/explorer/authorized/client.go:239`) with
`explorer.Source{URI, Title, MediaType, Content, Metadata}`
(`pkg/explorer/model.go:44`); `Document.content` is a single TEXT column
(`Document.java:43-44`), exactly what `Source.Content` wants. Two frictions:
Shoal derives its own document/revision/section/span IDs, so the Sentrius `Long
id` rides in `Metadata` and a mapping table has to exist, and the Explorer alpha
accepts UTF-8 Markdown and plain text only (README:167-169). `username` is
dropped, here and everywhere: identity comes from the bound `auth.Decision`,
never a parameter (`chat_service.go:52-57`).

**`createNode(KnowledgeGraphNode, username)`** — no clean mapping. The authorized
client has no node-creation method; nodes arrive from document ingest,
`ExtractDocument` (`pkg/explorer/authorized/extraction.go:47`), or
`MaterializeGraph` (`pkg/explorer/authorized/graph_materialization.go:29`). The
last is closest and a different shape: a batch, immutable publication of
`GraphNodeSpec{Key, Kind, Properties}` (`pkg/explorer/graph_materialization.go:29`)
whose identity Shoal derives and returns as `GraphIdentity{Key, ID}` (`:56`). A
caller cannot assert the node ID `"session:1234"` that
`KnowledgeGraphIngestionService.java:162` builds today, and of
`KnowledgeGraphNode`'s fields only `Kind` and `Properties` survive
(`KnowledgeGraphNode.java:27-90`) — `markings` has nowhere to go (§3).

**`createRelationship(from, to, type, weight, username)`** — the near-exact
match, onto `graph.Edge{ID, From, To, Type, Weight, Properties}`
(`pkg/graph/graph.go:69-76`) via `authorized.Client.Connect`
(`pkg/explorer/authorized/graph.go:61`). Weight is first-class.

Two conditions current callers do not satisfy: `Connect` requires both endpoints
to be already-registered, already-authorized nodes (`authorizedNode`,
`graph.go:399-426`) whose rules share an authorization domain
(`graph.go:90-93`), and the caller supplies the edge ID, which must be in the
caller's own namespace (`Client.ApplicationEdgeID`, #699). Today
`BehavioralPatternAgent.java:225-234` creates a routine node and its edges in one
pass; against Shoal that becomes materialize-then-connect, or one
`MaterializeGraph` batch — whose `GraphRelationSpec`
(`graph_materialization.go:35-41`) has **no** `Weight` field, forcing weight into
`Properties`. That asymmetry is worth fixing on the Shoal side first.

**`findSimilarDocuments(documentId, username, limit)`** — no mapping.
`retrieval.Request` requires nonempty `Text`
(`pkg/retrieval/retrieval.go:116-118`), and `Scope.DocumentIDs`
(`retrieval.go:64-68`) narrows candidates rather than seeding a similarity
query — there is no "documents like this one" entry point in either package.
Sentrius would have to fetch the text and issue it as a query, a different
operation with different results, or Shoal needs a new API.

**`executeQuery(...)`** — splits by `QueryType`
(`KnowledgeGraphQueryRequest.java:67-73`):

- `SEARCH` → `Retrieve` (`pkg/explorer/authorized/retrieve.go:33`,
  `POST /api/v1/retrieve`). Strictly better than the status quo: results carry
  revision-pinned span citations (`retrieval.Evidence`, `retrieval.go:204`)
  rather than whole documents.
- `NEIGHBORS` / `TRAVERSE` / `SUBGRAPH` → `Neighborhood`
  (`pkg/explorer/authorized/graph.go:132`) or `BoundedNeighborhood`
  (`bounded.go:255`). `NeighborhoodRequest{NodeIDs, Depth, EdgeTypes}`
  (`pkg/explorer/model.go:103-107`) covers `startNodeId`, `maxDepth` and
  `relationshipTypes` but **not** `nodeTypes` — there is no node kind filter —
  and `limit` maps only to `BoundedNeighborhoodRequest.MaxNodes`.
- `PATH` → `POST /api/v1/path` (`pkg/explorer/webapi/http.go:295`), and
  `getRelationshipsForNodeIds` → `Neighborhood` at depth 1. Both clean.
- `customQuery` (`KnowledgeGraphQueryRequest.java:65`) → nothing. ShoalQL sits
  below the authorized layer with no authorized passthrough; an adapter should
  reject it rather than translate it.

`KnowledgeGraphQueryResponse.metadata` is a `String`
(`KnowledgeGraphQueryResponse.java:44`) and cannot carry an explanation,
snapshot ID or citation set; it has to become structured or the interesting half
of every response is discarded at the boundary.

**`deleteNode` and `updateNodeProperties`** — no mapping, and no clever adapter
closes it. There is no delete or in-place update on the authorized client;
revisions are immutable and content-addressed, and `Suppressed`
(`pkg/explorer/authorized/documents.go:116`) means authorization-withheld, not
removed. This collides with Sentrius directly: `Document.version` is a counter
incremented in `@PreUpdate` (`Document.java:93-95, 119-123`) with no history
table — Sentrius overwrites, Shoal accumulates. An adapter returns `false`,
which the contract permits (`KnowledgeGraphPort.java:20-24`), leaving every
caller expecting mutation silently a no-op.

**`getAllNodes(limit)`** — no mapping. `Documents`
(`pkg/explorer/authorized/documents.go:33`) lists authorized documents and
neighborhood queries require seeds; there is no whole-graph enumeration, by
design, since disclosure depends on candidate sets being authorization-scoped
before ranking.

**`isAvailable()` and `getStatistics()`** — no liveness or statistics API.
`BoundedAvailable`/`VectorAvailable` (`pkg/explorer/authorized/bounded.go:61,66`)
report capability, not liveness; `Snapshot` (`bounded.go:42`) returns
`{ID, AsOf, Frontier}` (`pkg/explorer/model.go:166`) and `Changes`
(`changes.go:102`) a change feed, but nothing gives a node count. Probe
`GET /api/v1/meta` (`pkg/explorer/webapi/http.go:199`) for the first and fill the
second with snapshot metadata — it is a UI contract, not a capability one.

**`answerQuestion(question, username)`** — the strongest mapping in the
interface and the reason to do this at all. `ChatService.Ask`
(`pkg/explorer/webapi/chat_service.go:155`, `POST /api/v1/ask`) returns a
`CitationEnvelope` (`pkg/explorer/webapi/citation_response.go:57-82`) carrying
verification status, snapshot pin, authorization fingerprint, embedding-space
identity and retrieved/cited source IDs. The current return type
`Map<String, Object>` holds `{question, answer, error}`
(`UnavailableKnowledgeGraph.java:117-124`) and would discard all of it: keep that
signature and Sentrius has bought the enforcement and thrown away the evidence.

## 2. Integration mechanism

Java to Go is out-of-process. Four surfaces exist; three are wrong.

**`shoal-embed` gRPC** (`proto/embed.proto:26`, `internal/embedpb`) is the
storage engine — `CreateTable`, `Write`, `Scan`, `Compact`, `Status` — with no
document, graph, retrieval or authorization semantics, and its only security
control is optional TLS/mTLS on the listener
(`cmd/shoal-embed/main.go:394-414`): no per-request principal, no
`auth.Decision`. Rejected, as is **`capi/`**, the Accumulo-client C ABI
(`capi/include/shoal_types.h:257-298`) — JNI into it gives a KV client, not a
knowledge API.

**`proto/knowledge.proto:27` `KnowledgeRetrieval`** has the right shape —
`Citation{document_id, revision_id, section_id, span_id, SourceRange}`, modes,
explanations — and a Go server adapter at
`pkg/retrieval/grpc/retrievalgrpc.go:47`. Two blockers. Nothing registers it:
grep for `retrievalgrpc` outside its own package returns no callers, so no
binary serves it. And the server wraps a bare `retrieval.Retriever` with no
decision plumbing (`retrievalgrpc.go:41-44`) — the same reason `-backend remote`
is disabled (README:243-244). Using it means building the authorization carry
first: a Shoal design decision, not a Sentrius integration task.

**The authenticated web surface on `shoal-explore-web` is the only one that
carries a principal end to end**, and it is the recommendation: `/api/v1/ingest`,
`/documents`, `/document`, `/retrieve`, `/neighborhood`, `/path`, `/changes`,
`/graph/materialize`, `/extract` (`pkg/explorer/webapi/http.go:283-296`),
`/api/v1/ask` (`chat_http.go:41`), `/api/v1/provenance*`
(`provenance_http.go:38-41`) and the fleet routes (§6). The MCP Streamable-HTTP
handler mounts inside the same `webapi.Handler` and inherits its authentication
(`pkg/explorer/mcp/http.go:52-56`), so MCP and REST are one trust boundary.

Costs, plainly: JSON over HTTP rather than a typed binding, so Sentrius writes
and maintains a Java client; opaque Shoal IDs arrive base64-encoded
(README:203-205) and need a mapping table against Postgres row IDs; and
`shoal-explore-web` becomes a required deployment component with its own corpus
and lifecycle. The stdio MCP server is not a candidate — identity there comes
from launcher configuration (README:261-263).

## 3. Authorization composition

Shoal takes one `auth.Decision` per request — subject, actor, client, delegation
chain, authorization domain, allowed operations, permitted source and policy IDs,
policy generation, expiry, request and correlation IDs, audit purpose, optional
service role and ontology (`pkg/explorer/auth/decision.go:32-69`) — minted at the
transport, with the authorized client the only enforcement point
(`pkg/explorer/auth/resolver.go:98-118`).

**Authentication composes cleanly.** Keycloak is OIDC, and Shoal's OIDC
authenticator validates a token and mints a decision
(`cmd/shoal-explore-web/oidc.go:776-865`); issuer, JWKS, audience and claim
names are deployment configuration.

**Authorization does not.** Three concrete mismatches.

*Role mapping is three buckets.* `oidcAuthenticator.authority`
(`oidc.go:869-908`) tests the authorization claim against fleet, contributor and
reader value sets, unions fixed operation lists, then grants a single static
source and policy ID (`oidc.go:899-903`). The ingest-side selector is
`NewStaticPolicySelector(workspaceSourceID, workspaceGrantPolicyID)`
(`cmd/shoal-explore-web/main.go:1283`, selector at
`pkg/explorer/authorized/selector.go:79-107`), which ignores the source
entirely, so every document in the workspace carries the same grant. Sentrius's
per-document `markings` cannot be expressed through this path at all; supporting
it means a Sentrius-specific `PolicySelector`, an in-process Go hook, so a fork
or an upstream extension point rather than configuration.

*Shoal's object-side policy has no disjunction.* `auth.Policy` is domain ∧
source ∧ grant-policy (∧ service role) and `DecodePolicy` rejects `|`, `(`, `)`
and whitespace outright (`pkg/explorer/auth/policy.go:172-186`); `AccessRule` is
a conjunction of policies (`pkg/explorer/authorized/rule.go:34-42`) whose
`Authorize` requires every component to pass (`rule.go:99-122`). Sentrius
`Document.markings` is a full Accumulo visibility expression including
`"(SENSITIVE&FINANCE)|(HR&MANAGER)"` (`Document.java:55-71`), evaluated with
`org.apache.accumulo.access.AccessEvaluator`
(`DocumentAccessControlService.java:72`). A disjunctive marking has no
single-object representation in Shoal, and I found no disjunctive form anywhere
in the auth packages to build on.

*Sentrius's ABAC engine fails open; Shoal fails closed.*
`PolicyEvaluator.evaluate` defaults `allowOnNoPolicies = true`
(`PolicyEvaluator.java:43-64`) and, in PERMISSIVE mode, converts an evaluation
exception into ALLOW (`PolicyEvaluator.java:290-298`). If Sentrius mints the
`auth.Decision`, that fail-open becomes the input to everything Shoal enforces.
Shoal's guarantees are exactly as good as the decision handed to it; this is the
specific way they would be undermined.

**ZTAT does not belong in the decision.** A ZTAT approval authorizes
`(user, host, exact command hash)` (`ZeroTrustAccessTokenRequest.java:29`, hash
from `ZTATUtils.java:8`) — an action, not a read, and no read operation it gates
exists. Its home is `Decision.AuditPurpose` (`decision.go:45`),
`Decision.CorrelationID`, and the fleet `RequestContext.ReasonCode` /
`ReasonDetail` (§6). It should not become a Shoal operation or grant.

## 4. The provenance conflict

**What Sentrius has.** `ProvenanceEvent`
(`provenance-core/.../provenance/ProvenanceEvent.java:25-38`): `eventId`,
`sessionId`, `actor`, `triggeringUser`, `eventType`, `input`, `outputSummary`,
`sourceDocs` (a bare `List<String>` of document IDs), `ztatTokenId`,
`timestamp`. No hash, no signature, no parent pointer, no content addressing;
`eventId` is a random UUID assigned at the emission site
(`AccessControlAspect.java:295`). The agent submission endpoint validates the
Keycloak JWT and then forwards the client-supplied body to Kafka verbatim
without binding `actor` to the authenticated identity
(`AgentApiController.java:245-272`), and the Neo4j ingestor then drops `input`,
`sessionId` and `ztatTokenId` (`Neo4jProvenanceIngestor.java:59-77`). There is
no citation, evidence, claim or span concept anywhere in Sentrius:
`sourceDocs` has no offsets, no quoted text, no hash of the cited content, and
no link to the part of `outputSummary` it supports.

**What Shoal has.** `interaction.Session`
(`pkg/interaction/interaction.go:356-399`): snapshot ID and `as_of`,
authorization fingerprint and expiry, the exact authorized operation, ontology
and embedding-space pins, model/prompt/tool-policy provenance, query digest, and
seed/cited evidence as `EvidenceReference`s carrying `AnchorID` and `Citation`
(`pkg/interaction/evidence.go:48-55`). It never carries the question, prompt,
answer or quote text, and actor and subject come from the trusted decision, not
the request body. On write the document anchor is recomputed from the resolved
quote and the record rejected if it does not match
(`pkg/explorer/authorized/interaction_evidence.go:114-118`) — the property
Sentrius's record cannot have, having nothing to recompute from.

**Why two trails is the worst outcome.** They would disagree in a specific
direction: Sentrius's trail is the forgeable one, and it lives in Kafka and Neo4j
where the rest of the audit already is, so an investigator reaches for it first.
An easy-to-find weak record beats a hard-to-find strong one.

*A — run both, unreconciled.* Cheapest to build, worst to own: two answers to
"what did the agent rely on", no arbitration rule. Reject.

*B — Shoal authoritative for the knowledge plane; Sentrius provenance narrowed
to external effects.* Sentrius stops emitting the `KNOWLEDGE_*` event types and
keeps `COMMAND_EXECUTED`, `ENDPOINT_ACCESS`, `POLICY_EVALUATION` and session
lifecycle — work whose effect lands outside Shoal. Shoal events reach Kafka only
as a derived projection carrying the Shoal session ID, marked non-authoritative,
never re-ingested as source of truth. The bridge exists, authorized and
resumable: `POST /api/v1/fleet/events/subscriptions/{subscription}/pull`
(`pkg/explorer/webapi/fleet_events.go:115`), with durable cursors and explicit
resync (`pkg/explorer/fleetevents/types.go:59-60`). It is pull-based — Shoal has
no Kafka producer and no webhook — so Sentrius writes the pump.

*C — Sentrius authoritative, Shoal recording suppressed.* Not available.
Recording is not optional on the grounded path: `AskProvider` returns "only
verified, durably recorded reasoning responses" (`chat_service.go:60-63`).
Suppressing it means not using the feature that justifies the integration.

**Recommend B**, with one non-optional precondition: bind `actor` to the
authenticated principal at `/agent/provenance/submit` first. Otherwise
Shoal-derived events inherit a forgeable envelope on the way out and the bridge
manufactures exactly the disagreement it exists to prevent.

## 5. What Sentrius retires, what it keeps

**Qdrant — delete, but claim no credit.** `QdrantMemoryStore`
(`integration-proxy/.../services/QdrantMemoryStore.java:13`) has no bean
annotation, no caller and no client dependency in any `pom.xml`, and the chart
ships it disabled (`sentrius-chart/values.yaml:222-230`). Removing dead code is
housekeeping, not a consequence of adopting Shoal.

**Neo4j — narrows, does not disappear.** Used only by `provenance-ingestor`
(`Neo4jProvenanceIngestor.java:21`; the only module with a real dependency, at
`provenance-ingestor/pom.xml:50-51`), never as a `KnowledgeGraphPort` backend.
Under option B its role shrinks to external-effect events; whether that
justifies a second graph database is a judgement call, and my read is that the
remainder is a flat event log Postgres serves adequately.

**pgvector — keep, split by purpose.** Two tables hold `vector(1536)`:
`agent_memory.embedding`
(`api/src/main/resources/db/migration/V22__add_vector_support.sql:6`) and
`documents.embedding` (`V42__documents_table.sql:20`). `agent_memory` is agent
working memory, not corpus knowledge; it should not move, and Shoal has no
equivalent. `documents.embedding` is one vector over the whole document with
in-Java cosine (`Document.java:101-103, 263-283`); Shoal's span-level retrieval
supersedes it for document Q&A once an adapter ships. Postgres stays the system
of record for the `Document` row, with Shoal holding a second, immutable,
content-addressed copy — a real duplication, to be settled rather than designed
around (Q5).

## 6. What must not move into Shoal

The SSH, RDP and API proxies are externally-effecting by definition, and Shoal
runs only work whose effect lands in its own evidence record.

**This section was written against an unmerged branch and has been corrected.**
It originally said the `Effect` type was not on main and existed only on
`feat/effect-boundary`. That was true when written; #381, #384 and #385 have
since landed, and three of the claims below changed materially.

`Effects` is a declared *set* per `Action`, not the two-value ladder the
original note described: `EffectReadsCorpus`, `EffectEgressesContent` and
`EffectMutatesExternal`, checked against the executor's ceiling via
`EffectBounded.MaxEffects()` and `executorCeiling`. The classes do not order —
writing a local file mutates without transmitting, streaming a corpus to a
hosted model transmits without mutating — so `exceeds` is subset semantics
rather than a comparison.

`validateDeclaredEffects` refuses to register a descriptor whose action declares
effects its executor was not bound for; `resolveActionBinding` re-checks at
dispatch, so rebinding an executor to a narrower ceiling stops live descriptors;
and `capabilitiesSubset` treats effects as something delegation may narrow but
never widen.

`AskExecutor` no longer declares a constant. It derives its ceiling from the
model provider it was actually configured with, because transmission is
configuration rather than code: the same executor is egress-free against a
loopback provider and egress-bearing against a hosted one. A provider that
cannot report its posture is treated as transmitting.

**How a Sentrius proxy would register.** `POST /api/v1/fleet/agents`
(`pkg/explorer/webapi/fleet_registry.go:68`) with a `Spec{ID,
AuthorizationDomain, Scopes, ExecutorRef, Capabilities, LeaseExpiresAt}`
whose `Action` entries carry `Effects: {EffectMutatesExternal}`. Because the
host binds no in-process executor willing to serve external effect, that
descriptor can never resolve to in-process execution — a structural refusal,
not a convention.

Corrected since #384: the bound reference needs no in-process `Execute` at all.
`resolveAction` used to assert `ActionExecutor` for every path, which would have
forced a remote deployment to bind a stub whose only purpose was to be refused.
`resolveActionBinding` performs the same resolution — generation, scope,
authorization, delegation, effect ceiling — without that assertion, and only
`ExecuteClaim` demands something runnable. A gateway proxy therefore binds a
reference that declares `MaxEffects()` and implements nothing else. The proxy heartbeats to hold its lease, work is
enqueued (`POST /api/v1/fleet/actions`), and the proxy pulls and claims with a
lease and fence (`fleet_dispatch.go:97-154`). The `ActionRecord` binds subject,
actor, client, delegation chain, authorization fingerprint, policy generation
and expiry (`fleet/dispatch_model.go:72-118`) — the "reason it was allowed".

**There is a hole in this, and it is load-bearing: there is no HTTP completion
endpoint.** The dispatch routes are enqueue, invoke, pull, claim, cancel, status
(`fleet_dispatch.go:56-207`); nothing accepts an `ExecutionResult{Output,
ErrorCode, EvidenceSnapshotID, Evidence}` (`dispatch_model.go:243-250`). After
`Claim` the only terminal paths are `ExecuteClaim`, which runs an **in-process**
executor (`dispatch_service.go:274-330`), and `Cancel`. An out-of-process
executor can take work and cannot report it: "register as an external-effect
executor" is half-built today — the declaration side lands with #381, the
completion side does not exist. A prerequisite, not a detail.

**Separately, the shape is wrong for session transport even once that lands.**
Fleet dispatch is one action in, one bounded result out: 1 MiB payload and output
caps, a 5-minute claim TTL, a 24-hour deadline
(`fleet/dispatch_model.go:24-33`). An SSH session is a long-lived bidirectional
byte stream (`ssh-proxy/.../handler/ShellHandlerRunnable.java`) and RDP a
Guacamole tunnel (`rdp-proxy/.../servlet/GuacamoleTunnelWebSocketHandler.java`).
What fits dispatch is the *decision* — may this principal open this session, run
this command — and the *per-command* record, already how
`ZeroTrustAccessTokenRequest.java:29` is shaped.

## Open questions

**Q1. Which side mints the `auth.Decision`?** (a) Shoal's OIDC authenticator
reads Keycloak tokens directly — needs a Keycloak role-claim mapper, since
`getRoles(Jwt)` (`api/.../config/SecurityConfig.java:118-124`) is dead code and
no role claim reaches Sentrius authorities today, and accepts the three-bucket
operation model. (b) Sentrius mints decisions in Java and Shoal trusts a service
credential — needs a new trusted-transport path in Shoal and imports Sentrius's
fail-open ABAC defaults. (a) is cleaner and weaker; (b) is stronger and re-opens
the problem `-backend remote` is disabled over. I lean (a) plus a per-document
`PolicySelector`.

**Q2. How are disjunctive `markings` represented?** (a) Restrict markings to
conjunctions at ingest and reject `|`. (b) Normalize to DNF and register one
Shoal object per disjunct. (c) Extend `auth.Policy` / `AccessRule` with
disjunction. (b) is the only one that preserves existing data and the one most
likely to produce a subtle disclosure bug.

**Q3. Does `KnowledgeGraphPort` survive?** (a) Implement it as-is and accept that
`deleteNode`, `updateNodeProperties`, `getAllNodes`, `findSimilarDocuments` and
`customQuery` are permanent no-ops. (b) Narrow it to what Shoal can enforce —
ingest, retrieve, neighborhood, path, ask — and rewrite the seven callers. (a)
hides that half the feature set is inert; (b) is more work and produces
something true.

**Q4. Where does the completion endpoint come from?** §6 does not work without
it. (a) Shoal adds `POST /api/v1/fleet/actions/{action}/complete` taking an
`ExecutionResult` under the claim fence. (b) Sentrius proxies skip fleet
dispatch entirely and record externally-effecting work only in Sentrius
provenance, forfeiting the "reason it was allowed lives in Shoal" property.
Settle this before planning against #366/#381.

**Q5. What is authoritative when Postgres and Shoal disagree about a document?**
(a) Shoal authoritative for content, Postgres holds metadata and the pointer.
(b) Postgres authoritative, every save re-ingesting and accumulating a Shoal
revision per edit. (c) Accept drift. (c) is not an option; it is the absence of
a decision.
