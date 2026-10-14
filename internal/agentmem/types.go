package agentmem

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	modelio "github.com/phrocker/shoal-oss/pkg/model"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultTable = "graph"
	DefaultDim   = 16
)

type Intent string

const (
	IntentWhy     Intent = "WHY"
	IntentWhen    Intent = "WHEN"
	IntentEntity  Intent = "ENTITY"
	IntentGeneral Intent = "GENERAL"
)

type Embedder interface {
	Embed(context.Context, string) ([]float32, error)
}

type embeddingSpaceIdentityProvider interface {
	EmbeddingSpaceIdentity() (string, error)
}
type LLM interface {
	Infer(context.Context, string) (string, error)
}
type IntentClassifier interface{ Classify(string) Intent }

type EmbedStore interface {
	CreateTable(context.Context, string, []string) error
	Write(context.Context, string, []*embedpb.Mutation) error
	Scan(context.Context, string, *embedpb.ScanRequest) ([]*embedpb.Cell, error)
	Flush(context.Context, string) error
}

type Config struct {
	Table    string
	Embedder Embedder
	// EmbeddingSpace is the stable identity produced by Embedder. Empty is
	// resolved from embeddingSpaceIdentityProvider during New.
	EmbeddingSpace string
	LLM            LLM
	Enricher       Enricher
	Classifier     IntentClassifier
	Store          EmbedStore
	MaxAnchors     int
	BeamWidth      int
	MaxDepth       int
	TokenBudget    int

	// OntologyExtractor and OntologyRequestFactory enable safe structured
	// enrichment and consolidation planning without using the legacy entity
	// row write path.
	OntologyExtractor      OntologyExtractor
	OntologyRequestFactory OntologyRequestFactory

	// ConsolidationPublisher receives validated proposed plans. Storage
	// publication remains outside agentmem unless a caller supplies an atomic
	// high-level publisher.
	ConsolidationPublisher func(context.Context, extraction.PublicationPlan) error

	// UseIVF, when true, sources the semantic anchor list from a trained
	// IVF-PQ index (see cmd/shoal-ivf-train and IvfIndex) instead of the
	// brute-force VectorSearch path. It degrades gracefully: if no index has
	// been trained yet, anchors() transparently falls back to brute force, so
	// enabling the flag is always safe. Default false preserves the exact
	// brute-force behavior.
	UseIVF bool
	// IvfNprobe is the number of coarse clusters probed per IVF query. Only
	// consulted when UseIVF is true; defaults to 8 when unset.
	IvfNprobe int

	// IvfFreshness, when true, keeps a trained IVF-PQ index current on the
	// write path: each Ingest assigns the new vector to its nearest existing
	// centroid, PQ-encodes it, and writes the posting into <table>_ivf so the
	// memory is searchable through the IVF path immediately — no retrain. It is
	// best-effort and always safe to enable: when no index has been trained the
	// hook is a no-op, and any indexing error is swallowed (the vector remains
	// findable via the brute-force fallback). Pairs naturally with UseIVF, but
	// is independent so producers can maintain freshness for downstream IVF
	// readers without querying via IVF themselves. Default false.
	IvfFreshness bool
}

type Client struct {
	cfg     Config
	ids     *IDGenerator
	ivfOnce sync.Once
	ivf     *IvfIndex
	ivfErr  error
}

func New(cfg Config) (*Client, error) {
	if cfg.Store == nil {
		return nil, errors.New("agentmem: store is required")
	}
	if (cfg.OntologyExtractor == nil) != (cfg.OntologyRequestFactory == nil) {
		return nil, errors.New("agentmem: ontology extractor and request factory must be configured together")
	}
	if cfg.Table == "" {
		cfg.Table = DefaultTable
	}
	if cfg.Embedder == nil {
		cfg.Embedder = FakeEmbedder{Dim: DefaultDim}
	}
	if provider, ok := cfg.Embedder.(embeddingSpaceIdentityProvider); ok {
		identity, err := provider.EmbeddingSpaceIdentity()
		if err != nil {
			if cfg.EmbeddingSpace == "" ||
				!errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
				return nil, err
			}
		} else if cfg.EmbeddingSpace == "" {
			cfg.EmbeddingSpace = identity
		} else if err := embeddingspace.EnsureSameIdentity(
			"configure agent memory embedding",
			identity,
			cfg.EmbeddingSpace,
		); err != nil {
			return nil, err
		}
	} else if cfg.EmbeddingSpace == "" {
		return nil, embeddingspace.ErrQueryIdentityRequired
	}
	if err := embeddingspace.ValidateQueryStates(
		"configure agent memory embedding", cfg.EmbeddingSpace); err != nil {
		return nil, err
	}
	if cfg.LLM == nil {
		cfg.LLM = FakeLLM{}
	}
	if cfg.Enricher == nil {
		cfg.Enricher = HeuristicEnricher{}
	}
	if cfg.Classifier == nil {
		cfg.Classifier = RuleClassifier{}
	}
	if cfg.MaxAnchors <= 0 {
		cfg.MaxAnchors = 6
	}
	if cfg.BeamWidth <= 0 {
		cfg.BeamWidth = 8
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = 2
	}
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = 160
	}
	if cfg.IvfNprobe <= 0 {
		cfg.IvfNprobe = 8
	}
	return &Client{cfg: cfg, ids: NewIDGenerator()}, nil
}

