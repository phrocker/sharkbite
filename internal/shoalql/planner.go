package shoalql

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// planner.go lowers a parsed *SelectStmt against a TableBinding into a
// physical Plan the executor runs. It is data-model-agnostic: every layout
// decision (row prefixes, embedding CF, edge CFs) comes from the binding, so
// the same planner serves the graph binding today and a DataWave-style
// document binding later.

// Embedder turns query text into a vector, used for `ORDER BY col <-> 'text'`.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

type embeddingSpaceIdentityProvider interface {
	EmbeddingSpaceIdentity() (string, error)
}

// PlanOptions carries runtime inputs needed to finish lowering.
type PlanOptions struct {
	// Params supplies values for `:name` bind parameters (vector params).
	Params map[string][]float32
	// Embedder resolves `ORDER BY col <-> 'text'`. May be nil if no query
	// uses the text form.
	Embedder Embedder
	// Vector selects exact or approximate execution. The zero value remains
	// exact for compatibility and to prevent silently choosing a derived index.
	Vector VectorOptions
}

type VectorMode string

const (
	VectorExact       VectorMode = "exact"
	VectorApproximate VectorMode = "approximate"
)

type VectorOptions struct {
	Mode  VectorMode
	Index string
	// EmbeddingSpace is the stable identity of the model space that produced
	// a supplied literal or parameter vector. Text-query identity is derived
	// from its identity-reporting Embedder and, when this value is also set,
	// verified against it. Dimensionality alone is never treated as identity.
	EmbeddingSpace string
	NProbe         int
	Freshness      vectorindex.Freshness
	ExactFallback  bool
}

// OutColKind classifies how the executor materializes an output column.
type OutColKind int

const (
	// OutRowKey emits the row key with the table prefix stripped.
	OutRowKey OutColKind = iota
	// OutCell emits the value of a cell in CF (optionally CQ).
	OutCell
	// OutCount emits an aggregation count (GraphAggregation result).
	OutCount
	// OutExpand emits neighbor ids reached over EdgeCF.
	OutExpand
	// OutScore emits the vector similarity score attached by VectorKNN.
	OutScore
	// OutGroupKey emits an aggregation group value (the emitted cell CQ).
	OutGroupKey
	// OutDocID emits a reconstructed document's uid (from its event CF).
	OutDocID
	// OutDocType emits a reconstructed document's datatype (from its event CF).
	OutDocType
	// OutDocField emits a reconstructed document field value (from an event
	// cell qualified FIELD\x00value). Field names the physical FIELD.
	OutDocField
)

// OutputColumn describes one projected result column.
type OutputColumn struct {
	Name string
	Kind OutColKind
	CF   []byte // for OutCell
	CQ   []byte // for OutCell; nil = whole-CF
	// EdgeCF is set for OutExpand.
	EdgeCF []byte
	// SourceCol is the logical column an OutExpand hops from (its row-key
	// value identifies the anchor).
	SourceCol string
	// Field is the physical FIELD name for OutDocField.
	Field string
}

// PlanShape tags the overall execution strategy.
type PlanShape int

const (
	// ShapeScan is a range scan (optionally with AsOf/residual filters).
	ShapeScan PlanShape = iota
	// ShapeVectorKNN is a top-k nearest-neighbor search followed by row
	// hydration for the projection.
	ShapeVectorKNN
	// ShapeAggregate is a pushed-down GROUP BY count via GraphAggregation.
	ShapeAggregate
	// ShapeDocument is a DataWave-style document retrieval: candidate shards
	// are resolved from the global index, matching documents are resolved and
	// reconstructed by the DocumentIndexIterator, then grouped per document.
	ShapeDocument
)

// ResidualFilter is a predicate the planner could not push into the row range
// or an index, resolved to physical cell coordinates so the executor can
// evaluate it without the binding.
type ResidualFilter struct {
	CF  []byte
	CQ  []byte // nil = match any CQ in the CF
	Op  CompareOp
	Str string
	// IsMatch marks a MATCH(col,'terms') keyword filter (all terms must
	// appear as substrings of the cell value).
	IsMatch bool
	Terms   string
}

