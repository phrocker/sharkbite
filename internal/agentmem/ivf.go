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

package agentmem

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
)

// IvfIndex is a query-side handle over a trained IVF-PQ index produced by
// cmd/shoal-ivf-train. It loads the active PQ codebook and coarse centroids
// from the <graphTable>_ann_config table and answers approximate
// nearest-neighbor searches: it probes the nprobe nearest clusters of
// <graphTable>_ivf and ADC-scores their PQ codes against the query. This is the
// fast, memory-light alternative to the brute-force VectorSearch path that
// streams and rescans every full-precision vec: cell.
type IvfIndex struct {
	store     EmbedStore
	ivfTable  string
	pq        *ivfpq.VectorPQ
	centroids *ivfpq.Centroids
	version   int32
	space     string
}

// IvfResult is one approximate-nearest-neighbor hit. Row is the original source
// vertex id (e.g. "evt:01H..."); Score is the approximate inner product, where
// larger is better.
type IvfResult struct {
	Row   string
	Score float32
}

// Version reports the codebook version the index was loaded against.
func (ix *IvfIndex) Version() int32 { return ix.version }

// EmbeddingSpace reports the identity persisted with this codebook.
func (ix *IvfIndex) EmbeddingSpace() string { return ix.space }

// LoadIvfIndex reads the active codebook and centroids for graphTable's IVF-PQ
// index from the <graphTable>_ann_config table. It returns an error if no index
// has been trained (the active-version pointer is missing).
func LoadIvfIndex(ctx context.Context, store EmbedStore, graphTable string) (*IvfIndex, error) {
	if store == nil {
		return nil, fmt.Errorf("agentmem: LoadIvfIndex requires a store")
	}
	cfgTable := ivfpq.ConfigTableName(graphTable)

	versionBlob, err := readConfigCell(ctx, store, cfgTable, ivfpq.ConfigRowActiveVersion)
	if err != nil {
		return nil, fmt.Errorf("agentmem: read active version: %w", err)
	}
	if versionBlob == nil {
		return nil, fmt.Errorf("agentmem: no trained IVF-PQ index for %q (missing %s in %s)",
			graphTable, ivfpq.ConfigRowActiveVersion, cfgTable)
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(versionBlob)))
	if err != nil {
		return nil, fmt.Errorf("agentmem: bad active version %q: %w", versionBlob, err)
	}
	version := int32(v)
	spaceBlob, err := readConfigCell(
		ctx, store, cfgTable, ivfpq.EmbeddingSpaceRow(version))
	if err != nil {
		return nil, fmt.Errorf("agentmem: read embedding space v%d: %w", v, err)
	}
	if spaceBlob == nil {
		return nil, fmt.Errorf(
			"%w: legacy IVF-PQ codebook v%d has no embedding-space identity",
			embeddingspace.ErrQueryMetadataMissing, v)
	}
	space := strings.TrimSpace(string(spaceBlob))
	if err := embeddingspace.ValidateQueryStates(
		"load agent-memory IVF index", space); err != nil {
		return nil, err
	}

	pqBlob, err := readConfigCell(ctx, store, cfgTable, ivfpq.PQRow(version))
	if err != nil {
		return nil, fmt.Errorf("agentmem: read PQ codebook v%d: %w", v, err)
	}
	if pqBlob == nil {
		return nil, fmt.Errorf("agentmem: missing PQ codebook %s in %s", ivfpq.PQRow(version), cfgTable)
	}
	pq, err := ivfpq.FromBytes(pqBlob)
	if err != nil {
		return nil, fmt.Errorf("agentmem: parse PQ codebook v%d: %w", v, err)
	}

	centBlob, err := readConfigCell(ctx, store, cfgTable, ivfpq.CentroidsRow(version))
	if err != nil {
		return nil, fmt.Errorf("agentmem: read centroids v%d: %w", v, err)
	}
	if centBlob == nil {
		return nil, fmt.Errorf("agentmem: missing centroids %s in %s", ivfpq.CentroidsRow(version), cfgTable)
	}
	cent, err := ivfpq.CentroidsFromBytes(centBlob)
	if err != nil {
		return nil, fmt.Errorf("agentmem: parse centroids v%d: %w", v, err)
	}

	return &IvfIndex{
		store:     store,
		ivfTable:  ivfpq.IvfTableName(graphTable),
		pq:        pq,
		centroids: cent,
		version:   version,
		space:     space,
	}, nil
}

// LoadIvfIndexInSpace loads an index and verifies it was trained in expected.
func LoadIvfIndexInSpace(
	ctx context.Context,
	store EmbedStore,
	graphTable string,
	expected string,
) (*IvfIndex, error) {
	if err := embeddingspace.ValidateQueryStates(
		"load agent-memory IVF index", expected); err != nil {
		return nil, err
	}
	index, err := LoadIvfIndex(ctx, store, graphTable)
	if err != nil {
		return nil, err
	}
	if err := embeddingspace.EnsureSameIdentity(
		"load agent-memory IVF index", index.space, expected); err != nil {
		return nil, err
	}
	return index, nil
}

// Search returns the approximate topK nearest source rows to query. It probes
// the nprobe coarse clusters whose centroids are closest to the (unit-
// normalized) query, ADC-scores every PQ code found in those clusters, and
// returns the best topK by descending score with ties broken by ascending row
// for determinism. topK<=0 defaults to 10; nprobe<=0 defaults to 1.
func (ix *IvfIndex) Search(ctx context.Context, query []float32, topK, nprobe int) ([]IvfResult, error) {
	return nil, embeddingspace.ErrQueryIdentityRequired
}

