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
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

var (
	familyState      = []byte("s")
	qualifierHead    = []byte("head")
	qualifierPending = []byte("pending")
)

type Client struct {
	domain       coordination.DomainID
	visibility   []byte
	store        Store
	authority    AuthoritySource
	retirement   RetirementSource
	transactions TxnStatusSource
	reconciler   Reconciler
	clock        func() time.Time
	maxRetries   int
	retryBackoff time.Duration
}

func New(config Config) (*Client, error) {
	if err := config.Domain.Validate(); err != nil {
		return nil, err
	}
	if config.Store == nil || config.Authority == nil || config.Retirement == nil ||
		config.Transactions == nil {
		return nil, errors.New("entity guard: store and authority sources are required")
	}
	if config.MaxRetries < 0 || config.MaxRetries > 100 {
		return nil, errors.Join(ErrBounds, errors.New("entity guard: retry count is outside its bound"))
	}
	if config.RetryBackoff < 0 || config.RetryBackoff > time.Minute {
		return nil, errors.Join(ErrBounds, errors.New("entity guard: retry backoff is outside its bound"))
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = 10 * time.Millisecond
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &Client{
		domain:     append(coordination.DomainID(nil), config.Domain...),
		visibility: append([]byte(nil), config.ControlVisibility...),
		store:      config.Store, authority: config.Authority, retirement: config.Retirement,
		transactions: config.Transactions, reconciler: config.Reconciler,
		clock: config.Clock, maxRetries: config.MaxRetries, retryBackoff: config.RetryBackoff,
	}, nil
}

func (c *Client) Read(ctx context.Context, entity Entity) (*Head, *Pending, error) {
	if err := entity.validate(); err != nil {
		return nil, nil, err
	}
	row, err := coordination.EntityHeadRow(c.domain, entity.Kind, entity.ID)
	if err != nil {
		return nil, nil, err
	}
	headCoordinate := c.coordinate(row, qualifierHead)
	pendingCoordinate := c.coordinate(row, qualifierPending)
	cells, err := c.store.ReadExact(ctx, []allocator.Coordinate{headCoordinate, pendingCoordinate})
	if err != nil {
		return nil, nil, classifyUnavailable(err)
	}
	if len(cells) > 2 {
		return nil, nil, ErrCorruption
	}
	var head *Head
	var pending *Pending
	for _, cell := range cells {
		switch {
		case equalCoordinate(cell.Coordinate, headCoordinate):
			value, decodeErr := UnmarshalHead(cell.Value)
			if decodeErr != nil || cell.Timestamp != int64(value.Generation) {
				return nil, nil, fmt.Errorf("%w: invalid head record", ErrCorruption)
			}
			copy := cloneHead(value)
			head = &copy
		case equalCoordinate(cell.Coordinate, pendingCoordinate):
			value, decodeErr := UnmarshalPending(cell.Value)
			if decodeErr != nil || cell.Timestamp != int64(value.Generation) {
				return nil, nil, fmt.Errorf("%w: invalid pending record", ErrCorruption)
			}
			copy := clonePending(value)
			pending = &copy
		default:
			return nil, nil, ErrCorruption
		}
	}
	return head, pending, nil
}

func (c *Client) Acquire(ctx context.Context, intent Intent) (Acquisition, error) {
	intent = cloneIntent(intent)
	if err := intent.Validate(); err != nil {
		return Acquisition{}, err
	}
	if err := c.validateAuthorityAndRetention(ctx, intent); err != nil {
		return Acquisition{}, err
	}
	for attempt := 0; ; attempt++ {
		head, previousPending, err := c.Read(ctx, intent.Entity)
		if err != nil {
			return Acquisition{}, err
		}
		if previousPending != nil && previousPending.Active {
			if sameAcquisitionIntent(*previousPending, intent) {
				return Acquisition{
					Entity: intent.Entity, Decision: previousPending.Decision,
					Pending: clonePending(*previousPending), Head: cloneHeadPtr(head),
				}, nil
			}
			if !previousPending.Intent.LeaseUntil.After(c.clock().UTC()) {
				return Acquisition{}, ErrExpired
			}
			return Acquisition{}, ErrBusy
		}
		decision, err := decide(head, intent)
		if err != nil {
			return Acquisition{}, err
		}
		storedIntent := cloneIntent(intent)
		if decision == DecisionReuse {
			storedIntent.ExpectedEpoch = head.Epoch
			storedIntent.ExpectedDigest = head.LogicalDigest
		}
		generation, err := nextGeneration(headGeneration(head), pendingGeneration(previousPending))
		if err != nil {
			return Acquisition{}, err
		}
		pending := Pending{
			Generation: generation, UpdatedAt: c.clock().UTC(), Active: true,
			Decision: decision, Intent: storedIntent,
		}
		if err := pending.Validate(); err != nil {
			return Acquisition{}, err
		}
		request, err := c.pendingMutation(intent.Entity, head, previousPending, pending)
		if err != nil {
			return Acquisition{}, err
		}
		status, writeErr := c.store.CompareAndMutate(ctx, request)
		if status == allocator.StatusAccepted {
			return Acquisition{Entity: intent.Entity, Decision: decision, Pending: pending, Head: cloneHeadPtr(head)}, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return Acquisition{}, classifyUnavailable(writeErr)
		}
		applied, predecessor, reconcileErr := c.reconcilePending(ctx, intent.Entity, previousPending, pending)
		if reconcileErr != nil {
			return Acquisition{}, reconcileErr
		}
		if applied {
			return Acquisition{Entity: intent.Entity, Decision: decision, Pending: pending, Head: cloneHeadPtr(head)}, nil
		}
		if !predecessor || attempt >= c.maxRetries {
			if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
				return Acquisition{}, errors.Join(ErrUnavailable, ErrUnknown)
			}
			continue
		}
		if err := c.wait(ctx); err != nil {
			return Acquisition{}, err
		}
	}
}

