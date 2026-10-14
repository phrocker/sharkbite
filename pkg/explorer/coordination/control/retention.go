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

func (c *Client) CurrentHistoryFloor(ctx context.Context) (coordination.HistoryFloorV1, coordination.AllocatorHeadV1, error) {
	floorCoordinate, err := c.floorCoordinate()
	if err != nil {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, err
	}
	headCoordinate, err := c.headCoordinate()
	if err != nil {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, err
	}
	cells, err := c.read(ctx, floorCoordinate, headCoordinate)
	if err != nil {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, err
	}
	floorCell, hasFloor := findCell(cells, floorCoordinate)
	headCell, hasHead := findCell(cells, headCoordinate)
	if !hasFloor && !hasHead {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, ErrNotFound
	}
	if !hasFloor || !hasHead {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	floor, err := coordination.UnmarshalHistoryFloorV1(floorCell.Value)
	if err != nil || floorCell.Timestamp != int64(floor.RetentionGeneration) {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	head, err := decodeHead(headCell)
	if err != nil {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, err
	}
	if floor.Floor != head.HistoryFloor || floor.RetentionGeneration != head.RetentionGeneration {
		return coordination.HistoryFloorV1{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	return floor, head, nil
}

func (c *Client) InitializeHistoryFloor(ctx context.Context, floor coordination.Epoch, now time.Time) (coordination.HistoryFloorV1, error) {
	now, err := requestNow(now, c.now)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	headCoordinate, _ := c.headCoordinate()
	floorCoordinate, _ := c.floorCoordinate()
	cells, err := c.read(ctx, headCoordinate, floorCoordinate)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	headCell, hasHead := findCell(cells, headCoordinate)
	_, hasFloor := findCell(cells, floorCoordinate)
	if !hasHead {
		return coordination.HistoryFloorV1{}, ErrNotFound
	}
	if hasFloor {
		value, _, readErr := c.CurrentHistoryFloor(ctx)
		if readErr != nil {
			return coordination.HistoryFloorV1{}, readErr
		}
		if value.Floor == floor {
			return value, nil
		}
		return coordination.HistoryFloorV1{}, ErrConflict
	}
	head, err := decodeHead(headCell)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	if floor != head.HistoryFloor {
		return coordination.HistoryFloorV1{}, ErrConflict
	}
	value := coordination.HistoryFloorV1{Floor: floor, RetentionGeneration: head.RetentionGeneration, AdvancedAt: now}
	value.Digest = value.ComputeDigest()
	encoded, err := coordination.MarshalHistoryFloorV1(value)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	mutation := allocator.Mutation{Row: headCoordinate.Row, Conditions: []allocator.Condition{condition(headCell), absent(floorCoordinate)}, Updates: []allocator.Update{{Coordinate: floorCoordinate, Value: encoded, Timestamp: int64(value.RetentionGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, gotHead, readErr := c.CurrentHistoryFloor(ctx)
		if readErr != nil {
			return false, readErr
		}
		return got.Digest == value.Digest && gotHead.HeadGeneration == head.HeadGeneration, nil
	})
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	return value, nil
}

func (c *Client) AdvanceHistoryFloor(ctx context.Context, expected coordination.HistoryFloorV1, proposed coordination.Epoch, proof coordination.Digest, now time.Time) (coordination.HistoryFloorV1, error) {
	now, err := requestNow(now, c.now)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	if err := proof.Validate("history-floor proof"); err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	current, head, err := c.CurrentHistoryFloor(ctx)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	if current.Digest != expected.Digest || current.RetentionGeneration != expected.RetentionGeneration {
		return coordination.HistoryFloorV1{}, ErrConflict
	}
	if proposed < current.Floor || proposed > head.Frontier {
		return coordination.HistoryFloorV1{}, ErrBounds
	}
	if proposed == current.Floor {
		return current, nil
	}
	if c.leases == nil || c.history == nil {
		return coordination.HistoryFloorV1{}, ErrUnavailable
	}
	if err := c.history.VerifyHistoryFloor(ctx, coordination.DomainID(c.domain), current, proposed, proof); err != nil {
		return coordination.HistoryFloorV1{}, safeError("control: history-floor proof", err)
	}
	if err := c.leases.NoPinsBelow(ctx, coordination.DomainID(c.domain), proposed, now); err != nil {
		if errors.Is(err, ErrLeaseActive) {
			return coordination.HistoryFloorV1{}, ErrLeaseActive
		}
		return coordination.HistoryFloorV1{}, safeError("control: lease verification", err)
	}
	nextGeneration, err := increment(current.RetentionGeneration)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	next := coordination.HistoryFloorV1{Floor: proposed, RetentionGeneration: nextGeneration, AdvancedAt: now, PredecessorDigest: current.Digest}
	next.PredecessorDigest = current.Digest
	next.Digest = next.ComputeDigest()
	if err := coordination.ValidateHistoryFloorAdvance(current, next); err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	nextHead := head
	nextHead.HeadGeneration, err = increment(head.HeadGeneration)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	nextHead.HistoryFloor = proposed
	nextHead.RetentionGeneration = nextGeneration
	floorValue, err := coordination.MarshalHistoryFloorV1(next)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	headValue, err := coordination.MarshalAllocatorHeadV1(nextHead)
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	floorCoordinate, _ := c.floorCoordinate()
	headCoordinate, _ := c.headCoordinate()
	currentFloorValue, _ := coordination.MarshalHistoryFloorV1(current)
	currentHeadValue, _ := coordination.MarshalAllocatorHeadV1(head)
	mutation := allocator.Mutation{Row: headCoordinate.Row, Conditions: []allocator.Condition{
		{Coordinate: floorCoordinate, Value: currentFloorValue, Timestamp: int64(current.RetentionGeneration), TimestampSet: true},
		{Coordinate: headCoordinate, Value: currentHeadValue, Timestamp: int64(head.HeadGeneration), TimestampSet: true},
	}, Updates: []allocator.Update{
		{Coordinate: floorCoordinate, Value: floorValue, Timestamp: int64(next.RetentionGeneration)},
		{Coordinate: headCoordinate, Value: headValue, Timestamp: int64(nextHead.HeadGeneration)},
	}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, gotHead, readErr := c.CurrentHistoryFloor(ctx)
		if readErr != nil {
			return false, readErr
		}
		return got.Digest == next.Digest && gotHead.HeadGeneration == nextHead.HeadGeneration, nil
	})
	if err != nil {
		return coordination.HistoryFloorV1{}, err
	}
	return next, nil
}

func (c *Client) PublishRetirementCandidate(ctx context.Context, request RetirementRequest) (Retirement, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Retirement{}, err
	}
	if request.Decision.State != coordination.RetirementCandidate {
		return Retirement{}, ErrConflict
	}
	if request.Decision.ObjectKind.Validate() != nil || request.Decision.ObjectID.Validate() != nil || request.Owner.Validate() != nil || request.Fence.Validate() != nil {
		return Retirement{}, ErrBounds
	}
	_, head, err := c.liveAuthority(ctx, now, request.Decision.AuthorityGeneration)
	if err != nil {
		return Retirement{}, err
	}
	if request.RetentionGeneration != head.RetentionGeneration || request.Decision.HistoryFloor != head.HistoryFloor {
		return Retirement{}, ErrStaleRetention
	}
	if c.retirements == nil {
		return Retirement{}, ErrUnavailable
	}
	if err := c.retirements.VerifyRetirement(ctx, coordination.DomainID(c.domain), request.Decision); err != nil {
		return Retirement{}, safeError("control: retirement verification", err)
	}
	value := Retirement{Decision: request.Decision, Owner: append(coordination.OwnerID(nil), request.Owner...), Fence: request.Fence, RetentionGeneration: request.RetentionGeneration, RecordGeneration: 1, UpdatedAt: now}
	encoded, err := marshalRetirement(value)
	if err != nil {
		return Retirement{}, err
	}
	coordinate, err := c.retirementCoordinate(request.Decision.ObjectKind, request.Decision.ObjectID)
	if err != nil {
		return Retirement{}, err
	}
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{absent(coordinate)}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: 1}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, e := c.Retirement(ctx, request.Decision.ObjectKind, request.Decision.ObjectID)
		if e != nil {
			return false, e
		}
		return retirementEqual(got, value), nil
	})
	if err != nil {
		return Retirement{}, err
	}
	return cloneRetirement(value), nil
}

func (c *Client) Retirement(ctx context.Context, kind coordination.EntityKind, id coordination.EntityID) (Retirement, error) {
	coordinate, err := c.retirementCoordinate(kind, id)
	if err != nil {
		return Retirement{}, err
	}
	cells, err := c.read(ctx, coordinate)
	if err != nil {
		return Retirement{}, err
	}
	cell, found := findCell(cells, coordinate)
	if !found {
		return Retirement{}, ErrNotFound
	}
	value, err := unmarshalRetirement(cell.Value)
	if err != nil || cell.Timestamp != int64(value.RecordGeneration) || !bytes.Equal(value.Decision.ObjectKind, kind) || !bytes.Equal(value.Decision.ObjectID, id) {
		return Retirement{}, ErrCorruption
	}
	return cloneRetirement(value), nil
}

func (c *Client) ApproveRetirement(ctx context.Context, request RetirementTransition) (Retirement, error) {
	return c.transitionRetirement(ctx, request, coordination.RetirementApproved, false)
}
func (c *Client) ApplyRetirement(ctx context.Context, request RetirementTransition, deletePhysical bool) (Retirement, error) {
	return c.transitionRetirement(ctx, request, coordination.RetirementApplied, deletePhysical)
}
func (c *Client) transitionRetirement(ctx context.Context, request RetirementTransition, state coordination.RetirementState, deletePhysical bool) (Retirement, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Retirement{}, err
	}
	current, err := c.Retirement(ctx, request.Kind, request.ID)
	if err != nil {
		return Retirement{}, err
	}
	if state == coordination.RetirementApplied && current.Decision.State == coordination.RetirementApplied {
		if deletePhysical {
			if c.deleter == nil {
				return Retirement{}, ErrUnavailable
			}
			if err := c.deleter.DeleteRetired(ctx, coordination.DomainID(c.domain), current.Decision); err != nil {
				return Retirement{}, classify(err)
			}
		}
		return current, nil
	}
	if !bytes.Equal(current.Owner, request.Owner) || current.Fence != request.Fence {
		return Retirement{}, ErrStaleOwner
	}
	if current.RecordGeneration != request.RecordGeneration {
		return Retirement{}, ErrConflict
	}
	_, head, err := c.liveAuthority(ctx, now, request.AuthorityGeneration)
	if err != nil {
		return Retirement{}, err
	}
	if current.Decision.AuthorityGeneration > request.AuthorityGeneration {
		return Retirement{}, ErrStaleAuthority
	}
	if request.RetentionGeneration != head.RetentionGeneration || request.HistoryFloor != head.HistoryFloor {
		return Retirement{}, ErrStaleRetention
	}
	if c.retirements == nil || c.leases == nil {
		return Retirement{}, ErrUnavailable
	}
	if err := c.retirements.VerifyRetirement(ctx, coordination.DomainID(c.domain), current.Decision); err != nil {
		return Retirement{}, safeError("control: retirement verification", err)
	}
	selected, err := c.leases.SelectsObject(ctx, coordination.DomainID(c.domain), current.Decision.ObjectKind, current.Decision.ObjectID, current.Decision.ObjectGeneration, now)
	if err != nil {
		return Retirement{}, safeError("control: lease verification", err)
	}
	if selected {
		return Retirement{}, ErrLeaseActive
	}
	if state == coordination.RetirementApproved {
		if current.Decision.State != coordination.RetirementCandidate || head.HistoryFloor != current.Decision.HistoryFloor {
			return Retirement{}, ErrConflict
		}
		if head.Frontier < current.Decision.SafeAfterFrontier || now.Before(current.Decision.SafeAfterTime) {
			return Retirement{}, ErrLeaseActive
		}
	} else {
		if current.Decision.State != coordination.RetirementApproved {
			return Retirement{}, ErrConflict
		}
		if head.HistoryFloor <= current.Decision.SafeAfterFrontier {
			return Retirement{}, ErrLeaseActive
		}
		if now.Before(current.Decision.SafeAfterTime.Add(c.retirementGrace)) {
			return Retirement{}, ErrLeaseActive
		}
	}
	next := cloneRetirement(current)
	next.RecordGeneration, err = increment(current.RecordGeneration)
	if err != nil {
		return Retirement{}, err
	}
	next.UpdatedAt = now
	next.Decision.State = state
	next.Decision.AuthorityGeneration = request.AuthorityGeneration
	next.RetentionGeneration = request.RetentionGeneration
	result, err := c.replaceRetirement(ctx, current, next)
	if err != nil {
		return Retirement{}, err
	}
	if state == coordination.RetirementApplied && deletePhysical {
		if c.deleter == nil {
			return Retirement{}, ErrUnavailable
		}
		if err := c.deleter.DeleteRetired(ctx, coordination.DomainID(c.domain), result.Decision); err != nil {
			return Retirement{}, classify(err)
		}
	}
	return result, nil
}

