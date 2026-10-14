/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorercoord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

const legacyMigrationPageSize = 256

var (
	legacyMigrationRow    = []byte{0, 's', 'h', 'o', 'a', 'l', '-', 't', 'x', 'n', '-', 'v', '1'}
	legacyMarkerRowPrefix = []byte{0, 's', 'h', 'o', 'a', 'l', '-', 'l', 'e', 'g', 'a', 'c', 'y', '-'}
	legacyMarkerFamily    = []byte("txn")
	legacyMarkerQualifier = []byte("legacy")
)

type legacyDocumentRecord struct {
	row       []byte
	qualifier []byte
	value     []byte
}

func (r *Runtime) enableExplorerLegacyCompatibility(ctx context.Context) error {
	store, err := NewEngineStore(r.engine, explorer.EmbeddedTableName)
	if err != nil {
		return err
	}
	global := r.legacyMigrationCoordinate()
	expected := r.legacyMigrationValue()
	found, err := exactMarker(ctx, store, global, expected)
	if err != nil {
		return err
	}
	if found {
		r.legacyRecords = store
		return nil
	}
	head, err := r.allocator.CurrentHead(ctx)
	if err != nil {
		return err
	}
	if head.NextEpoch != 1 || head.Frontier != 0 {
		return fmt.Errorf(
			"%w: Explorer legacy migration marker is absent after transaction activity",
			transaction.ErrInternal,
		)
	}

	var cursor []byte
	for page := 0; page < r.recoveryPages; page++ {
		records, next, err := r.scanLegacyDocumentRows(
			ctx,
			cursor,
			legacyMigrationPageSize,
		)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err := putAbsentOrIdenticalMarker(
				ctx,
				store,
				legacyRecordCoordinate(record.row, record.qualifier),
				legacyRecordMarkerValue(
					record.row,
					record.qualifier,
					record.value,
				),
			); err != nil {
				return err
			}
		}
		if len(next) == 0 {
			if err := putAbsentOrIdenticalMarker(
				ctx,
				store,
				global,
				expected,
			); err != nil {
				return err
			}
			r.legacyRecords = store
			return nil
		}
		cursor = next
	}
	return errors.Join(
		transaction.ErrUnavailable,
		errors.New("Explorer legacy migration page bound reached"),
	)
}

func (r *Runtime) legacyRecordAllowed(
	ctx context.Context,
	request explorer.RecordPublication,
) (bool, error) {
	if r.legacyRecords == nil ||
		request.Table != explorer.EmbeddedTableName ||
		len(request.Row) == 0 ||
		(!bytes.Equal(request.Qualifier, []byte("v1")) &&
			!bytes.Equal(request.Qualifier, []byte("v2"))) {
		return false, nil
	}
	return exactMarker(
		ctx,
		r.legacyRecords,
		legacyRecordCoordinate(request.Row, request.Qualifier),
		legacyRecordMarkerValue(
			request.Row,
			request.Qualifier,
			request.Value,
		),
	)
}

func (r *Runtime) scanLegacyDocumentRows(
	ctx context.Context,
	start []byte,
	limit int,
) ([]legacyDocumentRecord, []byte, error) {
	prefix := []byte("document/")
	if len(start) == 0 {
		start = prefix
	}
	if !bytes.HasPrefix(start, prefix) || limit < 1 {
		return nil, nil, errors.Join(
			transaction.ErrInvalid,
			errors.New("legacy document scan arguments are invalid"),
		)
	}
	end, ok := prefixSuccessor(prefix)
	if !ok {
		return nil, nil, transaction.ErrInternal
	}
	scanner, err := r.engine.Scan(explorer.EmbeddedTableName, iterrt.Range{
		Start: &wire.Key{
			Row:       append([]byte(nil), start...),
			Timestamp: math.MaxInt64,
			Deleted:   true,
		},
		StartInclusive: true,
		End: &wire.Key{
			Row:       end,
			Timestamp: math.MaxInt64,
			Deleted:   true,
		},
		EndInclusive: false,
	}, engine.ScanOptions{
		ColumnFamilies:          [][]byte{[]byte("record")},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return nil, nil, err
	}
	defer scanner.Close()

	records := make([]legacyDocumentRecord, 0, limit)
	var current []byte
	var currentRecords []legacyDocumentRecord
	seenV1 := false
	seenV2 := false
	rowsInspected := 0
	finish := func() {
		records = append(records, currentRecords...)
	}
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		key := scanner.Key()
		if !bytes.Equal(current, key.Row) {
			if len(current) != 0 {
				finish()
			}
			if rowsInspected == limit {
				return records, append(append([]byte(nil), current...), 0), nil
			}
			current = append(current[:0], key.Row...)
			rowsInspected++
			currentRecords = currentRecords[:0]
			seenV1 = false
			seenV2 = false
		}
		if len(key.ColumnVisibility) == 0 {
			qualifier := string(key.ColumnQualifier)
			first := (qualifier == "v1" && !seenV1) ||
				(qualifier == "v2" && !seenV2)
			if qualifier == "v1" {
				seenV1 = true
			}
			if qualifier == "v2" {
				seenV2 = true
			}
			if first && !key.Deleted {
				currentRecords = append(currentRecords, legacyDocumentRecord{
					row:       append([]byte(nil), key.Row...),
					qualifier: append([]byte(nil), key.ColumnQualifier...),
					value:     append([]byte(nil), scanner.Value()...),
				})
			}
		}
		if err := scanner.Advance(); err != nil {
			return nil, nil, err
		}
	}
	if len(current) != 0 {
		finish()
	}
	return records, nil, nil
}

