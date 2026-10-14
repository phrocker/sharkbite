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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/tablet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultScanBatchSize = embedstore.DefaultScanBatchSize

// embedServer implements the ShoalEmbed gRPC service. All engine translation
// (mutation building, ScanRequest pushdowns, range derivation) is delegated to
// the shared embedstore.EngineStore so the wire server and in-process callers
// stay byte-for-byte identical.
type embedServer struct {
	embedpb.UnimplementedShoalEmbedServer
	eng     *engine.Engine
	store   *embedstore.EngineStore
	dataDir string
}

// newEmbedServer wires an embedServer over eng.
func newEmbedServer(eng *engine.Engine) *embedServer {
	return &embedServer{eng: eng, store: embedstore.New(eng), dataDir: engineDataDir(eng)}
}

type tableSelection struct {
	workload      engine.WorkloadProfile
	fileFormat    tablet.FileFormat
	workloadSet   bool
	fileFormatSet bool
}

type persistedTableStatus struct {
	workload   engine.WorkloadProfile
	fileFormat tablet.FileFormat
}

type tableManifest struct {
	Splits     [][]byte `json:"splits,omitempty"`
	FileFormat string   `json:"file_format"`
}

func engineDataDir(eng *engine.Engine) string {
	if eng == nil {
		return ""
	}
	v := reflect.ValueOf(eng)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ""
	}
	field := v.Elem().FieldByName("dir")
	if !field.IsValid() || field.Kind() != reflect.String {
		return ""
	}
	return field.String()
}

func adminStatusError(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "already exists"):
		return status.Error(codes.AlreadyExists, err.Error())
	case strings.Contains(err.Error(), "not found"):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Errorf(codes.Internal, "%s: %v", op, err)
	}
}

func workloadFromProto(value embedpb.TableWorkload) (engine.WorkloadProfile, bool, error) {
	switch value {
	case embedpb.TableWorkload_TABLE_WORKLOAD_UNSPECIFIED:
		return "", false, nil
	case embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL:
		return engine.WorkloadOperational, true, nil
	case embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL:
		return engine.WorkloadAnalytical, true, nil
	default:
		return "", false, fmt.Errorf("unsupported workload %d", value)
	}
}

func fileFormatFromProto(value embedpb.TableFileFormat) (tablet.FileFormat, bool, error) {
	switch value {
	case embedpb.TableFileFormat_TABLE_FILE_FORMAT_UNSPECIFIED:
		return "", false, nil
	case embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE:
		return tablet.FormatRFile, true, nil
	case embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET:
		return tablet.FormatParquet, true, nil
	default:
		return "", false, fmt.Errorf("unsupported file_format %d", value)
	}
}

func fileFormatForWorkload(workload engine.WorkloadProfile) tablet.FileFormat {
	if workload == engine.WorkloadAnalytical {
		return tablet.FormatParquet
	}
	return tablet.FormatRFile
}

func workloadForFileFormat(format tablet.FileFormat) engine.WorkloadProfile {
	if format == tablet.FormatParquet {
		return engine.WorkloadAnalytical
	}
	return engine.WorkloadOperational
}

func protoWorkload(workload engine.WorkloadProfile) embedpb.TableWorkload {
	if workload == engine.WorkloadAnalytical {
		return embedpb.TableWorkload_TABLE_WORKLOAD_ANALYTICAL
	}
	return embedpb.TableWorkload_TABLE_WORKLOAD_OPERATIONAL
}

func protoFileFormat(format tablet.FileFormat) embedpb.TableFileFormat {
	if format == tablet.FormatParquet {
		return embedpb.TableFileFormat_TABLE_FILE_FORMAT_PARQUET
	}
	return embedpb.TableFileFormat_TABLE_FILE_FORMAT_RFILE
}

func resolveTableSelection(workload embedpb.TableWorkload, fileFormat embedpb.TableFileFormat) (tableSelection, error) {
	resolved := tableSelection{}
	var err error
	if resolved.workload, resolved.workloadSet, err = workloadFromProto(workload); err != nil {
		return tableSelection{}, err
	}
	if resolved.fileFormat, resolved.fileFormatSet, err = fileFormatFromProto(fileFormat); err != nil {
		return tableSelection{}, err
	}
	if resolved.workloadSet && resolved.fileFormatSet && resolved.fileFormat != fileFormatForWorkload(resolved.workload) {
		return tableSelection{}, fmt.Errorf("conflicting workload %q and file_format %q", resolved.workload, resolved.fileFormat)
	}
	if resolved.workloadSet && !resolved.fileFormatSet {
		resolved.fileFormat = fileFormatForWorkload(resolved.workload)
	}
	if !resolved.workloadSet && resolved.fileFormatSet {
		resolved.workload = workloadForFileFormat(resolved.fileFormat)
	}
	return resolved, nil
}

