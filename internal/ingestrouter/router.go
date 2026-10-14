// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package ingestrouter

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

// Router opens independently cancellable ingest sessions over a hosted-tablet
// directory.
type Router struct {
	directory Directory
	limits    Limits
}

func New(directory Directory, limits Limits) (*Router, error) {
	if directory == nil {
		return nil, errors.New("ingestrouter: nil hosted-tablet directory")
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Router{directory: directory, limits: limits}, nil
}

// ConditionalCommit routes one conditional mutation to an exact hosted
// tablet. The tablet owns the atomic evaluate-and-commit boundary.
func (r *Router) ConditionalCommit(
	ctx context.Context,
	sessionID string,
	requestID string,
	batch Batch,
	evaluate ConditionalEvaluator,
) (bool, error) {
	if evaluate == nil {
		return false, errors.New("ingestrouter: nil conditional evaluator")
	}
	validated, err := validateRequest(batch.Extent.TableID, r.limits, Request{
		ID: requestID, Batches: []Batch{batch},
	})
	if err != nil {
		return false, err
	}
	batch = validated.request.Batches[0]
	tablet, err := r.directory.Lookup(ctx, batch.Extent)
	if err != nil {
		return false, err
	}
	if tablet == nil {
		return false, ErrNotHosted
	}
	if !tablet.Extent().Equal(batch.Extent) {
		return false, &RouteError{
			Cause: ErrStaleExtent, RetryExtents: []Extent{tablet.Extent().clone()},
		}
	}
	fence := tablet.Fence()
	if !fence.Valid() {
		return false, ErrStaleFence
	}
	if tablet.Authority() != AuthorityAccumuloWAL {
		return false, ErrWALAuthorityUnsupported
	}
	conditional, ok := tablet.(ConditionalTablet)
	if !ok {
		return false, ErrWALAuthorityUnsupported
	}
	return conditional.ConditionalCommit(ctx, CommitRequest{
		OperationID: operationID(sessionID, requestID, batch.Extent.Key()),
		SessionID:   sessionID,
		RequestID:   requestID,
		Extent:      batch.Extent.clone(),
		Fence:       fence,
		Mutations:   cloneMutations(batch.Mutations),
	}, evaluate)
}

// Open starts a session scoped to exactly one table.
func (r *Router) Open(sessionID, tableID string) (*Session, error) {
	if sessionID == "" || len(sessionID) > r.limits.MaxRequestIDBytes {
		return nil, errors.New("ingestrouter: invalid session id")
	}
	if tableID == "" || len(tableID) > r.limits.MaxTableIDBytes {
		return nil, errors.New("ingestrouter: invalid table id")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		id:       sessionID,
		tableID:  tableID,
		router:   r,
		ctx:      ctx,
		cancel:   cancel,
		requests: make(map[string]*requestState),
	}, nil
}

type requestState struct {
	digest   [sha256.Size]byte
	request  Request
	outcomes map[string]Outcome
	running  bool
	done     chan struct{}
}

// Session tracks idempotency and cancellation for a future
// TabletIngestClientService update ID.
type Session struct {
	mu       sync.Mutex
	id       string
	tableID  string
	router   *Router
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
	requests map[string]*requestState
}

// Apply validates the complete request before routing any batch. It returns
// partial outcomes when a later extent fails or the caller is cancelled.
func (s *Session) Apply(ctx context.Context, request Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Result{}, ErrSessionClosed
	}
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return Result{}, ErrSessionCancelled
	}
	s.mu.Unlock()

	validated, err := validateRequest(s.tableID, s.router.limits, request)
	if err != nil {
		return Result{}, err
	}

	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Result{}, ErrSessionClosed
		}
		if err := s.ctx.Err(); err != nil {
			s.mu.Unlock()
			return Result{}, ErrSessionCancelled
		}
		state, exists := s.requests[request.ID]
		if exists && state.digest != validated.digest {
			s.mu.Unlock()
			return Result{}, ErrIdempotencyConflict
		}
		if !exists {
			if len(s.requests) >= s.router.limits.MaxSessionRequests {
				s.mu.Unlock()
				return Result{}, ErrSessionLimit
			}
			state = &requestState{
				digest:   validated.digest,
				request:  validated.request,
				outcomes: make(map[string]Outcome, len(validated.request.Batches)),
			}
			s.requests[request.ID] = state
		}
		if state.running {
			done := state.done
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-s.ctx.Done():
				return Result{}, ErrSessionCancelled
			case <-done:
				continue
			}
		}
		if !hasRetryableWork(state) {
			result := resultFrom(state)
			s.mu.Unlock()
			return result, nil
		}
		state.running = true
		state.done = make(chan struct{})
		s.mu.Unlock()

		result, applyErr := s.apply(ctx, state)

		s.mu.Lock()
		state.running = false
		close(state.done)
		s.mu.Unlock()
		return result, applyErr
	}
}

