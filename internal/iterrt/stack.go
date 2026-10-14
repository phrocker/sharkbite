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

import "fmt"

// IterSpec names one iterator to compose into a stack, plus its
// per-iterator options. The parity harness, the compaction-stack
// composer (Phase C3), and the WAL merge stack (Bet 2) all describe a
// stack as an ordered []IterSpec.
type IterSpec struct {
	// Name is the iterator's registered identifier. See BuildStack for
	// the C1 set.
	Name string `json:"name"`
	// Options is the per-iterator option map handed to Init.
	Options map[string]string `json:"options,omitempty"`
}

// Stack identifiers recognized by BuildStack. The set grows as iterator
// ports land (the design doc's iterator router allowlist).
const (
	// IterVersioning is VersioningIterator — newest-N versions per
	// coordinate. Option "maxVersions" (default 1).
	IterVersioning = "versioning"
	// IterVisibility is VisibilityFilter — drops cells the env's
	// Authorizations cannot satisfy. Active only at ScopeScan.
	IterVisibility = "visibility"
	// IterDeleting is DeletingIterator — applies tombstone suppression.
	// Options "propagateDeletes" (bool) and "behavior" ("process"|"fail")
	// override env-derived defaults; default propagateDeletes is false
	// only at ScopeMajc with FullMajorCompaction, true otherwise.
	IterDeleting = "deleting"
	// IterLatentEdgeDiscovery is the graph_vidx majc iterator that emits
	// bidirectional `link:<otherVertex>` cells for embedding pairs above
	// a similarity threshold, scoped to the row's tessellation cell.
	// Options: similarityThreshold, maxPairsPerCell, maxCellBuffer.
	IterLatentEdgeDiscovery = "latentEdgeDiscovery"
	// IterSemanticEdge is the semantic-edge majc iterator: it reuses the
	// deterministic LatentEdgeDiscovery cosine-threshold emission engine in
	// whole-range mode and defaults edgeCF to the project convention
	// "edge.sem:". Options: similarityThreshold, edgeCF, embeddingCF,
	// embeddingCQ, maxEdgesPerVertex, maxVectors, direction, inverseEdgeCF.
	IterSemanticEdge = "semanticEdge"
	// IterGraphRank is the graph-table majc iterator that runs PageRank over
	// vertex rows and outgoing edge column families, emitting V:_rank cells.
	// Options: dampingFactor, maxIterations, edgeType, maxVertices,
	// convergenceThreshold, vertexCF, edgeCFPrefix, labelCQ, rankCQ.
	IterGraphRank = "graphRank"
	// IterCausalInference is a terminal scan iterator that traces one greedy
	// causal chain from startVertex and emits a single JSON result cell.
	// Options: query.b64, startVertex, direction, maxDepth, threshold,
	// edgeType, maxVertices, vertexCF, embeddingCQ, edgeCFPrefix,
	// inverseEdgeCFPrefix, resultCF, resultCQ.
	IterCausalInference = "causalInference"
	// IterTermIndex is the term-index (keyword) pushdown iterator: it
	// resolves posting rows to primary rows and emits the primary rows'
	// cells. Options: termCount, term.<i>, primaryPrefix, idSource, postingCF.
	// Must be hosted above a re-seekable whole-table source (see
	// TermIndexIterator).
	IterTermIndex = "termIndex"
	// IterVectorKNN is the brute-force vector k-NN pushdown iterator: it
	// scores packed-float32 embedding cells against a query vector and emits
	// the top-k cells, value replaced by a big-endian float32 score. Options:
	// query.b64, topK, embeddingCF, metric, minScore. A terminal ranking
	// iterator — must be hosted at the top of the stack (see
	// VectorKNNIterator).
	IterVectorKNN = "vectorKNN"
	// IterEdgeExpand is the one-hop graph-expansion pushdown iterator: it
	// walks each anchor row's edge cells, resolves embedded neighbor ids to
	// neighbor rows, and emits the neighbor rows' cells. Options: anchorCount,
	// anchor.<i>, edgeCF, edgeField, fieldSep, idIndex, relIndex, relCount,
	// rel.<i>, primaryPrefix, includeAnchors. Must be hosted above a
	// re-seekable whole-table source (see EdgeExpandIterator).
	IterEdgeExpand = "edgeExpand"
	// IterScoreFilter is a terminal ranking pushdown iterator: it scores cells
	// in range and emits the top-k cells with values replaced by big-endian
	// float32 scores. Options: scoreCF, method, query.b64, topK, paramCount,
	// param.<i>, timestampAnchorMs, halfLifeMs.
	IterScoreFilter = "scoreFilter"
	// IterGraphAggregation groups scan cells and emits one deterministic
	// aggregate cell per group. Options: aggregationOp, groupBy, rowPrefixSep,
	// valueCF, valueCQ, resultRow, resultCF.
	IterGraphAggregation = "graphAggregation"
	// IterAnomalyDetect filters rows whose selected numeric value cell lies
	// outside a configured band. Options: valueCF, valueCQ, min, max.
	IterAnomalyDetect = "anomalyDetect"
	// IterVisibilityStamp rewrites each cell's ColumnVisibility to carry a
	// tenant label so many producers can fan into one engine while staying
	// isolated (a scan sees a tenant's cells only with the matching auth).
	// Options: label (required), mode ("and" default | "whenEmpty"). It
	// buffers only the row/cf/cq window it re-sorts, like Accumulo's
	// TransformingIterator. Intended for whole-range compaction passes.
	IterVisibilityStamp = "visibilityStamp"
	// IterAsOf is AsOfIterator — a time-travel filter dropping cells newer
	// than a timestamp ceiling. Option "asOfTimestamp" (int64; <=0 disables).
	// Belongs at the bottom of a scan stack, beneath deleting/versioning.
	IterAsOf = "asOf"
	// IterDocumentIndex is the DataWave-style document retrieval pushdown
	// iterator: it resolves field=value terms against the in-shard field index
	// (fi) and reconstructs each matching document's event cells. Options:
	// shardCount, shard.<i>, termCount, term.<i>.field, term.<i>.value, boolOp.
	// Must be hosted above a re-seekable whole-table source (see
	// DocumentIndexIterator).
	IterDocumentIndex = "documentIndex"
)

