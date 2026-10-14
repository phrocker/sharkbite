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
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type AccumuloConfig = allocator.AccumuloConfig
type AccumuloStore = allocator.AccumuloStore

func NewAccumuloStore(config AccumuloConfig) (*AccumuloStore, error) {
	return allocator.NewAccumuloStore(config)
}

// TrustedCell is the complete physical mutation mapping produced only by a
// trusted Explorer adapter. Public APIs never accept it.
type TrustedCell struct {
	Table      []byte
	Row        []byte
	Family     []byte
	Qualifier  []byte
	Visibility []byte
	Timestamp  int64
	Value      []byte
	Delete     bool
}

type TrustedPhysicalSink interface {
	WriteTrusted(context.Context, []TrustedCell) error
	ReadTrusted(context.Context, []TrustedCell) ([]TrustedCell, error)
}

type AccumuloPhysicalAdapter struct {
	sink TrustedPhysicalSink
}

func NewAccumuloPhysicalAdapter(sink TrustedPhysicalSink) (*AccumuloPhysicalAdapter, error) {
	if sink == nil {
		return nil, errors.New("explorer transaction: trusted Accumulo physical sink is required")
	}
	return &AccumuloPhysicalAdapter{sink: sink}, nil
}

func mapTrusted(epoch coordination.Epoch, cells []PhysicalCell) []TrustedCell {
	result := make([]TrustedCell, len(cells))
	for i, cell := range cells {
		timestamp := int64(cell.Entry.ExplicitTimestamp)
		if cell.Entry.EpochSlot == coordination.EpochSlotContent {
			timestamp = int64(epoch)
		}
		result[i] = TrustedCell{
			Table: append([]byte(nil), cell.Entry.Table...), Row: append([]byte(nil), cell.Entry.Row...),
			Family:     append([]byte(nil), cell.Entry.ColumnFamily...),
			Qualifier:  append([]byte(nil), cell.Entry.ColumnQualifier...),
			Visibility: append([]byte(nil), cell.Visibility...), Timestamp: timestamp,
			Value: append([]byte(nil), cell.Value...), Delete: cell.Delete,
		}
	}
	return result
}

func (a *AccumuloPhysicalAdapter) Write(ctx context.Context, epoch coordination.Epoch, cells []PhysicalCell) error {
	return a.sink.WriteTrusted(ctx, mapTrusted(epoch, cells))
}

func (a *AccumuloPhysicalAdapter) Verify(ctx context.Context, epoch coordination.Epoch, cells []PhysicalCell) error {
	wanted := mapTrusted(epoch, cells)
	got, err := a.sink.ReadTrusted(ctx, wanted)
	if err != nil {
		return err
	}
	return verifyTrustedCells(wanted, got, physicalValueDigests(cells))
}

type expectedTrustedCell struct {
	cell        TrustedCell
	valueDigest coordination.Digest
}

func verifyTrustedCells(wanted, got []TrustedCell, valueDigests []coordination.Digest) error {
	if len(wanted) != len(valueDigests) {
		return fmt.Errorf("%w: physical verification digest count mismatch", ErrInternal)
	}
	expected := make(map[string]expectedTrustedCell, len(wanted))
	for ordinal, cell := range wanted {
		key := trustedCellKey(cell)
		if _, exists := expected[key]; exists {
			return fmt.Errorf("%w: expected physical key duplicate at ordinal %d", ErrInternal, ordinal)
		}
		_, digest, err := PhysicalValueCommitment(cell.Delete, cell.Value)
		if err != nil || digest != valueDigests[ordinal] {
			return fmt.Errorf("%w: expected physical value digest mismatch at ordinal %d", ErrInternal, ordinal)
		}
		expected[key] = expectedTrustedCell{cell: cell, valueDigest: valueDigests[ordinal]}
	}
	seen := make(map[string]struct{}, len(got))
	for ordinal, cell := range got {
		key := trustedCellKey(cell)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: returned physical key duplicate at ordinal %d", ErrInternal, ordinal)
		}
		seen[key] = struct{}{}
		wantedCell, exists := expected[key]
		if !exists {
			return fmt.Errorf("%w: unexpected physical key at ordinal %d", ErrInternal, ordinal)
		}
		_, digest, err := PhysicalValueCommitment(cell.Delete, cell.Value)
		if err != nil ||
			wantedCell.cell.Delete != cell.Delete ||
			!bytes.Equal(wantedCell.cell.Value, cell.Value) ||
			digest != wantedCell.valueDigest {
			return fmt.Errorf("%w: physical value digest mismatch at ordinal %d", ErrInternal, ordinal)
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("%w: missing physical key", ErrInternal)
	}
	return nil
}

func physicalValueDigests(cells []PhysicalCell) []coordination.Digest {
	result := make([]coordination.Digest, len(cells))
	for i := range cells {
		result[i] = cells[i].Entry.ValueDigest
	}
	return result
}

func trustedCellKey(cell TrustedCell) string {
	encoded := make([]byte, 0, 64)
	var length [8]byte
	for _, component := range [][]byte{
		cell.Table, cell.Row, cell.Family, cell.Qualifier, cell.Visibility,
	} {
		binary.BigEndian.PutUint64(length[:], uint64(len(component)))
		encoded = append(encoded, length[:]...)
		encoded = append(encoded, component...)
	}
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(cell.Timestamp))
	encoded = append(encoded, timestamp[:]...)
	return string(encoded)
}

var _ Store = (*AccumuloStore)(nil)
var _ PhysicalWriter = (*AccumuloPhysicalAdapter)(nil)
var _ PhysicalVerifier = (*AccumuloPhysicalAdapter)(nil)
