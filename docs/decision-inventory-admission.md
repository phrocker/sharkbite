# Authenticated inventory admission adapter

`internal/decisioninventoryadmission` connects the outcome admission hook to the
durable target inventory. Construct it with a concrete inventory store and a
mandatory trusted coverage authority, then assign it to
`decisionoutcomes.Config.Admission`. There is no default coverage authority and
no automatic target registration.

The authority resolves the operator-registered coverage binding for the actual
authenticated reporter and canonical prediction/outcome. The adapter derives the
shared target from task, picture, subject and question, and derives reporter
attribution from the trusted auth decision. It rejects substituted bindings.
This adapter supports correctness observations; execution reports require their
own explicit coverage policy.

Begin persists the exact pending intent. Publish binds the full original
immutable outcome receipt. Both recheck the coverage authority after inventory
IO, including failed reads or writes, before returning. The outcome store then
rechecks current source/evidence authorization. Inventory errors do not disclose
unauthorized target details.

Grant refreshes may authorize an exact retry without rewriting the original
receipt fingerprint, attribution or receipt time. Pending state never expires.
Any failure after the outcome store invokes Begin retains coordinated admission
uncertainty. A retry can reconcile the original receipt and publish it without
adding a duplicate outcome or inventing a new receipt time.

This adapter establishes admission ordering for a declared, guarded namespace.
It does not hydrate all reporters' receipts for adjudication, grant training
rights, or supply an atomic multi-target export snapshot. A production authority
must provide those separate capabilities and enforce that all admissions in its
coverage namespace pass through this adapter.
