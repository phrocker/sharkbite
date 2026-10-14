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
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// LatentEdgeDiscoveryIterator is the majc iterator on the graph_vidx table -
// a port of org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator.
//
// For every tessellation cell encountered (row prefix before the first ':'),
// the iterator buffers all `V:_embedding` cells in that cell, runs pairwise
// cosine similarity, and writes bidirectional edge cells for every pair at or
// above the similarity threshold. Original cells pass through unchanged. Edge
// cells already present are passed through but NOT re-processed.
//
// The same implementation also backs the registered semanticEdge iterator. The
// veculo SemanticEdgeIterator is the same core algorithm (embedding cosine
// threshold -> emitted semantic edges), but over a whole seek range instead of
// one tessellation cell, with top-N edges per vertex and an edge CF convention.
// NewSemanticEdgeIterator enables that mode with default edgeCF "edge.sem:".
//
// Emitting modes are deterministic: generated timestamps are max source
// timestamp in the emission boundary + 1 (cell for latent mode, whole seek range
// for semantic mode). Never use wall-clock timestamps in this iterator.
//
// Options:
//
//	similarityThreshold (float, default 0.85)
//	maxPairsPerCell     (int,   default 500; latent mode only)
//	maxCellBuffer       (int,   default 200; latent mode only)
//	edgeCF              emitted edge column family (latent default "link", semantic default "edge.sem:")
//	embeddingCF         embedding column family (default "V")
//	embeddingCQ         embedding column qualifier (default "_embedding")
//	maxEdgesPerVertex   top-N semantic edges per source vertex (semantic default 10; <=0 unlimited)
//	maxVectors          skip semantic computation when more vectors are found (semantic default 10000)
//	direction           "bidirectional" (default) or "forward" for semantic candidate emission
//	inverseEdgeCF       optional inverse edge CF for semantic mode
//	semanticMode        "true" enables whole-range semantic mode on this iterator id
//
// Edge CFs are schema-agnostic strings supplied by options. For the agentic-memory graph
// convention use edgeCF="edge.sem:"; iterrt intentionally does not import
// internal/graphschema.
type LatentEdgeDiscoveryIterator struct {
	source SortedKeyValueIterator

	similarityThreshold float32
	maxPairsPerCell     int
	maxCellBuffer       int

	edgeCF      string
	embeddingCF string
	embeddingCQ string

	semanticMode      bool
	maxEdgesPerVertex int
	maxVectors        int
	direction         string
	inverseEdgeCF     string

	out      []Cell // merged + sorted output buffer
	outIndex int
	err      error
}

// LatentEdgeDiscoveryIterator option keys.
const (
	LatentEdgeSimilarityThreshold = "similarityThreshold"
	LatentEdgeMaxPairsPerCell     = "maxPairsPerCell"
	LatentEdgeMaxCellBuffer       = "maxCellBuffer"
	LatentEdgeEdgeCF              = "edgeCF"
	LatentEdgeEmbeddingCF         = "embeddingCF"
	LatentEdgeEmbeddingCQ         = "embeddingCQ"
	LatentEdgeSemanticMode        = "semanticMode"
	LatentEdgeMaxEdgesPerVertex   = "maxEdgesPerVertex"
	LatentEdgeMaxVectors          = "maxVectors"
	LatentEdgeDirection           = "direction"
	LatentEdgeInverseEdgeCF       = "inverseEdgeCF"
)

// Cell-stream constants - match VectorIndexWriter/LatentEdgeDiscoveryIterator.
const (
	latentVertexCF      = "V"
	latentEmbeddingCQ   = "_embedding"
	latentLinkCF        = "link" // VectorIndexWriter.LINK_COLFAM
	semanticDefaultCF   = "edge.sem:"
	latentDefaultThresh = float32(0.85)
	latentDefaultPairs  = 500
	latentDefaultBuffer = 200
	semanticDefaultTopN = 10
	semanticMaxVectors  = 10000

	latentDirectionBidirectional = "bidirectional"
	latentDirectionForward       = "forward"
)

