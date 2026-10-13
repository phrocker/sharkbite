# Shoal Explorer public contract

## Status and scope

This document is the concise normative, storage-neutral contract for public
Shoal document, graph, retrieval, error, and code-ingestion values. The terms
**MUST**, **MUST NOT**, **SHOULD**, and **MAY** are normative.

`pkg/explorer.Client` currently exposes `Ingest`, `Documents`, `Document`,
`Connect`, `Neighborhood`, and `Retrieve`. Those operations form a useful
embedded/product facade, but they are not yet a general document/graph storage
repository contract. Storage publication, history, deletion, durable policy
persistence, migration, and mutation APIs remain additive work.

`pkg/explorer/authorized.Client` is the M2 authorization-enforcing wrapper for
that facade. Deployments must use it with a trusted `pkg/explorer/auth`
resolver, policy selector, policy catalog, and generation reader; constructing
or retaining an unwrapped client remains an explicitly unauthenticated path.

The authorized client's `Connect` accepts only edge IDs in the caller's own
application edge namespace (#699). `Client.ApplicationEdgeID(ctx, localID)`
returns that ID: a fixed prefix, a digest of the caller's identity (the
authorization domain, subject, actor, client ID, on-behalf-of chain and
service identity), `/`, then `localID`. Any other edge ID is
`InvalidArgument`, before an endpoint or the catalog is read. Reads report the
namespaced ID. A writer's retry under the same identity reaches the same ID,
and two identities never share one, so whether `Connect` succeeds never
depends on an edge connected under another identity. The namespace binds the
identity, not its current authority: a narrowed session of the same identity
(workspace narrowing, a down-scoped token, a revoked label) shares it, and can
still tell whether a local ID that identity's broader sessions connected
exists. This is library-only today; `Connect` has no HTTP or MCP route.
Trusted edge policy selectors that
key on the edge ID can recover the caller's local ID with
`authorized.ApplicationEdgeLocalID`.

## Identity, bytes, and bounds

- `shoal.ID` is an opaque byte string. Implementations MUST retain every byte,
  including NUL and non-UTF-8 bytes accepted through direct Go values.
- IDs order lexicographically by unsigned raw bytes. Implementations MUST NOT
  apply Unicode normalization, case folding, trimming, parsing, regeneration,
  or a generic ID-generation scheme.
- A public ID is required where its containing value says so and is at most
  1,024 bytes.
- Metadata is opaque application data, not an authorization channel. Per
  object it is limited to 256 entries, 256 bytes per key, 4 KiB per value, and
  256 KiB total. Validation MUST NOT rewrite accepted keys or values.
- Scores and graph weights MUST be finite.
- Kind, type, title, heading, and source-version strings are at most 4 KiB.
  Graph nodes MAY have an empty kind. A node has at most 64 nonempty labels,
  each at most 256 bytes.
- Transport encodings MAY impose additional representability checks. In
  particular, protobuf string fields require valid UTF-8; that wire rule does
  not redefine the direct Go ID contract.

## Documents, revisions, and source ranges

- `(Document.ID, Revision.ID)` identifies one immutable revision.
  `Document.RevisionID`, `Revision.DocumentID`, and every section/span owner
  MUST agree.
- `Revision.CreatedAt` and `Revision.SourceVersion` are source metadata. They
  are not publication sequence, commit order, a latest-winner rule, or an
  `AsOf` frontier.
- Canonical revision source is valid UTF-8. `SourcePosition.Offset` is a
  zero-based UTF-8 **byte** offset. `SourceRange` is half-open
  `[Start.Offset, End.Offset)`.
- Empty ranges are valid, including `[len(source), len(source))`. Both
  endpoints MUST be in bounds and on UTF-8 rune boundaries.
- A revision contains exactly one connected, acyclic section tree. Its root
  is `Document.RootSectionID` and has empty `ParentID`; every other section has
  one same-revision parent.
- Child section ranges are contained by their parent. Span ranges are
  contained by their section. Child sections and direct spans share one
  sibling `Order` namespace; duplicate orders are invalid.
- `Span.Text` MUST equal the exact canonical source byte slice named by its
  range. Spans are not a source reconstruction format.
- Initial hard maxima are 512 MiB canonical source, 100,000 sections, and
  1,000,000 spans per revision.
- These rules apply at new operation boundaries and explicit validation.
  Implementations MUST NOT reinterpret old persisted alpha records merely
  because they are passively loaded.

