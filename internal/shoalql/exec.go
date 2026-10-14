package shoalql

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// exec.go runs a physical Plan. The executor depends only on the Backend/
// RowStream seam defined here, never on *engine.Engine directly, so it is
// unit-testable with an in-memory fake (enginebackend.go wires the real
// engine).

// Cell is a materialized key/value pair.
type Cell struct {
	Key   *iterrt.Key
	Value []byte
}

// Neighbor is one resolved out-edge target.
type Neighbor struct {
	Target []byte
	Value  []byte
}

// ScanRequest carries the pushdown parameters for a scan.
type ScanRequest struct {
	Stack          []iterrt.IterSpec
	ColumnFamilies [][]byte
	CFInclusive    bool
}

// RowStream is a forward cursor over cells in ascending key order. Its
// protocol mirrors the engine Scanner: Next reports whether a cell is
// available (without advancing), Key/Value read it, Advance moves on.
type RowStream interface {
	Next() bool
	Key() *iterrt.Key
	Value() []byte
	Advance() error
	Close()
}

// Backend is the physical execution surface shoalql lowers onto.
type Backend interface {
	// Scan opens a cell stream over table for the range with the given
	// pushdown stack and CF restriction.
	Scan(ctx context.Context, table string, r iterrt.Range, req ScanRequest) (RowStream, error)
	// LookupRows fetches the cells of the given rows (KNN hydration). Cells
	// may be returned in any order; each carries its full key.
	LookupRows(ctx context.Context, table string, rows [][]byte, req ScanRequest) ([]Cell, error)
	// Neighbors returns out-edges of each row over edgeCF, aligned to rows.
	Neighbors(ctx context.Context, table string, rows [][]byte, edgeCF []byte) ([][]Neighbor, error)
}

// VectorSearcher is the optional distributed semantic-index seam. Backends
// delegate to a format-neutral index manager while retaining ownership of
// source-table hydration and exact fallback execution.
type VectorSearcher interface {
	SearchVector(context.Context, VectorSearchRequest) ([]VectorHit, vectorindex.Evidence, error)
}

type ApproximateVectorBackend interface {
	VectorSearcher
}

type VectorExplainBackend interface {
	DescribeVector(context.Context, string) (vectorindex.Manifest, error)
}

type VectorSearchRequest struct {
	Index            string
	Query            []float32
	EmbeddingSpace   string
	TopK             int
	NProbe           int
	AsOf             *int64
	Authorizations   map[string]bool
	Freshness        vectorindex.Freshness
	ExactFallback    bool
	AllowedDocuments map[string]bool
}

type VectorHit struct {
	Row      []byte
	ID       string
	Score    float64
	Document vectorindex.DocumentRef
}

// NeighborRequestBackend optionally accepts scan semantics such as AS OF for
// graph traversal. The base Backend method remains source-compatible.
type NeighborRequestBackend interface {
	NeighborsWithRequest(
		ctx context.Context,
		table string,
		rows [][]byte,
		edgeCF []byte,
		req ScanRequest,
	) ([][]Neighbor, error)
}

// ValueKind tags a result cell's type.
type ValueKind int

const (
	VNull ValueKind = iota
	VStr
	VNum
	VList
)

// Value is one output cell.
type Value struct {
	Kind ValueKind
	Str  string
	Num  float64
	List []string
}

func strVal(s string) Value  { return Value{Kind: VStr, Str: s} }
func numVal(n float64) Value { return Value{Kind: VNum, Num: n} }
func nullVal() Value         { return Value{Kind: VNull} }

// String renders a value for display.
func (v Value) String() string {
	switch v.Kind {
	case VStr:
		return v.Str
	case VNum:
		return strconv.FormatFloat(v.Num, 'g', -1, 64)
	case VList:
		return "[" + strings.Join(v.List, ", ") + "]"
	default:
		return ""
	}
}

// Row is one output tuple, aligned to Result.Columns.
type Row []Value

// Result is a query result set.
type Result struct {
	Columns []string
	Rows    []Row
}

