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

package explorercoord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

const MaxPruneTargets = 256

// PruneTarget identifies one exact committed physical value and the live
// entity guard that owns its retention lifetime. Cell should come directly
// from ScanCommitted or ReadCommittedCell.
type PruneTarget struct {
	Table  string
	Cell   CommittedCell
	Entity guard.Entity
}

// PruneCheckpoint is a caller-owned local readable-floor update committed in
// the same transaction as the physical tombstones and entity retirements. It
// deliberately does not update the shared allocator HistoryFloor.
type PruneCheckpoint struct {
	Cell  Cell
	Guard GuardIntent
}

// PruneCommittedRequest retires a bounded page of committed cells and advances
// one domain-local readable-floor checkpoint atomically.
type PruneCommittedRequest struct {
	Operation  []byte
	Token      []byte
	Targets    []PruneTarget
	Checkpoint PruneCheckpoint
	Results    []ResultIdentity
	Owner      coordination.OwnerID
	LeaseUntil time.Time
}

type PruneCommittedResult struct {
	Result
	Pruned int
}

// PruneCommitted verifies every target against its immutable committed proof,
// writes epoch-stamped physical tombstones, retires each target entity guard,
// and advances a caller-owned local floor in one existing transaction.
//
// Callers page candidates with ScanCommitted and pass at most MaxPruneTargets.
// The shared allocator HistoryFloor is neither read as the requested floor nor
// mutated by this API.
func (r *Runtime) PruneCommitted(
	ctx context.Context,
	request PruneCommittedRequest,
) (PruneCommittedResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return PruneCommittedResult{}, transaction.ErrUnavailable
	}
	if len(request.Targets) == 0 || len(request.Targets) > MaxPruneTargets {
		return PruneCommittedResult{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune target count is outside its bound"),
		)
	}
	txn, err := DeriveTXN(r.domain, request.Operation, request.Token)
	if err != nil {
		return PruneCommittedResult{}, err
	}
	if stored, loadErr := r.intents.Load(ctx, txn); loadErr == nil {
		matches, matchErr := r.pruneIntentMatchesRequest(
			ctx,
			stored.Intent,
			request,
		)
		if matchErr != nil {
			return PruneCommittedResult{}, matchErr
		}
		if !matches {
			return PruneCommittedResult{}, transaction.ErrConflict
		}
		result, publishErr := r.publishLocked(ctx, Request{
			Intent: stored.Intent, Owner: request.Owner,
			LeaseUntil: request.LeaseUntil,
		})
		if publishErr != nil {
			return PruneCommittedResult{}, publishErr
		}
		return PruneCommittedResult{
			Result: result,
			Pruned: len(request.Targets),
		}, nil
	} else if !errors.Is(loadErr, transaction.ErrNotFound) {
		return PruneCommittedResult{}, loadErr
	}
	intent, err := r.buildPruneIntentLocked(ctx, request)
	if err != nil {
		return PruneCommittedResult{}, err
	}
	result, err := r.publishLocked(ctx, Request{
		Intent: intent, Owner: request.Owner,
		LeaseUntil: request.LeaseUntil,
	})
	if err != nil {
		return PruneCommittedResult{}, err
	}
	return PruneCommittedResult{
		Result: result,
		Pruned: len(request.Targets),
	}, nil
}

