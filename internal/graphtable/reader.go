// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package graphtable

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/internal/tableschema"
)

// Bounds every read honours whatever the request asks.
const (
	MaxDepth    = 8
	MaxFanout   = 256
	MaxNodes    = 2048
	MaxSeeds    = 64
	MaxTerms    = 16
	MaxRowBytes = 4096
	// MaxRowCells bounds the cells read of one row. A row past it is read
	// up to the bound and the expansion reports truncation, so a hub row or
	// one carrying large embedding cells cannot make a reader materialize
	// it whole.
	MaxRowCells    = 8192
	MaxBrowseCells = 4096
	MaxBrowseRows  = 40

	DefaultDepth       = 1
	DefaultFanout      = 32
	DefaultMaxNodes    = 256
	DefaultParallelism = 16
)

// ErrInvalid marks a request the reader refuses before reading anything.
var ErrInvalid = errors.New("graphtable: invalid request")

// Node is one row of a described namespace.
type Node struct {
	// ID is the full row key.
	ID string `json:"id"`
	// Key is the row key after the namespace prefix: what names the node
	// within its namespace.
	Key string `json:"key"`
	// Identifier is the part of Key other than its kind component.
	Identifier string `json:"identifier"`
	// Namespace is the NodeSchema the row belongs to.
	Namespace string `json:"namespace"`
	// Kind is cut from the row by the namespace's kind rule, or the
	// namespace name when the layout records no kind in the row.
	Kind string `json:"kind"`
	// Found is false for a row with no visible cell: a dangling edge target,
	// a hidden-only row, or a seed that does not exist.
	Found      bool        `json:"found"`
	Attributes []Attribute `json:"attributes,omitempty"`
}

// Attribute is one attribute cell, decoded by the namespace's encoding.
type Attribute struct {
	Family string `json:"family"`
	Field  string `json:"field,omitempty"`
	Value  string `json:"value"`
}

// Edge is one edge cell read from its source row.
type Edge struct {
	From         string  `json:"from"`
	To           string  `json:"to"`
	Family       string  `json:"family"`
	Relationship string  `json:"relationship,omitempty"`
	Weight       float32 `json:"weight"`
	Weighted     bool    `json:"weighted"`
}

// ID is the edge's stable identity within its table.
func (e Edge) ID() string {
	return strings.Join([]string{e.From, e.Family, e.Relationship, e.To}, "\x1f")
}

// NeighborhoodRequest expands outward from the seeds.
type NeighborhoodRequest struct {
	Seeds []string
	// Depth is the number of hops; Fanout bounds the edges read per node
	// and MaxNodes the nodes reached. Zero takes the default.
	Depth, Fanout, MaxNodes int
	// Families and Relationships, when set, restrict the edges followed.
	Families, Relationships []string
}

// Neighborhood is an expansion's result. Nodes are in discovery order with
// the seeds first; Edges in the order they were followed. Truncated reports
// a fanout, node budget, or row cell bound that cut the walk short;
// DroppedEdges counts withheld edge cells and visible edges to hidden-only
// targets, as well as targets the reader cannot name: a row key that is not
// UTF-8, longer than MaxRowBytes, or in no node namespace. The engine's
// EdgeExpand follows any byte string; this reader serves JSON node ids and
// says what it left out.
type Neighborhood struct {
	Nodes        []Node `json:"nodes"`
	Edges        []Edge `json:"edges"`
	Truncated    bool   `json:"truncated"`
	DroppedEdges int    `json:"dropped_edges"`
}

// TermHit is the nodes a posting row names.
type TermHit struct {
	Term    string   `json:"term"`
	NodeIDs []string `json:"node_ids"`
}

// Ontology is what the descriptor says about the table, for a UI to show.
type Ontology struct {
	Table          string             `json:"table"`
	SchemaID       string             `json:"schema_id"`
	Version        int32              `json:"version"`
	CellVisibility bool               `json:"cell_visibility"`
	Description    string             `json:"description,omitempty"`
	Namespaces     []OntologyNodeKind `json:"namespaces"`
	Edges          []OntologyEdge     `json:"edges"`
	Postings       []OntologyPosting  `json:"postings,omitempty"`
	Vectors        []OntologyVector   `json:"vectors,omitempty"`
}

// OntologyNodeKind describes one node namespace.
type OntologyNodeKind struct {
	Name      string   `json:"name"`
	RowPrefix string   `json:"row_prefix"`
	Kinds     []string `json:"kinds,omitempty"`
	KindSep   string   `json:"kind_sep,omitempty"`
	KindIndex int32    `json:"kind_index,omitempty"`
}

