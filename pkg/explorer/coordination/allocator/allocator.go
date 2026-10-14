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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

var (
	ErrConflict    = errors.New("allocator: conflict")
	ErrUnavailable = errors.New("allocator: unavailable")
	ErrCorruption  = errors.New("allocator: internal corruption")
	ErrExhausted   = errors.New("allocator: exhausted")
	ErrWindowFull  = errors.New("allocator: active window full")
	ErrOverflow    = errors.New("allocator: overflow")
	ErrNotFound    = errors.New("allocator: not found")
)

const maxRetireConditions = 63

var (
	familyHead        = []byte("q")
	qualifierHead     = []byte("head")
	familyReservation = []byte("r")
	familyHistory     = []byte("f")
	familyOutcome     = []byte("o")
	qualifierOutcome  = []byte("terminal")
)

type Clock func() time.Time

type Config struct {
	Domain               coordination.DomainID
	ControlVisibility    []byte
	Store                Store
	Clock                Clock
	MaxRetries           int
	RetryBackoff         time.Duration
	MaxCheckpointAdvance int
	MaxRetireBatch       int
}

type Client struct {
	domain               coordination.DomainID
	visibility           []byte
	store                Store
	clock                Clock
	maxRetries           int
	retryBackoff         time.Duration
	maxCheckpointAdvance int
	maxRetireBatch       int
}

func New(config Config) (*Client, error) {
	if err := config.Domain.Validate(); err != nil {
		return nil, err
	}
	if config.Store == nil {
		return nil, errors.New("allocator: store is required")
	}
	if config.MaxRetries < 0 || config.MaxRetries > 100 {
		return nil, errors.New("allocator: max retries is outside its bound")
	}
	if config.RetryBackoff < 0 || config.RetryBackoff > time.Minute {
		return nil, errors.New("allocator: retry backoff is outside its bound")
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}
	if config.RetryBackoff == 0 {
		config.RetryBackoff = 10 * time.Millisecond
	}
	if config.MaxCheckpointAdvance == 0 {
		config.MaxCheckpointAdvance = 1024
	}
	if config.MaxCheckpointAdvance < 1 || config.MaxCheckpointAdvance > coordination.MaxActiveReservations {
		return nil, errors.New("allocator: checkpoint batch is outside its bound")
	}
	if config.MaxRetireBatch == 0 {
		config.MaxRetireBatch = maxRetireConditions
	}
	if config.MaxRetireBatch < 1 || config.MaxRetireBatch > maxRetireConditions {
		return nil, errors.New("allocator: retirement batch is outside its bound")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	return &Client{
		domain:     append(coordination.DomainID(nil), config.Domain...),
		visibility: append([]byte(nil), config.ControlVisibility...),
		store:      config.Store, clock: config.Clock, maxRetries: config.MaxRetries,
		retryBackoff: config.RetryBackoff, maxCheckpointAdvance: config.MaxCheckpointAdvance,
		maxRetireBatch: config.MaxRetireBatch,
	}, nil
}

type Authority struct {
	Generation coordination.Generation
	Mode       coordination.WriterMode
	Holder     coordination.OwnerID
	Fence      coordination.Fence
}

type InitializeOptions struct {
	HistoryFloor          coordination.Epoch
	RetentionGeneration   coordination.Generation
	Authority             Authority
	MaxActiveReservations uint32
	ImportPlanDigest      coordination.Digest
	ImportMaxEpoch        coordination.Epoch
}

type ReserveRequest struct {
	Predecessor         coordination.AllocatorHeadV1
	TXN                 coordination.TXN
	Owner               coordination.OwnerID
	LeaseUntil          time.Time
	Authority           Authority
	OwnerFence          coordination.Fence
	RetentionGeneration coordination.Generation
}

func (c *Client) EnsureInitialized(ctx context.Context, options InitializeOptions) (coordination.AllocatorHeadV1, error) {
	options.Authority.Holder = append(coordination.OwnerID(nil), options.Authority.Holder...)
	head := coordination.AllocatorHeadV1{
		HeadGeneration: 1, NextEpoch: 1, HistoryFloor: options.HistoryFloor,
		RetentionGeneration:       options.RetentionGeneration,
		WriterAuthorityGeneration: options.Authority.Generation,
		WriterMode:                options.Authority.Mode, WriterHolder: options.Authority.Holder,
		WriterFence: options.Authority.Fence, MaxActiveReservations: options.MaxActiveReservations,
		ImportPlanDigest: options.ImportPlanDigest, ImportMaxEpoch: options.ImportMaxEpoch,
	}
	if err := head.Validate(); err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	value, err := coordination.MarshalAllocatorHeadV1(head)
	if err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	mutation := Mutation{
		Row:        c.allocatorRow(),
		Conditions: []Condition{{Coordinate: c.headCoordinate(), Absent: true}},
		Updates:    []Update{{Coordinate: c.headCoordinate(), Value: value, Timestamp: int64(head.HeadGeneration)}},
	}
	ambiguous := false
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return head, nil
		}
		if status != StatusRejected && !errors.Is(writeErr, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return coordination.AllocatorHeadV1{}, classifyUnavailable(writeErr)
		}
		ambiguous = ambiguous || errors.Is(writeErr, ErrConditionalUnknown)
		cell, found, readErr := c.readOne(ctx, c.headCoordinate())
		if readErr != nil {
			return coordination.AllocatorHeadV1{}, readErr
		}
		if found {
			existing, decodeErr := decodeHeadCell(cell)
			if decodeErr != nil {
				return coordination.AllocatorHeadV1{}, decodeErr
			}
			if allocatorHeadEqual(existing, head) {
				return existing, nil
			}
			return coordination.AllocatorHeadV1{}, ErrConflict
		}
		if attempt >= c.maxRetries {
			if ambiguous {
				return coordination.AllocatorHeadV1{}, errors.Join(ErrUnavailable, ErrConditionalUnknown)
			}
			return coordination.AllocatorHeadV1{}, ErrUnavailable
		}
		if err := c.wait(ctx); err != nil {
			return coordination.AllocatorHeadV1{}, err
		}
	}
}

