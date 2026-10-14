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
	"math"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

const (
	DefaultCommittedScanLimit      = 100
	MaxCommittedScanLimit          = 10_000
	DefaultCommittedScanMultiplier = 64
	MaxCommittedScanCells          = 100_000
)

type CommittedScanRequest struct {
	Table     string
	RowPrefix []byte
	StartRow  []byte
	// StartAfterRow is an exclusive row cursor. It is mutually exclusive with
	// StartRow and is the preferred paging input for domain adapters.
	StartAfterRow []byte
	Family        []byte
	Qualifier     []byte
	Visibility    []byte
	Frontier      coordination.Epoch
	Limit         int
	MaxScanned    int
}

type CommittedCell struct {
	Cell          allocator.Cell
	TXN           coordination.TXN
	LogicalDigest coordination.Digest
	Epoch         coordination.Epoch
}

type CommittedPage struct {
	Cells        []CommittedCell
	Frontier     coordination.Epoch
	HistoryFloor coordination.Epoch
	NextRow      []byte
	Scanned      int
}

type physicalVersion struct {
	key   wire.Key
	value []byte
}

type publicationProof struct {
	committed bool
	txn       coordination.TXN
	digest    coordination.Digest
	plan      transaction.Plan
}

// ReadCommittedCell hydrates one exact physical coordinate at a required
// committed epoch. The bool is false when that exact committed version does
// not exist; a newer row sharing the byte prefix is never returned.
func (r *Runtime) ReadCommittedCell(
	ctx context.Context,
	table string,
	row, family, qualifier, visibility []byte,
	epoch coordination.Epoch,
) (CommittedCell, bool, error) {
	return r.readCommittedCell(
		ctx,
		table,
		row,
		family,
		qualifier,
		visibility,
		epoch,
		MaxCommittedScanCells,
	)
}

func (r *Runtime) readCommittedCell(
	ctx context.Context,
	table string,
	row, family, qualifier, visibility []byte,
	epoch coordination.Epoch,
	maxScanned int,
) (CommittedCell, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return CommittedCell{}, false, transaction.ErrUnavailable
	}
	return r.readCommittedCellLocked(
		ctx,
		table,
		row,
		family,
		qualifier,
		visibility,
		epoch,
		maxScanned,
	)
}

func (r *Runtime) readCommittedCellLocked(
	ctx context.Context,
	table string,
	row, family, qualifier, visibility []byte,
	epoch coordination.Epoch,
	maxScanned int,
) (CommittedCell, bool, error) {
	if err := epoch.Validate(); err != nil {
		return CommittedCell{}, false, errors.Join(transaction.ErrInvalid, err)
	}
	if maxScanned < 1 || maxScanned > MaxCommittedScanCells {
		return CommittedCell{}, false, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed exact read work limit is outside its bound"),
		)
	}
	if _, allowed := r.physicalTables[table]; !allowed {
		return CommittedCell{}, false, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed read table was not configured for the runtime"),
		)
	}
	if len(row) == 0 || len(row) > coordination.MaxCoordinateBytes ||
		len(family) == 0 || len(family) > coordination.MaxCoordinateBytes ||
		len(qualifier) == 0 || len(qualifier) > coordination.MaxCoordinateBytes ||
		len(visibility) > coordination.MaxCoordinateBytes {
		return CommittedCell{}, false, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed read coordinate is outside its bound"),
		)
	}
	head, err := r.allocator.CurrentHead(ctx)
	if err != nil {
		return CommittedCell{}, false, err
	}
	if epoch > head.Frontier {
		return CommittedCell{}, false, errors.Join(
			transaction.ErrUnavailable,
			errors.New("requested committed epoch is not available"),
		)
	}
	if epoch < head.HistoryFloor {
		return CommittedCell{}, false, errors.Join(
			transaction.ErrConflict,
			errors.New("requested committed epoch is below the history floor"),
		)
	}
	scanner, err := r.engine.Scan(table, exactRowRange(row), engine.ScanOptions{
		ColumnFamilies:          [][]byte{append([]byte(nil), family...)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return CommittedCell{}, false, err
	}
	defer scanner.Close()
	versions := make([]physicalVersion, 0, 4)
	scanned := 0
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return CommittedCell{}, false, err
		}
		key := scanner.Key()
		if bytes.Equal(key.Row, row) &&
			bytes.Equal(key.ColumnFamily, family) &&
			bytes.Equal(key.ColumnQualifier, qualifier) &&
			bytes.Equal(key.ColumnVisibility, visibility) {
			scanned++
			if scanned > maxScanned {
				return CommittedCell{}, false, errors.Join(
					transaction.ErrUnavailable,
					errors.New("committed exact read work limit exhausted"),
				)
			}
			versions = append(versions, physicalVersion{
				key:   *key.Clone(),
				value: append([]byte(nil), scanner.Value()...),
			})
		}
		if err := scanner.Advance(); err != nil {
			return CommittedCell{}, false, err
		}
	}
	cell, found, err := r.selectCommittedVersion(
		ctx,
		table,
		versions,
		epoch,
		make(map[coordination.Epoch]publicationProof),
	)
	if err != nil {
		return CommittedCell{}, false, err
	}
	if !found || cell.Epoch != epoch {
		return CommittedCell{}, false, nil
	}
	return cell, true, nil
}

