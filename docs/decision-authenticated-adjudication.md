# Authenticated adjudication service

`internal/decisionadjudication` composes an authenticated caller, an explicit
trusted evidence authority, retained basis snapshots, and the durable target
journal. `Adjudicate` returns an attributed receipt; `History` returns a complete
currently authorized history. Neither result grants dataset eligibility or model
promotion. There is no default authority or public HTTP mounting in this slice.

## Authority integration

The host must supply a capability-scoped `auth.Resolver` and implement all of
`Authority`. This interface is a security boundary, not a convenience adapter for
accepting fields from a request or a source document.

- `Resolve` authenticates the exact registered prediction and pinned label policy
  through an explicitly authorized adjudication path. Ordinary principal-scoped
  decision and outcome reads remain unchanged.
- `AuthorizeTarget` enforces current target and operation permission before
  journal or basis reads. It must not use a caller-asserted grant.
- `Capture` obtains the complete target outcome inventory and witness/controller
  provenance at a cutoff through trusted enumeration and verification. It records
  the authority revision, role evidence and original server availability times.
- `Verify` authenticates all retained assertions, checks historical admission
  evidence as of its cutoff, and enforces current access to every original input
  and their joint disclosure. For a candidate it checks the policy-derived current
  role requirements and that witnesses support the exact proposed label or truth.
  It finishes all source loading before its final authorization check.

The test authority is only a fixture. A production authority must implement
registered cross-principal access, witness verification and complete enumeration;
a structurally valid basis or a `verified` proposal is not sufficient.

## Local admission checks

The service reconstructs every proposal against trusted policy and prediction
records and binds its target, basis, receipt version and availability times.
Adjudicators must be separate from outcome reporters, source controllers, and
witness origins/controllers across subject, actor and delegation identities.
Changing only the client does not establish independence.

For a verified proposal, witnesses connected by common origin, byte-identical
content, or intersecting origin/controller identities form one component.
Components connected to reporters or source controllers do not count. The number
of remaining components must meet the pinned policy's minimum. This handles
transitive links; two apparently separate witnesses connected through a third
controller are one component. These conservative local checks supplement trusted
verification; they cannot prove the absence of an unrecorded control relationship.
Execution success, model agreement and repeated reports do not verify correctness.

Every write requires the adjudicator role. Every verified transition after the
first journal entry additionally requires the dispute-resolver role, even if an
intervening unresolved entry would otherwise hide an earlier dispute. Disputed
and unresolved states remain distinct from negative labels.

## Retry, persistence and authorization ordering

The service loads and hydrates the complete history before searching for the
idempotent receipt. An exact retry returns its original basis, attribution and
receipt time before attempting new collection, even when newer evidence exists
or the collector is unavailable. Reuse with another proposal conflicts only after
current authorization of the history.

For a new operation, the service captures and validates a basis and retains it
before journal append. Failed appends may leave unused retained bases. The journal
uses expected-head/version CAS; competing corrections cannot both take the same
head. The pure journal `ReceiptID` helper and guarded append preserve the existing
persisted encoding and receipt derivation.

`AppendChecked` supplies a detached copy of the exact history read internally for
its CAS. The service hydrates that history and jointly rechecks permission after
those reads, immediately before CAS. After append/readback it loads and verifies
the returned history again. Only caller fingerprint, expiry, and cancellation
checks follow the final joint authority check. Storage latency cannot silently
reuse an authorization check performed before the storage read.

This is revalidation before mutation and disclosure, not an atomic transaction
between an external authorization system and the engine. A strict atomic
revocation/admission guarantee needs a shared authority fence. The captured
inventory is complete at its retained cutoff, not promised unchanged until CAS.

If an append may have committed and final storage, source authorization or caller
resolution fails, the result remains `ErrIndeterminate` with no receipt disclosure.
Restore authorized access and retry the same key and proposal to reconcile it.
A denied guard performs no journal mutation. Basis-retention failure happens before
journal append and cannot create an adjudication receipt.

History never returns a partial prefix. Missing/inaccessible material and invalid
bindings fail closed; cumulative hydrated basis content is limited to 16 MiB in
addition to the journal's entry/encoding limits. An empty history is opaque
NotFound. A later authorized export must separately quarantine new disputes or
unadjudicated reports and enforce temporal/family isolation, current withdrawal,
training-purpose permission and evaluation policy.