func (c *Client) Initialize(ctx context.Context, options InitializeOptions) (coordination.AllocatorHeadV1, error) {
	return c.EnsureInitialized(ctx, options)
}

func (c *Client) CurrentHead(ctx context.Context) (coordination.AllocatorHeadV1, error) {
	cell, found, err := c.readOne(ctx, c.headCoordinate())
	if err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	if !found {
		return coordination.AllocatorHeadV1{}, ErrNotFound
	}
	return decodeHeadCell(cell)
}

func (c *Client) Reservation(ctx context.Context, epoch coordination.Epoch) (coordination.ReservationV1, error) {
	cell, found, err := c.readOne(ctx, c.reservationCoordinate(epoch))
	if err != nil {
		return coordination.ReservationV1{}, err
	}
	if !found {
		return coordination.ReservationV1{}, ErrNotFound
	}
	return decodeReservationCell(cell, epoch)
}

func (c *Client) Outcome(ctx context.Context, epoch coordination.Epoch) (coordination.EpochOutcomeV1, error) {
	cell, found, err := c.readOne(ctx, c.outcomeCoordinate(epoch))
	if err != nil {
		return coordination.EpochOutcomeV1{}, err
	}
	if !found {
		return coordination.EpochOutcomeV1{}, ErrNotFound
	}
	value, err := coordination.UnmarshalEpochOutcomeV1(cell.Value)
	if err != nil || value.Epoch != epoch || cell.Timestamp != int64(epoch) {
		return coordination.EpochOutcomeV1{}, fmt.Errorf("%w: outcome is invalid", ErrCorruption)
	}
	return value, nil
}

func (c *Client) Reserve(ctx context.Context, request ReserveRequest) (coordination.ReservationV1, error) {
	predecessor := request.Predecessor
	predecessor.WriterHolder = append(coordination.OwnerID(nil), predecessor.WriterHolder...)
	request.TXN = append(coordination.TXN(nil), request.TXN...)
	request.Owner = append(coordination.OwnerID(nil), request.Owner...)
	request.Authority.Holder = append(coordination.OwnerID(nil), request.Authority.Holder...)
	if err := predecessor.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	if err := request.TXN.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	if err := request.Owner.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	if request.LeaseUntil.IsZero() || request.LeaseUntil.Location() != time.UTC {
		return coordination.ReservationV1{}, errors.New("allocator: reservation lease must be UTC")
	}
	if err := validateAuthority(predecessor, request.Authority, request.RetentionGeneration); err != nil {
		return coordination.ReservationV1{}, err
	}
	if request.OwnerFence == 0 {
		request.OwnerFence = request.Authority.Fence
	}
	if err := request.OwnerFence.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	if predecessor.NextEpoch == coordination.Epoch(math.MaxInt64) {
		return coordination.ReservationV1{}, errors.Join(ErrExhausted, ErrOverflow)
	}
	if predecessor.ActiveReservations >= predecessor.MaxActiveReservations {
		return coordination.ReservationV1{}, ErrWindowFull
	}
	epoch := predecessor.NextEpoch
	reservation := coordination.ReservationV1{
		ReservationGeneration: 1, Epoch: epoch, TXN: append(coordination.TXN(nil), request.TXN...),
		Owner: append(coordination.OwnerID(nil), request.Owner...), LeaseUntil: request.LeaseUntil,
		Fence: request.OwnerFence, AuthorityGeneration: request.Authority.Generation,
		State: coordination.StateEpochReserved,
	}
	reservationBytes, err := coordination.MarshalReservationV1(reservation)
	if err != nil {
		return coordination.ReservationV1{}, err
	}
	predecessorBytes, err := coordination.MarshalAllocatorHeadV1(predecessor)
	if err != nil {
		return coordination.ReservationV1{}, err
	}
	next := predecessor
	if err := incrementHeadGeneration(&next); err != nil {
		return coordination.ReservationV1{}, err
	}
	next.NextEpoch++
	next.ActiveReservations++
	if next.ActiveWindowStart == 0 {
		next.ActiveWindowStart = epoch
	}
	nextBytes, err := coordination.MarshalAllocatorHeadV1(next)
	if err != nil {
		return coordination.ReservationV1{}, fmt.Errorf("%w: %v", ErrOverflow, err)
	}
	if err := coordination.ValidateAllocatorHeadSuccessor(predecessor, next); err != nil {
		return coordination.ReservationV1{}, fmt.Errorf("%w: invalid allocator head successor", ErrCorruption)
	}
	mutation := Mutation{
		Row: c.allocatorRow(),
		Conditions: []Condition{
			{Coordinate: c.headCoordinate(), Value: predecessorBytes, Timestamp: int64(predecessor.HeadGeneration), TimestampSet: true},
			{Coordinate: c.reservationCoordinate(epoch), Absent: true},
		},
		Updates: []Update{
			{Coordinate: c.headCoordinate(), Value: nextBytes, Timestamp: int64(next.HeadGeneration)},
			{Coordinate: c.reservationCoordinate(epoch), Value: reservationBytes, Timestamp: int64(reservation.ReservationGeneration)},
		},
	}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return reservation, nil
		}
		if status != StatusRejected && !errors.Is(writeErr, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return coordination.ReservationV1{}, classifyUnavailable(writeErr)
		}
		decision, reconcileErr := c.reconcileAllocation(ctx, predecessor, reservation)
		switch decision {
		case reconcileApplied:
			return reservation, nil
		case reconcileConflict:
			return coordination.ReservationV1{}, reconcileErr
		case reconcileRetry:
			if attempt >= c.maxRetries {
				return coordination.ReservationV1{}, errors.Join(ErrUnavailable, ErrConditionalUnknown)
			}
			if err := c.wait(ctx); err != nil {
				return coordination.ReservationV1{}, err
			}
		default:
			return coordination.ReservationV1{}, reconcileErr
		}
	}
}