// NewLatentEdgeDiscoveryIterator constructs an un-Init'd latent iterator.
func NewLatentEdgeDiscoveryIterator() *LatentEdgeDiscoveryIterator {
	return &LatentEdgeDiscoveryIterator{
		similarityThreshold: latentDefaultThresh,
		maxPairsPerCell:     latentDefaultPairs,
		maxCellBuffer:       latentDefaultBuffer,
		edgeCF:              latentLinkCF,
		embeddingCF:         latentVertexCF,
		embeddingCQ:         latentEmbeddingCQ,
		maxEdgesPerVertex:   0,
		maxVectors:          semanticMaxVectors,
		direction:           latentDirectionBidirectional,
	}
}

// NewSemanticEdgeIterator constructs an un-Init'd semantic-edge iterator. It
// reuses LatentEdgeDiscoveryIterator's deterministic emission engine in whole-
// range mode instead of duplicating the cosine-threshold implementation.
func NewSemanticEdgeIterator() *LatentEdgeDiscoveryIterator {
	it := NewLatentEdgeDiscoveryIterator()
	it.semanticMode = true
	it.edgeCF = semanticDefaultCF
	it.maxEdgesPerVertex = semanticDefaultTopN
	return it
}

// Init parses options from the map.
func (l *LatentEdgeDiscoveryIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: LatentEdgeDiscoveryIterator requires a non-nil source")
	}
	l.source = source

	if s, ok := options[LatentEdgeSimilarityThreshold]; ok && s != "" {
		v, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeSimilarityThreshold, s)
		}
		l.similarityThreshold = float32(v)
	}
	if s, ok := options[LatentEdgeMaxPairsPerCell]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeMaxPairsPerCell, s)
		}
		l.maxPairsPerCell = v
	}
	if s, ok := options[LatentEdgeMaxCellBuffer]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeMaxCellBuffer, s)
		}
		l.maxCellBuffer = v
	}
	if s := options[LatentEdgeEdgeCF]; s != "" {
		l.edgeCF = s
	}
	if s := options[LatentEdgeEmbeddingCF]; s != "" {
		l.embeddingCF = s
	}
	if s := options[LatentEdgeEmbeddingCQ]; s != "" {
		l.embeddingCQ = s
	}
	if s := options[LatentEdgeSemanticMode]; s == "true" {
		l.semanticMode = true
		if l.edgeCF == latentLinkCF {
			l.edgeCF = semanticDefaultCF
		}
		if l.maxEdgesPerVertex == 0 {
			l.maxEdgesPerVertex = semanticDefaultTopN
		}
	}
	if s, ok := options[LatentEdgeMaxEdgesPerVertex]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeMaxEdgesPerVertex, s)
		}
		l.maxEdgesPerVertex = v
	}
	if s, ok := options[LatentEdgeMaxVectors]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeMaxVectors, s)
		}
		l.maxVectors = v
	}
	if s := options[LatentEdgeDirection]; s != "" {
		switch s {
		case latentDirectionBidirectional, latentDirectionForward:
			l.direction = s
		default:
			return fmt.Errorf("iterrt: LatentEdgeDiscoveryIterator bad %s=%q", LatentEdgeDirection, s)
		}
	}
	if s := options[LatentEdgeInverseEdgeCF]; s != "" {
		l.inverseEdgeCF = s
	}
	return nil
}

func (l *LatentEdgeDiscoveryIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	l.out = l.out[:0]
	l.outIndex = 0
	l.err = nil

	if err := l.source.Seek(r, columnFamilies, inclusive); err != nil {
		l.err = err
		return err
	}
	if l.semanticMode {
		return l.seekSemantic()
	}
	return l.seekLatent()
}

