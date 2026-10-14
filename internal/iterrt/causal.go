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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// CausalInferenceIterator is a terminal graph-query iterator - a port of
// org.apache.accumulo.core.graph.CausalInferenceEngine.
//
// A Seek buffers the selected graph cells, traces one greedy causal chain from
// startVertex, and emits a single JSON result cell. Forward tracing follows
// outgoing edge column families only; backward tracing follows inverse edge
// column families only. At each hop the current vertex embedding updates an
// asymmetric hidden state before candidate neighbors are scored by cosine
// similarity. The chain strength is the product of accepted hop scores.
//
// The iterator is deterministic: generated timestamps are max source timestamp
// in the seek range + 1, buffered cells are key-sorted before graph assembly,
// candidate neighbors are sorted before tie-breaking, and duplicate embeddings
// keep the first key-sorted value instead of depending on scan overwrite order.
//
// Options:
//
//	query.b64            required query vector, packed big-endian float32
//	startVertex          required starting row id
//	direction            "forward" (default) or "backward"
//	maxDepth             maximum followed hops (default 5; 0 returns start)
//	threshold            minimum score for continuing (default 0.2)
//	edgeType             optional edge type; matches edge prefix + edgeType
//	maxVertices          vertex guard (default 100000; exceeded returns start)
//	vertexCF             vertex column family (default "V")
//	embeddingCQ          embedding column qualifier (default "_embedding")
//	edgeCFPrefix         outgoing edge column-family prefix (default "E_")
//	inverseEdgeCFPrefix  incoming edge column-family prefix (default "EI_")
//	resultCF             emitted result column family (default "V")
//	resultCQ             emitted result column qualifier (default "_causal")
type CausalInferenceIterator struct {
	source SortedKeyValueIterator

	query     []float32
	start     string
	direction string
	maxDepth  int
	threshold float32
	edgeType  string

	maxVertices int

	vertexCF            string
	embeddingCQ         string
	edgeCFPrefix        string
	inverseEdgeCFPrefix string
	resultCF            string
	resultCQ            string

	out      []Cell
	outIndex int
	err      error
}

// CausalInferenceIterator option keys.
const (
	CausalInferenceQuery               = "query.b64"
	CausalInferenceStartVertex         = "startVertex"
	CausalInferenceDirection           = "direction"
	CausalInferenceMaxDepth            = "maxDepth"
	CausalInferenceThreshold           = "threshold"
	CausalInferenceEdgeType            = "edgeType"
	CausalInferenceMaxVertices         = "maxVertices"
	CausalInferenceVertexCF            = "vertexCF"
	CausalInferenceEmbeddingCQ         = "embeddingCQ"
	CausalInferenceEdgeCFPrefix        = "edgeCFPrefix"
	CausalInferenceInverseEdgeCFPrefix = "inverseEdgeCFPrefix"
	CausalInferenceResultCF            = "resultCF"
	CausalInferenceResultCQ            = "resultCQ"

	causalDirectionForward  = "forward"
	causalDirectionBackward = "backward"

	causalDefaultMaxDepth            = 5
	causalDefaultThreshold           = float32(0.2)
	causalDefaultMaxVertices         = 100000
	causalDefaultVertexCF            = "V"
	causalDefaultEmbeddingCQ         = "_embedding"
	causalDefaultEdgeCFPrefix        = "E_"
	causalDefaultInverseEdgeCFPrefix = "EI_"
	causalDefaultResultCF            = "V"
	causalDefaultResultCQ            = "_causal"
	causalIteratorErrorPrefix        = "iterrt: CausalInferenceIterator"
)

// NewCausalInferenceIterator constructs an un-Init'd causal inference iterator.
func NewCausalInferenceIterator() *CausalInferenceIterator {
	return &CausalInferenceIterator{
		direction:           causalDirectionForward,
		maxDepth:            causalDefaultMaxDepth,
		threshold:           causalDefaultThreshold,
		maxVertices:         causalDefaultMaxVertices,
		vertexCF:            causalDefaultVertexCF,
		embeddingCQ:         causalDefaultEmbeddingCQ,
		edgeCFPrefix:        causalDefaultEdgeCFPrefix,
		inverseEdgeCFPrefix: causalDefaultInverseEdgeCFPrefix,
		resultCF:            causalDefaultResultCF,
		resultCQ:            causalDefaultResultCQ,
	}
}

