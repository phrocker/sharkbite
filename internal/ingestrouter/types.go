// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ingestrouter validates and routes mutation batches to tablets that
// are already hosted and fenced by the tablet-server lifecycle.
package ingestrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

var (
	ErrInvalidBatch            = errors.New("ingestrouter: invalid mutation batch")
	ErrIdempotencyConflict     = errors.New("ingestrouter: idempotency key reused with different data")
	ErrSessionLimit            = errors.New("ingestrouter: ingest session request limit exceeded")
	ErrSessionCancelled        = errors.New("ingestrouter: ingest session cancelled")
	ErrSessionClosed           = errors.New("ingestrouter: ingest session closed")
	ErrNotHosted               = errors.New("ingestrouter: tablet is not hosted")
	ErrStaleExtent             = errors.New("ingestrouter: stale tablet extent")
	ErrStaleFence              = errors.New("ingestrouter: stale tablet fence")
	ErrRetryable               = errors.New("ingestrouter: retryable tablet failure")
	ErrUnknownCommit           = errors.New("ingestrouter: commit outcome is unknown")
	ErrWALAuthorityUnsupported = errors.New("ingestrouter: authoritative Accumulo WAL commit is unsupported")
)

// Extent identifies one Accumulo tablet and covers (PrevEndRow, EndRow].
type Extent struct {
	TableID    string
	PrevEndRow []byte
	EndRow     []byte
}

func (e Extent) Validate() error {
	if e.TableID == "" {
		return fmt.Errorf("%w: empty table id", ErrInvalidBatch)
	}
	if len(e.PrevEndRow) > 0 && len(e.EndRow) > 0 &&
		bytes.Compare(e.PrevEndRow, e.EndRow) >= 0 {
		return fmt.Errorf("%w: extent lower bound is not below upper bound", ErrInvalidBatch)
	}
	return nil
}

// Contains reports whether row belongs to the extent.
func (e Extent) Contains(row []byte) bool {
	return len(row) > 0 &&
		(len(e.PrevEndRow) == 0 || bytes.Compare(row, e.PrevEndRow) > 0) &&
		(len(e.EndRow) == 0 || bytes.Compare(row, e.EndRow) <= 0)
}

// Key returns a stable, collision-free map key.
func (e Extent) Key() string {
	return fmt.Sprintf("%d:%s/%x/%x", len(e.TableID), e.TableID, e.PrevEndRow, e.EndRow)
}

func (e Extent) clone() Extent {
	return Extent{
		TableID:    e.TableID,
		PrevEndRow: append([]byte(nil), e.PrevEndRow...),
		EndRow:     append([]byte(nil), e.EndRow...),
	}
}

// Timestamp distinguishes an explicit int64 timestamp from a request for the
// tablet server to assign one.
type Timestamp struct {
	Set   bool
	Value int64
}

// Update is one put or delete.
type Update struct {
	ColumnFamily     []byte
	ColumnQualifier  []byte
	ColumnVisibility []byte
	Timestamp        Timestamp
	Value            []byte
	Delete           bool
}

// Mutation is an ordered set of updates for one non-empty row.
type Mutation struct {
	Row     []byte
	Updates []Update
}

// Batch groups mutations for exactly one expected extent.
type Batch struct {
	Extent    Extent
	Mutations []Mutation
}

// Request is one idempotent session operation. Reusing ID with identical data
// retries only extents whose previous outcome was retryable.
type Request struct {
	ID      string
	Batches []Batch
}

// Fence is an opaque hosting generation. Implementations should include every
// generation needed to reject an old server, manager, or assignment attempt.
type Fence struct {
	ServerGeneration  string
	ManagerGeneration string
	Assignment        uint64
}

func (f Fence) Valid() bool {
	return f.ServerGeneration != "" && f.ManagerGeneration != "" && f.Assignment != 0
}

// CommitAuthority describes what makes a tablet acknowledgement durable.
type CommitAuthority uint8

const (
	// AuthorityUnsupported is the required fail-closed value until a hosted
	// tablet has an Accumulo-authoritative WAL and metadata commit path.
	AuthorityUnsupported CommitAuthority = iota
	// AuthorityAccumuloWAL means Commit acknowledges only after the configured
	// Accumulo durability contract and fence have both been satisfied.
	AuthorityAccumuloWAL
)

// CommitRequest is delivered to one hosted tablet. OperationID is stable
// across retries and must be deduplicated by the authoritative commit layer.
type CommitRequest struct {
	OperationID string
	SessionID   string
	RequestID   string
	Extent      Extent
	Fence       Fence
	Mutations   []Mutation
}

// HostedTablet is the narrow future adapter point for a loaded tablet. Commit
// must verify Fence atomically with the authoritative write.
type HostedTablet interface {
	Extent() Extent
	Fence() Fence
	Authority() CommitAuthority
	Commit(context.Context, CommitRequest) error
}

// Cell is one tablet cell visible to conditional mutation evaluation.
type Cell struct {
	Row              []byte
	ColumnFamily     []byte
	ColumnQualifier  []byte
	ColumnVisibility []byte
	Timestamp        int64
	Value            []byte
	Deleted          bool
}

// ConditionalEvaluator reads immutable tablet state and evaluates conditions
// after overlaying the supplied active, unflushed cells.
type ConditionalEvaluator func(context.Context, []Cell) (bool, error)

// ConditionalTablet serializes condition evaluation and its accepted commit
// with all other tablet operations.
type ConditionalTablet interface {
	HostedTablet
	ConditionalCommit(context.Context, CommitRequest, ConditionalEvaluator) (bool, error)
}

// Directory resolves only currently hosted tablets.
type Directory interface {
	Lookup(context.Context, Extent) (HostedTablet, error)
}

// RouteError carries replacement extents for stale-assignment retry maps.
type RouteError struct {
	Cause        error
	RetryExtents []Extent
}

func (e *RouteError) Error() string {
	if e == nil || e.Cause == nil {
		return "ingestrouter: route failed"
	}
	return e.Cause.Error()
}

func (e *RouteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// OutcomeStatus is the per-extent result of a partially successful request.
type OutcomeStatus uint8

const (
	OutcomeApplied OutcomeStatus = iota + 1
	OutcomeRetry
	OutcomeRejected
)

// Outcome records one extent's terminal or retryable result.
type Outcome struct {
	Status       OutcomeStatus
	Cause        error
	RetryExtents []Extent
}

// Result exposes successful, retryable, and rejected extents without hiding a
// partial commit behind a single success-shaped error.
type Result struct {
	Outcomes map[string]Outcome
}

// Applied reports whether every batch was authoritatively committed.
func (r Result) Applied() bool {
	if len(r.Outcomes) == 0 {
		return false
	}
	for _, outcome := range r.Outcomes {
		if outcome.Status != OutcomeApplied {
			return false
		}
	}
	return true
}

// RetryMap returns only retryable extents and any replacement extents supplied
// by the directory. The returned map and extents are defensive copies.
func (r Result) RetryMap() map[string][]Extent {
	retries := make(map[string][]Extent)
	for key, outcome := range r.Outcomes {
		if outcome.Status != OutcomeRetry {
			continue
		}
		retries[key] = cloneOutcome(outcome).RetryExtents
	}
	return retries
}

func cloneOutcome(in Outcome) Outcome {
	out := in
	out.RetryExtents = make([]Extent, len(in.RetryExtents))
	for i := range in.RetryExtents {
		out.RetryExtents[i] = in.RetryExtents[i].clone()
	}
	return out
}
