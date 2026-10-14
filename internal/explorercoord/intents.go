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
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

const (
	intentVersion       = 1
	pendingIndexVersion = 1
	maxIntentBytes      = 128 << 20
	maxIntentCells      = 65_536
)

var (
	intentFamily      = []byte("i")
	intentQualifier   = []byte("intent")
	pendingQualifier  = []byte("pending")
	completeQualifier = []byte("complete")
	quarantineFamily  = []byte("q")
	quarantineCQ      = []byte("reason")
	intentRowMagic    = []byte{2, 'I'}
	attemptRowMagic   = []byte{2, 'A'}
	attemptQualifier  = []byte("txn")
	pendingIndexRow   = []byte{2, 'P'}
	pendingIndexCQ    = []byte("active")
)

// Cell is one immutable physical cell in a logical publication intent.
// EpochTimestamp selects the transaction's reserved epoch; otherwise
// Timestamp must be a positive explicit timestamp.
type Cell struct {
	Table           string                  `json:"table"`
	Row             []byte                  `json:"row"`
	Family          []byte                  `json:"family"`
	Qualifier       []byte                  `json:"qualifier"`
	Visibility      []byte                  `json:"visibility,omitempty"`
	Value           []byte                  `json:"value"`
	Delete          bool                    `json:"delete,omitempty"`
	EpochTimestamp  bool                    `json:"epoch_timestamp"`
	Timestamp       coordination.Epoch      `json:"timestamp,omitempty"`
	LPART           coordination.LPART      `json:"lpart"`
	CopyGeneration  coordination.Generation `json:"copy_generation"`
	IndexGeneration coordination.IGEN       `json:"index_generation,omitempty"`
	IndexFamily     coordination.Family     `json:"index_family,omitempty"`
}

// GuardIntent describes the expected and desired state of one logical entity.
// A zero DesiredDigest means the canonical digest of the whole Intent.
type GuardIntent struct {
	Entity               guard.Entity            `json:"entity"`
	Mode                 guard.Mode              `json:"mode"`
	ExpectedEpoch        coordination.Epoch      `json:"expected_epoch,omitempty"`
	ExpectedDigest       coordination.Digest     `json:"expected_digest,omitempty"`
	DesiredState         guard.EntityState       `json:"desired_state"`
	DesiredWinnerID      []byte                  `json:"desired_winner_id"`
	DesiredDigest        coordination.Digest     `json:"desired_digest,omitempty"`
	LPART                coordination.LPART      `json:"lpart"`
	LogicalPolicyID      []byte                  `json:"logical_policy_id"`
	RetirementGeneration coordination.Generation `json:"retirement_generation"`
}

type ResultIdentity struct {
	Kind []byte `json:"kind"`
	ID   []byte `json:"id"`
}

// Intent is the durable logical input to a publication. It is normalized and
// canonically encoded before any transaction-root or physical mutation.
type Intent struct {
	Operation   []byte           `json:"operation"`
	Token       []byte           `json:"token"`
	RecordKey   []byte           `json:"record_key,omitempty"`
	RecordToken []byte           `json:"record_token,omitempty"`
	Cells       []Cell           `json:"cells"`
	Guards      []GuardIntent    `json:"guards,omitempty"`
	Results     []ResultIdentity `json:"results,omitempty"`
}

// LogicalDigest returns the digest used for idempotent intent comparison and
// expected-value guard updates.
func LogicalDigest(intent Intent) (coordination.Digest, error) {
	_, _, digest, err := canonicalIntent(intent)
	return digest, err
}

type storedIntent struct {
	Version       uint8                 `json:"version"`
	Domain        coordination.DomainID `json:"domain"`
	TXN           coordination.TXN      `json:"txn"`
	LogicalDigest coordination.Digest   `json:"logical_digest"`
	Intent        Intent                `json:"intent"`
}

type intentCompletion struct {
	Digest coordination.Digest
	Epoch  coordination.Epoch
}

type pendingIntentIndex struct {
	Version    uint8              `json:"version"`
	Generation int64              `json:"generation"`
	TXNs       []coordination.TXN `json:"txns"`
}

type IntentStore struct {
	domain     coordination.DomainID
	visibility []byte
	store      allocator.Store
	indexMu    sync.Mutex
	pendingMu  sync.RWMutex
	pending    map[string]struct{}
}

func NewIntentStore(
	domain coordination.DomainID,
	visibility []byte,
	store *EngineStore,
) (*IntentStore, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("explorer coordination: intent store is required")
	}
	return &IntentStore{
		domain:     append(coordination.DomainID(nil), domain...),
		visibility: append([]byte(nil), visibility...),
		store:      store,
		pending:    make(map[string]struct{}),
	}, nil
}

// DeriveTXN produces the stable transaction identity for an operation/token
// pair within one domain.
func DeriveTXN(
	domain coordination.DomainID,
	operation, token []byte,
) (coordination.TXN, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if len(operation) == 0 || len(operation) > coordination.MaxOpaqueIDBytes ||
		len(token) == 0 || len(token) > coordination.MaxOpaqueIDBytes {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("operation and token are outside their bounds"))
	}
	hash := sha256.New()
	writeDigestPart(hash, []byte("shoal-embedded-txn-v1"))
	writeDigestPart(hash, domain)
	writeDigestPart(hash, operation)
	writeDigestPart(hash, token)
	return coordination.TXN(hash.Sum(nil)), nil
}

// Partition derives a stable LPART from a domain-local logical partition key.
func Partition(
	domain coordination.DomainID,
	partition []byte,
) (coordination.LPART, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if len(partition) == 0 || len(partition) > coordination.MaxOpaqueIDBytes {
		return nil, errors.Join(transaction.ErrInvalid, errors.New("partition key is outside its bound"))
	}
	hash := sha256.New()
	writeDigestPart(hash, []byte("shoal-embedded-lpart-v1"))
	writeDigestPart(hash, domain)
	writeDigestPart(hash, partition)
	return coordination.LPART(hash.Sum(nil)), nil
}

