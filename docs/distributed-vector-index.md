# Distributed IVF-PQ lifecycle

`internal/vectorindex` owns the derived index lifecycle independently of its
physical storage. A generation is an immutable `Snapshot` containing:

- a manifest with generation/parent/lineage, the exact embedding-space
  identity, deterministic codebook version, source and indexed watermarks,
  dimensions, shard layout, counts, and optional benchmark evidence;
- veculo-compatible coarse-centroid and PQ blobs;
- sharded postings carrying document identity/metadata, visibility, timestamp,
  tombstone, cluster, code, and codebook version.

`EncodeSnapshot` maps that state to sorted `Record` values. Their
row/CF/CQ/visibility/timestamp/delete fields map directly to Accumulo keys and
also form stable rows for RFile or Parquet writers. `DecodeSnapshot` permits
the same generation to be replayed in any order. Publication is atomic and
generation-parent checked, so failed or racing builders cannot replace a newer
active generation.

## Lifecycle

- `Build` sorts identifiers, selects a deterministic hash-ranked training
  sample, trains normalized cosine IVF-PQ codebooks, hashes all training
  inputs/configuration into the version, and publishes generation 1.
- `Update` reuses the active codebook and publishes timestamped vector changes
  or tombstones as a new generation.
- `Rebuild` retrains from a supplied authoritative live corpus and records
  lineage.
- `Compact` removes obsolete pre-fence versions while retaining newer versions
  and active-generation metadata.

Readers pin one immutable generation. Accumulo visibility expressions are
evaluated with the public compatibility evaluator. `AS OF` ignores newer
versions and tombstones; latest reads choose the newest authorized version,
with a same-timestamp tombstone winning.

## Query and freshness contract

Callers explicitly select approximate mode and provide `topK`, `nprobe`, and
optional requirements:

- minimum generation;
- minimum indexed watermark;
- source watermark plus maximum permitted lag.

Every build/update record and every query vector carries an embedding-space
identity. Training, updates, and comparisons fail with a typed mismatch even
when two models have the same dimensions. A legacy manifest without an identity
fails closed rather than treating dimension as identity. Recall evidence is
therefore evidence for the manifest's one recorded space, not a global claim.

Exact ShoalQL vectors require `PlanOptions.Vector.EmbeddingSpace`; the
`shoal-sql` CLI exposes the same value as `-embedding-space`. The planner
threads it into the `vectorKNN` iterator as `embeddingSpace`, and the embedded
engine checks every participating RFile/Parquet footer before scanning.
`unknown`, `no_embeddings`, unflushed cells with an unknown or incompatible
declared default, and same-dimension files from a different model all fail
with typed embedding-space errors. Because one raw
vector has only one identity, mixed-space exact scans are rejected rather than
rank-fused or compared by raw score. Callers that need mixed-space retrieval
must supply text to the multi-provider Explorer path so it can embed once per
space.

Embedded-engine writers declare the actual state for future files through
`engine.TableOptions.DefaultEmbedding` or
`Engine.SetTableDefaultEmbedding`. Changing it is refused while any tablet has
unflushed cells, preventing a new setting from relabelling writes accepted
under the old setting. This actual-state setting is deliberately separate from
the table's desired convergence target.

If any requirement fails, search returns `ErrStale`, or
`ErrExactFallback` only when exact fallback was explicitly enabled. ShoalQL
then runs its existing exhaustive vector iterator for graph rows. Document
semantic results carry shard, datatype, UID, source row, and arbitrary metadata
so ShoalQL can hydrate the exact document cells.

Selected clusters fan out by persisted shard. Every shard computes a bounded
partial top-k. The global merge is deterministic: score descending, then
document identifier ascending. Context cancellation stops fan-out and prevents
partial results from being returned.

Explorer's mixed-space fallback groups the eligible snapshot by the full
embedding-space identity. Query vectors use a bounded cache keyed by that
identity and trimmed query text. `MaxEmbeddingSpaceFanout` refuses excess
spaces before any provider call, while `EmbeddingQueryObserver` reports the
sorted participating identities, cache hits, provider calls, unavailable
spaces, and fan-out refusals without exposing query text or vector bytes.
Single-space retrieval keeps score ordering; mixed-space retrieval uses
deterministic rank fusion and never compares raw scores across models.

The core observer is an operator surface for an unwrapped `Explorer`; its space
identities are storage identities and are not safe to forward across an
authorization boundary. `pkg/explorer/authorized.Client` replaces any
request-context core observer only after it has resolved the request
`Decision`, read current policy registrations, and removed unauthorized and
mosaic-restricted documents. `RetrieveWithReport` and
`authorized.WithEmbeddingQueryObserver` then expose one request-local report:

- only distinct spaces in that caller-authorized candidate projection appear;
- each visible space is named by a process-keyed opaque pseudonym, not by
  provider, model, version, dimensions, or the persisted space identity;
- `unavailable`, `not_attempted`, and `not_completed` are explicit states, and
  fan-out refusal and degradation are explicit flags;
- authorization suppression and mosaic restriction are booleans derived from
  the separately permitted document counts; no withheld space count or
  identity is added;
- a hidden-only projection invokes no embedding provider and reports no space;
- policy generation is rechecked before the report is released. Cancellation,
  expiry, revocation, or an unreadable generation scrubs space IDs and
  per-space activity counters from the callback/error report.

The pseudonym is stable for repeated requests in one process and rotates on
restart to resist offline guessing of low-entropy provider/model combinations.
Neither the core nor authorized observer payload contains prompt text,
credentials, source text, quotes, or vector bytes.

## Recall contract

Configuration is not a recall claim. `BenchmarkRecall` compares approximate
results with exhaustive cosine top-k over a named deterministic corpus.
`SetRecallContract` accepts evidence only when corpus, query count, and
benchmark reference are present. `EXPLAIN` otherwise states
`unbenchmarked; no recall claim`.

Tests declare a measurable minimum recall, compare approximate and exact
results, and cover stale generations, visibility, `AS OF`, tombstones,
RFile/Parquet/mixed/Accumulo record replay, cancellation, publication faults,
and generation races. A live Accumulo run remains optional and unsupported
without Docker or an externally configured endpoint.

Compaction convergence applies classification policy before provider work
through `embedconverge.EgressGuard`. A denied cell never reaches the rewriter,
does not consume cell budget, and causes the deterministic unconverged fallback
to retain the original file identity, source bytes, and visibility. Rewriters
receive a cloned key so they cannot widen visibility. Migration epochs persist
per-file span counts, and progress reports both file and span counts per space.
