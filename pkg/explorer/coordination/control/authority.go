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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

func (c *Client) CurrentAuthority(ctx context.Context, now time.Time) (Authority, coordination.AllocatorHeadV1, error) {
	now, err := requestNow(now, c.now)
	if err != nil {
		return Authority{}, coordination.AllocatorHeadV1{}, err
	}

	authorityCoordinate, _ := c.authorityCoordinate()
	headCoordinate, _ := c.headCoordinate()
	cells, err := c.read(ctx, authorityCoordinate, headCoordinate)
	if err != nil {
		return Authority{}, coordination.AllocatorHeadV1{}, err
	}
	authorityCell, hasAuthority := findCell(cells, authorityCoordinate)
	headCell, hasHead := findCell(cells, headCoordinate)
	if !hasAuthority && !hasHead {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrNotFound
	}
	if !hasAuthority || !hasHead {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	authority, err := unmarshalAuthority(authorityCell.Value)
	if err != nil || authorityCell.Timestamp != int64(authority.RecordGeneration) {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	head, err := decodeHead(headCell)
	if err != nil {
		return Authority{}, coordination.AllocatorHeadV1{}, err
	}
	if !sameHeadAuthority(head, authority) {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrCorruption
	}
	return cloneAuthority(authority), head, nil
}

func (c *Client) liveAuthority(ctx context.Context, now time.Time, generation coordination.Generation) (Authority, coordination.AllocatorHeadV1, error) {
	authority, head, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return Authority{}, coordination.AllocatorHeadV1{}, err
	}
	if authority.Record.State != coordination.AuthorityActive || !now.Before(authority.Record.LeaseUntil) {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrUnavailable
	}
	if authority.Record.Generation != generation {
		return Authority{}, coordination.AllocatorHeadV1{}, ErrStaleAuthority
	}
	return authority, head, nil
}

func (c *Client) InitializeAuthority(ctx context.Context, request AuthorityRequest) (Authority, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Authority{}, err
	}
	if err := c.validateAuthorityRequest(request, now); err != nil {
		return Authority{}, err
	}
	headCoordinate, _ := c.headCoordinate()
	authorityCoordinate, _ := c.authorityCoordinate()
	cells, err := c.read(ctx, headCoordinate, authorityCoordinate)
	if err != nil {
		return Authority{}, err
	}
	headCell, hasHead := findCell(cells, headCoordinate)
	authorityCell, hasAuthority := findCell(cells, authorityCoordinate)
	if !hasHead {
		return Authority{}, ErrNotFound
	}
	if hasAuthority {
		existing, _, readErr := c.CurrentAuthority(ctx, now)
		if readErr != nil {
			return Authority{}, readErr
		}
		if bytes.Equal(existing.Record.Owner, request.Owner) && existing.Mode == request.Mode {
			return existing, nil
		}
		return Authority{}, ErrConflict
	}
	_ = authorityCell
	head, err := decodeHead(headCell)
	if err != nil {
		return Authority{}, err
	}
	term := append(coordination.AuthorityTerm(nil), request.Term...)
	if len(term) == 0 {
		term, err = c.terms.NewAuthorityTerm(ctx, coordination.DomainID(c.domain), request.Owner)
		if err != nil {
			return Authority{}, classify(err)
		}
	}
	record := coordination.WriterAuthorityV1{Term: term, Generation: head.WriterAuthorityGeneration, Owner: append(coordination.OwnerID(nil), request.Owner...), LeaseUntil: request.LeaseUntil, Fence: head.WriterFence, State: coordination.AuthorityActive}
	record.Digest = record.ComputeDigest()
	value := Authority{Record: record, Mode: request.Mode, RecordGeneration: record.Generation, UpdatedAt: now}
	if !sameHeadAuthority(head, value) {
		return Authority{}, ErrConflict
	}
	encoded, err := marshalAuthority(value)
	if err != nil {
		return Authority{}, err
	}
	mutation := allocator.Mutation{Row: headCoordinate.Row, Conditions: []allocator.Condition{condition(headCell), absent(authorityCoordinate)}, Updates: []allocator.Update{{Coordinate: authorityCoordinate, Value: encoded, Timestamp: int64(value.RecordGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, gotHead, e := c.CurrentAuthority(ctx, now)
		if e != nil {
			return false, e
		}
		return authorityEqual(got, value) && gotHead.HeadGeneration == head.HeadGeneration, nil
	})
	if err != nil {
		return Authority{}, err
	}
	return cloneAuthority(value), nil
}

func (c *Client) AcquireAuthority(ctx context.Context, request AuthorityRequest) (Authority, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Authority{}, err
	}
	if err := c.validateAuthorityRequest(request, now); err != nil {
		return Authority{}, err
	}
	current, head, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return Authority{}, err
	}
	if current.Record.State == coordination.AuthorityActive && now.Before(current.Record.LeaseUntil) {
		return Authority{}, ErrLeaseActive
	}
	nextGeneration, err := increment(current.Record.Generation)
	if err != nil {
		return Authority{}, err
	}
	nextRecordGeneration, err := increment(current.RecordGeneration)
	if err != nil {
		return Authority{}, err
	}
	nextFence, err := incrementFence(current.Record.Fence)
	if err != nil {
		return Authority{}, err
	}
	term := append(coordination.AuthorityTerm(nil), request.Term...)
	if len(term) == 0 {
		term, err = c.terms.NewAuthorityTerm(ctx, coordination.DomainID(c.domain), request.Owner)
		if err != nil {
			return Authority{}, classify(err)
		}
	}
	record := coordination.WriterAuthorityV1{Term: term, Generation: nextGeneration, Owner: append(coordination.OwnerID(nil), request.Owner...), LeaseUntil: request.LeaseUntil, Fence: nextFence, State: coordination.AuthorityActive, PredecessorDigest: current.Record.Digest}
	record.Digest = record.ComputeDigest()
	next := Authority{Record: record, Mode: request.Mode, RecordGeneration: nextRecordGeneration, UpdatedAt: now}
	if err := coordination.ValidateWriterAuthorityAcquisition(&current.Record, next.Record); err != nil {
		return Authority{}, err
	}
	return c.replaceAuthority(ctx, current, head, next)
}