// Init wires the source and parses causal-query options.
func (c *CausalInferenceIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New(causalIteratorErrorPrefix + " requires a non-nil source")
	}
	c.source = source

	qB64 := options[CausalInferenceQuery]
	if qB64 == "" {
		return fmt.Errorf("%s missing option %q", causalIteratorErrorPrefix, CausalInferenceQuery)
	}
	qBytes, err := base64.StdEncoding.DecodeString(qB64)
	if err != nil {
		return fmt.Errorf("%s bad %s: %w", causalIteratorErrorPrefix, CausalInferenceQuery, err)
	}
	c.query, err = unpackFloat32BE(qBytes)
	if err != nil {
		return fmt.Errorf("%s %s: %w", causalIteratorErrorPrefix, CausalInferenceQuery, err)
	}
	if len(c.query) == 0 {
		return fmt.Errorf("%s %s is empty", causalIteratorErrorPrefix, CausalInferenceQuery)
	}

	c.start = options[CausalInferenceStartVertex]
	if c.start == "" {
		return fmt.Errorf("%s missing option %q", causalIteratorErrorPrefix, CausalInferenceStartVertex)
	}

	switch options[CausalInferenceDirection] {
	case "", causalDirectionForward:
		c.direction = causalDirectionForward
	case causalDirectionBackward:
		c.direction = causalDirectionBackward
	default:
		return fmt.Errorf("%s bad %s=%q (want %q or %q)",
			causalIteratorErrorPrefix, CausalInferenceDirection, options[CausalInferenceDirection],
			causalDirectionForward, causalDirectionBackward)
	}

	if s, ok := options[CausalInferenceMaxDepth]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("%s bad %s=%q", causalIteratorErrorPrefix, CausalInferenceMaxDepth, s)
		}
		c.maxDepth = v
	}
	if s, ok := options[CausalInferenceThreshold]; ok && s != "" {
		v, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return fmt.Errorf("%s bad %s=%q", causalIteratorErrorPrefix, CausalInferenceThreshold, s)
		}
		c.threshold = float32(v)
	}
	if s, ok := options[CausalInferenceMaxVertices]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("%s bad %s=%q", causalIteratorErrorPrefix, CausalInferenceMaxVertices, s)
		}
		c.maxVertices = v
	}
	if s, ok := options[CausalInferenceEdgeType]; ok {
		c.edgeType = s
	}
	if s := options[CausalInferenceVertexCF]; s != "" {
		c.vertexCF = s
	}
	if s := options[CausalInferenceEmbeddingCQ]; s != "" {
		c.embeddingCQ = s
	}
	if s := options[CausalInferenceEdgeCFPrefix]; s != "" {
		c.edgeCFPrefix = s
	}
	if s := options[CausalInferenceInverseEdgeCFPrefix]; s != "" {
		c.inverseEdgeCFPrefix = s
	}
	if s := options[CausalInferenceResultCF]; s != "" {
		c.resultCF = s
	}
	if s := options[CausalInferenceResultCQ]; s != "" {
		c.resultCQ = s
	}
	return nil
}

func (c *CausalInferenceIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	c.out = c.out[:0]
	c.outIndex = 0
	c.err = nil

	if err := c.source.Seek(r, columnFamilies, inclusive); err != nil {
		c.err = err
		return err
	}

	records := []Cell{}
	var maxTS int64
	for c.source.HasTop() {
		k := c.source.GetTopKey().Clone()
		v := append([]byte(nil), c.source.GetTopValue()...)
		records = append(records, Cell{Key: k, Value: v})
		if k.Timestamp > maxTS {
			maxTS = k.Timestamp
		}
		if err := c.source.Next(); err != nil {
			c.err = err
			return err
		}
	}

	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Key.Compare(records[j].Key) < 0
	}) // Load-bearing determinism: TestCausalInference_DeterministicAcrossInputOrders pins source-order independence.

	vertices, edges := c.buildGraph(records)
	result := c.trace(vertices, edges)
	value, err := json.Marshal(result)
	if err != nil {
		c.err = err
		return err
	}

	visibility := []byte(nil)
	if vertex, ok := vertices[c.start]; ok {
		visibility = append([]byte(nil), vertex.visibility...)
	}
	resultTS := maxTS + 1 // Load-bearing determinism: TestCausalInference_DerivedTimestamp pins source-derived timestamps.
	c.out = append(c.out, Cell{
		Key: &wire.Key{
			Row:              []byte(c.start),
			ColumnFamily:     []byte(c.resultCF),
			ColumnQualifier:  []byte(c.resultCQ),
			ColumnVisibility: visibility,
			Timestamp:        resultTS,
		},
		Value: value,
	})
	return nil
}