func PackVector(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.BigEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func UnpackVector(raw []byte) ([]float32, error) {
	if len(raw)%4 != 0 {
		return nil, errors.New("packed vector length must be a multiple of 4")
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.BigEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}

type GRPCStore struct{ Client embedpb.ShoalEmbedClient }

func NewGRPCStore(conn grpc.ClientConnInterface) GRPCStore {
	return GRPCStore{Client: embedpb.NewShoalEmbedClient(conn)}
}
func (s GRPCStore) CreateTable(ctx context.Context, table string, splits []string) error {
	_, err := s.Client.CreateTable(ctx, &embedpb.CreateTableRequest{Table: table, Splits: splits})
	return err
}

func (s GRPCStore) CreateTableWithEmbedding(
	ctx context.Context,
	table string,
	splits []string,
	defaultEmbedding string,
) error {
	_, err := s.Client.CreateTableV2(ctx, &embedpb.CreateTableRequest{
		Table: table, Splits: splits, DefaultEmbedding: defaultEmbedding,
	})
	return err
}
func (s GRPCStore) Write(ctx context.Context, table string, muts []*embedpb.Mutation) error {
	results, err := s.WriteWithResults(ctx, table, muts)
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.Status == embedpb.MutationStatus_MUTATION_STATUS_REJECTED {
			return errors.New("agentmem: conditional mutation rejected")
		}
	}
	return nil
}
func (s GRPCStore) WriteWithResults(ctx context.Context, table string, muts []*embedpb.Mutation) ([]*embedpb.MutationResult, error) {
	hasConditions := false
	for _, mutation := range muts {
		hasConditions = hasConditions || mutation != nil && len(mutation.Conditions) > 0
	}
	req := &embedpb.WriteRequest{Table: table, Mutations: muts}
	var (
		resp *embedpb.WriteResponse
		err  error
	)
	if hasConditions {
		resp, err = s.Client.ConditionalWrite(ctx, req)
	} else {
		resp, err = s.Client.Write(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if hasConditions && len(resp.Results) != len(muts) {
		return nil, errors.New("agentmem: server did not return conditional mutation results")
	}
	return resp.Results, nil
}
func (s GRPCStore) Flush(ctx context.Context, table string) error {
	_, err := s.Client.Flush(ctx, &embedpb.FlushRequest{Table: table})
	return err
}
func (s GRPCStore) Scan(ctx context.Context, table string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	clone := proto.Clone(req).(*embedpb.ScanRequest)
	clone.Table = table
	var stream grpc.ServerStreamingClient[embedpb.ScanResponse]
	var err error
	if clone.VectorSearch != nil {
		stream, err = s.Client.ScanV2(ctx, clone)
	} else {
		stream, err = s.Client.Scan(ctx, clone)
	}
	if err != nil {
		return nil, err
	}
	var cells []*embedpb.Cell
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		cells = append(cells, resp.Cells...)
	}
	return cells, nil
}

type modelEmbedderAdapter struct{ embedder modelio.Embedder }

func (a modelEmbedderAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	result, err := a.embedder.Embed(ctx, modelio.EmbedRequest{Text: text})
	return append([]float32(nil), result.Vector...), err
}

func (a modelEmbedderAdapter) EmbeddingSpaceIdentity() (string, error) {
	provider, ok := a.embedder.(modelio.EmbeddingSpaceIdentityProvider)
	if !ok {
		return "", embeddingspace.ErrQueryIdentityRequired
	}
	return provider.EmbeddingSpaceIdentity()
}

type modelGeneratorAdapter struct{ generator modelio.TextGenerator }

func (a modelGeneratorAdapter) Infer(ctx context.Context, prompt string) (string, error) {
	result, err := a.generator.Generate(ctx, modelio.GenerateRequest{Prompt: prompt})
	return result.Text, err
}

func AdaptEmbedder(embedder modelio.Embedder) Embedder {
	if embedder == nil {
		return nil
	}
	return modelEmbedderAdapter{embedder: embedder}
}

func AdaptTextGenerator(generator modelio.TextGenerator) LLM {
	if generator == nil {
		return nil
	}
	return modelGeneratorAdapter{generator: generator}
}

func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().UnixNano() / int64(time.Millisecond)
}
