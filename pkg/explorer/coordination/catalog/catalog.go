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
	"fmt"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

var (
	familyHead       = []byte("h")
	familyCopy       = []byte("c")
	familyMap        = []byte("m")
	familyManifest   = []byte("m")
	familyDelta      = []byte("d")
	familyActivation = []byte("a")
	qualifierHead    = []byte("head")
	qualifierFence   = []byte("fence")
	qualifierRoot    = []byte("root")
	qualifierActive  = []byte("active")
	qualifierBuild   = []byte("manifest")
	qualifierDelta   = []byte("delta")
)

type Client struct {
	domain         coordination.DomainID
	visibility     []byte
	store          Store
	authority      AuthoritySource
	operations     OperationSource
	leases         LeaseSource
	policyVerifier PolicyVerifier
	indexVerifier  IndexVerifier
	clock          func() time.Time
	maxRetries     int
	retryBackoff   time.Duration
	maxScan        int
}

func New(config Config) (*Client, error) {
	if err := config.Domain.Validate(); err != nil {
		return nil, err
	}
	if config.Store == nil || config.Authority == nil || config.Operations == nil ||
		config.Leases == nil || config.PolicyVerifier == nil || config.IndexVerifier == nil {
		return nil, errors.New("catalog: store and verification sources are required")
	}
	if config.MaxRetries < 0 || config.MaxRetries > MaxConditionalRetry {
		return nil, ErrBounds
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryBackoff < 0 || config.RetryBackoff > time.Minute {
		return nil, ErrBounds
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = 10 * time.Millisecond
	}
	if config.MaxScan == 0 {
		config.MaxScan = MaxCatalogScan
	}
	if config.MaxScan < 1 || config.MaxScan > MaxCatalogScan {
		return nil, ErrBounds
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Client{
		domain: append(coordination.DomainID(nil), config.Domain...), visibility: append([]byte(nil), config.ControlVisibility...),
		store: config.Store, authority: config.Authority, operations: config.Operations, leases: config.Leases,
		policyVerifier: config.PolicyVerifier, indexVerifier: config.IndexVerifier,
		clock: config.Clock, maxRetries: config.MaxRetries, retryBackoff: config.RetryBackoff, maxScan: config.MaxScan,
	}, nil
}

func (c *Client) coordinate(row, family, qualifier []byte) allocator.Coordinate {
	return allocator.Coordinate{
		Row: append([]byte(nil), row...), Family: append([]byte(nil), family...),
		Qualifier: append([]byte(nil), qualifier...), Visibility: append([]byte(nil), c.visibility...),
	}
}

func equalCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) &&
		bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}

func (c *Client) readOne(ctx context.Context, coordinate allocator.Coordinate) (allocator.Cell, bool, error) {
	if err := ctx.Err(); err != nil {
		return allocator.Cell{}, false, err
	}
	cells, err := c.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return allocator.Cell{}, false, classifyUnavailable(err)
	}
	if len(cells) == 0 {
		return allocator.Cell{}, false, nil
	}
	if len(cells) != 1 || !equalCoordinate(cells[0].Coordinate, coordinate) {
		return allocator.Cell{}, false, ErrCorruption
	}
	return cells[0], true, nil
}

func (c *Client) currentAuthority(ctx context.Context) (Authority, error) {
	if err := ctx.Err(); err != nil {
		return Authority{}, err
	}
	value, err := c.authority.Current(ctx, c.domain)
	if err != nil {
		return Authority{}, classifyUnavailable(err)
	}
	if err := value.Generation.Validate(); err != nil {
		return Authority{}, errors.Join(ErrCorruption, err)
	}
	if err := value.RetentionGeneration.Validate(); err != nil {
		return Authority{}, errors.Join(ErrCorruption, err)
	}
	if err := value.HistoryFloor.Validate(); err != nil {
		return Authority{}, errors.Join(ErrCorruption, err)
	}
	return value, nil
}

func requireAuthority(expected coordination.Generation, retention coordination.Generation, current Authority) error {
	if expected != current.Generation {
		return ErrStaleAuthority
	}
	if retention != current.RetentionGeneration {
		return ErrStaleRetention
	}
	return nil
}

