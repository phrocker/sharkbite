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
	"errors"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

func (c *Client) CreateLease(ctx context.Context, request CreateLeaseRequest) (Lease, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Lease{}, err
	}
	if err := c.validateLeaseWindow(now, request.ExpiresAt); err != nil {
		return Lease{}, err
	}
	if err := request.Owner.Validate(); err != nil {
		return Lease{}, err
	}
	if err := request.Fence.Validate(); err != nil {
		return Lease{}, err
	}
	_, head, err := c.liveAuthority(ctx, now, request.AuthorityGeneration)
	if err != nil {
		return Lease{}, err
	}
	if request.Frontier < head.HistoryFloor || request.Frontier > head.Frontier {
		return Lease{}, ErrExpired
	}
	if request.RetentionGeneration != head.RetentionGeneration {
		return Lease{}, ErrStaleRetention
	}
	id, err := c.leaseIDs.NewLeaseID(ctx, coordination.DomainID(c.domain), request.Owner)
	if err != nil {
		return Lease{}, classify(err)
	}
	if err := id.Validate(); err != nil {
		return Lease{}, err
	}
	record := coordination.SnapshotLeaseV3{
		LeaseID: append(coordination.LeaseID(nil), id...), Frontier: request.Frontier, Owner: append(coordination.OwnerID(nil), request.Owner...),
		Fence: request.Fence, AuthorityGeneration: request.AuthorityGeneration, RetentionGeneration: request.RetentionGeneration,
		PolicyGeneration: request.PolicyGeneration, PolicyCopyPinDigest: request.PolicyCopyPinDigest,
		PolicyCopyPins: clonePolicyCopyPins(request.PolicyCopyPins), IndexPins: clonePins(request.IndexPins),
		CreatedAt: now, RenewedAt: now, ExpiresAt: request.ExpiresAt, State: coordination.LeaseStateActive,
	}
	if err := record.Validate(); err != nil {
		return Lease{}, err
	}
	if c.pins == nil {
		return Lease{}, ErrUnavailable
	}
	if err := c.pins.VerifySnapshotPins(ctx, coordination.DomainID(c.domain), record); err != nil {
		return Lease{}, safeError("control: pin verification", err)
	}
	value := Lease{Record: record, RecordGeneration: 1, UpdatedAt: now}
	encoded, err := marshalLease(value)
	if err != nil {
		return Lease{}, err
	}
	coordinate, err := c.leaseCoordinate(id)
	if err != nil {
		return Lease{}, err
	}
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{absent(coordinate)}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: 1}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		existing, readErr := c.Lease(ctx, id)
		if errors.Is(readErr, ErrNotFound) {
			return false, nil
		}
		if readErr != nil {
			return false, readErr
		}
		return bytes.Equal(existing.Record.Owner, record.Owner) && leaseEqual(existing, value), nil
	})
	if err != nil {
		return Lease{}, err
	}
	return cloneLease(value), nil
}

func (c *Client) Lease(ctx context.Context, id coordination.LeaseID) (Lease, error) {
	coordinate, err := c.leaseCoordinate(id)
	if err != nil {
		return Lease{}, err
	}
	cells, err := c.read(ctx, coordinate)
	if err != nil {
		return Lease{}, err
	}
	cell, found := findCell(cells, coordinate)
	if !found {
		return Lease{}, ErrNotFound
	}
	value, err := unmarshalLease(cell.Value)
	if err != nil || cell.Timestamp != int64(value.RecordGeneration) || !bytes.Equal(value.Record.LeaseID, id) {
		return Lease{}, ErrCorruption
	}
	return cloneLease(value), nil
}