// Plan is the physical query plan.
type Plan struct {
	Shape PlanShape
	// Explain causes the executor to describe this plan without reading data.
	Explain       bool
	ExplainFormat ExplainFormat

	Table          string // physical engine table
	RowPrefix      []byte // table row prefix (for stripping id output)
	Range          iterrt.Range
	ColumnFamilies [][]byte // nil = no CF restriction
	CFInclusive    bool
	Stack          []iterrt.IterSpec // top-of-stack pushdown iterators
	AsOf           *int64
	Limit          *int

	Projection []OutputColumn
	// Residual holds filters the executor applies to materialized rows.
	Residual []ResidualFilter

	// GroupByColumn is set for ShapeAggregate.
	GroupByColumn string
	// NeedsHydration is true when the primary scan yields only row keys
	// (ShapeVectorKNN) and the executor must re-fetch cells to project.
	NeedsHydration bool

	// --- ShapeDocument fields ---

	// IndexTable is the physical global forward-index table (row=value).
	IndexTable string
	// DocTerms are the indexed equality/token predicates driving shard
	// resolution and the DocumentIndexIterator.
	DocTerms []DocTerm
	// DocBoolOr selects OR (union) over the terms instead of AND (intersect).
	// The current grammar is AND-only, so this is false today.
	DocBoolOr bool
	// DocResidual holds id/type equality filters applied to reconstructed
	// documents (these are not in the field index).
	DocResidual []DocResidual
	// DocStar is true for `SELECT *` over documents: the executor emits id,
	// type, and the union of all field names encountered.
	DocStar bool

	VectorMode           VectorMode
	VectorIndex          string
	VectorQuery          []float32
	VectorEmbeddingSpace string
	VectorTopK           int
	VectorNProbe         int
	VectorFreshness      vectorindex.Freshness
	VectorExactFallback  bool
}

// DocTerm is one indexed document predicate: FIELD = Value (exact).
type DocTerm struct {
	Field string
	Value string
}

// DocResidual is an id/type equality filter applied after reconstruction.
type DocResidual struct {
	// Special is "id" (uid) or "type" (datatype).
	Special string
	Value   string
}

// PlanQuery lowers stmt against binding.
func PlanQuery(ctx context.Context, stmt *SelectStmt, binding TableBinding, opts PlanOptions) (*Plan, error) {
	if stmt == nil {
		return nil, fmt.Errorf("shoalql: nil statement")
	}
	p := &Plan{
		Explain:       stmt.Explain,
		ExplainFormat: stmt.ExplainFormat,
		Table:         binding.PhysicalTable(),
		RowPrefix:     binding.RowPrefix(),
		AsOf:          stmt.AsOf,
		Limit:         stmt.Limit,
	}

	// AS OF -> AsOf iterator (shared by both the graph and document paths;
	// it sits directly above the leaf so higher iterators see time-filtered
	// cells).
	if stmt.AsOf != nil {
		p.Stack = append(p.Stack, iterrt.IterSpec{
			Name:    iterrt.IterAsOf,
			Options: map[string]string{iterrt.AsOfOption: fmt.Sprintf("%d", *stmt.AsOf)},
		})
	}

	// Document model (DataWave-style sharded documents) takes a dedicated
	// lowering path: row=shard, dynamic fields, index-driven resolution.
	if dm, ok := binding.(documentModel); ok {
		return planDocument(ctx, stmt, dm, opts, p)
	}

	// Row range: start from the table's prefix, narrow by id predicates.
	lo, hi, residual, err := rowBounds(stmt.Where, binding)
	if err != nil {
		return nil, err
	}
	p.Residual = residual
	p.Range = bytesRange(lo, hi)

	// Projection.
	proj, err := projection(stmt, binding)
	if err != nil {
		return nil, err
	}
	p.Projection = proj

	// GROUP BY count(*) -> aggregation.
	if stmt.GroupBy != "" {
		return planAggregate(stmt, binding, p)
	}

	// ORDER BY col <-> vec -> vector KNN.
	if stmt.Order != nil {
		return planVectorKNN(ctx, stmt, binding, opts, p)
	}

	p.Shape = ShapeScan
	p.ColumnFamilies, p.CFInclusive = cfRestriction(stmt, proj)
	return p, nil
}