func (r *Runtime) buildPruneIntentLocked(
	ctx context.Context,
	request PruneCommittedRequest,
) (Intent, error) {
	checkpointCell := cloneCell(request.Checkpoint.Cell)
	checkpointGuard := cloneGuardIntent(request.Checkpoint.Guard)
	if checkpointCell.CopyGeneration == 0 {
		checkpointCell.CopyGeneration = 1
	}
	if checkpointGuard.DesiredState == 0 {
		checkpointGuard.DesiredState = guard.StateLive
	}
	if checkpointGuard.RetirementGeneration == 0 {
		checkpointGuard.RetirementGeneration = 1
	}
	if checkpointCell.Delete || !checkpointCell.EpochTimestamp {
		return Intent{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune checkpoint must be an epoch-stamped value"),
		)
	}
	if checkpointGuard.Mode != guard.ModeAbsentOrIdentical &&
		checkpointGuard.Mode != guard.ModeMutate {
		return Intent{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune checkpoint guard must create or mutate"),
		)
	}
	if checkpointGuard.DesiredState != guard.StateLive ||
		!bytes.Equal(checkpointCell.LPART, checkpointGuard.LPART) {
		return Intent{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune checkpoint guard disagrees with its physical cell"),
		)
	}

	intent := Intent{
		Operation: append([]byte(nil), request.Operation...),
		Token:     append([]byte(nil), request.Token...),
		Cells:     make([]Cell, 0, len(request.Targets)+1),
		Guards:    make([]GuardIntent, 0, len(request.Targets)+1),
		Results:   cloneResultIntents(request.Results),
	}
	entities := make(map[string]struct{}, len(request.Targets)+1)
	for index, target := range request.Targets {
		deleteCell, retireGuard, err := r.buildPruneTargetLocked(ctx, target)
		if err != nil {
			return Intent{}, fmt.Errorf("prune target %d: %w", index, err)
		}
		entityKey := pruneEntityKey(target.Entity)
		if _, duplicate := entities[entityKey]; duplicate {
			return Intent{}, errors.Join(
				transaction.ErrInvalid,
				errors.New("prune targets contain a duplicate entity"),
			)
		}
		entities[entityKey] = struct{}{}
		intent.Cells = append(intent.Cells, deleteCell)
		intent.Guards = append(intent.Guards, retireGuard)
	}
	if _, duplicate := entities[pruneEntityKey(checkpointGuard.Entity)]; duplicate {
		return Intent{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune checkpoint entity is also a target"),
		)
	}
	intent.Cells = append(intent.Cells, checkpointCell)
	intent.Guards = append(intent.Guards, checkpointGuard)
	normalized, _, _, err := canonicalIntent(intent)
	return normalized, err
}