func (c *Client) RenewLease(ctx context.Context, request RenewLeaseRequest) (Lease, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Lease{}, err
	}
	if err := c.validateLeaseWindow(now, request.ExpiresAt); err != nil {
		return Lease{}, err
	}
	current, err := c.Lease(ctx, request.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	if current.Record.State != coordination.LeaseStateActive || !now.Before(current.Record.ExpiresAt) {
		return Lease{}, ErrExpired
	}
	if !bytes.Equal(current.Record.Owner, request.Owner) || current.Record.Fence != request.Fence {
		return Lease{}, ErrStaleOwner
	}
	if current.RecordGeneration != request.RecordGeneration {
		return Lease{}, ErrConflict
	}
	if !request.ExpiresAt.After(current.Record.ExpiresAt) {
		return Lease{}, ErrBounds
	}
	_, head, err := c.liveAuthority(ctx, now, request.AuthorityGeneration)
	if err != nil {
		return Lease{}, err
	}
	if request.RetentionGeneration != head.RetentionGeneration {
		return Lease{}, ErrStaleRetention
	}
	if request.RetentionGeneration != current.Record.RetentionGeneration {
		return Lease{}, ErrStaleRetention
	}
	next := cloneLease(current)
	next.RecordGeneration, err = increment(current.RecordGeneration)
	if err != nil {
		return Lease{}, err
	}
	next.Record.AuthorityGeneration = request.AuthorityGeneration
	next.Record.RenewedAt = now
	next.Record.ExpiresAt = request.ExpiresAt
	next.UpdatedAt = now
	if c.pins == nil {
		return Lease{}, ErrUnavailable
	}
	if err := c.pins.VerifySnapshotPins(ctx, coordination.DomainID(c.domain), next.Record); err != nil {
		return Lease{}, safeError("control: pin verification", err)
	}
	return c.replaceLease(ctx, current, next)
}

func (c *Client) ReleaseLease(ctx context.Context, request ReleaseLeaseRequest) (Lease, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Lease{}, err
	}
	current, err := c.Lease(ctx, request.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	if current.Record.State != coordination.LeaseStateActive {
		return Lease{}, ErrExpired
	}
	if !bytes.Equal(current.Record.Owner, request.Owner) || current.Record.Fence != request.Fence {
		return Lease{}, ErrStaleOwner
	}
	if current.RecordGeneration != request.RecordGeneration {
		return Lease{}, ErrConflict
	}
	next := cloneLease(current)
	next.RecordGeneration, err = increment(current.RecordGeneration)
	if err != nil {
		return Lease{}, err
	}
	if now.Before(current.Record.RenewedAt) {
		return Lease{}, ErrBounds
	}
	next.Record.RenewedAt = now
	next.Record.State = coordination.LeaseStateReleased
	next.UpdatedAt = now
	return c.replaceLease(ctx, current, next)
}

func (c *Client) ExpireLease(ctx context.Context, id coordination.LeaseID, now time.Time) (Lease, error) {
	now, err := requestNow(now, c.now)
	if err != nil {
		return Lease{}, err
	}
	current, err := c.Lease(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	if current.Record.State != coordination.LeaseStateActive {
		return current, nil
	}
	if now.Before(current.Record.ExpiresAt) {
		return Lease{}, ErrLeaseActive
	}
	next := cloneLease(current)
	next.RecordGeneration, err = increment(current.RecordGeneration)
	if err != nil {
		return Lease{}, err
	}
	next.Record.State = coordination.LeaseStateExpired
	next.UpdatedAt = now
	return c.replaceLease(ctx, current, next)
}

func (c *Client) TakeoverExpiredLease(ctx context.Context, request TakeoverLeaseRequest) (Lease, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Lease{}, err
	}
	if err := c.validateLeaseWindow(now, request.ExpiresAt); err != nil {
		return Lease{}, err
	}
	current, err := c.Lease(ctx, request.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	if current.RecordGeneration != request.PreviousGeneration {
		return Lease{}, ErrConflict
	}
	if current.Record.State != coordination.LeaseStateActive || now.Before(current.Record.ExpiresAt) {
		return Lease{}, ErrLeaseActive
	}
	if request.Owner.Validate() != nil || request.Fence.Validate() != nil ||
		request.Fence <= current.Record.Fence {
		return Lease{}, ErrBounds
	}
	_, head, err := c.liveAuthority(ctx, now, request.AuthorityGeneration)
	if err != nil {
		return Lease{}, err
	}
	if request.RetentionGeneration != head.RetentionGeneration ||
		request.RetentionGeneration != current.Record.RetentionGeneration {
		return Lease{}, ErrStaleRetention
	}
	next := cloneLease(current)
	next.RecordGeneration, err = increment(current.RecordGeneration)
	if err != nil {
		return Lease{}, err
	}
	next.Record.Owner = append(coordination.OwnerID(nil), request.Owner...)
	next.Record.Fence = request.Fence
	next.Record.AuthorityGeneration = request.AuthorityGeneration
	next.Record.RenewedAt = now
	next.Record.ExpiresAt = request.ExpiresAt
	next.UpdatedAt = now
	if c.pins == nil {
		return Lease{}, ErrUnavailable
	}
	if err := c.pins.VerifySnapshotPins(ctx, coordination.DomainID(c.domain), next.Record); err != nil {
		return Lease{}, safeError("control: pin verification", err)
	}
	return c.replaceTakenOverLease(ctx, current, next)
}

func (c *Client) replaceTakenOverLease(ctx context.Context, current, next Lease) (Lease, error) {
	if current.Record.Frontier != next.Record.Frontier ||
		current.Record.PolicyGeneration != next.Record.PolicyGeneration ||
		current.Record.PolicyCopyPinDigest != next.Record.PolicyCopyPinDigest ||
		!equalPolicyCopyPins(current.Record.PolicyCopyPins, next.Record.PolicyCopyPins) ||
		!equalPins(current.Record.IndexPins, next.Record.IndexPins) ||
		!bytes.Equal(current.Record.LeaseID, next.Record.LeaseID) ||
		!next.Record.ExpiresAt.After(current.Record.ExpiresAt) {
		return Lease{}, ErrConflict
	}
	encoded, err := marshalLease(next)
	if err != nil {
		return Lease{}, err
	}
	old, _ := marshalLease(current)
	coordinate, _ := c.leaseCoordinate(current.Record.LeaseID)
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{
		Coordinate: coordinate, Value: old, Timestamp: int64(current.RecordGeneration), TimestampSet: true,
	}}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: int64(next.RecordGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, readErr := c.Lease(ctx, current.Record.LeaseID)
		if readErr != nil {
			return false, readErr
		}
		return leaseEqual(got, next), nil
	})
	if err != nil {
		return Lease{}, err
	}
	return cloneLease(next), nil
}