// Executor runs plans against a Backend.
type Executor struct {
	be Backend
}

// NewExecutor builds an executor over be.
func NewExecutor(be Backend) *Executor { return &Executor{be: be} }

// Run executes plan p and returns its result set.
func (e *Executor) Run(ctx context.Context, p *Plan) (*Result, error) {
	if p.Explain {
		return e.explain(p)
	}
	switch p.Shape {
	case ShapeAggregate:
		return e.runAggregate(ctx, p)
	case ShapeVectorKNN:
		return e.runVectorKNN(ctx, p)
	case ShapeDocument:
		return e.runDocument(ctx, p)
	default:
		return e.runScan(ctx, p)
	}
}

// materializedRow groups a logical row's cells under its row key.
type materializedRow struct {
	row   []byte
	cells []Cell
}

func (e *Executor) runScan(ctx context.Context, p *Plan) (*Result, error) {
	req := ScanRequest{Stack: p.Stack, ColumnFamilies: p.ColumnFamilies, CFInclusive: p.CFInclusive}
	stream, err := e.be.Scan(ctx, p.Table, p.Range, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	rows, err := groupRows(stream, p.Limit, p.Residual)
	if err != nil {
		return nil, err
	}
	return e.project(ctx, p, rows)
}

func (e *Executor) runVectorKNN(ctx context.Context, p *Plan) (*Result, error) {
	if p.VectorMode == VectorApproximate {
		if backend, ok := e.be.(ApproximateVectorBackend); ok {
			result, err := e.runApproxVectorKNN(ctx, p, backend)
			if err == nil {
				return result, nil
			}
			if !errors.Is(err, vectorindex.ErrExactFallback) || !p.VectorExactFallback {
				return nil, err
			}
		} else if !p.VectorExactFallback {
			return nil, fmt.Errorf("shoalql: approximate vector backend unavailable")
		}
	}
	return e.runExactVectorKNN(ctx, p)
}

func (e *Executor) runExactVectorKNN(ctx context.Context, p *Plan) (*Result, error) {
	req := ScanRequest{Stack: p.Stack, ColumnFamilies: p.ColumnFamilies, CFInclusive: p.CFInclusive}
	stream, err := e.be.Scan(ctx, p.Table, p.Range, req)
	if err != nil {
		return nil, err
	}
	// Collect KNN hits in score order (one embedding cell + score per row).
	type hit struct {
		row   []byte
		score float64
	}
	var hits []hit
	seen := map[string]bool{}
	for stream.Next() {
		k := stream.Key()
		rk := string(k.Row)
		if !seen[rk] {
			seen[rk] = true
			hits = append(hits, hit{row: append([]byte(nil), k.Row...), score: decodeScore(stream.Value())})
		}
		if err := stream.Advance(); err != nil {
			stream.Close()
			return nil, err
		}
	}
	stream.Close()

	// Materialize cells for the hit rows (order-preserving).
	byRow := map[string][]Cell{}
	if p.NeedsHydration && len(hits) > 0 {
		rowKeys := make([][]byte, len(hits))
		for i, h := range hits {
			rowKeys[i] = h.row
		}
		cells, err := e.be.LookupRows(ctx, p.Table, rowKeys, ScanRequest{
			Stack: stackBefore(p.Stack, iterrt.IterVectorKNN),
		})
		if err != nil {
			return nil, err
		}
		for _, c := range cells {
			rk := string(c.Key.Row)
			byRow[rk] = append(byRow[rk], c)
		}
	}

	res := &Result{Columns: columnNames(p.Projection)}
	for _, h := range hits {
		if !residualPass(byRow[string(h.row)], p.Residual) {
			continue
		}
		row, err := e.projectRow(ctx, p, h.row, byRow[string(h.row)], &h.score)
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, row)
		if p.Limit != nil && len(res.Rows) >= *p.Limit {
			break
		}
	}
	return res, nil
}

