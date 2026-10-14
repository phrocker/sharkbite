// Package vectorindex implements a storage-format-neutral distributed IVF-PQ
// lifecycle. Persisted snapshots contain only deterministic records, so the
// same generation can be replayed through RFile, Parquet, or Accumulo stores.
package vectorindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
)

var (
	ErrNotFound           = errors.New("vectorindex: index not found")
	ErrStale              = errors.New("vectorindex: index freshness contract not met")
	ErrExactFallback      = errors.New("vectorindex: exact fallback required")
	ErrGenerationConflict = errors.New("vectorindex: active generation changed")
	ErrEmbeddingSpace     = errors.New("vectorindex: embedding space identity required")
)

type DocumentRef struct {
	Row      string            `json:"row,omitempty"`
	Shard    string            `json:"shard,omitempty"`
	Datatype string            `json:"datatype,omitempty"`
	UID      string            `json:"uid,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type VectorRecord struct {
	ID             string      `json:"id"`
	Vector         []float32   `json:"-"`
	EmbeddingSpace string      `json:"embedding_space,omitempty"`
	Document       DocumentRef `json:"document"`
	Visibility     string      `json:"visibility,omitempty"`
	Timestamp      int64       `json:"timestamp"`
	Tombstone      bool        `json:"tombstone,omitempty"`
}

type Posting struct {
	ID              string      `json:"id"`
	Cluster         int         `json:"cluster"`
	Shard           int         `json:"shard"`
	Code            []byte      `json:"code,omitempty"`
	Document        DocumentRef `json:"document"`
	Visibility      string      `json:"visibility,omitempty"`
	Timestamp       int64       `json:"timestamp"`
	Tombstone       bool        `json:"tombstone,omitempty"`
	CodebookVersion string      `json:"codebook_version"`
}

type RecallContract struct {
	Corpus          string  `json:"corpus,omitempty"`
	TopK            int     `json:"top_k,omitempty"`
	NProbe          int     `json:"nprobe,omitempty"`
	Minimum         float64 `json:"minimum,omitempty"`
	Measured        float64 `json:"measured,omitempty"`
	Queries         int     `json:"queries,omitempty"`
	BenchmarkRef    string  `json:"benchmark_ref,omitempty"`
	EmbeddingSpace  string  `json:"embedding_space,omitempty"`
	Generation      uint64  `json:"generation,omitempty"`
	CodebookVersion string  `json:"codebook_version,omitempty"`
}

func (r RecallContract) Benchmarked() bool {
	return r.Corpus != "" &&
		r.Queries > 0 &&
		r.BenchmarkRef != "" &&
		r.EmbeddingSpace != "" &&
		r.Generation > 0 &&
		r.CodebookVersion != ""
}

type Manifest struct {
	FormatVersion     int            `json:"format_version"`
	Index             string         `json:"index"`
	Generation        uint64         `json:"generation"`
	ParentGeneration  uint64         `json:"parent_generation,omitempty"`
	CodebookVersion   string         `json:"codebook_version"`
	EmbeddingSpace    string         `json:"embedding_space"`
	Dimension         int            `json:"dimension"`
	NList             int            `json:"nlist"`
	Subspaces         int            `json:"subspaces"`
	CentroidsPerSpace int            `json:"centroids_per_space"`
	ShardCount        int            `json:"shard_count"`
	SourceWatermark   int64          `json:"source_watermark"`
	IndexedWatermark  int64          `json:"indexed_watermark"`
	CreatedAtUnixMS   int64          `json:"created_at_unix_ms"`
	Lineage           []uint64       `json:"lineage"`
	RecordCount       int            `json:"record_count"`
	TombstoneCount    int            `json:"tombstone_count"`
	Recall            RecallContract `json:"recall"`
}

type Snapshot struct {
	Manifest  Manifest
	Centroids []byte
	PQ        []byte
	Postings  []Posting
}

// Store atomically publishes complete immutable generations. Implementations
// may map Snapshot records to local files or Accumulo cells.
type Store interface {
	Active(context.Context, string) (Snapshot, error)
	Commit(context.Context, Snapshot) error
}

type Config struct {
	NList             int
	Subspaces         int
	CentroidsPerSpace int
	MaxIterations     int
	Seed              int64
	TrainingSamples   int
	ShardCount        int
	CreatedAt         func() time.Time
}

func (c Config) normalized(n int) (Config, error) {
	if n == 0 {
		return c, errors.New("vectorindex: no vectors")
	}
	if c.NList <= 0 {
		c.NList = min(64, n)
	}
	if c.NList > n {
		c.NList = n
	}
	if c.Subspaces <= 0 {
		c.Subspaces = 1
	}
	if c.CentroidsPerSpace <= 0 {
		c.CentroidsPerSpace = min(256, n)
	}
	if c.CentroidsPerSpace > n {
		c.CentroidsPerSpace = n
	}
	if c.MaxIterations <= 0 {
		c.MaxIterations = 25
	}
	if c.ShardCount <= 0 {
		c.ShardCount = c.NList
	}
	if c.CreatedAt == nil {
		c.CreatedAt = time.Now
	}
	return c, nil
}

type Manager struct {
	store Store
	cfg   Config
}

func New(store Store, cfg Config) *Manager { return &Manager{store: store, cfg: cfg} }

func (m *Manager) Build(ctx context.Context, index string, records []VectorRecord, sourceWatermark int64) (Manifest, error) {
	return m.build(ctx, index, records, sourceWatermark, 0, nil)
}

func (m *Manager) Rebuild(ctx context.Context, index string, records []VectorRecord, sourceWatermark int64) (Manifest, error) {
	var parent uint64
	var lineage []uint64
	if active, err := m.store.Active(ctx, index); err == nil {
		parent = active.Manifest.Generation
		lineage = append(lineage, active.Manifest.Lineage...)
		lineage = append(lineage, parent)
	} else if !errors.Is(err, ErrNotFound) {
		return Manifest{}, err
	}
	return m.build(ctx, index, records, sourceWatermark, parent, lineage)
}

func (m *Manager) build(ctx context.Context, index string, records []VectorRecord, sourceWatermark int64, parent uint64, lineage []uint64) (Manifest, error) {
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	live := make([]VectorRecord, 0, len(records))
	for _, r := range records {
		if !r.Tombstone {
			live = append(live, cloneRecord(r))
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].ID < live[j].ID })
	embeddingSpace, err := embeddingSpaceForRecords("train vector index", live)
	if err != nil {
		return Manifest{}, err
	}
	cfg, err := m.cfg.normalized(len(live))
	if err != nil {
		return Manifest{}, err
	}
	dim := len(live[0].Vector)
	if dim == 0 || dim%cfg.Subspaces != 0 {
		return Manifest{}, fmt.Errorf("vectorindex: dimension %d is not divisible by subspaces %d", dim, cfg.Subspaces)
	}
	for i := range live {
		if live[i].ID == "" || len(live[i].Vector) != dim {
			return Manifest{}, fmt.Errorf("vectorindex: invalid vector %q dimension %d, want %d", live[i].ID, len(live[i].Vector), dim)
		}
		live[i].Vector = normalize(live[i].Vector)
	}
	sample := deterministicSample(live, cfg.TrainingSamples, cfg.Seed)
	if cfg.NList > len(sample) {
		cfg.NList = len(sample)
	}
	if cfg.CentroidsPerSpace > len(sample) {
		cfg.CentroidsPerSpace = len(sample)
	}
	version := codebookDigest(sample, cfg, embeddingSpace)
	versionNumber := int32(binary.BigEndian.Uint32(mustDecodePrefix(version, 4)) & 0x7fffffff)
	vectors := make([][]float32, len(sample))
	for i := range sample {
		vectors[i] = sample[i].Vector
	}
	centroids, err := ivfpq.TrainCentroidsInSpace(
		vectors, cfg.NList, cfg.MaxIterations, cfg.Seed, versionNumber, embeddingSpace)
	if err != nil {
		return Manifest{}, err
	}
	pq, err := ivfpq.TrainPQInSpace(
		vectors, cfg.Subspaces, cfg.CentroidsPerSpace, cfg.MaxIterations,
		cfg.Seed, versionNumber, embeddingSpace)
	if err != nil {
		return Manifest{}, err
	}
	postings := make([]Posting, 0, len(live))
	for _, record := range live {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		cluster := centroids.Assign(record.Vector)
		code, err := pq.Encode(record.Vector)
		if err != nil {
			return Manifest{}, err
		}
		postings = append(postings, postingFor(record, cluster, cfg.ShardCount, version, code))
	}
	generation := parent + 1
	if parent == 0 {
		generation = 1
	}
	centroidBytes, _ := centroids.Bytes()
	pqBytes, _ := pq.Bytes()
	manifest := Manifest{
		FormatVersion: 2, Index: index, Generation: generation, ParentGeneration: parent,
		CodebookVersion: version, EmbeddingSpace: embeddingSpace,
		Dimension: dim, NList: cfg.NList, Subspaces: cfg.Subspaces,
		CentroidsPerSpace: cfg.CentroidsPerSpace, ShardCount: cfg.ShardCount,
		SourceWatermark: sourceWatermark, IndexedWatermark: sourceWatermark,
		CreatedAtUnixMS: cfg.CreatedAt().UTC().UnixMilli(), Lineage: append([]uint64(nil), lineage...),
		RecordCount: len(postings),
	}
	snapshot := Snapshot{Manifest: manifest, Centroids: centroidBytes, PQ: pqBytes, Postings: postings}
	if err := m.store.Commit(ctx, snapshot); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Update publishes a new generation using the active codebook. Newer
// timestamped records supersede older records at query time; tombstones are
// persisted and participate in the same visibility and AS OF semantics.
func (m *Manager) Update(ctx context.Context, index string, changes []VectorRecord, sourceWatermark int64) (Manifest, error) {
	active, err := m.store.Active(ctx, index)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestEmbeddingSpace(active.Manifest); err != nil {
		return Manifest{}, err
	}
	centroids, err := ivfpq.CentroidsFromBytes(active.Centroids)
	if err != nil {
		return Manifest{}, err
	}
	centroids, err = centroids.WithEmbeddingSpace(active.Manifest.EmbeddingSpace)
	if err != nil {
		return Manifest{}, err
	}
	pq, err := ivfpq.FromBytes(active.PQ)
	if err != nil {
		return Manifest{}, err
	}
	pq, err = pq.WithEmbeddingSpace(active.Manifest.EmbeddingSpace)
	if err != nil {
		return Manifest{}, err
	}
	postings := append([]Posting(nil), active.Postings...)
	tombstones := active.Manifest.TombstoneCount
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if change.ID == "" {
			return Manifest{}, errors.New("vectorindex: update has empty id")
		}
		if change.Tombstone {
			if change.EmbeddingSpace != "" {
				if err := embeddingspace.EnsureSameIdentity(
					"update vector index", active.Manifest.EmbeddingSpace,
					change.EmbeddingSpace); err != nil {
					return Manifest{}, err
				}
			}
			postings = append(postings, postingFor(change, 0, active.Manifest.ShardCount, active.Manifest.CodebookVersion, nil))
			tombstones++
			continue
		}
		if err := requireEmbeddingSpace("update vector index", change.EmbeddingSpace); err != nil {
			return Manifest{}, err
		}
		if err := embeddingspace.EnsureSameIdentity(
			"update vector index", active.Manifest.EmbeddingSpace,
			change.EmbeddingSpace); err != nil {
			return Manifest{}, err
		}
		if len(change.Vector) != active.Manifest.Dimension {
			return Manifest{}, fmt.Errorf("vectorindex: update %q dimension %d, want %d", change.ID, len(change.Vector), active.Manifest.Dimension)
		}
		vec := normalize(change.Vector)
		cluster := centroids.Assign(vec)
		code, err := pq.Encode(vec)
		if err != nil {
			return Manifest{}, err
		}
		postings = append(postings, postingFor(change, cluster, active.Manifest.ShardCount, active.Manifest.CodebookVersion, code))
	}
	manifest := active.Manifest
	manifest.ParentGeneration = active.Manifest.Generation
	manifest.Generation++
	manifest.Lineage = append(append([]uint64(nil), active.Manifest.Lineage...), active.Manifest.Generation)
	manifest.SourceWatermark = sourceWatermark
	manifest.IndexedWatermark = sourceWatermark
	manifest.CreatedAtUnixMS = m.now().UTC().UnixMilli()
	manifest.RecordCount = len(postings)
	manifest.TombstoneCount = tombstones
	manifest.Recall = RecallContract{}
	active.Manifest = manifest
	active.Postings = postings
	if err := m.store.Commit(ctx, active); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Compact removes versions hidden by a newer record while preserving the
// active codebook and generation lineage.
func (m *Manager) Compact(ctx context.Context, index string, beforeTimestamp int64) (Manifest, error) {
	active, err := m.store.Active(ctx, index)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestEmbeddingSpace(active.Manifest); err != nil {
		return Manifest{}, err
	}
	latest := map[string]Posting{}
	var keep []Posting
	for _, p := range active.Postings {
		key := p.ID + "\x00" + p.Visibility
		if p.Timestamp >= beforeTimestamp {
			keep = append(keep, p)
			continue
		}
		if old, ok := latest[key]; !ok || postingNewer(p, old) {
			latest[key] = p
		}
	}
	for _, p := range latest {
		if !p.Tombstone {
			keep = append(keep, p)
		}
	}
	sortPostings(keep)
	manifest := active.Manifest
	manifest.ParentGeneration = manifest.Generation
	manifest.Generation++
	manifest.Lineage = append(append([]uint64(nil), manifest.Lineage...), manifest.ParentGeneration)
	manifest.CreatedAtUnixMS = m.now().UTC().UnixMilli()
	manifest.RecordCount = len(keep)
	manifest.TombstoneCount = countTombstones(keep)
	manifest.Recall = RecallContract{}
	active.Manifest, active.Postings = manifest, keep
	if err := m.store.Commit(ctx, active); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// SetRecallContract records externally reproducible benchmark evidence. An
// unbenchmarked declaration is rejected so EXPLAIN cannot claim recall based
// only on configuration.
func (m *Manager) SetRecallContract(ctx context.Context, index string, recall RecallContract) (Manifest, error) {
	if !recall.Benchmarked() {
		return Manifest{}, errors.New("vectorindex: recall contract lacks corpus benchmark evidence")
	}
	active, err := m.store.Active(ctx, index)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestEmbeddingSpace(active.Manifest); err != nil {
		return Manifest{}, err
	}
	if err := embeddingspace.EnsureSameIdentity(
		"set vector recall contract",
		active.Manifest.EmbeddingSpace,
		recall.EmbeddingSpace,
	); err != nil {
		return Manifest{}, err
	}
	if recall.Generation != active.Manifest.Generation ||
		recall.CodebookVersion != active.Manifest.CodebookVersion {
		return Manifest{}, fmt.Errorf(
			"vectorindex: recall contract targets generation/codebook %d/%q, active is %d/%q",
			recall.Generation,
			recall.CodebookVersion,
			active.Manifest.Generation,
			active.Manifest.CodebookVersion,
		)
	}
	manifest := active.Manifest
	manifest.ParentGeneration = manifest.Generation
	manifest.Generation++
	manifest.Lineage = append(
		append([]uint64(nil), manifest.Lineage...), manifest.ParentGeneration)
	manifest.CreatedAtUnixMS = m.now().UTC().UnixMilli()
	recall.Generation = manifest.Generation
	manifest.Recall = recall
	active.Manifest = manifest
	if err := m.store.Commit(ctx, active); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

type Freshness struct {
	RequiredGeneration uint64
	MinimumWatermark   int64
	SourceWatermark    int64
	MaxLag             int64
}

type Query struct {
	Vector         []float32
	EmbeddingSpace string
	TopK           int
	NProbe         int
	Authorizations map[string]bool
	AsOf           *int64
	Freshness      Freshness
	ExactFallback  bool
	// AllowedDocuments restricts scoring before partial top-k selection. Keys
	// are shard\x00datatype\x00uid identities.
	AllowedDocuments map[string]bool
}

type Hit struct {
	ID       string
	Score    float32
	Document DocumentRef
}

type Evidence struct {
	Index              string         `json:"index"`
	Generation         uint64         `json:"generation"`
	CodebookVersion    string         `json:"codebook_version"`
	EmbeddingSpace     string         `json:"embedding_space"`
	IndexedWatermark   int64          `json:"indexed_watermark"`
	SourceWatermark    int64          `json:"source_watermark"`
	NProbe             int            `json:"nprobe"`
	Clusters           []int          `json:"clusters"`
	Shards             []int          `json:"shards"`
	CandidateCount     int            `json:"candidate_count"`
	PartialTopKCount   int            `json:"partial_top_k_count"`
	ExactFallback      bool           `json:"exact_fallback"`
	FallbackReason     string         `json:"fallback_reason,omitempty"`
	Recall             RecallContract `json:"recall"`
	RecallClaimed      bool           `json:"recall_claimed"`
	DeterministicMerge string         `json:"deterministic_merge"`
}

func (e Evidence) Explain() string {
	recall := "unbenchmarked; no recall claim"
	if e.RecallClaimed {
		recall = fmt.Sprintf("measured recall@%d=%.4f minimum=%.4f corpus=%s ref=%s",
			e.Recall.TopK, e.Recall.Measured, e.Recall.Minimum, e.Recall.Corpus, e.Recall.BenchmarkRef)
	}
	return fmt.Sprintf("ivf-pq index=%s generation=%d space=%s codebook=%s watermark=%d/%d nprobe=%d clusters=%v shards=%v candidates=%d merge=%s fallback=%t reason=%q recall=%s",
		e.Index, e.Generation, e.EmbeddingSpace, e.CodebookVersion, e.IndexedWatermark, e.SourceWatermark,
		e.NProbe, e.Clusters, e.Shards, e.CandidateCount, e.DeterministicMerge,
		e.ExactFallback, e.FallbackReason, recall)
}

func (m *Manager) Search(ctx context.Context, index string, query Query) ([]Hit, Evidence, error) {
	active, err := m.store.Active(ctx, index)
	if err != nil {
		return nil, Evidence{}, err
	}
	if err := validateManifestEmbeddingSpace(active.Manifest); err != nil {
		return nil, Evidence{}, err
	}
	evidence := Evidence{
		Index: index, Generation: active.Manifest.Generation, CodebookVersion: active.Manifest.CodebookVersion,
		EmbeddingSpace:   active.Manifest.EmbeddingSpace,
		IndexedWatermark: active.Manifest.IndexedWatermark, SourceWatermark: query.Freshness.SourceWatermark,
		Recall: active.Manifest.Recall, RecallClaimed: active.Manifest.Recall.Benchmarked(),
		DeterministicMerge: "score descending, document id ascending",
	}
	if evidence.SourceWatermark == 0 {
		evidence.SourceWatermark = active.Manifest.SourceWatermark
	}
	if err := requireEmbeddingSpace("compare vector query", query.EmbeddingSpace); err != nil {
		return nil, evidence, err
	}
	if err := embeddingspace.EnsureSameIdentity(
		"compare vector query", active.Manifest.EmbeddingSpace,
		query.EmbeddingSpace); err != nil {
		return nil, evidence, err
	}
	if reason := staleReason(active.Manifest, query.Freshness); reason != "" {
		evidence.ExactFallback = query.ExactFallback
		evidence.FallbackReason = reason
		if query.ExactFallback {
			return nil, evidence, fmt.Errorf("%w: %s", ErrExactFallback, reason)
		}
		return nil, evidence, fmt.Errorf("%w: %s", ErrStale, reason)
	}
	if query.TopK <= 0 {
		query.TopK = 10
	}
	centroids, err := ivfpq.CentroidsFromBytes(active.Centroids)
	if err != nil {
		return nil, evidence, err
	}
	centroids, err = centroids.WithEmbeddingSpace(active.Manifest.EmbeddingSpace)
	if err != nil {
		return nil, evidence, err
	}
	pq, err := ivfpq.FromBytes(active.PQ)
	if err != nil {
		return nil, evidence, err
	}
	pq, err = pq.WithEmbeddingSpace(active.Manifest.EmbeddingSpace)
	if err != nil {
		return nil, evidence, err
	}
	vec := normalize(query.Vector)
	if len(vec) != active.Manifest.Dimension {
		return nil, evidence, fmt.Errorf("vectorindex: query dimension %d, want %d", len(vec), active.Manifest.Dimension)
	}
	if query.NProbe <= 0 {
		query.NProbe = min(8, active.Manifest.NList)
	}
	clusters, err := centroids.NProbeInSpace(vec, query.NProbe, query.EmbeddingSpace)
	if err != nil {
		return nil, evidence, err
	}
	evidence.NProbe, evidence.Clusters = len(clusters), append([]int(nil), clusters...)
	clusterSet := make(map[int]bool, len(clusters))
	shardSet := map[int]bool{}
	for _, cluster := range clusters {
		clusterSet[cluster] = true
		shardSet[cluster%active.Manifest.ShardCount] = true
	}
	for shard := range shardSet {
		evidence.Shards = append(evidence.Shards, shard)
	}
	sort.Ints(evidence.Shards)
	ip, err := pq.InnerProductTableInSpace(vec, query.EmbeddingSpace)
	if err != nil {
		return nil, evidence, err
	}

	visible := latestVisible(active.Postings, query.Authorizations, query.AsOf)
	type partial struct {
		hits       []Hit
		candidates int
	}
	ch := make(chan partial, len(evidence.Shards))
	var wg sync.WaitGroup
	for _, shard := range evidence.Shards {
		shard := shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]Hit, 0, query.TopK)
			candidates := 0
			for _, posting := range visible {
				if ctx.Err() != nil {
					return
				}
				if posting.Shard != shard || !clusterSet[posting.Cluster] || posting.Tombstone {
					continue
				}
				if len(query.AllowedDocuments) > 0 &&
					!query.AllowedDocuments[documentIdentity(posting.Document)] {
					continue
				}
				candidates++
				local = append(local, Hit{ID: posting.ID, Score: pq.Dot(posting.Code, ip), Document: cloneDocument(posting.Document)})
			}
			sortHits(local)
			if len(local) > query.TopK {
				local = local[:query.TopK]
			}
			select {
			case ch <- partial{hits: local, candidates: candidates}:
			case <-ctx.Done():
			}
		}()
	}
	go func() { wg.Wait(); close(ch) }()
	var merged []Hit
	for part := range ch {
		evidence.CandidateCount += part.candidates
		evidence.PartialTopKCount += len(part.hits)
		merged = append(merged, part.hits...)
	}
	if err := ctx.Err(); err != nil {
		return nil, evidence, err
	}
	sortHits(merged)
	if len(merged) > query.TopK {
		merged = merged[:query.TopK]
	}
	return merged, evidence, nil
}

func documentIdentity(document DocumentRef) string {
	return document.Shard + "\x00" + document.Datatype + "\x00" + document.UID
}

func (m *Manager) Describe(ctx context.Context, index string) (Manifest, error) {
	snapshot, err := m.store.Active(ctx, index)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestEmbeddingSpace(snapshot.Manifest); err != nil {
		return Manifest{}, err
	}
	return snapshot.Manifest, nil
}

func staleReason(m Manifest, f Freshness) string {
	if f.RequiredGeneration > 0 && m.Generation < f.RequiredGeneration {
		return fmt.Sprintf("generation %d is older than required %d", m.Generation, f.RequiredGeneration)
	}
	if f.MinimumWatermark > 0 && m.IndexedWatermark < f.MinimumWatermark {
		return fmt.Sprintf("indexed watermark %d is older than required %d", m.IndexedWatermark, f.MinimumWatermark)
	}
	if f.SourceWatermark > 0 && f.MaxLag >= 0 && f.SourceWatermark-m.IndexedWatermark > f.MaxLag {
		return fmt.Sprintf("watermark lag %d exceeds maximum %d", f.SourceWatermark-m.IndexedWatermark, f.MaxLag)
	}
	return ""
}

func latestVisible(postings []Posting, auth map[string]bool, asOf *int64) []Posting {
	latest := map[string]Posting{}
	labels := make([]string, 0, len(auth))
	for label, enabled := range auth {
		if enabled {
			labels = append(labels, label)
		}
	}
	evaluator := accumulo.NewVisibilityEvaluator(accumulo.NewAuthorizationStrings(labels...))
	for _, p := range postings {
		if asOf != nil && p.Timestamp > *asOf {
			continue
		}
		visible, err := evaluator.Evaluate([]byte(p.Visibility))
		if err != nil || !visible {
			continue
		}
		if old, ok := latest[p.ID]; !ok || postingNewer(p, old) {
			latest[p.ID] = p
		}
	}
	out := make([]Posting, 0, len(latest))
	for _, p := range latest {
		if !p.Tombstone {
			out = append(out, p)
		}
	}
	sortPostings(out)
	return out
}

func postingNewer(a, b Posting) bool {
	if a.Timestamp != b.Timestamp {
		return a.Timestamp > b.Timestamp
	}
	if a.Tombstone != b.Tombstone {
		return a.Tombstone
	}
	return a.CodebookVersion > b.CodebookVersion
}

func postingFor(r VectorRecord, cluster, shardCount int, version string, code []byte) Posting {
	shard := 0
	if shardCount > 0 {
		shard = cluster % shardCount
	}
	return Posting{
		ID: r.ID, Cluster: cluster, Shard: shard, Code: append([]byte(nil), code...),
		Document: cloneDocument(r.Document), Visibility: r.Visibility, Timestamp: r.Timestamp,
		Tombstone: r.Tombstone, CodebookVersion: version,
	}
}

func deterministicSample(records []VectorRecord, maximum int, seed int64) []VectorRecord {
	if maximum <= 0 || maximum >= len(records) {
		return append([]VectorRecord(nil), records...)
	}
	type ranked struct {
		record VectorRecord
		hash   [32]byte
	}
	rankedRecords := make([]ranked, len(records))
	var seedBytes [8]byte
	binary.BigEndian.PutUint64(seedBytes[:], uint64(seed))
	for i, record := range records {
		h := sha256.New()
		_, _ = h.Write(seedBytes[:])
		_, _ = h.Write([]byte(record.ID))
		copy(rankedRecords[i].hash[:], h.Sum(nil))
		rankedRecords[i].record = record
	}
	sort.Slice(rankedRecords, func(i, j int) bool {
		if cmp := bytes.Compare(rankedRecords[i].hash[:], rankedRecords[j].hash[:]); cmp != 0 {
			return cmp < 0
		}
		return rankedRecords[i].record.ID < rankedRecords[j].record.ID
	})
	out := make([]VectorRecord, maximum)
	for i := range out {
		out[i] = rankedRecords[i].record
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func codebookDigest(records []VectorRecord, cfg Config, embeddingSpace string) string {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(embeddingSpace)))
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(embeddingSpace))
	for _, v := range []int64{int64(cfg.NList), int64(cfg.Subspaces), int64(cfg.CentroidsPerSpace), int64(cfg.MaxIterations), cfg.Seed} {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	for _, record := range records {
		_, _ = h.Write([]byte(record.ID))
		for _, value := range record.Vector {
			binary.BigEndian.PutUint32(buf[:4], math.Float32bits(value))
			_, _ = h.Write(buf[:4])
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func embeddingSpaceForRecords(operation string, records []VectorRecord) (string, error) {
	if len(records) == 0 {
		return "", fmt.Errorf("%w: %s has no live vectors", ErrEmbeddingSpace, operation)
	}
	identity := records[0].EmbeddingSpace
	if err := requireEmbeddingSpace(operation, identity); err != nil {
		return "", err
	}
	for _, record := range records[1:] {
		if err := requireEmbeddingSpace(operation, record.EmbeddingSpace); err != nil {
			return "", err
		}
		if err := embeddingspace.EnsureSameIdentity(
			operation, identity, record.EmbeddingSpace); err != nil {
			return "", err
		}
	}
	return identity, nil
}

func requireEmbeddingSpace(operation, identity string) error {
	normalized := strings.TrimSpace(identity)
	if normalized == "" {
		return fmt.Errorf("%w: %s", ErrEmbeddingSpace, operation)
	}
	if normalized != identity {
		return fmt.Errorf(
			"%w: %s identity has surrounding whitespace",
			ErrEmbeddingSpace, operation)
	}
	if err := embeddingspace.Has(identity).Validate(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return nil
}

func validateManifestEmbeddingSpace(manifest Manifest) error {
	if err := requireEmbeddingSpace("read vector index manifest", manifest.EmbeddingSpace); err != nil {
		return err
	}
	return nil
}

func mustDecodePrefix(value string, bytes int) []byte {
	out, _ := hex.DecodeString(value[:bytes*2])
	return out
}

func normalize(vector []float32) []float32 {
	out := append([]float32(nil), vector...)
	var sum float64
	for _, value := range out {
		sum += float64(value) * float64(value)
	}
	if sum == 0 {
		return out
	}
	norm := float32(math.Sqrt(sum))
	for i := range out {
		out[i] /= norm
	}
	return out
}

func sortHits(hits []Hit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
}

func sortPostings(postings []Posting) {
	sort.SliceStable(postings, func(i, j int) bool {
		if postings[i].Shard != postings[j].Shard {
			return postings[i].Shard < postings[j].Shard
		}
		if postings[i].Cluster != postings[j].Cluster {
			return postings[i].Cluster < postings[j].Cluster
		}
		if postings[i].ID != postings[j].ID {
			return postings[i].ID < postings[j].ID
		}
		return postingNewer(postings[i], postings[j])
	})
}

func countTombstones(postings []Posting) int {
	n := 0
	for _, posting := range postings {
		if posting.Tombstone {
			n++
		}
	}
	return n
}

func cloneRecord(record VectorRecord) VectorRecord {
	record.Vector = append([]float32(nil), record.Vector...)
	record.Document = cloneDocument(record.Document)
	return record
}

func cloneDocument(document DocumentRef) DocumentRef {
	document.Metadata = cloneMap(document.Metadata)
	return document
}

func cloneMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func (m *Manager) now() time.Time {
	if m.cfg.CreatedAt != nil {
		return m.cfg.CreatedAt()
	}
	return time.Now()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