func (c *Client) replaceLease(ctx context.Context, current, next Lease) (Lease, error) {
	if err := coordination.ValidateSnapshotLeaseTransition(current.Record, next.Record); err != nil {
		return Lease{}, err
	}
	encoded, err := marshalLease(next)
	if err != nil {
		return Lease{}, err
	}
	coordinate, err := c.leaseCoordinate(current.Record.LeaseID)
	if err != nil {
		return Lease{}, err
	}
	old, err := marshalLease(current)
	if err != nil {
		return Lease{}, err
	}
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{Coordinate: coordinate, Value: old, Timestamp: int64(current.RecordGeneration), TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: int64(next.RecordGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		value, readErr := c.Lease(ctx, current.Record.LeaseID)
		if readErr != nil {
			return false, readErr
		}
		return leaseEqual(value, next), nil
	})
	if err != nil {
		return Lease{}, err
	}
	return cloneLease(next), nil
}

func (c *Client) ListLeases(ctx context.Context, now time.Time, cursor LeaseCursor, limit int, activeOnly bool) ([]Lease, LeaseCursor, error) {
	values, next, _, err := c.listLeasesPage(ctx, now, cursor, limit, activeOnly, c.maxScan)
	return values, next, err
}

func (c *Client) listLeasesPage(
	ctx context.Context,
	now time.Time,
	cursor LeaseCursor,
	limit int,
	activeOnly bool,
	scanBudget int,
) ([]Lease, LeaseCursor, int, error) {
	now, err := requestNow(now, c.now)
	if err != nil {
		return nil, LeaseCursor{}, 0, err
	}
	if limit < 1 || limit > c.maxScan || scanBudget < 1 || scanBudget > c.maxScan {
		return nil, LeaseCursor{}, 0, ErrBounds
	}
	if len(cursor.Row) != 0 &&
		!bytes.HasPrefix(cursor.Row, leaseBandPrefix(cursor.Band, coordination.DomainID(c.domain))) {
		return nil, LeaseCursor{}, 0, ErrBounds
	}
	result := make([]Lease, 0, limit)
	band := cursor.Band
	start := append([]byte(nil), cursor.Row...)
	scanned := 0
	for ; ; band++ {
		prefix := leaseBandPrefix(band, coordination.DomainID(c.domain))
		if len(start) == 0 || !bytes.HasPrefix(start, prefix) {
			start = prefix
		}
		fetch := limit - len(result) + 1
		if remaining := scanBudget - scanned; fetch > remaining {
			fetch = remaining
		}
		cells, scanErr := c.store.ScanPrefixFrom(ctx, prefix, start, familyLease, qualifierLease, c.visibility, fetch)
		if scanErr != nil {
			return nil, LeaseCursor{}, scanned, classify(scanErr)
		}
		scanned += len(cells)
		for _, cell := range cells {
			value, decodeErr := unmarshalLease(cell.Value)
			if decodeErr != nil || cell.Timestamp != int64(value.RecordGeneration) {
				return nil, LeaseCursor{}, scanned, ErrCorruption
			}
			if activeOnly {
				active, e := value.Record.ActiveAt(now)
				if e != nil {
					return nil, LeaseCursor{}, scanned, ErrCorruption
				}
				if !active {
					continue
				}
			}
			result = append(result, cloneLease(value))
			if len(result) == limit {
				sortLeases(result)
				return result, afterLeaseCell(band, cell), scanned, nil
			}
		}
		if len(cells) == fetch {
			sortLeases(result)
			return result, afterLeaseCell(band, cells[len(cells)-1]), scanned, nil
		}
		if band == 255 {
			break
		}
		start = nil
	}
	sortLeases(result)
	return result, LeaseCursor{}, scanned, nil
}

