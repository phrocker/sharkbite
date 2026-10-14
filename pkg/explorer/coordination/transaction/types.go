/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package transaction coordinates durable Explorer publication transactions.
package transaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var (
	ErrInvalid     = errors.New("explorer transaction: invalid argument")
	ErrConflict    = errors.New("explorer transaction: conflict")
	ErrUnavailable = errors.New("explorer transaction: unavailable")
	ErrInternal    = errors.New("explorer transaction: internal")
	ErrQuarantined = errors.New("explorer transaction: quarantined")
	ErrNotFound    = errors.New("explorer transaction: not found")
)

// PublicError maps internal coordination failures to stable, redacted Explorer
// semantics without exposing rows, visibilities, owners, fences, or digests.
func PublicError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return shoal.WrapError(shoal.ErrorCanceled, "transaction canceled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return shoal.WrapError(shoal.ErrorDeadline, "transaction deadline exceeded", err)
	case errors.Is(err, ErrInvalid):
		return shoal.WrapError(shoal.ErrorInvalidArgument, "invalid transaction", err)
	case errors.Is(err, ErrConflict):
		return shoal.WrapError(shoal.ErrorConflict, "transaction conflicts with existing state", err)
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotFound):
		return shoal.WrapError(shoal.ErrorUnavailable, "transaction is not currently available", err)
	default:
		return shoal.WrapError(shoal.ErrorInternal, "transaction consistency failure", err)
	}
}

const MaxCommitMutationBytes = 8 << 20

type Store interface {
	allocator.Store
}

type Allocator interface {
	CurrentHead(context.Context) (coordination.AllocatorHeadV1, error)
	Reserve(context.Context, allocator.ReserveRequest) (coordination.ReservationV1, error)
	Reservation(context.Context, coordination.Epoch) (coordination.ReservationV1, error)
	TakeoverReservation(context.Context, coordination.ReservationV1, coordination.OwnerID, time.Time, coordination.Fence, time.Time) (coordination.ReservationV1, error)
	Outcome(context.Context, coordination.Epoch) (coordination.EpochOutcomeV1, error)
	Terminalize(context.Context, coordination.ReservationV1, coordination.TxnState) (allocator.CompletionState, coordination.EpochOutcomeV1, error)
	AdvanceFrontier(context.Context) (coordination.FrontierCheckpointV1, error)
	Retire(context.Context) (coordination.AllocatorHeadV1, error)
}

type Guards interface {
	Read(context.Context, guard.Entity) (*guard.Head, *guard.Pending, error)
	AcquireMany(context.Context, []guard.Intent) ([]guard.Acquisition, error)
	Takeover(context.Context, guard.Pending, coordination.OwnerID, time.Time, coordination.Fence, time.Time) (guard.TakeoverResult, error)
	Prepare(context.Context, guard.Pending, guard.Published, time.Time) (guard.Pending, error)
	Commit(context.Context, guard.Pending, guard.Published, time.Time) (guard.Head, error)
	Abort(context.Context, guard.Pending, bool) error
}

type Authority struct {
	Generation          coordination.Generation
	Fence               coordination.Fence
	Holder              coordination.OwnerID
	Mode                coordination.WriterMode
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
}

type Publication struct {
	TXN           coordination.TXN
	Token         []byte
	LogicalDigest coordination.Digest
	Owner         coordination.OwnerID
	LeaseUntil    time.Time
	Authority     Authority
}

type GuardPlan struct {
	Entity               guard.Entity
	Mode                 guard.Mode
	ExpectedEpoch        coordination.Epoch
	ExpectedDigest       coordination.Digest
	DesiredState         guard.EntityState
	DesiredWinnerID      []byte
	DesiredDigest        coordination.Digest
	LPART                coordination.LPART
	LogicalPolicyID      []byte
	RetirementGeneration coordination.Generation
	ManifestChunk        uint32
	ManifestEntry        uint32
	Ordinal              uint32
	PhysicalDigest       coordination.Digest
}

type PhysicalCell struct {
	Entry      coordination.ManifestEntry
	Value      []byte
	Visibility []byte
	Delete     bool
}