func (s tableSelection) requested() bool {
	return s.workloadSet || s.fileFormatSet
}

func (s *embedServer) readPersistedTableStatus(table string) (persistedTableStatus, error) {
	if s.dataDir == "" {
		return persistedTableStatus{}, fmt.Errorf("engine data directory unavailable")
	}
	path := filepath.Join(s.dataDir, table, "table.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return persistedTableStatus{
			workload:   engine.WorkloadOperational,
			fileFormat: tablet.FormatRFile,
		}, nil
	}
	if err != nil {
		return persistedTableStatus{}, fmt.Errorf("read manifest %s: %w", path, err)
	}
	var manifest tableManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return persistedTableStatus{}, fmt.Errorf("decode manifest %s: %w", path, err)
	}
	format, err := tablet.ParseFileFormat(manifest.FileFormat)
	if err != nil {
		return persistedTableStatus{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return persistedTableStatus{
		workload:   workloadForFileFormat(format),
		fileFormat: format,
	}, nil
}

func (s *embedServer) CreateTable(_ context.Context, req *embedpb.CreateTableRequest) (*embedpb.CreateTableResponse, error) {
	if req.DefaultEmbedding != "" {
		return nil, status.Error(
			codes.FailedPrecondition,
			"default_embedding requires CreateTableV2",
		)
	}
	return s.createTable(req)
}

func (s *embedServer) CreateTableV2(
	_ context.Context,
	req *embedpb.CreateTableRequest,
) (*embedpb.CreateTableResponse, error) {
	return s.createTable(req)
}

func (s *embedServer) createTable(
	req *embedpb.CreateTableRequest,
) (*embedpb.CreateTableResponse, error) {
	if req.Table == "" {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	selection, err := resolveTableSelection(req.Workload, req.FileFormat)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	opts := engine.TableOptions{}
	if len(req.Splits) > 0 {
		opts.Splits = engine.PrefixSplit(req.Splits...)
	}
	if selection.workloadSet {
		opts.Workload = selection.workload
	}
	if selection.fileFormatSet {
		opts.TabletOptions.FileFormat = selection.fileFormat
	}
	if req.DefaultEmbedding != "" {
		state, err := embeddingspace.Parse(req.DefaultEmbedding)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		opts.DefaultEmbedding = state
	}
	if err := s.eng.CreateTable(req.Table, opts); err != nil {
		return nil, adminStatusError("create table", err)
	}
	if !selection.requested() {
		selection.workload = engine.WorkloadOperational
		selection.fileFormat = tablet.FormatRFile
	}
	return &embedpb.CreateTableResponse{
		Table:      req.Table,
		Tablets:    int32(len(req.Splits) + 1),
		Workload:   protoWorkload(selection.workload),
		FileFormat: protoFileFormat(selection.fileFormat),
	}, nil
}

func (s *embedServer) Write(ctx context.Context, req *embedpb.WriteRequest) (*embedpb.WriteResponse, error) {
	if req.Table == "" {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	for _, mutation := range req.Mutations {
		if mutation != nil && len(mutation.Conditions) > 0 {
			return nil, status.Error(codes.InvalidArgument, "conditional mutations require ConditionalWrite")
		}
	}
	return s.writeWithResults(ctx, req)
}

func (s *embedServer) ConditionalWrite(ctx context.Context, req *embedpb.WriteRequest) (*embedpb.WriteResponse, error) {
	if req.Table == "" {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	return s.writeWithResults(ctx, req)
}

func (s *embedServer) writeWithResults(ctx context.Context, req *embedpb.WriteRequest) (*embedpb.WriteResponse, error) {
	results, err := s.store.WriteWithResults(ctx, req.Table, req.Mutations)
	if errors.Is(err, embedstore.ErrInvalidCondition) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write: %v", err)
	}
	var written int32
	for _, result := range results {
		if result.Status == embedpb.MutationStatus_MUTATION_STATUS_ACCEPTED {
			written++
		}
	}
	return &embedpb.WriteResponse{Written: written, Results: results}, nil
}

// scanStatusError maps embedstore's validation sentinels onto gRPC codes,
// defaulting any other error to Internal.
func scanStatusError(err error) error {
	switch {
	case errors.Is(err, embedstore.ErrMultiplePushdowns),
		errors.Is(err, embedstore.ErrVectorQueryRequired),
		errors.Is(err, embedstore.ErrVectorEmbeddingSpaceRequired),
		errors.Is(err, embedstore.ErrNegativeMaxHops):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, embeddingspace.ErrMismatch),
		errors.Is(err, embeddingspace.ErrIntegrity),
		errors.Is(err, embeddingspace.ErrQuerySpaceUnknown),
		errors.Is(err, embeddingspace.ErrQueryNoEmbeddings),
		errors.Is(err, embeddingspace.ErrQueryMetadataMissing):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	default:
		return status.Errorf(codes.Internal, "scan: %v", err)
	}
}

type scanServer interface {
	Context() context.Context
	Send(*embedpb.ScanResponse) error
}

func (s *embedServer) Scan(
	req *embedpb.ScanRequest,
	stream embedpb.ShoalEmbed_ScanServer,
) error {
	if req.VectorSearch != nil {
		return status.Error(
			codes.FailedPrecondition,
			"vector_search requires ScanV2 identity enforcement",
		)
	}
	return s.scan(req, stream)
}

func (s *embedServer) ScanV2(
	req *embedpb.ScanRequest,
	stream embedpb.ShoalEmbed_ScanV2Server,
) error {
	return s.scan(req, stream)
}

func (s *embedServer) scan(req *embedpb.ScanRequest, stream scanServer) error {
	if req.Table == "" {
		return status.Error(codes.InvalidArgument, "table is required")
	}

	sc, err := s.store.ScannerContext(stream.Context(), req.Table, req)
	if err != nil {
		return scanStatusError(err)
	}
	defer sc.Close()

	batchSize := int(req.BatchSize)
	if batchSize <= 0 {
		batchSize = defaultScanBatchSize
	}
	limit := int(req.Limit)

	batch := make([]*embedpb.Cell, 0, batchSize)
	total := 0

	for sc.Next() {
		if err := stream.Context().Err(); err != nil {
			return scanStatusError(err)
		}
		k := sc.Key()
		batch = append(batch, &embedpb.Cell{
			Row:              k.Row,
			ColumnFamily:     k.ColumnFamily,
			ColumnQualifier:  k.ColumnQualifier,
			ColumnVisibility: k.ColumnVisibility,
			Timestamp:        k.Timestamp,
			Value:            sc.Value(),
		})

		if err := sc.Advance(); err != nil {
			return status.Errorf(codes.Internal, "scan advance: %v", err)
		}

		total++

		// Flush batch when full
		if len(batch) >= batchSize {
			if err := stream.Send(&embedpb.ScanResponse{Cells: batch}); err != nil {
				return err
			}
			batch = make([]*embedpb.Cell, 0, batchSize)
		}

		if limit > 0 && total >= limit {
			break
		}
	}

	// Send remaining cells
	if len(batch) > 0 {
		if err := stream.Send(&embedpb.ScanResponse{Cells: batch}); err != nil {
			return err
		}
	}

	return nil
}

func (s *embedServer) Flush(ctx context.Context, req *embedpb.FlushRequest) (*embedpb.FlushResponse, error) {
	if req.Table == "" {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	if err := s.store.Flush(ctx, req.Table); err != nil {
		return nil, adminStatusError("flush", err)
	}
	return &embedpb.FlushResponse{}, nil
}

func (s *embedServer) Compact(ctx context.Context, req *embedpb.CompactRequest) (*embedpb.CompactResponse, error) {
	if req.Table == "" {
		return nil, status.Error(codes.InvalidArgument, "table is required")
	}
	current, err := s.readPersistedTableStatus(req.Table)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "compact: %v", err)
	}
	selection, err := resolveTableSelection(req.Workload, req.FileFormat)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	workload := current.workload
	fileFormat := current.fileFormat
	if selection.requested() {
		workload = selection.workload
		fileFormat = selection.fileFormat
		if err := s.eng.MigrateTableStorageFormat(req.Table, engine.StorageFormat(fileFormat), nil); err != nil {
			return nil, adminStatusError("compact migrate format", err)
		}
	} else if err := s.store.Compact(ctx, req.Table); err != nil {
		return nil, adminStatusError("compact", err)
	}
	return &embedpb.CompactResponse{
		Table:      req.Table,
		Workload:   protoWorkload(workload),
		FileFormat: protoFileFormat(fileFormat),
	}, nil
}

func (s *embedServer) Status(_ context.Context, _ *embedpb.StatusRequest) (*embedpb.StatusResponse, error) {
	stats := s.eng.Stats()
	resp := &embedpb.StatusResponse{
		Tables:        make([]string, 0, len(stats)),
		TableStatuses: make([]*embedpb.TableStatus, 0, len(stats)),
	}
	for _, stat := range stats {
		meta, err := s.readPersistedTableStatus(stat.Name)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "status: %v", err)
		}
		resp.Tables = append(resp.Tables, stat.Name)
		resp.TableStatuses = append(resp.TableStatuses, &embedpb.TableStatus{
			Table:          stat.Name,
			Tablets:        int32(stat.Tablets),
			ImmutableFiles: int32(stat.RFiles),
			Workload:       protoWorkload(meta.workload),
			FileFormat:     protoFileFormat(meta.fileFormat),
		})
	}
	return resp, nil
}

// Verify at compile time that embedServer implements the interface.
var _ embedpb.ShoalEmbedServer = (*embedServer)(nil)