func (c *Client) replaceRetirement(ctx context.Context, current, next Retirement) (Retirement, error) {
	if err := coordination.ValidateRetirementTransition(current.Decision, next.Decision); err != nil {
		return Retirement{}, err
	}
	old, _ := marshalRetirement(current)
	encoded, err := marshalRetirement(next)
	if err != nil {
		return Retirement{}, err
	}
	coordinate, _ := c.retirementCoordinate(current.Decision.ObjectKind, current.Decision.ObjectID)
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{Coordinate: coordinate, Value: old, Timestamp: int64(current.RecordGeneration), TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: int64(next.RecordGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, e := c.Retirement(ctx, current.Decision.ObjectKind, current.Decision.ObjectID)
		if e != nil {
			return false, e
		}
		return retirementEqual(got, next), nil
	})
	if err != nil {
		return Retirement{}, err
	}
	return cloneRetirement(next), nil
}

func (c *Client) ListPendingRetirements(ctx context.Context, after []byte, limit int) ([]Retirement, []byte, error) {
	if limit < 1 || limit > c.maxScan {
		return nil, nil, ErrBounds
	}
	var values []Retirement
	startBand := 0
	if len(after) != 0 {
		if len(after) < 3 || after[0] != 1 || after[1] != 'R' ||
			!bytes.HasPrefix(after, retirementBandPrefix(after[2], coordination.DomainID(c.domain))) {
			return nil, nil, ErrBounds
		}
		startBand = int(after[2])
	}
	scanned := 0
	for band := startBand; band < 256; band++ {
		prefix := retirementBandPrefix(byte(band), coordination.DomainID(c.domain))
		start := prefix
		if len(after) > 0 && bytes.HasPrefix(after, prefix) {
			start = after
		}
		fetch := limit - len(values) + 1
		if remaining := c.maxScan - scanned; fetch > remaining {
			fetch = remaining
		}
		cells, err := c.store.ScanPrefixFrom(ctx, prefix, start, familyRetirement, qualifierRetirement, c.visibility, fetch)
		if err != nil {
			return nil, nil, classify(err)
		}
		scanned += len(cells)
		for _, cell := range cells {
			value, e := unmarshalRetirement(cell.Value)
			if e != nil || cell.Timestamp != int64(value.RecordGeneration) {
				return nil, nil, ErrCorruption
			}
			if value.Decision.State == coordination.RetirementCandidate || value.Decision.State == coordination.RetirementApproved {
				values = append(values, cloneRetirement(value))
			}
			if len(values) == limit {
				sortRetirements(values)
				return values, append(append([]byte(nil), cell.Coordinate.Row...), 0), nil
			}
		}
		if len(cells) == fetch {
			sortRetirements(values)
			return values, append(append([]byte(nil), cells[len(cells)-1].Coordinate.Row...), 0), nil
		}
		after = nil
	}
	sortRetirements(values)
	return values, nil, nil
}

func sortRetirements(values []Retirement) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return bytes.Compare(values[i].Decision.ObjectID, values[j].Decision.ObjectID) < 0
		}
		return values[i].UpdatedAt.Before(values[j].UpdatedAt)
	})
}

func cloneRetirement(v Retirement) Retirement {
	v.Owner = append(coordination.OwnerID(nil), v.Owner...)
	v.Decision.ObjectKind = append(coordination.EntityKind(nil), v.Decision.ObjectKind...)
	v.Decision.ObjectID = append(coordination.EntityID(nil), v.Decision.ObjectID...)
	return v
}
func retirementEqual(a, b Retirement) bool {
	av, _ := marshalRetirement(a)
	bv, _ := marshalRetirement(b)
	return bytes.Equal(av, bv)
}
func retirementBandPrefix(band byte, domain coordination.DomainID) []byte {
	return append([]byte{1, 'R', band}, coordination.E(domain)...)
}
