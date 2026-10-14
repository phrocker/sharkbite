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

package guard

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

// AccumuloConfig is trusted coordinator configuration. Public callers never
// provide table names, control visibility, scanner authorizations, or cells.
type AccumuloConfig = allocator.AccumuloConfig

// AccumuloStore maps exact guard cells to Accumulo Scanner and
// ConditionalWriter operations.
type AccumuloStore struct {
	inner *allocator.AccumuloStore
}

func NewAccumuloStore(config AccumuloConfig) (*AccumuloStore, error) {
	inner, err := allocator.NewAccumuloStore(config)
	if err != nil {
		return nil, err
	}
	return &AccumuloStore{inner: inner}, nil
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

var _ Store = (*AccumuloStore)(nil)
