# Retained adjudication evidence basis

An adjudication basis records what a collector claimed was available for a
particular proposal at a cutoff. It binds the proposal to an authority revision,
an enumeration identity, the complete claimed outcome inventory, witness
provenance, source controllers, role evidence, and capture time. Its immutable
identity includes all of that content.

`decision.NewAdjudicationBasis` validates structure, not authority. A caller can
assert `InventoryComplete`, `ControllersComplete`, and witness verification
references; constructing or storing the basis does not make those assertions
true. The authenticated adjudication service must obtain them through a trusted
collector and check current access and policy. No training eligibility is
conferred by this contract or its retention store.

## Snapshot contents

The outcome receipt set and witness set must exactly match the proposal. Outcomes
must concern the same task, picture, subject, and correctness question. Different
requests and predictions may contribute to that shared target. Execution success
is not a correctness label. Correction ancestry must be present, acyclic, bound to
the same request, prediction and reporter identity, and no deeper than the durable
outcome store's 32-ancestor limit. Branches are preserved.

Witnesses retain a content digest, verification receipt, origin identity,
controller identities, and common-provenance group. Several witness IDs may share
one provenance group; their separate IDs do not establish independence. Subject,
actor, client and delegation identities preserve opaque authentication bytes.
Delegation order is preserved. A later independence check must compare subject,
actor and delegation relationships; changing a client alone is insufficient.

Outcome receipt times and witness receipt/verification times cannot exceed the
cutoff; capture cannot precede it. These times remain assertions until matched to
trusted receipts. They describe availability for this captured snapshot, not an
atomic promise that no new report arrived before a later journal commit. Current
withdrawal, disputes and access checks still apply when the snapshot is used.

Inputs are bounded before copying, references and identity sets are canonical,
and getters return independent copies. The maximum manifest budget is 4 MiB.
The constructor rejects incomplete inventory claims; a verified proposal also
requires a controller-completeness claim. Unresolved or disputed proposals may
retain unknown controllers without converting uncertainty into a negative label.

## Persistence and service ordering

`internal/decisionbasisstore` is an internal trusted-caller persistence boundary.
It retains immutable, domain-scoped snapshots with canonical encoding and content
identity checks. It has no resolver and cannot authorize a domain or a read.
A caller loading a basis supplies the exact proposal, reconstructed from trusted
policy and prediction records. A checksum detects corruption; it is not a defense
against a malicious storage administrator able to replace authoritative data.

The service must retain the basis successfully **before** appending its ID to the
adjudication journal. A failed journal append can leave an unused retained basis;
a committed receipt must not refer to an unretained basis. An exact retry must
recover its original journal receipt and basis before collecting newer evidence.
A new inventory must never silently alter an existing idempotent operation.

After all journal, basis and authority reads, the service must check current
permission and joint disclosure for every input of the complete returned history.
No further source-loading operation may occur after that final check. Unknown
write acknowledgment must retain commit uncertainty until an exact readback
reconciles it. Engine ownership and fencing requirements continue to apply.

Authenticated collection and adjudication, authorized dataset export, temporal
and family isolation, poisoning evaluation, and promotion remain follow-on work.
The frozen source-code experiment and its full-review fallback are unchanged.