func (r *Runtime) legacyMigrationCoordinate() allocator.Coordinate {
	return allocator.Coordinate{
		Row:        append([]byte(nil), legacyMigrationRow...),
		Family:     append([]byte(nil), legacyMarkerFamily...),
		Qualifier:  append([]byte(nil), legacyMarkerQualifier...),
		Visibility: nil,
	}
}

func (r *Runtime) legacyMigrationValue() []byte {
	value := append([]byte("shoal-explorer-transaction-migration-v1\x00"), r.domain...)
	return value
}

func legacyRecordCoordinate(row, qualifier []byte) allocator.Coordinate {
	hash := sha256.New()
	writeDigestPart(hash, row)
	writeDigestPart(hash, qualifier)
	markerRow := append([]byte(nil), legacyMarkerRowPrefix...)
	markerRow = append(markerRow, hash.Sum(nil)...)
	return allocator.Coordinate{
		Row:        markerRow,
		Family:     append([]byte(nil), legacyMarkerFamily...),
		Qualifier:  append([]byte(nil), legacyMarkerQualifier...),
		Visibility: nil,
	}
}

func legacyRecordMarkerValue(row, qualifier, value []byte) []byte {
	hash := sha256.New()
	writeDigestPart(hash, []byte("shoal-explorer-legacy-record-v1"))
	writeDigestPart(hash, row)
	writeDigestPart(hash, qualifier)
	writeDigestPart(hash, value)
	return hash.Sum(nil)
}

func exactMarker(
	ctx context.Context,
	store *EngineStore,
	coordinate allocator.Coordinate,
	expected []byte,
) (bool, error) {
	cells, err := store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return false, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return false, nil
	}
	if len(cells) != 1 ||
		cells[0].Timestamp != 1 ||
		!bytes.Equal(cells[0].Value, expected) {
		return false, fmt.Errorf(
			"%w: Explorer legacy migration marker is invalid",
			transaction.ErrInternal,
		)
	}
	return true, nil
}

func putAbsentOrIdenticalMarker(
	ctx context.Context,
	store *EngineStore,
	coordinate allocator.Coordinate,
	value []byte,
) error {
	status, writeErr := store.CompareAndMutate(ctx, allocator.Mutation{
		Row: coordinate.Row,
		Conditions: []allocator.Condition{{
			Coordinate: coordinate,
			Absent:     true,
		}},
		Updates: []allocator.Update{{
			Coordinate: coordinate,
			Value:      append([]byte(nil), value...),
			Timestamp:  1,
		}},
	})
	if status == allocator.StatusAccepted {
		return nil
	}
	found, readErr := exactMarker(ctx, store, coordinate, value)
	if readErr != nil {
		return errors.Join(writeErr, readErr)
	}
	if found {
		return nil
	}
	if status == allocator.StatusRejected {
		return transaction.ErrConflict
	}
	return errors.Join(
		transaction.ErrUnavailable,
		allocator.ErrConditionalUnknown,
		writeErr,
	)
}
