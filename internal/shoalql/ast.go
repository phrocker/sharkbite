package shoalql

// AST for the read-only SELECT dialect. The parser produces a *SelectStmt;
// the planner (planner.go) lowers it against a TableBinding into a physical
// plan. Nothing here is graph-specific.

// SelectStmt is a parsed query.
type SelectStmt struct {
	Explain       bool
	ExplainFormat ExplainFormat

	Columns []SelectItem // projection; empty Columns with Star=true means *
	Star    bool
	Table   string
	AsOf    *int64 // time-travel upper bound (ms); nil = latest

	Where   []Predicate // AND-joined
	GroupBy string      // empty = no grouping
	Order   *OrderBy    // nil = no ordering
	Limit   *int        // nil = no limit
}

// ExplainFormat controls the stable representation returned by EXPLAIN.
type ExplainFormat string

const (
	ExplainText ExplainFormat = "text"
	ExplainJSON ExplainFormat = "json"
)

// SelectItem is one projected output column.
type SelectItem struct {
	// Exactly one of the following forms is set.
	Column string // plain column reference

	CountStar bool // count(*)

	ExpandCol  string // expand(<ExpandCol>, '<ExpandEdge>')
	ExpandEdge string

	Alias string // optional AS <alias>
}

// isExpand reports whether the item is an expand(...) graph hop.
func (s SelectItem) isExpand() bool { return s.ExpandCol != "" }

// PredKind distinguishes predicate forms.
type PredKind int

const (
	// PredCompare is "<col> <op> <literal>".
	PredCompare PredKind = iota
	// PredMatch is "MATCH(<col>, '<terms>')" — keyword search.
	PredMatch
)

// CompareOp is a scalar comparison operator.
type CompareOp int

const (
	OpEq CompareOp = iota
	OpGE
	OpGT
	OpLE
	OpLT
	OpLike // prefix match: col LIKE 'foo%'
)

// Predicate is one WHERE conjunct.
type Predicate struct {
	Kind PredKind

	Column string
	Op     CompareOp
	Value  Literal // for PredCompare

	MatchTerms string // for PredMatch
}

// LitKind tags a literal's type.
type LitKind int

const (
	LitString LitKind = iota
	LitNumber
)

// Literal is a constant value in the query.
type Literal struct {
	Kind LitKind
	Str  string
	Num  float64
}

// OrderBy encodes a single vector-distance ordering:
//
//	ORDER BY <Column> <-> <Target>
type OrderBy struct {
	Column string
	Target VectorExpr
}

// VecKind tags the source of the query vector.
type VecKind int

const (
	// VecLiteral is an inline "[f, f, ...]" vector.
	VecLiteral VecKind = iota
	// VecParam is a ":name" bind parameter carrying a []float32.
	VecParam
	// VecText is a 'string' embedded at plan time via an Embedder.
	VecText
)

// VectorExpr is the right-hand side of a "<->" ordering.
type VectorExpr struct {
	Kind    VecKind
	Literal []float32
	Param   string
	Text    string
}