// SearchInSpace searches only after verifying the supplied raw vector's space.
func (ix *IvfIndex) SearchInSpace(
	ctx context.Context,
	query []float32,
	embeddingSpace string,
	topK, nprobe int,
) ([]IvfResult, error) {
	if err := embeddingspace.EnsureSameIdentity(
		"search agent-memory IVF index", ix.space, embeddingSpace); err != nil {
		return nil, err
	}
	if topK <= 0 {
		topK = 10
	}
	if nprobe <= 0 {
		nprobe = 1
	}
	norm := append([]float32(nil), query...)
	normalize(norm)
	clusters := ix.centroids.NProbe(norm, nprobe)

	ipTable, err := ix.pq.InnerProductTable(query)
	if err != nil {
		return nil, fmt.Errorf("agentmem: build IVF inner-product table: %w", err)
	}

	type scored struct {
		row   string
		score float32
	}
	var hits []scored
	seen := map[string]bool{}
	for _, c := range clusters {
		cells, err := ix.store.Scan(ctx, ix.ivfTable, &embedpb.ScanRequest{RowPrefix: ivfpq.ClusterPrefix(c)})
		if err != nil {
			return nil, fmt.Errorf("agentmem: scan IVF cluster %d: %w", c, err)
		}
		type posting struct {
			code       []byte
			version    int32
			hasVersion bool
		}
		postings := make(map[string]*posting)
		for _, cell := range cells {
			if string(cell.ColumnFamily) != ivfpq.ColFam {
				continue
			}
			row := string(cell.Row)
			entry := postings[row]
			if entry == nil {
				entry = &posting{}
				postings[row] = entry
			}
			switch string(cell.ColumnQualifier) {
			case ivfpq.QualPQCode:
				entry.code = append([]byte(nil), cell.Value...)
			case ivfpq.QualCodebookVersion:
				value, err := strconv.Atoi(strings.TrimSpace(string(cell.Value)))
				if err != nil {
					continue
				}
				entry.version = int32(value)
				entry.hasVersion = true
			}
		}
		rows := make([]string, 0, len(postings))
		for row := range postings {
			rows = append(rows, row)
		}
		sort.Strings(rows)
		for _, row := range rows {
			entry := postings[row]
			if !entry.hasVersion ||
				entry.version != ix.version ||
				len(entry.code) == 0 {
				continue
			}
			vid := ivfpq.ExtractVertexID(row)
			if seen[vid] {
				continue
			}
			seen[vid] = true
			hits = append(hits, scored{
				row: vid, score: ix.pq.Dot(entry.code, ipTable),
			})
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].row < hits[j].row
		}
		return hits[i].score > hits[j].score
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	out := make([]IvfResult, len(hits))
	for i, h := range hits {
		out[i] = IvfResult{Row: h.row, Score: h.score}
	}
	return out, nil
}

// Add incrementally indexes one full-precision vector into the IVF table so it
// becomes searchable immediately, with no full k-means retrain. It assigns vec
// to its nearest coarse centroid (the same spherical, unit-normalized
// assignment training uses), PQ-encodes the raw vector against the active
// codebook, and writes the posting — RowKey(cluster, vertexID) carrying the
// _pq code and _centroidVersion — exactly as cmd/shoal-ivf-train does.
//
// The code is encoded with the codebook this index was loaded against; postings
// added between trainings therefore stay consistent with this index's own
// query path. The next full training re-encodes every vector authoritatively.
func (ix *IvfIndex) Add(ctx context.Context, vertexID string, vec []float32) error {
	return embeddingspace.ErrQueryIdentityRequired
}

// AddInSpace incrementally indexes a vector only in the codebook's space.
func (ix *IvfIndex) AddInSpace(
	ctx context.Context,
	vertexID string,
	vec []float32,
	embeddingSpace string,
) error {
	if err := embeddingspace.EnsureSameIdentity(
		"update agent-memory IVF index", ix.space, embeddingSpace); err != nil {
		return err
	}
	if vertexID == "" {
		return fmt.Errorf("agentmem: IvfIndex.Add: empty vertexID")
	}
	if len(vec) != ix.centroids.Dim() {
		return fmt.Errorf("agentmem: IvfIndex.Add: vec dim %d != index dim %d", len(vec), ix.centroids.Dim())
	}
	norm := append([]float32(nil), vec...)
	normalize(norm)
	clusterID := ix.centroids.Assign(norm)
	code, err := ix.pq.Encode(vec)
	if err != nil {
		return fmt.Errorf("agentmem: IvfIndex.Add: encode %s: %w", vertexID, err)
	}
	mut := &embedpb.Mutation{
		Row: []byte(ivfpq.RowKey(clusterID, vertexID)),
		Entries: []*embedpb.Entry{
			{ColumnFamily: []byte(ivfpq.ColFam), ColumnQualifier: []byte(ivfpq.QualPQCode), Value: code},
			{ColumnFamily: []byte(ivfpq.ColFam), ColumnQualifier: []byte(ivfpq.QualCodebookVersion), Value: []byte(strconv.Itoa(int(ix.version)))},
		},
	}
	return ix.store.Write(ctx, ix.ivfTable, []*embedpb.Mutation{mut})
}

// readConfigCell returns the value of the (ConfigColFam, ConfigQual) cell at
// the given config-table row, or nil when the row/cell is absent.
func readConfigCell(ctx context.Context, store EmbedStore, table, row string) ([]byte, error) {
	cells, err := store.Scan(ctx, table, &embedpb.ScanRequest{
		StartRow:       []byte(row),
		StartInclusive: true,
		EndRow:         []byte(row),
		EndInclusive:   true,
	})
	if err != nil {
		return nil, err
	}
	for _, c := range cells {
		if string(c.ColumnFamily) == ivfpq.ConfigColFam && string(c.ColumnQualifier) == ivfpq.ConfigQual {
			return c.Value, nil
		}
	}
	return nil, nil
}
