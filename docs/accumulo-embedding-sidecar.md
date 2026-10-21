# Accumulo embedding sidecar boundary

Accumulo 4 rejects `file.embedding` in both root and metadata-table tablet
metadata. The metadata authority now has a tested, injectable hosted
publication path for `has_embeddings`, but the tserver does not configure it.
All production known-state commits and write-mode backfill retain their
fail-closed guards until pinned live Accumulo evidence is green. This is an
incremental #171 path, not its full acceptance.

A v1 sidecar object is addressed by a SHA-256 hash of the exact table ID,
tablet extent, raw `file` qualifier and value, decoded file path, and SHA-256
digest of the RFile embedding footer. Its record carries the publishing owner
fence, embedding state, and source-evidence kind. Hosted records additionally
bind the full RFile SHA-256 and size: a replacement that preserves the native
file value and footer cannot inherit the old state. `CreateExclusive` prevents a
second writer from replacing different bytes at that address. A lost create
answer is accepted only when a fresh read finds the exact expected bytes.
The record may outlive the publishing owner: after reassignment, the same live
file can still use its evidence. If a split changes the extent or qualifier,
if replacement changes the file value or footer, or if GC removes the file,
resolution returns unknown. Orphan cleanup is maintenance, never a safety
condition. An orphan that conflicts with a later owner's record currently
blocks that write; a future reconciler must resolve it with durable commit
evidence instead of overwriting it.

No-embeddings requires scan-verified evidence with a versioned attestation
digest. A footer assertion alone is insufficient because the old write path
could fabricate `no_embeddings` in both metadata and footer. The sidecar's
`scan_verified` value and digest are only claims until a production
`PublicationVerifier` checks the attestation bytes, exact footer, live file,
and operator's `--trust-no-embeddings` provenance. The injectable hosted path
only installs a footer/full-file verifier for `has_embeddings`; negative
records still resolve unknown. Once a complete scan verifier exists, a
validated scan may classify a historic file whose footer is unknown. Any
contradictory known footer refuses promotion, and a
footer-only record always requires exact state agreement.

The RFile evidence reader hashes the exact compressed embedding meta-block
representation; an absent block has a separate digest and decodes as unknown.
For a scan attestation, it also streams SHA-256 over the complete RFile in
fixed-size reads with cancellation. The footer, full-file hash, and scanner
receive one opened source snapshot, so a same-path replacement cannot combine
evidence from different object versions. `RFileEvidenceReader` requires the
storage `SnapshotOpener` capability and never falls back to ordinary `Open`.
The attestation record is bounded,
canonical, signed under the authority namespace, and binds the exact native
file identity, full RFile digest/size, footer digest, scan algorithm/config,
locality-group coverage, and observed cell counts. Lost create answers need
exact durable readback; copied, changed, or unverifiable objects remain unknown.

No production scanner or operator-authorization verifier is installed. Current
RFile iteration covers the default locality group only, so even a signed
attestation claiming a complete scan and a valid operator approval cannot
promote `no_embeddings`. A future negative proof must scan every locality
group under a complete, versioned vector schema and verify durable operator
authorization bytes. The signed record format reserves an authorization
digest for that protocol; the current write and resolve paths refuse negative
attestations. Full-file hashing costs one streamed read per verification until
an immutable-object digest cache is introduced with equivalent provenance.

**Hosted activation requires source snapshot stability.** GCS snapshot
reads pin an object generation; S3 and Azure pin a version or enforce the ETag
seen at open on every range request. A missing token or unsupported backend,
including the path-keyed disk cache, fails closed. Rooted storage forwards
snapshot reads only when its inner backend supports them. An ETag is a
consistency token, not a content digest; the attestation still hashes the
complete RFile. These reads need deployment-specific replacement testing.
The native HDFS client now pins block locations before accepting an open and
checks the namenode file ID, size, and modification time around each range
read. Custom HDFS client adapters and unsupported metadata fail closed. The
pinned Accumulo live harness must still exercise this path before activation.