## Citations, quotes, and evidence

- `Citation.Validate` is structural: document/revision identity, at least one
  section or span anchor, bounded IDs, and a structurally valid range.
- Contextual resolution additionally requires the exact retained revision,
  canonical source bytes, and referenced sections/spans. If both section and
  span are present, the span belongs to the section, and the cited range is
  contained by every named object.
- A canonical quote is exactly
  `source[Start.Offset:End.Offset]`. Submitted quote text, indexes, spans, and
  the latest revision are not quote authority.
- Citation validation helpers do not imply that a storage implementation can
  hydrate retained source. Missing hydration is an unavailable capability,
  not permission to approximate a quote.
- `retrieval.Evidence` structurally requires a valid citation, a finite score,
  and, when present, a valid directed `graph.Path`. Canonical quote equality
  remains a document-context check.

## Graphs, paths, and neighborhoods

- `graph.Edge` is directed from `From` to `To`. Cycles, self-edges, and
  parallel edges with distinct IDs are valid.
- `graph.Path.Edges[i]` MUST run from `Path.Nodes[i].ID` to
  `Path.Nodes[i+1].ID`. Repeated nodes and directed cycles are valid; reverse
  or mixed walks are not paths.
- For `B <- A -> C`, a neighborhood rooted at `B` MAY contain `A`, `C`, and
  the canonical edges when its requested depth/direction permits. It MUST NOT
  report `[B,A]` or `[B,A,C]` as a `graph.Path`.
- Incoming or mixed traversal is therefore neighborhood-only unless every
  emitted path step is directed from the current path tail. No replacement
  public walk type is defined.
- The current embedded `Neighborhood` expands both incoming and outgoing
  edges, normalizes depth zero to one, rejects depth above 16, and serializes
  final nodes by raw node ID and edges by raw edge ID. It does not return
  paths.

## Reserved `interaction.*` namespace

Interaction provenance lives in the same corpus as content, under a reserved
node-kind and edge-type namespace. Phase 1 of issue #266 defines:

- Node kinds `interaction.session`, `interaction.turn`, `interaction.tool_call`,
  and `interaction.tombstone`. Content MUST NOT use the `interaction.` kind
  prefix; ingest and `Connect` reject it.
- Session identities are minted by `interaction.SessionID` or
  `interaction.OperationSessionID` and always live under the
  `interaction.session_` prefix. Because every source publication path rejects
  the reserved prefix, interaction identities and source identities are
  disjoint by construction, so independent writers can never durably commit a
  source node and an interaction node under one graph identity.
- Edge types `interaction.has_turn`, `interaction.has_tool_call`,
  `interaction.retrieved`, and `interaction.cited`. Retrieved and cited are
  distinct: retrieval records everything the model was shown, citation records
  only what the answer referenced. Recording citations alone would understate
  exposure.
- Retrieval and graph expansion exclude interaction nodes by default. An
  interaction node is returned by `Neighborhood` or `BoundedNeighborhood` only
  when it was an explicit seed in the request, and never by `Retrieve`. The
  exclusion lives in the retrieval path itself, not in caller convention, so a
  model cannot cite its own prior output as source evidence.
- `Interactions` and `InteractionSubgraph` are the explicit, kind-scoped entry
  points for reading interaction provenance.
- The `shoal.visibility` property of an interaction node is the conjunction of
  every visibility label of every source node the session touched, retrieved as
  well as cited. It is never derived from the asker's grant set, because that
  would turn a highly cleared asker's session into a covert channel. An
  unresolvable touched node fails the record rather than defaulting to public.
  A reviewed declassification path is deferred to a later phase.
- Capture is part of serving an inference. `EnsureInteractionSink` reports at
  setup time whether a corpus can durably record interactions; a read-only or
  offline corpus refuses `ask` outright with a clear diagnostic rather than
  failing at first write. A recording failure fails the request.
- Retention is explicit deletion only; there is no TTL. `DeleteInteraction`
  replaces a session's nodes and edges with an `interaction.tombstone` node
  carrying the original visibility, so the deletion is itself auditable. A
  deleted session ID can never be reused.
- Interaction nodes and edges are excluded from the content snapshot hash, so
  concurrent capture never invalidates a pinned snapshot.
- Interaction records are redacted by construction: they carry identities,
  digests, counts, and source node IDs, never the question, prompt, answer,
  evidence text, or model-chosen correlation strings.
