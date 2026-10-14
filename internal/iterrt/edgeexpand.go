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

package iterrt

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// EdgeExpandIterator is the graph-expansion pushdown iterator described
// in docs/ai-knowledge-graph.md capability (3). It moves neighborhood traversal
// server-side, next to the data.
//
// A consumer co-locates a node's out-edges with the node row as ordinary cells:
// each edge is a cell whose column family marks it as an edge and whose column
// qualifier (or value) encodes the relationship and the neighbor's id. Reading
// a neighborhood without pushdown means the client scans each anchor row,
// extracts neighbor ids, then issues one point lookup per neighbor — streaming
// the edges only to throw them away and round-tripping per hop. This iterator
// does that traversal next to the data: given a set of anchor rows, it walks
// their edge cells, extracts each neighbor id, resolves it to a neighbor row,
// and emits the neighbor rows' cells directly. One Seek therefore returns the
// one-hop neighborhood, not the whole table.
//
// The iterator defines only the mechanism (anchor edge cells -> neighbor rows).
// The edge schema is the consumer's, supplied via options — no relationship or
// column-family vocabulary is baked in:
//
//	anchorCount    number of anchor rows (N); rows are anchor.0 .. anchor.<N-1>
//	anchor.<i>     the i-th anchor row key (raw bytes as a string)
//	edgeCF         optional column family marking edge cells on an anchor row;
//	               empty = every cell on an anchor row is an edge
//	edgeField      "qualifier" (default) | "value" — where the edge token (the
//	               relationship+neighbor encoding) lives in an edge cell
//	fieldSep       optional separator splitting the edge token into fields
//	               (e.g. a NUL between relationship and neighbor id); empty = the
//	               whole token is the neighbor id
//	idIndex        when fieldSep is set, the split-field index holding the
//	               neighbor id; -1 (default) = the last field
//	relIndex       when fieldSep is set, the split-field index holding the
//	               relationship (default 0); used only for relationship filtering
//	relCount       optional number of allowed relationships (rel.0 .. rel.<M-1>);
//	               0 = every relationship is expanded. Requires fieldSep.
//	rel.<i>        the i-th allowed relationship string
//	primaryPrefix  prepended to each neighbor id to form the neighbor row key
//	includeAnchors "true" also emits the anchor rows' own cells; default false
//	maxHops       hop limit; 0/1 preserves the original one-hop behaviour
//	edgeWeightCount
//	               optional number of relationship weights. Each entry is
//	               encoded as edgeWeight.rel.<i> and edgeWeight.weight.<i>.
//	               When supplied, only relationships with weight > 0 are
//	               expanded, and each hop's frontier is ordered by descending
//	               weight, then relationship/id, before de-duplication.
//
// Resolution is the union of neighbors across all anchors, de-duplicated. The
// neighbor row key is primaryPrefix + id. Output is every cell of each resolved
// row that falls within the Seek range and column-family filter, sorted by
// wire.Key — identical in shape to an ordinary scan restricted to those rows.
//
// Source contract: re-seekable (seeked once per anchor row and once per
// resolved neighbor row), so it is hosted above a whole-table merge — anchors
// and their neighbors may live in different tablets.
//
// Edges stored as separate edge rows (rather than co-located cells) are served
// by an ordinary prefix range scan over the edge-row prefix; this iterator
// targets the co-located-cell layout, where the neighbor id is embedded in the
// edge cell and a point resolution per neighbor would otherwise be required.
type EdgeExpandIterator struct {
	source SortedKeyValueIterator

	anchors        [][]byte
	edgeCF         []byte // nil/empty = any cf
	edgeFromValue  bool   // false = qualifier (default), true = value
	fieldSep       []byte // nil/empty = whole token is the id
	idIndex        int    // -1 = last field
	relIndex       int
	allowedRels    map[string]struct{} // nil = all relationships
	edgeWeights    map[string]float32  // nil = unweighted; positive weights only
	primaryPrefix  []byte
	includeAnchors bool
	maxHops        int

	out      []Cell
	outIndex int
	err      error
}

