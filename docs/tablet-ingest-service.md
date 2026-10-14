# Tablet ingest service integration

`internal/ingestservice` implements the Accumulo 4
`TabletIngestClientService` update-session surface over
`internal/ingestrouter`. It decodes Accumulo's compact `TMutation` format,
authenticates sessions, checks table WRITE permission per extent, preserves
partial success, and returns stale/retry, authorization, and constraint
failures through `UpdateErrors`.

Supported durability requests are `DEFAULT`, `SYNC`, `FLUSH`, and `LOG`.
The hosted WAL authority currently satisfies all four with the stronger
append-and-sync contract. `NONE` is rejected explicitly. Conditional mutation
sessions are also rejected explicitly with `UNSUPPORTED_OPERATION`; the
service never returns a success-shaped result for semantics it does not
implement.

`internal/hostedingest` composes a manager assignment attempt with:

- exact ServiceLock, manager-lock, and assignment fencing;
- referenced-WAL recovery before a tablet is published;
- deterministic server timestamps before WAL append;
- idempotent memtable application;
- threshold/manual minor compaction through `internal/mincauthority`;
- serialized commit, flush, drain, and unload.

`tserverprocess.NewWritableStore` publishes a tablet to scans and ingest only
after that composition opens successfully. `tserverprocess.Services` registers
and advertises `TABLET_INGEST` only when a live ingest service is supplied and
accepting. `cmd/shoal-tserver` now supplies a production metadata authority:
root-tablet changes use ZooKeeper version CAS, while metadata and user-tablet
rows use Accumulo conditional mutations over exact location, lock, previous-row,
WAL, and file columns. Unknown transport outcomes are reconciled by rereading
authoritative state and retries are idempotent. Durable checkpoints discover
and resume incomplete minor compactions after restart; ownership release
flushes, retires WAL references, and conditionally removes the exact owner.
Capability advertisement additionally requires the explicit `-enable-ingest`
release gate; it defaults off until the live mixed-fleet evidence tracked by
#74 is accepted.

General client conditional-mutation sessions remain unsupported and fail
explicitly. The internal metadata CAS path is supported when the backing root
or metadata tablet is hosted by an Accumulo implementation that provides
conditional updates. Shoal therefore refuses root and metadata-table tablet
assignments instead of advertising unsupported self-hosted conditional
semantics; production mixed fleets keep those system tablets on Java tservers.

Live unmodified-Java-client evidence remains part of issue #74 and requires
Docker or another Accumulo 4 cluster environment.