func (c *CausalInferenceIterator) buildGraph(records []Cell) (map[string]causalVertex, map[string][]causalEdge) {
	vertices := map[string]causalVertex{}
	edges := map[string][]causalEdge{}

	for _, record := range records {
		k := record.Key
		row := string(k.Row)
		cf := string(k.ColumnFamily)
		cq := string(k.ColumnQualifier)

		if cf == c.vertexCF && cq == c.embeddingCQ {
			if _, dup := vertices[row]; dup {
				continue // Load-bearing determinism: TestCausalInference_NewestEmbeddingWins pins first key-sorted embedding.
			}
			if emb := parseEmbedding(record.Value); emb != nil {
				vertices[row] = causalVertex{
					embedding:  emb,
					visibility: append([]byte(nil), k.ColumnVisibility...),
				}
			}
		}

		if edgeType, ok := c.matchEdgeCF(cf); ok {
			edges[row] = append(edges[row], causalEdge{
				neighbor:  cq,
				edgeType:  edgeType,
				cf:        cf,
				timestamp: k.Timestamp,
			})
		}
	}
	return vertices, edges
}

func (c *CausalInferenceIterator) matchEdgeCF(cf string) (string, bool) {
	prefix := c.edgeCFPrefix
	if c.direction == causalDirectionBackward {
		prefix = c.inverseEdgeCFPrefix
	}
	if !strings.HasPrefix(cf, prefix) {
		return "", false
	}
	if c.direction == causalDirectionForward && c.inverseEdgeCFPrefix != "" && strings.HasPrefix(cf, c.inverseEdgeCFPrefix) {
		return "", false // Load-bearing directionality: TestCausalInference_ForwardExcludesInversePrefix pins E/EI disambiguation.
	}
	edgeType := strings.TrimPrefix(cf, prefix)
	if c.edgeType != "" && edgeType != c.edgeType {
		return "", false
	}
	return edgeType, true
}

func (c *CausalInferenceIterator) trace(vertices map[string]causalVertex, edges map[string][]causalEdge) causalResult {
	startRole := "cause"
	hopRole := "effect"
	if c.direction == causalDirectionBackward {
		startRole = "effect"
		hopRole = "cause"
	}

	result := causalResult{
		Chain: []causalHop{{
			VertexID: c.start,
			Score:    1,
			Role:     startRole,
		}},
		Direction:      c.direction,
		CausalStrength: 1,
	}

	if len(vertices) > c.maxVertices {
		result.HopCount = len(result.Chain)
		return result // Load-bearing guard: TestCausalInference_MaxVerticesReturnsStartOnly pins cap behavior.
	}

	hidden := causalNormalize(append([]float32(nil), c.query...))
	visited := map[string]struct{}{c.start: {}}
	current := c.start

	for hop := 0; hop < c.maxDepth; hop++ {
		if vertex, ok := vertices[current]; ok && len(vertex.embedding) == len(hidden) {
			alpha := float32(0.85)
			if c.direction == causalDirectionBackward {
				alpha = 0.8 // Load-bearing Java parity: TestCausalInference_BackwardUsesDifferentHiddenAlpha pins asymmetric update.
			}
			emb := causalNormalize(append([]float32(nil), vertex.embedding...))
			for i := range hidden {
				hidden[i] = alpha*hidden[i] + (1-alpha)*emb[i]
			}
			hidden = causalNormalize(hidden)
		}

		candidates := make([]causalEdge, 0, len(edges[current]))
		for _, edge := range edges[current] {
			if _, seen := visited[edge.neighbor]; seen {
				continue // Load-bearing traversal guard: TestCausalInference_SkipsSelfLoopsAndCycles pins visited filtering.
			}
			candidates = append(candidates, edge)
		}
		if len(candidates) == 0 {
			break
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].neighbor != candidates[j].neighbor {
				return candidates[i].neighbor < candidates[j].neighbor
			}
			if candidates[i].edgeType != candidates[j].edgeType {
				return candidates[i].edgeType < candidates[j].edgeType
			}
			if candidates[i].cf != candidates[j].cf {
				return candidates[i].cf < candidates[j].cf
			}
			return candidates[i].timestamp > candidates[j].timestamp
		}) // Load-bearing determinism: TestCausalInference_TieBreaksByNeighbor pins sorted candidate ties.

		bestIndex := -1
		bestScore := float32(-1)
		for i, candidate := range candidates {
			score := c.scoreCandidate(vertices[candidate.neighbor], hidden)
			if score > bestScore {
				bestScore = score
				bestIndex = i
			}
		}
		if bestIndex < 0 || bestScore < c.threshold {
			break
		}

		chosen := candidates[bestIndex]
		result.CausalStrength *= bestScore // Load-bearing Java parity: TestCausalInference_StrengthMultipliesHopScores pins product scoring.
		edgeType := chosen.edgeType
		result.Chain = append(result.Chain, causalHop{
			VertexID: chosen.neighbor,
			EdgeType: &edgeType,
			Score:    bestScore,
			Role:     hopRole,
		})
		visited[chosen.neighbor] = struct{}{}
		current = chosen.neighbor
	}

	result.HopCount = len(result.Chain)
	return result
}

