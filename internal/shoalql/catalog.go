package shoalql

// The catalog is the "aperture" that keeps the SQL layer data-model-agnostic.
// The parser and planner know nothing about the graph key layout, embedding
// column families, or edge encodings — all of that lives behind a
// TableBinding. GraphBinding (graphbinding.go) is the first implementation;
// a DataWave-style DocumentBinding for generic unstructured data can satisfy
// the same interface later, and every query above keeps working unchanged.

// ColumnRole classifies how a logical column maps to physical storage.
type ColumnRole int

const (
	// RoleRowKey means the column is the (prefix-stripped) row key.
	RoleRowKey ColumnRole = iota
	// RoleValueCell means the column is a value cell in a column family.
	RoleValueCell
	// RoleVector means the column holds an embedding cell usable as the
	// target of an ORDER BY <-> vector search.
	RoleVector
)

// ColumnInfo is a logical column's physical mapping.
type ColumnInfo struct {
	Role ColumnRole
	CF   []byte // for RoleValueCell / RoleVector
	CQ   []byte // optional column qualifier; nil = any/whole-CF
}

// TableBinding maps one logical table onto a physical Accumulo key layout and
// the iterator machinery that serves predicates over it. Implementations must
// be safe for concurrent use (they are read-only descriptors).
type TableBinding interface {
	// TableName is the logical table this binding serves.
	TableName() string

	// PhysicalTable is the underlying Accumulo/engine table the logical
	// table lives in. Several logical tables may share one physical table,
	// distinguished only by RowPrefix (e.g. events and entities both live
	// in the graph table).
	PhysicalTable() string

	// RowPrefix is prepended to row-key column values to form physical row
	// keys (e.g. "evt:"). May be empty for bindings with no prefixing.
	RowPrefix() []byte

	// Column resolves a logical column name to its physical mapping. The
	// bool is false for unknown columns.
	Column(name string) (ColumnInfo, bool)

	// EdgeCF maps an expand() edge name (e.g. "semantic") to the column
	// family holding those edges. The bool is false for unknown edges.
	EdgeCF(name string) ([]byte, bool)
}

// Catalog resolves logical table names to their bindings.
type Catalog interface {
	Binding(table string) (TableBinding, bool)
}

// MapCatalog is a simple in-memory Catalog.
type MapCatalog struct {
	bindings map[string]TableBinding
}

// NewMapCatalog builds a Catalog from the given bindings, keyed by
// TableName().
func NewMapCatalog(bindings ...TableBinding) *MapCatalog {
	m := &MapCatalog{bindings: make(map[string]TableBinding, len(bindings))}
	for _, b := range bindings {
		m.bindings[b.TableName()] = b
	}
	return m
}

// Binding implements Catalog.
func (m *MapCatalog) Binding(table string) (TableBinding, bool) {
	b, ok := m.bindings[table]
	return b, ok
}