func (c *Client) AcquireMany(ctx context.Context, intents []Intent) ([]Acquisition, error) {
	if len(intents) == 0 {
		return nil, nil
	}
	if len(intents) > MaxEntities {
		return nil, ErrBounds
	}
	type ordered struct {
		row    []byte
		intent Intent
	}
	values := make([]ordered, 0, len(intents))
	for _, intent := range intents {
		if err := intent.Validate(); err != nil {
			return nil, err
		}
		row, err := coordination.EntityHeadRow(c.domain, intent.Entity.Kind, intent.Entity.ID)
		if err != nil {
			return nil, err
		}
		values = append(values, ordered{row: row, intent: cloneIntent(intent)})
	}
	sort.Slice(values, func(i, j int) bool { return bytes.Compare(values[i].row, values[j].row) < 0 })
	deduped := values[:0]
	for _, value := range values {
		if len(deduped) != 0 && bytes.Equal(deduped[len(deduped)-1].row, value.row) {
			if !sameIntent(deduped[len(deduped)-1].intent, value.intent) {
				return nil, ErrConflict
			}
			continue
		}
		deduped = append(deduped, value)
	}
	acquired := make([]Acquisition, 0, len(deduped))
	for _, value := range deduped {
		result, err := c.Acquire(ctx, value.intent)
		if err == nil {
			acquired = append(acquired, result)
			continue
		}
		for i := len(acquired) - 1; i >= 0; i-- {
			releaseErr := c.Abort(ctx, acquired[i].Pending, false)
			if releaseErr != nil {
				return nil, errors.Join(ErrUnavailable, releaseErr)
			}
		}
		return nil, err
	}
	return acquired, nil
}

