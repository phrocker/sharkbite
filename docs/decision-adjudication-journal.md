# Durable adjudication journal

`internal/decisionadjudicationstore` provides the persistence boundary for a future
authenticated adjudication service. A single bounded journal per authorization
domain and adjudication target retains ordered proposals, attribution, server
receipt times, and expected-head links using the existing engine's atomic row
compare-and-mutate operation. Storage follows the WAL/RFile path.

**This store does not authenticate an adjudicator or verify a label.** Its caller
must derive the domain and attribution from trusted authentication, resolve the
registered label policy, check role and contributor separation, and authorize
every contributing input before and after storage. Direct store access is an
internal trusted capability, not a public registration or read API.

The stored proposal's `verified` disposition remains a requested disposition
until the surrounding service has performed those checks. A stored receipt alone
does not grant training eligibility. The contract and service requirements are
described in [the adjudication boundary](decision-adjudication.md).

Each receipt requires a `BasisID` for the exact retained evidence/authority
snapshot used at admission. The service must durably retain that basis before
appending the journal reference. It must bind the complete target inventory,
verified witness provenance, role/conflict-check evidence and capture cutoff.
The journal validates the reference's structure and immutable binding; it cannot
establish that the referenced artifact is available or authoritative. Retention
and current authorization remain explicit service prerequisites.

## One target, one ordered history

The journal coordinate includes the authorization domain and the canonical
task/picture/subject/question target. It does not split by adjudicator, request,
prediction, report subset, or witness subset. Competing proposals over the same
picture therefore compete for the same head.

Append requires the proposal's expected head ID and version to match the current
history. The engine CAS checks the complete prior journal bytes and version;
only one competing update can advance that head. Each new receipt has the next
version and points to its predecessor. All prior proposals remain retained,
including disputed and unresolved dispositions. The store does not decide who
may resolve a dispute or which label is usable.

Each journal is limited to 128 entries and 8 MiB of encoded data. Reaching either
bound rejects a new append before mutation. There is no silent truncation,
history replacement, or automatic rollover to a new target. A future authorized
lifecycle operation must preserve lineage if a target needs more history.

## Retry and uncertainty

An idempotency key is scoped to the target, authorization domain, and stable
authenticated subject/actor/client/delegation identity. The raw key is not stored.
An exact retry returns its original receipt, attribution and timestamp even when
later entries have advanced the head. Changed content under the same key
conflicts. Changing a grant fingerprint cannot rewrite the original attribution.
Changing the basis under the same key also conflicts. An exact retry must resolve
and reuse the original basis rather than capture a new inventory snapshot.

Append attempts one CAS. An exact readback can reconcile a lost acknowledgment;
an acknowledged rejection is a conflict. If storage or cancellation prevents
confirmation after the mutation attempt, `ErrIndeterminate` preserves uncertainty.
The caller must keep the same key and reconcile, rather than inventing a new
key or treating an error as proof of rollback.

The authenticated service must also preserve uncertainty when authorization
changes after a successful store append. It must withhold inaccessible payloads
and storage-state distinctions. Those access checks cannot be delegated to this
storage package, which intentionally has no resolver or permissive default.

## Record integrity and availability

The versioned checksum encoding preserves opaque authentication IDs as bytes.
Decoding requires canonical bytes, bounded structure, exact target/proposal
metadata identities, ordered versions, predecessor links, unique receipt IDs,
valid attribution and nondecreasing server receipt times. Input and returned
records do not provide an alias to persisted bytes.

Metadata identity checks do not establish task-specific label validity or source
authority. Before disclosing or using history, the service must rehydrate every
proposal using its exact retained prediction and policy through
`decision.NewAdjudicationProposal`, and jointly reauthorize the complete evidence
history after storage reads. Receipt times alone do not establish witness or
label availability; the authority must retain and validate those separately.

The caller must use a backend that supplies the required atomic, durable row-CAS
semantics. Independent embedded-engine processes require the deployment's normal
exclusive ownership/fencing; this package does not invent a cross-process lock.
Checksum validation detects corruption, not a malicious storage administrator
who can rewrite records and checksums.

## Validation and remaining work

Tests cover competing expected-head updates, exact retry after head advancement,
conflicting keys, preserved dispositions, bounds, clock rollback, unknown write
acknowledgments, canonical corruption and rebinding, and physical RFile
flush/reopen with byte-exact authenticated identities.

```sh
go test -race ./internal/decisionadjudicationstore
go vet ./internal/decisionadjudicationstore
```

Authenticated cross-principal resolution, independent adjudicator/witness
verification, dispute-resolution roles, training eligibility/export, and public
API integration remain open. No model is trained or promoted by this journal,
and existing prediction/outcome formats are unchanged.
