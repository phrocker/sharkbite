# Shoal features

This document is the feature and validation index for Shoal. The
[`README.md`](README.md) is the task-oriented entry point; detailed design and
operational contracts remain in the linked runbooks and ADRs.

## Durable fleet events

`pkg/explorer/fleetevents` defines bounded event publication and subscription
lifecycle operations for the agent-fleet control plane. Event envelopes carry
stable identity, correlation, reason, and authorization-evidence fields only;
they cannot contain arbitrary payloads, callbacks, credentials, or egress
destinations.

The embedded `internal/explorerfleetevents` writer uses Explorer transactions
and entity guards as its source of truth and allocates a dense stream sequence
under durable compare-and-swap. Delivery uses the runtime-owned
`Runtime.ScanCommitted` proof boundary: raw physical presence is not proof
that an event committed, and prepared, quarantined, and aborted cells are
excluded. Pull cursors are authenticated,
expiring, opaque, and scoped to the subscription, subscriber, subscription
generation, and exact authorization fingerprint. Version 3 cursors encrypt and
authenticate their complete contents with AES-GCM; earlier and other legacy
formats are rejected. Their 32-byte encryption root is domain-separated from a
durable per-corpus load-or-create key, so cursors survive process restart but
not corpus replacement. Delivery reuses the narrow
`subscription_create` authorization operation rather than generic data read
or event publication. It rechecks
authorization generation and the target agent's exact positive registry
generation, lease, revocation, parent delegation, and narrowing before and
after page computation.

The event log retains 10,000 logical event slots by default and durably records
its event-local readable floor without advancing the shared runtime history
floor. Expired cells are physically tombstoned with their owning guards while
the floor advances atomically; sequence-specific guards prevent a retired
ownership claim from being revived when a physical slot is reused. Publication
receipts are keyed independently by a digest of the caller token and retained
for their declared UTC retry window of at most 24 hours. Event slots therefore
rotate at the configured log bound without waiting for receipt expiry, while
an exact retry still returns the original receipt after its event leaves the
readable log. Reusing an active token with changed content or deadline fails
closed; expired receipts are incrementally pruned and the token may then begin
a new window with a distinct event identity. Exact retries after their
deadline return `fleetevents.ErrPublicationExpired`.
A cursor below the floor returns `fleetevents.ErrResyncRequired`.
Subscription records likewise use bounded deterministic slots: active
collisions fail closed, while expired or revoked occupants can be replaced.
Separate bounded, token-qualified create and delete receipts preserve the
original immutable result through live-slot deletion/reuse and WAL restart,
allowing an ambiguous interaction receipt to be repaired during the declared
retry window. Expired mutation retries return
`fleetevents.ErrMutationExpired`; unexpired receipt-slot collisions return
`fleetevents.ErrRetentionCapacity` rather than repurposing a receipt.
Older values remain readable only from valid pre-prune frontiers; current
committed scans hide them.

`internal/explorerfleetevents.NewActionEventPublisher` maps durable dispatch
lifecycle transitions into this log. Each envelope carries the immutable
producer generation and an opaque SHA-256 transition ID derived from the event
kind, action ID, and exact dispatch transition discriminator. The publication
token binds only the kind, action ID, and durable action version. Immutable
producer identity, producer generation, and transition identity remain in the
canonical persisted envelope, so a same-version divergent retry conflicts
rather than appending another event. Every hashed component is uint64
length-framed; raw enqueue, claim, execution, and cancellation keys are never
exposed, and exact retries remain idempotent. Trusted lifecycle publication is separate
from public `event_publish`, preserves its canonical token across authorization
refreshes, and accepts only the original narrow
`dispatch` or `invoke` authorization, not `event_publish`, and reauthorizes the
durable action identity on every attempt. The five `action.*` lifecycle kinds
are reserved from public publication, and each trusted kind is bound to its
exact `dispatch` or `invoke` operation. Event evidence includes the base
authorization target plus canonical executor node, edge, anchor, revision,
range, and assertion references. Visibility remains trusted interaction-sink
state and is re-derived from current labels rather than persisted from callers.
Lifecycle receipts retain the durable transition's original authorization
fingerprint, expiry, request, and correlation pins across an ambiguous retry,
while the current decision is still reauthorized before each attempt.
Dispatch mutations atomically persist an immutable transition outbox/history
entry with the action state. Publication acknowledges that entry only after
both event append and lifecycle audit succeed. `DispatchService` can explicitly
reconcile pending transitions for an action with a fresh authorized context,
independent of the original request deadline and after runtime restart; later
action states cannot overwrite an earlier unpublished transition. Lifecycle
interaction sinks authorize exact evidence with the transition's `dispatch` or
`invoke` operation, rather than requiring unrelated `retrieve` permission.