- A typed session carries one correlation string, `Session.CorrelationID`: the
  correlation of the authenticated decision it was recorded under — the
  `Shoal-Correlation-ID` a caller supplied, or the one its authenticator
  generated (#527, #532). It lets one operation be followed across hops (a
  gateway acting for a user, an approval and the dispatch it becomes), which a
  request ID, naming one request, cannot.
  - **Stamped by the trusted sink.** The authorizing sink sets it from the
    decision, as it sets `Actor` and `Reason`, and ignores any value the
    producer supplied. Producers and recorders accept the stamped value.
  - **Metadata only.** It is caller-supplyable, so it decides nothing about
    what a session is: it is not an input to any session ID, it is not
    materialized into the interaction subgraph (no graph property carries it),
    and it is excluded from `AuthorizationFingerprint`. A retry that differs
    only in correlation is the same session; it neither conflicts nor
    overwrites, and the first recorded correlation stands. A caller can
    therefore neither split one session into two nor merge two into one by
    choosing a header.
  - **Bounded and printable.** The session boundary refuses a value over
    1024 bytes, not valid UTF-8, containing a space, or containing a
    non-printable character (control characters, bidi overrides) — the shape
    the hosted authenticators already enforce at mint, checked again because a
    decision can be minted elsewhere and a stored record can be read back.
    The sink *drops* an unrecordable decision correlation rather than
    refusing: correlation must never be why an otherwise authorized operation,
    or its audit, fails. The session then records none rather than a
    substitute.
  - **Not integrity-protected, like every other field.** The durable record
    has no MAC or digest over its fields; anyone who can rewrite the row can
    rewrite `Actor` as easily. A digest of the correlation stored beside it
    would be rewritten with it, so none is added; tamper evidence for the whole
    record is a separate concern.
  - **Compatibility.** Sessions are gob-encoded and gob matches fields by
    name: a session written before #532 reads back with an empty correlation,
    and the previous build reads a #532 session, ignoring the field.
  - **Exposure.** It is served only where the session's actor already is —
    `InteractionSummary`/`InteractionRecord` and the provenance list and
    inspect views (`correlation_id`, omitted when empty) — and so only to a
    caller already authorized to read that session. A tombstone keeps no
    correlation.

### Fold summaries

Phase 2 of issue #266 adds fold-style summarization, following the public
`fold` semantics of `phrocker/sag`: a set of recorded sessions collapses into
one compact vertex that can later be unfolded back into what it replaced.

- A fold is an `interaction.fold` node with `interaction.folds` edges to the
  sessions it folds, plus `interaction.retrieved` and `interaction.cited` edges
  to the source nodes those sessions touched. Because the kind and the edge
  types are in the reserved namespace, every default-exclusion rule that
  applies to a session applies to a fold unchanged: `Retrieve` never returns
  one, expansion never discovers one, and the visibility resolver refuses to
  treat one as source evidence for a later inference. A model cannot cite its
  own prior summary as though it were a source document.
- A fold's visibility is the conjunction of every folded session's own
  visibility and every label of every source node those sessions touched,
  retrieved as well as cited. Summarizing can therefore only narrow visibility,
  never widen it. That is precisely why publishing a redacted public summary
  must be a separate, explicit, reviewed action; **no declassification
  primitive exists**. The declassification workflow and its authority model
  remain deferred; issue #128 tracks deployment coordination authority, not
  declassification.
- Fold identity is content-addressed over the canonical folded provenance and
  the summary digest. The same input always folds to the same vertex, so
  refolding is idempotent rather than a conflict. Neither the fold time nor the
  derived visibility participates in identity, so a corpus whose labels moved
  under an existing fold fails closed instead of serving a stale visibility.
- `FoldInteractions` names sessions only. The provenance it folds is read from
  what those sessions actually recorded, never from the caller, so a caller
  cannot understate what a session was shown and thereby widen the fold.
- `RehydrateFold` is lossless with respect to provenance: every folded session
  comes back with its retrieved set and its cited set kept apart. Collapsing
  the two would understate what the model was shown and make the visibility
  conjunction unsound.
- A fold carries no summary text, only the SHA-256 digest of a summary held
  out-of-band. The digest shape is enforced, not trusted, so the field cannot
  be used to smuggle evidence-derived text into a node payload.
- Retention matches sessions: `DeleteFold` leaves an `interaction.tombstone`
  and drops the folded members, and a session that a live fold summarizes
  cannot be deleted until that fold is.
