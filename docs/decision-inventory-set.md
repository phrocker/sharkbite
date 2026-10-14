# Consistent cuts of registered target inventories

`internal/decisioninventoryset.Reader.CaptureSet` collects a bounded set of
already registered target inventories. It performs no writes, registration,
admission, authorization, source loading, adjudication or automatic retry.
The caller supplies a trusted domain and exact coverage/target bindings and must
authorize access before invoking this internal primitive or disclosing its output.

Construct it with `Config{Store: inventoryStore, Clock: serverClock}`. Both
capabilities are required. Call `CaptureSet(ctx, scope, bindings)` with 1–256
unique targets. Bindings are validated and sorted by target ID; conflicting or
repeated targets are rejected instead of silently deduplicated.

## Protocol and guarantee

The reader loads every first-pass snapshot, then starts a separate second pass.
Every first-pass and second-pass snapshot must be complete, and the second pass
must exactly match each target's binding, snapshot ID and monotonically increasing
version. A pending intent blocks capture regardless of its receipt timestamps.
A Begin followed by Publish cannot hide behind the return to a complete state:
both operations advance the inventory version and change its identity.

For each target, an equal pair proves that target did not change between its two
reads. Because the entire first pass finishes before any second-pass read begins,
all those unchanged intervals overlap. The returned inventories therefore existed
together at a common point between the first pass finishing and the second pass
starting. The proof requires:

- Linearizable row reads/CAS and the engine's required writer ownership/fencing.
- No rollback, deletion/recreation, or version reset of registered inventories.
- Every writer inside the operator-asserted coverage barrier uses guarded admission.
- Exact immutable target and coverage bindings throughout both passes.

Registration asserts coverage; this reader cannot discover legacy or bypassed
outcome writes. Its guarantee covers only the registered inventories. Source
permissions, training-purpose grants, adjudication heads and withdrawals maintained
outside those inventories are separate authority state. No global transaction or
atomicity over that additional state is implied.

A writer may change an inventory after its second-pass read. The returned cut
remains a valid historical cut; it is not a promise of freshness through response
delivery. Pending admissions that arise after the established cut need not appear
in that historical cut. Future reads cannot treat its old completeness as current.

## Identity, measurements and bounds

`Capture.VectorID` hashes a versioned domain plus canonically ordered tuples of
full binding, snapshot ID and version. The domain is encoded as bytes, preserving
opaque identities. Measurement times do not enter the vector identity, so an
unchanged vector has the same ID across repeated captures and physical restart.
`Capture.Snapshots` contains the matching first-pass inventories, not merely IDs.

The window records server-clock `StartedAt`, `FirstPassFinishedAt`,
`SecondPassStartedAt` and `CompletedAt`. Times are normalized to UTC and must be
valid and nondecreasing. These report collection timing, not source observation
freshness or a reconstructed historical observation time. Consistency follows
from operation ordering and monotonic versions, not clock precision.

The first-pass retained-material budget is 32 MiB. Accounting includes snapshot
and entry backing capacities, binding and receipt strings, and opaque reporter
and delegation bytes. Count and payload sizes are checked without serializing
unbounded objects. One individually bounded `Store.Load` result is accounted for
before retaining it or proceeding to another load. Transient decoding allocations
and the one-at-a-time second-pass snapshot are additional; this budget is not a
whole-process RSS guarantee.

A successful capture performs exactly two scans. Changed versions, pending
intents, missing/corrupt records, byte/count bounds, cancellation and invalid or
backward clock values return no partial capture. The reader never spins until
writers become quiet. A host may apply its separately bounded retry policy.

## Use by a dataset authority

After loading its exact prediction/source/label material, an authority can capture
all relevant target inventories and require each loaded target's inventory ID to
match its captured snapshot. It must then perform its final joint source and
training-purpose permission check, without further source/storage loading after
that final authorization decision. Adjudication heads and other mutable inputs
need their own checks; this primitive does not supply them.

Tests cover two-pass ordering, changed complete inventories, pending admissions,
canonical vector IDs, capture-window timing, bounds and cancellation, and physical
RFile reopen with stable vector identity and non-expiring pending intents. A
mutation after the established cut is explicitly tested to prevent accidental
claims of delivery-time freshness. The change makes no model-quality or training
eligibility claim.
