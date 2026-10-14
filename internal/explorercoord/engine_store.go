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

// Package explorercoord binds Explorer's storage-neutral transaction protocol
// to Shoal's embedded engine.
package explorercoord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

const engineReadBound = 10_001

// EngineStore is the exact-read, bounded-scan, row-CAS implementation shared
// by the allocator, guards, control records, transaction coordinator, and
// recovery worker.
type EngineStore struct {
	engine *engine.Engine
	table  string
}

func NewEngineStore(eng *engine.Engine, table string) (*EngineStore, error) {
	if eng == nil {
		return nil, errors.New("explorer coordination: engine is required")
	}
	if table == "" {
		return nil, errors.New("explorer coordination: table is required")
	}
	return &EngineStore{engine: eng, table: table}, nil
}

func (s *EngineStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	if len(coordinates) > engineReadBound {
		return nil, errors.New("explorer coordination: exact read exceeds its bound")
	}
	result := make([]allocator.Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cell, found, err := s.readNewest(ctx, coordinate)
		if err != nil {
			return nil, err
		}
		if found {
			result = append(result, cell)
		}
	}
	return result, nil
}

func (s *EngineStore) ScanRowPrefix(
	ctx context.Context,
	row, family, qualifierStart, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	if len(row) == 0 || limit < 1 || limit > engineReadBound {
		return nil, errors.New("explorer coordination: row scan arguments are outside their bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scanner, err := s.engine.Scan(s.table, exactRowRange(row), engine.ScanOptions{
		ColumnFamilies:          [][]byte{append([]byte(nil), family...)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return nil, err
	}
	defer scanner.Close()

	result := make([]allocator.Cell, 0, limit)
	seen := make(map[string]struct{})
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := scanner.Key()
		if bytes.Equal(key.Row, row) && bytes.Equal(key.ColumnFamily, family) &&
			bytes.Equal(key.ColumnVisibility, visibility) &&
			bytes.Compare(key.ColumnQualifier, qualifierStart) >= 0 {
			identity := coordinateIdentity(key.Row, key.ColumnFamily, key.ColumnQualifier, key.ColumnVisibility)
			if _, ok := seen[identity]; !ok {
				seen[identity] = struct{}{}
				if !key.Deleted {
					result = append(result, cellFromEngine(key, scanner.Value()))
					if len(result) == limit {
						break
					}
				}
			}
		}
		if err := scanner.Advance(); err != nil {
			return nil, err
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Qualifier, result[j].Coordinate.Qualifier) < 0
	})
	return result, nil
}

func (s *EngineStore) ScanPrefix(
	ctx context.Context,
	rowPrefix, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.ScanPrefixFrom(ctx, rowPrefix, rowPrefix, family, qualifier, visibility, limit)
}