func decide(head *Head, intent Intent) (Decision, error) {
	// A head belongs to the store that created it, and no other writer may
	// adopt it, whatever mode it asks for. See ownedBy.
	if head != nil && !ownedBy(head, intent) {
		return 0, ErrConflict
	}
	switch intent.Mode {
	case ModeAppend:
		if head != nil && head.State == StateTombstone {
			return 0, ErrConflict
		}
		return DecisionAppend, nil
	case ModeAbsentOrIdentical:
		if head == nil {
			return DecisionCreate, nil
		}
		if head.State != StateLive || head.LogicalDigest != intent.DesiredDigest ||
			!bytes.Equal(head.WinnerID, intent.DesiredWinnerID) ||
			!bytes.Equal(head.LPART, intent.LPART) {
			return 0, ErrConflict
		}
		return DecisionReuse, nil
	case ModeMutate:
		if !matchesExpected(head, intent) || head.State != StateLive || intent.DesiredState != StateLive {
			return 0, ErrConflict
		}
		return DecisionMutate, nil
	case ModeRetire:
		if !matchesExpected(head, intent) || head.State != StateLive || intent.DesiredState != StateTombstone {
			return 0, ErrConflict
		}
		return DecisionRetire, nil
	default:
		return 0, ErrConflict
	}
}

// ownedBy reports whether an existing head was created by the same store as
// the intent's writer, which is what its logical policy names.
//
// The head key is (domain, kind, entity ID). It carries no table or store,
// so two stores that chose one kind letter share a key space, and an entity
// ID one of them lets a caller choose can name the other's entity. Before
// this, only ModeAbsentOrIdentical compared the owner. ModeMutate and
// ModeRetire compared only the expected epoch and digest, which a writer
// reads off the head it is about to adopt, so a writer of one store could
// mutate another store's head (the fleet-event publication guard over an
// approval's, both kind 'P'). The record it guarded was then unreadable for
// good, because its head named an epoch with no committed cell.
//
// The owner is the logical policy, and deliberately not the LPART as well.
// Every store names one logical policy, and no two stores share one
// (fleet/approval/v1, fleet/dispatch/v1, fleet/registry/v1,
// fleet-events/default, the embedded document policy). The LPART, by
// contrast, may legitimately differ between writes of one store's entity:
// fleet-event subscription receipt slots are partitioned by the subscription
// that last used them, so a reused slot is mutated under a different LPART.
// ModeAbsentOrIdentical still requires the LPART to match for a reuse, as it
// always did.
//
// The comparison reads the head's stored policy, so it needs no format change
// and moves no existing head. Pruning copies the policy from the head it
// retires.
func ownedBy(head *Head, intent Intent) bool {
	return bytes.Equal(head.LogicalPolicyID, intent.LogicalPolicyID)
}

func matchesExpected(head *Head, intent Intent) bool {
	return head != nil && intent.ExpectedEpoch != 0 && head.Epoch == intent.ExpectedEpoch &&
		head.LogicalDigest == intent.ExpectedDigest
}

func (c *Client) Renew(ctx context.Context, current Pending, leaseUntil, now time.Time) (Pending, error) {
	current = clonePending(current)
	if err := current.Validate(); err != nil {
		return Pending{}, err
	}
	if !current.Active {
		return Pending{}, ErrNotFound
	}
	if err := utc("renewal time", now); err != nil {
		return Pending{}, err
	}
	if err := utc("renewed lease", leaseUntil); err != nil {
		return Pending{}, err
	}
	if !now.Before(current.Intent.LeaseUntil) {
		return Pending{}, ErrExpired
	}
	if !leaseUntil.After(current.Intent.LeaseUntil) || !leaseUntil.After(now) {
		return Pending{}, ErrConflict
	}
	if err := c.validateAuthorityAndRetention(ctx, current.Intent); err != nil {
		return Pending{}, err
	}
	next := clonePending(current)
	generation, err := nextGeneration(current.Generation)
	if err != nil {
		return Pending{}, err
	}
	next.Generation, next.UpdatedAt, next.Intent.LeaseUntil = generation, now, leaseUntil
	return c.transitionPending(ctx, current, next)
}

