# Embedding-space backfill

`shoalctl embedding-backfill` records an explicit `file.embedding` column
for tablet files that carry none. It is the migration path for issue
#274.

## Why it is needed

`internal/embeddingspace` models three states per file:
`has_embeddings:<id>`, `no_embeddings`, and `unknown`. `unknown` is
absorbing — it refuses to merge — because absence of information must
never be mistaken for information about absence.

Before #274, several layers filled in `no_embeddings` when they simply
had no data. `no_embeddings` merges freely with every space, so a file
carrying foreign vectors could be merged into an output labelled with
the target space. Those layers now report `unknown`, and only an
explicitly decoded column or footer produces `no_embeddings`.

The operator-visible consequence is that files written before the
`file.embedding` column existed now read as `unknown`. Compaction and
convergence fail closed on them until their state is recorded once.

## What it does

For each file entry in a table:

| Situation | Outcome |
| --- | --- |
| The entry already has a definite `has_embeddings` column | left alone, counted as *already labelled* |
| The entry already has a `no_embeddings` column | see below; without `--trust-no-embeddings` it is listed as *unresolvable* and never rewritten |
| The footer declares `has_embeddings:<identity>` | that state is written to `file.embedding`, counted as *resolved* |
| The footer declares `no_embeddings` | written only with `--trust-no-embeddings`; otherwise listed as *unresolvable* — see below |
| The footer is absent, unreadable, or explicitly `unknown` | left `unknown` and listed individually as *unresolvable* |
| The entry changed mid-run | not written, listed individually as *raced*; re-run picks it up |

An entry whose column is explicitly `unknown` is treated exactly like one
with no column: neither establishes anything, and both are repaired by
the same run.

The authority is the file's own footer meta block
(`embeddingspace.RFileMetaBlockName`), read through
`internal/rfile`. It travels with the file and was written by whoever
produced it. Nothing else is consulted: the backfill never inspects cell
values to guess whether something looks like a vector, because a guess
written into durable metadata is the exact failure #274 removes.

A file whose footer does not establish a known state cannot be resolved
from metadata alone, so it is reported by name with the reason rather
than guessed at.

### Legacy `no_embeddings` footers and columns

The minor-compaction path fixed by #274 fabricated `no_embeddings` and
stamped it into both the RFile footer and the `file.embedding` column.
On a file written by that code neither is independent evidence — each is
the same unfounded claim one layer down, and copying the footer into
`file.embedding`, or accepting an existing `no_embeddings` column as
established, would make the bug's own output durable while reporting the
file migrated.

The backfill will not do that on its own. A `no_embeddings` footer, and
an existing `no_embeddings` column, are each left unresolvable, named,
with the reason given, unless the operator passes
`--trust-no-embeddings`, which is an explicit assertion
that this table's ingest pipeline really did emit no vectors — the same
assertion `--default-embedding` makes going forward.

An existing `no_embeddings` column is reported but never rewritten: the
backfill only ever adds a column where none is established, so an
operator who disagrees with a stored value must correct it deliberately
rather than have a migration silently overwrite it.

`has_embeddings` is trusted unconditionally, in both the footer and the
column: no version of the writer ever invented an identity.

## Running it

```
shoalctl embedding-backfill --zk zk1:2181,zk2:2181 --instance accumulo \
    --table 2k --storage gs --user root
```

It defaults to `--dry-run=true`, which reports the size and shape of the
migration without writing anything. Pass `--dry-run=false` to apply.
Supply the password through `SHOAL_PASSWORD` rather than `--password`.

The command exits non-zero when the run left anything outstanding —
unresolvable files, raced files, or a dry run that would have written
columns. A dry run is never reported as a completed migration unless
there was nothing to write, so the default mode cannot be mistaken for
evidence that a table needs no migration.

## Safety and idempotence

Every write is conditional on:

* the tablet's `~tab:~pr` being unchanged, so the row still describes the
  tablet that was examined and has not split;
* the `file:` entry still holding exactly the bytes that were read, so
  the file has not been replaced by a compaction; and
* the `file.embedding` column still holding exactly what it held when the
  file was examined — absent, or an explicit `unknown` — so a concurrent
  writer that established the state first wins. An established column is
  never replaced.

A second run therefore writes nothing and reports every file as already
labelled. The run is safe to interrupt and safe to repeat.

The root tablet is refused (`metadatacas.ErrRootBackfillUnsupported`):
its metadata lives in ZooKeeper behind a different mutation path owned by
whichever server hosts it. A table id that locates no tablets is also
refused, so a typo cannot report a completed zero-file migration.

## Cost

Resolving which metadata tablet holds a given row is a root-tablet scan.
Because the backfill visits every file in a table, doing that per file
would make a large migration cost one routing scan per file. The writer
therefore caches the metadata table's routing for the pass.

The cache is only ever an optimisation. It is dropped whenever a write
fails, is rejected, or returns an ambiguous outcome, and whenever the
cached routing no longer contains the row being written — so a split or a
tablet that moved mid-run costs one extra scan rather than stalling or
misdirecting the remainder of the migration. Correctness never rests on
the cache: the conditional write's own preconditions decide whether a
mutation applies.

Each conditional write is its own session, which is the shape of the
shared `ingestclient` conditional API used by every writer in the tree,
not something the backfill introduces.

## Declaring a default instead

An operator who knows an ingest pipeline emits no vectors can say so
rather than have it inferred. `shoal-tserver --default-embedding` takes
`no_embeddings`, `unknown`, or `has_embeddings:<identity>`, and is the
state recorded for a minor-compaction snapshot that declares none. When
it is unset the recorded state is `unknown`. The value is validated at
startup.

Internally that is `hostedingest.Config.DefaultEmbedding` →
`mincauthority.Config.DefaultEmbedding`. `WriterOptions.EmbeddingSpace`
is honoured as a fallback default for the same reason, and the
coordinator now forces the RFile footer to carry the same state it
records in metadata — previously a configured writer option could reach
the footer while the metadata column said something else, which the next
integrity check rejects.

Changing the flag is safe across a restart. A minor compaction that was
checkpointed and is being resumed reuses the state recorded in its
checkpoint rather than re-deriving it from current configuration, so a
flag change — or the upgrade to this version itself, where every
in-flight checkpoint carries the old implicit `no_embeddings` — cannot
make a resumed operation look changed and leave the tablet unable to
open.

## Diagnosing a refusal

When a compaction refuses because an input's embedding space was never
established, the error wraps
`compaction.ErrEmbeddingBackfillRequired`, names the offending inputs,
and names this command. That distinguishes "nobody ever recorded what
this file holds" from "these files genuinely hold different embedding
spaces" — the second is a real data condition resolved by convergence,
not by a backfill.
