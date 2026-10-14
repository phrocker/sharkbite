# Accumulo-backed ShoalQL

`internal/shoalql/accumulobackend` runs the existing ShoalQL plans through the
public Accumulo client. It pushes row ranges, inclusive column-family
restrictions, scan authorizations, cancellation/deadlines, and RPC page size to
`BatchScanner`. Row hydration and graph traversal use batched row ranges.
`WriteCells` preserves row, column, visibility, timestamp, value, and tombstone
semantics through `BatchWriter`, independent of whether promoted data
originated in RFile or Parquet.

Shoal iterator implementations do not have compatible distributed Java class
lifecycle in this slice. Scanner pages are therefore materialized, globally
key-sorted, and replayed through the local iterator runtime for document
reconstruction, aggregation, and `AS OF`. Exact raw-vector ranking is refused
with `embeddingspace.ErrQueryMetadataMissing` until the Accumulo adapter can
obtain the authoritative per-file `file.embedding` snapshot; replaying one raw
vector across unverified files would silently mix model spaces. Because a
standard Accumulo scan may apply versioning before the client sees cells,
`AS OF` is rejected unless `HistoricalVersions` explicitly
confirms that the configured scanner/replay retains historical versions.

Approximate vector search is opt-in. Configure `Options.VectorSearcher` with a
`shoalql.ManagedVectorSearcher` backed by `internal/vectorindex.Manager`.
Without it, the backend continues to report approximate search as unsupported.
With it, ShoalQL routes IVF cluster shards, merges deterministic partial top-k
results, applies Accumulo visibility and timestamp/tombstone semantics, and
exposes generation, codebook, lineage, watermark, fallback, and recall evidence
in `EXPLAIN`. Recall is reported only after a reproducible corpus benchmark.
See [`distributed-vector-index.md`](distributed-vector-index.md).

The cross-backend corpus test covers RFile, Parquet, mixed local tables, and an
Accumulo scanner replay. The optional live test is gated by
`SHOAL_ACCUMULO_LIVE=1` and skips with an unsupported reason when Docker or an
external Accumulo endpoint is unavailable.
