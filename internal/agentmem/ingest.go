package agentmem

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/pkg/extraction"
)

type IngestRequest struct {
	Text string
	Time time.Time
}
type IngestResult struct {
	ID       string
	Row      string
	Entities []Entity
	Summary  string
}

func (c *Client) EnsureTable(ctx context.Context) error {
	if store, ok := c.cfg.Store.(interface {
		CreateTableWithEmbedding(context.Context, string, []string, string) error
	}); ok {
		return store.CreateTableWithEmbedding(
			ctx,
			c.cfg.Table,
			[]string{
				graphschema.EventRowPrefix,
				graphschema.EntityRowPrefix,
				graphschema.TermRowPrefix,
			},
			embeddingspace.Has(c.cfg.EmbeddingSpace).String(),
		)
	}
	return fmt.Errorf(
		"%w: agent-memory store does not support embedding-aware table creation",
		embeddingspace.ErrQueryMetadataMissing)
}

func (c *Client) Ingest(ctx context.Context, req IngestRequest) (IngestResult, error) {
	vec, err := c.cfg.Embedder.Embed(ctx, req.Text)
	if err != nil {
		return IngestResult{}, err
	}
	id := c.ids.New(req.Time)
	row := graphschema.EventRow(id)
	ts := unixMillis(req.Time)
	entities, err := c.cfg.Enricher.Entities(ctx, req.Text)
	if err != nil {
		return IngestResult{}, err
	}
	summary, err := c.cfg.Enricher.Summarize(ctx, req.Text)
	if err != nil {
		return IngestResult{}, err
	}
	entries := []*embedpb.Entry{
		{ColumnFamily: graphschema.ContentCF(), ColumnQualifier: []byte("text"), Timestamp: ts, Value: []byte(req.Text)},
		{ColumnFamily: graphschema.ContentCF(), ColumnQualifier: []byte("summary"), Timestamp: ts, Value: []byte(summary)},
		{ColumnFamily: graphschema.VectorCF(), ColumnQualifier: []byte("embedding"), Timestamp: ts, Value: PackVector(vec)},
		{ColumnFamily: graphschema.AttributeCF(), ColumnQualifier: []byte("timestamp_ms"), Timestamp: ts, Value: []byte(strconv.FormatInt(ts, 10))},
	}
	prev := c.previousEvent(ctx, row)
	if prev != "" {
		entries = append(entries, &embedpb.Entry{ColumnFamily: graphschema.TemporalEdgeCF(), ColumnQualifier: []byte(prev), Timestamp: ts, Value: graphschema.PackWeight(1)})
	}
	for _, ent := range entities {
		entries = append(entries, &embedpb.Entry{ColumnFamily: graphschema.EntityEdgeCF(), ColumnQualifier: []byte(ent.ID), Timestamp: ts, Value: graphschema.PackWeight(1)})
	}
	muts := []*embedpb.Mutation{{Row: row, Entries: entries}}
	for _, ent := range entities {
		muts = append(muts, &embedpb.Mutation{Row: graphschema.EntityRow(ent.ID), Entries: []*embedpb.Entry{
			{ColumnFamily: graphschema.ContentCF(), ColumnQualifier: []byte("label"), Timestamp: ts, Value: []byte(ent.Label)},
			{ColumnFamily: graphschema.AttributeCF(), ColumnQualifier: []byte("type"), Timestamp: ts, Value: []byte(ent.Type)},
			{ColumnFamily: graphschema.AttributeCF(), ColumnQualifier: []byte("extractedFrom"), Timestamp: ts, Value: []byte(id)},
		}})
	}
	for _, kw := range Keywords(req.Text) {
		muts = append(muts, &embedpb.Mutation{Row: graphschema.TermRow(kw), Entries: []*embedpb.Entry{{ColumnFamily: []byte("post:"), ColumnQualifier: []byte(id), Timestamp: ts, Value: []byte("1")}}})
	}
	if err := c.cfg.Store.Write(ctx, c.cfg.Table, muts); err != nil {
		return IngestResult{}, err
	}
	c.indexVectorForFreshness(ctx, string(row), vec)
	return IngestResult{ID: id, Row: string(row), Entities: entities, Summary: summary}, nil
}