func (s *IntentStore) Put(
	ctx context.Context,
	intent Intent,
) (storedIntent, bool, error) {
	normalized, _, digest, err := canonicalIntent(intent)
	if err != nil {
		return storedIntent{}, false, err
	}
	txn, err := DeriveTXN(s.domain, normalized.Operation, normalized.Token)
	if err != nil {
		return storedIntent{}, false, err
	}
	if err := s.addPending(ctx, txn); err != nil {
		return storedIntent{}, false, err
	}
	record := storedIntent{
		Version: intentVersion, Domain: append(coordination.DomainID(nil), s.domain...),
		TXN: append(coordination.TXN(nil), txn...), LogicalDigest: digest, Intent: normalized,
	}
	encoded, err := encodeStoredIntent(record)
	if err != nil {
		return storedIntent{}, false, err
	}
	coordinate := s.intentCoordinate(txn)
	mutation := allocator.Mutation{
		Row: coordinate.Row,
		Conditions: []allocator.Condition{
			{Coordinate: coordinate, Absent: true},
			{Coordinate: s.pendingCoordinate(txn), Absent: true},
		},
		Updates: []allocator.Update{
			{Coordinate: coordinate, Value: encoded, Timestamp: intentVersion},
			{
				Coordinate: s.pendingCoordinate(txn),
				Value:      digest[:],
				Timestamp:  intentVersion,
			},
		},
	}
	for attempt := 0; attempt <= 3; attempt++ {
		status, writeErr := s.store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return record, false, nil
		}
		existing, readErr := s.Load(ctx, txn)
		if readErr == nil {
			existingBytes, encodeErr := encodeStoredIntent(existing)
			if encodeErr != nil {
				return storedIntent{}, false, encodeErr
			}
			if bytes.Equal(existingBytes, encoded) {
				active, err := s.ensurePending(ctx, existing)
				if err != nil {
					if errors.Is(err, allocator.ErrConditionalUnknown) {
						s.markPending(txn)
					}
					return storedIntent{}, false, err
				}
				if active {
					s.markPending(txn)
				} else {
					if err := s.removePending(ctx, txn); err != nil {
						return storedIntent{}, false, err
					}
				}
				return existing, true, nil
			}
			active, pendingErr := s.hasPendingMarker(ctx, existing)
			if pendingErr != nil {
				return storedIntent{}, false, pendingErr
			}
			if !active {
				if removeErr := s.removePending(ctx, txn); removeErr != nil {
					return storedIntent{}, false, errors.Join(
						transaction.ErrConflict,
						removeErr,
					)
				}
			}
			return storedIntent{}, false, transaction.ErrConflict
		}
		if !errors.Is(readErr, transaction.ErrNotFound) {
			if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
				s.markPending(txn)
				return storedIntent{}, false, errors.Join(
					transaction.ErrUnavailable,
					allocator.ErrConditionalUnknown,
					writeErr,
					readErr,
				)
			}
			return storedIntent{}, false, readErr
		}
		if status == allocator.StatusRejected {
			return storedIntent{}, false, transaction.ErrConflict
		}
		if writeErr != nil && !errors.Is(writeErr, allocator.ErrConditionalUnknown) {
			return storedIntent{}, false, errors.Join(transaction.ErrUnavailable, writeErr)
		}
		if attempt == 3 {
			s.markPending(txn)
			return storedIntent{}, false, errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown)
		}
	}
	return storedIntent{}, false, transaction.ErrUnavailable
}

func (s *IntentStore) Load(
	ctx context.Context,
	txn coordination.TXN,
) (storedIntent, error) {
	if err := txn.Validate(); err != nil {
		return storedIntent{}, errors.Join(transaction.ErrInvalid, err)
	}
	coordinate := s.intentCoordinate(txn)
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return storedIntent{}, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return storedIntent{}, transaction.ErrNotFound
	}
	if len(cells) != 1 || cells[0].Timestamp < intentVersion {
		return storedIntent{}, fmt.Errorf("%w: durable intent cell is invalid", transaction.ErrInternal)
	}
	record, err := decodeStoredIntent(cells[0].Value)
	if err != nil {
		return storedIntent{}, fmt.Errorf("%w: decode durable intent: %v", transaction.ErrInternal, err)
	}
	if !bytes.Equal(record.Domain, s.domain) || !bytes.Equal(record.TXN, txn) {
		return storedIntent{}, fmt.Errorf("%w: durable intent identity mismatch", transaction.ErrInternal)
	}
	normalized, canonical, digest, err := canonicalIntent(record.Intent)
	if err != nil || digest != record.LogicalDigest {
		return storedIntent{}, fmt.Errorf("%w: durable intent digest mismatch", transaction.ErrInternal)
	}
	record.Intent = normalized
	again, err := encodeStoredIntent(record)
	if err != nil || !bytes.Equal(again, cells[0].Value) || len(canonical) == 0 {
		return storedIntent{}, fmt.Errorf("%w: durable intent is not canonical", transaction.ErrInternal)
	}
	return record, nil
}

func (s *IntentStore) Complete(
	ctx context.Context,
	txn coordination.TXN,
	digest coordination.Digest,
	epoch coordination.Epoch,
) error {
	if err := epoch.Validate(); err != nil {
		return err
	}
	coordinate := s.completionCoordinate(txn)
	value := encodeCompletion(intentCompletion{Digest: digest, Epoch: epoch})
	mutation := allocator.Mutation{
		Row:        coordinate.Row,
		Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
		Updates: []allocator.Update{
			{Coordinate: coordinate, Value: value, Timestamp: int64(epoch)},
			{
				Coordinate: s.pendingCoordinate(txn),
				Delete:     true,
				Timestamp:  intentVersion,
			},
		},
	}
	status, writeErr := s.store.CompareAndMutate(ctx, mutation)
	if status == allocator.StatusAccepted {
		return s.removePending(ctx, txn)
	}
	cells, readErr := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if readErr != nil {
		return errors.Join(transaction.ErrUnavailable, writeErr, readErr)
	}
	if len(cells) == 1 && cells[0].Timestamp == int64(epoch) && bytes.Equal(cells[0].Value, value) {
		return s.removePending(ctx, txn)
	}
	if status == allocator.StatusRejected {
		return transaction.ErrConflict
	}
	return errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown, writeErr)
}

func (s *IntentStore) Completed(
	ctx context.Context,
	txn coordination.TXN,
	digest coordination.Digest,
) (coordination.Epoch, bool, error) {
	coordinate := s.completionCoordinate(txn)
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return 0, false, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return 0, false, nil
	}
	value, err := decodeCompletion(cells[0].Value)
	if err != nil || cells[0].Timestamp != int64(value.Epoch) {
		return 0, false, fmt.Errorf("%w: durable intent completion is invalid", transaction.ErrInternal)
	}
	if value.Digest != digest {
		return 0, false, fmt.Errorf("%w: durable intent completion digest mismatch", transaction.ErrInternal)
	}
	return value.Epoch, true, nil
}

func (s *IntentStore) Candidates(
	ctx context.Context,
	after []byte,
	limit int,
) ([]coordination.TXN, []byte, error) {
	if limit < 1 || limit > engineReadBound {
		return nil, nil, errors.New("explorer coordination: intent scan limit is outside its bound")
	}
	prefix := s.intentPrefix()
	if len(after) != 0 {
		if !bytes.HasPrefix(after, prefix) || bytes.Compare(after, prefix) < 0 {
			return nil, nil, errors.New("explorer coordination: invalid intent recovery cursor")
		}
	}
	s.indexMu.Lock()
	index, _, _, err := s.readPendingIndex(ctx)
	s.indexMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	start := 0
	if len(after) != 0 {
		start = sort.Search(len(index.TXNs), func(i int) bool {
			return bytes.Compare(s.intentRow(index.TXNs[i]), after) >= 0
		})
	}
	end := min(start+limit, len(index.TXNs))
	result := make([]coordination.TXN, 0, end-start)
	for _, txn := range index.TXNs[start:end] {
		result = append(result, append(coordination.TXN(nil), txn...))
		s.markPending(txn)
	}
	var next []byte
	if end < len(index.TXNs) {
		next = append(s.intentRow(index.TXNs[end-1]), 0)
	}
	return result, next, nil
}

