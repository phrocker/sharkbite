package agentmem

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
)

type FakeStore struct {
	mu     sync.RWMutex
	tables map[string]map[string][]*embedpb.Cell
}

func NewFakeStore() *FakeStore { return &FakeStore{tables: map[string]map[string][]*embedpb.Cell{}} }
func (s *FakeStore) CreateTable(_ context.Context, table string, _ []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tables[table] == nil {
		s.tables[table] = map[string][]*embedpb.Cell{}
	}
	return nil
}

func (s *FakeStore) CreateTableWithEmbedding(
	ctx context.Context,
	table string,
	splits []string,
	defaultEmbedding string,
) error {
	state, err := embeddingspace.Parse(defaultEmbedding)
	if err != nil {
		return err
	}
	if state == (embeddingspace.FileState{}) {
		return embeddingspace.ErrQueryMetadataMissing
	}
	return s.CreateTable(ctx, table, splits)
}

func (s *FakeStore) Flush(context.Context, string) error { return nil }
func (s *FakeStore) Write(_ context.Context, table string, muts []*embedpb.Mutation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tables[table] == nil {
		s.tables[table] = map[string][]*embedpb.Cell{}
	}
	for _, m := range muts {
		row := string(m.Row)
		for _, e := range m.Entries {
			if e.Delete {
				continue
			}
			c := &embedpb.Cell{Row: append([]byte(nil), m.Row...), ColumnFamily: append([]byte(nil), e.ColumnFamily...), ColumnQualifier: append([]byte(nil), e.ColumnQualifier...), ColumnVisibility: append([]byte(nil), e.ColumnVisibility...), Timestamp: e.Timestamp, Value: append([]byte(nil), e.Value...)}
			s.tables[table][row] = append(s.tables[table][row], c)
		}
	}
	return nil
}
func (s *FakeStore) Scan(_ context.Context, table string, req *embedpb.ScanRequest) ([]*embedpb.Cell, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := s.tables[table]
	if rows == nil {
		return nil, nil
	}
	if req.TermFilter != nil {
		return s.term(rows, req.TermFilter), nil
	}
	if req.VectorSearch != nil {
		return s.vector(rows, req), nil
	}
	if req.EdgeExpand != nil {
		return s.edge(rows, req.EdgeExpand), nil
	}
	return sortedCells(filterRows(rows, req)), nil
}
func filterRows(rows map[string][]*embedpb.Cell, req *embedpb.ScanRequest) map[string][]*embedpb.Cell {
	out := map[string][]*embedpb.Cell{}
	for r, c := range rows {
		ok := true
		if req.RowPrefix != "" {
			ok = strings.HasPrefix(r, req.RowPrefix)
		} else {
			if len(req.StartRow) > 0 {
				cmp := strings.Compare(r, string(req.StartRow))
				ok = ok && (cmp > 0 || cmp == 0 && req.StartInclusive)
			}
			if len(req.EndRow) > 0 {
				cmp := strings.Compare(r, string(req.EndRow))
				ok = ok && (cmp < 0 || cmp == 0 && req.EndInclusive)
			}
		}
		if ok {
			out[r] = c
		}
	}
	return out
}
func sortedCells(rows map[string][]*embedpb.Cell) []*embedpb.Cell {
	keys := make([]string, 0, len(rows))
	for r := range rows {
		keys = append(keys, r)
	}
	sort.Strings(keys)
	var out []*embedpb.Cell
	for _, r := range keys {
		cells := append([]*embedpb.Cell(nil), rows[r]...)
		sort.Slice(cells, func(i, j int) bool {
			if string(cells[i].ColumnFamily) == string(cells[j].ColumnFamily) {
				return string(cells[i].ColumnQualifier) < string(cells[j].ColumnQualifier)
			}
			return string(cells[i].ColumnFamily) < string(cells[j].ColumnFamily)
		})
		out = append(out, cells...)
	}
	return out
}
func (s *FakeStore) term(rows map[string][]*embedpb.Cell, tf *embedpb.TermFilter) []*embedpb.Cell {
	ids := map[string]bool{}
	for _, tr := range tf.TermRows {
		for _, c := range rows[string(tr)] {
			if len(tf.PostingCf) > 0 && string(c.ColumnFamily) != string(tf.PostingCf) {
				continue
			}
			ids[string(tf.PrimaryPrefix)+string(c.ColumnQualifier)] = true
		}
	}
	sub := map[string][]*embedpb.Cell{}
	for id := range ids {
		if c := rows[id]; c != nil {
			sub[id] = c
		}
	}
	return sortedCells(sub)
}
func (s *FakeStore) vector(rows map[string][]*embedpb.Cell, req *embedpb.ScanRequest) []*embedpb.Cell {
	q, _ := UnpackVector(req.VectorSearch.Query)
	type hit struct {
		c     *embedpb.Cell
		score float32
	}
	var hits []hit
	for _, cells := range filterRows(rows, req) {
		for _, c := range cells {
			if len(req.VectorSearch.EmbeddingCf) > 0 && string(c.ColumnFamily) != string(req.VectorSearch.EmbeddingCf) {
				continue
			}
			v, err := UnpackVector(c.Value)
			if err != nil {
				continue
			}
			sc := cosine(q, v)
			hits = append(hits, hit{&embedpb.Cell{Row: c.Row, ColumnFamily: c.ColumnFamily, ColumnQualifier: c.ColumnQualifier, Timestamp: c.Timestamp, Value: PackVector([]float32{sc})[:4]}, sc})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return string(hits[i].c.Row) < string(hits[j].c.Row)
		}
		return hits[i].score > hits[j].score
	})
	k := int(req.VectorSearch.TopK)
	if k <= 0 {
		k = 10
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	out := make([]*embedpb.Cell, len(hits))
	for i, h := range hits {
		out[i] = h.c
	}
	return out
}
func (s *FakeStore) edge(rows map[string][]*embedpb.Cell, ee *embedpb.EdgeExpand) []*embedpb.Cell {
	found := map[string][]*embedpb.Cell{}
	frontier := append([][]byte(nil), ee.AnchorRows...)
	hops := int(ee.MaxHops)
	if hops <= 0 {
		hops = 1
	}
	for _, a := range ee.AnchorRows {
		if ee.IncludeAnchors {
			if c := rows[string(a)]; c != nil {
				found[string(a)] = c
			}
		}
	}
	for h := 0; h < hops; h++ {
		var next [][]byte
		for _, a := range frontier {
			for _, c := range rows[string(a)] {
				if len(ee.EdgeCf) > 0 && string(c.ColumnFamily) != string(ee.EdgeCf) {
					continue
				}
				target := string(ee.PrimaryPrefix) + string(c.ColumnQualifier)
				if tc := rows[target]; tc != nil {
					if found[target] == nil {
						found[target] = tc
						next = append(next, []byte(target))
					}
				}
			}
		}
		frontier = next
	}
	return sortedCells(found)
}