type TakeoverResult struct {
	Pending    Pending
	TakenOver  bool
	Reconciled bool
}

func (c *Client) Takeover(
	ctx context.Context,
	current Pending,
	owner coordination.OwnerID,
	leaseUntil time.Time,
	fence coordination.Fence,
	now time.Time,
) (TakeoverResult, error) {
	current = clonePending(current)
	if err := current.Validate(); err != nil {
		return TakeoverResult{}, err
	}
	if c.transactions == nil {
		return TakeoverResult{}, ErrUnavailable
	}
	if err := utc("takeover time", now); err != nil {
		return TakeoverResult{}, err
	}
	if err := utc("takeover lease", leaseUntil); err != nil {
		return TakeoverResult{}, err
	}
	if !leaseUntil.After(now) {
		return TakeoverResult{}, ErrConflict
	}
	if current.Intent.LeaseUntil.After(now) {
		return TakeoverResult{}, ErrBusy
	}
	if err := owner.Validate(); err != nil {
		return TakeoverResult{}, err
	}
	if fence <= current.Intent.Fence {
		return TakeoverResult{}, ErrConflict
	}
	status, err := c.transactions.Status(ctx, c.domain, current.Intent.TXN)
	if err != nil {
		return TakeoverResult{}, classifyUnavailable(err)
	}
	switch status {
	case TxnCommitted:
		if c.reconciler == nil {
			return TakeoverResult{}, ErrUnavailable
		}
		if err := c.reconciler.ReconcileCommitted(ctx, c.domain, current.Intent.Entity, current); err != nil {
			return TakeoverResult{}, classifyUnavailable(err)
		}
		return TakeoverResult{Reconciled: true}, nil
	case TxnTerminal:
		if err := c.Abort(ctx, current, false); err != nil {
			return TakeoverResult{}, err
		}
		return TakeoverResult{Reconciled: true}, nil
	case TxnNonterminal:
	default:
		return TakeoverResult{}, ErrCorruption
	}
	next := clonePending(current)
	generation, err := nextGeneration(current.Generation)
	if err != nil {
		return TakeoverResult{}, err
	}
	next.Generation, next.UpdatedAt = generation, now
	next.Intent.Owner, next.Intent.LeaseUntil, next.Intent.Fence =
		append(coordination.OwnerID(nil), owner...), leaseUntil, fence
	value, err := c.transitionPending(ctx, current, next)
	if err != nil {
		return TakeoverResult{}, err
	}
	return TakeoverResult{Pending: value, TakenOver: true}, nil
}

func (c *Client) Prepare(ctx context.Context, current Pending, published Published, now time.Time) (Pending, error) {
	if err := c.validatePublished(current, published); err != nil {
		return Pending{}, err
	}
	if err := utc("prepare time", now); err != nil {
		return Pending{}, err
	}
	if current.Prepared {
		return clonePending(current), nil
	}
	if err := c.validateAuthorityAndRetention(ctx, current.Intent); err != nil {
		return Pending{}, err
	}
	next := clonePending(current)
	generation, err := nextGeneration(current.Generation)
	if err != nil {
		return Pending{}, err
	}
	next.Generation, next.UpdatedAt, next.Prepared = generation, now, true
	return c.transitionPending(ctx, current, next)
}

