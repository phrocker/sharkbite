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

package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

var (
	familyHead          = []byte("q")
	qualifierHead       = []byte("head")
	qualifierFloor      = []byte("history-floor")
	qualifierAuthority  = []byte("writer-authority")
	familyLease         = []byte("l")
	qualifierLease      = []byte("lease")
	familyRetirement    = []byte("r")
	qualifierRetirement = []byte("decision")
	familyBackend       = []byte("b")
)

type Client struct {
	domain, visibility                         []byte
	store                                      Store
	pins                                       PinVerifier
	leases                                     RetentionLeaseVerifier
	history                                    HistoryFloorVerifier
	retirements                                RetirementVerifier
	deleter                                    TrustedDeleter
	migration                                  MigrationVerifier
	route                                      Route
	leaseIDs                                   LeaseIDGenerator
	terms                                      TermGenerator
	embedded, accumulo                         []byte
	clock                                      func() time.Time
	maxRetries, maxScan                        int
	retryBackoff, maxLeaseTTL, retirementGrace time.Duration
}

type randomIDs struct{}

func (randomIDs) NewLeaseID(_ context.Context, _ coordination.DomainID, _ coordination.OwnerID) (coordination.LeaseID, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return coordination.LeaseID(id[:]), nil
}
func (randomIDs) NewAuthorityTerm(_ context.Context, _ coordination.DomainID, _ coordination.OwnerID) (coordination.AuthorityTerm, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return coordination.AuthorityTerm(id[:]), nil
}

func New(config Config) (*Client, error) {
	if err := config.Domain.Validate(); err != nil {
		return nil, err
	}
	if config.Store == nil {
		return nil, errors.New("control: store is required")
	}
	if err := config.EmbeddedBackend.Validate(); err != nil {
		return nil, err
	}
	if err := config.AccumuloBackend.Validate(); err != nil {
		return nil, err
	}
	if bytes.Equal(config.EmbeddedBackend, config.AccumuloBackend) {
		return nil, ErrConflict
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.MaxRetries < 0 || config.MaxRetries > MaxRetries {
		return nil, ErrBounds
	}
	if config.MaxScan == 0 {
		config.MaxScan = MaxScan
	}
	if config.MaxScan < 1 || config.MaxScan > MaxScan {
		return nil, ErrBounds
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = 10 * time.Millisecond
	}
	if config.RetryBackoff < 0 || config.RetryBackoff > time.Minute {
		return nil, ErrBounds
	}
	if config.MaxLeaseTTL == 0 {
		config.MaxLeaseTTL = MaxLeaseTTL
	}
	if config.MaxLeaseTTL <= 0 || config.MaxLeaseTTL > MaxLeaseTTL {
		return nil, ErrBounds
	}
	if config.RetirementGrace < 0 || config.RetirementGrace > MaxGrace {
		return nil, ErrBounds
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.LeaseIDs == nil {
		config.LeaseIDs = randomIDs{}
	}
	if config.Terms == nil {
		config.Terms = randomIDs{}
	}
	return &Client{
		domain: append([]byte(nil), config.Domain...), visibility: append([]byte(nil), config.ControlVisibility...),
		store: config.Store, pins: config.Pins, leases: config.Leases, history: config.History, retirements: config.Retirements,
		deleter: config.Deleter, migration: config.Migration, route: config.Route,
		leaseIDs: config.LeaseIDs, terms: config.Terms,
		embedded: append([]byte(nil), config.EmbeddedBackend...), accumulo: append([]byte(nil), config.AccumuloBackend...),
		clock: config.Clock, maxRetries: config.MaxRetries, maxScan: config.MaxScan,
		retryBackoff: config.RetryBackoff, maxLeaseTTL: config.MaxLeaseTTL, retirementGrace: config.RetirementGrace,
	}, nil
}

func (c *Client) now() time.Time { return c.clock().UTC() }

func (c *Client) MatchesDomain(domain coordination.DomainID) bool {
	return c != nil && bytes.Equal(c.domain, domain)
}
func requestNow(value time.Time, fallback func() time.Time) (time.Time, error) {
	if value.IsZero() {
		value = fallback()
	}
	if value.Location() != time.UTC {
		return time.Time{}, ErrBounds
	}
	return value, nil
}
func (c *Client) coordinate(row, family, qualifier []byte) allocator.Coordinate {
	return allocator.Coordinate{Row: append([]byte(nil), row...), Family: append([]byte(nil), family...),
		Qualifier: append([]byte(nil), qualifier...), Visibility: append([]byte(nil), c.visibility...)}
}
func (c *Client) allocatorRow() ([]byte, error) {
	return coordination.AllocatorRow(coordination.DomainID(c.domain))
}
func (c *Client) headCoordinate() (allocator.Coordinate, error) {
	row, err := c.allocatorRow()
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyHead, qualifierHead), nil
}
func (c *Client) floorCoordinate() (allocator.Coordinate, error) {
	row, err := c.allocatorRow()
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyHead, qualifierFloor), nil
}
func (c *Client) authorityCoordinate() (allocator.Coordinate, error) {
	row, err := c.allocatorRow()
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyHead, qualifierAuthority), nil
}
func (c *Client) observationCoordinate(backend coordination.BackendID) (allocator.Coordinate, error) {
	row, err := c.allocatorRow()
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyBackend, coordination.E(backend)), nil
}
func (c *Client) leaseCoordinate(id coordination.LeaseID) (allocator.Coordinate, error) {
	row, err := coordination.SnapshotLeaseRow(coordination.DomainID(c.domain), id)
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyLease, qualifierLease), nil
}
func (c *Client) retirementCoordinate(kind coordination.EntityKind, id coordination.EntityID) (allocator.Coordinate, error) {
	row, err := coordination.RetirementRow(coordination.DomainID(c.domain), kind, id)
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyRetirement, qualifierRetirement), nil
}

func equalCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) && bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}
func (c *Client) read(ctx context.Context, coordinates ...allocator.Coordinate) ([]allocator.Cell, error) {
	if len(coordinates) > maxExactRead {
		return nil, ErrBounds
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cells, err := c.store.ReadExact(ctx, coordinates)
	if err != nil {
		return nil, classify(err)
	}
	found := make(map[int]bool)
	for _, cell := range cells {
		match := -1
		for i, coordinate := range coordinates {
			if equalCoordinate(cell.Coordinate, coordinate) {
				match = i
				break
			}
		}
		if match < 0 || found[match] {
			return nil, ErrCorruption
		}
		found[match] = true
	}
	return cells, nil
}
func findCell(cells []allocator.Cell, coordinate allocator.Coordinate) (allocator.Cell, bool) {
	for _, cell := range cells {
		if equalCoordinate(cell.Coordinate, coordinate) {
			return cell, true
		}
	}
	return allocator.Cell{}, false
}
func condition(cell allocator.Cell) allocator.Condition {
	return allocator.Condition{Coordinate: cell.Coordinate, Value: append([]byte(nil), cell.Value...), Timestamp: cell.Timestamp, TimestampSet: true}
}
func absent(coordinate allocator.Coordinate) allocator.Condition {
	return allocator.Condition{Coordinate: coordinate, Absent: true}
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, allocator.ErrConditionalUnknown) {
		return errors.Join(ErrUnavailable, ErrUnknown, err)
	}
	return errors.Join(ErrUnavailable, err)
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
func increment(value coordination.Generation) (coordination.Generation, error) {
	if value == coordination.Generation(math.MaxInt64) {
		return 0, ErrOverflow
	}
	return value + 1, nil
}
func incrementFence(value coordination.Fence) (coordination.Fence, error) {
	if value == coordination.Fence(math.MaxInt64) {
		return 0, ErrOverflow
	}
	return value + 1, nil
}

func (c *Client) mutate(ctx context.Context, mutation allocator.Mutation, reconcile func() (bool, error)) error {
	for attempt := 0; ; attempt++ {
		status, err := c.store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return nil
		}
		if status != allocator.StatusRejected && !errors.Is(err, allocator.ErrConditionalUnknown) {
			return classify(err)
		}
		ok, readErr := reconcile()
		if readErr != nil {
			return readErr
		}
		if ok {
			return nil
		}
		if status == allocator.StatusRejected {
			return ErrConflict
		}
		if attempt >= c.maxRetries {
			return errors.Join(ErrUnavailable, ErrUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func decodeHead(cell allocator.Cell) (coordination.AllocatorHeadV1, error) {
	value, err := coordination.UnmarshalAllocatorHeadV1(cell.Value)
	if err != nil || cell.Timestamp != int64(value.HeadGeneration) {
		return coordination.AllocatorHeadV1{}, ErrCorruption
	}
	return value, nil
}

func sameHeadAuthority(head coordination.AllocatorHeadV1, authority Authority) bool {
	return head.WriterAuthorityGeneration == authority.Record.Generation && head.WriterMode == authority.Mode &&
		bytes.Equal(head.WriterHolder, authority.Record.Owner) && head.WriterFence == authority.Record.Fence
}

func (c *Client) validateLeaseWindow(now, expires time.Time) error {
	if now.Location() != time.UTC || expires.Location() != time.UTC || !expires.After(now) || expires.Sub(now) > c.maxLeaseTTL {
		return ErrBounds
	}
	return nil
}

func safeError(prefix string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", prefix, err)
}