Evidence visibility labels are checked per subscriber at delivery (#562). A
lifecycle envelope carries, alongside each exact evidence reference, the
visibility expression the executor recorded it under
(`Event.ConsumedEvidenceVisibility` / `CitedEvidenceVisibility`). The
`(domain, source, policy, object)` authorization delivery performs is not a
label check, so `Service.authorizeDelivery` — the one gate every
subscriber-facing read passes through: backfill from the retained floor,
resume from a cursor, long-poll, and the HTTP pull route — then asks
`fleetevents.Config.EvidenceVisibility` whether the subscriber, under its own
context at delivery, holds each reference's labels. That seam is
`evidencelabels.Visibility`, the same single type and the same
`evidencelabels.Filter` rule the dispatch read paths use for #369
(`fleet.DispatchConfig.EvidenceVisibility` is an alias of it). A reference the
subscriber may not see is dropped whole — anchor, citation, node and edge IDs,
offsets, label expression, and the authorization join entry naming it. The
host implementation is `authorized.LabelVisibility` (#564), which decides
structured grant labels against the subscriber's own decision (and, for a
trusted service, its configured ceiling); dispatch records each reference's
labels as the structured terms of the (source, label) policies enforcing
them (#570), so a subscriber holding them receives the reference unchanged.
Nil means no subscriber may see labelled evidence; unlabelled references are
delivered unchanged; an evaluator error fails the pull rather than reading as
a redaction. The rule is evaluated at delivery, never at publish: the
publisher is the wrong identity to ask, and the durable event — like the
action record and the lifecycle audit receipt — keeps every reference.

No count of withheld references is delivered, and nothing marks that one was
withheld. This is #398's rule: a standing refusal must be indistinguishable
from absence, and a count that disagrees with the delivered list is an
existence oracle for the rest. The consequence is stated rather than hidden —
a delivered evidence list makes no completeness claim, and a subscriber cannot
tell "recorded no evidence" from "recorded evidence you may not see". Events
written before the visibility fields existed decode as unlabelled.

`webapi.NewFleetEventsHandler` serves the `/api/v1/fleet/events/` subtree and
`webapi.Handler.MountFleetEvents` mounts it once through the existing
authenticated handler. Routes cover subscription create/delete, event publish,
and bounded long-poll delivery. Long polls are capped at 25 seconds, below the
production server's 30-second write timeout; requests have an event-specific 512 KiB limit
and inherit the host-authority, authentication, cancellation, and response
controls. Raw wait and TTL integers are range-checked before duration
conversion. Opaque byte IDs use unpadded base64url at the HTTP boundary.

## Status legend

- **Shipped**: implemented, tested, and available on `main`.
- **Partial**: useful implementation exists, but a named production adapter or
  contract remains unimplemented.
- **Code complete; live validation pending**: the production path and
  fail-closed conformance gate are implemented, but the exact Accumulo 4
  Docker verdict still needs to run on a Docker-capable host.
- **External validation pending**: implementation is complete, but native
  platform or release-infrastructure evidence is unavailable in the current
  environment.
- **Infrastructure blocked**: repository code or scripts exist, but completing
  the release path requires credentials or infrastructure unavailable to the
  current maintainers.

## Core storage engine

| Capability | Status |
|---|---|
| Embedded engine with WAL, memtable, splits, flush, and compaction | Shipped |
| RFile read/write, locality groups, CRC, compression, and block metadata | Shipped |
| Native Parquet persistence, restart, import/export, migration, and compaction | Shipped |
| Indexed Parquet reads with row-group statistics, bloom filters, and page indexes | Shipped |
| Mixed RFile/Parquet tablet scans and ShoalQL execution | Shipped |
| Local, memory, GCS, S3, Azure Blob, and HDFS storage | Shipped |
| Atomic publication, cancellation, recovery, and stale-artifact cleanup | Shipped |
| Format policy and authoritative-RFile promotion boundary | Shipped |

## Query and indexing

| Capability | Status |
|---|---|
| ShoalQL parser, planner, capability contracts, and text/JSON `EXPLAIN` | Shipped |
| SQL predicates, ranges, ordering, and aggregation | Shipped |
| Graph traversal and latent-edge discovery | Shipped |
| Document predicates and term-index queries | Shipped |
| Exact vector search | Shipped |
| Distributed IVF-PQ lifecycle, tombstones, freshness, and recall contracts | Shipped |
| Local RFile, local Parquet, mixed, and Accumulo-backed corpus parity | Shipped |
| Provider-neutral model I/O with deterministic fake, bounded local lexical, Ollama, OpenAI-compatible, and Voyage embedding adapters | Shipped |

Authenticated hosted providers are opt-in adapters. Shoal does not configure,
select, or imply any cloud model provider by default; applications must supply
an explicit endpoint, model names, and request-time credential source.
Production local deployments should use a real embedding model through Ollama
or a loopback OpenAI-compatible server; the local lexical embedder is only a
zero-dependency offline and CI fallback, produces lexical/statistical vectors,
and makes no semantic quality claim. Anthropic publishes no embeddings API, so
hosted embedding paths are OpenAI-compatible endpoints or Voyage.

## Knowledge and inference contracts

| Capability | Status |
|---|---|
| Hierarchical document, graph, retrieval, and ontology contracts | Shipped |
| Provider-neutral grounded inference context, evidence, claim, result, and provenance contracts | Shipped |
| Bounded provider-neutral tool-using agent harness contract and deterministic fake | Shipped |
| Bounded model-guided Explorer retrieval, section, and graph expansion loop with inspectable trace | Shipped |
| Concrete Copilot/SDK agent execution backend | Not implemented |
| Other hosted runtime LLM orchestration backends | Not implemented |
| Canonical bounded Explorer context construction, citation/path revalidation, and explicit section/neighbor expansion | Shipped |
| Optional snapshot-pinned Explorer web workspace with cited retrieval and bounded graph/path exploration | Shipped |
| Ontology-guided structured extraction, validation, stable inferred IDs, and publication planning | Shipped |
| Atomic publication of extraction plans through Explorer | Not implemented |

The inference surface and context builder validate document-only, graph-only,
and mixed evidence without introducing storage rows, RPC messages, raw
prompts, credentials, or provider SDK types. The extraction orchestrator
builds deterministic prompts only from an ontology snapshot and exact evidence
anchors, rejects malformed or out-of-schema output, and returns proposed
publication contracts without writing graph rows. See
[`docs/inference-contracts.md`](docs/inference-contracts.md) and
[`docs/ontology-extraction.md`](docs/ontology-extraction.md).

## Accumulo replacement roles

| Role or capability | Status |
|---|---|
| Stateful scan and multi-scan sessions, continuation, expiry, and close | Shipped |
| Fail-closed Accumulo 4 iterator capability registry | Shipped |
| Tablet ServiceLock, manager RPCs, metadata/config/file/WAL loading | Shipped |
| Tablet ingest routing, fenced WAL authority, recovery, and minor compaction | Shipped |
| `shoal-tserver` process, readiness, metrics, TLS, drain, and deployment | Code complete; live validation pending |
| External compaction executor and `shoal-compactor` worker | Code complete; live validation pending |
| Offline full major compaction | Shipped |
| Local-to-Accumulo promotion, split reconciliation, and cutover state | Partial — durable state and FATE integration are shipped; public engine/manager authority and cutover adapters remain |

Replacement-role live evidence is tracked by
[issue #74](https://github.com/phrocker/shoal-oss/issues/74). The checked-in
verdict runner reports `unsupported`, not `pass`, when Docker or a required
live adapter is unavailable.

## Client APIs and Sharkbite compatibility

| Surface | Status |
|---|---|
| Go Accumulo 4 client, topology, scanner, writer, and administration | Shipped |
| Stable C ABI with capability negotiation and owned lifetimes | Shipped |
| `shoal-sharkbite` wheel/sdist with `sharkbite` and `pysharkbite` imports | Shipped |
| Python scanner, batch scanner, mutation, writer, and administration APIs | Shipped |
| Python RFile and HDFS APIs | Shipped |
| Structured errors, logging, deadlines, retries, GIL, and fork-safety contracts | Shipped |
| Windows shared/static ABI and wheel artifacts | Shipped |
| Linux manylinux distribution | Shipped |
| Native macOS dylib/wheel runtime and Mach-O verification | Optional/deferred by @phrocker on 2026-08-20 |
| Controlled Linux manylinux release workflow | Shipped and main-verified in run [32493148857](https://github.com/phrocker/shoal-oss/actions/runs/32493148857); macOS publication is optional |

The normative matrix is
[`docs/sharkbite-compatibility.md`](docs/sharkbite-compatibility.md), with
scope defined by
[`docs/sharkbite-client-scope.md`](docs/sharkbite-client-scope.md). Revision
56 satisfies 390 of 394 required rows. The four remaining rows require live
Accumulo 4 evidence:

- `SB-SCAN-022`, `SB-SCAN-026`, `SB-XCUT-015`,
  `SB-XCUT-016`.

Native macOS publication/runtime rows `SB-PKG-008`, `SB-XCUT-014`, and
`SB-XCUT-019` remain visible but are optional/deferred under the named,
dated owner decision.

## Operations and conformance

| Capability | Status |
|---|---|
| Semantic readiness, metrics, alerts, TLS/mTLS, and bounded drain | Shipped |
| Kubernetes/Helm manifests, PDBs, secure contexts, and rollout policy | Shipped |
| Exact Accumulo 4 ZooKeeper/HDFS/Accumulo Docker harness | Shipped |
| Deterministic replay verdicts with source hashes | Shipped |
| Live tserver, scanserver, client, compactor, and promotion verdict adapters | Shipped |
| Live Docker execution and retained verdict/log artifacts | External validation pending |

Run the static and live gates from a Linux Docker host:

```bash
make test-accumulo-static
make test-accumulo
make conformance-replay
make conformance-live
```

See [`test/accumulo/README.md`](test/accumulo/README.md) for prerequisites,
expected evidence, debugging, and cleanup commands.

## Detailed documentation

- [`ARCHITECTURE.md`](ARCHITECTURE.md): coordination and serving architecture
- [`docs/tserver-hosting-lifecycle.md`](docs/tserver-hosting-lifecycle.md):
  tablet-server lifecycle and fencing
- [`docs/tablet-ingest-service.md`](docs/tablet-ingest-service.md): ingest,
  WAL, and minor-compaction authority
- [`docs/write-tier-operations.md`](docs/write-tier-operations.md):
  production operations
- [`docs/promotion.md`](docs/promotion.md): promotion and cutover
- [`docs/offline-compaction.md`](docs/offline-compaction.md): offline
  compaction runbook
- [`docs/inference-contracts.md`](docs/inference-contracts.md):
  provider-neutral grounded inference contracts and current runtime status
- [`docs/ontology-extraction.md`](docs/ontology-extraction.md):
  structured ontology extraction and safe publication boundary
- [`python/README.md`](python/README.md): Python install and API usage
- [`test/accumulo/README.md`](test/accumulo/README.md): exact Accumulo 4
  conformance harness