func (e *Executor) runApproxVectorKNN(ctx context.Context, p *Plan, backend ApproximateVectorBackend) (*Result, error) {
	hits, _, err := backend.SearchVector(ctx, VectorSearchRequest{
		Index: p.VectorIndex, Query: append([]float32(nil), p.VectorQuery...),
		EmbeddingSpace: p.VectorEmbeddingSpace,
		TopK:           p.VectorTopK, NProbe: p.VectorNProbe, AsOf: p.AsOf,
		Freshness: p.VectorFreshness, ExactFallback: p.VectorExactFallback,
	})
	if err != nil {
		return nil, err
	}
	byRow := map[string][]Cell{}
	if p.NeedsHydration && len(hits) > 0 {
		rows := make([][]byte, len(hits))
		for i, hit := range hits {
			rows[i] = hit.Row
		}
		cells, err := e.be.LookupRows(ctx, p.Table, rows, ScanRequest{Stack: stackBefore(p.Stack, iterrt.IterVectorKNN)})
		if err != nil {
			return nil, err
		}
		for _, cell := range cells {
			byRow[string(cell.Key.Row)] = append(byRow[string(cell.Key.Row)], cell)
		}
	}
	res := &Result{Columns: columnNames(p.Projection)}
	for _, hit := range hits {
		if !residualPass(byRow[string(hit.Row)], p.Residual) {
			continue
		}
		score := hit.Score
		row, err := e.projectRow(ctx, p, hit.Row, byRow[string(hit.Row)], &score)
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, row)
		if p.Limit != nil && len(res.Rows) >= *p.Limit {
			break
		}
	}
	return res, nil
}

