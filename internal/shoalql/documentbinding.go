package shoalql

// DocumentBinding maps a logical table onto shoal's DataWave-style document
// layout (documentschema): sharded documents whose fields live in event cells
// (cf=datatype\x00uid, cq=FIELD\x00value), an in-shard field index (fi) for
// candidate resolution, and a global forward index (value -> shards) for
// narrowing which shards to search.
//
// It is the second TableBinding, alongside GraphBinding. Because documents are
// row=shard (not row=id) with dynamic fields, the planner detects the
// documentModel marker interface below and lowers document queries down a
// dedicated path (global index -> DocumentIndexIterator -> reconstruction)
// rather than the graph row-key path. The grammar and the rest of the pipeline
// are unchanged.
type DocumentBinding struct {
	name       string
	docTable   string
	indexTable string
	fields     map[string]string // logical column -> physical FIELD override
}

// Special document column names understood by the document plan path.
const (
	// ColType projects/filters a document's datatype.
	ColType = "type"
)

// documentModel is the optional interface a TableBinding implements to signal
// the sharded-document layout. The planner type-asserts for it and, when
// present, lowers to the document plan path instead of the graph path.
type documentModel interface {
	// DocumentTable is the physical engine table holding shard/event/fi
	// entries.
	DocumentTable() string
	// GlobalIndexTable is the physical engine table holding the forward index
	// (row=value, cf=FIELD, cq=shard\x00datatype).
	GlobalIndexTable() string
	// FieldColumn maps a logical column to its physical FIELD name and a
	// "special" tag: "id" (the document uid), "type" (the datatype), or ""
	// for an ordinary indexed field.
	FieldColumn(col string) (field, special string)
	// DocumentVector returns the logical semantic column and persisted index.
	DocumentVector() (column, index string)
}

// NewDocumentBinding builds a binding for a logical document table. docTable
// holds the shard/event/fi entries; indexTable holds the global forward index.
// fieldOverrides optionally remaps logical column names to physical FIELD
// names; unmapped columns use the column name verbatim as the FIELD.
func NewDocumentBinding(table, docTable, indexTable string, fieldOverrides map[string]string) *DocumentBinding {
	f := make(map[string]string, len(fieldOverrides))
	for k, v := range fieldOverrides {
		f[k] = v
	}
	return &DocumentBinding{
		name:       table,
		docTable:   docTable,
		indexTable: indexTable,
		fields:     f,
	}
}

// TableName implements TableBinding.
func (d *DocumentBinding) TableName() string { return d.name }

// PhysicalTable implements TableBinding. It reports the document table; the
// document plan path also consults GlobalIndexTable for the forward index.
func (d *DocumentBinding) PhysicalTable() string { return d.docTable }

// RowPrefix implements TableBinding. Documents are keyed by shard, not by a
// prefixed id, so there is no row prefix.
func (d *DocumentBinding) RowPrefix() []byte { return nil }

// Column implements TableBinding. The document plan path resolves columns via
// FieldColumn instead, so this reports "unknown" for every name — it exists
// only to satisfy the interface and must not be reached for a document query.
func (d *DocumentBinding) Column(string) (ColumnInfo, bool) { return ColumnInfo{}, false }

// EdgeCF implements TableBinding. Documents have no edge model.
func (d *DocumentBinding) EdgeCF(string) ([]byte, bool) { return nil, false }

// DocumentTable implements documentModel.
func (d *DocumentBinding) DocumentTable() string { return d.docTable }

// GlobalIndexTable implements documentModel.
func (d *DocumentBinding) GlobalIndexTable() string { return d.indexTable }

// FieldColumn implements documentModel. "id" -> uid, "type" -> datatype; any
// other column maps to a physical FIELD (via the override map, else verbatim).
func (d *DocumentBinding) FieldColumn(col string) (field, special string) {
	switch col {
	case ColID:
		return "", "id"
	case ColType:
		return "", "type"
	}
	if f, ok := d.fields[col]; ok {
		return f, ""
	}
	return col, ""
}

func (d *DocumentBinding) DocumentVector() (string, string) {
	return ColEmbedding, d.docTable + "_ivf"
}

// NewDocumentCatalog returns a Catalog with a single document table backed by
// the given document and global-index engine tables.
func NewDocumentCatalog(table, docTable, indexTable string) *MapCatalog {
	return NewMapCatalog(NewDocumentBinding(table, docTable, indexTable, nil))
}