// EdgeExpandIterator option keys.
const (
	// EdgeExpandAnchorCount is the number of anchor rows supplied
	// (anchor.0..anchor.N-1).
	EdgeExpandAnchorCount = "anchorCount"
	// EdgeExpandAnchorPrefix is the option-key prefix for each anchor row; the
	// i-th anchor row is at option "anchor.<i>".
	EdgeExpandAnchorPrefix = "anchor."
	// EdgeExpandEdgeCF optionally restricts which column family of an anchor
	// row counts as an edge cell.
	EdgeExpandEdgeCF = "edgeCF"
	// EdgeExpandEdgeField selects where the edge token lives in an edge cell:
	// "qualifier" (default) or "value".
	EdgeExpandEdgeField = "edgeField"
	// EdgeExpandFieldSep optionally splits the edge token into fields.
	EdgeExpandFieldSep = "fieldSep"
	// EdgeExpandIDIndex is the split-field index of the neighbor id when
	// fieldSep is set; -1 (default) = the last field.
	EdgeExpandIDIndex = "idIndex"
	// EdgeExpandRelIndex is the split-field index of the relationship when
	// fieldSep is set; default 0.
	EdgeExpandRelIndex = "relIndex"
	// EdgeExpandRelCount is the number of allowed relationships
	// (rel.0..rel.M-1); 0 = all. Requires fieldSep.
	EdgeExpandRelCount = "relCount"
	// EdgeExpandRelPrefix is the option-key prefix for each allowed
	// relationship; the i-th is at option "rel.<i>".
	EdgeExpandRelPrefix = "rel."
	// EdgeExpandPrimaryPrefix is prepended to each neighbor id to form the
	// neighbor row key.
	EdgeExpandPrimaryPrefix = "primaryPrefix"
	// EdgeExpandIncludeAnchors, when "true", also emits the anchor rows' cells.
	EdgeExpandIncludeAnchors = "includeAnchors"
	// EdgeExpandMaxHops is the traversal hop limit; 0/1 preserves one-hop.
	EdgeExpandMaxHops = "maxHops"
	// EdgeExpandWeightCount is the number of relationship weights.
	EdgeExpandWeightCount = "edgeWeightCount"
	// EdgeExpandWeightRelPrefix is the option-key prefix for a weighted
	// relationship; the i-th is at "edgeWeight.rel.<i>".
	EdgeExpandWeightRelPrefix = "edgeWeight.rel."
	// EdgeExpandWeightValuePrefix is the option-key prefix for a weight value;
	// the i-th is at "edgeWeight.weight.<i>".
	EdgeExpandWeightValuePrefix = "edgeWeight.weight."

	edgeExpandFieldQualifier = "qualifier"
	edgeExpandFieldValue     = "value"
)

// NewEdgeExpandIterator constructs an un-Init'd iterator.
func NewEdgeExpandIterator() *EdgeExpandIterator {
	return &EdgeExpandIterator{idIndex: -1}
}