func planAggregate(stmt *SelectStmt, binding TableBinding, p *Plan) (*Plan, error) {
	if stmt.Order != nil {
		return nil, fmt.Errorf("shoalql: ORDER BY not supported with GROUP BY")
	}
	countName := ""
	for _, it := range stmt.Columns {
		if it.CountStar {
			countName = outName(it)
		}
	}
	if countName == "" {
		return nil, fmt.Errorf("shoalql: GROUP BY requires count(*)")
	}
	mode, ok := aggGroupMode(stmt.GroupBy)
	if !ok {
		return nil, fmt.Errorf("shoalql: GROUP BY %q not supported (use id, cf, cq, cv, or type)", stmt.GroupBy)
	}
	p.Stack = append(p.Stack, iterrt.IterSpec{
		Name: iterrt.IterGraphAggregation,
		Options: map[string]string{
			iterrt.GraphAggregationOp:      "count",
			iterrt.GraphAggregationGroupBy: mode,
		},
	})
	p.Shape = ShapeAggregate
	p.GroupByColumn = stmt.GroupBy
	// Aggregation emits row="_agg", cf="result", cq=<group>, value=<count>.
	p.Projection = []OutputColumn{
		{Name: stmt.GroupBy, Kind: OutGroupKey},
		{Name: countName, Kind: OutCount},
	}
	return p, nil
}

// planDocument lowers a document query. WHERE predicates become indexed terms
// (field=value and MATCH tokens) plus id/type residuals; the executor resolves
// candidate shards from the global index, runs the DocumentIndexIterator, and
// reconstructs matching documents.
func planDocument(ctx context.Context, stmt *SelectStmt, dm documentModel, opts PlanOptions, p *Plan) (*Plan, error) {
	if stmt.GroupBy != "" {
		return nil, fmt.Errorf("shoalql: GROUP BY not supported on document tables")
	}
	p.Shape = ShapeDocument
	p.Table = dm.DocumentTable()
	p.IndexTable = dm.GlobalIndexTable()
	p.Range = iterrt.InfiniteRange()

	var terms []DocTerm
	var residual []DocResidual
	for _, pr := range stmt.Where {
		field, special := dm.FieldColumn(pr.Column)
		switch pr.Kind {
		case PredMatch:
			if special != "" {
				return nil, fmt.Errorf("shoalql: MATCH not supported on %q", pr.Column)
			}
			for _, tok := range strings.Fields(pr.MatchTerms) {
				terms = append(terms, DocTerm{Field: field, Value: tok})
			}
		case PredCompare:
			if pr.Op != OpEq {
				return nil, fmt.Errorf("shoalql: document filters support = and MATCH only (column %q)", pr.Column)
			}
			val := literalString(pr.Value)
			switch special {
			case "id", "type":
				residual = append(residual, DocResidual{Special: special, Value: val})
			default:
				terms = append(terms, DocTerm{Field: field, Value: val})
			}
		default:
			return nil, fmt.Errorf("shoalql: unsupported document predicate on %q", pr.Column)
		}
	}
	if len(terms) == 0 && stmt.Order == nil {
		return nil, fmt.Errorf("shoalql: document query requires an indexed predicate (field = value or MATCH)")
	}
	p.DocTerms = terms
	p.DocResidual = residual

	proj, star, err := documentProjection(stmt, dm)
	if err != nil {
		return nil, err
	}
	p.Projection = proj
	p.DocStar = star
	if stmt.Order != nil {
		vectorColumn, vectorIndex := dm.DocumentVector()
		if stmt.Order.Column != vectorColumn {
			return nil, fmt.Errorf("shoalql: ORDER BY <-> requires document vector column %q, got %q", vectorColumn, stmt.Order.Column)
		}
		if opts.Vector.Mode != VectorApproximate {
			return nil, fmt.Errorf("shoalql: document semantic search requires explicit approximate vector mode")
		}
		embeddingSpace, err := resolveVectorEmbeddingSpace(
			stmt.Order.Target, opts, "plan approximate vector query")
		if err != nil {
			return nil, err
		}
		vec, err := resolveVector(ctx, stmt.Order.Target, opts)
		if err != nil {
			return nil, err
		}
		topK := 10
		if p.Limit != nil && *p.Limit > 0 {
			topK = *p.Limit
		}
		p.VectorMode = VectorApproximate
		p.VectorIndex = vectorIndex
		if opts.Vector.Index != "" {
			p.VectorIndex = opts.Vector.Index
		}
		p.VectorQuery = append([]float32(nil), vec...)
		p.VectorEmbeddingSpace = embeddingSpace
		p.VectorTopK = topK
		p.VectorNProbe = opts.Vector.NProbe
		p.VectorFreshness = opts.Vector.Freshness
		p.VectorExactFallback = opts.Vector.ExactFallback
	}
	return p, nil
}