func (s *IntentStore) IndexPending(
	ctx context.Context,
	limit, maxPages int,
) error {
	if limit < 1 || maxPages < 1 {
		return errors.Join(
			transaction.ErrInvalid,
			errors.New("pending intent index bounds are invalid"),
		)
	}
	s.indexMu.Lock()
	index, _, _, err := s.readPendingIndex(ctx)
	if err != nil {
		s.indexMu.Unlock()
		return err
	}
	s.pendingMu.Lock()
	s.pending = make(map[string]struct{})
	for _, txn := range index.TXNs {
		s.pending[string(txn)] = struct{}{}
	}
	s.pendingMu.Unlock()
	s.indexMu.Unlock()
	return nil
}

func (s *IntentStore) HasPending() bool {
	s.pendingMu.RLock()
	defer s.pendingMu.RUnlock()
	return len(s.pending) != 0
}

func (s *IntentStore) markPending(txn coordination.TXN) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending[string(txn)] = struct{}{}
}

func (s *IntentStore) clearPending(txn coordination.TXN) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pending, string(txn))
}

func (s *IntentStore) addPending(
	ctx context.Context,
	txn coordination.TXN,
) error {
	if err := s.updatePendingIndex(ctx, txn, true); err != nil {
		if errors.Is(err, allocator.ErrConditionalUnknown) {
			s.markPending(txn)
		}
		return err
	}
	s.markPending(txn)
	return nil
}

func (s *IntentStore) removePending(
	ctx context.Context,
	txn coordination.TXN,
) error {
	if err := s.updatePendingIndex(ctx, txn, false); err != nil {
		return err
	}
	s.clearPending(txn)
	return nil
}

func (s *IntentStore) updatePendingIndex(
	ctx context.Context,
	txn coordination.TXN,
	add bool,
) error {
	if err := txn.Validate(); err != nil {
		return errors.Join(transaction.ErrInvalid, err)
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	for attempt := 0; attempt <= 7; attempt++ {
		index, encoded, found, err := s.readPendingIndex(ctx)
		if err != nil {
			return err
		}
		position, present := s.pendingIndexPosition(index.TXNs, txn)
		if present == add {
			return nil
		}
		next := pendingIntentIndex{
			Version:    pendingIndexVersion,
			Generation: 1,
			TXNs:       cloneTXNs(index.TXNs),
		}
		if found {
			if index.Generation == math.MaxInt64 {
				return errors.Join(
					transaction.ErrInvalid,
					errors.New("pending intent index generation is exhausted"),
				)
			}
			next.Generation = index.Generation + 1
		}
		if add {
			if len(next.TXNs) >= coordination.MaxActiveReservations {
				return allocator.ErrWindowFull
			}
			next.TXNs = append(next.TXNs, nil)
			copy(next.TXNs[position+1:], next.TXNs[position:])
			next.TXNs[position] = append(coordination.TXN(nil), txn...)
		} else {
			next.TXNs = append(next.TXNs[:position], next.TXNs[position+1:]...)
		}
		nextEncoded, err := json.Marshal(next)
		if err != nil {
			return errors.Join(transaction.ErrInternal, err)
		}
		coordinate := s.pendingIndexCoordinate()
		condition := allocator.Condition{Coordinate: coordinate, Absent: true}
		if found {
			condition = allocator.Condition{
				Coordinate:   coordinate,
				Value:        encoded,
				Timestamp:    index.Generation,
				TimestampSet: true,
			}
		}
		status, writeErr := s.store.CompareAndMutate(ctx, allocator.Mutation{
			Row:        coordinate.Row,
			Conditions: []allocator.Condition{condition},
			Updates: []allocator.Update{{
				Coordinate: coordinate,
				Value:      nextEncoded,
				Timestamp:  next.Generation,
			}},
		})
		if status == allocator.StatusAccepted {
			return nil
		}
		observed, _, observedFound, readErr := s.readPendingIndex(ctx)
		if readErr != nil {
			if errors.Is(writeErr, allocator.ErrConditionalUnknown) {
				return errors.Join(
					transaction.ErrUnavailable,
					allocator.ErrConditionalUnknown,
					writeErr,
					readErr,
				)
			}
			return errors.Join(transaction.ErrUnavailable, writeErr, readErr)
		}
		_, observedPresent := s.pendingIndexPosition(observed.TXNs, txn)
		if observedFound &&
			observed.Generation == next.Generation &&
			observedPresent == add {
			return nil
		}
		if status == allocator.StatusUnknown {
			return errors.Join(
				transaction.ErrUnavailable,
				allocator.ErrConditionalUnknown,
				writeErr,
			)
		}
	}
	return transaction.ErrConflict
}

func (s *IntentStore) readPendingIndex(
	ctx context.Context,
) (pendingIntentIndex, []byte, bool, error) {
	coordinate := s.pendingIndexCoordinate()
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return pendingIntentIndex{}, nil, false, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return pendingIntentIndex{Version: pendingIndexVersion}, nil, false, nil
	}
	if len(cells) != 1 || cells[0].Timestamp < pendingIndexVersion {
		return pendingIntentIndex{}, nil, false, fmt.Errorf(
			"%w: pending intent index cell is invalid",
			transaction.ErrInternal,
		)
	}
	var index pendingIntentIndex
	if err := json.Unmarshal(cells[0].Value, &index); err != nil {
		return pendingIntentIndex{}, nil, false, fmt.Errorf(
			"%w: decode pending intent index: %v",
			transaction.ErrInternal,
			err,
		)
	}
	if index.Version != pendingIndexVersion ||
		index.Generation != cells[0].Timestamp ||
		index.Generation < 1 ||
		len(index.TXNs) > coordination.MaxActiveReservations {
		return pendingIntentIndex{}, nil, false, fmt.Errorf(
			"%w: pending intent index metadata is invalid",
			transaction.ErrInternal,
		)
	}
	for position, txn := range index.TXNs {
		if err := txn.Validate(); err != nil {
			return pendingIntentIndex{}, nil, false, fmt.Errorf(
				"%w: pending intent index transaction is invalid",
				transaction.ErrInternal,
			)
		}
		if position > 0 &&
			bytes.Compare(
				s.intentRow(index.TXNs[position-1]),
				s.intentRow(txn),
			) >= 0 {
			return pendingIntentIndex{}, nil, false, fmt.Errorf(
				"%w: pending intent index is not canonical",
				transaction.ErrInternal,
			)
		}
	}
	again, err := json.Marshal(index)
	if err != nil || !bytes.Equal(again, cells[0].Value) {
		return pendingIntentIndex{}, nil, false, fmt.Errorf(
			"%w: pending intent index encoding is not canonical",
			transaction.ErrInternal,
		)
	}
	return index, append([]byte(nil), cells[0].Value...), true, nil
}

