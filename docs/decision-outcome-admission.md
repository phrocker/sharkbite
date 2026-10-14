# Coordinated outcome admission

`decisionoutcomes.Config.Admission` is an optional internal hook for a trusted
inventory coordinator. Legacy callers without a hook retain their existing
receipt format and behavior; they assert no inventory coverage. A production
host may claim coverage only when every writer in that namespace uses the same
coordinator and existing outcomes have been reconciled explicitly.

The hook receives the actual resolved caller, canonical prediction and outcome,
and derived receipt ID. Caller-asserted reporter provenance never substitutes
for the authenticated identity. An exact existing receipt is fully authorized
before any hook runs. Conflicting requests do not create pending intents.

For new writes, the store performs validation and authorization, invokes Begin,
reauthorizes after its IO, then attempts the outcome CAS. After an exact authorized
receipt readback, Publish receives the original receipt, including original
attribution and receipt time. Authorization is checked again after Publish.
Ordinary Read never invokes either hook.

Begin must durably record a pending intent before returning success. Publish must
reconcile the exact original receipt into that inventory. Hooks must be safe for
concurrent retries. They must not expire or abort pending intents by interpreting
an absent read as proof of rollback. The receipt's ReceivedAt may predate Begin;
inventory visibility needs its own publication time and generation.

Any error from the first Begin invocation onward includes ErrIndeterminate. This
means coordinated admission may be incomplete: it does not prove the outcome
committed, and it does not promise rollback. Retrying the same key and canonical
observation can repair a pending publication. A raced conflicting observation
may leave a pending intent requiring explicit trusted reconciliation; this hook
provides no automatic abort. No artifacts are returned on failure.

The interface itself supplies no durable inventory or authority implementation.
The persistence primitive and authenticated inventory adapter are separate layers.