// BrowseRequest reads a bounded page of records in one namespace. Kind and
// IdentifierPrefix narrow the row prefix; After is the previous page cursor.
type BrowseRequest struct {
	Namespace, Kind, IdentifierPrefix, After string
	Limit                                    int
}

type BrowsePage struct {
	Nodes []Node `json:"nodes"`
	Next  string `json:"next,omitempty"`
}

// OntologyEdge describes one edge family.
type OntologyEdge struct {
	Name          string   `json:"name"`
	ColumnFamily  string   `json:"column_family"`
	Relationships []string `json:"relationships,omitempty"`
	Weighted      bool     `json:"weighted"`
	TargetPrefix  string   `json:"target_prefix,omitempty"`
}

// OntologyPosting describes one term posting namespace.
type OntologyPosting struct {
	Name          string `json:"name"`
	TermRowPrefix string `json:"term_row_prefix"`
}

// OntologyVector describes one embedding family.
type OntologyVector struct {
	Name           string `json:"name"`
	EmbeddingSpace string `json:"embedding_space"`
	Metric         string `json:"metric,omitempty"`
}

// Reader serves a described table through a Store.
type Reader struct {
	store  Store
	schema *schemapb.TableSchema
	raw    []byte

	namespaces []namespace
	edges      map[string]*edgeFamily
	claimed    map[string]bool

	// Parallelism bounds the concurrent row reads of one hop.
	Parallelism int
}

type cellVisibilityKey struct{}

// WithCellVisibility supplies the caller's CV decision for one read. A
// described table that declares cell visibility with no decision sees only
// unlabelled cells. The callback must be safe for concurrent row reads.
func WithCellVisibility(ctx context.Context, allowed func([]byte) bool) context.Context {
	if allowed == nil {
		return ctx
	}
	return context.WithValue(ctx, cellVisibilityKey{}, allowed)
}

func (r *Reader) cellVisible(ctx context.Context, cv []byte) bool {
	if !r.schema.GetCellVisibility() || len(cv) == 0 {
		return true
	}
	allowed, ok := ctx.Value(cellVisibilityKey{}).(func([]byte) bool)
	return ok && allowed(cv)
}

type namespace struct {
	spec *schemapb.NodeSchema
}

type edgeFamily struct {
	spec    *schemapb.EdgeSchema
	idIndex int
}

// Open reads the newest descriptor of table from the store's reserved
// schema table and returns a reader over it. When the store is sharded,
// every shard must carry the same newest descriptor.
func Open(ctx context.Context, store Store, table string) (*Reader, error) {
	if store == nil {
		return nil, errors.New("graphtable: store is required")
	}
	type sharded interface{ Shards() []Shard }
	stores := []Store{store}
	names := []string{""}
	if s, ok := store.(sharded); ok {
		stores, names = nil, nil
		for _, shard := range s.Shards() {
			stores = append(stores, shard.Store)
			names = append(names, shard.Name)
		}
	}
	var raw []byte
	var version int64
	for i, s := range stores {
		doc, v, err := readDescriptor(ctx, s, table)
		if err != nil {
			if names[i] != "" {
				return nil, fmt.Errorf("shard %s: %w", names[i], err)
			}
			return nil, err
		}
		if i == 0 {
			raw, version = doc, v
			continue
		}
		if v != version || !bytes.Equal(doc, raw) {
			return nil, fmt.Errorf("graphtable: shard %s records descriptor version %d of %q, shard %s version %d; every shard must carry the same descriptor", names[0], version, table, names[i], v)
		}
	}
	schema, err := tableschema.Decode(raw)
	if err != nil {
		return nil, err
	}
	if schema.GetTable() != table {
		return nil, fmt.Errorf("graphtable: descriptor recorded under %q describes table %q", table, schema.GetTable())
	}
	reader, err := New(schema, store)
	if err != nil {
		return nil, err
	}
	reader.raw = raw
	return reader, nil
}