func (l *LatentEdgeDiscoveryIterator) seekLatent() error {
	cellVerts := []string{}
	cellEmb := map[string]semanticEmbedding{}
	var currentCell string
	var currentCellMaxTS int64

	for l.source.HasTop() {
		k := l.source.GetTopKey().Clone()
		v := append([]byte(nil), l.source.GetTopValue()...)
		l.out = append(l.out, Cell{Key: k, Value: v})

		sep := bytes.IndexByte(k.Row, ':')
		if sep < 0 {
			if err := l.source.Next(); err != nil {
				l.err = err
				return err
			}
			continue
		}
		cellID := string(k.Row[:sep])
		if currentCell != "" && currentCell != cellID {
			l.processCell(currentCell, cellVerts, cellEmb, currentCellMaxTS+1)
			cellVerts = cellVerts[:0]
			for kk := range cellEmb {
				delete(cellEmb, kk)
			}
			currentCellMaxTS = 0
		}
		currentCell = cellID
		if k.Timestamp > currentCellMaxTS {
			currentCellMaxTS = k.Timestamp
		}

		if string(k.ColumnFamily) != l.embeddingCF || string(k.ColumnQualifier) != l.embeddingCQ {
			if err := l.source.Next(); err != nil {
				l.err = err
				return err
			}
			continue
		}
		vertexID := string(k.Row[sep+1:])
		if emb := parseEmbedding(v); emb != nil && len(cellEmb) < l.maxCellBuffer {
			if _, dup := cellEmb[vertexID]; !dup {
				cellVerts = append(cellVerts, vertexID)
			}
			cellEmb[vertexID] = semanticEmbedding{vertexID: vertexID, vector: emb, visibility: append([]byte(nil), k.ColumnVisibility...)}
		}
		if err := l.source.Next(); err != nil {
			l.err = err
			return err
		}
	}
	if currentCell != "" && len(cellEmb) > 0 {
		l.processCell(currentCell, cellVerts, cellEmb, currentCellMaxTS+1)
	}
	l.sortOut()
	return nil
}

func (l *LatentEdgeDiscoveryIterator) seekSemantic() error {
	vertices := []string{}
	emb := map[string]semanticEmbedding{}
	var maxTS int64
	for l.source.HasTop() {
		k := l.source.GetTopKey().Clone()
		v := append([]byte(nil), l.source.GetTopValue()...)
		l.out = append(l.out, Cell{Key: k, Value: v})
		if k.Timestamp > maxTS {
			maxTS = k.Timestamp
		}
		if string(k.ColumnFamily) == l.embeddingCF && string(k.ColumnQualifier) == l.embeddingCQ {
			id := string(k.Row)
			if vec := parseEmbedding(v); vec != nil {
				if _, dup := emb[id]; !dup {
					vertices = append(vertices, id)
				}
				emb[id] = semanticEmbedding{vertexID: id, vector: vec, visibility: append([]byte(nil), k.ColumnVisibility...)}
			}
		}
		if err := l.source.Next(); err != nil {
			l.err = err
			return err
		}
	}
	if l.maxVectors <= 0 || len(emb) <= l.maxVectors {
		l.processSemantic(vertices, emb, maxTS+1)
	}
	l.sortOut()
	return nil
}

func (l *LatentEdgeDiscoveryIterator) processCell(cellID string, vertices []string, emb map[string]semanticEmbedding, ts int64) {
	sort.Strings(vertices)
	pairsChecked := 0
	for i := 0; i < len(vertices) && pairsChecked < l.maxPairsPerCell; i++ {
		for j := i + 1; j < len(vertices) && pairsChecked < l.maxPairsPerCell; j++ {
			a, b := vertices[i], vertices[j]
			ea, eb := emb[a].vector, emb[b].vector
			if len(ea) != len(eb) {
				pairsChecked++
				continue
			}
			sim := cosineSimilarity(ea, eb)
			pairsChecked++
			if sim < l.similarityThreshold {
				continue
			}
			val := []byte(fmt.Sprintf("%.4f", sim))
			l.out = append(l.out,
				Cell{Key: &wire.Key{Row: []byte(cellID + ":" + a), ColumnFamily: []byte(l.edgeCF), ColumnQualifier: []byte(b), Timestamp: ts}, Value: val},
				Cell{Key: &wire.Key{Row: []byte(cellID + ":" + b), ColumnFamily: []byte(l.edgeCF), ColumnQualifier: []byte(a), Timestamp: ts}, Value: val},
			)
		}
	}
}

