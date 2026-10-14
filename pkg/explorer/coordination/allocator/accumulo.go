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

package allocator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/accumulo"
)

type AccumuloConfig struct {
	Connector        *accumulo.Connector
	Table            accumulo.Table
	Authorizations   [][]byte
	WriterOptions    accumulo.ConditionalWriterOptions
	ScannerBatchSize int32
}

type accumuloScanner interface {
	Scan(context.Context, *accumulo.Range) ([]accumulo.KeyValue, error)
}

type accumuloStreamScanner interface {
	Stream(context.Context, *accumulo.Range) (*accumulo.ResultStream, error)
}

type accumuloConditionalWriter interface {
	Write(context.Context, *accumulo.ConditionalMutation) (accumulo.ConditionalStatus, error)
}

type AccumuloStore struct {
	connector      *accumulo.Connector
	table          accumulo.Table
	authorizations [][]byte
	batchSize      int32
	scanner        accumuloScanner
	writer         accumuloConditionalWriter
}

func NewAccumuloStore(config AccumuloConfig) (*AccumuloStore, error) {
	if config.Connector == nil {
		return nil, errors.New("allocator: Accumulo connector is required")
	}
	authorizations := make([][]byte, len(config.Authorizations))
	for i := range config.Authorizations {
		authorizations[i] = append([]byte(nil), config.Authorizations[i]...)
	}
	writerOptions := trustedConditionalWriterOptions(config.WriterOptions, authorizations)
	writer, err := config.Connector.NewConditionalWriter(config.Table, writerOptions)
	if err != nil {
		return nil, err
	}

	return &AccumuloStore{
		connector: config.Connector, table: config.Table, authorizations: authorizations,
		batchSize: config.ScannerBatchSize, writer: writer,
	}, nil
}

func trustedConditionalWriterOptions(
	options accumulo.ConditionalWriterOptions,
	authorizations [][]byte,
) accumulo.ConditionalWriterOptions {
	options.Authorizations = make([][]byte, len(authorizations))
	for i := range authorizations {
		options.Authorizations[i] = append([]byte(nil), authorizations[i]...)
	}
	return options
}

func (s *AccumuloStore) ReadExact(ctx context.Context, coordinates []Coordinate) ([]Cell, error) {
	if len(coordinates) > coordinationReadBound {
		return nil, errors.New("allocator: exact read exceeds its bound")
	}
	result := make([]Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		scanner, err := s.newScanner([]accumulo.Column{accumulo.NewColumnWithVisibility(
			coordinate.Family, coordinate.Qualifier, coordinate.Visibility,
		)})
		if err != nil {
			return nil, err
		}
		scanRange, err := accumulo.NewRangeRow(coordinate.Row)
		if err != nil {
			return nil, err
		}
		values, err := scanBounded(ctx, scanner, scanRange, 2)
		if err != nil {
			var cleanup *accumulo.CleanupError
			if !errors.As(err, &cleanup) {
				return nil, err
			}
		}
		var selected Cell
		found := false
		for _, value := range values {
			cell := Cell{
				Coordinate: Coordinate{
					Row:        append([]byte(nil), value.Key.Row...),
					Family:     append([]byte(nil), value.Key.ColumnFamily...),
					Qualifier:  append([]byte(nil), value.Key.ColumnQualifier...),
					Visibility: append([]byte(nil), value.Key.ColumnVisibility...),
				},
				Value: append([]byte(nil), value.Value...), Timestamp: value.Key.Timestamp,
			}
			if !cell.Coordinate.equal(coordinate) {
				continue
			}
			if !found || cell.Timestamp > selected.Timestamp {
				selected, found = cell, true
			} else if cell.Timestamp == selected.Timestamp && !bytes.Equal(cell.Value, selected.Value) {
				return nil, errors.New("allocator: conflicting values at one exact timestamp")
			}
		}
		if found {
			result = append(result, selected)
		}
	}
	return result, nil
}

func scanBounded(ctx context.Context, scanner accumuloScanner, scanRange *accumulo.Range, limit int) ([]accumulo.KeyValue, error) {
	streaming, ok := scanner.(accumuloStreamScanner)
	if !ok {
		values, err := scanner.Scan(ctx, scanRange)
		return values, usableScanError(err)
	}
	stream, err := streaming.Stream(ctx, scanRange)
	if err != nil {
		return nil, err
	}
	values := make([]accumulo.KeyValue, 0, limit)
	for len(values) < limit && stream.Next() {
		values = append(values, stream.Entry())
	}
	streamErr := stream.Err()
	closeErr := stream.Close()
	if streamErr != nil {
		return nil, errors.Join(streamErr, closeErr)
	}
	if err := usableScanError(closeErr); err != nil {
		return nil, err
	}
	return values, nil
}

