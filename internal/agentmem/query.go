package agentmem

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
)

type QueryRequest struct {
	Text string
	Time time.Time
}
type QueryResult struct {
	Intent  Intent
	Context string
	Nodes   []ScoredNode
	Anchors []string
}
type ScoredNode struct {
	ID        string
	Row       string
	Content   string
	Timestamp int64
	Score     float32
}

type analysis struct {
	intent           Intent
	vector           []float32
	keywords         []string
	startRow, endRow []byte
}

func (c *Client) Query(ctx context.Context, req QueryRequest) (QueryResult, error) {
	a, err := c.analyze(ctx, req.Text)
	if err != nil {
		return QueryResult{}, err
	}
	anchors, err := c.anchors(ctx, a)
	if err != nil {
		return QueryResult{}, err
	}
	nodes, err := c.beam(ctx, anchors, a)
	if err != nil {
		return QueryResult{}, err
	}
	ctxText := Synthesize(nodes, a.intent, c.cfg.TokenBudget)
	return QueryResult{Intent: a.intent, Context: ctxText, Nodes: nodes, Anchors: anchors}, nil
}

func (c *Client) analyze(ctx context.Context, q string) (analysis, error) {
	v, err := c.cfg.Embedder.Embed(ctx, q)
	if err != nil {
		return analysis{}, err
	}
	a := analysis{intent: c.cfg.Classifier.Classify(q), vector: v, keywords: Keywords(q)}
	if strings.Contains(strings.ToLower(q), "today") {
		d := time.Now().UTC().Format("20060102")
		a.startRow = []byte(graphschema.EventRowPrefix + d)
		a.endRow = []byte(graphschema.EventRowPrefix + d + "~")
	}
	return a, nil
}

func (c *Client) anchors(ctx context.Context, a analysis) ([]string, error) {
	type res struct {
		name string
		rows []string
		err  error
	}
	ch := make(chan res, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		rows, err := c.semanticAnchors(ctx, a)
		ch <- res{"semantic", rows, err}
	}()
	go func() {
		defer wg.Done()
		var termRows [][]byte
		for _, kw := range a.keywords {
			termRows = append(termRows, graphschema.TermRow(kw))
		}
		cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, &embedpb.ScanRequest{TermFilter: &embedpb.TermFilter{TermRows: termRows, PrimaryPrefix: []byte(graphschema.EventRowPrefix), PostingCf: []byte("post:")}})
		ch <- res{"lexical", uniqueRows(cells), err}
	}()
	go func() {
		defer wg.Done()
		scan := &embedpb.ScanRequest{RowPrefix: graphschema.EventRowPrefix, Limit: int32(c.cfg.MaxAnchors * 8)}
		if len(a.startRow) > 0 || len(a.endRow) > 0 {
			scan.RowPrefix = ""
			scan.StartRow = a.startRow
			scan.StartInclusive = true
			scan.EndRow = a.endRow
		}
		cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, scan)
		ch <- res{"temporal", uniqueRows(cells), err}
	}()
	wg.Wait()
	close(ch)
	var lists [][]string
	for r := range ch {
		if r.err != nil {
			return nil, r.err
		}
		if len(r.rows) > 0 {
			lists = append(lists, r.rows)
		}
	}
	scores := RRF(lists, 60)
	type pair struct {
		row   string
		score float64
	}
	var pairs []pair
	for row, sc := range scores {
		pairs = append(pairs, pair{row, sc})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].score == pairs[j].score {
			return pairs[i].row < pairs[j].row
		}
		return pairs[i].score > pairs[j].score
	})
	limit := c.cfg.MaxAnchors
	if len(pairs) < limit {
		limit = len(pairs)
	}
	out := make([]string, limit)
	for i := 0; i < limit; i++ {
		out[i] = pairs[i].row
	}
	return out, nil
}