// Init wires the source and parses options. anchorCount must be a non-negative
// integer with every anchor.<i> present; edgeField, if set, must be
// "qualifier" or "value"; a relationship allowlist requires fieldSep.
func (e *EdgeExpandIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: EdgeExpandIterator requires a non-nil source")
	}
	e.source = source

	n := 0
	if s, ok := options[EdgeExpandAnchorCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandAnchorCount, s)
		}
		n = v
	}
	e.anchors = make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s%d", EdgeExpandAnchorPrefix, i)
		s, ok := options[key]
		if !ok {
			return fmt.Errorf("iterrt: EdgeExpandIterator missing option %q (anchorCount=%d)", key, n)
		}
		e.anchors = append(e.anchors, []byte(s))
	}

	if s, ok := options[EdgeExpandEdgeCF]; ok && s != "" {
		e.edgeCF = []byte(s)
	}
	switch options[EdgeExpandEdgeField] {
	case "", edgeExpandFieldQualifier:
		e.edgeFromValue = false
	case edgeExpandFieldValue:
		e.edgeFromValue = true
	default:
		return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q (want %q or %q)",
			EdgeExpandEdgeField, options[EdgeExpandEdgeField], edgeExpandFieldQualifier, edgeExpandFieldValue)
	}

	if s, ok := options[EdgeExpandFieldSep]; ok && s != "" {
		e.fieldSep = []byte(s)
	}

	e.idIndex = -1
	if s, ok := options[EdgeExpandIDIndex]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < -1 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandIDIndex, s)
		}
		e.idIndex = v
	}
	e.relIndex = 0
	if s, ok := options[EdgeExpandRelIndex]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandRelIndex, s)
		}
		e.relIndex = v
	}

	relN := 0
	if s, ok := options[EdgeExpandRelCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandRelCount, s)
		}
		relN = v
	}
	if relN > 0 {
		if len(e.fieldSep) == 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator %s requires %s", EdgeExpandRelCount, EdgeExpandFieldSep)
		}
		e.allowedRels = make(map[string]struct{}, relN)
		for i := 0; i < relN; i++ {
			key := fmt.Sprintf("%s%d", EdgeExpandRelPrefix, i)
			s, ok := options[key]
			if !ok {
				return fmt.Errorf("iterrt: EdgeExpandIterator missing option %q (relCount=%d)", key, relN)
			}
			e.allowedRels[s] = struct{}{}
		}
	}

	if s, ok := options[EdgeExpandPrimaryPrefix]; ok {
		e.primaryPrefix = []byte(s)
	}
	if s, ok := options[EdgeExpandIncludeAnchors]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandIncludeAnchors, s)
		}
		e.includeAnchors = v
	}
	e.maxHops = 1
	if s, ok := options[EdgeExpandMaxHops]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandMaxHops, s)
		}
		if v > 1 {
			e.maxHops = v
		}
	}
	weightN := 0
	if s, ok := options[EdgeExpandWeightCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", EdgeExpandWeightCount, s)
		}
		weightN = v
	}
	if weightN > 0 {
		if len(e.fieldSep) == 0 {
			return fmt.Errorf("iterrt: EdgeExpandIterator %s requires %s", EdgeExpandWeightCount, EdgeExpandFieldSep)
		}
		e.edgeWeights = make(map[string]float32, weightN)
		for i := 0; i < weightN; i++ {
			relKey := fmt.Sprintf("%s%d", EdgeExpandWeightRelPrefix, i)
			weightKey := fmt.Sprintf("%s%d", EdgeExpandWeightValuePrefix, i)
			rel, ok := options[relKey]
			if !ok {
				return fmt.Errorf("iterrt: EdgeExpandIterator missing option %q (%s=%d)", relKey, EdgeExpandWeightCount, weightN)
			}
			weightS, ok := options[weightKey]
			if !ok {
				return fmt.Errorf("iterrt: EdgeExpandIterator missing option %q (%s=%d)", weightKey, EdgeExpandWeightCount, weightN)
			}
			weight, err := strconv.ParseFloat(weightS, 32)
			if err != nil {
				return fmt.Errorf("iterrt: EdgeExpandIterator bad %s=%q", weightKey, weightS)
			}
			e.edgeWeights[rel] = float32(weight)
		}
	}
	return nil
}

