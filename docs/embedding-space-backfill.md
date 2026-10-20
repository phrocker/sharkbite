# Embedding-space backfill

`shoalctl embedding-backfill` can inspect files and report which embedding
states a migration would need to establish. **Write mode is disabled for
Accumulo-authoritative metadata.** Accumulo 4 rejects Shoal's custom
`file.embedding` column family when its manager, garbage collector, or client
parses tablet metadata, including the root tablet's ZooKeeper record. Writing
that family could make the cluster unable to locate tablets.

The command's default `--dry-run=true` remains available. It reads existing
metadata and RFile footers, then reports files that are already labelled,
resolvable from a known footer, unresolvable, or changed during inspection. A
dry run makes no metadata mutations and does not certify that a migration has
completed.

```bash
shoalctl embedding-backfill --zk zk1:2181,zk2:2181 --instance accumulo \
    --table 2k --storage gs --user root --dry-run=true
```

Supply the password through `SHOAL_PASSWORD`. With `--dry-run=false`, the
command fails before any write with
`metadatacas: known embedding state requires an Accumulo-compatible sidecar`.
There is no supported flag to bypass this guard. Keep files whose state is
unknown in the fail-closed path; do not treat a footer or an old
`no_embeddings` column as permission to inject a custom metadata family.

The same guard applies to hosted minor-compaction publication. An undeclared
or explicitly `unknown` embedding state leaves the Shoal annotation absent
while committing Accumulo's standard `file` entry. A declared
`has_embeddings:<identity>` or `no_embeddings` state is rejected before the
metadata mutation until a compatible sidecar can durably bind it to the file.
The RFile footer can still carry the declared state, but the Accumulo-backed
path cannot claim a complete per-file metadata snapshot from it alone.
An in-flight minor-compaction checkpoint carrying a known state will also
reject on replay; an operator must keep it pending until the sidecar path is
available rather than silently relabel or discard it.

Before a write-mode migration can be restored, the sidecar must preserve the
file qualifier, state, source footer evidence, and a fenced generation; it must
update atomically or reconcile safely with file-entry publication and removal.
The reader must verify the sidecar against the current `file` entry and treat
missing, stale, or conflicting records as unknown. Only then can the backfill
write known states without changing Accumulo's tablet metadata format.