func (c *Client) RenewAuthority(ctx context.Context, request AuthorityTransition) (Authority, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Authority{}, err
	}
	current, head, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return Authority{}, err
	}
	if current.Record.State != coordination.AuthorityActive || !now.Before(current.Record.LeaseUntil) {
		return Authority{}, ErrExpired
	}
	if !authorityOwnerMatches(current, request) {
		return Authority{}, ErrStaleOwner
	}
	if request.Mode != current.Mode || !request.LeaseUntil.After(current.Record.LeaseUntil) {
		return Authority{}, ErrBounds
	}
	if err := c.validateLeaseWindow(now, request.LeaseUntil); err != nil {
		return Authority{}, err
	}
	next, err := c.authoritySuccessor(current, now, current.Record.State)
	if err != nil {
		return Authority{}, err
	}
	next.Record.LeaseUntil = request.LeaseUntil
	next.Record.Digest = next.Record.ComputeDigest()
	return c.replaceAuthority(ctx, current, head, next)
}

func (c *Client) RevokeAuthority(ctx context.Context, request AuthorityTransition) (Authority, error) {
	request.TerminalState = coordination.AuthorityRevoked
	return c.terminalAuthority(ctx, request)
}
func (c *Client) SupersedeAuthority(ctx context.Context, request AuthorityTransition) (Authority, error) {
	request.TerminalState = coordination.AuthoritySuperseded
	return c.terminalAuthority(ctx, request)
}
func (c *Client) terminalAuthority(ctx context.Context, request AuthorityTransition) (Authority, error) {
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Authority{}, err
	}
	current, head, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return Authority{}, err
	}
	if current.Record.State != coordination.AuthorityActive {
		return current, nil
	}
	if !authorityOwnerMatches(current, request) {
		return Authority{}, ErrStaleOwner
	}
	next, err := c.authoritySuccessor(current, now, request.TerminalState)
	if err != nil {
		return Authority{}, err
	}
	next.Record.State = request.TerminalState
	next.Record.LeaseUntil = time.Time{}
	next.Record.Digest = next.Record.ComputeDigest()
	return c.replaceAuthority(ctx, current, head, next)
}