// Seek walks each anchor's edge cells, resolves neighbors, and buffers the
// neighbor rows' cells (plus the anchors' own cells when includeAnchors). The
// anchor edge scan ignores the requested range and column-family filter
// (anchors are addressed by their own row keys); the requested range r and the
// column-family filter bound only the emitted cells. Subsequent
// HasTop/GetTopKey/GetTopValue/Next walk the buffer.
func (e *EdgeExpandIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	e.out = e.out[:0]
	e.outIndex = 0
	e.err = nil

	// Phase 1: collect neighbor (and optionally anchor) row keys.
	seen := map[string]struct{}{}
	var rows [][]byte
	add := func(rk []byte) {
		if _, dup := seen[string(rk)]; dup {
			return
		}
		seen[string(rk)] = struct{}{}
		rows = append(rows, rk)
	}
	if e.maxHops <= 1 {
		for _, a := range e.anchors {
			if e.includeAnchors {
				add(append([]byte(nil), a...))
			}
			edges, err := e.collectNeighborEdges(a)
			if err != nil {
				e.err = err
				return err
			}
			for _, edge := range edges {
				add(e.rowForID(edge.id))
			}
		}
	} else {
		visited := map[string]struct{}{}
		frontier := make([][]byte, 0, len(e.anchors))
		for _, a := range e.anchors {
			ak := string(a)
			visited[ak] = struct{}{}
			frontier = append(frontier, append([]byte(nil), a...))
			if e.includeAnchors {
				add(append([]byte(nil), a...))
			}
		}
		for hop := 0; hop < e.maxHops && len(frontier) > 0; hop++ {
			var nextEdges []edgeExpandNeighbor
			for _, row := range frontier {
				edges, err := e.collectNeighborEdges(row)
				if err != nil {
					e.err = err
					return err
				}
				nextEdges = append(nextEdges, edges...)
			}
			sort.SliceStable(nextEdges, func(i, j int) bool {
				if nextEdges[i].weight != nextEdges[j].weight {
					return nextEdges[i].weight > nextEdges[j].weight
				}
				if c := bytes.Compare(nextEdges[i].rel, nextEdges[j].rel); c != 0 {
					return c < 0
				}
				return bytes.Compare(nextEdges[i].id, nextEdges[j].id) < 0
			})
			frontier = frontier[:0]
			for _, edge := range nextEdges {
				rk := e.rowForID(edge.id)
				if _, dup := visited[string(rk)]; dup {
					continue
				}
				visited[string(rk)] = struct{}{}
				add(rk)
				frontier = append(frontier, rk)
			}
		}
	}

	// Deterministic output order: resolve rows in sorted-key order so the
	// buffer is globally wire.Key-sorted (cells within a row already are).
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i], rows[j]) < 0
	})

	// Phase 2: fetch each row's cells, bounded by r and the cf filter.
	for _, rk := range rows {
		if err := e.collectRow(rk, r, columnFamilies, inclusive); err != nil {
			e.err = err
			return err
		}
	}
	return nil
}

// collectNeighborEdges seeks the source to anchor row a and returns the neighbor
// edges extracted from its edge cells (restricted to edgeCF when configured, and
// to the relationship allowlist when configured).
func (e *EdgeExpandIterator) collectNeighborEdges(a []byte) ([]edgeExpandNeighbor, error) {
	start := &wire.Key{Row: append([]byte(nil), a...)}
	if err := e.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, nil, false); err != nil {
		return nil, err
	}
	var edges []edgeExpandNeighbor
	for e.source.HasTop() {
		k := e.source.GetTopKey()
		if !bytes.Equal(k.Row, a) {
			break // past the anchor row
		}
		if len(e.edgeCF) == 0 || bytes.Equal(k.ColumnFamily, e.edgeCF) {
			var token []byte
			if e.edgeFromValue {
				token = e.source.GetTopValue()
			} else {
				token = k.ColumnQualifier
			}
			if edge, ok := e.neighborID(token); ok {
				edges = append(edges, edge)
			}
		}
		if err := e.source.Next(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].weight != edges[j].weight {
			return edges[i].weight > edges[j].weight
		}
		if c := bytes.Compare(edges[i].rel, edges[j].rel); c != 0 {
			return c < 0
		}
		return bytes.Compare(edges[i].id, edges[j].id) < 0
	})
	return edges, nil
}

type edgeExpandNeighbor struct {
	id     []byte
	rel    []byte
	weight float32
}