func (s *IntentStore) pendingIndexPosition(
	txns []coordination.TXN,
	txn coordination.TXN,
) (int, bool) {
	row := s.intentRow(txn)
	position := sort.Search(len(txns), func(i int) bool {
		return bytes.Compare(s.intentRow(txns[i]), row) >= 0
	})
	return position, position < len(txns) && bytes.Equal(txns[position], txn)
}

func (s *IntentStore) pendingIndexCoordinate() allocator.Coordinate {
	row := append([]byte(nil), pendingIndexRow...)
	row = append(row, coordination.E(s.domain)...)
	return allocator.Coordinate{
		Row:        row,
		Family:     append([]byte(nil), intentFamily...),
		Qualifier:  append([]byte(nil), pendingIndexCQ...),
		Visibility: append([]byte(nil), s.visibility...),
	}
}

func cloneTXNs(txns []coordination.TXN) []coordination.TXN {
	result := make([]coordination.TXN, len(txns))
	for index, txn := range txns {
		result[index] = append(coordination.TXN(nil), txn...)
	}
	return result
}

func (s *IntentStore) Materialize(
	ctx context.Context,
	request transaction.MaterializeRequest,
) (transaction.Plan, error) {
	record, err := s.Load(ctx, request.TXN)
	if err != nil {
		return transaction.Plan{}, err
	}
	if record.LogicalDigest != request.LogicalDigest {
		return transaction.Plan{}, fmt.Errorf("%w: materializer logical digest mismatch", transaction.ErrInternal)
	}
	return buildPlan(record.Intent, record.LogicalDigest)
}

func (s *IntentStore) Record(
	ctx context.Context,
	domain coordination.DomainID,
	txn coordination.TXN,
	reason string,
) error {
	if !bytes.Equal(domain, s.domain) {
		return transaction.ErrInvalid
	}
	if len(reason) == 0 || len(reason) > 4096 {
		return transaction.ErrInvalid
	}
	row := append(append([]byte(nil), s.intentRow(txn)...), 0, 'q')
	coordinate := allocator.Coordinate{
		Row: row, Family: quarantineFamily, Qualifier: quarantineCQ,
		Visibility: append([]byte(nil), s.visibility...),
	}
	mutation := allocator.Mutation{
		Row:        row,
		Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
		Updates:    []allocator.Update{{Coordinate: coordinate, Value: []byte(reason), Timestamp: 1}},
	}
	status, err := s.store.CompareAndMutate(ctx, mutation)
	if status == allocator.StatusAccepted || status == allocator.StatusRejected {
		return nil
	}
	return errors.Join(transaction.ErrUnavailable, err)
}

func (s *IntentStore) intentCoordinate(txn coordination.TXN) allocator.Coordinate {
	return allocator.Coordinate{
		Row: s.intentRow(txn), Family: append([]byte(nil), intentFamily...),
		Qualifier: append([]byte(nil), intentQualifier...), Visibility: append([]byte(nil), s.visibility...),
	}
}

func (s *IntentStore) completionCoordinate(txn coordination.TXN) allocator.Coordinate {
	return allocator.Coordinate{
		Row: s.intentRow(txn), Family: append([]byte(nil), intentFamily...),
		Qualifier: append([]byte(nil), completeQualifier...), Visibility: append([]byte(nil), s.visibility...),
	}
}

func (s *IntentStore) pendingCoordinate(txn coordination.TXN) allocator.Coordinate {
	return allocator.Coordinate{
		Row: s.intentRow(txn), Family: append([]byte(nil), intentFamily...),
		Qualifier: append([]byte(nil), pendingQualifier...), Visibility: append([]byte(nil), s.visibility...),
	}
}

func (s *IntentStore) Attempt(
	ctx context.Context,
	key []byte,
) (coordination.TXN, bool, error) {
	binding, found, err := s.readAttempt(ctx, key)
	return binding.txn, found, err
}

type attemptBinding struct {
	txn        coordination.TXN
	generation int64
}

func (s *IntentStore) readAttempt(
	ctx context.Context,
	key []byte,
) (attemptBinding, bool, error) {
	coordinate, err := s.attemptCoordinate(key)
	if err != nil {
		return attemptBinding{}, false, err
	}
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return attemptBinding{}, false, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return attemptBinding{}, false, nil
	}
	if len(cells) != 1 || cells[0].Timestamp < intentVersion {
		return attemptBinding{}, false, fmt.Errorf("%w: record attempt binding is invalid", transaction.ErrInternal)
	}
	txn := coordination.TXN(append([]byte(nil), cells[0].Value...))
	if err := txn.Validate(); err != nil {
		return attemptBinding{}, false, fmt.Errorf("%w: record attempt transaction is invalid", transaction.ErrInternal)
	}
	return attemptBinding{txn: txn, generation: cells[0].Timestamp}, true, nil
}

func (s *IntentStore) SetAttempt(
	ctx context.Context,
	key []byte,
	previous, next coordination.TXN,
) error {
	current, found, err := s.readAttempt(ctx, key)
	if err != nil {
		return err
	}
	if len(previous) == 0 {
		if found {
			return transaction.ErrConflict
		}
		return s.setAttempt(ctx, key, attemptBinding{}, next)
	}
	if !found || !bytes.Equal(current.txn, previous) {
		return transaction.ErrConflict
	}
	return s.setAttempt(ctx, key, current, next)
}

func (s *IntentStore) setAttempt(
	ctx context.Context,
	key []byte,
	previous attemptBinding,
	next coordination.TXN,
) error {
	coordinate, err := s.attemptCoordinate(key)
	if err != nil {
		return err
	}
	condition := allocator.Condition{Coordinate: coordinate, Absent: true}
	generation := int64(intentVersion)
	if len(previous.txn) != 0 {
		if previous.generation == math.MaxInt64 {
			return errors.Join(transaction.ErrInvalid, errors.New("record attempt generation is exhausted"))
		}
		condition = allocator.Condition{
			Coordinate: coordinate, Value: previous.txn,
			Timestamp: previous.generation, TimestampSet: true,
		}
		generation = previous.generation + 1
	}
	mutation := allocator.Mutation{
		Row:        coordinate.Row,
		Conditions: []allocator.Condition{condition},
		Updates: []allocator.Update{{
			Coordinate: coordinate, Value: next, Timestamp: generation,
		}},
	}
	status, writeErr := s.store.CompareAndMutate(ctx, mutation)
	if status == allocator.StatusAccepted {
		return nil
	}
	current, found, readErr := s.readAttempt(ctx, key)
	if readErr != nil {
		return errors.Join(transaction.ErrUnavailable, writeErr, readErr)
	}
	if found && bytes.Equal(current.txn, next) &&
		current.generation == generation {
		return nil
	}
	if status == allocator.StatusRejected {
		return transaction.ErrConflict
	}
	return errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown, writeErr)
}

