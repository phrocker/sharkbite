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

// Command shoal-ivf-train builds an out-of-band IVF-PQ index for a shoal
// graph table. It scans full-precision vectors from the source table, trains
// coarse centroids and a PQ codebook, then writes the coded vectors and
// codebook blobs back to <table>_ivf and <table>_ann_config respectively.
//
// Output is byte-compatible with veculo's IvfCentroids.java / VectorPQ.java
// so that a local shoal index can be imported into the veculo platform without
// re-ingestion.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/agentmem"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9876", "shoal-embed gRPC address")
	table := flag.String("table", agentmem.DefaultTable, "source graph table")
	vecCF := flag.String("vec-cf", graphschema.VectorCFName, "column family holding full-precision vectors")
	m := flag.Int("m", 8, "number of PQ subspaces")
	ks := flag.Int("ks", 256, "number of centroids per subspace (1..256)")
	nlist := flag.Int("nlist", 64, "target number of coarse IVF clusters")
	maxIter := flag.Int("max-iter", 25, "k-means iterations")
	seed := flag.Int64("seed", 42, "RNG seed for deterministic training")
	sample := flag.Int("sample", 0, "max training vectors (0=all)")
	version := flag.Int("version", 1, "codebook version tag")
	embeddingSpace := flag.String(
		"embedding-space", "",
		"required stable identity of the source vector space",
	)
	ts := flag.Int64("ts", 0, "timestamp for all written cells (0 = let server assign)")
	flag.Parse()
	if err := embeddingspace.ValidateQueryStates(
		"train agent-memory IVF index", strings.TrimSpace(*embeddingSpace)); err != nil {
		fatal("embedding-space: %v", err)
	}

	ctx := context.Background()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fatal("dial: %v", err)
	}
	defer conn.Close()

	store := agentmem.NewGRPCStore(conn)

	// ── 1. Scan source vectors ───────────────────────────────────────────────

	cells, err := store.Scan(ctx, *table, &embedpb.ScanRequest{
		RowPrefix: graphschema.EventRowPrefix,
	})
	if err != nil {
		fatal("scan: %v", err)
	}

	type entry struct {
		vertexID string
		vec      []float32
	}
	var records []entry
	var dim int

	for _, cell := range cells {
		if string(cell.ColumnFamily) != *vecCF {
			continue
		}
		vec, err := unpackVecBE(cell.Value)
		if err != nil {
			continue // skip malformed cells
		}
		if len(vec) == 0 {
			continue
		}
		if dim == 0 {
			dim = len(vec)
		}
		if len(vec) != dim {
			fatal("inconsistent vector dim: got %d, expected %d (row %s)", len(vec), dim, cell.Row)
		}
		records = append(records, entry{
			vertexID: string(cell.Row),
			vec:      vec,
		})
	}

	n := len(records)
	if n == 0 {
		fatal("no vectors found in table %q with column family %q", *table, *vecCF)
	}
	if _, err := store.Scan(ctx, *table, &embedpb.ScanRequest{
		Limit: 1,
		VectorSearch: &embedpb.VectorSearch{
			Query:          agentmem.PackVector(records[0].vec),
			TopK:           1,
			EmbeddingCf:    []byte(*vecCF),
			EmbeddingSpace: strings.TrimSpace(*embeddingSpace),
		},
	}); err != nil {
		fatal("verify source embedding space: %v", err)
	}
	fmt.Printf("found %d vectors (dim=%d)\n", n, dim)

	// ── 2. Deterministic training sample ────────────────────────────────────

	trainRecords := records
	if *sample > 0 && *sample < n {
		rng := rand.New(rand.NewSource(*seed))
		perm := rng.Perm(n)
		sub := make([]entry, *sample)
		for i := 0; i < *sample; i++ {
			sub[i] = records[perm[i]]
		}
		trainRecords = sub
	}

	trainVecs := make([][]float32, len(trainRecords))
	for i, r := range trainRecords {
		trainVecs[i] = r.vec
	}

	if *version <= 0 || int64(*version) > int64(^uint32(0)>>1) {
		fatal("version must be in [1, %d]", uint32(^uint32(0)>>1))
	}
	cbVersion := int32(*version)

	ivfTable := ivfpq.IvfTableName(*table)
	cfgTable := ivfpq.ConfigTableName(*table)
	_ = store.CreateTable(ctx, ivfTable, nil)
	_ = store.CreateTable(ctx, cfgTable, nil)
	reservation, err := store.WriteWithResults(ctx, cfgTable, []*embedpb.Mutation{{
		Row: []byte(ivfpq.VersionReservationRow(cbVersion)),
		Entries: []*embedpb.Entry{{
			ColumnFamily:    []byte(ivfpq.ConfigColFam),
			ColumnQualifier: []byte(ivfpq.ConfigQual),
			Timestamp:       *ts,
			Value:           []byte("reserved"),
		}},
		Conditions: []*embedpb.Condition{{
			ColumnFamily:    []byte(ivfpq.ConfigColFam),
			ColumnQualifier: []byte(ivfpq.ConfigQual),
			Predicate:       &embedpb.Condition_Absent{Absent: true},
		}},
	}})
	if err != nil {
		fatal("reserve codebook version %d: %v", cbVersion, err)
	}
	if len(reservation) != 1 ||
		reservation[0].Status != embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED {
		fatal("codebook version %d already exists; choose a new version", cbVersion)
	}

	// Clamp nlist so we never ask for more clusters than samples.
	k := *nlist
	if k > len(trainVecs) {
		k = len(trainVecs)
	}

	fmt.Printf("training coarse centroids: k=%d maxIter=%d seed=%d\n", k, *maxIter, *seed)
	centroids, err := ivfpq.TrainCentroids(trainVecs, k, *maxIter, *seed, cbVersion)
	if err != nil {
		fatal("TrainCentroids: %v", err)
	}

	fmt.Printf("training PQ codebook: M=%d Ks=%d maxIter=%d seed=%d\n", *m, *ks, *maxIter, *seed)
	pq, err := ivfpq.TrainPQ(trainVecs, *m, *ks, *maxIter, *seed, cbVersion)
	if err != nil {
		fatal("TrainPQ: %v", err)
	}

	// ── 3. Create IVF + config tables ────────────────────────────────────────

	// ── 4. Write coded vectors ───────────────────────────────────────────────

	versionBytes := []byte(strconv.Itoa(*version))
	const batchSize = 1000

	batch := make([]*embedpb.Mutation, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.Write(ctx, ivfTable, batch); err != nil {
			fatal("write coded vectors: %v", err)
		}
		batch = batch[:0]
	}

	for _, r := range records {
		norm := unitNorm32(r.vec)
		clusterID := centroids.Assign(norm)
		code, err := pq.Encode(r.vec)
		if err != nil {
			fatal("Encode %s: %v", r.vertexID, err)
		}
		rowKey := []byte(ivfpq.RowKey(clusterID, r.vertexID))
		mut := &embedpb.Mutation{
			Row: rowKey,
			Entries: []*embedpb.Entry{
				{
					ColumnFamily:    []byte(ivfpq.ColFam),
					ColumnQualifier: []byte(ivfpq.QualPQCode),
					Timestamp:       *ts,
					Value:           code,
				},
				{
					ColumnFamily:    []byte(ivfpq.ColFam),
					ColumnQualifier: []byte(ivfpq.QualCodebookVersion),
					Timestamp:       *ts,
					Value:           versionBytes,
				},
			},
		}
		batch = append(batch, mut)
		if len(batch) >= batchSize {
			flush()
		}
	}
	flush()
	fmt.Printf("wrote %d coded vectors to %s\n", n, ivfTable)

	// ── 5. Write codebook blobs to config table ──────────────────────────────

	pqBlob, err := pq.Bytes()
	if err != nil {
		fatal("pq.Bytes: %v", err)
	}
	centroidsBlob, err := centroids.Bytes()
	if err != nil {
		fatal("centroids.Bytes: %v", err)
	}

	cfgMuts := []*embedpb.Mutation{
		cfgMut(ivfpq.PQRow(cbVersion), ivfpq.ConfigColFam, ivfpq.ConfigQual, pqBlob, *ts),
		cfgMut(ivfpq.CentroidsRow(cbVersion), ivfpq.ConfigColFam, ivfpq.ConfigQual, centroidsBlob, *ts),
		cfgMut(ivfpq.EmbeddingSpaceRow(cbVersion), ivfpq.ConfigColFam, ivfpq.ConfigQual, []byte(strings.TrimSpace(*embeddingSpace)), *ts),
		cfgMut(ivfpq.ConfigRowActiveVersion, ivfpq.ConfigColFam, ivfpq.ConfigQual, []byte(strconv.Itoa(*version)), *ts),
		cfgMut(ivfpq.ConfigRowLastTrainedRows, ivfpq.ConfigColFam, ivfpq.ConfigQual, []byte(strconv.Itoa(n)), *ts),
	}
	if err := store.Write(ctx, cfgTable, cfgMuts); err != nil {
		fatal("write config: %v", err)
	}
	fmt.Printf("wrote codebook blobs to %s\n", cfgTable)

	// ── 6. Flush ─────────────────────────────────────────────────────────────

	_ = store.Flush(ctx, ivfTable)
	_ = store.Flush(ctx, cfgTable)

	// ── 7. Summary ───────────────────────────────────────────────────────────

	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("vectors trained : %d\n", n)
	fmt.Printf("dim             : %d\n", dim)
	fmt.Printf("M (subspaces)   : %d\n", pq.M())
	fmt.Printf("Ks (cents/sub)  : %d\n", pq.Ks())
	fmt.Printf("nlist (clusters): %d\n", k)
	fmt.Printf("version         : %d\n", *version)
	fmt.Printf("ivf table       : %s\n", ivfTable)
	fmt.Printf("config table    : %s\n", cfgTable)
}

// cfgMut builds a Mutation for a single config-table cell.
func cfgMut(row, cf, cq string, value []byte, ts int64) *embedpb.Mutation {
	return &embedpb.Mutation{
		Row: []byte(row),
		Entries: []*embedpb.Entry{
			{
				ColumnFamily:    []byte(cf),
				ColumnQualifier: []byte(cq),
				Timestamp:       ts,
				Value:           value,
			},
		},
	}
}

// unpackVecBE decodes a big-endian packed float32 slice.
func unpackVecBE(raw []byte) ([]float32, error) {
	if len(raw)%4 != 0 {
		return nil, fmt.Errorf("vector length %d not a multiple of 4", len(raw))
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.BigEndian.Uint32(raw[i*4:]))
	}
	return out, nil
}

// unitNorm32 returns a unit-normalised copy of v.
func unitNorm32(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	var sum float64
	for _, x := range out {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return out
	}
	n := float32(math.Sqrt(sum))
	for i := range out {
		out[i] /= n
	}
	return out
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shoal-ivf-train: "+format+"\n", args...)
	os.Exit(1)
}
