# Read-time dynamic ontology

RDO implements issue #279 as a conservative read-time lens over immutable
assertions, including governed evolution and first-party workspace selection.
The source graph remains schema-neutral and ingestion is not rejected merely
because an unrelated ontology does not describe it. Host integrations select a
lens through `auth.DecisionConfig.SelectedOntology`; browser workspace settings select
from the authorized, published ontology catalog.

## Source-evidence matrix

The following matrix records the original backend implementation baseline,
not an inventory of remaining work.

| Prerequisite from #279 | Source evidence at implementation baseline | Result |
|---|---|---|
| Version-qualified assertion meaning | `pkg/ontology/identity.go` defines schema+version identity; `Assertion` stores it; extraction persistence stores both IDs | Already shipped and preserved |
| Unknown identity is absorbing | `ReadOntologyUnder` returns `unresolved` and never substitutes an ambient version | Already shipped and preserved |
| Immutable original assertions | Assertion fields are private, constructors clone inputs, and persisted extraction records retain ontology IDs | Already shipped and preserved |
| Graph read path carries assertions | `Neighborhood.Assertions`, latent projection, extraction persistence, and authorized filtering already exist | Already shipped and preserved |
| Governed lifecycle and durable history | `GovernedProposal.Transition` plus append-only proposal transition records | Already shipped and preserved |
| Typed evolution vocabulary | No morphism type or safety classification existed | Added |
| Proposal-to-morphism linkage | Proposals carried only a proposed version | Added, immutable and persisted |
| Read-time interpretation | Reads could compare versions but could not map them | Added |
| Caller ontology selection | `auth.Decision` had no ontology field | Added |
| Cache isolation | fingerprints/cache keys omitted ontology selection | Added |
| Browser/report surface | neighborhood responses exposed originals only | Added interpretation reports |

## Product defaults

- Assertions and graph observations remain immutable; interpretations are
  separate result values.
- Only morphisms on published proposals participate in a lens.
- Published version transitions remain traversable even when an additive
  release needs no definition mappings.
- Widening is safe and monotonic. Narrowing is unsafe and resolves only when
  the assertion remains valid.
- Rename requires an explicit mapping. Split requires an explicit metadata
  discriminator. Merge is marked lossy while retaining the complete original
  assertion.
- Missing versions, missing paths, ambiguous paths, and unmatched split
  discriminators return explicit `unresolved` interpretations.
- A published transition whose retained definitions changed without supported
  morphisms is excluded from the interpretation path, preserving unresolved
  history instead of silently adopting the changed meaning.
- A decision without `SelectedOntology` preserves the previous no-lens result.

## Caller-held workspace lenses

The embedded web workspace wires durable settings to the live governed catalog.
The browser settings panel lists choices and persists a caller-owned selection:

- `GET /api/v1/workspaces/{workspace_id}/settings/lens` lists the active version
  and retained published ancestors visible to the authenticated caller.
- `PUT` to the same route selects a lens with `expected_revision`,
  `mutation_id`, and `selected_ontology`; it preserves existing operation,
  source, policy, budget, and output restrictions.
- Workspace IDs and ontology schema/version IDs use the API's base64url opaque
  ID codec. Reads carry the selected workspace in `Shoal-Workspace-ID`.
- Workspace settings require their respective read/write authority; ordinary
  reads reauthorize the selected published identity for the actual operation.
  A lens cannot grant access to otherwise unauthorized observations or evidence.
- A workspace pin is monotonic: it cannot be cleared or replaced. A caller
  authorized for multiple versions can pin another owned workspace to a different
  lens. An issuer-pinned ontology cannot be overridden by workspace settings.
- Neighborhoods and paths return unchanged assertions with separate
  `ontology_interpretations`. Extracted path assertions are joined through their
  retained `shoal.graph.edge_id`, not by assuming assertion IDs equal graph edge
  IDs. Derived assertions retain their existing ID-based association.

`TestRDOCallerLensesOverStoredGraph` in
`pkg/explorer/webapi/rdo_acceptance_test.go` exercises actual deterministic skill
extraction, evidence-authorized morphism publication, HTTP settings selection,
and authorized neighborhood/path reads over one persisted graph. It verifies
different interpretations for two callers, no interpretation from unpublished
morphisms, same-caller fingerprint/cache isolation across separately pinned
workspaces, no-lens behavior, and unchanged observations after corpus/settings
restart. Existing ontology model tests cover the asymmetric safety classes,
including narrowing, split discriminators, and lossy merges.