// indexVectorForFreshness incrementally adds the just-written vector to a
// trained IVF-PQ index so it is searchable through the IVF path without waiting
// for the next full retrain. It is a best-effort no-op unless Config.IvfFreshness
// is set and an index has been trained; any error is intentionally swallowed —
// the vector is already durably written and remains findable via the
// brute-force fallback, so freshness never compromises ingest.
func (c *Client) indexVectorForFreshness(ctx context.Context, vertexID string, vec []float32) {
	if !c.cfg.IvfFreshness {
		return
	}
	ix := c.ivfIndex(ctx)
	if ix == nil {
		return
	}
	_ = ix.AddInSpace(ctx, vertexID, vec, c.cfg.EmbeddingSpace)
}

func (c *Client) previousEvent(ctx context.Context, current []byte) string {
	cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, &embedpb.ScanRequest{RowPrefix: graphschema.EventRowPrefix, Limit: 100000})
	if err != nil {
		return ""
	}
	var rows []string
	seen := map[string]bool{}
	for _, cell := range cells {
		r := string(cell.Row)
		if r < string(current) && !seen[r] {
			seen[r] = true
			rows = append(rows, r)
		}
	}
	sort.Strings(rows)
	if len(rows) == 0 {
		return ""
	}
	id, _ := graphschema.ParseEventID([]byte(rows[len(rows)-1]))
	return id
}

type Consolidator struct {
	client *Client
	queue  chan string
	stop   chan struct{}
	once   sync.Once
}

func NewConsolidator(c *Client, size int) *Consolidator {
	if size <= 0 {
		size = 128
	}
	return &Consolidator{client: c, queue: make(chan string, size), stop: make(chan struct{})}
}
func (co *Consolidator) Enqueue(id string) {
	select {
	case co.queue <- id:
	default:
	}
}
func (co *Consolidator) Stop() { co.once.Do(func() { close(co.stop) }) }
func (co *Consolidator) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-co.stop:
			return nil
		case id := <-co.queue:
			if err := co.Consolidate(ctx, id); err != nil {
				return err
			}
		}
	}
}
func (co *Consolidator) SeedAll(ctx context.Context) error {
	cells, err := co.client.cfg.Store.Scan(ctx, co.client.cfg.Table, &embedpb.ScanRequest{RowPrefix: graphschema.EventRowPrefix})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, cell := range cells {
		if id, ok := graphschema.ParseEventID(cell.Row); ok && !seen[id] {
			seen[id] = true
			co.Enqueue(id)
		}
	}
	return nil
}

func (co *Consolidator) Consolidate(ctx context.Context, id string) error {
	if co.client.cfg.OntologyExtractor == nil &&
		co.client.cfg.OntologyRequestFactory == nil {
		// Legacy substring-based model output is intentionally disabled.
		// Existing agents continue running without publishing unsafe links.
		return nil
	}
	if co.client.cfg.ConsolidationPublisher == nil {
		return nil
	}
	plan, err := co.PlanConsolidation(ctx, id)
	if err != nil {
		return err
	}
	return co.client.cfg.ConsolidationPublisher(ctx, plan)
}

// PlanConsolidation returns a validated proposed publication plan. It never
// writes graph rows directly.
func (co *Consolidator) PlanConsolidation(ctx context.Context, id string) (extraction.PublicationPlan, error) {
	if co.client.cfg.OntologyExtractor == nil ||
		co.client.cfg.OntologyRequestFactory == nil {
		return extraction.PublicationPlan{}, ErrOntologyExtractionUnavailable
	}
	anchor := graphschema.EventRow(id)
	cells, err := co.client.cfg.Store.Scan(ctx, co.client.cfg.Table, &embedpb.ScanRequest{EdgeExpand: &embedpb.EdgeExpand{AnchorRows: [][]byte{anchor}, EdgeCf: graphschema.TemporalEdgeCF(), PrimaryPrefix: []byte(graphschema.EventRowPrefix), IncludeAnchors: true, MaxHops: 2}})
	if err != nil {
		return extraction.PublicationPlan{}, err
	}
	text := strings.Builder{}
	nodes := rowsToNodes(cells)
	for _, n := range nodes {
		text.WriteString(n.ID)
		text.WriteByte(' ')
		text.WriteString(n.Content)
		text.WriteByte('\n')
	}
	request, err := co.client.cfg.OntologyRequestFactory(ctx, text.String())
	if err != nil {
		return extraction.PublicationPlan{}, err
	}
	result, err := co.client.cfg.OntologyExtractor.Extract(ctx, request)
	if err != nil {
		return extraction.PublicationPlan{}, err
	}
	return result.PublicationPlan(), nil
}