// semanticAnchors produces the semantic anchor row list. When UseIVF is enabled
// and a trained index is available it probes the IVF-PQ index (rows returned in
// descending approximate-similarity order); otherwise, and on any IVF load or
// search error, it falls back to the brute-force VectorSearch scan so behavior
// degrades gracefully and the default (UseIVF=false) path is byte-for-byte
// unchanged.
func (c *Client) semanticAnchors(ctx context.Context, a analysis) ([]string, error) {
	if c.cfg.UseIVF {
		if index := c.ivfIndex(ctx); index != nil {
			nprobe := c.cfg.IvfNprobe
			if nprobe <= 0 {
				nprobe = 8
			}
			hits, err := index.SearchInSpace(
				ctx,
				a.vector,
				c.cfg.EmbeddingSpace,
				c.cfg.MaxAnchors,
				nprobe,
			)
			if err == nil {
				rows := make([]string, len(hits))
				for i, hit := range hits {
					rows[i] = hit.Row
				}
				return rows, nil
			}
		}
	}
	cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, &embedpb.ScanRequest{RowPrefix: graphschema.EventRowPrefix, VectorSearch: &embedpb.VectorSearch{
		Query: PackVector(a.vector), TopK: int32(c.cfg.MaxAnchors),
		EmbeddingCf: graphschema.VectorCF(), Metric: "cosine",
		EmbeddingSpace: c.cfg.EmbeddingSpace,
	}})
	return uniqueRows(cells), err
}

// ivfIndex lazily loads (once) the trained IVF-PQ index for the configured
// table. It returns nil when no index has been trained yet, signaling callers
// to fall back to the brute-force path.
func (c *Client) ivfIndex(ctx context.Context) *IvfIndex {
	c.ivfOnce.Do(func() {
		c.ivf, c.ivfErr = LoadIvfIndexInSpace(
			ctx, c.cfg.Store, c.cfg.Table, c.cfg.EmbeddingSpace)
	})
	return c.ivf
}

func RRF(lists [][]string, k float64) map[string]float64 {
	if k <= 0 {
		k = 60
	}
	out := map[string]float64{}
	for _, list := range lists {
		seen := map[string]bool{}
		for rank, row := range list {
			if row == "" || seen[row] {
				continue
			}
			seen[row] = true
			out[row] += 1.0 / (k + float64(rank+1))
		}
	}
	return out
}

func (c *Client) beam(ctx context.Context, anchors []string, a analysis) ([]ScoredNode, error) {
	weights := intentWeights(a.intent)
	seen := map[string]ScoredNode{}
	frontier := make([]ScoredNode, 0, len(anchors))
	for i, row := range anchors {
		n := ScoredNode{Row: row, ID: idFromRow(row), Score: float32(1.0 / (1 + float32(i)))}
		frontier = append(frontier, n)
		seen[row] = n
	}
	for depth := 0; depth < c.cfg.MaxDepth && len(frontier) > 0; depth++ {
		var next []ScoredNode
		for _, edgeKind := range orderedEdges(weights) {
			var anchorRows [][]byte
			for _, n := range frontier {
				anchorRows = append(anchorRows, []byte(n.Row))
			}
			// EdgeWeights are intentionally omitted: each pass scans a single
			// EdgeCf (relationship is implied by the column family, not encoded
			// in the edge token), and the engine's EdgeExpand iterator only
			// applies relationship-keyed weights when a fieldSep splits the
			// token. The per-relationship weight is instead applied below in
			// scoring (weights[edgeKind]).
			cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, &embedpb.ScanRequest{EdgeExpand: &embedpb.EdgeExpand{AnchorRows: anchorRows, EdgeCf: graphschema.EdgeCF(edgeKind), PrimaryPrefix: []byte(graphschema.EventRowPrefix), MaxHops: 1}})
			if err != nil {
				return nil, err
			}
			for _, n := range rowsToNodes(cells) {
				if _, ok := seen[n.Row]; ok {
					continue
				}
				sim := cosine(a.vector, nVec(n))
				n.Score = weights[edgeKind]*0.7 + sim*0.3 + float32(c.cfg.MaxDepth-depth)*0.01
				seen[n.Row] = n
				next = append(next, n)
			}
		}
		frontier = trimNodes(next, c.cfg.BeamWidth)
	}
	// hydrate anchors and merge all seen nodes.
	for _, row := range anchors {
		cells, err := c.cfg.Store.Scan(ctx, c.cfg.Table, &embedpb.ScanRequest{StartRow: []byte(row), StartInclusive: true, EndRow: []byte(row), EndInclusive: true})
		if err != nil {
			return nil, err
		}
		for _, n := range rowsToNodes(cells) {
			old := seen[row]
			n.Score = old.Score
			seen[row] = n
		}
	}
	var out []ScoredNode
	for _, n := range seen {
		if n.Content != "" {
			out = append(out, n)
		}
	}
	return orderNodes(out, a.intent), nil
}

