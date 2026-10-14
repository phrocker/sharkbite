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
 */

package catalog

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type AccumuloConfig = allocator.AccumuloConfig

type AccumuloStore struct {
	inner          *allocator.AccumuloStore
	connector      *accumulo.Connector
	table          accumulo.Table
	authorizations [][]byte
	batchSize      int32
	scanner        interface {
		Stream(context.Context, *accumulo.Range) (*accumulo.ResultStream, error)
	}
}

func NewAccumuloStore(config AccumuloConfig) (*AccumuloStore, error) {
	inner, err := allocator.NewAccumuloStore(config)
	if err != nil {
		return nil, err
	}
	authorizations := make([][]byte, len(config.Authorizations))
	for i := range config.Authorizations {
		authorizations[i] = append([]byte(nil), config.Authorizations[i]...)
	}
	return &AccumuloStore{
		inner: inner, connector: config.Connector, table: config.Table,
		authorizations: authorizations, batchSize: config.ScannerBatchSize,
	}, nil
}

func (s *AccumuloStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	return s.inner.ReadExact(ctx, coordinates)
}

func (s *AccumuloStore) CompareAndMutate(
	ctx context.Context,
	request allocator.Mutation,
) (allocator.Status, error) {
	return s.inner.CompareAndMutate(ctx, request)
}

func (s *AccumuloStore) ScanPrefix(
	ctx context.Context,
	rowPrefix, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.ScanPrefixFrom(ctx, rowPrefix, rowPrefix, family, qualifier, visibility, limit)
}

func (s *AccumuloStore) ScanPrefixFrom(
	ctx context.Context,
	rowPrefix, startRow, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	if len(rowPrefix) == 0 || !bytes.HasPrefix(startRow, rowPrefix) ||
		bytes.Compare(startRow, rowPrefix) < 0 || limit < 1 || limit > MaxCatalogScan {
		return nil, ErrBounds
	}
	endRow, ok := prefixSuccessor(rowPrefix)
	if !ok {
		return nil, ErrBounds
	}
	start := &accumulo.Key{Row: append([]byte(nil), startRow...), Timestamp: math.MaxInt64}
	end := &accumulo.Key{Row: endRow, Timestamp: math.MaxInt64}
	scanRange, err := accumulo.NewKeyRange(start, true, end, false)
	if err != nil {
		return nil, err
	}
	scanner := s.scanner
	if scanner == nil {
		created, createErr := s.connector.NewScanner(s.table, accumulo.ScannerOptions{
			BatchSize: s.batchSize, Authorizations: s.authorizations,
			Columns: []accumulo.Column{accumulo.NewColumnWithVisibility(family, qualifier, visibility)},
		})
		if createErr != nil {
			return nil, createErr
		}
		scanner = created
	}
	stream, err := scanner.Stream(ctx, scanRange)
	if err != nil {
		return nil, err
	}
	values := make([]allocator.Cell, 0, limit)
	for len(values) < limit && stream.Next() {
		entry := stream.Entry()
		if !bytes.HasPrefix(entry.Key.Row, rowPrefix) ||
			!bytes.Equal(entry.Key.ColumnFamily, family) ||
			!bytes.Equal(entry.Key.ColumnQualifier, qualifier) ||
			!bytes.Equal(entry.Key.ColumnVisibility, visibility) {
			continue
		}
		values = append(values, allocator.Cell{
			Coordinate: allocator.Coordinate{
				Row: append([]byte(nil), entry.Key.Row...), Family: append([]byte(nil), entry.Key.ColumnFamily...),
				Qualifier:  append([]byte(nil), entry.Key.ColumnQualifier...),
				Visibility: append([]byte(nil), entry.Key.ColumnVisibility...),
			},
			Value: append([]byte(nil), entry.Value...), Timestamp: entry.Key.Timestamp,
		})
	}
	streamErr := stream.Err()
	closeErr := stream.Close()
	if streamErr != nil {
		return nil, errors.Join(streamErr, closeErr)
	}
	if err := usableScanCloseError(closeErr); err != nil {
		return nil, err
	}
	sort.Slice(values, func(i, j int) bool {
		return bytes.Compare(values[i].Coordinate.Row, values[j].Coordinate.Row) < 0
	})
	return values, nil
}

func usableScanCloseError(err error) error {
	var cleanup *accumulo.CleanupError
	if errors.As(err, &cleanup) {
		return nil
	}
	return err
}

func prefixSuccessor(value []byte) ([]byte, bool) {
	result := append([]byte(nil), value...)
	for i := len(result) - 1; i >= 0; i-- {
		if result[i] != 0xff {
			result[i]++
			return result[:i+1], true
		}
	}
	return nil, false
}

var _ Store = (*AccumuloStore)(nil)