// neighborID extracts the neighbor id from an edge token, applying field
// splitting, the relationship allowlist, and positive relationship weights when
// configured. The returned id is a fresh copy.
// ok is false when the token is empty, malformed, or filtered out.
func (e *EdgeExpandIterator) neighborID(token []byte) (edgeExpandNeighbor, bool) {
	if len(e.fieldSep) == 0 {
		if len(token) == 0 {
			return edgeExpandNeighbor{}, false
		}
		return edgeExpandNeighbor{id: append([]byte(nil), token...), weight: 1}, true
	}
	fields := bytes.Split(token, e.fieldSep)
	var rel []byte
	if e.relIndex < len(fields) {
		rel = fields[e.relIndex]
	}
	if e.allowedRels != nil {
		if e.relIndex >= len(fields) {
			return edgeExpandNeighbor{}, false
		}
		if _, ok := e.allowedRels[string(fields[e.relIndex])]; !ok {
			return edgeExpandNeighbor{}, false
		}
	}
	weight := float32(1)
	if e.edgeWeights != nil {
		if e.relIndex >= len(fields) {
			return edgeExpandNeighbor{}, false
		}
		var ok bool
		weight, ok = e.edgeWeights[string(fields[e.relIndex])]
		if !ok || weight <= 0 {
			return edgeExpandNeighbor{}, false
		}
	}
	idx := e.idIndex
	if idx == -1 {
		idx = len(fields) - 1
	}
	if idx < 0 || idx >= len(fields) {
		return edgeExpandNeighbor{}, false
	}
	id := fields[idx]
	if len(id) == 0 {
		return edgeExpandNeighbor{}, false
	}
	return edgeExpandNeighbor{id: append([]byte(nil), id...), rel: append([]byte(nil), rel...), weight: weight}, true
}

func (e *EdgeExpandIterator) rowForID(id []byte) []byte {
	rk := make([]byte, 0, len(e.primaryPrefix)+len(id))
	rk = append(rk, e.primaryPrefix...)
	rk = append(rk, id...)
	return rk
}

// collectRow seeks the source to row rk and appends its cells (within range r
// and allowed by the cf filter) to the output buffer.
func (e *EdgeExpandIterator) collectRow(rk []byte, r Range, cfs [][]byte, inclusive bool) error {
	start := &wire.Key{Row: append([]byte(nil), rk...)}
	if err := e.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, cfs, inclusive); err != nil {
		return err
	}
	for e.source.HasTop() {
		k := e.source.GetTopKey()
		if !bytes.Equal(k.Row, rk) {
			break // past the row
		}
		if r.Contains(k) {
			e.out = append(e.out, Cell{
				Key:   k.Clone(),
				Value: append([]byte(nil), e.source.GetTopValue()...),
			})
		}
		if err := e.source.Next(); err != nil {
			return err
		}
	}
	return nil
}

// HasTop reports whether a cell is available.
func (e *EdgeExpandIterator) HasTop() bool {
	return e.err == nil && e.outIndex < len(e.out)
}

// GetTopKey returns the current top key, or nil when exhausted.
func (e *EdgeExpandIterator) GetTopKey() *Key {
	if !e.HasTop() {
		return nil
	}
	return e.out[e.outIndex].Key
}

// GetTopValue returns the current top value, or nil when exhausted.
func (e *EdgeExpandIterator) GetTopValue() []byte {
	if !e.HasTop() {
		return nil
	}
	return e.out[e.outIndex].Value
}

// Next advances past the current top.
func (e *EdgeExpandIterator) Next() error {
	if e.err != nil {
		return e.err
	}
	if !e.HasTop() {
		return errors.New("iterrt: EdgeExpandIterator.Next called without a top")
	}
	e.outIndex++
	return nil
}

// DeepCopy returns an un-Seeked iterator over a DeepCopy'd source, carrying the
// same resolved options forward.
func (e *EdgeExpandIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &EdgeExpandIterator{
		source:         e.source.DeepCopy(env),
		edgeCF:         e.edgeCF,
		edgeFromValue:  e.edgeFromValue,
		fieldSep:       e.fieldSep,
		idIndex:        e.idIndex,
		relIndex:       e.relIndex,
		allowedRels:    e.allowedRels,
		edgeWeights:    e.edgeWeights,
		primaryPrefix:  e.primaryPrefix,
		includeAnchors: e.includeAnchors,
		maxHops:        e.maxHops,
	}
	cp.anchors = make([][]byte, len(e.anchors))
	copy(cp.anchors, e.anchors)
	return cp
}