// documentProjection lowers the SELECT list for a document query. Star defers
// column discovery to the executor (fields are dynamic).
func documentProjection(stmt *SelectStmt, dm documentModel) ([]OutputColumn, bool, error) {
	if stmt.Star {
		return nil, true, nil
	}
	out := make([]OutputColumn, 0, len(stmt.Columns))
	for _, it := range stmt.Columns {
		if it.CountStar || it.isExpand() {
			return nil, false, fmt.Errorf("shoalql: count(*)/expand() not supported on document tables")
		}
		name := outName(it)
		field, special := dm.FieldColumn(it.Column)
		switch special {
		case "id":
			out = append(out, OutputColumn{Name: name, Kind: OutDocID})
		case "type":
			out = append(out, OutputColumn{Name: name, Kind: OutDocType})
		default:
			out = append(out, OutputColumn{Name: name, Kind: OutDocField, Field: field})
		}
	}
	return out, false, nil
}

// literalString renders a predicate literal as a string (values are compared
// as strings against the field index).
func literalString(l Literal) string {
	if l.Kind == LitString {
		return l.Str
	}
	return fmt.Sprintf("%v", l.Num)
}

// aggGroupMode maps a SQL GROUP BY column to a GraphAggregation groupBy mode.
// The iterator groups by physical key dimensions, not arbitrary attribute
// values (that awaits an index-backed document binding).
func aggGroupMode(col string) (string, bool) {
	switch col {
	case "id", "row":
		return "row", true
	case "cf":
		return "cf", true
	case "cq":
		return "cq", true
	case "cv":
		return "cv", true
	case "type":
		return "rowPrefix", true
	default:
		return "", false
	}
}

func planVectorKNN(ctx context.Context, stmt *SelectStmt, binding TableBinding, opts PlanOptions, p *Plan) (*Plan, error) {
	col, ok := binding.Column(stmt.Order.Column)
	if !ok || col.Role != RoleVector {
		return nil, fmt.Errorf("shoalql: ORDER BY <-> requires a vector column, got %q", stmt.Order.Column)
	}
	embeddingSpace, err := resolveVectorEmbeddingSpace(
		stmt.Order.Target, opts, "plan exact vector query")
	if err != nil {
		return nil, err
	}
	vec, err := resolveVector(ctx, stmt.Order.Target, opts)
	if err != nil {
		return nil, err
	}
	topK := 10
	if p.Limit != nil && *p.Limit > 0 {
		topK = *p.Limit
	}
	opt := map[string]string{
		iterrt.VectorKNNQuery:          packVecBE(vec),
		iterrt.VectorKNNEmbeddingSpace: embeddingSpace,
		iterrt.VectorKNNTopK:           fmt.Sprintf("%d", topK),
		iterrt.VectorKNNMetric:         "cosine",
	}
	if len(col.CF) > 0 {
		opt[iterrt.VectorKNNEmbeddingCF] = string(col.CF)
		p.ColumnFamilies = [][]byte{col.CF}
		p.CFInclusive = true
	}
	p.Stack = append(p.Stack, iterrt.IterSpec{Name: iterrt.IterVectorKNN, Options: opt})
	p.Shape = ShapeVectorKNN
	p.VectorMode = opts.Vector.Mode
	if p.VectorMode == "" {
		p.VectorMode = VectorExact
	}
	p.VectorIndex = opts.Vector.Index
	if p.VectorIndex == "" {
		p.VectorIndex = p.Table + "_ivf"
	}
	p.VectorQuery = append([]float32(nil), vec...)
	p.VectorEmbeddingSpace = embeddingSpace
	p.VectorTopK = topK
	p.VectorNProbe = opts.Vector.NProbe
	p.VectorFreshness = opts.Vector.Freshness
	p.VectorExactFallback = opts.Vector.ExactFallback
	// KNN returns only embedding cells + scores; hydrate for the rest of the
	// projection unless the projection is score-only.
	p.NeedsHydration = projectionNeedsHydration(p.Projection)
	return p, nil
}

// resolveVector turns a parsed VectorExpr into a concrete query vector.
func resolveVector(ctx context.Context, v VectorExpr, opts PlanOptions) ([]float32, error) {
	switch v.Kind {
	case VecLiteral:
		if len(v.Literal) == 0 {
			return nil, fmt.Errorf("shoalql: empty vector literal")
		}
		return v.Literal, nil
	case VecParam:
		vec, ok := opts.Params[v.Param]
		if !ok || len(vec) == 0 {
			return nil, fmt.Errorf("shoalql: missing or empty vector param :%s", v.Param)
		}
		return vec, nil
	case VecText:
		if opts.Embedder == nil {
			return nil, fmt.Errorf("shoalql: ORDER BY <-> 'text' needs an embedder")
		}
		vec, err := opts.Embedder.Embed(ctx, v.Text)
		if err != nil {
			return nil, fmt.Errorf("shoalql: embed query text: %w", err)
		}
		if len(vec) == 0 {
			return nil, fmt.Errorf("shoalql: embedder returned empty vector")
		}
		return vec, nil
	default:
		return nil, fmt.Errorf("shoalql: unknown vector expression")
	}
}