func afterLeaseCell(band byte, cell allocator.Cell) LeaseCursor {
	return LeaseCursor{Band: band, Row: append(append([]byte(nil), cell.Coordinate.Row...), 0)}
}

func sortLeases(result []Lease) {
	sort.Slice(result, func(i, j int) bool {
		if result[i].Record.ExpiresAt.Equal(result[j].Record.ExpiresAt) {
			return bytes.Compare(result[i].Record.LeaseID, result[j].Record.LeaseID) < 0
		}
		return result[i].Record.ExpiresAt.Before(result[j].Record.ExpiresAt)
	})
}

func (c *Client) anyLease(ctx context.Context, now time.Time, predicate func(coordination.SnapshotLeaseV3) bool) (bool, error) {
	cursor := LeaseCursor{}
	scanned := 0
	for {
		remaining := c.maxScan - scanned
		if remaining == 0 {
			return false, errors.Join(ErrUnavailable, ErrBounds)
		}
		values, next, pageScanned, err := c.listLeasesPage(ctx, now, cursor, remaining, true, remaining)
		if err != nil {
			return false, err
		}
		scanned += pageScanned
		for _, value := range values {
			if predicate(value.Record) {
				return true, nil
			}
		}
		if len(next.Row) == 0 {
			return false, nil
		}
		if scanned >= c.maxScan {
			return false, errors.Join(ErrUnavailable, ErrBounds)
		}
		cursor = next
	}
}

func (c *Client) currentHead(ctx context.Context) (coordination.AllocatorHeadV1, error) {
	coordinate, err := c.headCoordinate()
	if err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	cells, err := c.read(ctx, coordinate)
	if err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	cell, found := findCell(cells, coordinate)
	if !found {
		return coordination.AllocatorHeadV1{}, ErrNotFound
	}
	return decodeHead(cell)
}

func clonePins(value []coordination.IndexPin) []coordination.IndexPin {
	result := make([]coordination.IndexPin, len(value))
	for i := range value {
		result[i] = coordination.IndexPin{Family: append(coordination.Family(nil), value[i].Family...), IGEN: append(coordination.IGEN(nil), value[i].IGEN...)}
	}
	coordination.SortIndexPins(result)
	return result
}
func clonePolicyCopyPins(value []coordination.PolicyCopyPin) []coordination.PolicyCopyPin {
	result := make([]coordination.PolicyCopyPin, len(value))
	for i := range value {
		result[i] = coordination.PolicyCopyPin{
			LPART:         append(coordination.LPART(nil), value[i].LPART...),
			MapGeneration: value[i].MapGeneration, CopyGeneration: value[i].CopyGeneration,
			VisibilityDigest: value[i].VisibilityDigest,
		}
	}
	coordination.SortPolicyCopyPins(result)
	return result
}
func cloneLease(value Lease) Lease {
	value.Record.LeaseID = append(coordination.LeaseID(nil), value.Record.LeaseID...)
	value.Record.Owner = append(coordination.OwnerID(nil), value.Record.Owner...)
	value.Record.PolicyCopyPins = clonePolicyCopyPins(value.Record.PolicyCopyPins)
	value.Record.IndexPins = clonePins(value.Record.IndexPins)
	return value
}
func leaseEqual(a, b Lease) bool {
	av, _ := marshalLease(a)
	bv, _ := marshalLease(b)
	return bytes.Equal(av, bv)
}
func equalPolicyCopyPins(a, b []coordination.PolicyCopyPin) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if coordination.ComparePolicyCopyPins(a[index], b[index]) != 0 {
			return false
		}
	}
	return true
}
func equalPins(a, b []coordination.IndexPin) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if !bytes.Equal(a[index].Family, b[index].Family) || !bytes.Equal(a[index].IGEN, b[index].IGEN) {
			return false
		}
	}
	return true
}
func leaseBandPrefix(band byte, domain coordination.DomainID) []byte {
	return append([]byte{1, 'L', band}, coordination.E(domain)...)
}