func (s *EngineStore) ScanPrefixFrom(
	ctx context.Context,
	rowPrefix, startRow, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	if len(rowPrefix) == 0 || !bytes.HasPrefix(startRow, rowPrefix) ||
		bytes.Compare(startRow, rowPrefix) < 0 || limit < 1 || limit > engineReadBound {
		return nil, errors.New("explorer coordination: prefix scan arguments are outside their bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	endRow, ok := prefixSuccessor(rowPrefix)
	if !ok {
		return nil, errors.New("explorer coordination: row prefix has no bounded successor")
	}
	scanner, err := s.engine.Scan(s.table, iterrt.Range{
		Start:          &wire.Key{Row: append([]byte(nil), startRow...), Timestamp: math.MaxInt64, Deleted: true},
		StartInclusive: true,
		End:            &wire.Key{Row: endRow, Timestamp: math.MaxInt64, Deleted: true},
		EndInclusive:   false,
	}, engine.ScanOptions{
		ColumnFamilies:          [][]byte{append([]byte(nil), family...)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return nil, err
	}
	defer scanner.Close()

	result := make([]allocator.Cell, 0, limit)
	seen := make(map[string]struct{})
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := scanner.Key()
		if bytes.HasPrefix(key.Row, rowPrefix) &&
			bytes.Compare(key.Row, startRow) >= 0 &&
			bytes.Equal(key.ColumnFamily, family) &&
			bytes.Equal(key.ColumnQualifier, qualifier) &&
			bytes.Equal(key.ColumnVisibility, visibility) {
			identity := coordinateIdentity(key.Row, key.ColumnFamily, key.ColumnQualifier, key.ColumnVisibility)
			if _, ok := seen[identity]; !ok {
				seen[identity] = struct{}{}
				if !key.Deleted {
					result = append(result, cellFromEngine(key, scanner.Value()))
					if len(result) == limit {
						break
					}
				}
			}
		}
		if err := scanner.Advance(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *EngineStore) CompareAndMutate(
	ctx context.Context,
	request allocator.Mutation,
) (allocator.Status, error) {
	if err := ctx.Err(); err != nil {
		return allocator.StatusUnknown, err
	}
	if len(request.Row) == 0 || len(request.Conditions) == 0 || len(request.Updates) == 0 ||
		len(request.Conditions) > engineReadBound || len(request.Updates) > engineReadBound {
		return allocator.StatusUnknown, errors.New("explorer coordination: malformed conditional mutation")
	}
	mutation, err := cclient.NewMutation(request.Row)
	if err != nil {
		return allocator.StatusUnknown, err
	}
	for index, update := range request.Updates {
		if !bytes.Equal(update.Coordinate.Row, request.Row) {
			return allocator.StatusUnknown, fmt.Errorf("explorer coordination: update %d crosses rows", index)
		}
		if update.Delete {
			mutation.Delete(
				update.Coordinate.Family,
				update.Coordinate.Qualifier,
				update.Coordinate.Visibility,
				update.Timestamp,
			)
		} else {
			mutation.Put(
				update.Coordinate.Family,
				update.Coordinate.Qualifier,
				update.Coordinate.Visibility,
				update.Timestamp,
				update.Value,
			)
		}
	}
	conditions := make([]engine.Condition, len(request.Conditions))
	for index, condition := range request.Conditions {
		if !bytes.Equal(condition.Coordinate.Row, request.Row) {
			return allocator.StatusUnknown, fmt.Errorf("explorer coordination: condition %d crosses rows", index)
		}
		kind := engine.ConditionValueEquals
		if condition.Absent {
			kind = engine.ConditionAbsent
		} else if condition.TimestampSet {
			kind = engine.ConditionLatestValueAndTimestampEquals
		}
		var timestamp *int64
		if condition.TimestampSet {
			value := condition.Timestamp
			timestamp = &value
		}
		conditions[index] = engine.Condition{
			ColumnFamily:     append([]byte(nil), condition.Coordinate.Family...),
			ColumnQualifier:  append([]byte(nil), condition.Coordinate.Qualifier...),
			ColumnVisibility: append([]byte(nil), condition.Coordinate.Visibility...),
			Timestamp:        timestamp,
			Kind:             kind,
			Value:            append([]byte(nil), condition.Value...),
		}
	}
	results, err := s.engine.ConditionalWrite(s.table, []engine.ConditionalMutation{{
		Mutation: mutation, Conditions: conditions,
	}})
	if err != nil {
		return allocator.StatusUnknown, errors.Join(allocator.ErrConditionalUnknown, err)
	}
	if len(results) != 1 {
		return allocator.StatusUnknown, errors.New("explorer coordination: invalid conditional result count")
	}
	if results[0] {
		return allocator.StatusAccepted, nil
	}
	return allocator.StatusRejected, nil
}

func (s *EngineStore) readNewest(
	ctx context.Context,
	coordinate allocator.Coordinate,
) (allocator.Cell, bool, error) {
	if len(coordinate.Row) == 0 {
		return allocator.Cell{}, false, errors.New("explorer coordination: exact read row is required")
	}
	if err := ctx.Err(); err != nil {
		return allocator.Cell{}, false, err
	}
	scanner, err := s.engine.Scan(s.table, exactRowRange(coordinate.Row), engine.ScanOptions{
		ColumnFamilies:          [][]byte{append([]byte(nil), coordinate.Family...)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return allocator.Cell{}, false, err
	}
	defer scanner.Close()
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return allocator.Cell{}, false, err
		}
		key := scanner.Key()
		if bytes.Equal(key.Row, coordinate.Row) &&
			bytes.Equal(key.ColumnFamily, coordinate.Family) &&
			bytes.Equal(key.ColumnQualifier, coordinate.Qualifier) &&
			bytes.Equal(key.ColumnVisibility, coordinate.Visibility) {
			if key.Deleted {
				return allocator.Cell{}, false, nil
			}
			return cellFromEngine(key, scanner.Value()), true, nil
		}
		if err := scanner.Advance(); err != nil {
			return allocator.Cell{}, false, err
		}
	}
	return allocator.Cell{}, false, nil
}

func exactRowRange(row []byte) iterrt.Range {
	return iterrt.Range{
		Start:          &wire.Key{Row: append([]byte(nil), row...), Timestamp: math.MaxInt64, Deleted: true},
		StartInclusive: true,
		End:            &wire.Key{Row: append(append([]byte(nil), row...), 0), Timestamp: math.MaxInt64, Deleted: true},
		EndInclusive:   false,
	}
}

func cellFromEngine(key *wire.Key, value []byte) allocator.Cell {
	return allocator.Cell{
		Coordinate: allocator.Coordinate{
			Row:        append([]byte(nil), key.Row...),
			Family:     append([]byte(nil), key.ColumnFamily...),
			Qualifier:  append([]byte(nil), key.ColumnQualifier...),
			Visibility: append([]byte(nil), key.ColumnVisibility...),
		},
		Value: append([]byte(nil), value...), Timestamp: key.Timestamp,
	}
}

func coordinateIdentity(row, family, qualifier, visibility []byte) string {
	var value []byte
	for _, component := range [][]byte{row, family, qualifier, visibility} {
		value = append(value, byte(len(component)>>24), byte(len(component)>>16), byte(len(component)>>8), byte(len(component)))
		value = append(value, component...)
	}
	return string(value)
}

func prefixSuccessor(value []byte) ([]byte, bool) {
	result := append([]byte(nil), value...)
	for index := len(result) - 1; index >= 0; index-- {
		if result[index] != 0xff {
			result[index]++
			return result[:index+1], true
		}
	}
	return nil, false
}

var _ allocator.Store = (*EngineStore)(nil)