type CompletionState uint8

const (
	CompletionOutcomeDurable CompletionState = iota + 1
	CompletionReservationDurableOutcomePending
)

func (c *Client) Terminalize(
	ctx context.Context,
	predecessor coordination.ReservationV1,
	state coordination.TxnState,
) (CompletionState, coordination.EpochOutcomeV1, error) {
	if err := predecessor.Validate(); err != nil {
		return 0, coordination.EpochOutcomeV1{}, err
	}
	if predecessor.State.Terminal() {
		if predecessor.State != state {
			return 0, coordination.EpochOutcomeV1{}, ErrConflict
		}
	} else if !state.Terminal() {
		return 0, coordination.EpochOutcomeV1{}, errors.New("allocator: terminal state is required")
	}
	terminal := predecessor
	if !predecessor.State.Terminal() {
		if terminal.ReservationGeneration == coordination.Generation(math.MaxInt64) {
			return 0, coordination.EpochOutcomeV1{}, errors.Join(ErrExhausted, ErrOverflow)
		}
		terminal.ReservationGeneration++
		terminal.State = state
	}
	beforeBytes, _ := coordination.MarshalReservationV1(predecessor)
	terminalBytes, err := coordination.MarshalReservationV1(terminal)
	if err != nil {
		return 0, coordination.EpochOutcomeV1{}, err
	}
	if !predecessor.State.Terminal() {
		if err := coordination.ValidateReservationSuccessor(predecessor, terminal); err != nil {
			return 0, coordination.EpochOutcomeV1{}, fmt.Errorf("%w: invalid reservation successor", ErrCorruption)
		}
	}
	if !predecessor.State.Terminal() {
		mutation := Mutation{
			Row: c.allocatorRow(),
			Conditions: []Condition{{
				Coordinate: c.reservationCoordinate(predecessor.Epoch), Value: beforeBytes,
				Timestamp: int64(predecessor.ReservationGeneration), TimestampSet: true,
			}},
			Updates: []Update{{Coordinate: c.reservationCoordinate(predecessor.Epoch), Value: terminalBytes, Timestamp: int64(terminal.ReservationGeneration)}},
		}
		if err := c.applyReservationTransition(ctx, mutation, terminal); err != nil {
			return 0, coordination.EpochOutcomeV1{}, err
		}
	}
	outcome, err := coordination.NewEpochOutcomeV1(
		terminal.Epoch, terminal.TXN, terminal.State, terminal.Fence, terminal.AuthorityGeneration,
	)
	if err != nil {
		return CompletionReservationDurableOutcomePending, coordination.EpochOutcomeV1{}, err
	}
	if err := c.createOutcome(ctx, outcome); err != nil {
		return CompletionReservationDurableOutcomePending, outcome, err
	}
	return CompletionOutcomeDurable, outcome, nil
}

