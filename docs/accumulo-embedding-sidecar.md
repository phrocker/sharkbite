# Accumulo embedding sidecar boundary

Accumulo 4 rejects `file.embedding` in both root and metadata-table tablet
metadata. The known-state guard in `metadatacas.Authority.Commit` and the
write-mode backfill refusal remain active. This package establishes an
immutable sidecar record format and reconciliation rule; it does **not** yet
authorize known-state publication or backfill.

A v1 sidecar object is addressed by a SHA-256 hash of the exact table ID,
tablet extent, raw `file` qualifier and value, decoded file path, and SHA-256
digest of the RFile embedding footer. Its record carries the publishing owner
fence, embedding state, and source-evidence kind. `CreateExclusive` prevents a
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
and operator's `--trust-no-embeddings` provenance. No production verifier is
installed, so `Resolve` currently returns unknown by default. Once that
verifier exists, a validated scan may classify a historic file whose footer
is unknown. Any contradictory known footer refuses promotion, and a
footer-only record always requires exact state agreement.

The remaining publication protocol must use a shared durable sidecar backend
and a current fence. It should publish and verify immutable RFile bytes and
footer, create the exact sidecar object, then conditionally commit the native
`file` entry under Accumulo's owner fence. After any ambiguous answer it must
re-read both stores. A file entry without matching sidecar evidence remains
unknown; an orphan sidecar without a live file entry grants nothing. Backfill
must verify a live file and source evidence, create the sidecar, and re-read
the exact live metadata entry; split and replacement races may only produce
unknown. The sidecar and metadata stores do not share an atomic transaction,
so the implementation must prove this ordering, replay, and fail-closed
behavior before removing the existing guard. The CLI backfill path also needs
an authorization protocol distinct from a hosted tablet's owner fence; this
foundation does not grant a CLI writer that authority.

The pinned Accumulo live gate must then exercise known-state minor compaction,
write-mode backfill, reassignment, split, and GC, and separately remain red
for its unresolved Shoal WAL recovery gap until hard-kill replay passes.