func (c *Client) Commit(ctx context.Context, current Pending, published Published, now time.Time) (Head, error) {
	current = clonePending(current)
	if !current.Prepared {
		return Head{}, ErrConflict
	}
	if err := c.validatePublished(current, published); err != nil {
		return Head{}, err
	}
	if err := utc("commit time", now); err != nil {
		return Head{}, err
	}
	if err := c.validateAuthorityAndRetention(ctx, current.Intent); err != nil {
		return Head{}, err
	}
	disposition, err := c.transactions.Status(ctx, c.domain, current.Intent.TXN)
	if err != nil {
		return Head{}, classifyUnavailable(err)
	}
	if disposition != TxnCommitted {
		return Head{}, ErrConflict
	}
	head, actual, err := c.Read(ctx, current.Intent.Entity)
	if err != nil {
		return Head{}, err
	}
	if actual == nil || !pendingEqual(*actual, current) {
		if current.Decision == DecisionReuse && head != nil &&
			reuseHeadMatchesIntent(*head, current.Intent) &&
			(actual == nil || !actual.Active) {
			return cloneHead(*head), nil
		}
		if current.Decision != DecisionReuse && head != nil &&
			headMatchesPublished(*head, published) && (actual == nil || !actual.Active) {
			return cloneHead(*head), nil
		}
		return Head{}, ErrConflict
	}
	if current.Decision == DecisionReuse {
		if head == nil || !reuseHeadMatchesIntent(*head, current.Intent) {
			return Head{}, ErrCorruption
		}
		released, err := releasedPending(current, now, head.Generation)
		if err != nil {
			return Head{}, err
		}
		request, err := c.commitMutation(current.Intent.Entity, head, current, nil, released)
		if err != nil {
			return Head{}, err
		}
		if err := c.applyCommit(ctx, request, head, current, nil, released); err != nil {
			return Head{}, err
		}
		return cloneHead(*head), nil
	}
	if head != nil && published.Epoch <= head.Epoch {
		return Head{}, ErrConflict
	}
	generation, err := nextGeneration(headGeneration(head), current.Generation)
	if err != nil {
		return Head{}, err
	}
	nextHead := Head{
		Generation: generation, UpdatedAt: now, State: published.State,
		WinnerID: append([]byte(nil), published.WinnerID...), Epoch: published.Epoch,
		TXN: append(coordination.TXN(nil), published.TXN...), LogicalDigest: published.LogicalDigest,
		LPART:                append(coordination.LPART(nil), published.LPART...),
		LogicalPolicyID:      append([]byte(nil), published.LogicalPolicyID...),
		RetirementGeneration: current.Intent.RetirementGeneration,
	}
	if err := nextHead.Validate(); err != nil {
		return Head{}, err
	}
	released, err := releasedPending(current, now, generation)
	if err != nil {
		return Head{}, err
	}
	request, err := c.commitMutation(current.Intent.Entity, head, current, &nextHead, released)
	if err != nil {
		return Head{}, err
	}
	if err := c.applyCommit(ctx, request, head, current, &nextHead, released); err != nil {
		return Head{}, err
	}
	return nextHead, nil
}

func (c *Client) Abort(ctx context.Context, current Pending, conflicted bool) error {
	current = clonePending(current)
	if err := current.Validate(); err != nil {
		return err
	}
	if !current.Active {
		return nil
	}
	head, actual, err := c.Read(ctx, current.Intent.Entity)
	if err != nil {
		return err
	}
	if actual == nil || !actual.Active {
		return nil
	}
	if !pendingEqual(*actual, current) {
		return ErrConflict
	}
	now := c.clock().UTC()
	released, err := releasedPending(current, now, headGeneration(head))
	if err != nil {
		return err
	}
	_ = conflicted
	request, err := c.pendingMutation(current.Intent.Entity, head, actual, released)
	if err != nil {
		return err
	}
	status, writeErr := c.store.CompareAndMutate(ctx, request)
	if status == allocator.StatusAccepted {
		return nil
	}
	if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
		return classifyUnavailable(writeErr)
	}
	applied, predecessor, reconcileErr := c.reconcilePending(ctx, current.Intent.Entity, actual, released)
	if reconcileErr != nil {
		return reconcileErr
	}
	if applied {
		return nil
	}
	if predecessor {
		return errors.Join(ErrUnavailable, ErrUnknown)
	}
	return ErrConflict
}