// TakeoverReservation replaces an expired nonterminal owner under a strictly
// newer transaction fence. It does not allocate a new epoch.
func (c *Client) TakeoverReservation(
	ctx context.Context,
	predecessor coordination.ReservationV1,
	owner coordination.OwnerID,
	leaseUntil time.Time,
	fence coordination.Fence,
	now time.Time,
) (coordination.ReservationV1, error) {
	if err := predecessor.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	if predecessor.State.Terminal() {
		return coordination.ReservationV1{}, ErrConflict
	}
	if now.IsZero() || now.Location() != time.UTC || leaseUntil.IsZero() || leaseUntil.Location() != time.UTC {
		return coordination.ReservationV1{}, errors.New("allocator: takeover times must be UTC")
	}
	if predecessor.LeaseUntil.After(now) || !leaseUntil.After(now) ||
		fence <= predecessor.Fence {
		return coordination.ReservationV1{}, ErrConflict
	}
	if err := owner.Validate(); err != nil {
		return coordination.ReservationV1{}, err
	}
	next := predecessor
	next.ReservationGeneration++
	next.Owner = append(coordination.OwnerID(nil), owner...)
	next.LeaseUntil = leaseUntil
	next.Fence = fence
	if err := coordination.ValidateReservationTakeover(predecessor, next); err != nil {
		return coordination.ReservationV1{}, fmt.Errorf("%w: invalid reservation takeover", ErrCorruption)
	}
	beforeBytes, _ := coordination.MarshalReservationV1(predecessor)
	nextBytes, _ := coordination.MarshalReservationV1(next)
	mutation := Mutation{
		Row: c.allocatorRow(),
		Conditions: []Condition{{
			Coordinate: c.reservationCoordinate(predecessor.Epoch), Value: beforeBytes,
			Timestamp: int64(predecessor.ReservationGeneration), TimestampSet: true,
		}},
		Updates: []Update{{
			Coordinate: c.reservationCoordinate(next.Epoch), Value: nextBytes,
			Timestamp: int64(next.ReservationGeneration),
		}},
	}
	if err := c.applyReservationTransition(ctx, mutation, next); err != nil {
		return coordination.ReservationV1{}, err
	}
	return next, nil
}