func (s *IntentStore) attemptCoordinate(key []byte) (allocator.Coordinate, error) {
	if len(key) == 0 || len(key) > coordination.MaxOpaqueIDBytes {
		return allocator.Coordinate{}, errors.Join(
			transaction.ErrInvalid,
			errors.New("record attempt key is outside its bound"),
		)
	}
	row := append([]byte(nil), attemptRowMagic...)
	row = append(row, coordination.E(s.domain)...)
	row = append(row, coordination.E(key)...)
	return allocator.Coordinate{
		Row: row, Family: append([]byte(nil), intentFamily...),
		Qualifier:  append([]byte(nil), attemptQualifier...),
		Visibility: append([]byte(nil), s.visibility...),
	}, nil
}

func (s *IntentStore) ensurePending(
	ctx context.Context,
	record storedIntent,
) (bool, error) {
	if _, complete, err := s.Completed(
		ctx, record.TXN, record.LogicalDigest,
	); err != nil {
		return false, err
	} else if complete {
		return false, nil
	}
	coordinate := s.pendingCoordinate(record.TXN)
	mutation := allocator.Mutation{
		Row:        coordinate.Row,
		Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
		Updates: []allocator.Update{{
			Coordinate: coordinate, Value: record.LogicalDigest[:],
			Timestamp: intentVersion,
		}},
	}
	status, writeErr := s.store.CompareAndMutate(ctx, mutation)
	if status == allocator.StatusAccepted {
		return true, nil
	}
	cells, readErr := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if readErr != nil {
		return false, errors.Join(transaction.ErrUnavailable, writeErr, readErr)
	}
	if len(cells) == 1 && cells[0].Timestamp == intentVersion &&
		bytes.Equal(cells[0].Value, record.LogicalDigest[:]) {
		return true, nil
	}
	if status == allocator.StatusRejected {
		return false, transaction.ErrConflict
	}
	return false, errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown, writeErr)
}

func (s *IntentStore) hasPendingMarker(
	ctx context.Context,
	record storedIntent,
) (bool, error) {
	coordinate := s.pendingCoordinate(record.TXN)
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return false, errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return false, nil
	}
	if len(cells) != 1 ||
		cells[0].Timestamp != intentVersion ||
		!bytes.Equal(cells[0].Value, record.LogicalDigest[:]) {
		return false, fmt.Errorf(
			"%w: pending intent marker is invalid",
			transaction.ErrInternal,
		)
	}
	return true, nil
}

func (s *IntentStore) Settle(
	ctx context.Context,
	txn coordination.TXN,
	logicalDigest coordination.Digest,
) error {
	coordinate := s.pendingCoordinate(txn)
	cells, err := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil {
		return errors.Join(transaction.ErrUnavailable, err)
	}
	if len(cells) == 0 {
		return s.removePending(ctx, txn)
	}
	if len(cells) != 1 || cells[0].Timestamp != intentVersion ||
		!bytes.Equal(cells[0].Value, logicalDigest[:]) {
		return fmt.Errorf("%w: pending intent marker is invalid", transaction.ErrInternal)
	}
	if err := s.removePending(ctx, txn); err != nil {
		return err
	}
	mutation := allocator.Mutation{
		Row: coordinate.Row,
		Conditions: []allocator.Condition{{
			Coordinate: coordinate, Value: cells[0].Value,
			Timestamp: cells[0].Timestamp, TimestampSet: true,
		}},
		Updates: []allocator.Update{{
			Coordinate: coordinate, Delete: true, Timestamp: intentVersion,
		}},
	}
	status, writeErr := s.store.CompareAndMutate(ctx, mutation)
	if status == allocator.StatusAccepted {
		return nil
	}
	after, readErr := s.store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if readErr != nil {
		return errors.Join(transaction.ErrUnavailable, writeErr, readErr)
	}
	if len(after) == 0 {
		return nil
	}
	if status == allocator.StatusRejected {
		return transaction.ErrConflict
	}
	return errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown, writeErr)
}

func (s *IntentStore) intentPrefix() []byte {
	result := append([]byte(nil), intentRowMagic...)
	return append(result, coordination.E(s.domain)...)
}

func (s *IntentStore) intentRow(txn coordination.TXN) []byte {
	result := s.intentPrefix()
	return append(result, coordination.E(txn)...)
}

func (s *IntentStore) parseIntentRow(row []byte) (coordination.TXN, error) {
	prefix := s.intentPrefix()
	if !bytes.HasPrefix(row, prefix) {
		return nil, errors.New("explorer coordination: invalid intent row")
	}
	value, used, err := coordination.DecodeE(row[len(prefix):])
	if err != nil || len(prefix)+used != len(row) {
		return nil, errors.New("explorer coordination: invalid intent row")
	}
	txn := coordination.TXN(value)
	if err := txn.Validate(); err != nil {
		return nil, err
	}
	return txn, nil
}

func canonicalIntent(intent Intent) (Intent, []byte, coordination.Digest, error) {
	normalized := cloneIntent(intent)
	for index := range normalized.Cells {
		if normalized.Cells[index].CopyGeneration == 0 {
			normalized.Cells[index].CopyGeneration = 1
		}
	}
	for index := range normalized.Guards {
		if normalized.Guards[index].DesiredState == 0 {
			normalized.Guards[index].DesiredState = guard.StateLive
		}
		if normalized.Guards[index].RetirementGeneration == 0 {
			normalized.Guards[index].RetirementGeneration = 1
		}
	}
	sort.Slice(normalized.Cells, func(i, j int) bool {
		return compareIntentCells(normalized.Cells[i], normalized.Cells[j]) < 0
	})
	sort.Slice(normalized.Guards, func(i, j int) bool {
		if normalized.Guards[i].Entity.Kind != normalized.Guards[j].Entity.Kind {
			return normalized.Guards[i].Entity.Kind < normalized.Guards[j].Entity.Kind
		}
		return bytes.Compare(normalized.Guards[i].Entity.ID, normalized.Guards[j].Entity.ID) < 0
	})
	sort.Slice(normalized.Results, func(i, j int) bool {
		if order := bytes.Compare(normalized.Results[i].Kind, normalized.Results[j].Kind); order != 0 {
			return order < 0
		}
		return bytes.Compare(normalized.Results[i].ID, normalized.Results[j].ID) < 0
	})
	if err := validateIntent(normalized); err != nil {
		return Intent{}, nil, coordination.Digest{}, err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return Intent{}, nil, coordination.Digest{}, errors.Join(transaction.ErrInvalid, err)
	}
	if len(payload) > maxIntentBytes {
		return Intent{}, nil, coordination.Digest{}, errors.Join(transaction.ErrInvalid, errors.New("canonical intent exceeds its bound"))
	}
	return normalized, payload, coordination.Sum(payload), nil
}