func (c *Client) validatePublished(current Pending, published Published) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if !current.Active || !bytes.Equal(current.Intent.TXN, published.TXN) ||
		current.Intent.Fence != published.Fence ||
		current.Intent.AuthorityGeneration != published.AuthorityGeneration ||
		current.Intent.DesiredDigest != published.LogicalDigest ||
		!bytes.Equal(current.Intent.LPART, published.LPART) ||
		!bytes.Equal(current.Intent.LogicalPolicyID, published.LogicalPolicyID) ||
		current.Intent.DesiredState != published.State ||
		!bytes.Equal(current.Intent.DesiredWinnerID, published.WinnerID) {
		return ErrConflict
	}
	if err := published.Epoch.Validate(); err != nil {
		return err
	}
	if published.Epoch < current.Intent.HistoryFloor {
		return ErrStaleRetention
	}
	return nil
}

func (c *Client) validateAuthorityAndRetention(ctx context.Context, intent Intent) error {
	current, err := c.authority.Current(ctx, c.domain)
	if err != nil {
		return classifyUnavailable(err)
	}
	if current.Generation != intent.AuthorityGeneration || current.Fence != intent.AuthorityFence {
		return ErrStaleAuthority
	}
	if current.RetentionGeneration != intent.RetentionGeneration ||
		current.HistoryFloor != intent.HistoryFloor ||
		intent.ExpectedEpoch != 0 && intent.ExpectedEpoch < current.HistoryFloor {
		return ErrStaleRetention
	}
	retired, generation, err := c.retirement.Retired(ctx, c.domain, intent.Entity)
	if err != nil {
		return classifyUnavailable(err)
	}
	if generation != intent.RetirementGeneration {
		return ErrStaleRetention
	}
	if retired {
		return ErrConflict
	}
	return nil
}