func (l *LatentEdgeDiscoveryIterator) processSemantic(vertices []string, emb map[string]semanticEmbedding, ts int64) {
	sort.Strings(vertices)
	candidates := map[string][]semanticCandidate{}
	for i := 0; i < len(vertices); i++ {
		for j := i + 1; j < len(vertices); j++ {
			a, b := emb[vertices[i]], emb[vertices[j]]
			if len(a.vector) != len(b.vector) {
				continue
			}
			sim := cosineSimilarity(a.vector, b.vector)
			if sim < l.similarityThreshold {
				continue
			}
			candidates[a.vertexID] = append(candidates[a.vertexID], semanticCandidate{neighborID: b.vertexID, similarity: sim, visibility: compoundVisibility(a.visibility, b.visibility)})
			if l.direction == latentDirectionBidirectional {
				candidates[b.vertexID] = append(candidates[b.vertexID], semanticCandidate{neighborID: a.vertexID, similarity: sim, visibility: compoundVisibility(a.visibility, b.visibility)})
			}
		}
	}
	for _, vertexID := range vertices {
		cs := candidates[vertexID]
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].similarity != cs[j].similarity {
				return cs[i].similarity > cs[j].similarity
			}
			return cs[i].neighborID < cs[j].neighborID
		})
		limit := len(cs)
		if l.maxEdgesPerVertex > 0 && limit > l.maxEdgesPerVertex {
			limit = l.maxEdgesPerVertex
		}
		for i := 0; i < limit; i++ {
			c := cs[i]
			val := []byte(fmt.Sprintf("%.4f", c.similarity))
			l.out = append(l.out, Cell{Key: &wire.Key{Row: []byte(vertexID), ColumnFamily: []byte(l.edgeCF), ColumnQualifier: []byte(c.neighborID), ColumnVisibility: c.visibility, Timestamp: ts}, Value: val})
			if l.inverseEdgeCF != "" {
				l.out = append(l.out, Cell{Key: &wire.Key{Row: []byte(c.neighborID), ColumnFamily: []byte(l.inverseEdgeCF), ColumnQualifier: []byte(vertexID), ColumnVisibility: c.visibility, Timestamp: ts}, Value: val})
			}
		}
	}
}

func (l *LatentEdgeDiscoveryIterator) sortOut() {
	sort.SliceStable(l.out, func(i, j int) bool { return l.out[i].Key.Compare(l.out[j].Key) < 0 })
}

type semanticEmbedding struct {
	vertexID   string
	vector     []float32
	visibility []byte
}

type semanticCandidate struct {
	neighborID string
	similarity float32
	visibility []byte
}

func compoundVisibility(a, b []byte) []byte {
	if len(a) == 0 {
		return append([]byte(nil), b...)
	}
	if len(b) == 0 || bytes.Equal(a, b) {
		return append([]byte(nil), a...)
	}
	if bytes.Compare(a, b) > 0 {
		a, b = b, a
	}
	out := make([]byte, 0, len(a)+1+len(b))
	out = append(out, a...)
	out = append(out, '&')
	out = append(out, b...)
	return out
}

// parseEmbedding decodes a BIG_ENDIAN float32 array.
func parseEmbedding(v []byte) []float32 {
	if len(v) < 4 || len(v)%4 != 0 {
		return nil
	}
	out := make([]float32, len(v)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.BigEndian.Uint32(v[i*4 : i*4+4]))
	}
	return out
}

func cosineSimilarity(a, b []float32) float32 {
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(na))*math.Sqrt(float64(nb)))
}

func (l *LatentEdgeDiscoveryIterator) HasTop() bool { return l.err == nil && l.outIndex < len(l.out) }
func (l *LatentEdgeDiscoveryIterator) GetTopKey() *Key {
	if !l.HasTop() {
		return nil
	}
	return l.out[l.outIndex].Key
}
func (l *LatentEdgeDiscoveryIterator) GetTopValue() []byte {
	if !l.HasTop() {
		return nil
	}
	return l.out[l.outIndex].Value
}
func (l *LatentEdgeDiscoveryIterator) Next() error {
	if l.err != nil {
		return l.err
	}
	if !l.HasTop() {
		return errors.New("iterrt: LatentEdgeDiscoveryIterator.Next called without a top")
	}
	l.outIndex++
	return nil
}

func (l *LatentEdgeDiscoveryIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &LatentEdgeDiscoveryIterator{
		source:              l.source.DeepCopy(env),
		similarityThreshold: l.similarityThreshold,
		maxPairsPerCell:     l.maxPairsPerCell,
		maxCellBuffer:       l.maxCellBuffer,
		edgeCF:              l.edgeCF,
		embeddingCF:         l.embeddingCF,
		embeddingCQ:         l.embeddingCQ,
		semanticMode:        l.semanticMode,
		maxEdgesPerVertex:   l.maxEdgesPerVertex,
		maxVectors:          l.maxVectors,
		direction:           l.direction,
		inverseEdgeCF:       l.inverseEdgeCF,
	}
}