func (r *Runtime) buildPruneTargetLocked(
	ctx context.Context,
	target PruneTarget,
) (Cell, GuardIntent, error) {
	if target.Entity.Kind == 0 {
		return Cell{}, GuardIntent{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("prune entity kind is required"),
		)
	}
	if err := target.Entity.ID.Validate(); err != nil {
		return Cell{}, GuardIntent{}, errors.Join(transaction.ErrInvalid, err)
	}
	actual, found, err := r.readCommittedCellLocked(
		ctx,
		target.Table,
		target.Cell.Cell.Coordinate.Row,
		target.Cell.Cell.Coordinate.Family,
		target.Cell.Cell.Coordinate.Qualifier,
		target.Cell.Cell.Coordinate.Visibility,
		target.Cell.Epoch,
		MaxCommittedScanCells,
	)
	if err != nil {
		return Cell{}, GuardIntent{}, err
	}
	if !found || !committedCellEqual(actual, target.Cell) {
		return Cell{}, GuardIntent{}, transaction.ErrConflict
	}
	record, err := r.intents.Load(ctx, actual.TXN)
	if err != nil {
		return Cell{}, GuardIntent{}, err
	}
	sourceCell, found, err := sourceIntentCell(
		record.Intent,
		target.Table,
		actual,
	)
	if err != nil {
		return Cell{}, GuardIntent{}, err
	}
	if !found || !sourceCell.EpochTimestamp || sourceCell.Delete {
		return Cell{}, GuardIntent{}, fmt.Errorf(
			"%w: committed prune source is not an epoch-stamped value",
			transaction.ErrInternal,
		)
	}
	sourceGuard, found, err := sourceIntentGuard(record.Intent, target.Entity)
	if err != nil {
		return Cell{}, GuardIntent{}, err
	}
	if !found || sourceGuard.DesiredState != guard.StateLive ||
		!bytes.Equal(sourceGuard.LPART, sourceCell.LPART) {
		return Cell{}, GuardIntent{}, fmt.Errorf(
			"%w: committed prune source has no live owning guard",
			transaction.ErrInternal,
		)
	}
	head, _, err := r.guards.Read(ctx, target.Entity)
	if err != nil {
		return Cell{}, GuardIntent{}, err
	}
	if head == nil || head.State != guard.StateLive ||
		head.Epoch != actual.Epoch ||
		head.LogicalDigest != actual.LogicalDigest ||
		!bytes.Equal(head.LPART, sourceCell.LPART) {
		return Cell{}, GuardIntent{}, transaction.ErrConflict
	}
	return Cell{
			Table:           sourceCell.Table,
			Row:             append([]byte(nil), sourceCell.Row...),
			Family:          append([]byte(nil), sourceCell.Family...),
			Qualifier:       append([]byte(nil), sourceCell.Qualifier...),
			Visibility:      append([]byte(nil), sourceCell.Visibility...),
			Delete:          true,
			EpochTimestamp:  true,
			LPART:           append(coordination.LPART(nil), sourceCell.LPART...),
			CopyGeneration:  sourceCell.CopyGeneration,
			IndexGeneration: append(coordination.IGEN(nil), sourceCell.IndexGeneration...),
			IndexFamily:     append(coordination.Family(nil), sourceCell.IndexFamily...),
		}, GuardIntent{
			Entity:               target.Entity,
			Mode:                 guard.ModeRetire,
			ExpectedEpoch:        head.Epoch,
			ExpectedDigest:       head.LogicalDigest,
			DesiredState:         guard.StateTombstone,
			DesiredWinnerID:      append([]byte(nil), head.WinnerID...),
			LPART:                append(coordination.LPART(nil), head.LPART...),
			LogicalPolicyID:      append([]byte(nil), head.LogicalPolicyID...),
			RetirementGeneration: head.RetirementGeneration,
		}, nil
}

func (r *Runtime) pruneIntentMatchesRequest(
	ctx context.Context,
	intent Intent,
	request PruneCommittedRequest,
) (bool, error) {
	if !bytes.Equal(intent.Operation, request.Operation) ||
		!bytes.Equal(intent.Token, request.Token) ||
		len(intent.Cells) != len(request.Targets)+1 ||
		len(intent.Guards) != len(request.Targets)+1 ||
		!resultIntentsEqual(intent.Results, request.Results) {
		return false, nil
	}
	checkpointCell := cloneCell(request.Checkpoint.Cell)
	checkpointGuard := cloneGuardIntent(request.Checkpoint.Guard)
	if checkpointCell.CopyGeneration == 0 {
		checkpointCell.CopyGeneration = 1
	}
	if checkpointGuard.DesiredState == 0 {
		checkpointGuard.DesiredState = guard.StateLive
	}
	if checkpointGuard.RetirementGeneration == 0 {
		checkpointGuard.RetirementGeneration = 1
	}
	checkpointFound := false
	for _, cell := range intent.Cells {
		if !cell.Delete && intentCellEqual(cell, checkpointCell) {
			checkpointFound = true
			break
		}
	}
	if !checkpointFound {
		return false, nil
	}
	checkpointGuardFound := false
	for _, value := range intent.Guards {
		if value.Mode != guard.ModeRetire &&
			guardIntentEqual(value, checkpointGuard) {
			checkpointGuardFound = true
			break
		}
	}
	if !checkpointGuardFound {
		return false, nil
	}
	for _, target := range request.Targets {
		sourceResult, committed, err := r.committedLocked(
			ctx,
			target.Cell.TXN,
			target.Cell.LogicalDigest,
		)
		if err != nil {
			return false, err
		}
		if !committed || sourceResult.Epoch != target.Cell.Epoch {
			return false, nil
		}
		source, err := r.intents.Load(ctx, target.Cell.TXN)
		if err != nil {
			return false, err
		}
		if source.LogicalDigest != target.Cell.LogicalDigest {
			return false, nil
		}
		if _, found, err := sourceIntentCell(
			source.Intent,
			target.Table,
			target.Cell,
		); err != nil {
			return false, err
		} else if !found {
			return false, nil
		}
		if sourceGuard, found, err := sourceIntentGuard(
			source.Intent,
			target.Entity,
		); err != nil {
			return false, err
		} else if !found ||
			sourceGuard.DesiredState != guard.StateLive {
			return false, nil
		}
		cellFound := false
		for _, cell := range intent.Cells {
			if cell.Delete &&
				cell.Table == target.Table &&
				bytes.Equal(cell.Row, target.Cell.Cell.Coordinate.Row) &&
				bytes.Equal(cell.Family, target.Cell.Cell.Coordinate.Family) &&
				bytes.Equal(cell.Qualifier, target.Cell.Cell.Coordinate.Qualifier) &&
				bytes.Equal(cell.Visibility, target.Cell.Cell.Coordinate.Visibility) {
				cellFound = true
				break
			}
		}
		guardFound := false
		for _, value := range intent.Guards {
			if value.Mode == guard.ModeRetire &&
				entityEqual(value.Entity, target.Entity) &&
				value.ExpectedEpoch == target.Cell.Epoch &&
				value.ExpectedDigest == target.Cell.LogicalDigest {
				guardFound = true
				break
			}
		}
		if !cellFound || !guardFound {
			return false, nil
		}
	}
	return true, nil
}

