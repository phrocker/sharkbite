# Ingest routing core

`internal/ingestrouter` is the validation, session, idempotency, and routing
core used by `internal/ingestservice`.

The boundary accepts table-scoped mutation batches, validates every batch
before contacting a tablet, enforces extent membership, visibility syntax,
timestamp policy, and configured byte/count limits, then routes each extent to
a `Directory`. A resolved `HostedTablet` carries an opaque hosting `Fence`;
the tablet implementation must re-check that fence atomically with commit.

Requests have stable idempotency keys. A duplicate key with different bytes is
rejected. A duplicate identical request preserves applied/rejected extents and
retries only retryable extents. Results are per extent, so stale assignments,
replacement extents, cancellation, unknown outcomes, and partial commits are
never collapsed into a success-shaped response.

## Service boundary

The Thrift update-session adapter, compact mutation decoder, hosted WAL,
memtable, recovery, minor-compaction coordinator, writable store, and
authorization/error mapping now compose through `internal/ingestservice` and
`internal/hostedingest`.

`HostedTablet.Authority()` must report `AuthorityAccumuloWAL` before the router
will acknowledge a commit. The zero/default `AuthorityUnsupported` returns
`ErrWALAuthorityUnsupported`; there is no local-WAL, in-memory, or
success-shaped fallback. The production command constructs the Accumulo conditional metadata authority
before the writable tablet store and, with the explicit `-enable-ingest`
release gate, advertises `TABLET_INGEST` only when that complete composition is
initialized; see
[tablet ingest service integration](tablet-ingest-service.md).