// BuildStack composes an iterator stack on top of leaf, in order: specs[0]
// sits directly above leaf, specs[1] above specs[0], and so on. The
// returned iterator is the top of the stack — Seek/Next it directly.
//
// Every iterator in the stack is Init'd against env. leaf is assumed
// already Init'd by the caller (it is a leaf — RFileSource or
// SliceSource — with construction-time inputs).
//
// An empty specs list returns leaf unchanged: "identity compaction"
// (cells pass through untouched), which is the C0 harness behaviour.
func BuildStack(leaf SortedKeyValueIterator, specs []IterSpec, env IteratorEnvironment) (SortedKeyValueIterator, error) {
	if err := ValidateStack(specs, ContextFromScope(env.Scope)); err != nil {
		return nil, err
	}
	cur := leaf
	for i, spec := range specs {
		next, err := newIterator(spec.Name)
		if err != nil {
			return nil, fmt.Errorf("iterrt: stack position %d: %w", i, err)
		}
		if err := next.Init(cur, spec.Options, env); err != nil {
			return nil, fmt.Errorf("iterrt: stack position %d (%s): %w", i, spec.Name, err)
		}
		cur = next
	}
	return cur, nil
}

// newIterator constructs an un-Init'd iterator by name.
func newIterator(name string) (SortedKeyValueIterator, error) {
	reg, ok := registryByName[name]
	if !ok {
		return nil, &UnknownIteratorError{Name: name}
	}
	return reg.new(), nil
}