func (e *Executor) runAggregate(ctx context.Context, p *Plan) (*Result, error) {
	req := ScanRequest{Stack: p.Stack, ColumnFamilies: p.ColumnFamilies, CFInclusive: p.CFInclusive}
	stream, err := e.be.Scan(ctx, p.Table, p.Range, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	res := &Result{Columns: columnNames(p.Projection)}
	for stream.Next() {
		k := stream.Key()
		group := string(k.ColumnQualifier)
		count, _ := strconv.ParseFloat(string(stream.Value()), 64)
		row := make(Row, len(p.Projection))
		for i, c := range p.Projection {
			switch c.Kind {
			case OutGroupKey:
				row[i] = strVal(group)
			case OutCount:
				row[i] = numVal(count)
			default:
				row[i] = nullVal()
			}
		}
		res.Rows = append(res.Rows, row)
		if err := stream.Advance(); err != nil {
			return nil, err
		}
		if p.Limit != nil && len(res.Rows) >= *p.Limit {
			break
		}
	}
	return res, nil
}

// groupRows collects a cell stream into materialized rows, applying residual
// filters and the row limit. Cells arrive in ascending key order, so cells of
// the same row are contiguous.
func groupRows(stream RowStream, limit *int, residual []ResidualFilter) ([]materializedRow, error) {
	var out []materializedRow
	var cur *materializedRow
	flush := func() bool {
		if cur == nil {
			return true
		}
		if residualPass(cur.cells, residual) {
			out = append(out, *cur)
		}
		cur = nil
		return limit == nil || len(out) < *limit
	}
	for stream.Next() {
		k := stream.Key()
		if cur == nil || string(cur.row) != string(k.Row) {
			if !flush() {
				return out, nil
			}
			cur = &materializedRow{row: append([]byte(nil), k.Row...)}
		}
		cur.cells = append(cur.cells, Cell{
			Key:   k.Clone(),
			Value: append([]byte(nil), stream.Value()...),
		})
		if err := stream.Advance(); err != nil {
			return nil, err
		}
	}
	flush()
	return out, nil
}

func (e *Executor) project(ctx context.Context, p *Plan, rows []materializedRow) (*Result, error) {
	res := &Result{Columns: columnNames(p.Projection)}
	for i := range rows {
		row, err := e.projectRow(ctx, p, rows[i].row, rows[i].cells, nil)
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// projectRow builds one output tuple. score is non-nil for KNN results.
func (e *Executor) projectRow(ctx context.Context, p *Plan, rowKey []byte, cells []Cell, score *float64) (Row, error) {
	row := make(Row, len(p.Projection))
	for i, c := range p.Projection {
		switch c.Kind {
		case OutRowKey:
			row[i] = strVal(stripPrefix(rowKey, p.RowPrefix))
		case OutCell:
			if cell, ok := findCell(cells, c.CF, c.CQ); ok {
				row[i] = strVal(string(cell.Value))
			} else {
				row[i] = nullVal()
			}
		case OutScore:
			if score != nil {
				row[i] = numVal(*score)
			} else {
				row[i] = nullVal()
			}
		case OutExpand:
			var nbrs [][]Neighbor
			var err error
			req := ScanRequest{Stack: stackBefore(p.Stack, iterrt.IterVectorKNN)}
			if backend, ok := e.be.(NeighborRequestBackend); ok {
				nbrs, err = backend.NeighborsWithRequest(ctx, p.Table, [][]byte{rowKey}, c.EdgeCF, req)
			} else {
				nbrs, err = e.be.Neighbors(ctx, p.Table, [][]byte{rowKey}, c.EdgeCF)
			}
			if err != nil {
				return nil, err
			}
			var targets []string
			if len(nbrs) > 0 {
				for _, n := range nbrs[0] {
					targets = append(targets, stripPrefix(n.Target, p.RowPrefix))
				}
			}
			row[i] = Value{Kind: VList, List: targets}
		default:
			row[i] = nullVal()
		}
	}
	return row, nil
}

func stackBefore(stack []iterrt.IterSpec, terminal string) []iterrt.IterSpec {
	out := make([]iterrt.IterSpec, 0, len(stack))
	for _, spec := range stack {
		if spec.Name == terminal {
			break
		}
		out = append(out, spec)
	}
	return out
}

// residualPass reports whether a row's cells satisfy every residual filter.
func residualPass(cells []Cell, filters []ResidualFilter) bool {
	for _, f := range filters {
		cell, ok := findCell(cells, f.CF, f.CQ)
		if !ok {
			return false
		}
		val := string(cell.Value)
		if f.IsMatch {
			if !matchAllTerms(val, f.Terms) {
				return false
			}
			continue
		}
		if !compareStr(val, f.Op, f.Str) {
			return false
		}
	}
	return true
}

func matchAllTerms(text, terms string) bool {
	lt := strings.ToLower(text)
	for _, term := range strings.Fields(terms) {
		if !strings.Contains(lt, strings.ToLower(term)) {
			return false
		}
	}
	return true
}

func compareStr(got string, op CompareOp, want string) bool {
	switch op {
	case OpEq:
		return got == want
	case OpGE:
		return got >= want
	case OpGT:
		return got > want
	case OpLE:
		return got <= want
	case OpLT:
		return got < want
	case OpLike:
		return strings.HasPrefix(got, strings.TrimSuffix(want, "%"))
	default:
		return false
	}
}

func findCell(cells []Cell, cf, cq []byte) (Cell, bool) {
	for _, c := range cells {
		if !bytesEq(c.Key.ColumnFamily, cf) {
			continue
		}
		if len(cq) > 0 && !bytesEq(c.Key.ColumnQualifier, cq) {
			continue
		}
		return c, true
	}
	return Cell{}, false
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stripPrefix(row, prefix []byte) string {
	if len(prefix) > 0 && len(row) >= len(prefix) && bytesEq(row[:len(prefix)], prefix) {
		return string(row[len(prefix):])
	}
	return string(row)
}

func columnNames(proj []OutputColumn) []string {
	names := make([]string, len(proj))
	for i, c := range proj {
		names[i] = c.Name
	}
	return names
}

// decodeScore reads a VectorKNN score value: a big-endian float32. Falls back
// to a decimal string if the length is not 4.
func decodeScore(v []byte) float64 {
	if len(v) == 4 {
		return float64(math.Float32frombits(binary.BigEndian.Uint32(v)))
	}
	f, _ := strconv.ParseFloat(string(v), 64)
	return f
}