func (c *Client) transitionPending(ctx context.Context, previous, next Pending) (Pending, error) {
	for attempt := 0; ; attempt++ {
		head, actual, err := c.Read(ctx, previous.Intent.Entity)
		if err != nil {
			return Pending{}, err
		}
		if actual == nil || !pendingEqual(*actual, previous) {
			if actual != nil && pendingEqual(*actual, next) {
				return clonePending(next), nil
			}
			return Pending{}, ErrConflict
		}
		request, err := c.pendingMutation(previous.Intent.Entity, head, actual, next)
		if err != nil {
			return Pending{}, err
		}
		status, writeErr := c.store.CompareAndMutate(ctx, request)
		if status == allocator.StatusAccepted {
			return next, nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return Pending{}, classifyUnavailable(writeErr)
		}
		applied, predecessor, reconcileErr := c.reconcilePending(ctx, previous.Intent.Entity, &previous, next)
		if reconcileErr != nil {
			return Pending{}, reconcileErr
		}
		if applied {
			return next, nil
		}
		if !predecessor {
			return Pending{}, ErrConflict
		}
		if attempt >= c.maxRetries {
			return Pending{}, errors.Join(ErrUnavailable, ErrUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return Pending{}, err
		}
	}
}

func (c *Client) pendingMutation(
	entity Entity,
	head *Head,
	previous *Pending,
	next Pending,
) (allocator.Mutation, error) {
	row, _ := coordination.EntityHeadRow(c.domain, entity.Kind, entity.ID)
	headCoordinate, pendingCoordinate := c.coordinate(row, qualifierHead), c.coordinate(row, qualifierPending)
	conditions := []allocator.Condition{conditionForPending(pendingCoordinate, previous)}
	if head == nil {
		conditions = append(conditions, allocator.Condition{Coordinate: headCoordinate, Absent: true})
	} else {
		value, err := MarshalHead(*head)
		if err != nil {
			return allocator.Mutation{}, err
		}
		conditions = append(conditions, allocator.Condition{
			Coordinate: headCoordinate, Value: value, Timestamp: int64(head.Generation), TimestampSet: true,
		})
	}
	value, err := MarshalPending(next)
	if err != nil {
		return allocator.Mutation{}, err
	}
	return allocator.Mutation{
		Row: row, Conditions: conditions,
		Updates: []allocator.Update{{Coordinate: pendingCoordinate, Value: value, Timestamp: int64(next.Generation)}},
	}, nil
}

func (c *Client) commitMutation(
	entity Entity,
	previousHead *Head,
	previousPending Pending,
	nextHead *Head,
	nextPending Pending,
) (allocator.Mutation, error) {
	row, _ := coordination.EntityHeadRow(c.domain, entity.Kind, entity.ID)
	headCoordinate, pendingCoordinate := c.coordinate(row, qualifierHead), c.coordinate(row, qualifierPending)
	conditions := []allocator.Condition{conditionForPending(pendingCoordinate, &previousPending)}
	if previousHead == nil {
		conditions = append(conditions, allocator.Condition{Coordinate: headCoordinate, Absent: true})
	} else {
		value, err := MarshalHead(*previousHead)
		if err != nil {
			return allocator.Mutation{}, err
		}
		conditions = append(conditions, allocator.Condition{
			Coordinate: headCoordinate, Value: value,
			Timestamp: int64(previousHead.Generation), TimestampSet: true,
		})
	}
	pendingBytes, err := MarshalPending(nextPending)
	if err != nil {
		return allocator.Mutation{}, err
	}
	updates := []allocator.Update{{
		Coordinate: pendingCoordinate, Value: pendingBytes, Timestamp: int64(nextPending.Generation),
	}}
	if nextHead != nil {
		headBytes, err := MarshalHead(*nextHead)
		if err != nil {
			return allocator.Mutation{}, err
		}
		updates = append(updates, allocator.Update{
			Coordinate: headCoordinate, Value: headBytes, Timestamp: int64(nextHead.Generation),
		})
	}
	return allocator.Mutation{Row: row, Conditions: conditions, Updates: updates}, nil
}

func (c *Client) applyCommit(
	ctx context.Context,
	request allocator.Mutation,
	previousHead *Head,
	previousPending Pending,
	nextHead *Head,
	nextPending Pending,
) error {
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, request)
		if status == allocator.StatusAccepted {
			return nil
		}
		if status != allocator.StatusRejected && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return classifyUnavailable(writeErr)
		}
		head, pending, err := c.Read(ctx, previousPending.Intent.Entity)
		if err != nil {
			return err
		}
		successorHead := nextHead == nil && headEqualPtr(head, previousHead) ||
			nextHead != nil && head != nil && headEqual(*head, *nextHead)
		successorPending := pending != nil && pendingEqual(*pending, nextPending)
		if successorHead && successorPending {
			return nil
		}
		predecessorHeadMatches := headEqualPtr(head, previousHead)
		predecessorPendingMatches := pending != nil && pendingEqual(*pending, previousPending)
		if !predecessorHeadMatches || !predecessorPendingMatches {
			return ErrCorruption
		}
		if attempt >= c.maxRetries {
			return errors.Join(ErrUnavailable, ErrUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) reconcilePending(
	ctx context.Context,
	entity Entity,
	previous *Pending,
	next Pending,
) (applied bool, predecessor bool, err error) {
	_, actual, err := c.Read(ctx, entity)
	if err != nil {
		return false, false, err
	}
	if actual != nil && pendingEqual(*actual, next) {
		return true, false, nil
	}
	if previous == nil {
		return false, actual == nil, nil
	}
	return false, actual != nil && pendingEqual(*actual, *previous), nil
}

func conditionForPending(coordinate allocator.Coordinate, previous *Pending) allocator.Condition {
	if previous == nil {
		return allocator.Condition{Coordinate: coordinate, Absent: true}
	}
	value, _ := MarshalPending(*previous)
	return allocator.Condition{
		Coordinate: coordinate, Value: value,
		Timestamp: int64(previous.Generation), TimestampSet: true,
	}
}

func releasedPending(previous Pending, now time.Time, other coordination.Generation) (Pending, error) {
	generation, err := nextGeneration(previous.Generation, other)
	if err != nil {
		return Pending{}, err
	}
	return Pending{Generation: generation, UpdatedAt: now}, nil
}

func (c *Client) coordinate(row, qualifier []byte) allocator.Coordinate {
	return allocator.Coordinate{
		Row: append([]byte(nil), row...), Family: append([]byte(nil), familyState...),
		Qualifier: append([]byte(nil), qualifier...), Visibility: append([]byte(nil), c.visibility...),
	}
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
	return errors.Join(ErrUnavailable, err)
}

func equalCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) &&
		bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}