func resolveVectorEmbeddingSpace(
	target VectorExpr,
	opts PlanOptions,
	operation string,
) (string, error) {
	configured := opts.Vector.EmbeddingSpace
	if target.Kind != VecText {
		if err := embeddingspace.ValidateQueryStates(
			operation, configured); err != nil {
			return "", err
		}
		return configured, nil
	}
	if opts.Embedder == nil {
		return "", fmt.Errorf("shoalql: ORDER BY <-> 'text' needs an embedder")
	}
	provider, ok := opts.Embedder.(embeddingSpaceIdentityProvider)
	if !ok {
		return "", fmt.Errorf(
			"%w: text embedder does not report its embedding space",
			embeddingspace.ErrQueryIdentityRequired)
	}
	identity, err := provider.EmbeddingSpaceIdentity()
	if err != nil {
		return "", fmt.Errorf("shoalql: resolve text embedding identity: %w", err)
	}
	if err := embeddingspace.ValidateQueryStates(operation, identity); err != nil {
		return "", err
	}
	if configured != "" {
		if err := embeddingspace.ValidateQueryStates(
			operation, configured); err != nil {
			return "", err
		}
		if err := embeddingspace.EnsureSameIdentity(
			operation, identity, configured); err != nil {
			return "", err
		}
	}
	return identity, nil
}

// projection lowers the SELECT list. Star projects id + content by convention.
func projection(stmt *SelectStmt, binding TableBinding) ([]OutputColumn, error) {
	if stmt.Star {
		out := []OutputColumn{{Name: ColID, Kind: OutRowKey}}
		if ci, ok := binding.Column(ColContent); ok && ci.Role == RoleValueCell {
			out = append(out, OutputColumn{Name: ColContent, Kind: OutCell, CF: ci.CF, CQ: ci.CQ})
		}
		return out, nil
	}
	out := make([]OutputColumn, 0, len(stmt.Columns))
	for _, it := range stmt.Columns {
		name := outName(it)
		switch {
		case it.CountStar:
			out = append(out, OutputColumn{Name: name, Kind: OutCount})
		case it.isExpand():
			cf, ok := binding.EdgeCF(it.ExpandEdge)
			if !ok {
				return nil, fmt.Errorf("shoalql: unknown edge %q", it.ExpandEdge)
			}
			if _, ok := binding.Column(it.ExpandCol); !ok {
				return nil, fmt.Errorf("shoalql: unknown expand column %q", it.ExpandCol)
			}
			out = append(out, OutputColumn{Name: name, Kind: OutExpand, EdgeCF: cf, SourceCol: it.ExpandCol})
		default:
			ci, ok := binding.Column(it.Column)
			if !ok {
				return nil, fmt.Errorf("shoalql: unknown column %q", it.Column)
			}
			switch ci.Role {
			case RoleRowKey:
				out = append(out, OutputColumn{Name: name, Kind: OutRowKey})
			default:
				out = append(out, OutputColumn{Name: name, Kind: OutCell, CF: ci.CF, CQ: ci.CQ})
			}
		}
	}
	return out, nil
}

func outName(it SelectItem) string {
	if it.Alias != "" {
		return it.Alias
	}
	switch {
	case it.CountStar:
		return "count"
	case it.isExpand():
		return "expand"
	default:
		return it.Column
	}
}

func projectionNeedsHydration(proj []OutputColumn) bool {
	for _, c := range proj {
		if c.Kind == OutCell || c.Kind == OutExpand {
			return true
		}
	}
	return false
}

// cfRestriction returns the CF include-set for a plain scan, or nil when the
// scan must see every CF (Star, expand, or any unknown-CF column).
func cfRestriction(stmt *SelectStmt, proj []OutputColumn) ([][]byte, bool) {
	if stmt.Star {
		return nil, false
	}
	seen := map[string]bool{}
	var cfs [][]byte
	for _, c := range proj {
		switch c.Kind {
		case OutCell:
			if len(c.CF) == 0 {
				return nil, false
			}
			if !seen[string(c.CF)] {
				seen[string(c.CF)] = true
				cfs = append(cfs, c.CF)
			}
		case OutExpand:
			return nil, false
		}
	}
	// Residual/predicate columns may need their CFs too; be conservative and
	// only restrict when the projection alone determines the needed CFs.
	if len(cfs) == 0 {
		return nil, false
	}
	return cfs, true
}