func (c *Client) absentOrIdentical(
	ctx context.Context,
	coordinate allocator.Coordinate,
	value []byte,
	timestamp int64,
) error {
	request := allocator.Mutation{
		Row:        coordinate.Row,
		Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
		Updates:    []allocator.Update{{Coordinate: coordinate, Value: append([]byte(nil), value...), Timestamp: timestamp}},
	}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, request)
		if status == allocator.StatusAccepted {
			return nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return classifyUnavailable(writeErr)
		}
		cell, found, readErr := c.readOne(ctx, coordinate)
		if readErr != nil {
			return readErr
		}
		if found {
			if bytes.Equal(cell.Value, value) && cell.Timestamp == timestamp {
				return nil
			}
			return ErrCorruption
		}
		if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return errors.Join(ErrUnavailable, ErrUnknown)
		}
		if attempt >= c.maxRetries {
			return ErrUnavailable
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) reserveGeneration(ctx context.Context, row, reservationID []byte) (coordination.Generation, error) {
	if len(reservationID) == 0 || len(reservationID) > coordination.MaxOpaqueIDBytes {
		return 0, ErrBounds
	}
	coordinate := c.coordinate(row, familyHead, qualifierHead)
	for attempt := 0; ; attempt++ {
		cell, found, err := c.readOne(ctx, coordinate)
		if err != nil {
			return 0, err
		}
		var previous coordination.Generation
		var previousValue []byte
		var previousTimestamp int64
		if found {
			previous, _, _, err = unmarshalCounter(cell.Value)
			if err != nil || cell.Timestamp != int64(previous) {
				return 0, ErrCorruption
			}
			previousValue = cell.Value
			previousTimestamp = cell.Timestamp
		}
		if previous == coordination.Generation(math.MaxInt64) {
			return 0, ErrOverflow
		}
		next := previous + 1
		value, err := marshalCounter(next, c.clock().UTC(), reservationID)
		if err != nil {
			return 0, err
		}
		condition := allocator.Condition{Coordinate: coordinate, Absent: !found}
		if found {
			condition.Value = previousValue
			condition.Timestamp = previousTimestamp
			condition.TimestampSet = true
		}
		request := allocator.Mutation{
			Row: row, Conditions: []allocator.Condition{condition},
			Updates: []allocator.Update{{Coordinate: coordinate, Value: value, Timestamp: int64(next)}},
		}
		status, writeErr := c.store.CompareAndMutate(ctx, request)
		if status == allocator.StatusAccepted {
			return next, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return 0, classifyUnavailable(writeErr)
		}
		after, afterFound, readErr := c.readOne(ctx, coordinate)
		if readErr != nil {
			return 0, readErr
		}
		if afterFound {
			generation, _, appliedID, decodeErr := unmarshalCounter(after.Value)
			if decodeErr != nil || after.Timestamp != int64(generation) {
				return 0, ErrCorruption
			}
			if errors.Is(writeErr, allocator.ErrConditionalUnknown) &&
				generation == next && bytes.Equal(appliedID, reservationID) && bytes.Equal(after.Value, value) {
				return next, nil
			}
		}
		if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return 0, errors.Join(ErrUnavailable, ErrUnknown)
		}
		if attempt >= c.maxRetries {
			return 0, ErrUnavailable
		}
		if err := c.wait(ctx); err != nil {
			return 0, err
		}
	}
}

func (c *Client) transition(
	ctx context.Context,
	coordinate allocator.Coordinate,
	beforeValue []byte,
	beforeTimestamp int64,
	afterValue []byte,
	afterTimestamp int64,
) error {
	request := allocator.Mutation{
		Row: coordinate.Row,
		Conditions: []allocator.Condition{{
			Coordinate: coordinate, Value: beforeValue, Timestamp: beforeTimestamp, TimestampSet: true,
		}},
		Updates: []allocator.Update{{Coordinate: coordinate, Value: afterValue, Timestamp: afterTimestamp}},
	}
	status, writeErr := c.store.CompareAndMutate(ctx, request)
	if status == allocator.StatusAccepted {
		return nil
	}
	if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
		return classifyUnavailable(writeErr)
	}
	cell, found, err := c.readOne(ctx, coordinate)
	if err != nil {
		return err
	}
	if found && bytes.Equal(cell.Value, afterValue) && cell.Timestamp == afterTimestamp {
		return nil
	}
	if found && bytes.Equal(cell.Value, beforeValue) && cell.Timestamp == beforeTimestamp {
		if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return errors.Join(ErrUnavailable, ErrUnknown)
		}
		return ErrConflict
	}
	if !found {
		return ErrCorruption
	}
	return ErrConflict
}

func (c *Client) wait(ctx context.Context) error {
	timer := time.NewTimer(c.retryBackoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func classifyUnavailable(err error) error {
	if err == nil {
		return ErrUnavailable
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: storage or authority operation failed", ErrUnavailable)
}