func headGeneration(head *Head) coordination.Generation {
	if head == nil {
		return 0
	}
	return head.Generation
}

func pendingGeneration(pending *Pending) coordination.Generation {
	if pending == nil {
		return 0
	}
	return pending.Generation
}

func pendingEqual(a, b Pending) bool {
	left, errLeft := MarshalPending(a)
	right, errRight := MarshalPending(b)
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

func headEqual(a, b Head) bool {
	left, errLeft := MarshalHead(a)
	right, errRight := MarshalHead(b)
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

func headEqualPtr(a, b *Head) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return headEqual(*a, *b)
}

func headMatchesPublished(head Head, published Published) bool {
	return bytes.Equal(head.TXN, published.TXN) && head.Epoch == published.Epoch &&
		head.LogicalDigest == published.LogicalDigest && head.State == published.State &&
		bytes.Equal(head.WinnerID, published.WinnerID) &&
		bytes.Equal(head.LPART, published.LPART) &&
		bytes.Equal(head.LogicalPolicyID, published.LogicalPolicyID)
}

func reuseHeadMatchesIntent(head Head, intent Intent) bool {
	return head.State == StateLive && head.Epoch == intent.ExpectedEpoch &&
		head.LogicalDigest == intent.DesiredDigest &&
		head.LogicalDigest == intent.ExpectedDigest &&
		bytes.Equal(head.WinnerID, intent.DesiredWinnerID) &&
		bytes.Equal(head.LPART, intent.LPART) &&
		bytes.Equal(head.LogicalPolicyID, intent.LogicalPolicyID)
}

func sameAcquisitionIntent(pending Pending, intent Intent) bool {
	stored := cloneIntent(pending.Intent)
	if pending.Decision == DecisionReuse && intent.Mode == ModeAbsentOrIdentical {
		stored.ExpectedEpoch = intent.ExpectedEpoch
		stored.ExpectedDigest = intent.ExpectedDigest
	}
	return sameIntent(stored, intent)
}

func cloneHead(value Head) Head {
	value.WinnerID = append([]byte(nil), value.WinnerID...)
	value.TXN = append(coordination.TXN(nil), value.TXN...)
	value.LPART = append(coordination.LPART(nil), value.LPART...)
	value.LogicalPolicyID = append([]byte(nil), value.LogicalPolicyID...)
	return value
}

func cloneHeadPtr(value *Head) *Head {
	if value == nil {
		return nil
	}
	copy := cloneHead(*value)
	return &copy
}

func cloneIntent(value Intent) Intent {
	value.Entity.ID = append(coordination.EntityID(nil), value.Entity.ID...)
	value.TXN = append(coordination.TXN(nil), value.TXN...)
	value.Owner = append(coordination.OwnerID(nil), value.Owner...)
	value.DesiredWinnerID = append([]byte(nil), value.DesiredWinnerID...)
	value.LPART = append(coordination.LPART(nil), value.LPART...)
	value.LogicalPolicyID = append([]byte(nil), value.LogicalPolicyID...)
	return value
}

func clonePending(value Pending) Pending {
	value.Intent = cloneIntent(value.Intent)
	return value
}
