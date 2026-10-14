# Independent adjudication boundary

`pkg/decision` defines a versioned label policy and an immutable adjudication
proposal. These contracts connect a task's label policy reference to registered
role and training-purpose identities, and bind a proposed label or dispute to an
exact prediction, subject, question, outcome receipts, and evidence references.

**Constructing a proposal does not verify a label.** Its disposition is what the
caller requests. The contracts contain no authenticated adjudicator, server
receipt time, or training-eligibility flag. They cannot be substituted for an
authorized adjudication receipt in a dataset producer.

## What the contracts validate

A task pins the exact label policy identity. Policy ownership can differ from
task ownership so a trusted registry can approve shared policies; a matching hash
does not itself authorize use of the policy. The policy identifies adjudication,
dispute-resolution, and training-purpose authorities and a minimum number of
independent witnesses. Distinct witness IDs are only a structural prerequisite:
copied reports or evidence with common provenance do not establish independence.

A requested verified disposition carries the task's typed label or proposition
truth and witness references. Disputed and unresolved dispositions carry a
reason rather than a label. An unresolved proposal may explicitly have no
witnesses; it must not become a negative label. Outcome references identify
retained receipts rather than unreceived assertions.

Canonical identities include the full proposal, including its expected journal
head and version. Input slices and pointers are copied and bounded. A separate
target identity binds **task, picture, subject, and question**. Changing the
request, prediction, report subset, or adjudicator must not create a separate
target and hide a conflicting adjudication over the same picture.

The bounded `AdjudicationTargetID` and `AdjudicationProposalID` helpers let storage
check canonical metadata identities without duplicating their encoding. A
metadata hash is not a validated proposal: readers must reconstruct with
`NewAdjudicationProposal` and trusted policy/prediction records before accepting
task-specific labels, witness requirements, or any authority claim.

## Required service behavior

The following service work remains open under #403, #405, and #419. The contracts
do not provide it implicitly.

1. Authenticate the adjudicator and resolve the registered task and label policy.
   Check the policy's role and purpose using server-owned authority. A caller
   cannot assert role membership or policy permission in the proposal.
2. Resolve original durable outcome receipts and prediction through an explicitly
   authorized adjudication path. Keep ordinary principal-scoped receipt reads
   unchanged. Check current access to every contributing source, outcome, and
   witness, including combined disclosure restrictions.
3. Derive reporter and source-controller identities from trusted registrations
   and authenticated receipts. Enforce separation from the adjudicator across
   subject, actor, and delegation identities. A different client ID, an asserted
   reviewer name, or another model invocation does not establish independence.
4. Verify witnesses and common provenance under the pinned policy. Model
   agreement, execution success, repeated submissions, and report counts are
   insufficient to verify correctness. Missing provenance or unresolved control
   relationships must prevent verified-label admission.
5. Append a service-stamped adjudication to a durable target journal using
   expected-head CAS. Preserve history and disputes. Concurrent corrections
   cannot both become the current label; resolving a dispute requires the
   registered dispute-resolution authority.
6. Recheck authorization after storage and preserve commit uncertainty. Never
   disclose a record after access has been revoked or manufacture a rollback
   from a failed acknowledgment.

The original proposed-outcome store permits branching corrections to preserve
conflicting observations. An adjudication journal must select and retain an
explicit current disposition; it must not silently adopt the most recent report.

## Training eligibility follows adjudication

An authorized exporter must derive eligibility from the complete trusted target
journal, rather than a caller-selected subset of reports or a `verified` input
field. It must retain the exact label/policy versions, evidence lineage, original
observation receipt times, and adjudication receipt time. All required material
must have been available by the registered evaluation cutoff. Client-asserted
observation times cannot backdate availability.

Current withdrawal and permission checks still apply to historical snapshots.
A later dispute must preserve the historical record while quarantining its
current use pending resolution. Temporal and family isolation, duplicate-origin
accounting, and protected evaluation remain dataset/promotion requirements.
Deleting a receipt does not unlearn model weights.

No production role resolver, adjudication journal, eligibility exporter, public
adjudication API, or model promotion is implemented by this contract slice. The
existing source-code and review-guidance experiments retain their protocols and
full-review fallback.