func validateIntent(intent Intent) error {
	if len(intent.Operation) == 0 || len(intent.Operation) > coordination.MaxOpaqueIDBytes ||
		len(intent.Token) == 0 || len(intent.Token) > coordination.MaxOpaqueIDBytes {
		return errors.Join(transaction.ErrInvalid, errors.New("operation and token are outside their bounds"))
	}
	if len(intent.RecordKey) > coordination.MaxOpaqueIDBytes {
		return errors.Join(transaction.ErrInvalid, errors.New("record key exceeds its bound"))
	}
	if len(intent.RecordToken) > coordination.MaxOpaqueIDBytes {
		return errors.Join(transaction.ErrInvalid, errors.New("record token exceeds its bound"))
	}
	if len(intent.Cells) == 0 || len(intent.Cells) > maxIntentCells {
		return errors.Join(transaction.ErrInvalid, errors.New("intent cell count is outside its bound"))
	}
	if len(intent.Guards) > guard.MaxEntities || len(intent.Results) > coordination.MaxResultIdentities {
		return errors.Join(transaction.ErrInvalid, errors.New("intent metadata count is outside its bound"))
	}
	partitions := make(map[string]Cell)
	physicalKeys := make(map[string]struct{}, len(intent.Cells))
	for index, cell := range intent.Cells {
		if !validTableName(cell.Table) || len(cell.Row) == 0 || len(cell.Row) > coordination.MaxCoordinateBytes ||
			len(cell.Family) == 0 || len(cell.Family) > coordination.MaxCoordinateBytes ||
			len(cell.Qualifier) == 0 || len(cell.Qualifier) > coordination.MaxCoordinateBytes ||
			len(cell.Visibility) > coordination.MaxCoordinateBytes ||
			len(cell.Value) > coordination.MaxManifestValueBytes {
			return fmt.Errorf("%w: cell %d is outside its bound", transaction.ErrInvalid, index)
		}
		if err := cell.LPART.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		if err := cell.CopyGeneration.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		if cell.Delete {
			if len(cell.Value) != 0 {
				return errors.Join(
					transaction.ErrInvalid,
					errors.New("physical tombstone cannot carry a value"),
				)
			}
			if !cell.EpochTimestamp {
				return errors.Join(
					transaction.ErrInvalid,
					errors.New("physical tombstone requires the transaction epoch"),
				)
			}
		}
		if cell.EpochTimestamp {
			if cell.Timestamp != 0 {
				return errors.Join(transaction.ErrInvalid, errors.New("epoch and explicit timestamps are mutually exclusive"))
			}
		} else if err := cell.Timestamp.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		if len(cell.IndexGeneration) == 0 != (len(cell.IndexFamily) == 0) {
			return errors.Join(transaction.ErrInvalid, errors.New("index generation and family must both be present"))
		}
		if len(cell.IndexGeneration) != 0 {
			if err := cell.IndexGeneration.Validate(); err != nil {
				return errors.Join(transaction.ErrInvalid, err)
			}
			if err := cell.IndexFamily.Validate(); err != nil {
				return errors.Join(transaction.ErrInvalid, err)
			}
		}
		physicalKey := intentPhysicalIdentity(cell)
		if _, duplicate := physicalKeys[physicalKey]; duplicate {
			return errors.Join(transaction.ErrInvalid, errors.New("intent has a duplicate physical key"))
		}
		physicalKeys[physicalKey] = struct{}{}
		key := string(cell.LPART)
		if previous, ok := partitions[key]; ok {
			if previous.CopyGeneration != cell.CopyGeneration ||
				!bytes.Equal(previous.Visibility, cell.Visibility) {
				return errors.Join(transaction.ErrInvalid, errors.New("one LPART has inconsistent copy metadata"))
			}
		} else {
			partitions[key] = cell
		}
	}
	if len(partitions) > coordination.MaxLPARTs {
		return errors.Join(transaction.ErrInvalid, errors.New("intent has too many LPARTs"))
	}
	for index, value := range intent.Guards {
		if value.Entity.Kind == 0 {
			return fmt.Errorf("%w: guard %d entity kind is required", transaction.ErrInvalid, index)
		}
		if err := value.Entity.ID.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		switch value.Mode {
		case guard.ModeAppend, guard.ModeAbsentOrIdentical, guard.ModeMutate, guard.ModeRetire:
		default:
			return errors.Join(transaction.ErrInvalid, errors.New("guard mode is invalid"))
		}
		if value.ExpectedEpoch == 0 {
			if value.ExpectedDigest != (coordination.Digest{}) {
				return errors.Join(transaction.ErrInvalid, errors.New("absent expected epoch has a digest"))
			}
		} else {
			if err := value.ExpectedEpoch.Validate(); err != nil {
				return errors.Join(transaction.ErrInvalid, err)
			}
			if err := value.ExpectedDigest.Validate("expected digest"); err != nil {
				return errors.Join(transaction.ErrInvalid, err)
			}
		}
		if (value.Mode == guard.ModeMutate || value.Mode == guard.ModeRetire) &&
			value.ExpectedEpoch == 0 {
			return errors.Join(transaction.ErrInvalid, errors.New("mutate and retire guards require an expected epoch"))
		}
		if value.DesiredDigest != (coordination.Digest{}) {
			if err := value.DesiredDigest.Validate("desired digest"); err != nil {
				return errors.Join(transaction.ErrInvalid, err)
			}
		}
		if value.DesiredState != guard.StateLive && value.DesiredState != guard.StateTombstone {
			return errors.Join(transaction.ErrInvalid, errors.New("guard desired state is invalid"))
		}
		if len(value.DesiredWinnerID) == 0 || len(value.DesiredWinnerID) > coordination.MaxOpaqueIDBytes ||
			len(value.LogicalPolicyID) == 0 || len(value.LogicalPolicyID) > coordination.MaxOpaqueIDBytes {
			return errors.Join(transaction.ErrInvalid, errors.New("guard identity is outside its bound"))
		}
		if err := value.LPART.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		if _, ok := partitions[string(value.LPART)]; !ok {
			return errors.Join(transaction.ErrInvalid, errors.New("guard LPART has no physical cells"))
		}
		if err := value.RetirementGeneration.Validate(); err != nil {
			return errors.Join(transaction.ErrInvalid, err)
		}
		if index > 0 && value.Entity.Kind == intent.Guards[index-1].Entity.Kind &&
			bytes.Equal(value.Entity.ID, intent.Guards[index-1].Entity.ID) {
			return errors.Join(transaction.ErrInvalid, errors.New("intent has duplicate entity guards"))
		}
	}
	for index, result := range intent.Results {
		if len(result.Kind) == 0 || len(result.Kind) > coordination.MaxResultIdentityBytes ||
			len(result.ID) == 0 || len(result.ID) > coordination.MaxResultIdentityBytes {
			return fmt.Errorf("%w: result %d is outside its bound", transaction.ErrInvalid, index)
		}
		if index > 0 && bytes.Equal(result.Kind, intent.Results[index-1].Kind) &&
			bytes.Equal(result.ID, intent.Results[index-1].ID) {
			return errors.Join(transaction.ErrInvalid, errors.New("intent has duplicate results"))
		}
	}
	return nil
}