func (c *Client) replaceAuthority(ctx context.Context, current Authority, head coordination.AllocatorHeadV1, next Authority) (Authority, error) {
	if bytes.Equal(next.Record.Term, current.Record.Term) {
		if next.Record.Generation != current.Record.Generation ||
			next.RecordGeneration != current.RecordGeneration+1 ||
			!bytes.Equal(next.Record.Owner, current.Record.Owner) ||
			next.Record.Fence != current.Record.Fence ||
			next.Record.PredecessorDigest != current.Record.PredecessorDigest {
			return Authority{}, ErrConflict
		}
		if err := coordination.ValidateWriterAuthorityTransition(current.Record, next.Record); err != nil {
			return Authority{}, err
		}
	} else if err := coordination.ValidateWriterAuthorityAcquisition(&current.Record, next.Record); err != nil {
		return Authority{}, err
	}
	nextHead := head
	var err error
	nextHead.HeadGeneration, err = increment(head.HeadGeneration)
	if err != nil {
		return Authority{}, err
	}
	nextHead.WriterAuthorityGeneration = next.Record.Generation
	nextHead.WriterMode = next.Mode
	nextHead.WriterHolder = append(coordination.OwnerID(nil), next.Record.Owner...)
	nextHead.WriterFence = next.Record.Fence
	authorityCoordinate, _ := c.authorityCoordinate()
	headCoordinate, _ := c.headCoordinate()
	oldAuthority, _ := marshalAuthority(current)
	newAuthority, err := marshalAuthority(next)
	if err != nil {
		return Authority{}, err
	}
	oldHead, _ := coordination.MarshalAllocatorHeadV1(head)
	newHead, err := coordination.MarshalAllocatorHeadV1(nextHead)
	if err != nil {
		return Authority{}, err
	}
	mutation := allocator.Mutation{Row: headCoordinate.Row, Conditions: []allocator.Condition{
		{Coordinate: authorityCoordinate, Value: oldAuthority, Timestamp: int64(current.RecordGeneration), TimestampSet: true},
		{Coordinate: headCoordinate, Value: oldHead, Timestamp: int64(head.HeadGeneration), TimestampSet: true},
	}, Updates: []allocator.Update{
		{Coordinate: authorityCoordinate, Value: newAuthority, Timestamp: int64(next.RecordGeneration)},
		{Coordinate: headCoordinate, Value: newHead, Timestamp: int64(nextHead.HeadGeneration)},
	}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, gotHead, e := c.CurrentAuthority(ctx, next.UpdatedAt)
		if e != nil {
			return false, e
		}
		return authorityEqual(got, next) && gotHead.HeadGeneration == nextHead.HeadGeneration, nil
	})
	if err != nil {
		return Authority{}, err
	}
	return cloneAuthority(next), nil
}

func (c *Client) authoritySuccessor(current Authority, now time.Time, state coordination.AuthorityState) (Authority, error) {
	recordGeneration, err := increment(current.RecordGeneration)
	if err != nil {
		return Authority{}, err
	}
	next := cloneAuthority(current)
	next.RecordGeneration = recordGeneration
	next.Record.State = state
	next.UpdatedAt = now
	return next, nil
}

func (c *Client) PublishObservation(ctx context.Context, next Observation) (Observation, error) {
	if next.Record.Backend.Validate() != nil || next.Mode.Validate() != nil || next.Record.Validate() != nil {
		return Observation{}, ErrBounds
	}
	if !bytes.Equal(next.Record.Backend, c.embedded) && !bytes.Equal(next.Record.Backend, c.accumulo) {
		return Observation{}, ErrConflict
	}
	authority, _, err := c.CurrentAuthority(ctx, next.Record.ObservedAt)
	if err != nil {
		return Observation{}, err
	}
	if next.Record.AuthorityGeneration != authority.Record.Generation || next.Record.AuthorityFence != authority.Record.Fence || next.Mode != authority.Mode {
		return Observation{}, ErrStaleAuthority
	}
	coordinate, _ := c.observationCoordinate(next.Record.Backend)
	cells, err := c.read(ctx, coordinate)
	if err != nil {
		return Observation{}, err
	}
	cell, found := findCell(cells, coordinate)
	if !found {
		if next.RecordGeneration == 0 {
			next.RecordGeneration = 1
		}
		encoded, e := marshalObservation(next)
		if e != nil {
			return Observation{}, e
		}
		mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{absent(coordinate)}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: int64(next.RecordGeneration)}}}
		err = c.mutate(ctx, mutation, func() (bool, error) {
			got, e := c.Observation(ctx, next.Record.Backend)
			if e != nil {
				return false, e
			}
			return observationEqual(got, next), nil
		})
		if err != nil {
			return Observation{}, err
		}
		return cloneObservation(next), nil
	}
	current, e := unmarshalObservation(cell.Value)
	if e != nil || cell.Timestamp != int64(current.RecordGeneration) {
		return Observation{}, ErrCorruption
	}
	if observationEqual(current, next) {
		return current, nil
	}
	if e := coordination.ValidateBackendObservationSuccessor(current.Record, next.Record); e != nil {
		return Observation{}, ErrConflict
	}
	expected, overflow := increment(current.RecordGeneration)
	if overflow != nil {
		return Observation{}, overflow
	}
	if next.RecordGeneration == 0 {
		next.RecordGeneration = expected
	}
	if next.RecordGeneration != expected {
		return Observation{}, ErrConflict
	}
	old, _ := marshalObservation(current)
	encoded, e := marshalObservation(next)
	if e != nil {
		return Observation{}, e
	}
	mutation := allocator.Mutation{Row: coordinate.Row, Conditions: []allocator.Condition{{Coordinate: coordinate, Value: old, Timestamp: int64(current.RecordGeneration), TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coordinate, Value: encoded, Timestamp: int64(next.RecordGeneration)}}}
	err = c.mutate(ctx, mutation, func() (bool, error) {
		got, readErr := c.Observation(ctx, next.Record.Backend)
		if readErr != nil {
			return false, readErr
		}
		return observationEqual(got, next), nil
	})
	if err != nil {
		return Observation{}, err
	}
	return cloneObservation(next), nil
}