func readDescriptor(ctx context.Context, store Store, table string) ([]byte, int64, error) {
	cells, err := store.Scan(ctx, &embedpb.ScanRequest{
		Table: tableschema.Table, StartRow: []byte(table), StartInclusive: true,
		EndRow: []byte(table), EndInclusive: true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("graphtable: read descriptor of %q: %w", table, err)
	}
	var newest []byte
	var version int64
	for _, cell := range cells {
		if string(cell.GetColumnFamily()) != tableschema.SchemaCF {
			continue
		}
		v, err := tableschema.ParseQualifier(cell.GetColumnQualifier())
		if err != nil {
			return nil, 0, err
		}
		if v > version {
			version, newest = v, cell.GetValue()
		}
	}
	if newest == nil {
		return nil, 0, fmt.Errorf("%w: %q", tableschema.ErrNotDescribed, table)
	}
	return newest, version, nil
}

// New returns a reader over a validated descriptor.
func New(schema *schemapb.TableSchema, store Store) (*Reader, error) {
	if store == nil {
		return nil, errors.New("graphtable: store is required")
	}
	if err := tableschema.Validate(schema); err != nil {
		return nil, err
	}
	r := &Reader{store: store, schema: schema, edges: map[string]*edgeFamily{}, claimed: map[string]bool{}, Parallelism: DefaultParallelism}
	for _, node := range schema.GetNodes() {
		r.namespaces = append(r.namespaces, namespace{spec: node})
	}
	// Longest prefix wins when one namespace prefix extends another.
	sort.SliceStable(r.namespaces, func(i, j int) bool {
		return len(r.namespaces[i].spec.GetRowPrefix()) > len(r.namespaces[j].spec.GetRowPrefix())
	})
	for _, edge := range schema.GetEdges() {
		family := &edgeFamily{spec: edge, idIndex: -1}
		if edge.GetIdIndexSet() {
			family.idIndex = int(edge.GetIdIndex())
		}
		r.edges[edge.GetEdgeCf()] = family
		r.claimed[edge.GetEdgeCf()] = true
	}
	for _, vector := range schema.GetVectors() {
		r.claimed[vector.GetEmbeddingCf()] = true
	}
	for _, posting := range schema.GetPostings() {
		if posting.GetPostingCf() != "" {
			r.claimed[posting.GetPostingCf()] = true
		}
	}
	return r, nil
}

// Schema returns the descriptor the reader serves.
func (r *Reader) Schema() *schemapb.TableSchema { return r.schema }

// Table returns the described table's name.
func (r *Reader) Table() string { return r.schema.GetTable() }

// Ontology projects the descriptor.
func (r *Reader) Ontology() Ontology {
	out := Ontology{
		Table: r.schema.GetTable(), SchemaID: r.schema.GetSchemaId(),
		Version: r.schema.GetVersion(), CellVisibility: r.schema.GetCellVisibility(), Description: r.schema.GetDescription(),
		Namespaces: []OntologyNodeKind{}, Edges: []OntologyEdge{},
	}
	for _, node := range r.schema.GetNodes() {
		out.Namespaces = append(out.Namespaces, OntologyNodeKind{Name: node.GetName(), RowPrefix: node.GetRowPrefix(), Kinds: append([]string(nil), node.GetKinds()...), KindSep: node.GetKindSep(), KindIndex: node.GetKindIndex()})
	}
	for _, edge := range r.schema.GetEdges() {
		out.Edges = append(out.Edges, OntologyEdge{
			Name: edge.GetName(), ColumnFamily: edge.GetEdgeCf(),
			Relationships: append([]string(nil), edge.GetRelationships()...),
			Weighted:      edge.GetValueEncoding() == schemapb.ValueEncoding_VALUE_ENCODING_FLOAT32_BE,
			TargetPrefix:  edge.GetPrimaryPrefix(),
		})
	}
	for _, posting := range r.schema.GetPostings() {
		out.Postings = append(out.Postings, OntologyPosting{Name: posting.GetName(), TermRowPrefix: posting.GetTermRowPrefix()})
	}
	for _, vector := range r.schema.GetVectors() {
		out.Vectors = append(out.Vectors, OntologyVector{Name: vector.GetName(), EmbeddingSpace: vector.GetEmbeddingSpace(), Metric: vector.GetMetric()})
	}
	return out
}

// namespaceOf finds the namespace whose prefix the row carries.
func (r *Reader) namespaceOf(row string) (namespace, bool) {
	for _, ns := range r.namespaces {
		if strings.HasPrefix(row, ns.spec.GetRowPrefix()) {
			return ns, true
		}
	}
	return namespace{}, false
}

func (ns namespace) kindOf(row string) string {
	rest := row[len(ns.spec.GetRowPrefix()):]
	if sep := ns.spec.GetKindSep(); sep != "" {
		fields := strings.Split(rest, sep)
		if idx := int(ns.spec.GetKindIndex()); idx >= 0 && idx < len(fields) && fields[idx] != "" {
			return fields[idx]
		}
		return ""
	}
	return ns.spec.GetName()
}

func (ns namespace) identifierOf(row string) string {
	rest := row[len(ns.spec.GetRowPrefix()):]
	sep := ns.spec.GetKindSep()
	if sep == "" {
		return rest
	}
	fields := strings.Split(rest, sep)
	i := int(ns.spec.GetKindIndex())
	if i < 0 || i >= len(fields) {
		return rest
	}
	return strings.Join(append(fields[:i:i], fields[i+1:]...), sep)
}

// Browse enumerates live rows without an unbounded table scan. The scan's
// cell limit also bounds a single unusually wide row; paging always advances
// beyond the last row returned, even if that row was only partly read.
func (r *Reader) Browse(ctx context.Context, req BrowseRequest) (BrowsePage, error) {
	page := BrowsePage{Nodes: []Node{}}
	if req.Limit <= 0 || req.Limit > MaxBrowseRows {
		return page, fmt.Errorf("%w: browse limit must be between 1 and %d", ErrInvalid, MaxBrowseRows)
	}
	var ns namespace
	found := false
	for _, candidate := range r.namespaces {
		if candidate.spec.GetName() == req.Namespace {
			ns, found = candidate, true
			break
		}
	}
	if !found {
		return page, fmt.Errorf("%w: unknown namespace", ErrInvalid)
	}
	if !utf8.ValidString(req.Kind) || !utf8.ValidString(req.IdentifierPrefix) || !utf8.ValidString(req.After) || len(req.Kind) > 128 || len(req.IdentifierPrefix) > MaxRowBytes || len(req.After) > MaxRowBytes {
		return page, fmt.Errorf("%w: invalid browse filter", ErrInvalid)
	}
	prefix := ns.spec.GetRowPrefix()
	if req.Kind != "" {
		sep := ns.spec.GetKindSep()
		if sep == "" || ns.spec.GetKindIndex() != 0 || strings.Contains(req.Kind, sep) || req.Kind == "." || req.Kind == ".." {
			return page, fmt.Errorf("%w: kind filter unsupported by namespace", ErrInvalid)
		}
		if kinds := ns.spec.GetKinds(); len(kinds) > 0 && !slices.Contains(kinds, req.Kind) {
			return page, fmt.Errorf("%w: unknown kind", ErrInvalid)
		}
		prefix += req.Kind + sep
	}
	if req.IdentifierPrefix != "" {
		if req.Kind == "" {
			return page, fmt.Errorf("%w: identifier prefix requires kind", ErrInvalid)
		}
		prefix += req.IdentifierPrefix
	}
	if len(prefix) > MaxRowBytes {
		return page, fmt.Errorf("%w: browse prefix too long", ErrInvalid)
	}
	scan := &embedpb.ScanRequest{Table: r.Table(), Limit: MaxBrowseCells}
	if req.After == "" {
		scan.RowPrefix = prefix
	} else {
		if !strings.HasPrefix(req.After, prefix) {
			return page, fmt.Errorf("%w: cursor outside browse range", ErrInvalid)
		}
		scan.StartRow, scan.StartInclusive = []byte(req.After), false
		scan.EndRow = prefixEnd([]byte(prefix))
	}
	cells, err := r.store.Scan(ctx, scan)
	if err != nil {
		return page, err
	}
	var current string
	var lastScanned string
	var rowCells []*embedpb.Cell
	finish := func() {
		if current == "" {
			return
		}
		node, _, _ := r.parseRow(ctx, current, rowCells, nil)
		if node.Found {
			page.Nodes = append(page.Nodes, node)
		}
	}
	for _, cell := range cells {
		row := string(cell.GetRow())
		if !strings.HasPrefix(row, prefix) || !utf8.ValidString(row) || len(row) > MaxRowBytes {
			continue
		}
		lastScanned = row
		if row != current {
			finish()
			if len(page.Nodes) > req.Limit {
				break
			}
			current, rowCells = row, nil
		}
		rowCells = append(rowCells, cell)
	}
	if len(page.Nodes) <= req.Limit {
		finish()
	}
	if len(page.Nodes) > req.Limit {
		page.Nodes = page.Nodes[:req.Limit]
		page.Next = page.Nodes[len(page.Nodes)-1].ID
	} else if len(cells) == MaxBrowseCells && lastScanned != "" {
		page.Next = lastScanned
	}
	return page, nil
}

func prefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// validateRow refuses a row the descriptor does not place in a namespace.
func (r *Reader) validateRow(what, row string) error {
	if row == "" || len(row) > MaxRowBytes || !utf8.ValidString(row) {
		return fmt.Errorf("%w: %s must be a non-empty UTF-8 row key of at most %d bytes", ErrInvalid, what, MaxRowBytes)
	}
	if _, ok := r.namespaceOf(row); !ok {
		return fmt.Errorf("%w: %s %q is in no node namespace of %q", ErrInvalid, what, row, r.Table())
	}
	return nil
}

// Nodes reads the listed rows. Every id is returned, in order; one the
// store holds nothing for has Found false.
func (r *Reader) Nodes(ctx context.Context, ids []string) ([]Node, error) {
	if len(ids) == 0 || len(ids) > MaxSeeds {
		return nil, fmt.Errorf("%w: between 1 and %d node ids", ErrInvalid, MaxSeeds)
	}
	for _, id := range ids {
		if err := r.validateRow("node id", id); err != nil {
			return nil, err
		}
	}
	rows, err := r.rows(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		node, _, _ := r.parseRow(ctx, id, rows[id], nil)
		out = append(out, node)
	}
	return out, nil
}

type edgeFilter struct {
	families      map[string]bool
	relationships map[string]bool
}

func (r *Reader) filter(families, relationships []string) (*edgeFilter, error) {
	f := &edgeFilter{}
	if len(families) > 0 {
		f.families = map[string]bool{}
		for _, name := range families {
			found := false
			for _, edge := range r.schema.GetEdges() {
				if edge.GetName() == name {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: %q is not an edge family of %q", ErrInvalid, name, r.Table())
			}
			f.families[name] = true
		}
	}
	if len(relationships) > 0 {
		f.relationships = map[string]bool{}
		for _, rel := range relationships {
			if rel == "" {
				return nil, fmt.Errorf("%w: empty relationship", ErrInvalid)
			}
			f.relationships[rel] = true
		}
	}
	return f, nil
}

func (f *edgeFilter) allows(e Edge) bool {
	if f == nil {
		return true
	}
	if f.families != nil && !f.families[e.Family] {
		return false
	}
	if f.relationships != nil && !f.relationships[e.Relationship] {
		return false
	}
	return true
}

func bounded(value, fallback, max int, what string) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 || value > max {
		return 0, fmt.Errorf("%w: %s must be between 1 and %d", ErrInvalid, what, max)
	}
	return value, nil
}

// Neighborhood expands outward from the seeds, one hop at a time. Each hop
// reads the frontier's rows through the store, follows up to Fanout edges
// per node in (family, relationship, target) order, and stops adding nodes
// at MaxNodes. The nodes reached by the last hop are read so their kinds
// and attributes are known; their own edges are not followed.
func (r *Reader) Neighborhood(ctx context.Context, req NeighborhoodRequest) (Neighborhood, error) {
	if len(req.Seeds) == 0 || len(req.Seeds) > MaxSeeds {
		return Neighborhood{}, fmt.Errorf("%w: between 1 and %d seeds", ErrInvalid, MaxSeeds)
	}
	depth, err := bounded(req.Depth, DefaultDepth, MaxDepth, "depth")
	if err != nil {
		return Neighborhood{}, err
	}
	fanout, err := bounded(req.Fanout, DefaultFanout, MaxFanout, "fanout")
	if err != nil {
		return Neighborhood{}, err
	}
	maxNodes, err := bounded(req.MaxNodes, DefaultMaxNodes, MaxNodes, "max_nodes")
	if err != nil {
		return Neighborhood{}, err
	}
	filter, err := r.filter(req.Families, req.Relationships)
	if err != nil {
		return Neighborhood{}, err
	}
	walk := newWalk(r, fanout, maxNodes, filter)
	for _, seed := range req.Seeds {
		if err := r.validateRow("seed", seed); err != nil {
			return Neighborhood{}, err
		}
		walk.discover(seed)
	}
	if len(walk.order) > maxNodes {
		return Neighborhood{}, fmt.Errorf("%w: %d seeds exceed max_nodes %d", ErrInvalid, len(walk.order), maxNodes)
	}
	for hop := 0; hop < depth && len(walk.frontier) > 0; hop++ {
		if err := walk.expand(ctx); err != nil {
			return Neighborhood{}, err
		}
	}
	if err := walk.materializeFrontier(ctx); err != nil {
		return Neighborhood{}, err
	}
	return walk.result(), nil
}

// Path finds a directed path from one row to another by the same hop
// expansion, stopping at the hop that reaches the target.
func (r *Reader) Path(ctx context.Context, from, to string, maxDepth, fanout, maxNodes int) (Neighborhood, bool, error) {
	if err := r.validateRow("from", from); err != nil {
		return Neighborhood{}, false, err
	}
	if err := r.validateRow("to", to); err != nil {
		return Neighborhood{}, false, err
	}
	depth, err := bounded(maxDepth, 4, MaxDepth, "max_depth")
	if err != nil {
		return Neighborhood{}, false, err
	}
	fan, err := bounded(fanout, DefaultFanout, MaxFanout, "fanout")
	if err != nil {
		return Neighborhood{}, false, err
	}
	budget, err := bounded(maxNodes, MaxNodes, MaxNodes, "max_nodes")
	if err != nil {
		return Neighborhood{}, false, err
	}
	walk := newWalk(r, fan, budget, nil)
	walk.discover(from)
	if from == to {
		if err := walk.materializeFrontier(ctx); err != nil {
			return Neighborhood{}, false, err
		}
		result := walk.result()
		return result, len(result.Nodes) > 0, nil
	}
	for hop := 0; hop < depth && len(walk.frontier) > 0; hop++ {
		if err := walk.expand(ctx); err != nil {
			return Neighborhood{}, false, err
		}
		if _, reached := walk.parent[to]; reached {
			break
		}
	}
	if _, reached := walk.parent[to]; !reached {
		return Neighborhood{}, false, nil
	}
	var chain []Edge
	for cursor := to; cursor != from; {
		edge := walk.parent[cursor]
		chain = append(chain, edge)
		cursor = edge.From
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	ids := []string{from}
	for _, edge := range chain {
		ids = append(ids, edge.To)
	}
	rows, err := r.rows(ctx, ids)
	if err != nil {
		return Neighborhood{}, false, err
	}
	out := Neighborhood{Edges: chain, Truncated: walk.truncated, DroppedEdges: walk.dropped}
	for _, id := range ids {
		node, _, _ := r.parseRow(ctx, id, rows[id], nil)
		if r.schema.GetCellVisibility() && !node.Found {
			return Neighborhood{}, false, nil
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out, true, nil
}

// Terms resolves posting rows to node ids under every posting namespace.
func (r *Reader) Terms(ctx context.Context, terms []string) ([]TermHit, error) {
	if len(terms) == 0 || len(terms) > MaxTerms {
		return nil, fmt.Errorf("%w: between 1 and %d terms", ErrInvalid, MaxTerms)
	}
	if len(r.schema.GetPostings()) == 0 {
		return nil, fmt.Errorf("%w: %q describes no term postings", ErrInvalid, r.Table())
	}
	var rows []string
	owners := map[string]*schemapb.PostingSchema{}
	for _, term := range terms {
		if term == "" || len(term) > MaxRowBytes || !utf8.ValidString(term) {
			return nil, fmt.Errorf("%w: term must be a non-empty UTF-8 string of at most %d bytes", ErrInvalid, MaxRowBytes)
		}
		for _, posting := range r.schema.GetPostings() {
			row := posting.GetTermRowPrefix() + term
			rows = append(rows, row)
			owners[row] = posting
		}
	}
	cells, err := r.rows(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]TermHit, 0, len(terms))
	checkedNodes := 0
	for _, term := range terms {
		hit := TermHit{Term: term, NodeIDs: []string{}}
		seen := map[string]bool{}
		for _, posting := range r.schema.GetPostings() {
			row := posting.GetTermRowPrefix() + term
			for _, cell := range cells[row] {
				if !r.cellVisible(ctx, cell.GetColumnVisibility()) {
					continue
				}
				if cf := posting.GetPostingCf(); cf != "" && string(cell.GetColumnFamily()) != cf {
					continue
				}
				id := cell.GetColumnQualifier()
				if posting.GetIdSource() == "value" {
					id = cell.GetValue()
				}
				if len(id) == 0 {
					continue
				}
				node := posting.GetPrimaryPrefix() + string(id)
				if !utf8.ValidString(node) || seen[node] {
					continue
				}
				seen[node] = true
				hit.NodeIDs = append(hit.NodeIDs, node)
			}
		}
		sort.Strings(hit.NodeIDs)
		if r.schema.GetCellVisibility() && len(hit.NodeIDs) > 0 {
			checkedNodes += len(hit.NodeIDs)
			if checkedNodes > MaxNodes {
				return nil, fmt.Errorf("%w: term lookup exceeds %d candidate nodes", ErrInvalid, MaxNodes)
			}
			visibleIDs := make([]string, 0, len(hit.NodeIDs))
			for start := 0; start < len(hit.NodeIDs); start += MaxSeeds {
				end := min(start+MaxSeeds, len(hit.NodeIDs))
				var valid []string
				for _, id := range hit.NodeIDs[start:end] {
					if r.validateRow("term target", id) == nil {
						valid = append(valid, id)
					}
				}
				if len(valid) == 0 {
					continue
				}
				nodes, err := r.Nodes(ctx, valid)
				if err != nil {
					return nil, err
				}
				for _, node := range nodes {
					if node.Found {
						visibleIDs = append(visibleIDs, node.ID)
					}
				}
			}
			hit.NodeIDs = visibleIDs
		}
		out = append(out, hit)
	}
	return out, nil
}

// rows reads each listed row through the store, concurrently up to
// Parallelism, and returns the cells by row.
func (r *Reader) rows(ctx context.Context, ids []string) (map[string][]*embedpb.Cell, error) {
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	out := make(map[string][]*embedpb.Cell, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	parallel := r.Parallelism
	if parallel <= 0 {
		parallel = DefaultParallelism
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	sem := make(chan struct{}, parallel)
	for _, id := range unique {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			cells, err := r.store.Scan(ctx, &embedpb.ScanRequest{
				Table: r.Table(), StartRow: []byte(id), StartInclusive: true,
				EndRow: []byte(id), EndInclusive: true, Limit: MaxRowCells + 1,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("graphtable: read row %q: %w", id, err)
					cancel()
				}
				return
			}
			out[id] = cells
		}(id)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// rowStats is what parseRow left out of a row.
type rowStats struct {
	// oversized reports a row read up to MaxRowCells and cut there.
	oversized bool
	// dropped counts edge cells whose target the reader cannot name.
	dropped int
}

// parseRow turns a row's cells into its node and out-edges. Cells of a
// family an edge, vector, or posting entry claims are never attributes.
func (r *Reader) parseRow(ctx context.Context, id string, cells []*embedpb.Cell, filter *edgeFilter) (Node, []Edge, rowStats) {
	var stats rowStats
	if len(cells) > MaxRowCells {
		cells, stats.oversized = cells[:MaxRowCells], true
	}
	ns, _ := r.namespaceOf(id)
	node := Node{ID: id, Key: id[len(ns.spec.GetRowPrefix()):], Identifier: ns.identifierOf(id), Namespace: ns.spec.GetName(), Kind: ns.kindOf(id)}
	var edges []Edge
	attributeCFs := map[string]bool{}
	for _, cf := range ns.spec.GetAttributeCfs() {
		attributeCFs[cf] = true
	}
	for _, cell := range cells {
		if !bytes.Equal(cell.GetRow(), []byte(id)) {
			continue
		}
		cf := string(cell.GetColumnFamily())
		if !r.cellVisible(ctx, cell.GetColumnVisibility()) {
			if _, isEdge := r.edges[cf]; isEdge {
				stats.dropped++
			}
			continue
		}
		node.Found = true
		if family, ok := r.edges[cf]; ok {
			edge, ok := family.parse(id, cell)
			if !ok {
				continue
			}
			if r.validateRow("edge target", edge.To) != nil {
				stats.dropped++
				continue
			}
			if filter.allows(edge) {
				edges = append(edges, edge)
			}
			continue
		}
		if r.claimed[cf] {
			continue
		}
		if len(attributeCFs) > 0 && !attributeCFs[cf] {
			continue
		}
		attribute := Attribute{Family: cf, Value: decodeValue(ns.spec.GetAttributeEncoding(), cell.GetValue())}
		if ns.spec.GetAttributeQualifier() != schemapb.QualifierRole_QUALIFIER_ROLE_OPAQUE {
			attribute.Field = printable(cell.GetColumnQualifier())
		}
		node.Attributes = append(node.Attributes, attribute)
	}
	sort.SliceStable(node.Attributes, func(i, j int) bool {
		if node.Attributes[i].Family != node.Attributes[j].Family {
			return node.Attributes[i].Family < node.Attributes[j].Family
		}
		return node.Attributes[i].Field < node.Attributes[j].Field
	})
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Family != edges[j].Family {
			return edges[i].Family < edges[j].Family
		}
		if edges[i].Relationship != edges[j].Relationship {
			return edges[i].Relationship < edges[j].Relationship
		}
		return edges[i].To < edges[j].To
	})
	return node, edges, stats
}

// parse reads one edge cell exactly as the engine's EdgeExpand iterator
// would: the token is the qualifier or the value, split by the separator
// when one is set, with the id at id_index (the last field when unset) and
// the relationship at rel_index; the neighbor row is primary_prefix + id.
func (f *edgeFamily) parse(from string, cell *embedpb.Cell) (Edge, bool) {
	token := cell.GetColumnQualifier()
	if f.spec.GetEdgeField() == "value" {
		token = cell.GetValue()
	}
	if len(token) == 0 {
		return Edge{}, false
	}
	edge := Edge{From: from, Family: f.spec.GetName(), Weight: 1}
	var id []byte
	if sep := f.spec.GetFieldSep(); sep == "" {
		id = token
	} else {
		fields := bytes.Split(token, []byte(sep))
		if rel := int(f.spec.GetRelIndex()); rel >= 0 && rel < len(fields) {
			edge.Relationship = printable(fields[rel])
		}
		idx := f.idIndex
		if idx == -1 {
			idx = len(fields) - 1
		}
		if idx < 0 || idx >= len(fields) {
			return Edge{}, false
		}
		id = fields[idx]
	}
	if len(id) == 0 {
		return Edge{}, false
	}
	edge.To = f.spec.GetPrimaryPrefix() + string(id)
	if f.spec.GetValueEncoding() == schemapb.ValueEncoding_VALUE_ENCODING_FLOAT32_BE && f.spec.GetEdgeField() != "value" {
		if raw := cell.GetValue(); len(raw) == 4 {
			edge.Weight, edge.Weighted = math.Float32frombits(binary.BigEndian.Uint32(raw)), true
		}
	}
	return edge, true
}

func decodeValue(encoding schemapb.ValueEncoding, raw []byte) string {
	switch encoding {
	case schemapb.ValueEncoding_VALUE_ENCODING_FLOAT32_BE:
		if len(raw) == 4 {
			return strconv.FormatFloat(float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), 'g', -1, 32)
		}
		return base64.StdEncoding.EncodeToString(raw)
	case schemapb.ValueEncoding_VALUE_ENCODING_BYTES:
		return base64.StdEncoding.EncodeToString(raw)
	default:
		return printable(raw)
	}
}

// printable renders bytes as text when they are valid UTF-8 and as base64
// otherwise, so a JSON response never carries a replacement character for a
// byte string that was something else.
func printable(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// walk is one expansion's state: the discovery order, the frontier still
// to read, and the edges followed.
type walk struct {
	reader   *Reader
	fanout   int
	maxNodes int
	filter   *edgeFilter

	order     []string
	nodes     map[string]Node
	frontier  []string
	edges     []Edge
	parent    map[string]Edge
	truncated bool
	dropped   int
}

func newWalk(reader *Reader, fanout, maxNodes int, filter *edgeFilter) *walk {
	return &walk{reader: reader, fanout: fanout, maxNodes: maxNodes, filter: filter, nodes: map[string]Node{}, parent: map[string]Edge{}}
}

// discover adds a row to the walk once. It reports false when the row is
// already known or the node budget is spent.
func (w *walk) discover(id string) bool {
	if _, known := w.nodes[id]; known {
		return false
	}
	if len(w.order) >= w.maxNodes {
		w.truncated = true
		return false
	}
	w.nodes[id] = Node{ID: id}
	w.order = append(w.order, id)
	w.frontier = append(w.frontier, id)
	return true
}

// expand reads the frontier's rows and follows their edges.
func (w *walk) expand(ctx context.Context) error {
	frontier := w.frontier
	w.frontier = nil
	rows, err := w.reader.rows(ctx, frontier)
	if err != nil {
		return err
	}
	for _, id := range frontier {
		node, edges, stats := w.reader.parseRow(ctx, id, rows[id], w.filter)
		w.nodes[id] = node
		w.dropped += stats.dropped
		if stats.oversized {
			w.truncated = true
		}
		if len(edges) > w.fanout {
			edges = edges[:w.fanout]
			w.truncated = true
		}
		for _, edge := range edges {
			if _, known := w.nodes[edge.To]; !known {
				if !w.discover(edge.To) {
					continue
				}
				w.parent[edge.To] = edge
			}
			w.edges = append(w.edges, edge)
		}
	}
	return nil
}

// materializeFrontier reads the rows the last hop reached, so the result
// carries their kinds and attributes, without following their edges.
func (w *walk) materializeFrontier(ctx context.Context) error {
	frontier := w.frontier
	w.frontier = nil
	rows, err := w.reader.rows(ctx, frontier)
	if err != nil {
		return err
	}
	for _, id := range frontier {
		node, _, stats := w.reader.parseRow(ctx, id, rows[id], nil)
		w.nodes[id] = node
		if stats.oversized {
			w.truncated = true
		}
	}
	return nil
}

func (w *walk) result() Neighborhood {
	out := Neighborhood{Nodes: make([]Node, 0, len(w.order)), Edges: w.edges, Truncated: w.truncated, DroppedEdges: w.dropped}
	if out.Edges == nil {
		out.Edges = []Edge{}
	}
	for _, id := range w.order {
		node := w.nodes[id]
		if !w.reader.schema.GetCellVisibility() || node.Found {
			out.Nodes = append(out.Nodes, node)
		}
	}
	if w.reader.schema.GetCellVisibility() {
		visibleEdges := out.Edges[:0]
		for _, edge := range out.Edges {
			if w.nodes[edge.From].Found && w.nodes[edge.To].Found {
				visibleEdges = append(visibleEdges, edge)
			} else {
				out.DroppedEdges++
			}
		}
		out.Edges = visibleEdges
	}
	return out
}
