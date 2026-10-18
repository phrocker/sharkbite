package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/agentmem"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func collectProjected(t *testing.T, stream embedpb.ShoalEmbed_ScanV3Client) []*embedpb.VectorHit {
	t.Helper()
	var hits []*embedpb.VectorHit
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return hits
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Cells) != 0 {
			t.Fatalf("projected response includes legacy cells: %+v", response)
		}
		hits = append(hits, response.VectorHits...)
	}
}

func TestVectorProjectionWireModesAndAsOf(t *testing.T) {
	h := startTestServe(t)
	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const identity = "test:model:v1:1:normalized"
	if _, err := client.CreateTableV2(ctx, &embedpb.CreateTableRequest{
		Table: "vectors", DefaultEmbedding: "has_embeddings:" + identity,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(ctx, &embedpb.WriteRequest{
		Table: "vectors",
		Mutations: []*embedpb.Mutation{
			{Row: []byte("r1"), Entries: []*embedpb.Entry{
				{ColumnFamily: []byte("vec"), Timestamp: 10, Value: agentmem.PackVector([]float32{1})},
				{ColumnFamily: []byte("vec"), Timestamp: 20, Value: agentmem.PackVector([]float32{-1})},
				{ColumnFamily: []byte("meta"), ColumnVisibility: []byte("A"), Timestamp: 10, Value: []byte("old")},
				{ColumnFamily: []byte("meta"), ColumnVisibility: []byte("A"), Timestamp: 20, Value: []byte("new")},
			}},
			{Row: []byte("r2"), Entries: []*embedpb.Entry{
				{ColumnFamily: []byte("vec"), Timestamp: 10, Value: agentmem.PackVector([]float32{0.5})},
				{ColumnFamily: []byte("meta"), Timestamp: 10, Value: []byte("other")},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := &embedpb.ScanRequest{
		Table: "vectors", AsOf: 15,
		VectorSearch: &embedpb.VectorSearch{
			Query: agentmem.PackVector([]float32{1}), Metric: "dot", TopK: 2,
			EmbeddingCf: []byte("vec"), EmbeddingSpace: identity,
		},
	}
	legacy, err := client.ScanV2(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	legacyResponse, err := legacy.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyResponse.Cells) != 2 || len(legacyResponse.VectorHits) != 0 ||
		string(legacyResponse.Cells[0].Row) != "r1" {
		t.Fatalf("legacy response changed or as_of ignored: %+v", legacyResponse)
	}

	request.VectorSearch.Projection = &embedpb.VectorProjection{
		Mode: embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW,
	}
	old, err := client.ScanV2(ctx, request)
	if err == nil {
		_, err = old.Recv()
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ScanV2 with projection error = %v", err)
	}
	stream, err := client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits := collectProjected(t, stream)
	if len(hits) != 2 || string(hits[0].ScoredCell.Row) != "r1" || hits[0].EmbeddingSpace != identity {
		t.Fatalf("projected hits = %+v", hits)
	}
	if score := math.Float32frombits(binary.BigEndian.Uint32(hits[0].ScoredCell.Value)); score != 1 {
		t.Fatalf("score = %v, want 1", score)
	}
	if len(hits[0].RowCells) != 2 {
		t.Fatalf("as-of row cells = %+v, want vec and old meta", hits[0].RowCells)
	}
	for _, cell := range hits[0].RowCells {
		if cell.Timestamp > 15 || string(cell.Value) == "new" {
			t.Fatalf("future cell leaked into as-of projection: %+v", cell)
		}
	}
	request.VectorSearch.Projection = &embedpb.VectorProjection{
		Mode:           embedpb.VectorProjectionMode_VECTOR_PROJECTION_COLUMN_FAMILIES,
		ColumnFamilies: [][]byte{[]byte("meta")},
	}
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits = collectProjected(t, stream)
	if len(hits) != 2 || len(hits[0].RowCells) != 1 ||
		string(hits[0].RowCells[0].ColumnFamily) != "meta" ||
		string(hits[0].RowCells[0].ColumnVisibility) != "A" ||
		string(hits[0].RowCells[0].Value) != "old" {
		t.Fatalf("family projection = %+v", hits)
	}
	request.VectorSearch.Projection = &embedpb.VectorProjection{}
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits = collectProjected(t, stream)
	if len(hits) != 2 || len(hits[0].RowCells) != 0 {
		t.Fatalf("scored-cell-only projection = %+v", hits)
	}
	request.RowPrefix = "r1"
	request.VectorSearch.Projection.Mode = embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits = collectProjected(t, stream)
	if len(hits) != 1 || string(hits[0].ScoredCell.Row) != "r1" {
		t.Fatalf("projection escaped requested row prefix: %+v", hits)
	}
}

func TestVectorProjectionSplitTableAndDeletedEmbedding(t *testing.T) {
	h := startTestServe(t)
	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const identity = "test:model:v1:1:normalized"
	if _, err := client.CreateTableV2(ctx, &embedpb.CreateTableRequest{
		Table: "split_vectors", Splits: []string{"m"},
		DefaultEmbedding: "has_embeddings:" + identity,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(ctx, &embedpb.WriteRequest{
		Table: "split_vectors", Mutations: []*embedpb.Mutation{
			{Row: []byte("a"), Entries: []*embedpb.Entry{
				{ColumnFamily: []byte("vec"), Timestamp: 10, Value: agentmem.PackVector([]float32{1})},
				{ColumnFamily: []byte("vec"), Timestamp: 30, Delete: true},
			}},
			{Row: []byte("z"), Entries: []*embedpb.Entry{
				{ColumnFamily: []byte("vec"), Timestamp: 10, Value: agentmem.PackVector([]float32{0.5})},
				{ColumnFamily: []byte("meta"), Timestamp: 10, Value: []byte("right tablet")},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	request := &embedpb.ScanRequest{Table: "split_vectors", VectorSearch: &embedpb.VectorSearch{
		Query: agentmem.PackVector([]float32{1}), Metric: "dot", TopK: 2,
		EmbeddingCf: []byte("vec"), EmbeddingSpace: identity,
		Projection: &embedpb.VectorProjection{Mode: embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW},
	}}
	stream, err := client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits := collectProjected(t, stream)
	if len(hits) != 1 || string(hits[0].ScoredCell.Row) != "z" || len(hits[0].RowCells) != 2 {
		t.Fatalf("split/deleted projection = %+v, want live z row", hits)
	}
	request.AsOf = 15
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits = collectProjected(t, stream)
	if len(hits) != 2 || string(hits[0].ScoredCell.Row) != "a" || len(hits[0].RowCells) != 1 {
		t.Fatalf("as-of projection = %+v, want pre-delete a row", hits)
	}
}

func TestVectorProjectionDuplicateRowsAndBounds(t *testing.T) {
	h := startTestServe(t)
	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const identity = "test:model:v1:1:normalized"
	if _, err := client.CreateTableV2(ctx, &embedpb.CreateTableRequest{
		Table: "vectors", DefaultEmbedding: "has_embeddings:" + identity,
	}); err != nil {
		t.Fatal(err)
	}
	entries := []*embedpb.Entry{
		{ColumnFamily: []byte("vec"), ColumnQualifier: []byte("a"), Timestamp: 10, Value: agentmem.PackVector([]float32{1})},
		{ColumnFamily: []byte("vec"), ColumnQualifier: []byte("b"), Timestamp: 10, Value: agentmem.PackVector([]float32{1})},
	}
	for i := 0; i < maxProjectedCellsPerRow; i++ {
		entries = append(entries, &embedpb.Entry{
			ColumnFamily: []byte("meta"), ColumnQualifier: []byte(fmt.Sprintf("%03d", i)),
			Timestamp: 10, Value: []byte("v"),
		})
	}
	if _, err := client.Write(ctx, &embedpb.WriteRequest{
		Table: "vectors", Mutations: []*embedpb.Mutation{{Row: []byte("row"), Entries: entries}},
	}); err != nil {
		t.Fatal(err)
	}
	request := &embedpb.ScanRequest{
		Table: "vectors",
		VectorSearch: &embedpb.VectorSearch{
			Query: agentmem.PackVector([]float32{1}), Metric: "dot", TopK: 2,
			EmbeddingCf: []byte("vec"), EmbeddingSpace: identity,
			Projection: &embedpb.VectorProjection{
				Mode:           embedpb.VectorProjectionMode_VECTOR_PROJECTION_COLUMN_FAMILIES,
				ColumnFamilies: [][]byte{[]byte("vec")},
			},
		},
	}
	stream, err := client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	hits := collectProjected(t, stream)
	if len(hits) != 2 || string(hits[0].ScoredCell.Row) != "row" ||
		string(hits[1].ScoredCell.Row) != "row" ||
		string(hits[0].ScoredCell.ColumnQualifier) != "a" ||
		string(hits[1].ScoredCell.ColumnQualifier) != "b" ||
		len(hits[0].RowCells) != 2 || len(hits[1].RowCells) != 2 {
		t.Fatalf("duplicate-row hit order/projection = %+v", hits)
	}
	request.VectorSearch.Projection = &embedpb.VectorProjection{
		Mode: embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW,
	}
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := stream.Recv(); status.Code(err) != codes.ResourceExhausted || response != nil {
		t.Fatalf("oversized projection first Recv = (%+v, %v), want ResourceExhausted before data", response, err)
	}
	request.VectorSearch.Projection = &embedpb.VectorProjection{
		Mode:           embedpb.VectorProjectionMode_VECTOR_PROJECTION_COLUMN_FAMILIES,
		ColumnFamilies: [][]byte{[]byte("meta"), []byte("meta")},
	}
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("duplicate family error = %v, want InvalidArgument", err)
	}
	if _, err := client.Write(ctx, &embedpb.WriteRequest{
		Table: "vectors", Mutations: []*embedpb.Mutation{{
			Row: []byte("big"), Entries: []*embedpb.Entry{
				{ColumnFamily: []byte("vec"), Timestamp: 10, Value: agentmem.PackVector([]float32{2})},
				{ColumnFamily: []byte("meta"), Timestamp: 10, Value: bytes.Repeat([]byte("x"), maxProjectedBytesPerRow+1)},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	request.RowPrefix = "big"
	request.VectorSearch.Projection = &embedpb.VectorProjection{
		Mode: embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW,
	}
	stream, err = client.ScanV3(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := stream.Recv(); status.Code(err) != codes.ResourceExhausted || response != nil {
		t.Fatalf("oversized cell first Recv = (%+v, %v), want ResourceExhausted", response, err)
	}
}
