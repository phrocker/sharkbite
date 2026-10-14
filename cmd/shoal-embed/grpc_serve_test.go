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

package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEmbedServer_CreateTableDefaultsAndStatus(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	srv := newEmbedServer(eng)
	resp, err := srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{Table: "graph"})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if resp.Workload != embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL {
		t.Fatalf("CreateTable workload = %v, want %v", resp.Workload, embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL)
	}
	if resp.FileFormat != embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE {
		t.Fatalf("CreateTable file_format = %v, want %v", resp.FileFormat, embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE)
	}

	st, err := srv.Status(context.Background(), &embedpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.Tables) != 1 || st.Tables[0] != "graph" {
		t.Fatalf("Status tables = %v, want [graph]", st.Tables)
	}
	if len(st.TableStatuses) != 1 {
		t.Fatalf("Status table_statuses len = %d, want 1", len(st.TableStatuses))
	}
	got := st.TableStatuses[0]
	if got.Table != "graph" || got.Tablets != 1 || got.ImmutableFiles != 0 {
		t.Fatalf("Status table_status = %+v", got)
	}
	if got.Workload != embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL || got.FileFormat != embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE {
		t.Fatalf("Status table_status selection = %+v", got)
	}
}

func TestEmbedServer_CreateTableAnalyticalWorkloadUsesParquet(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	srv := newEmbedServer(eng)
	resp, err := srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{
		Table:    "analytics",
		Workload: embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL,
	})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if resp.Workload != embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL || resp.FileFormat != embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET {
		t.Fatalf("CreateTable selection = %+v", resp)
	}
	if _, err := srv.Write(context.Background(), &embedpb.WriteRequest{
		Table:     "analytics",
		Mutations: []*embedpb.Mutation{testMutation("row", "value")},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := srv.Flush(context.Background(), &embedpb.FlushRequest{Table: "analytics"}); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := countFilesWithExt(t, dir, ".parquet"); got != 1 {
		t.Fatalf("parquet files = %d, want 1", got)
	}
}

func TestEmbedServer_CompactMigratesToRequestedFormat(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	srv := newEmbedServer(eng)
	if _, err := srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{Table: "graph"}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if _, err := srv.Write(context.Background(), &embedpb.WriteRequest{
		Table:     "graph",
		Mutations: []*embedpb.Mutation{testMutation("row", "value")},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := srv.Flush(context.Background(), &embedpb.FlushRequest{Table: "graph"}); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := countFilesWithExt(t, dir, ".rf"); got != 1 {
		t.Fatalf("rfiles before compact = %d, want 1", got)
	}

	resp, err := srv.Compact(context.Background(), &embedpb.CompactRequest{
		Table:      "graph",
		FileFormat: embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if resp.Workload != embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL || resp.FileFormat != embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET {
		t.Fatalf("Compact selection = %+v", resp)
	}
	if got := countFilesWithExt(t, dir, ".rf"); got != 0 {
		t.Fatalf("rfiles after compact = %d, want 0", got)
	}
	if got := countFilesWithExt(t, dir, ".parquet"); got != 1 {
		t.Fatalf("parquet files after compact = %d, want 1", got)
	}

	st, err := srv.Status(context.Background(), &embedpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(st.TableStatuses) != 1 {
		t.Fatalf("Status table_statuses len = %d, want 1", len(st.TableStatuses))
	}
	if got := st.TableStatuses[0]; got.Workload != embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL || got.FileFormat != embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET {
		t.Fatalf("Status table_status = %+v", got)
	}
}

func TestScanStatusErrorMapsEmbeddingContractFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code codes.Code
	}{
		{
			name: "missing query identity",
			err: errors.Join(
				embedstore.ErrVectorEmbeddingSpaceRequired,
				embeddingspace.ErrQueryIdentityRequired,
			),
			code: codes.InvalidArgument,
		},
		{
			name: "incompatible stored identity",
			err: &embeddingspace.MismatchError{
				Operation: "scan",
				Left:      embeddingspace.Has("space-a"),
				Right:     embeddingspace.Has("space-b"),
			},
			code: codes.FailedPrecondition,
		},
		{
			name: "unknown stored identity",
			err:  embeddingspace.ErrQuerySpaceUnknown,
			code: codes.FailedPrecondition,
		},
		{
			name: "canceled metadata read",
			err:  context.Canceled,
			code: codes.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := status.Code(scanStatusError(test.err)); got != test.code {
				t.Fatalf("status code = %v, want %v", got, test.code)
			}
		})
	}
}

func TestEmbedServer_RejectsInvalidSelections(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	srv := newEmbedServer(eng)
	_, err = srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{
		Table:      "bad",
		Workload:   embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL,
		FileFormat: embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET,
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "conflicting workload") {
		t.Fatalf("CreateTable conflict error = %v, want InvalidArgument conflicting workload", err)
	}

	if _, err := srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{Table: "graph"}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	_, err = srv.Compact(context.Background(), &embedpb.CompactRequest{
		Table:    "graph",
		Workload: embedpb.TableWorkload(99),
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "unsupported workload") {
		t.Fatalf("Compact invalid workload error = %v, want InvalidArgument unsupported workload", err)
	}

	_, err = srv.Compact(context.Background(), &embedpb.CompactRequest{
		Table:      "graph",
		Workload:   embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL,
		FileFormat: embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE,
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "conflicting workload") {
		t.Fatalf("Compact conflict error = %v, want InvalidArgument conflicting workload", err)
	}
}

func TestEmbedServer_WriteReturnsPerMutationConditionalResults(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	srv := newEmbedServer(eng)
	if _, err := srv.CreateTable(context.Background(), &embedpb.CreateTableRequest{Table: "cas"}); err != nil {
		t.Fatal(err)
	}

	conditional := func(value string) *embedpb.Mutation {
		mutation := testMutation("row", value)
		mutation.Conditions = []*embedpb.Condition{{
			ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"),
			Predicate: &embedpb.Condition_Absent{Absent: true},
		}}
		return mutation
	}
	resp, err := srv.ConditionalWrite(context.Background(), &embedpb.WriteRequest{
		Table: "cas", Mutations: []*embedpb.Mutation{conditional("first"), conditional("second")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Written != 1 || len(resp.Results) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Results[0].Status != embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED ||
		resp.Results[1].Status != embedpb.MutationStatus_MUTATION_STATUS_REJECTED {
		t.Fatalf("results = %+v", resp.Results)
	}

	_, err = srv.ConditionalWrite(context.Background(), &embedpb.WriteRequest{
		Table: "cas",
		Mutations: []*embedpb.Mutation{{
			Row: []byte("other"), Entries: testMutation("other", "value").Entries,
			Conditions: []*embedpb.Condition{{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq")}},
		}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing predicate error = %v, want InvalidArgument", err)
	}

	_, err = srv.Write(context.Background(), &embedpb.WriteRequest{
		Table: "cas", Mutations: []*embedpb.Mutation{conditional("unsafe")},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("conditional Write error = %v, want InvalidArgument", err)
	}
}

func testMutation(row, value string) *embedpb.Mutation {
	return &embedpb.Mutation{
		Row: []byte(row),
		Entries: []*embedpb.Entry{{
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       1,
			Value:           []byte(value),
		}},
	}
}

func countFilesWithExt(t *testing.T, root, ext string) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ext {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