// rowBounds computes the [lo, hi) row-key byte bounds from id predicates and
// resolves the non-id predicates to physical ResidualFilters. Bounds are in
// physical row space (prefix already applied).
func rowBounds(preds []Predicate, binding TableBinding) (lo, hi []byte, residual []ResidualFilter, err error) {
	prefix := binding.RowPrefix()
	lo = append([]byte(nil), prefix...)
	hi = successor(prefix)

	for _, pr := range preds {
		if pr.Kind == PredCompare {
			ci, ok := binding.Column(pr.Column)
			if ok && ci.Role == RoleRowKey {
				plo, phi, perr := idBound(prefix, pr)
				if perr != nil {
					return nil, nil, nil, perr
				}
				lo = maxBytes(lo, plo)
				hi = minBytes(hi, phi)
				continue
			}
		}
		rf, rerr := residualFor(pr, binding)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		residual = append(residual, rf)
	}
	return lo, hi, residual, nil
}

// residualFor resolves a non-id predicate to a physical ResidualFilter.
func residualFor(pr Predicate, binding TableBinding) (ResidualFilter, error) {
	ci, ok := binding.Column(pr.Column)
	if !ok {
		return ResidualFilter{}, fmt.Errorf("shoalql: unknown column %q", pr.Column)
	}
	if ci.Role == RoleVector {
		return ResidualFilter{}, fmt.Errorf("shoalql: cannot filter on vector column %q", pr.Column)
	}
	rf := ResidualFilter{CF: ci.CF, CQ: ci.CQ}
	if pr.Kind == PredMatch {
		rf.IsMatch = true
		rf.Terms = pr.MatchTerms
		return rf, nil
	}
	if pr.Value.Kind != LitString {
		rf.Str = fmt.Sprintf("%v", pr.Value.Num)
	} else {
		rf.Str = pr.Value.Str
	}
	rf.Op = pr.Op
	return rf, nil
}

// idBound maps one id comparison to a [lo, hi) row-key range.
func idBound(prefix []byte, pr Predicate) (lo, hi []byte, err error) {
	if pr.Value.Kind != LitString {
		return nil, nil, fmt.Errorf("shoalql: id comparison requires a string literal")
	}
	rk := append(append([]byte(nil), prefix...), []byte(pr.Value.Str)...)
	switch pr.Op {
	case OpEq:
		return rk, appendByte(rk, 0x00), nil
	case OpGE:
		return rk, nil, nil
	case OpGT:
		return appendByte(rk, 0x00), nil, nil
	case OpLE:
		return nil, appendByte(rk, 0x00), nil
	case OpLT:
		return nil, rk, nil
	case OpLike:
		val := strings.TrimSuffix(pr.Value.Str, "%")
		pk := append(append([]byte(nil), prefix...), []byte(val)...)
		return pk, successor(pk), nil
	default:
		return nil, nil, fmt.Errorf("shoalql: unsupported id operator")
	}
}

// --- byte / range helpers ---

func bytesRange(lo, hi []byte) iterrt.Range {
	r := iterrt.Range{StartInclusive: true, EndInclusive: false}
	if len(lo) == 0 {
		r.InfiniteStart = true
	} else {
		r.Start = &iterrt.Key{Row: lo}
	}
	if len(hi) == 0 {
		r.InfiniteEnd = true
	} else {
		r.End = &iterrt.Key{Row: hi}
	}
	return r
}

// successor returns the smallest byte string greater than every string having
// b as a prefix. An all-0xFF (or empty) input yields nil, meaning unbounded.
func successor(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

func appendByte(b []byte, c byte) []byte {
	return append(append([]byte(nil), b...), c)
}

// maxBytes returns the greater lower bound. An empty (infinite) bound loses.
func maxBytes(a, b []byte) []byte {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	if string(b) > string(a) {
		return b
	}
	return a
}

// minBytes returns the lesser upper bound. An empty (infinite) bound loses.
func minBytes(a, b []byte) []byte {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	if string(b) < string(a) {
		return b
	}
	return a
}

// packVecBE packs a float32 vector big-endian and base64-encodes it, matching
// the VectorKNN iterator's query.b64 contract.
func packVecBE(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.BigEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