var physicalDeleteDigest = coordination.Sum([]byte("shoal-physical-delete-v1"))

// PhysicalValueCommitment returns the manifest value commitment for a physical
// write or tombstone. Tombstones use a domain-separated digest so they cannot
// be confused with an ordinary empty value without changing the manifest wire
// format.
func PhysicalValueCommitment(
	delete bool,
	value []byte,
) (uint32, coordination.Digest, error) {
	if delete {
		if len(value) != 0 {
			return 0, coordination.Digest{}, errors.Join(
				ErrInvalid,
				errors.New("physical tombstone cannot carry a value"),
			)
		}
		return 0, physicalDeleteDigest, nil
	}
	if len(value) > coordination.MaxManifestValueBytes {
		return 0, coordination.Digest{}, errors.Join(
			ErrInvalid,
			errors.New("physical value exceeds its bound"),
		)
	}
	return uint32(len(value)), coordination.Sum(value), nil
}

type CommitCopy struct {
	LPART                 coordination.LPART
	CopyGeneration        coordination.Generation
	VisibilityDigest      coordination.Digest
	LogicalDigest         coordination.Digest
	PhysicalCopyDigest    coordination.Digest
	RequiredIndexFamilies []coordination.Family
	Visibility            []byte
}

type Plan struct {
	Chunks  []coordination.ManifestChunkV2
	Cells   []PhysicalCell
	Guards  []GuardPlan
	Copies  []CommitCopy
	Results []coordination.ResultIdentity
}

func (p Plan) Validate() (coordination.ManifestSummary, error) {
	summary, err := coordination.VerifyManifest(p.Chunks)
	if err != nil {
		return summary, errors.Join(ErrInvalid, err)
	}
	entries := make([]coordination.ManifestEntry, 0, summary.TotalEntries)
	for _, chunk := range p.Chunks {
		entries = append(entries, chunk.Entries...)
	}
	if len(entries) != len(p.Cells) {
		return summary, fmt.Errorf("%w: physical cells do not cover the manifest", ErrInvalid)
	}
	for i := range entries {
		cell := p.Cells[i]
		valueLength, valueDigest, valueErr := PhysicalValueCommitment(
			cell.Delete,
			cell.Value,
		)
		if valueErr != nil {
			return summary, fmt.Errorf(
				"physical cell %d commitment is invalid: %w",
				i,
				valueErr,
			)
		}
		if coordination.CompareManifestEntries(entries[i], cell.Entry) != 0 ||
			!manifestEntryEqual(entries[i], cell.Entry) ||
			valueLength != cell.Entry.ValueLength ||
			valueDigest != cell.Entry.ValueDigest ||
			coordination.Sum(cell.Visibility) != cell.Entry.VisibilityDigest {
			return summary, fmt.Errorf("%w: physical cell %d disagrees with the manifest", ErrInvalid, i)
		}
	}
	if len(p.Copies) == 0 || len(p.Copies) > coordination.MaxLPARTs {
		return summary, fmt.Errorf("%w: commit-copy count is outside its bound", ErrInvalid)
	}
	for i := range p.Copies {
		copy := p.Copies[i]
		if err := copy.LPART.Validate(); err != nil {
			return summary, errors.Join(ErrInvalid, err)
		}
		if err := copy.CopyGeneration.Validate(); err != nil {
			return summary, errors.Join(ErrInvalid, err)
		}
		if coordination.Sum(copy.Visibility) != copy.VisibilityDigest {
			return summary, fmt.Errorf("%w: commit-copy visibility digest mismatch", ErrInvalid)
		}
		if i > 0 && compareCopies(p.Copies[i-1], copy) >= 0 {
			return summary, fmt.Errorf("%w: commit copies must be strictly ordered", ErrInvalid)
		}
	}
	if len(p.Guards) > guard.MaxEntities {
		return summary, fmt.Errorf("%w: guard count exceeds its bound", ErrInvalid)
	}
	results := append([]coordination.ResultIdentity(nil), p.Results...)
	coordination.SortResultIdentities(results)
	for i := range results {
		if !bytes.Equal(results[i].Kind, p.Results[i].Kind) || !bytes.Equal(results[i].ID, p.Results[i].ID) {
			return summary, fmt.Errorf("%w: results must be strictly ordered", ErrInvalid)
		}
	}
	return summary, nil
}