- Folding is an operator action, not part of serving an inference, so it never
  sits on the request latency path.

### Output labels on interaction and fold reads (#564, #567, #568)

An interaction record and a fold store the output-policy label set they
require of a reader. That is the session's `RequiredVisibility`, its conjoined
`Visibility` expression, the `shoal.visibility` property on its derived nodes
and edges, a fold's `Visibility`, and each fold member's `Visibility`. Every
interaction and fold read of the authorized client evaluates it against the
reader with the reader label evaluator (`authorized.LabelVisibility`):
`InteractionRecord(s)`, `InteractionRecordsPage`, `Interaction(s)`,
`InteractionSubgraph`, `Folds`, `FoldsPage`, `RehydrateFold`, and
`FoldInteractions`. Team overview, the HTTP provenance API
(`output_visibility`) and the MCP `shoal.provenance.*` tools all read through
these calls, so they inherit it.

- **The evaluator.** A stored label set is a conjunction of structured grant
  labels: `d:<domain>`, `s:<source>`, `g:<policy>:e:<epoch>`, `svc:<role>`.
  A user decision may see it when its grants cover every term
  (`auth.VisibilityPermittedForUser`; a grant at any epoch passes). A trusted
  service additionally needs every term inside its configured ceiling
  (`auth.VisibilityPermittedForService`, exact epochs), resolved per request
  by a `CeilingResolver`; a service with no ceiling configured is refused
  every labelled set. A free-form or malformed term is held by nobody and is
  false. A resolver or ceiling-resolver failure is an error. There is no
  cross-request cache and no change to `auth.Decision` or its fingerprint.
