package main

import (
	"context"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Projection limits are independent of top-k and are checked before sending
// any hit. A large row fails the entire request rather than yielding a partial
// projection that a client might mistake for complete metadata.
const (
	maxProjectedHits        = 100
	maxProjectedFamilies    = 32
	maxProjectedCellsPerRow = 256
	maxProjectedBytesPerRow = 1 << 20
	maxProjectedBytesTotal  = 8 << 20
)

func validateVectorProjection(req *embedpb.ScanRequest) (*embedpb.VectorProjection, error) {
	vs := req.VectorSearch
	if vs == nil || vs.Projection == nil {
		return nil, status.Error(codes.InvalidArgument, "vector_search.projection is required")
	}
	projection := vs.Projection
	if vs.TopK < 0 || vs.TopK > maxProjectedHits {
		return nil, status.Errorf(codes.InvalidArgument, "projected vector top_k must be in [0,%d]", maxProjectedHits)
	}
	if req.Limit < 0 || req.Limit > maxProjectedHits {
		return nil, status.Errorf(codes.InvalidArgument, "projected vector limit must be in [0,%d]", maxProjectedHits)
	}
	switch projection.Mode {
	case embedpb.VectorProjectionMode_VECTOR_PROJECTION_SCORED_CELL_ONLY,
		embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW:
		if len(projection.ColumnFamilies) != 0 {
			return nil, status.Error(codes.InvalidArgument, "column_families requires COLUMN_FAMILIES mode")
		}
	case embedpb.VectorProjectionMode_VECTOR_PROJECTION_COLUMN_FAMILIES:
		if len(projection.ColumnFamilies) == 0 || len(projection.ColumnFamilies) > maxProjectedFamilies {
			return nil, status.Errorf(codes.InvalidArgument, "column_families count must be in [1,%d]", maxProjectedFamilies)
		}
		seen := make(map[string]bool, len(projection.ColumnFamilies))
		for _, family := range projection.ColumnFamilies {
			if len(family) > 256 || seen[string(family)] {
				return nil, status.Error(codes.InvalidArgument, "column_families must be unique and at most 256 bytes each")
			}
			seen[string(family)] = true
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown vector projection mode")
	}
	return projection, nil
}

func viewCell(key *iterrt.Key, value []byte) *embedpb.Cell {
	return &embedpb.Cell{
		Row:              key.Row,
		ColumnFamily:     key.ColumnFamily,
		ColumnQualifier:  key.ColumnQualifier,
		ColumnVisibility: key.ColumnVisibility,
		Timestamp:        key.Timestamp,
		Value:            value,
	}
}

func copyCell(key *iterrt.Key, value []byte) *embedpb.Cell {
	cell := viewCell(key, value)
	clone := func(b []byte) []byte { return append([]byte(nil), b...) }
	cell.Row = clone(cell.Row)
	cell.ColumnFamily = clone(cell.ColumnFamily)
	cell.ColumnQualifier = clone(cell.ColumnQualifier)
	cell.ColumnVisibility = clone(cell.ColumnVisibility)
	cell.Value = clone(cell.Value)
	return cell
}

func cellWireSize(key *iterrt.Key, value []byte) int {
	return proto.Size(viewCell(key, value))
}

func projectionScanStack(asOf int64) []iterrt.IterSpec {
	stack := []iterrt.IterSpec{
		{Name: iterrt.IterDeleting, Options: map[string]string{iterrt.DeletingOptionPropagate: "false"}},
		{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
	}
	if asOf > 0 {
		stack = append([]iterrt.IterSpec{{
			Name:    iterrt.IterAsOf,
			Options: map[string]string{iterrt.AsOfOption: strconv.FormatInt(asOf, 10)},
		}}, stack...)
	}
	return stack
}

func (s *embedServer) scanProjectedVector(req *embedpb.ScanRequest, stream scanServer) error {
	if req.Table == "" {
		return status.Error(codes.InvalidArgument, "table is required")
	}
	projection, err := validateVectorProjection(req)
	if err != nil {
		return err
	}
	ctx := stream.Context()
	scanner, err := s.store.ScannerContext(ctx, req.Table, req)
	if err != nil {
		return scanStatusError(err)
	}
	defer scanner.Close()

	// top_k remains the number of scored embedding cells, not unique rows.
	// Preserve the iterator's score/key order, including duplicate row IDs.
	hits := make([]*embedpb.VectorHit, 0)
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return scanStatusError(err)
		}
		if proto.Size(&embedpb.VectorHit{
			ScoredCell:     viewCell(scanner.Key(), scanner.Value()),
			EmbeddingSpace: req.VectorSearch.EmbeddingSpace,
		}) > maxProjectedBytesPerRow {
			return status.Error(codes.ResourceExhausted, "scored vector hit exceeds projection byte limit")
		}
		hit := &embedpb.VectorHit{
			ScoredCell:     copyCell(scanner.Key(), scanner.Value()),
			EmbeddingSpace: req.VectorSearch.EmbeddingSpace,
		}
		hits = append(hits, hit)
		if len(hits) > maxProjectedHits {
			return status.Error(codes.ResourceExhausted, "projected vector hit count exceeds limit")
		}
		if req.Limit > 0 && len(hits) >= int(req.Limit) {
			break
		}
		if err := scanner.Advance(); err != nil {
			return status.Errorf(codes.Internal, "projected scan advance: %v", err)
		}
	}

	if projection.Mode != embedpb.VectorProjectionMode_VECTOR_PROJECTION_SCORED_CELL_ONLY {
		// LookupRows shares the ordinary scan's deletion, versioning, as-of,
		// column-family, and visibility stack. It routes all selected rows through
		// one merge instead of requiring a full prefix scan per hit.
		rows := make([][]byte, 0, len(hits))
		rowIndex := make(map[string]int, len(hits))
		hitRows := make([]int, len(hits))
		for i, hit := range hits {
			row := hit.ScoredCell.Row
			idx, ok := rowIndex[string(row)]
			if !ok {
				idx = len(rows)
				rowIndex[string(row)] = idx
				rows = append(rows, row)
			}
			hitRows[i] = idx
		}
		rowCells := make([][]*embedpb.Cell, len(rows))
		rowBytes := make([]int, len(rows))
		lookupCtx, cancelLookup := context.WithCancel(ctx)
		defer cancelLookup()
		opts := engine.ScanOptions{Context: lookupCtx, Stack: projectionScanStack(req.AsOf)}
		if projection.Mode == embedpb.VectorProjectionMode_VECTOR_PROJECTION_COLUMN_FAMILIES {
			opts.ColumnFamilies = projection.ColumnFamilies
			opts.ColumnFamiliesInclusive = true
		}
		var limitErr error
		totalStoredBytes := 0
		err = s.eng.LookupRows(req.Table, rows, opts, func(idx int, key *iterrt.Key, value []byte) {
			if limitErr != nil {
				return
			}
			size := cellWireSize(key, value)
			if len(rowCells[idx]) >= maxProjectedCellsPerRow ||
				rowBytes[idx]+size > maxProjectedBytesPerRow ||
				totalStoredBytes+size > maxProjectedBytesTotal {
				limitErr = status.Error(codes.ResourceExhausted, "projected vector row exceeds cell or byte limit")
				cancelLookup()
				return
			}
			cell := copyCell(key, value)
			rowCells[idx] = append(rowCells[idx], cell)
			rowBytes[idx] += size
			totalStoredBytes += size
		})
		if limitErr != nil {
			return limitErr
		}
		if err != nil {
			return scanStatusError(err)
		}
		if err := ctx.Err(); err != nil {
			return scanStatusError(err)
		}
		for i, hit := range hits {
			hit.RowCells = rowCells[hitRows[i]]
			if projection.Mode == embedpb.VectorProjectionMode_VECTOR_PROJECTION_FULL_ROW && len(hit.RowCells) == 0 {
				return status.Errorf(codes.Aborted, "scored row %q disappeared during projection", hit.ScoredCell.Row)
			}
		}
	}

	// Check the fully encoded output before the first Send. Repeated hits from
	// one row may amplify the response even though lookup storage was deduped.
	totalWireBytes := 0
	for _, hit := range hits {
		response := &embedpb.ScanResponse{VectorHits: []*embedpb.VectorHit{hit}}
		if proto.Size(response) > maxProjectedBytesPerRow {
			return status.Error(codes.ResourceExhausted, "projected vector hit exceeds byte limit")
		}
		totalWireBytes += proto.Size(response)
		if totalWireBytes > maxProjectedBytesTotal {
			return status.Error(codes.ResourceExhausted, "projected vector response exceeds byte limit")
		}
	}
	for _, hit := range hits {
		if err := ctx.Err(); err != nil {
			return scanStatusError(err)
		}
		if err := stream.Send(&embedpb.ScanResponse{VectorHits: []*embedpb.VectorHit{hit}}); err != nil {
			return err
		}
	}
	return nil
}