func buildPlan(intent Intent, logical coordination.Digest) (transaction.Plan, error) {
	type plannedCell struct {
		entry coordination.ManifestEntry
		cell  transaction.PhysicalCell
	}
	type copyState struct {
		generation coordination.Generation
		visibility []byte
		digest     coordination.Digest
		families   []coordination.Family
	}
	groups := make(map[string][]Cell)
	for _, cell := range intent.Cells {
		groups[string(cell.LPART)] = append(groups[string(cell.LPART)], cell)
	}
	copies := make(map[string]copyState, len(groups))
	for key, cells := range groups {
		encoded, err := json.Marshal(cells)
		if err != nil {
			return transaction.Plan{}, errors.Join(transaction.ErrInternal, err)
		}
		state := copyState{
			generation: cells[0].CopyGeneration,
			visibility: append([]byte(nil), cells[0].Visibility...),
			digest:     coordination.Sum(append([]byte("shoal-physical-copy-v1\x00"), encoded...)),
		}
		familySet := make(map[string]coordination.Family)
		for _, cell := range cells {
			if len(cell.IndexFamily) != 0 {
				familySet[string(cell.IndexFamily)] = append(coordination.Family(nil), cell.IndexFamily...)
			}
		}
		for _, family := range familySet {
			state.families = append(state.families, family)
		}
		sort.Slice(state.families, func(i, j int) bool {
			return bytes.Compare(state.families[i], state.families[j]) < 0
		})
		copies[key] = state
	}
	planned := make([]plannedCell, len(intent.Cells))
	for index, cell := range intent.Cells {
		copy := copies[string(cell.LPART)]
		slot := coordination.EpochSlotNone
		timestamp := cell.Timestamp
		if cell.EpochTimestamp {
			slot, timestamp = coordination.EpochSlotContent, 0
		}
		valueLength, valueDigest, err := transaction.PhysicalValueCommitment(
			cell.Delete,
			cell.Value,
		)
		if err != nil {
			return transaction.Plan{}, err
		}
		entry := coordination.ManifestEntry{
			Table: []byte(cell.Table), Row: append([]byte(nil), cell.Row...),
			ColumnFamily:    append([]byte(nil), cell.Family...),
			ColumnQualifier: append([]byte(nil), cell.Qualifier...),
			EpochSlot:       slot, ExplicitTimestamp: timestamp,
			ValueLength: valueLength, ValueDigest: valueDigest,
			LPART:            append(coordination.LPART(nil), cell.LPART...),
			CopyGeneration:   cell.CopyGeneration,
			VisibilityDigest: coordination.Sum(cell.Visibility),
			LogicalDigest:    logical, PhysicalCopyDigest: copy.digest,
			IGEN:   append(coordination.IGEN(nil), cell.IndexGeneration...),
			Family: append(coordination.Family(nil), cell.IndexFamily...),
		}
		planned[index] = plannedCell{
			entry: entry,
			cell: transaction.PhysicalCell{
				Entry: entry, Value: append([]byte(nil), cell.Value...),
				Visibility: append([]byte(nil), cell.Visibility...),
				Delete:     cell.Delete,
			},
		}
	}
	sort.Slice(planned, func(i, j int) bool {
		return coordination.CompareManifestEntries(planned[i].entry, planned[j].entry) < 0
	})
	entries := make([]coordination.ManifestEntry, len(planned))
	physical := make([]transaction.PhysicalCell, len(planned))
	firstByLPART := make(map[string]coordination.ManifestEntry)
	for index := range planned {
		if index > 0 && coordination.CompareManifestEntries(planned[index-1].entry, planned[index].entry) >= 0 {
			return transaction.Plan{}, errors.Join(transaction.ErrInvalid, errors.New("manifest entries are not unique"))
		}
		entries[index], physical[index] = planned[index].entry, planned[index].cell
		if _, ok := firstByLPART[string(entries[index].LPART)]; !ok {
			firstByLPART[string(entries[index].LPART)] = entries[index]
		}
	}
	chunks, err := coordination.ChunkManifest(entries)
	if err != nil {
		return transaction.Plan{}, errors.Join(transaction.ErrInvalid, err)
	}
	location := make(map[string][2]uint32)
	for chunkIndex, chunk := range chunks {
		for entryIndex, entry := range chunk.Entries {
			key := manifestIdentity(entry)
			location[key] = [2]uint32{uint32(chunkIndex), uint32(entryIndex)}
		}
	}
	plan := transaction.Plan{Chunks: chunks, Cells: physical}
	for ordinal, value := range intent.Guards {
		entry := firstByLPART[string(value.LPART)]
		position, ok := location[manifestIdentity(entry)]
		if !ok {
			return transaction.Plan{}, fmt.Errorf("%w: guard manifest entry is missing", transaction.ErrInternal)
		}
		desired := value.DesiredDigest
		if desired == (coordination.Digest{}) {
			desired = logical
		}
		copy := copies[string(value.LPART)]
		plan.Guards = append(plan.Guards, transaction.GuardPlan{
			Entity: value.Entity, Mode: value.Mode,
			ExpectedEpoch: value.ExpectedEpoch, ExpectedDigest: value.ExpectedDigest,
			DesiredState: value.DesiredState, DesiredWinnerID: append([]byte(nil), value.DesiredWinnerID...),
			DesiredDigest: desired, LPART: append(coordination.LPART(nil), value.LPART...),
			LogicalPolicyID:      append([]byte(nil), value.LogicalPolicyID...),
			RetirementGeneration: value.RetirementGeneration,
			ManifestChunk:        position[0], ManifestEntry: position[1], Ordinal: uint32(ordinal),
			PhysicalDigest: copy.digest,
		})
	}
	lparts := make([]string, 0, len(copies))
	for lpart := range copies {
		lparts = append(lparts, lpart)
	}
	sort.Slice(lparts, func(i, j int) bool {
		return bytes.Compare([]byte(lparts[i]), []byte(lparts[j])) < 0
	})
	for _, lpart := range lparts {
		copy := copies[lpart]
		plan.Copies = append(plan.Copies, transaction.CommitCopy{
			LPART: coordination.LPART([]byte(lpart)), CopyGeneration: copy.generation,
			VisibilityDigest: coordination.Sum(copy.visibility), LogicalDigest: logical,
			PhysicalCopyDigest: copy.digest, RequiredIndexFamilies: copy.families,
			Visibility: copy.visibility,
		})
	}
	for _, result := range intent.Results {
		plan.Results = append(plan.Results, coordination.ResultIdentity{
			Kind: append([]byte(nil), result.Kind...), ID: append([]byte(nil), result.ID...),
		})
	}
	if _, err := plan.Validate(); err != nil {
		return transaction.Plan{}, err
	}
	return plan, nil
}