// Committed reports whether a transaction completed publication, checkpoint,
// and guard finalization at the exact durable logical digest.
func (r *Runtime) Committed(
	ctx context.Context,
	txn coordination.TXN,
	logicalDigest coordination.Digest,
) (Result, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return Result{}, false, transaction.ErrUnavailable
	}
	return r.committedLocked(ctx, txn, logicalDigest)
}

// ScanCommitted returns a bounded page of epoch-stamped physical cells proven
// by the durable intent, committed outcome/root, completion marker, and pinned
// allocator frontier. Newer uncommitted versions do not hide an older
// committed version of the same row coordinate.
func (r *Runtime) ScanCommitted(
	ctx context.Context,
	request CommittedScanRequest,
) (CommittedPage, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return CommittedPage{}, transaction.ErrUnavailable
	}
	if _, allowed := r.physicalTables[request.Table]; !allowed {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan table was not configured for the runtime"),
		)
	}
	if len(request.RowPrefix) == 0 ||
		len(request.RowPrefix) > coordination.MaxCoordinateBytes ||
		len(request.Family) == 0 ||
		len(request.Family) > coordination.MaxCoordinateBytes ||
		len(request.Qualifier) == 0 ||
		len(request.Qualifier) > coordination.MaxCoordinateBytes ||
		len(request.Visibility) > coordination.MaxCoordinateBytes ||
		len(request.StartRow) > coordination.MaxCoordinateBytes ||
		len(request.StartAfterRow) > coordination.MaxCoordinateBytes {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan coordinate is outside its bound"),
		)
	}
	if len(request.StartRow) != 0 && len(request.StartAfterRow) != 0 {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan start cursors are mutually exclusive"),
		)
	}
	if len(request.StartAfterRow) != 0 {
		if !bytes.HasPrefix(request.StartAfterRow, request.RowPrefix) {
			return CommittedPage{}, errors.Join(
				transaction.ErrInvalid,
				errors.New("committed scan exclusive cursor is outside its prefix"),
			)
		}
		request.StartRow = append(append([]byte(nil), request.StartAfterRow...), 0)
	}
	if len(request.StartRow) == 0 {
		request.StartRow = request.RowPrefix
	}
	if !bytes.HasPrefix(request.StartRow, request.RowPrefix) ||
		bytes.Compare(request.StartRow, request.RowPrefix) < 0 {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan start row is outside its prefix"),
		)
	}
	if request.Limit == 0 {
		request.Limit = DefaultCommittedScanLimit
	}
	if request.Limit < 1 || request.Limit > MaxCommittedScanLimit {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan limit is outside its bound"),
		)
	}
	if request.MaxScanned == 0 {
		request.MaxScanned = request.Limit * DefaultCommittedScanMultiplier
		if request.MaxScanned < 1024 {
			request.MaxScanned = 1024
		}
		if request.MaxScanned > MaxCommittedScanCells {
			request.MaxScanned = MaxCommittedScanCells
		}
	}
	if request.MaxScanned < request.Limit ||
		request.MaxScanned > MaxCommittedScanCells {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan work limit is outside its bound"),
		)
	}
	if request.Frontier != 0 {
		if err := request.Frontier.Validate(); err != nil {
			return CommittedPage{}, errors.Join(transaction.ErrInvalid, err)
		}
	}
	head, err := r.allocator.CurrentHead(ctx)
	if err != nil {
		return CommittedPage{}, err
	}
	frontier := request.Frontier
	if frontier == 0 {
		frontier = head.Frontier
	}
	if frontier == 0 {
		return CommittedPage{Frontier: 0, HistoryFloor: head.HistoryFloor}, nil
	}
	if frontier > head.Frontier {
		return CommittedPage{}, errors.Join(
			transaction.ErrUnavailable,
			errors.New("requested committed frontier is not available"),
		)
	}
	if frontier < head.HistoryFloor {
		return CommittedPage{}, errors.Join(
			transaction.ErrConflict,
			errors.New("requested committed frontier is below the history floor"),
		)
	}
	endRow, ok := prefixSuccessor(request.RowPrefix)
	if !ok {
		return CommittedPage{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("committed scan prefix has no bounded successor"),
		)
	}
	scanner, err := r.engine.Scan(request.Table, iterrt.Range{
		Start: &wire.Key{
			Row:       append([]byte(nil), request.StartRow...),
			Timestamp: math.MaxInt64, Deleted: true,
		},
		StartInclusive: true,
		End: &wire.Key{
			Row: endRow, Timestamp: math.MaxInt64, Deleted: true,
		},
		EndInclusive: false,
	}, engine.ScanOptions{
		ColumnFamilies: [][]byte{
			append([]byte(nil), request.Family...),
		},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return CommittedPage{}, err
	}
	defer scanner.Close()

	page := CommittedPage{
		Cells:    make([]CommittedCell, 0, request.Limit),
		Frontier: frontier, HistoryFloor: head.HistoryFloor,
	}
	proofs := make(map[coordination.Epoch]publicationProof)
	var row []byte
	var scanRow []byte
	var versions []physicalVersion
	finishRow := func() (bool, error) {
		if len(versions) == 0 {
			return false, nil
		}
		cell, found, proofErr := r.selectCommittedVersion(
			ctx, request.Table, versions, frontier, proofs,
		)
		if proofErr != nil {
			return false, proofErr
		}
		if found {
			page.Cells = append(page.Cells, cell)
			if len(page.Cells) == request.Limit {
				page.NextRow = append([]byte(nil), row...)
				return true, nil
			}
		}
		return false, nil
	}
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return CommittedPage{}, err
		}
		key := scanner.Key()
		if scanRow != nil && !bytes.Equal(scanRow, key.Row) {
			done, finishErr := finishRow()
			if finishErr != nil {
				return CommittedPage{}, finishErr
			}
			if done {
				return page, nil
			}
			versions = versions[:0]
			row = nil
		}
		if scanRow == nil || !bytes.Equal(scanRow, key.Row) {
			scanRow = append(scanRow[:0], key.Row...)
		}
		page.Scanned++
		if page.Scanned > request.MaxScanned {
			return CommittedPage{}, errors.Join(
				transaction.ErrUnavailable,
				errors.New("committed scan exhausted its work limit"),
			)
		}
		if bytes.Equal(key.ColumnFamily, request.Family) &&
			bytes.Equal(key.ColumnQualifier, request.Qualifier) &&
			bytes.Equal(key.ColumnVisibility, request.Visibility) {
			if row == nil || !bytes.Equal(row, key.Row) {
				row = append(row[:0], key.Row...)
			}
			versions = append(versions, physicalVersion{
				key:   *key.Clone(),
				value: append([]byte(nil), scanner.Value()...),
			})
		}
		if err := scanner.Advance(); err != nil {
			return CommittedPage{}, err
		}
	}
	if _, err := finishRow(); err != nil {
		return CommittedPage{}, err
	}
	page.NextRow = nil
	return page, nil
}

