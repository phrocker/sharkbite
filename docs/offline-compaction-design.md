<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# Offline compaction of Accumulo-hosted tablets — design

Status: **implemented with a deprecated direct-commit seam**. Orchestrator,
OFFLINE fence, plan generation, verification, and the
`shoal-offline-compact` CLI have landed (todos `oc-*`). The accepted
[coordination authority contract](./coordination-authority.md) supersedes the
earlier direct-metadata decision: release use is limited to Mode P until an
Accumulo manager/FATE commit API exists. Operators should start with the runbook:
[`offline-compaction.md`](./offline-compaction.md).

## 1. Goal

Let shoal perform a **major compaction of a tablet whose table is
offline** from a standalone Go binary — no tserver or compaction coordinator
in the compaction work — and emit a guarded commit plan for
Accumulo-authoritative tooling to apply.

This is the offline counterpart to the online `shoal-compactor`
(`cmd/shoal-compactor`), which today connects to the coordinator but
**stops at the metadata-commit boundary** because a non-manager process
has no write authority over `accumulo.metadata` / `accumulo.root`.
Taking a table offline removes concurrent tablet writers, not that authority
boundary.

## 2. Why offline is required but not sufficient

The online commit problem is a **concurrency + authority** problem: while
a tablet is hosted, its tserver is the single writer of that tablet's
file set, and only the manager (via Ample + the metadata-constraint
iterators) may mutate `file:` refs. Bypassing that risks the exact
"authority bleeding" wedge called out in the `shoal-compactor` file
header.

Taking the table **OFFLINE removes the concurrent writer entirely**:

- No tserver hosts any tablet of an offline table, so nothing else is
  reading or rewriting the tablet's RFiles or its metadata row.
- The tablet's `file:` column set is quiescent — it cannot change under
  us between the moment we read it and the moment we commit.
- Accumulo's own bulk-import / clone / export tooling relies on the same
  offline invariant, so we are reusing a well-understood safety envelope
  rather than inventing one.

That invariant makes off-cluster compaction and commit-plan generation safe,
but it does **not** grant metadata-write authority to a non-manager process.
The OFFLINE fence must be paired with the manager/coordinator/FATE commit
boundary required by the coordination authority contract.

## 3. Safety model

### 3.1 The OFFLINE fence

The table state lives in ZooKeeper (`/accumulo/<iid>/tables/<id>/state`,
value `OFFLINE` / `ONLINE`). The orchestrator:

