# Accumulo RFile-direct compatibility investigation

Issue [#173](https://github.com/phrocker/sharkbite/issues/173) asks for a separate commit mode that never adds a Shoal WAL reference to Accumulo's native `log` family. This document records the current boundary and the proof still needed. The mode is not implemented or enabled.

## Existing commit path

`walauthority.Tablet.Commit` installs the `log` reference before appending the operation. `hostedingest.Tablet` assigns timestamps in memory, then writes through that WAL and flushes with a fresh `minc-UUID` operation ID. `mincauthority.Coordinator` requires a nonempty set of covered WAL references and a sequence boundary; it publishes and reads back an immutable RFile before a fenced metadata CAS adds `file` and removes exactly those `log` references. Its local checkpoint is resumable by the same owner, but it is not an operation ledger across reassignment.

The metadata CAS implementation previously accepted a `MetadataCommit` with zero `RemoveWALs`. That could add a `file` entry without the source proof required by its WAL-backed contract. It now rejects that request before issuing a conditional metadata mutation. This guard does not provide a direct commit mode.

## Why a file entry alone is insufficient

An immutable object written before the metadata CAS can be orphaned by a crash. A lost CAS response needs an authoritative outcome that distinguishes that orphan from a committed write. The new `file` entry can establish the immediate outcome while it exists, but Accumulo compaction replaces file entries. Split distributes overlapping files to child tablets and does not carry an arbitrary Shoal operation marker: in the exact Accumulo 4 source revision pinned by `test/accumulo/image/Dockerfile` (`1a716b2c1bb5762ead4b46d2bc4f53e13873b314`), `server/manager/src/main/java/org/apache/accumulo/manager/tableOps/split/UpdateTablets.java` explicitly copies selected recognized fields and files when creating children. An unrecognized tablet metadata column is therefore not a proven permanent operation ledger. The current `minc-UUID` path also does not bind a client operation ID, its source payload, and its assigned timestamp decision across an owner change.

Before a direct write can be acknowledged, the design needs an operation outcome that survives object-store response loss, metadata CAS response loss, tserver reassignment, compaction, split/merge, and table deletion. It must bind a stable operation ID, source mutation digest, assigned timestamps, target extent/lineage, and exact RFile outcome. A retry with the same ID and different source must conflict. A committed outcome must be discoverable after the original `file` entry disappears. No current tablet metadata field or local checkpoint has been shown to meet that full contract, so neither can authorize a direct write today.

## Candidate protocol tests before live wiring

The next implementation should have fault-injection tests at each boundary: intent/identity persistence, immutable RFile publication and readback, fenced `file` CAS, and acknowledgment. A crash before CAS may leave an orphan but must not expose or acknowledge rows. A lost CAS response must reconcile against authoritative outcome evidence; unresolved evidence remains ambiguous. Reassignment must preserve the source/timestamp decision and reject a conflicting retry. Compaction, split/merge, and deletion must preserve or deliberately retire the operation outcome under a proved retention rule. Only after those tests and the pinned Java hard-kill verdict pass should the live gate change from WAL recovery to verified durability.