func (r *Runtime) selectCommittedVersion(
	ctx context.Context,
	table string,
	versions []physicalVersion,
	frontier coordination.Epoch,
	proofs map[coordination.Epoch]publicationProof,
) (CommittedCell, bool, error) {
	for index := 0; index < len(versions); {
		timestamp := versions[index].key.Timestamp
		end := index + 1
		for end < len(versions) && versions[end].key.Timestamp == timestamp {
			end++
		}
		epoch := coordination.Epoch(timestamp)
		if epoch > 0 && epoch <= frontier {
			proof, err := r.proofForEpoch(ctx, epoch, proofs)
			if err != nil {
				return CommittedCell{}, false, err
			}
			if proof.committed {
				deleted := versions[index].key.Deleted
				value := versions[index].value
				for duplicate := index + 1; duplicate < end; duplicate++ {
					if versions[duplicate].key.Deleted != deleted {
						return CommittedCell{}, false, fmt.Errorf(
							"%w: physical deletion state disagrees at table %q row %x family %x qualifier %x visibility %x timestamp %d",
							transaction.ErrInternal,
							table,
							versions[index].key.Row,
							versions[index].key.ColumnFamily,
							versions[index].key.ColumnQualifier,
							versions[index].key.ColumnVisibility,
							timestamp,
						)
					}
					if !versions[duplicate].key.Deleted &&
						!bytes.Equal(value, versions[duplicate].value) {
						return CommittedCell{}, false, fmt.Errorf(
							"%w: physical versions disagree at one timestamp",
							transaction.ErrInternal,
						)
					}
				}
				version := versions[index]
				if !planContainsEpochCell(proof.plan, table, epoch, version) {
					return CommittedCell{}, false, fmt.Errorf(
						"%w: committed physical cell is absent from its durable intent",
						transaction.ErrInternal,
					)
				}
				if deleted {
					return CommittedCell{}, false, nil
				}
				return CommittedCell{
					Cell: allocator.Cell{
						Coordinate: allocator.Coordinate{
							Row:        append([]byte(nil), version.key.Row...),
							Family:     append([]byte(nil), version.key.ColumnFamily...),
							Qualifier:  append([]byte(nil), version.key.ColumnQualifier...),
							Visibility: append([]byte(nil), version.key.ColumnVisibility...),
						},
						Value:     append([]byte(nil), version.value...),
						Timestamp: version.key.Timestamp,
					},
					TXN:           append(coordination.TXN(nil), proof.txn...),
					LogicalDigest: proof.digest,
					Epoch:         epoch,
				}, true, nil
			}
		}
		index = end
	}
	return CommittedCell{}, false, nil
}