func (c *CausalInferenceIterator) scoreCandidate(vertex causalVertex, hidden []float32) float32 {
	if vertex.embedding != nil && len(vertex.embedding) == len(hidden) {
		return cosineSimilarity(hidden, vertex.embedding)
	}
	return c.threshold + 0.01 // Load-bearing Java parity: TestCausalInference_MissingEmbeddingGetsFallbackScore pins absent-neighbor fallback.
}

type causalVertex struct {
	embedding  []float32
	visibility []byte
}

type causalEdge struct {
	neighbor  string
	edgeType  string
	cf        string
	timestamp int64
}

type causalHop struct {
	VertexID string  `json:"vertex_id"`
	EdgeType *string `json:"edge_type"`
	Score    float32 `json:"score"`
	Role     string  `json:"role"`
}

type causalResult struct {
	Chain          []causalHop `json:"chain"`
	Direction      string      `json:"direction"`
	CausalStrength float32     `json:"causal_strength"`
	HopCount       int         `json:"hop_count"`
}

func causalNormalize(vector []float32) []float32 {
	var norm float32
	for _, v := range vector {
		norm += v * v
	}
	if norm == 0 {
		return vector
	}
	norm = float32(math.Sqrt(float64(norm)))
	for i := range vector {
		vector[i] /= norm
	}
	return vector
}

func (c *CausalInferenceIterator) HasTop() bool { return c.err == nil && c.outIndex < len(c.out) }
func (c *CausalInferenceIterator) GetTopKey() *Key {
	if !c.HasTop() {
		return nil
	}
	return c.out[c.outIndex].Key
}
func (c *CausalInferenceIterator) GetTopValue() []byte {
	if !c.HasTop() {
		return nil
	}
	return c.out[c.outIndex].Value
}
func (c *CausalInferenceIterator) Next() error {
	if c.err != nil {
		return c.err
	}
	if !c.HasTop() {
		return errors.New(causalIteratorErrorPrefix + ".Next called without a top")
	}
	c.outIndex++
	return nil
}

func (c *CausalInferenceIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &CausalInferenceIterator{
		source:              c.source.DeepCopy(env),
		query:               append([]float32(nil), c.query...),
		start:               c.start,
		direction:           c.direction,
		maxDepth:            c.maxDepth,
		threshold:           c.threshold,
		edgeType:            c.edgeType,
		maxVertices:         c.maxVertices,
		vertexCF:            c.vertexCF,
		embeddingCQ:         c.embeddingCQ,
		edgeCFPrefix:        c.edgeCFPrefix,
		inverseEdgeCFPrefix: c.inverseEdgeCFPrefix,
		resultCF:            c.resultCF,
		resultCQ:            c.resultCQ,
	}
}