type MaterializeRequest struct {
	TXN           coordination.TXN
	LogicalDigest coordination.Digest
}

// Materializer deterministically reconstructs a plan without exposing storage
// coordinates through the high-level Publish API.
type Materializer interface {
	Materialize(context.Context, MaterializeRequest) (Plan, error)
}

type PhysicalWriter interface {
	Write(context.Context, coordination.Epoch, []PhysicalCell) error
}

type PhysicalVerifier interface {
	Verify(context.Context, coordination.Epoch, []PhysicalCell) error
}

type PinValidator interface {
	Validate(context.Context, coordination.TxnRootV3, Plan) error
}

type Quarantine interface {
	Record(context.Context, coordination.DomainID, coordination.TXN, string) error
}

type Result struct {
	Epoch      coordination.Epoch
	Identities []coordination.ResultIdentity
	Unchanged  bool
}

type Snapshot struct {
	Root  coordination.TxnRootV3
	Lease coordination.TxnLeaseV1
}

func compareCopies(a, b CommitCopy) int {
	return bytes.Compare(a.LPART, b.LPART)
}

func sortedLPARTs(copies []CommitCopy) []coordination.LPART {
	result := make([]coordination.LPART, len(copies))
	for i := range copies {
		result[i] = append(coordination.LPART(nil), copies[i].LPART...)
	}
	coordination.SortLPARTs(result)
	return result
}

func copyResults(values []coordination.ResultIdentity) []coordination.ResultIdentity {
	result := make([]coordination.ResultIdentity, len(values))
	for i := range values {
		result[i].Kind = append([]byte(nil), values[i].Kind...)
		result[i].ID = append([]byte(nil), values[i].ID...)
	}
	return result
}

func clonePlan(plan Plan) Plan {
	result := plan
	result.Chunks = append([]coordination.ManifestChunkV2(nil), plan.Chunks...)
	result.Cells = append([]PhysicalCell(nil), plan.Cells...)
	for i := range result.Cells {
		result.Cells[i].Value = append([]byte(nil), plan.Cells[i].Value...)
		result.Cells[i].Visibility = append([]byte(nil), plan.Cells[i].Visibility...)
	}
	result.Guards = append([]GuardPlan(nil), plan.Guards...)
	result.Copies = append([]CommitCopy(nil), plan.Copies...)
	for i := range result.Copies {
		result.Copies[i].Visibility = append([]byte(nil), plan.Copies[i].Visibility...)
		result.Copies[i].RequiredIndexFamilies = append([]coordination.Family(nil), plan.Copies[i].RequiredIndexFamilies...)
		sort.Slice(result.Copies[i].RequiredIndexFamilies, func(a, b int) bool {
			return bytes.Compare(result.Copies[i].RequiredIndexFamilies[a], result.Copies[i].RequiredIndexFamilies[b]) < 0
		})
	}
	result.Results = copyResults(plan.Results)
	return result
}

func manifestEntryEqual(a, b coordination.ManifestEntry) bool {
	return bytes.Equal(a.Table, b.Table) && bytes.Equal(a.Row, b.Row) &&
		bytes.Equal(a.ColumnFamily, b.ColumnFamily) && bytes.Equal(a.ColumnQualifier, b.ColumnQualifier) &&
		a.EpochSlot == b.EpochSlot && a.ExplicitTimestamp == b.ExplicitTimestamp &&
		a.ValueLength == b.ValueLength && a.ValueDigest == b.ValueDigest &&
		bytes.Equal(a.LPART, b.LPART) && a.CopyGeneration == b.CopyGeneration &&
		a.VisibilityDigest == b.VisibilityDigest && a.LogicalDigest == b.LogicalDigest &&
		a.PhysicalCopyDigest == b.PhysicalCopyDigest && bytes.Equal(a.IGEN, b.IGEN) &&
		bytes.Equal(a.Family, b.Family)
}