## Integration API

- `ontology.NewOntologyMorphism` creates validated evidence-bearing morphisms.
- `ontology.NewOntologyLensWithTransitions` and `ontology.NewOntologyTransition`
  model governed version edges independently from definition mappings.
- `ontology.NewPublishedCatalog` derives selectable versions from the unique
  reachable published chain. `webapi.EmbeddedService.OntologyCatalog` applies
  the corpus proposal bound before exposing that same catalog to host adapters;
  discovery and active-version replay do not implement separate eligibility
  rules.
- `ontology.NewGovernedProposalWithMorphisms` binds them to the existing
  lifecycle.
- `auth.DecisionConfig.SelectedOntology` selects a schema+version and is part
  of `AuthorizationFingerprint` and `CacheKey`.
- `explorer.OntologyInterpreter.InterpretAssertions` applies published
  morphisms after authorization filtering.
- `authorized.Config.OntologyInterpreter` is the explicit trusted
  interpretation dependency. The authorization wrapper never accepts
  interpretations from its untrusted graph `Base`.
- `authorized.Config.OntologyProposalStore` is the explicit trusted governance
  dependency. Proposal state, evidence references, and canonical citation
  bytes used by authorization are never accepted from the untrusted graph
  `Base`.
- Citation-backed governance evidence may omit its quote. A supplied quote
  must exactly match the cited immutable source bytes; omitting it never
  skips citation validation or source authorization.
- `explorer.OntologyProposalMutationStateProvider` exposes only the active
  ontology and a requested proposal's base identity to ingest-authorized
  mutation preflight. Mutation callers do not receive the governed proposal
  corpus and do not need unrelated read authority.
- `explorer.OntologyActiveStateProvider` derives the global durable active tip
  without proposal bodies. Extraction uses the ingest-authorized mutation
  state, while selectable proposal/catalog views apply read and evidence
  authorization independently.
- `explorer.PublishedOntologyCatalogProvider` exposes the bounded canonical
  published chain without proposal bodies. The authorized implementation
  accepts only workspace-settings read or write authority, checks current
  evidence object policies for every reachable transition, and rechecks the
  policy generation before returning. Raw proposal reads remain protected by
  `OperationRead`.
- `webapi.NeighborhoodResponse.OntologyInterpretations` and
  `webapi.PathResponse.OntologyInterpretations` expose effective and original
  identities, safety-path IDs, and unresolved reasons.
- Proposal morphism drafts reference definitions by `namespace` plus `key`;
  evidence uses the same opaque-ID citation/path codecs as other Explorer APIs.
  Morphism-level metadata in drafts and projections uses the same base64url
  key/value entry codec as evidence metadata, preserving opaque bytes and
  canonical morphism identities. The ontology model still requires canonical
  UTF-8 metadata: invalid byte sequences are rejected, not silently replaced
  by the JSON transport before validation.
- Published proposal transitions form the durable active-version chain.
  Publication rejects stale bases, and `ActiveOntology` replays the chain after
  restart, including a terminal chain of exactly 256 transitions.
- An indeterminate proposal or transition write fail-closes subsequent ontology
  mutations, mutation preflight, proposal reads, and read-time interpretation
  until the corpus is reopened and durable state is replayed.
- Governed proposal evidence is accepted only when the mutation caller is
  authorized for every cited revision and path object. Proposal reads expose
  full evidence only when every referenced object is authorized; mutation
  responses retain opaque evidence IDs without returning evidence contents.
- HTTP transitions enforce their complete response projection bounds under the
  store's write lock before appending the transition. A domain-valid proposal
  that exceeds the HTTP evidence, discriminator, or ontology size limits is
  rejected without changing its state or the active ontology. The lower-level
  transition API retains its domain bounds. Unexpected projection failures
  after a provider reports success carry indeterminate-commit semantics.
- A corpus admits at most 256 durable proposals across all schemas and lifecycle
  states. Admission is checked under the store's write lock before persistence,
  including concurrent requests through different service instances. Identical
  retries remain valid at capacity; new proposals are rejected without changing
  the stored history.