func (r *Runtime) proofForEpoch(
	ctx context.Context,
	epoch coordination.Epoch,
	proofs map[coordination.Epoch]publicationProof,
) (publicationProof, error) {
	if proof, found := proofs[epoch]; found {
		return proof, nil
	}
	outcome, err := r.allocator.Outcome(ctx, epoch)
	if err != nil {
		if errors.Is(err, allocator.ErrNotFound) ||
			errors.Is(err, allocator.ErrCorruption) {
			return publicationProof{}, errors.Join(
				transaction.ErrInternal,
				fmt.Errorf("read epoch outcome: %w", err),
			)
		}
		return publicationProof{}, fmt.Errorf("read epoch outcome: %w", err)
	}
	proof := publicationProof{}
	if outcome.State != coordination.StateCommitted {
		proofs[epoch] = proof
		return proof, nil
	}
	record, err := r.intents.Load(ctx, outcome.TXN)
	if err != nil {
		if errors.Is(err, transaction.ErrNotFound) {
			return publicationProof{}, errors.Join(
				transaction.ErrInternal,
				fmt.Errorf("load committed intent: %w", err),
			)
		}
		return publicationProof{}, fmt.Errorf("load committed intent: %w", err)
	}
	result, committed, err := r.committedLocked(
		ctx, outcome.TXN, record.LogicalDigest,
	)
	if err != nil {
		return publicationProof{}, err
	}
	if !committed || result.Epoch != epoch {
		proofs[epoch] = proof
		return proof, nil
	}
	plan, err := r.intents.Materialize(ctx, transaction.MaterializeRequest{
		TXN: outcome.TXN, LogicalDigest: record.LogicalDigest,
	})
	if err != nil {
		return publicationProof{}, err
	}
	proof = publicationProof{
		committed: true,
		txn:       append(coordination.TXN(nil), outcome.TXN...),
		digest:    record.LogicalDigest,
		plan:      plan,
	}
	proofs[epoch] = proof
	return proof, nil
}