The bounded publication-marker protocol requires a candidate record, an
owner-fenced read of the exact native `file` entry, verified RFile footer, and
verified scan attestation when applicable. It then creates and reads back an
immutable marker containing the candidate digest, exact file identity, owner
fence, operation ID, and attestation reference. A domain-separated keyed MAC
or signature binds those fields and a key ID; verification must use a
constant-time comparison and retain historical keys through rotation. Keys
must be scoped to one metadata authority/cluster namespace. An unauthenticated
precreated marker can deny publication but cannot promote known state. A
missing authenticator fails closed. A second owner-fenced native read, footer
check, and fence check precede success. A lost marker-create answer is accepted
only after a canonical, authenticated readback of the same candidate, operation,
and namespace; an
old valid key may be accepted across rotation. Resolution reads those bytes and
independently rechecks the footer and scan attestation; marker
existence alone grants no state. Split, replacement, GC, stale owner, and
missing or contradictory attestation refuse acknowledgement or promotion.

The metadata authority now exposes `ReadOwned` and `Verify` for this protocol.
They require exact byte-level tablet bounds, the current Accumulo location
and ServiceLock, and the original process-local assignment generation. They
check that generation before and after the native read, so an unload and
reassignment under the same ServiceLock cannot authorize an old publisher.
The hosted publisher uses this reader before and after marker creation.

`LoadHMACKeyring` supplies a file-backed, namespace-scoped MAC for markers and
attestations. The secret file must be a regular owner-only file (mode 0400 or
0600), at most 64 KiB, with this shape:

```json
{
  "version": 1,
  "namespace": "<Accumulo authority namespace>",
  "current": "key-2026-10",
  "keys": [
    {"id": "key-2026-10", "material_base64": "<base64 of at least 32 random bytes>"}
  ]
}
```

The active key signs new records; retained older keys verify existing records.
Rotate by installing a new owner-only file and restarting, retaining old keys
until their records have been retired. The namespace is part of the MAC input,
so copying a key file to another authority cannot validate its signatures.
The tserver and CLI do not load it yet. A future deployment configuration
must use the Accumulo instance ID as namespace and retain old keys during
rotation.

The marker's operation ID is asserted by the trusted caller. Native Accumulo
`file` metadata has no operation ID to compare with it, so the hosted path
requires the output file name to contain the SHA-256 of the durable operation
ID. It verifies the complete source checksum and footer before native CAS,
requires an absent native file entry, then rereads the exact file and retired
WAL references before publishing. It returns `CommitApplied` only after marker
readback; a lost native response or marker failure can be retried with the same
operation ID. A native file entry with no valid marker remains unknown; an
orphan candidate or marker without the exact live file grants nothing. The
two durable stores do not share an atomic transaction, so hard-kill replay,
split and GC still need pinned live evidence before #171 is complete. Backfill
must verify a live file and source evidence,
create the sidecar, and re-read the exact live metadata entry; split and
replacement races may only produce unknown. The CLI backfill path also needs
an authorization protocol distinct from a hosted tablet's owner fence; this
hosted path does not grant a CLI writer that authority.

The pinned Accumulo live gate must then exercise known-state minor compaction,
write-mode backfill, reassignment, split, and GC, and separately remain red
for its unresolved Shoal WAL recovery gap until hard-kill replay passes.

## Customer UI path

The explorer web UI does not yet read Accumulo sidecar evidence. Before a
customer-facing rollout, its operator view needs a table and file inventory
that distinguishes verified `has_embeddings`, verified `no_embeddings`, and
unknown, shows why a file is unknown, and presents a dry-run backfill report.
It must obtain those statuses from the same verified resolver used by hosted
reads; the browser must not infer state from a footer or sidecar object alone.
Write controls can appear only after the CLI backfill authority and its
authorization protocol are complete, with progress and ambiguous outcomes
visible to the operator. The present tserver flags are an operator deployment
setting, not a customer UI switch.