func (c *Client) Observation(ctx context.Context, backend coordination.BackendID) (Observation, error) {
	coordinate, err := c.observationCoordinate(backend)
	if err != nil {
		return Observation{}, err
	}
	cells, err := c.read(ctx, coordinate)
	if err != nil {
		return Observation{}, err
	}
	cell, found := findCell(cells, coordinate)
	if !found {
		return Observation{}, ErrNotFound
	}
	value, err := unmarshalObservation(cell.Value)
	if err != nil || cell.Timestamp != int64(value.RecordGeneration) || !bytes.Equal(value.Record.Backend, backend) {
		return Observation{}, ErrCorruption
	}
	return cloneObservation(value), nil
}

func (c *Client) RoutingBarrier(ctx context.Context, now time.Time) (RoutingDecision, error) {
	authority, head, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return RoutingDecision{}, err
	}
	embedded, err := c.Observation(ctx, coordination.BackendID(c.embedded))
	if err != nil {
		return RoutingDecision{}, ErrUnavailable
	}
	accumulo, err := c.Observation(ctx, coordination.BackendID(c.accumulo))
	if err != nil {
		return RoutingDecision{}, ErrUnavailable
	}
	result := RoutingDecision{Authority: authority, Head: head, Embedded: embedded, Accumulo: accumulo}
	if !observationAgrees(authority, embedded) || !observationAgrees(authority, accumulo) {
		return result, ErrConflict
	}
	if !backendRolesAgree(c, authority.Mode, embedded, accumulo) {
		return result, ErrConflict
	}
	if authority.Record.State != coordination.AuthorityActive || !now.Before(authority.Record.LeaseUntil) || c.route == nil {
		return result, ErrUnavailable
	}
	mode, generation, fence, open, err := c.route.Current(ctx, coordination.DomainID(c.domain))
	if err != nil {
		return result, classify(err)
	}
	if !open || mode != authority.Mode || generation != authority.Record.Generation || fence != authority.Record.Fence {
		return result, ErrUnavailable
	}
	result.Enabled = true
	return result, nil
}

