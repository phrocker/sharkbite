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

package transaction

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type FaultMode uint8

const (
	FaultUnknownBefore FaultMode = iota + 1
	FaultUnknownAfter
	FaultUnavailableBefore
)

type MemoryStore struct {
	mu         sync.Mutex
	control    map[string][]allocator.Cell
	physical   map[string]PhysicalValue
	faults     []FaultMode
	quarantine map[string]string
}

type PhysicalValue struct {
	Table      []byte
	Row        []byte
	Family     []byte
	Qualifier  []byte
	Visibility []byte
	Timestamp  int64
	Value      []byte
	Delete     bool
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		control: make(map[string][]allocator.Cell), physical: make(map[string]PhysicalValue),
		quarantine: make(map[string]string),
	}
}

func (s *MemoryStore) Inject(faults ...FaultMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, faults...)
}

func (s *MemoryStore) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

func (s *MemoryStore) nextFault() FaultMode {
	if len(s.faults) == 0 {
		return 0
	}
	value := s.faults[0]
	s.faults = s.faults[1:]
	return value
}

func controlKey(c allocator.Coordinate) string {
	return string(c.Row) + "\x00" + string(c.Family) + "\x00" +
		string(c.Qualifier) + "\x00" + string(c.Visibility)
}

func cloneCoordinate(c allocator.Coordinate) allocator.Coordinate {
	return allocator.Coordinate{
		Row: append([]byte(nil), c.Row...), Family: append([]byte(nil), c.Family...),
		Qualifier: append([]byte(nil), c.Qualifier...), Visibility: append([]byte(nil), c.Visibility...),
	}
}

func cloneCell(cell allocator.Cell) allocator.Cell {
	return allocator.Cell{
		Coordinate: cloneCoordinate(cell.Coordinate),
		Value:      append([]byte(nil), cell.Value...),
		Timestamp:  cell.Timestamp,
	}
}

func (s *MemoryStore) ReadExact(_ context.Context, coordinates []allocator.Coordinate) ([]allocator.Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]allocator.Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		versions := s.control[controlKey(coordinate)]
		if len(versions) != 0 {
			result = append(result, cloneCell(versions[len(versions)-1]))
		}
	}
	return result, nil
}