func (s *Session) apply(caller context.Context, state *requestState) (Result, error) {
	ctx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(caller, cancel)
	defer func() {
		stop()
		cancel()
	}()

	for batchIndex, batch := range state.request.Batches {
		key := batch.Extent.Key()
		if previous, exists := state.outcomes[key]; exists && previous.Status != OutcomeRetry {
			continue
		}
		if err := ctx.Err(); err != nil {
			for _, remaining := range state.request.Batches[batchIndex:] {
				remainingKey := remaining.Extent.Key()
				if prior, exists := state.outcomes[remainingKey]; exists && prior.Status != OutcomeRetry {
					continue
				}
				state.outcomes[remainingKey] = Outcome{Status: OutcomeRetry, Cause: err}
			}
			if caller.Err() != nil {
				return resultFrom(state), caller.Err()
			}
			return resultFrom(state), ErrSessionCancelled
		}
		state.outcomes[key] = s.route(ctx, state.request.ID, batch)
		if err := ctx.Err(); err != nil {
			for _, remaining := range state.request.Batches[batchIndex+1:] {
				remainingKey := remaining.Extent.Key()
				if prior, exists := state.outcomes[remainingKey]; exists && prior.Status != OutcomeRetry {
					continue
				}
				state.outcomes[remainingKey] = Outcome{Status: OutcomeRetry, Cause: err}
			}
			if caller.Err() != nil {
				return resultFrom(state), caller.Err()
			}
			return resultFrom(state), ErrSessionCancelled
		}
	}
	return resultFrom(state), nil
}

func (s *Session) route(ctx context.Context, requestID string, batch Batch) Outcome {
	tablet, err := s.router.directory.Lookup(ctx, batch.Extent)
	if err != nil {
		return classifyRouteError(err)
	}
	if tablet == nil {
		return Outcome{Status: OutcomeRetry, Cause: ErrNotHosted}
	}
	if !tablet.Extent().Equal(batch.Extent) {
		return Outcome{
			Status:       OutcomeRetry,
			Cause:        ErrStaleExtent,
			RetryExtents: []Extent{tablet.Extent().clone()},
		}
	}
	fence := tablet.Fence()
	if !fence.Valid() {
		return Outcome{Status: OutcomeRetry, Cause: ErrStaleFence}
	}
	if tablet.Authority() != AuthorityAccumuloWAL {
		return Outcome{Status: OutcomeRejected, Cause: ErrWALAuthorityUnsupported}
	}
	operationID := operationID(s.id, requestID, batch.Extent.Key())
	err = tablet.Commit(ctx, CommitRequest{
		OperationID: operationID,
		SessionID:   s.id,
		RequestID:   requestID,
		Extent:      batch.Extent.clone(),
		Fence:       fence,
		Mutations:   cloneMutations(batch.Mutations),
	})
	if err == nil {
		return Outcome{Status: OutcomeApplied}
	}
	return classifyRouteError(err)
}

func classifyRouteError(err error) Outcome {
	var routeErr *RouteError
	if errors.As(err, &routeErr) {
		retries := make([]Extent, len(routeErr.RetryExtents))
		for i := range routeErr.RetryExtents {
			retries[i] = routeErr.RetryExtents[i].clone()
		}
		status := OutcomeRejected
		if retryable(err) {
			status = OutcomeRetry
		}
		return Outcome{Status: status, Cause: err, RetryExtents: retries}
	}
	if retryable(err) {
		return Outcome{Status: OutcomeRetry, Cause: err}
	}
	return Outcome{Status: OutcomeRejected, Cause: err}
}

func retryable(err error) bool {
	return errors.Is(err, ErrStaleExtent) || errors.Is(err, ErrStaleFence) ||
		errors.Is(err, ErrNotHosted) || errors.Is(err, ErrRetryable) ||
		errors.Is(err, ErrUnknownCommit) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func operationID(sessionID, requestID, extentKey string) string {
	h := sha256.New()
	writeHashBytes(h, []byte(sessionID))
	writeHashBytes(h, []byte(requestID))
	writeHashBytes(h, []byte(extentKey))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Cancel permanently cancels the session and all in-flight commits.
func (s *Session) Cancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return false
	}
	s.cancel()
	return true
}

// Close makes the session terminal after in-flight calls observe cancellation.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
}

func hasRetryableWork(state *requestState) bool {
	for _, batch := range state.request.Batches {
		outcome, exists := state.outcomes[batch.Extent.Key()]
		if !exists || outcome.Status == OutcomeRetry {
			return true
		}
	}
	return false
}

func resultFrom(state *requestState) Result {
	result := Result{Outcomes: make(map[string]Outcome, len(state.outcomes))}
	for key, outcome := range state.outcomes {
		result.Outcomes[key] = cloneOutcome(outcome)
	}
	return result
}

func cloneMutations(in []Mutation) []Mutation {
	out := make([]Mutation, len(in))
	for i, mutation := range in {
		out[i].Row = append([]byte(nil), mutation.Row...)
		out[i].Updates = make([]Update, len(mutation.Updates))
		for j, update := range mutation.Updates {
			out[i].Updates[j] = Update{
				ColumnFamily:     append([]byte(nil), update.ColumnFamily...),
				ColumnQualifier:  append([]byte(nil), update.ColumnQualifier...),
				ColumnVisibility: append([]byte(nil), update.ColumnVisibility...),
				Timestamp:        update.Timestamp,
				Value:            append([]byte(nil), update.Value...),
				Delete:           update.Delete,
			}
		}
	}
	return out
}

// Equal reports whether extents name the same normalized tablet.
func (e Extent) Equal(other Extent) bool {
	return e.TableID == other.TableID &&
		bytesEqualAbsent(e.PrevEndRow, other.PrevEndRow) &&
		bytesEqualAbsent(e.EndRow, other.EndRow)
}

func bytesEqualAbsent(left, right []byte) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	return string(left) == string(right)
}