func Synthesize(nodes []ScoredNode, intent Intent, budget int) string {
	if budget <= 0 {
		budget = 160
	}
	nodes = orderNodes(append([]ScoredNode(nil), nodes...), intent)
	used := 0
	var b strings.Builder
	for _, n := range nodes {
		words := strings.Fields(n.Content)
		if len(words) == 0 {
			continue
		}
		if used+len(words) > budget {
			if used >= budget/2 {
				continue
			}
			words = words[:max(0, budget-used)]
		}
		if len(words) == 0 {
			continue
		}
		fmt.Fprintf(&b, "[<t:%d> %s <ref:%s>]\n", n.Timestamp, strings.Join(words, " "), n.ID)
		used += len(words)
	}
	return strings.TrimSpace(b.String())
}

func uniqueRows(cells []*embedpb.Cell) []string {
	seen := map[string]bool{}
	var rows []string
	for _, c := range cells {
		r := string(c.Row)
		if r != "" && !seen[r] {
			seen[r] = true
			rows = append(rows, r)
		}
	}
	sort.Strings(rows)
	return rows
}
func idFromRow(row string) string {
	if id, ok := graphschema.ParseEventID([]byte(row)); ok {
		return id
	}
	return row
}
func nVec(n ScoredNode) []float32 { return nil }
func rowsToNodes(cells []*embedpb.Cell) []ScoredNode {
	by := map[string]*ScoredNode{}
	for _, c := range cells {
		row := string(c.Row)
		n := by[row]
		if n == nil {
			n = &ScoredNode{Row: row, ID: idFromRow(row)}
			by[row] = n
		}
		if string(c.ColumnFamily) == graphschema.ContentCFName {
			n.Content = string(c.Value)
		}
		if string(c.ColumnFamily) == graphschema.AttributeCFName && string(c.ColumnQualifier) == "timestamp_ms" {
			if ts, err := strconv.ParseInt(string(c.Value), 10, 64); err == nil {
				n.Timestamp = ts
			}
		}
	}
	var out []ScoredNode
	for _, n := range by {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Row < out[j].Row })
	return out
}
func intentWeights(in Intent) map[graphschema.EdgeType]float32 {
	m := map[graphschema.EdgeType]float32{graphschema.Temporal: .5, graphschema.Causal: .5, graphschema.Semantic: .5, graphschema.Entity: .5}
	switch in {
	case IntentWhy:
		m[graphschema.Causal] = 1.5
	case IntentWhen:
		m[graphschema.Temporal] = 1.5
	case IntentEntity:
		m[graphschema.Entity] = 1.5
	default:
		m[graphschema.Semantic] = 1.0
	}
	return m
}
func orderedEdges(w map[graphschema.EdgeType]float32) []graphschema.EdgeType {
	e := []graphschema.EdgeType{graphschema.Temporal, graphschema.Causal, graphschema.Semantic, graphschema.Entity}
	sort.Slice(e, func(i, j int) bool {
		if w[e[i]] == w[e[j]] {
			return e[i] < e[j]
		}
		return w[e[i]] > w[e[j]]
	})
	return e
}
func trimNodes(nodes []ScoredNode, n int) []ScoredNode {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Score == nodes[j].Score {
			return nodes[i].Row < nodes[j].Row
		}
		return nodes[i].Score > nodes[j].Score
	})
	if len(nodes) > n {
		return nodes[:n]
	}
	return nodes
}
func orderNodes(nodes []ScoredNode, intent Intent) []ScoredNode {
	sort.Slice(nodes, func(i, j int) bool {
		if intent == IntentWhen {
			if nodes[i].Timestamp == nodes[j].Timestamp {
				return nodes[i].Row < nodes[j].Row
			}
			return nodes[i].Timestamp < nodes[j].Timestamp
		}
		if intent == IntentWhy {
			if nodes[i].Score == nodes[j].Score {
				return nodes[i].Row < nodes[j].Row
			}
			return nodes[i].Score > nodes[j].Score
		}
		if nodes[i].Score == nodes[j].Score {
			return nodes[i].Row < nodes[j].Row
		}
		return nodes[i].Score > nodes[j].Score
	})
	return nodes
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
