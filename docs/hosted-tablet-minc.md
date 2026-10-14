# Hosted-tablet minor-compaction authority

`internal/mincauthority` is the fenced commit coordinator for the
Accumulo-authoritative output of a hosted tablet's minor compaction. It builds
only native RFiles. Parquet remains a derived/export format and is never used
as authoritative minor-compaction output.

The hosted-tablet adapter must implement `Snapshotter.Prepare` as one atomic
memtable/WAL transition:

1. freeze the active memtable at a sequence boundary;
2. sync and roll the WAL;
3. retain the immutable cells and exact metadata WAL qualifiers;
4. return the same snapshot for the same operation after restart;
5. direct later writes to a new memtable and WAL.

`walauthority.Tablet.SealForMinorCompaction` provides the WAL half of this
transition and invokes the adapter's memtable-swap callback while ingest
commits are excluded.

The coordinator then durably checkpoints and resumes these phases:

1. **snapshotted** — stable snapshot identity, boundary, cell fingerprint, and
   covered WAL references recorded;
2. **published** — a deterministic `.rf` object durably closed;
3. **validated** — SHA-256, BCFile/RFile structure, ordering, entry count, and
   tablet extent/key range verified by reading the published object;
4. **committed** — one fenced conditional metadata mutation added that exact
   RFile and removed only the covered WAL qualifiers;
5. **complete** — the retained memtable generation can be released and covered
   WAL bytes can be retired.

Every external action is idempotent. Lost publish and metadata responses are
reconciled from the durable object or authoritative metadata. Metadata states
that show only half of the atomic change fail closed. Unrelated files and WALs
are preserved. The metadata implementation must condition the mutation on the
live ServiceLock, manager generation, assignment attempt, and exact covered WAL
references; a preflight check alone is not authority.

`HostedOwnerVerifier` uses `tserver.Host.VerifyHosted`, so loading, unloading,
forced-unloaded, lock-lost, and replacement attempts cannot start or reach a
metadata commit as the old owner. A CAS that races an unload must still reject
inside the metadata implementation.

`internal/hostedingest` now implements the hosted-tablet snapshot adapter and
connects it to the update-session service and writable store.
`cmd/shoal-tserver` initializes this path with `internal/metadatacas` and
advertises it only after the conditional metadata, WAL, loader, and
minor-compaction authorities are ready.