func cloneIntent(intent Intent) Intent {
	result := Intent{
		Operation:   append([]byte(nil), intent.Operation...),
		Token:       append([]byte(nil), intent.Token...),
		RecordKey:   append([]byte(nil), intent.RecordKey...),
		RecordToken: append([]byte(nil), intent.RecordToken...),
		Cells:       append([]Cell(nil), intent.Cells...),
		Guards:      append([]GuardIntent(nil), intent.Guards...),
		Results:     append([]ResultIdentity(nil), intent.Results...),
	}
	for index := range result.Cells {
		source := intent.Cells[index]
		result.Cells[index].Row = append([]byte(nil), source.Row...)
		result.Cells[index].Family = append([]byte(nil), source.Family...)
		result.Cells[index].Qualifier = append([]byte(nil), source.Qualifier...)
		result.Cells[index].Visibility = append([]byte(nil), source.Visibility...)
		result.Cells[index].Value = append([]byte(nil), source.Value...)
		result.Cells[index].LPART = append(coordination.LPART(nil), source.LPART...)
		result.Cells[index].IndexGeneration = append(coordination.IGEN(nil), source.IndexGeneration...)
		result.Cells[index].IndexFamily = append(coordination.Family(nil), source.IndexFamily...)
	}
	for index := range result.Guards {
		source := intent.Guards[index]
		result.Guards[index].Entity.ID = append(coordination.EntityID(nil), source.Entity.ID...)
		result.Guards[index].DesiredWinnerID = append([]byte(nil), source.DesiredWinnerID...)
		result.Guards[index].LPART = append(coordination.LPART(nil), source.LPART...)
		result.Guards[index].LogicalPolicyID = append([]byte(nil), source.LogicalPolicyID...)
	}
	for index := range result.Results {
		result.Results[index].Kind = append([]byte(nil), intent.Results[index].Kind...)
		result.Results[index].ID = append([]byte(nil), intent.Results[index].ID...)
	}
	return result
}

func compareIntentCells(left, right Cell) int {
	for _, pair := range [][2][]byte{
		{[]byte(left.Table), []byte(right.Table)},
		{left.Row, right.Row},
		{left.Family, right.Family},
		{left.Qualifier, right.Qualifier},
		{left.Visibility, right.Visibility},
	} {
		if order := bytes.Compare(pair[0], pair[1]); order != 0 {
			return order
		}
	}
	if left.EpochTimestamp != right.EpochTimestamp {
		if !left.EpochTimestamp {
			return -1
		}
		return 1
	}
	if left.Timestamp != right.Timestamp {
		if left.Timestamp < right.Timestamp {
			return -1
		}
		return 1
	}
	if left.Delete != right.Delete {
		if !left.Delete {
			return -1
		}
		return 1
	}
	return bytes.Compare(left.Value, right.Value)
}

func intentPhysicalIdentity(cell Cell) string {
	var encoded []byte
	encoded = appendLength(encoded, []byte(cell.Table))
	for _, component := range [][]byte{
		cell.Row, cell.Family, cell.Qualifier, cell.Visibility,
	} {
		encoded = appendLength(encoded, component)
	}
	if cell.EpochTimestamp {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	return string(binary.BigEndian.AppendUint64(encoded, uint64(cell.Timestamp)))
}

func manifestIdentity(entry coordination.ManifestEntry) string {
	var encoded []byte
	for _, component := range [][]byte{
		entry.Table, entry.Row, entry.ColumnFamily, entry.ColumnQualifier,
	} {
		encoded = appendLength(encoded, component)
	}
	encoded = append(encoded, byte(entry.EpochSlot))
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(entry.ExplicitTimestamp))
	encoded = append(encoded, entry.ValueDigest[:]...)
	return string(encoded)
}

func encodeStoredIntent(record storedIntent) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxIntentBytes {
		return nil, errors.New("explorer coordination: durable intent exceeds its bound")
	}
	encoded := make([]byte, 0, 8+8+sha256.Size+len(payload))
	encoded = append(encoded, []byte("SHOALTI1")...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(payload)))
	digest := sha256.Sum256(payload)
	encoded = append(encoded, digest[:]...)
	return append(encoded, payload...), nil
}

func decodeStoredIntent(encoded []byte) (storedIntent, error) {
	const header = 8 + 8 + sha256.Size
	if len(encoded) < header || !bytes.Equal(encoded[:8], []byte("SHOALTI1")) {
		return storedIntent{}, errors.New("durable intent envelope is invalid")
	}
	size := binary.BigEndian.Uint64(encoded[8:16])
	if size > maxIntentBytes || size != uint64(len(encoded)-header) {
		return storedIntent{}, errors.New("durable intent payload length is invalid")
	}
	expected := sha256.Sum256(encoded[header:])
	if !bytes.Equal(expected[:], encoded[16:header]) {
		return storedIntent{}, errors.New("durable intent checksum mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded[header:]))
	decoder.DisallowUnknownFields()
	var result storedIntent
	if err := decoder.Decode(&result); err != nil {
		return storedIntent{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return storedIntent{}, err
	}
	if result.Version != intentVersion {
		return storedIntent{}, errors.New("durable intent version is unsupported")
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("durable intent has trailing JSON")
		}
		return err
	}
	return nil
}

func encodeCompletion(value intentCompletion) []byte {
	result := make([]byte, 0, 8+sha256.Size)
	result = binary.BigEndian.AppendUint64(result, uint64(value.Epoch))
	return append(result, value.Digest[:]...)
}

func decodeCompletion(value []byte) (intentCompletion, error) {
	if len(value) != 8+sha256.Size {
		return intentCompletion{}, errors.New("invalid completion length")
	}
	result := intentCompletion{Epoch: coordination.Epoch(binary.BigEndian.Uint64(value[:8]))}
	copy(result.Digest[:], value[8:])
	if err := result.Epoch.Validate(); err != nil {
		return intentCompletion{}, err
	}
	if err := result.Digest.Validate("completion digest"); err != nil {
		return intentCompletion{}, err
	}
	return result, nil
}

func writeDigestPart(hash interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write(value)
}

func appendLength(destination, value []byte) []byte {
	destination = binary.BigEndian.AppendUint64(destination, uint64(len(value)))
	return append(destination, value...)
}

var (
	_ transaction.Materializer = (*IntentStore)(nil)
	_ transaction.Quarantine   = (*IntentStore)(nil)
)
