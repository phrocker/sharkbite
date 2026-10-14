# Durable decision artifacts

`internal/decisionartifacts.Catalog` implements the artifact interface consumed by
`internal/decisionservice`. `Retain` admits an already registered request;
`LoadAuthorized` reconstructs its original immutable contracts and exact model
input after restart. This is the retention component of #403, not a public request
registration API or a production provenance authority.

## What is retained

A schema-v1 row contains task, picture, predictor, request, evidence policy and
ranking configurations; the complete context pack (document quotes, graph paths,
assertion references, ontology identity, snapshot/auth pins and metadata); the task
resource mapping; the exact serialized model input; and complete bytes for every
source observation. Constructors rebuild and verify content-derived identities.
An immutable `PictureManifest.ContextPack()` accessor exposes the already pinned
pack without changing its identity or any existing persisted receipt format.

Every picture source requires exactly one retained source entry. The source digest
must be SHA-256 of those complete bytes; the input must match `InputDigest`.
Document quotes must equal the referenced byte range in each associated source,
including sources used by subjects outside the requested scoring subset. Graph
structure and assertion identities round-trip, but their authenticity requires
trusted verification. Referenced external inventories, rubrics, policy definitions,
model weights and builder implementations still require their own retained registry
artifacts; this row preserves their pins, not those external implementations.

Aggregate source bytes are capped at 8 MiB, input at the existing context-pack
bound, and the encoded row at 48 MiB. Rows use canonical JSON, a schema version and
a SHA-256 checksum; decoding rejects unknown fields, duplicate/noncanonical
encodings, inactive variants, checksum failures and substituted identities. The
checksum detects corruption; it is not a signature or an authority credential.

## Authority on admission and reads

Construction requires an auth resolver and a trusted `Authority`. There is no
allow-all default. The authority first checks current access to the registered
request before the catalog inspects its row. It then verifies every contributing
source, anchor and outcome, including current registrations, role/control/origin,
attestations, snapshot and measurement provenance, the pinned serialization builder,
and task/release/policy bindings. Joint-disclosure restrictions must be enforced
by this integration as well. Source-authored labels cannot implement it.

The resolver must revalidate current authority. The catalog checks the supplied
service decision against the resolver's current projection, derives its storage
namespace from domain/principal/request, and rechecks authority around persistence
and reconstruction. Admission also requires task invocation permission and the
request's original authorization fingerprint. Historical loads may use a changed
projection if the current authority approves all original evidence; the service
still separately enforces its read/invoke operation.

Missing and denied requests both return not-found. Storage/authority outages remain
availability failures. Expiry and cancellation are rechecked after blocking
verification. Authority callbacks receive a separate decoded copy, so mutable
verification arguments cannot rewrite the stored or returned input. These are
repeated current checks, not an atomic transaction with an external ACL system.

## Persistence and retries

Hosts create `_shoal_decision_artifacts` and bind an `explorercoord.EngineStore`.
Insert-only row CAS retains one exact record per domain/principal/request; exact
retries succeed and conflicting resource mappings cannot overwrite an existing
row. An uncertain acknowledgement is reconciled only by reading back identical
bytes at the expected coordinate/version. Unresolved writes return indeterminate,
never a claim that persistence rolled back. Revocation during a write can leave a
retained row while withholding success from the caller.

The normal engine path is WAL/memtable and RFile flush; Parquet is also tested.
Tests cover abrupt subprocess exit without `Engine.Close`, both immutable formats,
concurrent retries, lost acknowledgements, and service replay after engine restart
with one total predictor invocation.

## Internal adversarial review

The pre-publication review examined poisoning, disclosure, replay and failure
boundaries. It led to an expiry check after registry latency and an association
index to avoid rescanning the entire subject inventory for each document anchor.
Regression tests exercise:

- Changed role/control claims with valid hashes: the independent test registry
  rejects the substituted provenance; byte checks alone do not approve it.
- Missing source bytes, mismatched digests/quotes/input, unknown or duplicate JSON
  fields, inactive variants and rechecksummed request substitutions: rejected.
- Forged caller projections, missing/revoked requests, corrupted rows hidden behind
  denied access, and revocation/expiry during verification: no payload returned.
- Verifier argument mutation and conflicting resource mappings: original bytes
  remain intact.
- Lost write acknowledgements and revocation during commit: durable state remains
  observable only through later authorized recovery.
- Graph paths, ontology versions and assertion origins: exact identity survives
  serialization; this test does not establish authoritative graph provenance.

The initial pre-publication pass was a self-review. Three subsequent independent
reviewer-agent rounds were run on 2026-10-06, with fixes and regression checks
between rounds:

| Round | Reviewed code | Result | Fix |
|---|---|---|---|
| 1 | `023a8e3` | P2: conflicting/corrupt versus absent storage errors bypassed post-I/O authorization and disclosed state after revocation. | `daf43a9`: reauthorize before exposing storage-dependent errors; test both existing and absent rows. |
| 2 | `daf43a9` | P2: that reauthorization could hide indeterminate persistence behind cancellation/deadline or authority availability errors. | `4f8d232`: preserve `ErrIndeterminate` and the cause for unresolved writes; confirmed denial still masks state. |
| 3 | `4f8d232` | No actionable findings in a fresh review of the full change and both fixes. | No implementation changes required. |

The round-2 regression test failed before the fix for cancellation, deadline and
authority outage, then passed after it; each case confirms the bytes actually
persisted and can be recovered through a later authorized load. The full targeted
race suite and vet checks passed on `4f8d232`. Reviewers used static analysis and
local tests; these rounds do not verify a production authority or constitute
exhaustive fuzzing, model attestation or a new classifier-quality experiment. No
Copilot review or paid classifier inference was requested.

Validation:

```sh
go test -race ./internal/decisionartifacts ./internal/decisionservice ./internal/decisionstore ./pkg/decision ./pkg/inference ./pkg/explorer/auth
go vet ./internal/decisionartifacts ./internal/decisionservice ./pkg/decision
```

The production task/evidence authority and request-registration path remain to be
wired. Tests use an independent, fixed test registry and a fake predictor. A local
model adapter, attributed outcomes/adjudication, lifecycle/audit and HTTP/SDK wiring
also remain outstanding. The catalog neither executes a model nor promotes source
claims into training labels.