func committedCellEqual(left, right CommittedCell) bool {
	return left.Epoch == right.Epoch &&
		bytes.Equal(left.TXN, right.TXN) &&
		left.LogicalDigest == right.LogicalDigest &&
		left.Cell.Timestamp == right.Cell.Timestamp &&
		bytes.Equal(left.Cell.Coordinate.Row, right.Cell.Coordinate.Row) &&
		bytes.Equal(left.Cell.Coordinate.Family, right.Cell.Coordinate.Family) &&
		bytes.Equal(left.Cell.Coordinate.Qualifier, right.Cell.Coordinate.Qualifier) &&
		bytes.Equal(left.Cell.Coordinate.Visibility, right.Cell.Coordinate.Visibility) &&
		bytes.Equal(left.Cell.Value, right.Cell.Value)
}

func sourceIntentCell(
	intent Intent,
	table string,
	target CommittedCell,
) (Cell, bool, error) {
	var result Cell
	found := false
	for _, cell := range intent.Cells {
		if cell.Table == table &&
			bytes.Equal(cell.Row, target.Cell.Coordinate.Row) &&
			bytes.Equal(cell.Family, target.Cell.Coordinate.Family) &&
			bytes.Equal(cell.Qualifier, target.Cell.Coordinate.Qualifier) &&
			bytes.Equal(cell.Visibility, target.Cell.Coordinate.Visibility) &&
			bytes.Equal(cell.Value, target.Cell.Value) {
			if found {
				return Cell{}, false, fmt.Errorf(
					"%w: committed prune source has duplicate cells",
					transaction.ErrInternal,
				)
			}
			result = cloneCell(cell)
			found = true
		}
	}
	return result, found, nil
}

func sourceIntentGuard(
	intent Intent,
	entity guard.Entity,
) (GuardIntent, bool, error) {
	var result GuardIntent
	found := false
	for _, value := range intent.Guards {
		if entityEqual(value.Entity, entity) {
			if found {
				return GuardIntent{}, false, fmt.Errorf(
					"%w: committed prune source has duplicate guards",
					transaction.ErrInternal,
				)
			}
			result = cloneGuardIntent(value)
			found = true
		}
	}
	return result, found, nil
}

