package shoalql

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/documentschema"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// exec_document.go runs a ShapeDocument plan: the DataWave-style document path.
// It resolves candidate shards from the global forward index, runs the
// DocumentIndexIterator to resolve and reconstruct matching documents, then
// groups the emitted event cells back into documents for projection.

// docRow is a reconstructed document.
type docRow struct {
	shard    string
	datatype string
	uid      string
	fields   map[string]string
}

func (e *Executor) runDocument(ctx context.Context, p *Plan) (*Result, error) {
	if p.VectorMode == VectorApproximate {
		return e.runSemanticDocument(ctx, p)
	}
	// Phase 1: resolve candidate shards from the global index.
	shards, err := e.resolveDocShards(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(shards) == 0 {
		return e.emptyDocResult(p), nil
	}

	// Phase 2: run the DocumentIndexIterator over the document table. The
	// AsOf iterator (if any) is already in p.Stack, beneath the document
	// iterator, so reconstruction sees time-filtered cells.
	stack := append(append([]iterrt.IterSpec(nil), p.Stack...), docIndexSpec(shards, p.DocTerms, p.DocBoolOr))
	stream, err := e.be.Scan(ctx, p.Table, iterrt.InfiniteRange(), ScanRequest{Stack: stack})
	if err != nil {
		return nil, err
	}
	docs, err := groupDocuments(stream)
	stream.Close()
	if err != nil {
		return nil, err
	}

	// Phase 3: filter by id/type residuals and project.
	if p.DocStar {
		return e.projectDocumentStar(p, docs), nil
	}
	return e.projectDocuments(p, docs), nil
}

func (e *Executor) runSemanticDocument(ctx context.Context, p *Plan) (*Result, error) {
	backend, ok := e.be.(ApproximateVectorBackend)
	if !ok {
		return nil, fmt.Errorf("shoalql: approximate vector backend unavailable")
	}
	var docs []*docRow
	allowedDocuments := map[string]bool{}
	if len(p.DocTerms) > 0 {
		shards, err := e.resolveDocShards(ctx, p)
		if err != nil {
			return nil, err
		}
		if len(shards) == 0 {
			return e.emptyDocResult(p), nil
		}
		stack := append(append([]iterrt.IterSpec(nil), p.Stack...), docIndexSpec(shards, p.DocTerms, p.DocBoolOr))
		stream, err := e.be.Scan(ctx, p.Table, iterrt.InfiniteRange(), ScanRequest{Stack: stack})
		if err != nil {
			return nil, err
		}
		docs, err = groupDocuments(stream)
		stream.Close()
		if err != nil {
			return nil, err
		}
		for _, doc := range docs {
			if docPassesResidual(doc, p.DocResidual) {
				allowedDocuments[doc.shard+"\x00"+doc.datatype+"\x00"+doc.uid] = true
			}
		}
		if len(allowedDocuments) == 0 {
			return e.emptyDocResult(p), nil
		}
	}
	hits, _, err := backend.SearchVector(ctx, VectorSearchRequest{
		Index: p.VectorIndex, Query: append([]float32(nil), p.VectorQuery...),
		EmbeddingSpace: p.VectorEmbeddingSpace,
		TopK:           p.VectorTopK, NProbe: p.VectorNProbe, AsOf: p.AsOf,
		Freshness: p.VectorFreshness, ExactFallback: p.VectorExactFallback,
		AllowedDocuments: allowedDocuments,
	})
	if err != nil {
		return nil, err
	}
	var rows [][]byte
	seenShard := map[string]bool{}
	for _, hit := range hits {
		shard := hit.Document.Shard
		if shard == "" || seenShard[shard] {
			continue
		}
		seenShard[shard] = true
		rows = append(rows, []byte(shard))
	}
	if len(rows) == 0 {
		return e.emptyDocResult(p), nil
	}

	if len(p.DocTerms) == 0 {
		cells, err := e.be.LookupRows(ctx, p.Table, rows, ScanRequest{Stack: p.Stack})
		if err != nil {
			return nil, err
		}
		docs = groupDocumentCells(cells)
	}
	byIdentity := map[string]*docRow{}
	for _, doc := range docs {
		byIdentity[doc.shard+"\x00"+doc.datatype+"\x00"+doc.uid] = doc
	}
	ranked := make([]*docRow, 0, len(hits))
	for _, hit := range hits {
		key := hit.Document.Shard + "\x00" + hit.Document.Datatype + "\x00" + hit.Document.UID
		if doc := byIdentity[key]; doc != nil && docPassesResidual(doc, p.DocResidual) {
			ranked = append(ranked, doc)
		}
	}
	if p.DocStar {
		return e.projectDocumentStar(p, ranked), nil
	}
	return e.projectDocuments(p, ranked), nil
}

// resolveDocShards intersects (AND) or unions (OR) the per-term candidate shard
// sets drawn from the global forward index. An empty result means no shard can
// satisfy the query.
func (e *Executor) resolveDocShards(ctx context.Context, p *Plan) ([][]byte, error) {
	var acc map[string]struct{}
	for i, term := range p.DocTerms {
		set, err := e.shardsForTerm(ctx, p.IndexTable, term)
		if err != nil {
			return nil, err
		}
		if p.DocBoolOr {
			if acc == nil {
				acc = map[string]struct{}{}
			}
			for k := range set {
				acc[k] = struct{}{}
			}
			continue
		}
		if i == 0 {
			acc = set
		} else {
			for k := range acc {
				if _, ok := set[k]; !ok {
					delete(acc, k)
				}
			}
		}
		if len(acc) == 0 {
			return nil, nil
		}
	}
	shards := make([][]byte, 0, len(acc))
	for k := range acc {
		shards = append(shards, []byte(k))
	}
	sort.Slice(shards, func(i, j int) bool { return string(shards[i]) < string(shards[j]) })
	return shards, nil
}

// shardsForTerm scans the global forward index for a single term (row=value,
// cf=FIELD) and returns the distinct shards the value appears in.
func (e *Executor) shardsForTerm(ctx context.Context, indexTable string, term DocTerm) (map[string]struct{}, error) {
	cf := documentschema.IndexCF(term.Field)
	stream, err := e.be.Scan(ctx, indexTable, docRowExactRange(term.Value),
		ScanRequest{ColumnFamilies: [][]byte{cf}, CFInclusive: true})
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	set := map[string]struct{}{}
	for stream.Next() {
		k := stream.Key()
		if string(k.Row) == term.Value && bytesEq(k.ColumnFamily, cf) {
			if shard, _, ok := documentschema.ParseIndexCQ(k.ColumnQualifier); ok {
				set[shard] = struct{}{}
			}
		}
		if err := stream.Advance(); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// groupDocuments folds an event-cell stream (row=shard, cf=datatype\x00uid,
// cq=FIELD\x00value) into reconstructed documents. Cells arrive in wire.Key
// order, so a document's cells are contiguous.
func groupDocuments(stream RowStream) ([]*docRow, error) {
	var out []*docRow
	var cur *docRow
	var curKey string
	for stream.Next() {
		k := stream.Key()
		datatype, uid, ok := documentschema.ParseEventCF(k.ColumnFamily)
		if !ok {
			if err := stream.Advance(); err != nil {
				return nil, err
			}
			continue
		}
		key := string(k.Row) + "\x01" + string(k.ColumnFamily)
		if cur == nil || key != curKey {
			cur = &docRow{shard: string(k.Row), datatype: datatype, uid: uid, fields: map[string]string{}}
			curKey = key
			out = append(out, cur)
		}

		if field, value, ok := documentschema.ParseEventCQ(k.ColumnQualifier); ok {
			addDocumentField(cur.fields, field, value, k.ColumnQualifier, stream.Value())
		}
		if err := stream.Advance(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func groupDocumentCells(cells []Cell) []*docRow {
	sort.SliceStable(cells, func(i, j int) bool {
		return cells[i].Key.Compare(cells[j].Key) < 0
	})
	var out []*docRow
	byKey := map[string]*docRow{}
	for _, cell := range cells {
		datatype, uid, ok := documentschema.ParseEventCF(cell.Key.ColumnFamily)
		if !ok {
			continue
		}
		key := string(cell.Key.Row) + "\x00" + datatype + "\x00" + uid
		doc := byKey[key]
		if doc == nil {
			doc = &docRow{shard: string(cell.Key.Row), datatype: datatype, uid: uid, fields: map[string]string{}}
			byKey[key] = doc
			out = append(out, doc)
		}
		if field, value, ok := documentschema.ParseEventCQ(cell.Key.ColumnQualifier); ok {
			addDocumentField(doc.fields, field, value, cell.Key.ColumnQualifier, cell.Value)
		}
	}
	return out
}

func addDocumentField(
	fields map[string]string,
	field string,
	value string,
	cq []byte,
	cellValue []byte,
) {
	if documentschema.IsStructureCell(cq, cellValue) {
		return
	}
	if _, exists := fields[field]; !exists {
		fields[field] = value
	}
}

// projectDocuments builds the result for an explicit projection.
func (e *Executor) projectDocuments(p *Plan, docs []*docRow) *Result {
	res := &Result{Columns: columnNames(p.Projection)}
	for _, d := range docs {
		if !docPassesResidual(d, p.DocResidual) {
			continue
		}
		row := make(Row, len(p.Projection))
		for i, c := range p.Projection {
			switch c.Kind {
			case OutDocID:
				row[i] = strVal(d.uid)
			case OutDocType:
				row[i] = strVal(d.datatype)
			case OutDocField:
				if v, ok := d.fields[c.Field]; ok {
					row[i] = strVal(v)
				} else {
					row[i] = nullVal()
				}
			default:
				row[i] = nullVal()
			}
		}
		res.Rows = append(res.Rows, row)
		if p.Limit != nil && len(res.Rows) >= *p.Limit {
			break
		}
	}
	return res
}

// projectDocumentStar builds the result for `SELECT *`: id, type, and the
// sorted union of every field name across the returned documents.
func (e *Executor) projectDocumentStar(p *Plan, docs []*docRow) *Result {
	var kept []*docRow
	fieldSet := map[string]bool{}
	for _, d := range docs {
		if !docPassesResidual(d, p.DocResidual) {
			continue
		}
		kept = append(kept, d)
		for f := range d.fields {
			fieldSet[f] = true
		}
		if p.Limit != nil && len(kept) >= *p.Limit {
			break
		}
	}
	fields := make([]string, 0, len(fieldSet))
	for f := range fieldSet {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	cols := append([]string{ColID, ColType}, fields...)
	res := &Result{Columns: cols}
	for _, d := range kept {
		row := make(Row, len(cols))
		row[0] = strVal(d.uid)
		row[1] = strVal(d.datatype)
		for j, f := range fields {
			if v, ok := d.fields[f]; ok {
				row[2+j] = strVal(v)
			} else {
				row[2+j] = nullVal()
			}
		}
		res.Rows = append(res.Rows, row)
	}
	return res
}

// emptyDocResult builds a zero-row result with the right columns when no shard
// matches (so callers still see a well-formed, empty result set).
func (e *Executor) emptyDocResult(p *Plan) *Result {
	if p.DocStar {
		return &Result{Columns: []string{ColID, ColType}}
	}
	return &Result{Columns: columnNames(p.Projection)}
}

func docPassesResidual(d *docRow, res []DocResidual) bool {
	for _, r := range res {
		switch r.Special {
		case "id":
			if d.uid != r.Value {
				return false
			}
		case "type":
			if d.datatype != r.Value {
				return false
			}
		}
	}
	return true
}

// docIndexSpec builds the DocumentIndexIterator spec from resolved shards and
// terms.
func docIndexSpec(shards [][]byte, terms []DocTerm, orMode bool) iterrt.IterSpec {
	opts := map[string]string{
		iterrt.DocumentIndexShardCount: strconv.Itoa(len(shards)),
		iterrt.DocumentIndexTermCount:  strconv.Itoa(len(terms)),
	}
	if orMode {
		opts[iterrt.DocumentIndexBoolOp] = "or"
	}
	for i, s := range shards {
		opts[iterrt.DocumentIndexShardPrefix+strconv.Itoa(i)] = string(s)
	}
	for i, t := range terms {
		opts[iterrt.DocumentIndexTermPrefix+strconv.Itoa(i)+iterrt.DocumentIndexTermFieldSuffix] = t.Field
		opts[iterrt.DocumentIndexTermPrefix+strconv.Itoa(i)+iterrt.DocumentIndexTermValueSuffix] = t.Value
	}
	return iterrt.IterSpec{Name: iterrt.IterDocumentIndex, Options: opts}
}

// docRowExactRange returns a range covering exactly the cells whose row equals
// value (row=value, any cf/cq).
func docRowExactRange(value string) iterrt.Range {
	lo := []byte(value)
	hi := append(append([]byte(nil), lo...), 0x00)
	return iterrt.Range{
		Start:          &iterrt.Key{Row: lo},
		StartInclusive: true,
		End:            &iterrt.Key{Row: hi},
		EndInclusive:   false,
	}
}