func usableScanError(err error) error {
	var cleanup *accumulo.CleanupError
	if errors.As(err, &cleanup) {
		return nil
	}
	return err
}

const coordinationReadBound = 10_001

func (s *AccumuloStore) ScanRowPrefix(
	ctx context.Context,
	row, family, qualifierStart, visibility []byte,
	limit int,
) ([]Cell, error) {
	if limit < 1 || limit > coordinationReadBound {
		return nil, errors.New("allocator: scan limit is outside its bound")
	}

	start := &accumulo.Key{
		Row: row, ColumnFamily: family, ColumnQualifier: qualifierStart,
		ColumnVisibility: visibility, Timestamp: math.MaxInt64,
	}
	endFamily := append(append([]byte(nil), family...), 0)
	if len(family) == 1 && family[0] < 0xff {
		endFamily[0]++
		endFamily = endFamily[:1]
	}
	end := &accumulo.Key{Row: row, ColumnFamily: endFamily, Timestamp: math.MaxInt64}
	scanRange, err := accumulo.NewKeyRange(start, true, end, false)
	if err != nil {
		return nil, err
	}
	scanner, err := s.newScanner([]accumulo.Column{accumulo.NewColumnFamily(family)})
	if err != nil {
		return nil, err
	}
	values, err := scanBounded(ctx, scanner, scanRange, limit)
	if err != nil {
		var cleanup *accumulo.CleanupError
		if !errors.As(err, &cleanup) {
			return nil, err
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Key.Compare(values[j].Key) < 0 })
	result := make([]Cell, 0, limit)
	for _, value := range values {
		if !bytes.Equal(value.Key.Row, row) || !bytes.Equal(value.Key.ColumnFamily, family) ||
			!bytes.Equal(value.Key.ColumnVisibility, visibility) ||
			bytes.Compare(value.Key.ColumnQualifier, qualifierStart) < 0 {
			continue
		}
		result = append(result, Cell{
			Coordinate: Coordinate{
				Row: append([]byte(nil), value.Key.Row...), Family: append([]byte(nil), value.Key.ColumnFamily...),
				Qualifier:  append([]byte(nil), value.Key.ColumnQualifier...),
				Visibility: append([]byte(nil), value.Key.ColumnVisibility...),
			},
			Value: append([]byte(nil), value.Value...), Timestamp: value.Key.Timestamp,
		})
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

// ScanPrefix reads newest cells at one exact column across a bounded row
// prefix. It is used by coordination records whose authoritative rows are
// intentionally distributed by a hash band.
func (s *AccumuloStore) ScanPrefix(
	ctx context.Context,
	rowPrefix, family, qualifier, visibility []byte,
	limit int,
) ([]Cell, error) {
	return s.ScanPrefixFrom(ctx, rowPrefix, rowPrefix, family, qualifier, visibility, limit)
}

// ScanPrefixFrom is ScanPrefix with an inclusive row seek.
func (s *AccumuloStore) ScanPrefixFrom(
	ctx context.Context,
	rowPrefix, startRow, family, qualifier, visibility []byte,
	limit int,
) ([]Cell, error) {
	if len(rowPrefix) == 0 || !bytes.HasPrefix(startRow, rowPrefix) ||
		bytes.Compare(startRow, rowPrefix) < 0 || limit < 1 || limit > coordinationReadBound {
		return nil, errors.New("allocator: prefix scan arguments are outside their bound")
	}
	endRow, ok := prefixSuccessor(rowPrefix)
	if !ok {
		return nil, errors.New("allocator: row prefix has no bounded successor")
	}
	start := &accumulo.Key{Row: append([]byte(nil), startRow...), Timestamp: math.MaxInt64}
	end := &accumulo.Key{Row: endRow, Timestamp: math.MaxInt64}
	scanRange, err := accumulo.NewKeyRange(start, true, end, false)
	if err != nil {
		return nil, err
	}
	scanner, err := s.newScanner([]accumulo.Column{
		accumulo.NewColumnWithVisibility(family, qualifier, visibility),
	})
	if err != nil {
		return nil, err
	}
	values, err := scanBounded(ctx, scanner, scanRange, limit)
	if err != nil {
		var cleanup *accumulo.CleanupError
		if !errors.As(err, &cleanup) {
			return nil, err
		}
	}
	selected := make(map[string]Cell)
	for _, value := range values {
		if !bytes.HasPrefix(value.Key.Row, rowPrefix) ||
			!bytes.Equal(value.Key.ColumnFamily, family) ||
			!bytes.Equal(value.Key.ColumnQualifier, qualifier) ||
			!bytes.Equal(value.Key.ColumnVisibility, visibility) {
			continue
		}
		cell := Cell{
			Coordinate: Coordinate{
				Row: append([]byte(nil), value.Key.Row...), Family: append([]byte(nil), value.Key.ColumnFamily...),
				Qualifier:  append([]byte(nil), value.Key.ColumnQualifier...),
				Visibility: append([]byte(nil), value.Key.ColumnVisibility...),
			},
			Value: append([]byte(nil), value.Value...), Timestamp: value.Key.Timestamp,
		}
		key := string(cell.Coordinate.Row) + "\x00" + string(cell.Coordinate.Family) + "\x00" +
			string(cell.Coordinate.Qualifier) + "\x00" + string(cell.Coordinate.Visibility)
		previous, found := selected[key]
		if !found || cell.Timestamp > previous.Timestamp {
			selected[key] = cell
		} else if cell.Timestamp == previous.Timestamp && !bytes.Equal(cell.Value, previous.Value) {
			return nil, errors.New("allocator: conflicting values at one prefix-scan timestamp")
		}
	}
	result := make([]Cell, 0, len(selected))
	for _, cell := range selected {
		result = append(result, cell)
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Row, result[j].Coordinate.Row) < 0
	})
	return result, nil
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

func (s *AccumuloStore) newScanner(columns []accumulo.Column) (accumuloScanner, error) {
	if s.scanner != nil {
		return s.scanner, nil
	}
	return s.connector.NewScanner(s.table, accumulo.ScannerOptions{
		BatchSize: s.batchSize, Authorizations: s.authorizations, Columns: columns,
	})
}

func (s *AccumuloStore) CompareAndMutate(ctx context.Context, request Mutation) (Status, error) {
	if len(request.Row) == 0 || len(request.Conditions) == 0 || len(request.Updates) == 0 {
		return StatusUnknown, errors.New("allocator: malformed conditional mutation")
	}
	mutation, err := accumulo.NewMutation(request.Row)
	if err != nil {
		return StatusUnknown, err
	}
	for _, update := range request.Updates {
		if !bytes.Equal(update.Coordinate.Row, request.Row) {
			return StatusUnknown, errors.New("allocator: conditional update crosses rows")
		}
		if update.Delete {
			mutation.Delete(update.Coordinate.Family, update.Coordinate.Qualifier, update.Coordinate.Visibility, update.Timestamp)
		} else {
			mutation.Put(update.Coordinate.Family, update.Coordinate.Qualifier, update.Coordinate.Visibility, update.Timestamp, update.Value)
		}
	}
	conditions := make([]accumulo.Condition, len(request.Conditions))
	for i, condition := range request.Conditions {
		if !bytes.Equal(condition.Coordinate.Row, request.Row) {
			return StatusUnknown, errors.New("allocator: conditional predicate crosses rows")
		}
		if condition.Absent {
			conditions[i], err = accumulo.NewAbsentCondition(
				condition.Coordinate.Family, condition.Coordinate.Qualifier, condition.Coordinate.Visibility,
			)
		} else {
			conditions[i], err = accumulo.NewValueCondition(
				condition.Coordinate.Family, condition.Coordinate.Qualifier,
				condition.Coordinate.Visibility, condition.Value,
			)
		}
		if err != nil {
			return StatusUnknown, fmt.Errorf("allocator: invalid condition: %w", err)
		}
		if condition.TimestampSet {
			conditions[i] = conditions[i].WithTimestamp(condition.Timestamp)
		}
	}
	conditional, err := accumulo.NewConditionalMutation(mutation, conditions...)
	if err != nil {
		return StatusUnknown, err
	}
	status, err := s.writer.Write(ctx, conditional)
	switch status {
	case accumulo.ConditionalAccepted:
		return StatusAccepted, err
	case accumulo.ConditionalRejected:
		return StatusRejected, err
	default:
		if errors.Is(err, accumulo.ErrConditionalUnknown) {
			err = errors.Join(ErrConditionalUnknown, err)
		}
		return StatusUnknown, err
	}
}