1. **Fenced read**: read the state znode **and set the watch in the same
   ZK call** (`getData(path, watch=true)`), capturing both the value and
   the znode `Stat` (specifically `stat.version` and the read's `zxid`).
   Abort unless the value is exactly `OFFLINE`. Registering the watch in
   the same call as the read closes the classic watch race — a state
   flip between a separate read and a later watch registration cannot
   slip through, because there is no gap between them.
2. **Session-expiry is a fence failure**: if the ZK session expires (or
   disconnects long enough to lose the watch) at any point during the
   run, the operation aborts before commit (fail-closed). A lost session
   means we can no longer prove continuity, so we treat it as a
   definite fence trip rather than assuming OFFLINE held.
3. **Re-check at commit**: immediately before the metadata mutation,
   re-read the state znode and require **both** (a) value still exactly
   `OFFLINE`, and (b) `stat.version` **unchanged** from the fenced read
   in step 1. The version check is the real guard — ZK watches are
   one-shot and "the watch never fired" alone does not prove the absence
   of an intermediate ONLINE→OFFLINE round-trip; an unchanged
   `stat.version` does (any state write bumps the version).

This is a compare-and-swap in spirit: the CAS guard is the tuple
(value==`OFFLINE`, `stat.version` unchanged, session never lost) from the
fenced read through commit. If anyone onlines the table mid-run — even
transiently and back — the version advances and we throw the work away
rather than commit against a possibly-hosted tablet.

> We deliberately do **not** try to compact online tables by racing the
> tserver. If the operator wants online compaction, that is the separate
> coordinator-driven path and requires the Java-side `CompactionCommit`
> RPC (out of scope here).

### 3.2 Full-major semantics

Offline compaction is always a **full major compaction**: all of a
tablet's input RFiles are merged into exactly one output RFile, and the
output is the tablet's sole remaining file. This is the only mode where
`DeletingIterator` may legally drop tombstones (see `compaction.Spec.
FullMajorCompaction` and `iterrt` `DeletingIterator` Init contract), and
it keeps the metadata mutation trivial: replace the entire `file:` set
with one entry.

Partial/selection compactions (merge a subset) are a later extension;
they complicate delete propagation and are not needed for the primary
use case (shrink a tablet's file count / apply a stack offline).

### 3.3 GC-safe input dereference

We never delete input RFiles from the volume directly. The commit
removes the input `file:` refs from the tablet's metadata row; Accumulo's
garbage collector reclaims the now-unreferenced files on its normal
cycle. This matches how the manager's own commit path behaves and avoids
a shoal-side file-deletion authority claim.

### 3.4 Dry-run by default

The CLI defaults to **dry-run**: it performs the full read + compaction +
verification and prints the exact metadata mutation it *would* apply
(input refs to delete, output ref to insert), then exits without
touching metadata. Any output and plan are inspection-only because their
unreferenced RFiles may be reclaimed by Accumulo GC. The current
`--dry-run=false` path enters the legacy commit seams and is not
release-approved. Once the manager/coordinator/FATE application path exists,
the compaction must be rerun and applied while its outputs are still protected.

### 3.5 Legacy rollback behavior and replacement

The output RFile is written under a fresh, unique name (`A<uuid>.rf`
convention) before the legacy metadata mutation. Existing tests retain this
behavior:

- If we crash **before** the mutation: the orphan output file is
  unreferenced and GC reclaims it. Inputs are untouched. No-op.
- If we crash **during/after** the mutation: the mutation is a single
  conditional batch (see §4.3). It either landed atomically or it did
  not; on restart the orchestrator re-reads the tablet's file set and
  is idempotent (if it already sees the single output file, it treats
  the tablet as done).

This describes the deprecated test seam, not a release commit contract. The
replacement manager/coordinator/FATE operation must own the linearization,
authority-token verification, output protection, and recovery record.

## 4. Architecture

Reuses existing, already-tested components; the new code is orchestration
+ commit + verification glue.

```
metadata.Walker ──► enumerate tablets + file: sets (per table/range)
        │
        ▼
shadow/itercfg  ──► resolve table.iterator.majc.* stack  ─┐
        │                                                  │ (must have
internal/storage ─► fetch each input RFile's bytes         │  full shoal
        │                                                  │  coverage —
        ▼                                                  │  else abort:
internal/compaction.Compact(Spec{Inputs, Stack, Majc,      │  see §6)
        FullMajorCompaction:true}) ─► output RFile bytes ◄─┘
        │
        ▼
internal/storage ─► write A<uuid>.rf + fsync
        │
        ▼
internal/offlinecompact/verify ─► self-consistency + shadow oracle
        │
        ▼
internal/offlinecompact/commit ─► OFFLINE fence re-check + inspection plan
```

### 4.1 `internal/offlinecompact` (new package)

Pure-ish orchestrator. Given `(tableID, optional row range, options)`:

1. Fence pre-check (§3.1).
2. `Walker` enumerate target tablets.
3. For each tablet: resolve majc stack (itercfg), fetch inputs
   (storage), `compaction.Compact`, write output.
4. Verify (§5).
5. Fence re-check, then emit the inspection-only plan. The package retains
   legacy commit seams for regression tests, but the shipped CLI does not call
   them.

Kept free of I/O primitives it does not own — it composes `metadata`,
`storage`, `itercfg`, `compaction`, and `zk` rather than reimplementing
them.

### 4.2 Output file naming

Match Accumulo's convention so the file is indistinguishable from a
Java-produced one: full major compaction outputs are named `A<base>.rf`
in the tablet's directory (`<volume>/tables/<id>/<tabletDir>/`). We
generate `<base>` from a UUID to avoid collisions.

### 4.3 The metadata commit

**Mode P (plan, conservative, default):** emit a machine-readable commit
plan (per tablet: extent, input refs to delete, output ref + size +
entry count to insert). Plan generation is the only release-approved behavior
today. Earlier suggestions to apply it with a shell script or standalone Ample
writer are superseded. When a supported manager/coordinator/FATE operation
exists, rerun compaction so it can verify the logical-table epoch and operation
attempt while protecting and applying fresh outputs.

**Mode D (direct, deprecated and prohibited for release):** the implementation
seam can issue a conditional `accumulo.metadata` mutation, but that bypasses
manager/coordinator/FATE authority and therefore conflicts with the accepted
coordination contract. The seam remains only for existing tests and migration;
operators must not wire or use `--commit-mode=direct`. Re-enabling a direct
commit requires replacing it with a supported manager/FATE operation carrying
the logical-table epoch and operation attempt.

## 5. Verification (`oc-verify`)

There is no Java "expected output" for a fresh offline compaction (Java
never ran it), so we cannot use the shadow oracle's T2/T3 cross-diff
directly. Instead we layer three independent checks:

1. **Self-consistency (primary gate):** independently re-scan the *input*
   RFiles through the *same* iterator stack in a second pipeline and
   assert the emitted cell sequence is byte-identical to the output
   RFile's cell sequence. This catches writer bugs, block-boundary
   corruption, and stack non-determinism.
2. **Shadow oracle, shoal-only mode:** run `shadow.Compare` with
   `JavaCellsSourced=false` to get T1 (self-read: the output RFile is
   re-openable and fully scannable) + T4 (per-file summary sanity:
   cell count, CF histogram, first/last key).
3. **Optional Java cross-read:** if `SHOAL_JAVA_RFILE_VALIDATE` is
   configured, run the real Java RFile reader over shoal's output to
   prove Java can consume it (the T1 tier's external validator). This is
   the strongest single assurance that the offline output is a
   first-class Accumulo RFile.

Verification runs in dry-run too, so operators can gain confidence before
ever committing.

## 6. Iterator-coverage precondition (link to Workstream B)

Offline compaction can only run a tablet whose entire majc stack is
covered by shoal iterators. `itercfg.ResolvedStack.Skipped` lists any
`table.iterator.majc.*` class with no shoal port. If `Skipped` is
non-empty the orchestrator **aborts that tablet** (never silently drops
an iterator — that would corrupt semantics).

Closing that gap is exactly what the **iterator forge** (Workstream B,
`if-*`, see `iterator-forge-design.md`) does: it ports the missing Java
iterators to Go and widens `itercfg.ClassAllowlist`. The two workstreams
are complementary — A is usable today for allowlisted stacks; B expands
the set of tables A can service.

## 7. CLI surface (`cmd/shoal-offline-compact`)

```
shoal-offline-compact \
  --zk <quorum> --instance <name> \
  --table <name|id> [--range <startRow>:<endRow>] \
  [--dry-run=true]              # default true: plan+verify, no commit.
                               #   false is rejected before external work.
  [--commit-mode=plan]         # direct is rejected before external work.
  [--verify=true]              # default: run §5 checks
  [--out <dir>]                # where to write the commit plan (Mode P)
```

Exit non-zero if: table not OFFLINE, any target tablet has a Skipped
iterator, verification fails, or the fence trips.

## 8. Testing (`oc-e2e`)

Two layers:

**Hermetic legacy-seam test (implemented, runs every build).**
`internal/offlinecompact/e2e_test.go` wires the existing
Run → verify → Commit → apply → "online + full scan" implementation cycle
for regression coverage against an
in-process model of the pieces a cluster provides (RFile store, tablet
enumerator, majc-stack resolver, an `accumulo.metadata` model with a
conditional applier, and the **real** fence continuity predicates
`requireOffline` + `verifyContinuity` driven off a mutable table-state).
It covers: Mode D full cycle, the Mode P plan JSON hand-off to an
out-of-band applier, the ONLINE-round-trip version-guard trip at commit,
ONLINE-at-fence refusal, and an unported-iterator abort. This is the gate
that a full-scan of the committed output equals the original data
(post-versioning) and each tablet collapses to a single `file:` entry; it does
not make those legacy commit seams release-approved.

**Cluster-backed test (blocked on the authority API).**
Once the manager/coordinator/FATE commit operation exists, test against a
Mini/real Accumulo instance:

1. Ingest known data, flush + compact a few times to create multiple
   RFiles per tablet.
2. Offline the table.
3. Run the compaction and generate its plan.
4. Apply the plan through the manager/coordinator/FATE operation while
   verifying current authority and protecting the output RFiles.
5. Verify the operation's terminal result, then online the table.
6. Full scan; assert every original cell is present and correct, and the
   tablet now has a single `file:` entry.
7. Negative tests: table left ONLINE ⇒ abort; a table with an unported
   iterator ⇒ abort with the Skipped class named.

## 9. Decisions

- **D-1 (commit mechanism):** **SUPERSEDED — Mode P only for release.**
  Shoal keeps metadata-write authority inside Accumulo. The legacy
  `MetadataCommitter` seam and `--commit-mode=direct` path are deprecated and
  must not be wired; a future direct commit must be a supported
  manager/coordinator/FATE operation carrying current authority proof.
- **D-2 (range compaction):** **DECIDED — range compaction is
  supported.** `--range` selects whole tablets that intersect the given
  row range (tablet granularity, never sub-tablet); an empty `--range`
  compacts the whole table. See `SelectTablets`.