func (s *MemoryStore) ScanRowPrefix(
	_ context.Context,
	row, family, qualifierStart, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 {
		return nil, errors.New("memory transaction store: invalid scan limit")
	}
	var result []allocator.Cell
	for _, versions := range s.control {
		cell := versions[len(versions)-1]
		if bytes.Equal(cell.Coordinate.Row, row) && bytes.Equal(cell.Coordinate.Family, family) &&
			bytes.Equal(cell.Coordinate.Visibility, visibility) &&
			bytes.Compare(cell.Coordinate.Qualifier, qualifierStart) >= 0 {
			result = append(result, cloneCell(cell))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Qualifier, result[j].Coordinate.Qualifier) < 0
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) ScanPrefixFrom(
	_ context.Context,
	rowPrefix, startRow, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || !bytes.HasPrefix(startRow, rowPrefix) {
		return nil, errors.New("memory transaction store: invalid prefix scan")
	}
	selected := make(map[string]allocator.Cell)
	for _, versions := range s.control {
		cell := versions[len(versions)-1]
		if bytes.HasPrefix(cell.Coordinate.Row, rowPrefix) &&
			bytes.Compare(cell.Coordinate.Row, startRow) >= 0 &&
			bytes.Equal(cell.Coordinate.Family, family) &&
			bytes.Equal(cell.Coordinate.Qualifier, qualifier) &&
			bytes.Equal(cell.Coordinate.Visibility, visibility) {
			selected[string(cell.Coordinate.Row)] = cloneCell(cell)
		}
	}
	result := make([]allocator.Cell, 0, len(selected))
	for _, cell := range selected {
		result = append(result, cell)
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Row, result[j].Coordinate.Row) < 0
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *MemoryStore) CompareAndMutate(_ context.Context, mutation allocator.Mutation) (allocator.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault := s.nextFault()
	if fault == FaultUnavailableBefore {
		return allocator.StatusUnknown, errors.New("memory transaction store: unavailable")
	}
	if fault == FaultUnknownBefore {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	for _, condition := range mutation.Conditions {
		versions := s.control[controlKey(condition.Coordinate)]
		if condition.Absent {
			if len(versions) != 0 {
				return allocator.StatusRejected, nil
			}
			continue
		}
		if len(versions) == 0 {
			return allocator.StatusRejected, nil
		}
		cell := versions[len(versions)-1]
		if !bytes.Equal(cell.Value, condition.Value) ||
			condition.TimestampSet && cell.Timestamp != condition.Timestamp {
			return allocator.StatusRejected, nil
		}
	}
	for _, update := range mutation.Updates {
		key := controlKey(update.Coordinate)
		versions := s.control[key]
		if update.Delete {
			delete(s.control, key)
			continue
		}
		if len(versions) != 0 {
			latest := versions[len(versions)-1]
			if update.Timestamp < latest.Timestamp {
				continue
			}
			if update.Timestamp == latest.Timestamp {
				if !bytes.Equal(update.Value, latest.Value) {
					return allocator.StatusUnknown, errors.New("memory transaction store: same timestamp has different value")
				}
				continue
			}
		}
		s.control[key] = append(versions, allocator.Cell{
			Coordinate: cloneCoordinate(update.Coordinate),
			Value:      append([]byte(nil), update.Value...), Timestamp: update.Timestamp,
		})
	}
	if fault == FaultUnknownAfter {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	return allocator.StatusAccepted, nil
}

func physicalKey(value PhysicalValue) string {
	return string(value.Table) + "\x00" + string(value.Row) + "\x00" +
		string(value.Family) + "\x00" + string(value.Qualifier) + "\x00" +
		string(value.Visibility) + "\x00" + string(coordination.U64(uint64(value.Timestamp)))
}

func expandCell(epoch coordination.Epoch, cell PhysicalCell) PhysicalValue {
	timestamp := int64(cell.Entry.ExplicitTimestamp)
	if cell.Entry.EpochSlot == coordination.EpochSlotContent {
		timestamp = int64(epoch)
	}
	return PhysicalValue{
		Table: append([]byte(nil), cell.Entry.Table...), Row: append([]byte(nil), cell.Entry.Row...),
		Family:     append([]byte(nil), cell.Entry.ColumnFamily...),
		Qualifier:  append([]byte(nil), cell.Entry.ColumnQualifier...),
		Visibility: append([]byte(nil), cell.Visibility...), Timestamp: timestamp,
		Value: append([]byte(nil), cell.Value...), Delete: cell.Delete,
	}
}

func (s *MemoryStore) Write(_ context.Context, epoch coordination.Epoch, cells []PhysicalCell) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault := s.nextFault()
	if fault == FaultUnavailableBefore || fault == FaultUnknownBefore {
		return ErrUnavailable
	}
	for _, cell := range cells {
		value := expandCell(epoch, cell)
		key := physicalKey(value)
		if existing, found := s.physical[key]; found &&
			(existing.Delete != value.Delete ||
				!value.Delete && !bytes.Equal(existing.Value, value.Value)) {
			return ErrInternal
		}
		s.physical[key] = value
	}
	if fault == FaultUnknownAfter {
		return ErrUnavailable
	}
	return nil
}

func (s *MemoryStore) Verify(_ context.Context, epoch coordination.Epoch, cells []PhysicalCell) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fault := s.nextFault()
	if fault != 0 {
		return ErrUnavailable
	}
	expected := mapTrusted(epoch, cells)
	got := make([]TrustedCell, 0, len(cells))
	for _, cell := range cells {
		physical := expandCell(epoch, cell)
		value, found := s.physical[physicalKey(physical)]
		if found {
			got = append(got, TrustedCell{
				Table: value.Table, Row: value.Row, Family: value.Family,
				Qualifier: value.Qualifier, Visibility: value.Visibility,
				Timestamp: value.Timestamp, Value: value.Value, Delete: value.Delete,
			})
		}
	}
	return verifyTrustedCells(expected, got, physicalValueDigests(cells))
}

func (s *MemoryStore) Record(_ context.Context, domain coordination.DomainID, txn coordination.TXN, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quarantine[string(domain)+"\x00"+string(txn)] = reason
	return nil
}

func (s *MemoryStore) Quarantined(domain coordination.DomainID, txn coordination.TXN) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.quarantine[string(domain)+"\x00"+string(txn)]
	return found
}

func (s *MemoryStore) DeletePhysical(epoch coordination.Epoch, cell PhysicalCell) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.physical, physicalKey(expandCell(epoch, cell)))
}

var _ Store = (*MemoryStore)(nil)
var _ PhysicalWriter = (*MemoryStore)(nil)
var _ PhysicalVerifier = (*MemoryStore)(nil)
var _ Quarantine = (*MemoryStore)(nil)