func cloneCell(cell Cell) Cell {
	result := cell
	result.Row = append([]byte(nil), cell.Row...)
	result.Family = append([]byte(nil), cell.Family...)
	result.Qualifier = append([]byte(nil), cell.Qualifier...)
	result.Visibility = append([]byte(nil), cell.Visibility...)
	result.Value = append([]byte(nil), cell.Value...)
	result.LPART = append(coordination.LPART(nil), cell.LPART...)
	result.IndexGeneration = append(coordination.IGEN(nil), cell.IndexGeneration...)
	result.IndexFamily = append(coordination.Family(nil), cell.IndexFamily...)
	return result
}

func cloneGuardIntent(value GuardIntent) GuardIntent {
	result := value
	result.Entity.ID = append(coordination.EntityID(nil), value.Entity.ID...)
	result.DesiredWinnerID = append([]byte(nil), value.DesiredWinnerID...)
	result.LPART = append(coordination.LPART(nil), value.LPART...)
	result.LogicalPolicyID = append([]byte(nil), value.LogicalPolicyID...)
	return result
}

func cloneResultIntents(values []ResultIdentity) []ResultIdentity {
	result := make([]ResultIdentity, len(values))
	for index := range values {
		result[index] = ResultIdentity{
			Kind: append([]byte(nil), values[index].Kind...),
			ID:   append([]byte(nil), values[index].ID...),
		}
	}
	return result
}

func resultIntentsEqual(left, right []ResultIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	left = cloneResultIntents(left)
	right = cloneResultIntents(right)
	sortResultIntents(left)
	sortResultIntents(right)
	for index := range left {
		if !bytes.Equal(left[index].Kind, right[index].Kind) ||
			!bytes.Equal(left[index].ID, right[index].ID) {
			return false
		}
	}
	return true
}

func sortResultIntents(values []ResultIdentity) {
	sort.Slice(values, func(i, j int) bool {
		if order := bytes.Compare(values[i].Kind, values[j].Kind); order != 0 {
			return order < 0
		}
		return bytes.Compare(values[i].ID, values[j].ID) < 0
	})
}

func intentCellEqual(left, right Cell) bool {
	return left.Table == right.Table &&
		bytes.Equal(left.Row, right.Row) &&
		bytes.Equal(left.Family, right.Family) &&
		bytes.Equal(left.Qualifier, right.Qualifier) &&
		bytes.Equal(left.Visibility, right.Visibility) &&
		bytes.Equal(left.Value, right.Value) &&
		left.Delete == right.Delete &&
		left.EpochTimestamp == right.EpochTimestamp &&
		left.Timestamp == right.Timestamp &&
		bytes.Equal(left.LPART, right.LPART) &&
		left.CopyGeneration == right.CopyGeneration &&
		bytes.Equal(left.IndexGeneration, right.IndexGeneration) &&
		bytes.Equal(left.IndexFamily, right.IndexFamily)
}

func guardIntentEqual(left, right GuardIntent) bool {
	return entityEqual(left.Entity, right.Entity) &&
		left.Mode == right.Mode &&
		left.ExpectedEpoch == right.ExpectedEpoch &&
		left.ExpectedDigest == right.ExpectedDigest &&
		left.DesiredState == right.DesiredState &&
		bytes.Equal(left.DesiredWinnerID, right.DesiredWinnerID) &&
		left.DesiredDigest == right.DesiredDigest &&
		bytes.Equal(left.LPART, right.LPART) &&
		bytes.Equal(left.LogicalPolicyID, right.LogicalPolicyID) &&
		left.RetirementGeneration == right.RetirementGeneration
}

func entityEqual(left, right guard.Entity) bool {
	return left.Kind == right.Kind && bytes.Equal(left.ID, right.ID)
}

func pruneEntityKey(entity guard.Entity) string {
	return string([]byte{entity.Kind}) + "\x00" + string(entity.ID)
}