func (r *Runtime) committedLocked(
	ctx context.Context,
	txn coordination.TXN,
	logicalDigest coordination.Digest,
) (Result, bool, error) {
	if err := txn.Validate(); err != nil {
		return Result{}, false, errors.Join(transaction.ErrInvalid, err)
	}
	if err := logicalDigest.Validate("logical digest"); err != nil {
		return Result{}, false, errors.Join(transaction.ErrInvalid, err)
	}
	record, err := r.intents.Load(ctx, txn)
	if errors.Is(err, transaction.ErrNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if record.LogicalDigest != logicalDigest {
		return Result{}, false, transaction.ErrConflict
	}
	epoch, complete, err := r.intents.Completed(ctx, txn, logicalDigest)
	if err != nil || !complete {
		return Result{}, false, err
	}
	snapshot, err := r.coordinator.Inspect(ctx, txn)
	if err != nil {
		return Result{}, false, err
	}
	if snapshot.Root.State != coordination.StateCommitted ||
		snapshot.Root.Epoch != epoch ||
		snapshot.Root.LogicalDigest != logicalDigest {
		return Result{}, false, nil
	}
	outcome, err := r.allocator.Outcome(ctx, epoch)
	if err != nil {
		if errors.Is(err, allocator.ErrNotFound) ||
			errors.Is(err, allocator.ErrCorruption) {
			return Result{}, false, errors.Join(
				transaction.ErrInternal,
				fmt.Errorf("read committed epoch outcome: %w", err),
			)
		}
		return Result{}, false, fmt.Errorf(
			"read committed epoch outcome: %w",
			err,
		)
	}
	if outcome.State != coordination.StateCommitted ||
		!bytes.Equal(outcome.TXN, txn) {
		return Result{}, false, nil
	}
	head, err := r.allocator.CurrentHead(ctx)
	if err != nil {
		return Result{}, false, err
	}
	if head.Frontier < epoch {
		return Result{}, false, nil
	}
	return Result{
		TXN: append(coordination.TXN(nil), txn...), LogicalDigest: logicalDigest,
		Epoch: epoch, Identities: cloneResultIdentities(snapshot.Root.ResultIdentities),
		Unchanged: true,
	}, true, nil
}

func planContainsEpochCell(
	plan transaction.Plan,
	table string,
	epoch coordination.Epoch,
	version physicalVersion,
) bool {
	for _, cell := range plan.Cells {
		if cell.Entry.EpochSlot != coordination.EpochSlotContent ||
			cell.Delete != version.key.Deleted ||
			string(cell.Entry.Table) != table ||
			!bytes.Equal(cell.Entry.Row, version.key.Row) ||
			!bytes.Equal(cell.Entry.ColumnFamily, version.key.ColumnFamily) ||
			!bytes.Equal(cell.Entry.ColumnQualifier, version.key.ColumnQualifier) ||
			!bytes.Equal(cell.Visibility, version.key.ColumnVisibility) ||
			!bytes.Equal(cell.Value, version.value) ||
			version.key.Timestamp != int64(epoch) {
			continue
		}
		return true
	}
	return false
}
