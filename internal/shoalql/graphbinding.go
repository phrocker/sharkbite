package shoalql

import "github.com/phrocker/shoal-oss/internal/graphschema"

// GraphBinding maps a logical table onto shoal's graph key layout
// (graphschema): rows are prefixed (evt:, ent:), text lives in the content:
// CF, embeddings in the vec: CF, arbitrary attributes in the attr: CF keyed
// by qualifier, and edges in the edge.* CFs.
//
// This is the first TableBinding. A future DocumentBinding for generic
// unstructured data (DataWave-style sharded documents + global/field index)
// would implement the same interface, leaving the grammar and planner
// untouched.
type GraphBinding struct {
	name     string
	physical string
	prefix   []byte
	columns  map[string]ColumnInfo
	edges    map[string][]byte
}

// Standard logical column names exposed by the graph binding.
const (
	ColID        = "id"
	ColContent   = "content"
	ColEmbedding = "embedding"
)

// NewGraphBinding builds a binding for a logical table over rows carrying the
// given prefix (e.g. "evt:" for events). Columns other than the built-ins
// resolve to attribute cells in the attr: CF keyed by the column name.
func NewGraphBinding(table, physicalTable, rowPrefix string) *GraphBinding {
	g := &GraphBinding{
		name:     table,
		physical: physicalTable,
		prefix:   []byte(rowPrefix),
		columns: map[string]ColumnInfo{
			ColID:        {Role: RoleRowKey},
			ColContent:   {Role: RoleValueCell, CF: graphschema.ContentCF()},
			ColEmbedding: {Role: RoleVector, CF: graphschema.VectorCF()},
			// "vec" is an accepted alias for the embedding column.
			"vec": {Role: RoleVector, CF: graphschema.VectorCF()},
		},
		edges: map[string][]byte{
			"temporal": graphschema.TemporalEdgeCF(),
			"causal":   graphschema.CausalEdgeCF(),
			"semantic": graphschema.SemanticEdgeCF(),
			"entity":   graphschema.EntityEdgeCF(),
		},
	}
	return g
}

// TableName implements TableBinding.
func (g *GraphBinding) TableName() string { return g.name }

// PhysicalTable implements TableBinding.
func (g *GraphBinding) PhysicalTable() string { return g.physical }

// RowPrefix implements TableBinding.
func (g *GraphBinding) RowPrefix() []byte { return g.prefix }

// Column implements TableBinding. Unknown columns fall back to an attribute
// cell in the attr: CF, qualified by the column name — so ad-hoc attributes
// are queryable without pre-registration.
func (g *GraphBinding) Column(name string) (ColumnInfo, bool) {
	if ci, ok := g.columns[name]; ok {
		return ci, true
	}
	return ColumnInfo{
		Role: RoleValueCell,
		CF:   graphschema.AttributeCF(),
		CQ:   []byte(name),
	}, true
}

// EdgeCF implements TableBinding.
func (g *GraphBinding) EdgeCF(name string) ([]byte, bool) {
	cf, ok := g.edges[name]
	return cf, ok
}

// NewGraphCatalog returns a Catalog pre-populated with the conventional graph
// tables: "events" (evt:) and "entities" (ent:), both backed by the given
// physical engine table (e.g. "graph").
func NewGraphCatalog(physicalTable string) *MapCatalog {
	return NewMapCatalog(
		NewGraphBinding("events", physicalTable, graphschema.EventRowPrefix),
		NewGraphBinding("entities", physicalTable, graphschema.EntityRowPrefix),
	)
}