- **Which labels.** A free-form ingest label such as `secret` is enforced as a
  structured policy per (source, label) conjoined into the document's
  AccessRule (#570). A producer derives a session's output restriction from
  the `shoal.visibility` of the nodes it touched, which carries the free-form
  form, so the authorized recorder replaces each free-form term with the grant
  labels of the label policy that enforces it in the touched nodes' rules
  before the record is written. A term no touched rule enforces is kept as it
  is, which no reader holds.
- **Records written before translation** carry free-form terms. Their touched
  nodes are known, so a read translates them through the same mapping over
  the nodes' current rules; that is exact, because those rules are what the
  touched-source check already requires of the reader. A label no current
  rule enforces (an untranslatable document, or a record whose producer named
  a label no touched node carries) stays unsatisfiable: withheld from
  everyone.
- **Interactions.** A record whose output restriction the reader may not see
  is `ObjectNotFound` on a point read and absent from every list. Records are
  never partly withheld: one conjunction is stamped on every derived node and
  edge, and turns carry no labels of their own.
- **The expression.** It is returned exactly when the reader may see every
  term it names. A reader who may see the record but not its whole expression
  (a trusted service whose ceiling lacks a touched node's label, or a record
  whose producer kept an output label off its restriction) receives the
  expression cleared exactly as an unlabelled record stores it: empty string,
  absent property, including the visibility digest and count. This replaces
  the interim recorder-only rule of #574.
- **Folds.** A fold is visible only when every member session is and the
  reader may see the fold's own visibility, which conjoins every member's. A
  fold cannot be partly unfolded, because its summary digest covers every
  member, so a visible fold is returned unchanged. `FoldInteractions` refuses,
  before writing, a fold its caller could not then read.
- **No marker.** What a reader without the labels receives is byte-for-byte
  what it would receive had the record never been written. No response
  carries a per-record or per-response "filtered" marker or count (#398).

**Residuals.**

- A reader holding the labels sees the record whoever recorded it; the
  recorder sees it only while it still holds them.
- The free-form form of a label stays in the expression, as provenance,
  next to the structured terms, and is shown to readers permitted the whole
  expression.
- A trusted service whose role requires service visibility (every role but
  `data_read` and `data_write`) sees a labelled set only when it also carries
  that role's `svc:<role>` term, as at the tablet. No record does, so such a
  service, an executor worker included, sees no labelled stored record, and
  no dispatch evidence reference at all, since every document rule's terms
  would have to carry its role.
  `shoal-explore-web` mints no trusted-service decision and configures no
  ceiling.
- After a document is **tightened**, the explorer refuses an interaction
  record that touched it to every reader until it is re-recorded (its stored
  visibility no longer covers its sources), while dispatch evidence naming
  the same node is decided by the node's current rule and stays visible to a
  holder of the new labels. A cross-plane test pins both verdict tables.
- Re-ingesting a document's earlier revision exactly (same content, same
  labels) does not make it current and does not move any rule back. The
  authorized client refuses it with `authorized.ErrHistoricalRevision`
  (`conflict`) rather than report success (#674).

### Cross-session provenance traversal

- `InteractionsTouching` lists every session and fold that retrieved or cited a
  given source node. `RelatedInteractions` walks from one session or fold to
  every other interaction that touched at least one of the same source nodes.
- Both are explicit, kind-scoped reads, like `Interactions` and
  `InteractionSubgraph`. Neither is reachable from `Retrieve` or from expanding
  a source node's neighborhood: walking provenance is an operator and auditor
  capability, never something an inference can do to itself.
- The walk goes interaction to source node to interaction. `InteractionsTouching`
  refuses an interaction node as its subject, because treating derived content
  as the origin of a provenance walk is the same mistake as treating it as
  source evidence.
- `shoal-explore fold`, `shoal-explore unfold`, and `shoal-explore provenance`
  are the operator-facing surface for all of the above.

## Retrieval requests

### Normalization and limits

`Request.Normalize` returns cloned values and MUST NOT mutate caller-owned
slices. Normalization is idempotent:

- `TopK == 0` becomes 20; normalized `TopK` is at most 1,000.
- Empty modes become exactly lexical.
- Duplicate modes and duplicate scope IDs collapse by first occurrence.
- Query text is nonblank valid UTF-8 and at most 16 KiB.
- Combined normalized document/node scope cardinality is at most 10,000.
  Scope IDs are nonempty and individually obey the public ID bound.
- Nonzero `AsOf` represents the same instant normalized to UTC.

Equivalent normalized requests have the same request identity where an
implementation exposes one. Duplicate modes MUST NOT change scores.

### Scope and modes

- IDs are ORed within `DocumentIDs` and within `NodeIDs`.
- When both lists are nonempty, a candidate satisfies both dimensions only
  through a canonical document/graph association. Metadata, labels, string
  equality, or coincident IDs MUST NOT fabricate that association.
- Requesting vector mode requires vector support. An implementation that does
  not support it returns `unavailable`; it MUST NOT silently drop vector,
  substitute lexical retrieval, or claim an approximate result as exact.
- The fixed seed planner is lexical, then tree, then graph:
  - standalone tree requires document scope or bounded lexical seeds;
  - standalone graph requires node scope, implemented document-association
    seeds, or bounded earlier lexical/tree seeds;
  - a shape with no bounded seed source returns `unavailable` before corpus
    scanning. It MUST NOT fall back to a corpus-wide tree or graph seed scan.

### `AsOf`, completeness, and order

- `AsOf` requires a store-defined publication frontier. It MUST NOT be
  implemented by comparing `Revision.CreatedAt`.
- A store without publication-frontier semantics returns `unavailable` for an
  explicit `AsOf`. The current embedded Explorer does so.
- `Retrieve` is unary and complete-or-error. It has no partial-result envelope
  or cursor.
- Results order by score descending, then raw result-ID bytes ascending.
  Result IDs are nonempty, bounded, and unique, and result count does not
  exceed normalized `TopK`.
- Evidence orders by score descending, then
  `(document, revision, section, span, start offset, end offset)`, then the
  directed path tuple of node IDs followed by edge IDs.
- `pkg/retrieval` exposes versioned analyzer and scorer contracts. The shared
  embedded analyzer version includes its Unicode table version, and the shared
  coverage/fusion scorer fixes the lexical, tree, and graph formulas. An
  adapter claiming those versions MUST produce bit-identical terms and scores.
- A future duplicate-result merge function MUST preserve these boundaries.
  No adapter may claim a shared analyzer/scorer version while substituting
  storage-local behavior.

## Canonical logical records

- `pkg/explorer/canonical` defines the versioned deterministic logical record
  encoding used for cross-adapter comparison. It is distinct from the
  embedded persistence envelope and contains no rows, columns, visibilities,
  iterators, or tablet coordinates.
- Canonical bytes preserve opaque IDs and metadata exactly, sort metadata keys
  by unsigned bytes, retain caller-supplied slice order, and carry exact source
  bytes. The envelope includes magic, version, kind, payload length, and a
  SHA-256 checksum.
- A present publication coordinate has a positive signed-64-bit sequence and
  nonzero publication time. It is separate from `Revision.CreatedAt`.
- A canonical document-revision record is complete: graph edge endpoints must
  name nodes in that record, public values and ownership validate, IDs are
  unique within each value family, and unknown versions or malformed,
  oversized, truncated, checksum-broken, or trailing data are rejected.
- The encoding is injective: each logical record has exactly one valid byte
  representation, so equal records always have equal digests and distinct
  bytes never decode to equal records. Decoding therefore rejects any payload
  that is merely decodable rather than canonical — such as permuted metadata
  entries or an alternate long-form encoding of a zero time — even when its
  envelope checksum is consistent. `Digest` is a content identity and is safe
  to use for deduplication, idempotency, and integrity equality.

## Latest state, mutations, and retries

Current public values contain no generic epoch or tombstone field. Future
additive storage APIs use this vocabulary:

- latest selection chooses the greatest visible published generation at the
  requested frontier **including tombstones**, then interprets that winner;
- create requires expected base `ABSENT`; update/delete name the exact base;
- generic mutations carry a nonempty idempotency token of at most 128 bytes,
  scoped by operation/domain; token reuse with different canonical content is
  `conflict`;
- a lost response resolves to the committed token outcome and MUST NOT create
  new identities or duplicate artifacts.

These are requirements on future APIs, not permission to add epoch/tombstone
fields to existing public values.

`explorer.Ingest` is content-addressed. Its `IngestDisposition` (wire field
`disposition`) takes exactly one of three stable values:

- `applied` (`IngestApplied`): a new revision was published and is now the
  document's current revision;
- `unchanged` (`IngestUnchanged`): the content matches the document's current
  revision; nothing was written;
- `historical` (`IngestHistorical`, #674): the content matches an earlier,
  non-current revision; nothing was written and that revision was **not** made
  current again. The document keeps serving its current revision.

`historical` is a successful result, so a delayed retry of an ingest that has
since been superseded still succeeds on the base. Ingest never makes an
earlier revision current again, because a stale retry would otherwise roll the
document back silently. There is no restore operation; to make earlier
content current again, change it by any byte. When the document's
publication order is unavailable (only legacy revisions), the base reports
that `unavailable` error instead of either disposition. Graph materialization
reports only `applied` and `unchanged`.

The authorized client (`pkg/explorer/authorized`) does not report success for
content it did not make current. It refuses a `historical` result (and an
`unchanged` one whose revision is not current) with a `conflict` error that
wraps `authorized.ErrHistoricalRevision` (test with `errors.Is`). The source
claim is rolled back and nothing is registered, so the document, its current
rule and its claim are exactly as before. A narrowing request still leaves its
declassification intent record, which records an attempt, not an outcome.

Two authorized ingest refusals carry `explorer.IsRefusedWithoutEffect`
(#687): this historical refusal, once its claim rollback has succeeded, and
the `-require-declassification-reason` refusal raised before the source claim
(`authorized.IsDeclassificationReasonRequired`). The marker means the call
left the document, its registrations and its source claim as they were; the
only record it may leave is a narrowing's declassification intent, which
records an attempt. It is not set when the claim rollback failed, and it
is never reported beside an indeterminate-commit marker or through a joined
error. Other errors, including a reason refusal raised later in the ingest,
do not carry it.

The workspace upload batch (`POST /api/v1/ingest`, `webapi.IngestProvider`)
reports either marked refusal per file and goes on with the rest. Any other
per-file failure stops the batch. A batch that did not fully succeed is a
`webapi.IngestBatchError`. It unwraps to the batch's top-level error, which
keeps its code, message and indeterminate marker, and it carries one
`webapi.IngestFileOutcome` per file (`webapi.IngestFileOutcomes`; the wire
field `files` in the error body). Statuses are `applied`, `unchanged`,
`historical`, `refused` (with `reason` `historical_revision` or
`declassification_reason_required`), `failed` and `not_attempted`. The
top-level error is marked indeterminate only when a file's own outcome is
unknown, which is when the failure that stopped the batch was itself
indeterminate. A file that returned a result has a known outcome. So a batch
where files were applied or unchanged beside refusals, or before a definite
failure, keeps that definite error and is partial (`webapi.IngestBatchPartial`).
Over HTTP that means the `Shoal-Commit-Outcome: partial` header and
`"partial": true`. Retrying a partial batch converges, because its applied
files come back `unchanged`. The batch error unwraps only as a multi-error, so
it is never `explorer.IsRefusedWithoutEffect` itself; `errors.Is`, `errors.As`
and the Shoal code see through it. `webapi.IngestOutcomeKnown` reports a
batch in which every file was tried and has a known outcome.

The authorized client returns the no-effect marker only on the refusals it
gives itself. Any other error it returns, including one passed on from its
base, has the marker concealed (`explorer.ConcealRefusedWithoutEffect`).

Every error an authorized ingest returns once something durable may have
changed is indeterminate (`explorer.IsIndeterminateCommit`). That is from the
moment the base reports a write (any disposition but `unchanged` or
`historical`), or the registration is attempted. A failed read after the
write, a failed pre-commit audit, a commit-time reason refusal, a failed
registration and a failed guard all qualify: each may leave the claim pending
or a revision registered. The marker only adds, so codes and `errors.Is`
identities are kept. An error before any write stays definite, and so does an
error after an `unchanged` result that fails before the registration. The
base's own errors are classified by the base, and the client trusts an
unmarked one as definite, as its claim rollback does.

The base holds to that. Once `explorer.Ingest` has durably written the
document record, the revision is published: a retry finds it. Any later error
from the same call, such as a failed graph rebuild, is marked indeterminate
with its code kept. A graph materialization or extraction whose record is
durable reports a later rebuild or snapshot-registration failure the same
way. Interaction and fold writes already mark such failures with
`MarkCommittedInteraction`.

`pkg/code` retains its existing typed stable-ID algorithms. `Source.Ref` is
excluded from `Source.ID` and the ingestion key because multiple refs can
resolve to one immutable revision. Retrying a committed identical ingestion
returns `IngestUnchanged` with identical artifact identities. A materializer
version change cannot reinterpret an existing key; it honors the committed
outcome or uses a future versioned operation/key contract.

`pkg/explorer/codematerializer` implements `explorer-code-v1`. It derives the
document title and source URI only from identity-bound repository/path values,
excludes mutable refs from canonical output, preserves existing code entity
and relationship IDs byte-for-byte, and returns document then graph artifact
references in stable order. Its additive association values describe the
document/graph relationships that a later storage adapter must persist.

## Authorization and non-disclosure

- `pkg/explorer/auth.Decision` is immutable trusted input. It binds subject,
  actor/client, authorization domain, allowed operations, source/policy
  grants, policy generation, expiry, request identity, and an optional
  least-privilege service role. Metadata, retrieval scope, graph labels, URI
  text, and claimed object ownership never add grants.
- Trusted context binding is capability-scoped: a binder from one
  `auth.Authority` cannot forge values accepted by another authority's
  resolver. Missing, expired, wrong-domain, or wrong-operation whole-request
  authority returns `unauthorized`; cancellation and deadlines retain their
  stable categories.
- Direct hidden objects return the same `not_found` shape as absent objects.
  Collections omit hidden values. Retrieval intersects scope with the
  authorized current-document/node projection before ranking, so hidden
  candidates cannot displace results or influence scores, explanations, or
  limits.
- Vector-space observability is projected at that same boundary.
  `authorized.Client.RetrieveWithReport` and the request-local
  `authorized.WithEmbeddingQueryObserver` callback report only distinct spaces
  reached after current policy and mosaic filtering. An authorized space is
  represented by a process-keyed opaque ID and one of `available`,
  `unavailable`, `not_attempted`, or `not_completed`; raw provider/model
  identity and metadata are never returned. A hidden-only projection performs
  no provider call and exposes no space ID. The existing `Suppressed` and
  `Restricted` document counts remain the complete permitted disclosure about
  withheld content; the embedding report adds only corresponding booleans, not
  hidden-space identities or counts. Generation is rechecked before release,
  and cancellation, expiry, revocation, or a failed generation read scrubs
  observed IDs and activity counters.
- `webapi.RetrievalResponse.embedding` carries the same report on success.
  Failed vector retrieval returns an `EmbeddingQueryError`; HTTP places its
  report beside the stable error code, and `RemoteService` reconstructs it.
  The wire encodes opaque space IDs with the standard unpadded base64url ID
  codec. Error text is generic and never substitutes raw space identity for
  the opaque report.
- Ingesting a source URI that already identifies a registered document
  requires ingest authorization under that document's current rule. A caller
  who cannot see the document is refused with the same `not_found` shape as an
  absent one, so a source URI cannot be probed, seized, or relabeled. A caller
  the current rule authorizes may reclassify the document, because the newly
  selected policy must also authorize that same caller. Shared source-URI
  claims are acquired or compare-and-swap transitioned before base mutation,
  serialize concurrent wrapped clients, and retain the selected rule when a
  base commit outlives a policy-catalog failure. Legacy registered documents
  lazily backfill claims only after current-rule authorization; an existing
  document with neither a claim nor registration is unavailable. An explicitly
  indeterminate base commit releases operation ownership but leaves a pending
  claim containing the selected rule and any previous rule. First-ingest
  recovery requires the selected rule; reclassification recovery requires both
  rules and must select the pending desired rule, preventing either side from
  seizing an ambiguous outcome. One recovery attempt reacquires exclusive
  ownership. Definite failure restores the pending state, another indeterminate
  result keeps it pending, and successful current registration commits the
  desired rule. M3 supplies durable outcome reconciliation across processes.
  Only a returned current revision may finalize a claim transition. Content
  that resolves to a historical revision restores the previous source-wide
  claim, registers nothing, and is refused with
  `authorized.ErrHistoricalRevision` (#674).
  M2 claim finalization is local and atomic: commit, pend, and rollback of an
  exact acquired token are infallible and idempotent for the same outcome.
  Finalization errors therefore mean an invalid token or catalog invariant
  violation and are never discarded. Fallible durable coordination and
  recovery belong to M3.
- Structured policy labels are canonical lowercase no-padding base32 terms
  for domain, source, policy/epoch, and trusted service role. Callers cannot
  submit visibility expressions. Service-account ceilings intersect required
  labels and service roles cap allowed operations.
- Policy generation is rechecked before mutations and before response
  serialization. A changed or unreadable generation returns `unavailable`;
  old contexts cannot mutate. A newly issued decision may still authorize
  immutable objects written under an earlier physical policy epoch.
- Graph edges require their own policy plus the current policies of both
  endpoints. Neighborhood reachability is recomputed after authorization so a
  hidden intermediary cannot reveal or bridge visible nodes. Wrappers sharing
  a policy store serialize base mutations, reserve each edge ID and selected
  rule before the base write, and retain that reservation after ambiguous or
  catalog failures so another selector cannot seize or relabel the edge.
- Backend retrieval values are not trusted merely because their IDs are
  authorized. The wrapper verifies registered document digests, exact
  citations/quotes/ranges, canonical path nodes/edges, the exact canonical
  top-K prefix, and the shared analyzer/scorer explanation before returning
  them. Backend request IDs are cleared rather than exposed.
- Authorization fingerprints, cache keys, audit values, and public strings
  contain digests/categories rather than raw queries, source text, quotes,
  IDs, labels, credentials, physical coordinates, or serialized responses.
  Audit value pseudonyms use an ephemeral process HMAC key so low-entropy
  values cannot be recovered by hashing guesses and rotate on process restart.
- `authorized.MemoryPolicyStore` is an M2 reference catalog that can be reused
  across an in-process embedded restart. It is not durable process recovery;
  M3/M4 must atomically persist policy/publication state before production
  adapters claim restart or failover safety.

## Stable public error categories

Only the existing categories are used:

| Condition | Code |
|---|---|
| Invalid value, static bound, malformed range/tree/path | `invalid_argument` |
| Absent or hidden individual object; unavailable retained history | `not_found` |
| Idempotency/precondition/base mismatch | `conflict` |
| Whole-operation authentication or authorization denial | `unauthorized` |
| Unsupported requested capability/shape, transient store failure, or runtime budget exhaustion | `unavailable` |
| Caller cancellation | `canceled` |
| Deadline elapsed | `deadline_exceeded` |
| Detectable committed-data corruption or implementation fault | `internal` |

Object-level authorization, when implemented, is indistinguishable from
absence. Error text MUST NOT expose storage coordinates or hidden identities.

## Store-dependent and deferred behavior

The contract above does not claim current implementation of:

- a storage-neutral document/graph repository, generic mutation API, latest
  publication frontier, tombstone persistence, history retention, or
  citation hydration;
- durable authorization-policy persistence, canonical association
  persistence, pagination, migration, or distributed
  publication/conditional-write coordination;
- an Accumulo schema or adapter;
- vector indexing, alternative scorer implementations, or a new graph
  walk/path shape.

Adapters advertise and test only the capabilities they implement. Missing
store-dependent behavior returns the stable public error appropriate to the
condition; it is never approximated by reinterpreting source timestamps,
metadata, graph labels, or another retrieval mode.
