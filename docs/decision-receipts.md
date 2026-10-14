# Durable decision reservations and receipts

`internal/decisionstore` is the first storage slice of #403. It persists one
request reservation and at most one committed prediction under a scoped
idempotency key. It uses the existing `allocator` exact-read/row-CAS interface;
`internal/explorercoord.EngineStore` supplies the tested embedded implementation.
The composition owner creates the dedicated `_shoal_decision_receipts` table and
uses the engine's full WAL synchronization mode for acknowledged-write durability.
Writes use the WAL/memtable and flush through the configured engine table format
(default RFile, optionally Parquet). The receipt table is control state, separate
from source snapshots and Mosaic's
persisted record format.

This is an internal trusted store, not an authenticated public service. The caller
must obtain scope from authenticated context, resolve the pinned request and
release, retain authorized source/request artifacts, and apply current object and
source-derived access checks before every call. Domain/principal key partitioning
is not a replacement for those checks. The API must never accept `Scope` or lease
claims from arbitrary caller-supplied identity fields. Shared deployments must
bind any actor/client/delegation restrictions into the trusted scope and enforce
them in the service.

## State and recovery

1. `Reserve` binds domain, principal and idempotency key to the exact validated
   request, returning a pending receipt plus a private claim to one owner. A
   different request at the same key conflicts. The request's principal must
   match the trusted scope.
2. The owner performs inference outside any storage transaction. Pending retries
   return the receipt without its claim token; they must not start another call.
3. `Commit` validates the typed response, requires the live token and version,
   and atomically records the prediction. Invalid output does not consume the
   claim. Committed retries return the original receipt; a different result can
   never replace it, even if nondeterministic inference could have produced it.
4. An expired pending lease can be reclaimed with a new version and random token
   while the request deadline remains live. Old workers cannot commit. Repeated
   inference after a crash is possible; exactly one result can commit. This is
   not an exactly-once execution guarantee.
5. If a mutation's acknowledgement is lost, the store checks exact durable bytes.
   A verified match succeeds; otherwise it returns `ErrIndeterminate`, never a
   claim that the write rolled back. After restart, the caller reads/reserves the
   same key to discover pending or committed state. Unknown ownership never
   authorizes a model call.

An expired request with an abandoned lease remains visibly pending; the store
cannot infer whether work occurred. Recovery does not silently renew its deadline
or invent a negative result. A still-live lease can record a timeout/failure after
the request deadline. New successful/abstained commits are rejected after the
request deadline even when they claim an earlier completion time; exact committed
replays remain available. Completion cannot predate the active claim. Reservation
time is sampled after reading storage, and a write that returns after expiry
leaves its pending record visible without returning an executable claim. Commit
expiry is checked before the conditional mutation; storage latency is not an
atomic wall-clock predicate, while version fencing remains atomic. Full service-level
outcome reconciliation remains a follow-up. Lease time uses the trusted store
clock; the host must provide an appropriate clock and handle skew across workers.

## Record and retention boundary

Schema-v1 records bind receipt/request/task/picture/predictor/prediction identities,
version, lifecycle times and the original typed result. A bounded canonical JSON
envelope includes a checksum; reads reject corruption, unexpected schema/fields,
coordinate or version substitution, and responses that fail validation against
the exact supplied request. Checksums detect corruption, not a malicious storage
administrator. The claim token is persisted internally but omitted from receipt
reads and pending replays.

Receipts reference retained artifacts rather than embedding raw source or prompts.
`Get` requires the exact validated request to revalidate the stored result; this
slice is not an artifact repository or a lookup-by-public-receipt-ID service.
The future service must make retention and authorized rehydration durable before
claiming end-to-end replay. Storage visibility is configured by the composition
owner; deriving visibility from all evidence and outcomes is service work.

## Validation and remaining integration

Tests use the real embedded engine, including abrupt subprocess exit without
`Engine.Close` for both pending and committed records. They cover competing
reservations/results, normal reopen, lost acknowledgements, unavailable
reconciliation reads, stale-worker fencing, domain separation, conflicting
requests/results, malformed outputs, late failures and corrupt records. Explicit
RFile and Parquet cases verify physical flush files, reopen pending state, reclaim
leases, fence old workers, and replay committed results after another flush/reopen.
Latency tests cover reads/writes crossing deadlines and lease expiry; completion
cannot precede a reclaimed lease.

Run:

```sh
go test -race ./internal/decisionstore ./pkg/decision ./internal/explorercoord
go vet ./internal/decisionstore
```

The [authenticated service composition](decision-service.md) now exercises this
store behind trusted resolver/artifact/provider interfaces. Production artifact
retention and provider installation remain pending.

#403 remains open for attributed append-only
outcomes and their authorization/adjudication rules, durable artifact retention,
and lifecycle/audit integration. #418 will wire registered providers and HTTP/SDK
operations through that service. No model is invoked and no existing experiment,
source snapshot, or serving policy changes in this slice.