func (c *Client) TransitionPrimary(ctx context.Context, request AuthorityRequest) (Authority, error) {
	if c.route == nil || c.migration == nil {
		return Authority{}, ErrUnavailable
	}
	now, err := requestNow(request.Now, c.now)
	if err != nil {
		return Authority{}, err
	}
	request.Now = now
	if err := c.route.Close(ctx, coordination.DomainID(c.domain)); err != nil {
		return Authority{}, classify(err)
	}
	current, _, err := c.CurrentAuthority(ctx, now)
	if err != nil {
		return Authority{}, err
	}
	var target Authority
	if current.Mode == request.Mode && bytes.Equal(current.Record.Owner, request.Owner) &&
		current.Record.State == coordination.AuthorityActive && now.Before(current.Record.LeaseUntil) {
		target = current
	} else {
		if current.Record.State == coordination.AuthorityActive && now.Before(current.Record.LeaseUntil) {
			transition := AuthorityTransition{Owner: current.Record.Owner, Term: current.Record.Term, Generation: current.Record.Generation, Fence: current.Record.Fence, Now: now}
			if _, err = c.SupersedeAuthority(ctx, transition); err != nil {
				return Authority{}, err
			}
		}
		if err = c.migration.DrainAndVerify(ctx, coordination.DomainID(c.domain), request.Mode, current.Record.Generation); err != nil {
			return Authority{}, safeError("control: migration verification", err)
		}
		target, err = c.AcquireAuthority(ctx, request)
		if err != nil {
			return Authority{}, err
		}
	}
	for _, backend := range []coordination.BackendID{coordination.BackendID(c.embedded), coordination.BackendID(c.accumulo)} {
		state := coordination.BackendReplica
		if bytes.Equal(backend, primaryBackend(c, target.Mode)) {
			state = coordination.BackendPrimary
		}
		observation := coordination.BackendObservationV1{Backend: backend, AuthorityGeneration: target.Record.Generation, AuthorityFence: target.Record.Fence, ObservedFrontier: 1, State: state, ObservedDigest: target.Record.Digest, ObservedAt: target.UpdatedAt}
		if existing, e := c.Observation(ctx, backend); e == nil {
			observation.ObservedFrontier = existing.Record.ObservedFrontier
			if observation.ObservedAt.Before(existing.Record.ObservedAt) {
				observation.ObservedAt = existing.Record.ObservedAt
			}
		}
		if _, err = c.PublishObservation(ctx, Observation{Record: observation, Mode: target.Mode}); err != nil {
			return Authority{}, err
		}
	}
	if err = c.route.Open(ctx, coordination.DomainID(c.domain), target.Mode, target.Record.Generation, target.Record.Fence); err != nil {
		return Authority{}, classify(err)
	}
	if _, err = c.RoutingBarrier(ctx, now); err != nil {
		return Authority{}, err
	}
	return target, nil
}

func (c *Client) validateAuthorityRequest(request AuthorityRequest, now time.Time) error {
	if request.Owner.Validate() != nil || request.Mode.Validate() != nil || request.LeaseUntil.Location() != time.UTC || !request.LeaseUntil.After(now) || request.LeaseUntil.Sub(now) > c.maxLeaseTTL {
		return ErrBounds
	}
	if len(request.Term) > 0 && request.Term.Validate() != nil {
		return ErrBounds
	}
	return nil
}
func authorityOwnerMatches(current Authority, request AuthorityTransition) bool {
	return bytes.Equal(current.Record.Owner, request.Owner) && bytes.Equal(current.Record.Term, request.Term) && current.Record.Generation == request.Generation && current.Record.Fence == request.Fence
}
func cloneAuthority(v Authority) Authority {
	v.Record.Owner = append(coordination.OwnerID(nil), v.Record.Owner...)
	v.Record.Term = append(coordination.AuthorityTerm(nil), v.Record.Term...)
	return v
}
func authorityEqual(a, b Authority) bool {
	av, _ := marshalAuthority(a)
	bv, _ := marshalAuthority(b)
	return bytes.Equal(av, bv)
}
func cloneObservation(v Observation) Observation {
	v.Record.Backend = append(coordination.BackendID(nil), v.Record.Backend...)
	return v
}
func observationEqual(a, b Observation) bool {
	av, _ := marshalObservation(a)
	bv, _ := marshalObservation(b)
	return bytes.Equal(av, bv)
}
func observationAgrees(a Authority, o Observation) bool {
	return o.Mode == a.Mode && o.Record.AuthorityGeneration == a.Record.Generation && o.Record.AuthorityFence == a.Record.Fence && o.Record.ObservedDigest == a.Record.Digest
}
func backendRolesAgree(c *Client, mode coordination.WriterMode, embedded, accumulo Observation) bool {
	if !bytes.Equal(embedded.Record.Backend, c.embedded) || !bytes.Equal(accumulo.Record.Backend, c.accumulo) {
		return false
	}
	if mode == coordination.WriterModeEmbeddedPrimary {
		return embedded.Record.State == coordination.BackendPrimary && accumulo.Record.State == coordination.BackendReplica
	}
	return accumulo.Record.State == coordination.BackendPrimary && embedded.Record.State == coordination.BackendReplica
}
func primaryBackend(c *Client, mode coordination.WriterMode) []byte {
	if mode == coordination.WriterModeEmbeddedPrimary {
		return c.embedded
	}
	return c.accumulo
}