func (c *Client) applyReservationTransition(ctx context.Context, mutation Mutation, terminal coordination.ReservationV1) error {
	for attempt := 0; ; attempt++ {
		status, err := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return nil
		}
		if status != StatusRejected && !errors.Is(err, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(err) {
			return classifyUnavailable(err)
		}
		got, readErr := c.Reservation(ctx, terminal.Epoch)
		if readErr == nil {
			if reservationEqual(got, terminal) {
				return nil
			}
			expected, expectedErr := coordination.UnmarshalReservationV1(mutation.Conditions[0].Value)
			if expectedErr == nil && reservationEqual(got, expected) {
				if attempt >= c.maxRetries {
					return errors.Join(ErrUnavailable, ErrConditionalUnknown)
				}
				if err := c.wait(ctx); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: contradictory terminal reservation", ErrCorruption)
		}
		if !errors.Is(readErr, ErrNotFound) {
			return readErr
		}
		if attempt >= c.maxRetries {
			return errors.Join(ErrUnavailable, ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) createOutcome(ctx context.Context, outcome coordination.EpochOutcomeV1) error {
	value, _ := coordination.MarshalEpochOutcomeV1(outcome)
	mutation := Mutation{
		Row:        c.outcomeRow(outcome.Epoch),
		Conditions: []Condition{{Coordinate: c.outcomeCoordinate(outcome.Epoch), Absent: true}},
		Updates:    []Update{{Coordinate: c.outcomeCoordinate(outcome.Epoch), Value: value, Timestamp: int64(outcome.Epoch)}},
	}
	for attempt := 0; ; attempt++ {
		status, err := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return nil
		}
		if status != StatusRejected && !errors.Is(err, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(err) {
			return classifyUnavailable(err)
		}
		got, readErr := c.Outcome(ctx, outcome.Epoch)
		if readErr == nil {
			if outcomeEqual(got, outcome) {
				return nil
			}
			return fmt.Errorf("%w: contradictory immutable outcome", ErrCorruption)
		}
		if !errors.Is(readErr, ErrNotFound) {
			return readErr
		}
		if attempt >= c.maxRetries {
			return errors.Join(ErrUnavailable, ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) AdvanceFrontier(ctx context.Context) (coordination.FrontierCheckpointV1, error) {
	head, err := c.CurrentHead(ctx)
	if err != nil {
		return coordination.FrontierCheckpointV1{}, err
	}
	start := head.Frontier + 1
	if start >= head.NextEpoch {
		return coordination.FrontierCheckpointV1{}, ErrNotFound
	}
	limit := c.maxCheckpointAdvance
	if remaining := int(head.NextEpoch - start); remaining < limit {
		limit = remaining
	}
	outcomes := make([]coordination.EpochOutcomeV1, 0, limit)
	for i := 0; i < limit; i++ {
		epoch := start + coordination.Epoch(i)
		reservation, reservationErr := c.Reservation(ctx, epoch)
		outcome, outcomeErr := c.Outcome(ctx, epoch)
		if errors.Is(reservationErr, ErrNotFound) || errors.Is(outcomeErr, ErrNotFound) {
			break
		}
		if reservationErr != nil {
			return coordination.FrontierCheckpointV1{}, reservationErr
		}
		if outcomeErr != nil {
			return coordination.FrontierCheckpointV1{}, outcomeErr
		}
		if !reservation.State.Terminal() || !matchesReservation(outcome, reservation) {
			return coordination.FrontierCheckpointV1{}, fmt.Errorf("%w: terminal reservation and outcome disagree", ErrCorruption)
		}
		outcomes = append(outcomes, outcome)
	}
	if len(outcomes) == 0 {
		return coordination.FrontierCheckpointV1{}, ErrNotFound
	}
	visibleAt := c.clock().UTC()
	if !head.VisibleAt.IsZero() && !visibleAt.After(head.VisibleAt) {
		visibleAt = head.VisibleAt.Add(time.Nanosecond)
	}
	outcomesDigest := OutcomesDigest(outcomes)
	checkpoint, err := coordination.NewFrontierCheckpointV1(
		outcomes[len(outcomes)-1].Epoch, visibleAt, head.CheckpointDigest, outcomesDigest,
	)
	if err != nil {
		return coordination.FrontierCheckpointV1{}, err
	}
	headBytes, _ := coordination.MarshalAllocatorHeadV1(head)
	next := head
	if err := incrementHeadGeneration(&next); err != nil {
		return coordination.FrontierCheckpointV1{}, err
	}
	next.Frontier = checkpoint.Frontier
	next.VisibleAt = checkpoint.VisibleAt
	next.CheckpointDigest = checkpoint.Digest
	nextBytes, _ := coordination.MarshalAllocatorHeadV1(next)
	if err := coordination.ValidateAllocatorHeadSuccessor(head, next); err != nil {
		return coordination.FrontierCheckpointV1{}, fmt.Errorf("%w: invalid allocator head successor", ErrCorruption)
	}
	checkpointBytes, _ := coordination.MarshalFrontierCheckpointV1(checkpoint)
	history := c.historyCoordinate(checkpoint.VisibleAt, checkpoint.Frontier)
	mutation := Mutation{
		Row: c.allocatorRow(),
		Conditions: []Condition{
			{Coordinate: c.headCoordinate(), Value: headBytes, Timestamp: int64(head.HeadGeneration), TimestampSet: true},
			{Coordinate: history, Absent: true},
		},
		Updates: []Update{
			{Coordinate: c.headCoordinate(), Value: nextBytes, Timestamp: int64(next.HeadGeneration)},
			{Coordinate: history, Value: checkpointBytes, Timestamp: int64(checkpoint.Frontier)},
		},
	}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return checkpoint, nil
		}
		if status != StatusRejected && !errors.Is(writeErr, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return coordination.FrontierCheckpointV1{}, classifyUnavailable(writeErr)
		}
		decision, reconcileErr := c.reconcileCheckpoint(ctx, head, checkpoint, history)
		if decision == reconcileApplied {
			return checkpoint, nil
		}
		if decision != reconcileRetry {
			return coordination.FrontierCheckpointV1{}, reconcileErr
		}
		if attempt >= c.maxRetries {
			return coordination.FrontierCheckpointV1{}, errors.Join(ErrUnavailable, ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return coordination.FrontierCheckpointV1{}, err
		}
	}
}

// OutcomesDigest hashes canonical outcome envelopes in ascending epoch order.
func OutcomesDigest(outcomes []coordination.EpochOutcomeV1) coordination.Digest {
	copyOf := append([]coordination.EpochOutcomeV1(nil), outcomes...)
	sort.Slice(copyOf, func(i, j int) bool { return copyOf[i].Epoch < copyOf[j].Epoch })
	hash := sha256.New()
	for _, outcome := range copyOf {
		value, _ := coordination.MarshalEpochOutcomeV1(outcome)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(value)
	}
	var result coordination.Digest
	copy(result[:], hash.Sum(nil))
	return result
}

func (c *Client) Retire(ctx context.Context) (coordination.AllocatorHeadV1, error) {
	head, err := c.CurrentHead(ctx)
	if err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	start := head.RetiredThrough + 1
	if start > head.Frontier {
		return coordination.AllocatorHeadV1{}, ErrNotFound
	}
	limit := c.maxRetireBatch
	if remaining := int(head.Frontier - start + 1); remaining < limit {
		limit = remaining
	}
	conditions := make([]Condition, 0, limit+1)
	updates := make([]Update, 0, limit+1)
	headBytes, _ := coordination.MarshalAllocatorHeadV1(head)
	conditions = append(conditions, Condition{
		Coordinate: c.headCoordinate(), Value: headBytes,
		Timestamp: int64(head.HeadGeneration), TimestampSet: true,
	})
	retired := 0
	for i := 0; i < limit; i++ {
		epoch := start + coordination.Epoch(i)
		reservation, reservationErr := c.Reservation(ctx, epoch)
		outcome, outcomeErr := c.Outcome(ctx, epoch)
		if reservationErr != nil || outcomeErr != nil {
			if errors.Is(reservationErr, ErrNotFound) {
				break
			}
			if reservationErr != nil {
				return coordination.AllocatorHeadV1{}, reservationErr
			}
			return coordination.AllocatorHeadV1{}, outcomeErr
		}
		if !reservation.State.Terminal() || !matchesReservation(outcome, reservation) {
			return coordination.AllocatorHeadV1{}, fmt.Errorf("%w: retirement proof mismatch", ErrCorruption)
		}
		value, _ := coordination.MarshalReservationV1(reservation)
		coordinate := c.reservationCoordinate(epoch)
		conditions = append(conditions, Condition{
			Coordinate: coordinate, Value: value,
			Timestamp: int64(reservation.ReservationGeneration), TimestampSet: true,
		})
		updates = append(updates, Update{Coordinate: coordinate, Delete: true, Timestamp: int64(reservation.ReservationGeneration)})
		retired++
	}
	if retired == 0 {
		return coordination.AllocatorHeadV1{}, ErrNotFound
	}
	next := head
	if err := incrementHeadGeneration(&next); err != nil {
		return coordination.AllocatorHeadV1{}, err
	}
	next.RetiredThrough = start + coordination.Epoch(retired-1)
	if uint32(retired) > next.ActiveReservations {
		return coordination.AllocatorHeadV1{}, fmt.Errorf("%w: retirement exceeds active window", ErrCorruption)
	}
	next.ActiveReservations -= uint32(retired)
	if next.ActiveReservations == 0 {
		next.ActiveWindowStart = 0
	} else {
		next.ActiveWindowStart = next.RetiredThrough + 1
	}
	nextBytes, err := coordination.MarshalAllocatorHeadV1(next)
	if err != nil {
		return coordination.AllocatorHeadV1{}, fmt.Errorf("%w: retirement metadata invalid", ErrCorruption)
	}
	if err := coordination.ValidateAllocatorHeadSuccessor(head, next); err != nil {
		return coordination.AllocatorHeadV1{}, fmt.Errorf("%w: invalid allocator head successor", ErrCorruption)
	}
	updates = append([]Update{{Coordinate: c.headCoordinate(), Value: nextBytes, Timestamp: int64(next.HeadGeneration)}}, updates...)
	mutation := Mutation{Row: c.allocatorRow(), Conditions: conditions, Updates: updates}
	for attempt := 0; ; attempt++ {
		status, writeErr := c.store.CompareAndMutate(ctx, mutation)
		if status == StatusAccepted {
			return next, nil
		}
		if status != StatusRejected && !errors.Is(writeErr, ErrConditionalUnknown) &&
			!resumeAfterDefiniteFailure(writeErr) {
			return coordination.AllocatorHeadV1{}, classifyUnavailable(writeErr)
		}
		decision, reconcileErr := c.reconcileRetirement(ctx, head, next, conditions[1:])
		if decision == reconcileApplied {
			return next, nil
		}
		if decision != reconcileRetry {
			return coordination.AllocatorHeadV1{}, reconcileErr
		}
		if attempt >= c.maxRetries {
			return coordination.AllocatorHeadV1{}, errors.Join(ErrUnavailable, ErrConditionalUnknown)
		}
		if err := c.wait(ctx); err != nil {
			return coordination.AllocatorHeadV1{}, err
		}
	}
}

func (c *Client) LatestCheckpoint(ctx context.Context) (coordination.FrontierCheckpointV1, error) {
	head, err := c.CurrentHead(ctx)
	if err != nil {
		return coordination.FrontierCheckpointV1{}, err
	}
	if head.Frontier == 0 {
		return coordination.FrontierCheckpointV1{}, ErrNotFound
	}
	return c.CheckpointAtOrBefore(ctx, head.VisibleAt)
}

func (c *Client) CheckpointAtOrBefore(ctx context.Context, at time.Time) (coordination.FrontierCheckpointV1, error) {
	if at.IsZero() {
		return coordination.FrontierCheckpointV1{}, errors.New("allocator: checkpoint time is required")
	}
	prefix, err := coordination.INV_TIME(at.UTC())
	if err != nil {
		return coordination.FrontierCheckpointV1{}, err
	}
	cells, err := c.store.ScanRowPrefix(ctx, c.allocatorRow(), familyHistory, prefix, c.visibility, 1)
	if err != nil {
		return coordination.FrontierCheckpointV1{}, classifyUnavailable(err)
	}
	if len(cells) == 0 {
		return coordination.FrontierCheckpointV1{}, ErrNotFound
	}
	checkpoint, err := coordination.UnmarshalFrontierCheckpointV1(cells[0].Value)
	if err != nil || checkpoint.VisibleAt.After(at) || cells[0].Timestamp != int64(checkpoint.Frontier) {
		return coordination.FrontierCheckpointV1{}, fmt.Errorf("%w: checkpoint history is invalid", ErrCorruption)
	}
	return checkpoint, nil
}

type reconcileDecision uint8

const (
	reconcileApplied reconcileDecision = iota + 1
	reconcileRetry
	reconcileConflict
	reconcileCorrupt
)

func (c *Client) reconcileAllocation(ctx context.Context, predecessor coordination.AllocatorHeadV1, reservation coordination.ReservationV1) (reconcileDecision, error) {
	cells, err := c.store.ReadExact(ctx, []Coordinate{c.headCoordinate(), c.reservationCoordinate(reservation.Epoch)})
	if err != nil {
		return reconcileCorrupt, classifyUnavailable(err)
	}
	headCell, headFound := findCell(cells, c.headCoordinate())
	reservationCell, reservationFound := findCell(cells, c.reservationCoordinate(reservation.Epoch))
	if !headFound {
		return reconcileCorrupt, fmt.Errorf("%w: allocator head disappeared", ErrCorruption)
	}
	head, decodeErr := decodeHeadCell(headCell)
	if decodeErr != nil {
		return reconcileCorrupt, decodeErr
	}
	if head.NextEpoch == predecessor.NextEpoch+1 && reservationFound {
		got, reservationErr := decodeReservationCell(reservationCell, reservation.Epoch)
		if reservationErr == nil && reservationEqual(got, reservation) {
			return reconcileApplied, nil
		}
		return reconcileConflict, ErrConflict
	}
	if allocatorHeadEqual(head, predecessor) && !reservationFound {
		return reconcileRetry, nil
	}
	if reservationFound {
		got, reservationErr := decodeReservationCell(reservationCell, reservation.Epoch)
		if reservationErr == nil && reservationEqual(got, reservation) && head.NextEpoch > predecessor.NextEpoch {
			return reconcileApplied, nil
		}
	}
	if head.NextEpoch == predecessor.NextEpoch+1 && !reservationFound ||
		allocatorHeadEqual(head, predecessor) && reservationFound {
		return reconcileCorrupt, fmt.Errorf("%w: one-sided allocation state", ErrCorruption)
	}
	return reconcileConflict, ErrConflict
}

func (c *Client) reconcileCheckpoint(ctx context.Context, predecessor coordination.AllocatorHeadV1, checkpoint coordination.FrontierCheckpointV1, history Coordinate) (reconcileDecision, error) {
	cells, err := c.store.ReadExact(ctx, []Coordinate{c.headCoordinate(), history})
	if err != nil {
		return reconcileCorrupt, classifyUnavailable(err)
	}
	headCell, headFound := findCell(cells, c.headCoordinate())
	historyCell, historyFound := findCell(cells, history)
	if !headFound {
		return reconcileCorrupt, fmt.Errorf("%w: allocator head disappeared", ErrCorruption)
	}
	head, decodeErr := decodeHeadCell(headCell)
	if decodeErr != nil {
		return reconcileCorrupt, decodeErr
	}
	headApplied := head.Frontier == checkpoint.Frontier && head.VisibleAt.Equal(checkpoint.VisibleAt) && head.CheckpointDigest == checkpoint.Digest
	historyApplied := false
	if historyFound {
		got, checkpointErr := coordination.UnmarshalFrontierCheckpointV1(historyCell.Value)
		historyApplied = checkpointErr == nil && historyCell.Timestamp == int64(checkpoint.Frontier) &&
			checkpointEqual(got, checkpoint)
	}
	if headApplied && historyApplied {
		return reconcileApplied, nil
	}
	if allocatorHeadEqual(head, predecessor) && !historyFound {
		return reconcileRetry, nil
	}
	if headApplied != historyApplied {
		return reconcileCorrupt, fmt.Errorf("%w: one-sided checkpoint state", ErrCorruption)
	}
	return reconcileConflict, ErrConflict
}

func (c *Client) reconcileRetirement(ctx context.Context, predecessor, next coordination.AllocatorHeadV1, reservations []Condition) (reconcileDecision, error) {
	coordinates := make([]Coordinate, 1, len(reservations)+1)
	coordinates[0] = c.headCoordinate()
	for _, condition := range reservations {
		coordinates = append(coordinates, condition.Coordinate)
	}
	cells, err := c.store.ReadExact(ctx, coordinates)
	if err != nil {
		return reconcileCorrupt, classifyUnavailable(err)
	}
	headCell, found := findCell(cells, c.headCoordinate())
	if !found {
		return reconcileCorrupt, fmt.Errorf("%w: allocator head disappeared", ErrCorruption)
	}
	head, decodeErr := decodeHeadCell(headCell)
	if decodeErr != nil {
		return reconcileCorrupt, decodeErr
	}
	absent := 0
	predecessors := 0
	for _, condition := range reservations {
		cell, exists := findCell(cells, condition.Coordinate)
		if !exists {
			absent++
		} else if bytes.Equal(cell.Value, condition.Value) &&
			(!condition.TimestampSet || cell.Timestamp == condition.Timestamp) {
			predecessors++
		} else {
			return reconcileCorrupt, fmt.Errorf("%w: retirement reservation changed", ErrCorruption)
		}
	}
	if allocatorHeadEqual(head, next) && absent == len(reservations) {
		return reconcileApplied, nil
	}
	if allocatorHeadEqual(head, predecessor) && predecessors == len(reservations) {
		return reconcileRetry, nil
	}
	return reconcileCorrupt, fmt.Errorf("%w: mixed retirement state", ErrCorruption)
}

func validateAuthority(head coordination.AllocatorHeadV1, authority Authority, retention coordination.Generation) error {
	if authority.Generation != head.WriterAuthorityGeneration || authority.Mode != head.WriterMode ||
		!bytes.Equal(authority.Holder, head.WriterHolder) || authority.Fence != head.WriterFence ||
		retention != head.RetentionGeneration {
		return ErrConflict
	}
	return nil
}

func (c *Client) allocatorRow() []byte {
	row, _ := coordination.AllocatorRow(c.domain)
	return row
}

func (c *Client) outcomeRow(epoch coordination.Epoch) []byte {
	row, _ := coordination.OutcomeRow(c.domain, epoch)
	return row
}

func (c *Client) headCoordinate() Coordinate {
	return Coordinate{Row: c.allocatorRow(), Family: familyHead, Qualifier: qualifierHead, Visibility: append([]byte(nil), c.visibility...)}
}

func (c *Client) reservationCoordinate(epoch coordination.Epoch) Coordinate {
	return Coordinate{Row: c.allocatorRow(), Family: familyReservation, Qualifier: coordination.U64(uint64(epoch)), Visibility: append([]byte(nil), c.visibility...)}
}

func (c *Client) outcomeCoordinate(epoch coordination.Epoch) Coordinate {
	return Coordinate{Row: c.outcomeRow(epoch), Family: familyOutcome, Qualifier: qualifierOutcome, Visibility: append([]byte(nil), c.visibility...)}
}

func (c *Client) historyCoordinate(at time.Time, frontier coordination.Epoch) Coordinate {
	inverse, _ := coordination.INV_TIME(at)
	qualifier := append(inverse, coordination.U64(uint64(frontier))...)
	return Coordinate{Row: c.allocatorRow(), Family: familyHistory, Qualifier: qualifier, Visibility: append([]byte(nil), c.visibility...)}
}

func (c *Client) readOne(ctx context.Context, coordinate Coordinate) (Cell, bool, error) {
	cells, err := c.store.ReadExact(ctx, []Coordinate{coordinate})
	if err != nil {
		return Cell{}, false, classifyUnavailable(err)
	}
	cell, found := findCell(cells, coordinate)
	if len(cells) > 1 {
		return Cell{}, false, fmt.Errorf("%w: duplicate exact coordinate", ErrCorruption)
	}
	return cell, found, nil
}

func findCell(cells []Cell, coordinate Coordinate) (Cell, bool) {
	for _, cell := range cells {
		if cell.Coordinate.equal(coordinate) {
			return cell, true
		}
	}
	return Cell{}, false
}

func matchesReservation(outcome coordination.EpochOutcomeV1, reservation coordination.ReservationV1) bool {
	return outcome.Epoch == reservation.Epoch && bytes.Equal(outcome.TXN, reservation.TXN) &&
		outcome.State == reservation.State && outcome.OwnerFence == reservation.Fence &&
		outcome.AuthorityGeneration == reservation.AuthorityGeneration
}

func reservationEqual(left, right coordination.ReservationV1) bool {
	a, _ := coordination.MarshalReservationV1(left)
	b, _ := coordination.MarshalReservationV1(right)
	return bytes.Equal(a, b)
}

func outcomeEqual(left, right coordination.EpochOutcomeV1) bool {
	a, _ := coordination.MarshalEpochOutcomeV1(left)
	b, _ := coordination.MarshalEpochOutcomeV1(right)
	return bytes.Equal(a, b)
}

func checkpointEqual(left, right coordination.FrontierCheckpointV1) bool {
	a, _ := coordination.MarshalFrontierCheckpointV1(left)
	b, _ := coordination.MarshalFrontierCheckpointV1(right)
	return bytes.Equal(a, b)
}

func allocatorHeadEqual(left, right coordination.AllocatorHeadV1) bool {
	a, _ := coordination.MarshalAllocatorHeadV1(left)
	b, _ := coordination.MarshalAllocatorHeadV1(right)
	return bytes.Equal(a, b)
}

func classifyUnavailable(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.Join(ErrUnavailable, err)
}

// resumeAfterDefiniteFailure reports whether a non-rejected, non-ambiguous
// CompareAndMutate error should resume through the read-back reconcile path
// rather than abort. It returns true to resume, and false only for context
// cancellation, which short-circuits the loop. The Store contract
// (allocator/store.go) withholds ErrConditionalUnknown unless a mutation may
// have applied, so any other error means the conditional mutation definitely
// did not apply and is safe to reconcile and retry - strictly safer than the
// ambiguous ErrConditionalUnknown case the loops already handle.
func resumeAfterDefiniteFailure(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func incrementHeadGeneration(head *coordination.AllocatorHeadV1) error {
	if head.HeadGeneration == coordination.Generation(math.MaxInt64) {
		return errors.Join(ErrExhausted, ErrOverflow)
	}
	head.HeadGeneration++
	return nil
}

func decodeHeadCell(cell Cell) (coordination.AllocatorHeadV1, error) {
	head, err := coordination.UnmarshalAllocatorHeadV1(cell.Value)
	if err != nil || cell.Timestamp != int64(head.HeadGeneration) {
		return coordination.AllocatorHeadV1{}, fmt.Errorf("%w: allocator head record and timestamp disagree", ErrCorruption)
	}
	return head, nil
}

func decodeReservationCell(cell Cell, epoch coordination.Epoch) (coordination.ReservationV1, error) {
	reservation, err := coordination.UnmarshalReservationV1(cell.Value)
	if err != nil || reservation.Epoch != epoch ||
		cell.Timestamp != int64(reservation.ReservationGeneration) {
		return coordination.ReservationV1{}, fmt.Errorf("%w: reservation record and timestamp disagree", ErrCorruption)
	}
	return reservation, nil
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
