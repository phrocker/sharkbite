// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package graphtable

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/schemapb"
)

// MaxVectorQueryBytes bounds a packed float32 query. MaxVectorCandidates
// bounds the cells fetched before the coordinator keeps the global top k.
const (
	MaxVectorQueryBytes = 16 << 10
	MaxVectorCandidates = 8192
)

type VectorRequest struct {
	Family string
	Query  []byte
	TopK   int
}

type VectorHit struct {
	NodeID string  `json:"node_id"`
	Score  float32 `json:"score"`
}

type scoredCell struct {
	cell  *embedpb.Cell
	score float32
}

// Vector merges complete per-shard, per-namespace top-k results. A shard
// error refuses the entire ranking. The current Scan protocol has no caller
// authorizations for a vector pushdown, so CV-enabled tables are refused
// until the store can rank only cells the caller may see.
func (r *Reader) Vector(ctx context.Context, req VectorRequest) ([]VectorHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.schema.GetCellVisibility() {
		return nil, fmt.Errorf("%w: vector search on a cell-visibility table needs an authorized pushdown", ErrInvalid)
	}
	k, err := bounded(req.TopK, 10, MaxNodes, "top_k")
	if err != nil {
		return nil, err
	}
	if len(req.Query) == 0 || len(req.Query) > MaxVectorQueryBytes || len(req.Query)%4 != 0 {
		return nil, fmt.Errorf("%w: query must be packed float32 within %d bytes", ErrInvalid, MaxVectorQueryBytes)
	}
	for i := 0; i < len(req.Query); i += 4 {
		value := math.Float32frombits(binary.BigEndian.Uint32(req.Query[i:]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: query has a non-finite component", ErrInvalid)
		}
	}
	var family *schemapb.VectorSchema
	for _, candidate := range r.schema.GetVectors() {
		if candidate.GetName() == req.Family {
			family = candidate
			break
		}
	}
	if family == nil {
		return nil, fmt.Errorf("%w: unknown vector family %q", ErrInvalid, req.Family)
	}
	shards := []Shard{{Name: r.Table(), Store: r.store}}
	if sharded, ok := r.store.(*Sharded); ok {
		shards = sharded.Shards()
	}
	if len(shards) == 0 || len(r.namespaces) > MaxVectorCandidates/len(shards)/k {
		return nil, fmt.Errorf("%w: vector search exceeds %d candidate cells", ErrInvalid, MaxVectorCandidates)
	}
	var candidates []scoredCell
	for _, shard := range shards {
		for _, ns := range r.namespaces {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			start, end, ok := vectorRange(ns.spec.GetRowPrefix(), shard)
			if !ok {
				continue
			}
			scan := &embedpb.ScanRequest{Table: r.Table(), StartRow: start, StartInclusive: true, EndRow: end, Limit: int32(k),
				VectorSearch: &embedpb.VectorSearch{Query: req.Query, TopK: int32(k), EmbeddingCf: []byte(family.GetEmbeddingCf()), EmbeddingSpace: family.GetEmbeddingSpace(), Metric: family.GetMetric()}}
			cells, err := shard.Store.Scan(ctx, scan)
			if err != nil {
				return nil, fmt.Errorf("graphtable: vector shard %s: %w", shard.Name, err)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(cells) > k {
				return nil, fmt.Errorf("graphtable: vector shard %s exceeded top_k", shard.Name)
			}
			for _, cell := range cells {
				if cell == nil || !bytes.HasPrefix(cell.GetRow(), []byte(ns.spec.GetRowPrefix())) || bytes.Compare(cell.GetRow(), start) < 0 || (len(end) > 0 && bytes.Compare(cell.GetRow(), end) >= 0) || !bytes.Equal(cell.GetColumnFamily(), []byte(family.GetEmbeddingCf())) || len(cell.GetValue()) != 4 {
					return nil, fmt.Errorf("graphtable: vector shard %s returned an invalid scored cell", shard.Name)
				}
				if err := r.validateRow("vector hit", string(cell.GetRow())); err != nil {
					return nil, fmt.Errorf("graphtable: vector shard %s returned an invalid scored row", shard.Name)
				}
				score := math.Float32frombits(binary.BigEndian.Uint32(cell.GetValue()))
				if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
					return nil, fmt.Errorf("graphtable: vector shard %s returned a non-finite score", shard.Name)
				}
				candidates = append(candidates, scoredCell{cell: cell, score: score})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return compareCells(candidates[i].cell, candidates[j].cell) < 0
	})
	if len(candidates) > k {
		candidates = candidates[:k]
	}
	hits := make([]VectorHit, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, VectorHit{NodeID: string(candidate.cell.GetRow()), Score: candidate.score})
	}
	return hits, nil
}

func vectorRange(prefix string, shard Shard) (start, end []byte, ok bool) {
	start, end = []byte(prefix), prefixEnd([]byte(prefix))
	if len(shard.Start) > 0 && bytes.Compare(shard.Start, start) > 0 {
		start = shard.Start
	}
	if len(shard.End) > 0 && (len(end) == 0 || bytes.Compare(shard.End, end) < 0) {
		end = shard.End
	}
	return start, end, len(end) == 0 || bytes.Compare(start, end) < 0
}
