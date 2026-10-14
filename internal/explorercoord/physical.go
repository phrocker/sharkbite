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
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

type physicalCell struct {
	table      string
	row        []byte
	family     []byte
	qualifier  []byte
	visibility []byte
	value      []byte
	timestamp  int64
	delete     bool
}

// Physical writes trusted manifest cells to the embedded engine in stable
// table/row order and independently reads every exact version back.
type Physical struct {
	engine *engine.Engine
}

func NewPhysical(eng *engine.Engine) (*Physical, error) {
	if eng == nil {
		return nil, errors.New("explorer coordination: engine is required")
	}
	return &Physical{engine: eng}, nil
}

func (p *Physical) Write(
	ctx context.Context,
	epoch coordination.Epoch,
	cells []transaction.PhysicalCell,
) error {
	mapped, err := mapPhysical(epoch, cells)
	if err != nil {
		return err
	}
	sortPhysical(mapped)
	for start := 0; start < len(mapped); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + 1
		for end < len(mapped) && mapped[end].table == mapped[start].table &&
			bytes.Equal(mapped[end].row, mapped[start].row) {
			end++
		}
		if err := p.writeRow(ctx, mapped[start:end]); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func (p *Physical) Verify(
	ctx context.Context,
	epoch coordination.Epoch,
	cells []transaction.PhysicalCell,
) error {
	mapped, err := mapPhysical(epoch, cells)
	if err != nil {
		return err
	}
	for index := range mapped {
		got, found, readErr := p.readExact(ctx, mapped[index])
		if readErr != nil {
			return readErr
		}
		if !found {
			return fmt.Errorf("%w: physical cell %d is missing", transaction.ErrInternal, index)
		}
		if got.delete != mapped[index].delete ||
			!mapped[index].delete &&
				!bytes.Equal(got.value, mapped[index].value) {
			return fmt.Errorf("%w: physical cell %d value differs", transaction.ErrInternal, index)
		}
	}
	return nil
}

func (p *Physical) writeRow(ctx context.Context, cells []physicalCell) error {
	missing := make([]physicalCell, 0, len(cells))
	for index := range cells {
		got, found, err := p.readExact(ctx, cells[index])
		if err != nil {
			return err
		}
		if found {
			if got.delete != cells[index].delete ||
				!cells[index].delete &&
					!bytes.Equal(got.value, cells[index].value) {
				return fmt.Errorf("%w: physical key already has a different value", transaction.ErrInternal)
			}
			continue
		}
		missing = append(missing, cells[index])
	}
	if len(missing) == 0 {
		return nil
	}
	mutation, err := cclient.NewMutation(cells[0].row)
	if err != nil {
		return err
	}
	conditions := make([]engine.Condition, len(missing))
	for index := range missing {
		cell := missing[index]
		timestamp := cell.timestamp
		conditions[index] = engine.Condition{
			ColumnFamily:     append([]byte(nil), cell.family...),
			ColumnQualifier:  append([]byte(nil), cell.qualifier...),
			ColumnVisibility: append([]byte(nil), cell.visibility...),
			Timestamp:        &timestamp,
			Kind:             engine.ConditionAbsent,
		}
		if cell.delete {
			mutation.Delete(cell.family, cell.qualifier, cell.visibility, cell.timestamp)
		} else {
			mutation.Put(cell.family, cell.qualifier, cell.visibility, cell.timestamp, cell.value)
		}
	}
	results, err := p.engine.ConditionalWrite(cells[0].table, []engine.ConditionalMutation{{
		Mutation: mutation, Conditions: conditions,
	}})
	if err != nil {
		return errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown, err)
	}
	if len(results) != 1 {
		return fmt.Errorf("%w: physical conditional result count is invalid", transaction.ErrInternal)
	}
	if results[0] {
		return nil
	}
	for index := range cells {
		got, found, readErr := p.readExact(ctx, cells[index])
		if readErr != nil {
			return readErr
		}
		if !found {
			return errors.Join(transaction.ErrUnavailable, allocator.ErrConditionalUnknown)
		}
		if got.delete != cells[index].delete ||
			!cells[index].delete &&
				!bytes.Equal(got.value, cells[index].value) {
			return fmt.Errorf("%w: physical CAS observed a divergent value", transaction.ErrInternal)
		}
	}
	return nil
}

func (p *Physical) readExact(
	ctx context.Context,
	wanted physicalCell,
) (physicalCell, bool, error) {
	if err := ctx.Err(); err != nil {
		return physicalCell{}, false, err
	}
	scanner, err := p.engine.Scan(wanted.table, exactRowRange(wanted.row), engine.ScanOptions{
		ColumnFamilies:          [][]byte{append([]byte(nil), wanted.family...)},
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		return physicalCell{}, false, err
	}
	defer scanner.Close()
	var result physicalCell
	found := false
	for scanner.Next() {
		if err := ctx.Err(); err != nil {
			return physicalCell{}, false, err
		}
		key := scanner.Key()
		if bytes.Equal(key.Row, wanted.row) &&
			bytes.Equal(key.ColumnFamily, wanted.family) &&
			bytes.Equal(key.ColumnQualifier, wanted.qualifier) &&
			bytes.Equal(key.ColumnVisibility, wanted.visibility) &&
			key.Timestamp == wanted.timestamp {
			current := wanted
			current.delete = key.Deleted
			current.value = append([]byte(nil), scanner.Value()...)
			if found &&
				(result.delete != current.delete ||
					!current.delete &&
						!bytes.Equal(result.value, current.value)) {
				return physicalCell{}, false, fmt.Errorf(
					"%w: physical key has divergent exact versions",
					transaction.ErrInternal,
				)
			}
			result = current
			found = true
		}
		if err := scanner.Advance(); err != nil {
			return physicalCell{}, false, err
		}
	}
	return result, found, nil
}

func mapPhysical(
	epoch coordination.Epoch,
	cells []transaction.PhysicalCell,
) ([]physicalCell, error) {
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	result := make([]physicalCell, len(cells))
	seen := make(map[string][]byte, len(cells))
	for index, cell := range cells {
		if err := cell.Entry.Validate(); err != nil {
			return nil, errors.Join(transaction.ErrInvalid, err)
		}
		valueLength, valueDigest, commitmentErr := transaction.PhysicalValueCommitment(
			cell.Delete,
			cell.Value,
		)
		if commitmentErr != nil {
			return nil, commitmentErr
		}
		if valueLength != cell.Entry.ValueLength ||
			valueDigest != cell.Entry.ValueDigest ||
			coordination.Sum(cell.Visibility) != cell.Entry.VisibilityDigest {
			return nil, fmt.Errorf("%w: physical cell %d disagrees with its manifest", transaction.ErrInternal, index)
		}
		table := string(cell.Entry.Table)
		if !validTableName(table) {
			return nil, fmt.Errorf("%w: physical table name is invalid", transaction.ErrInvalid)
		}
		timestamp := int64(cell.Entry.ExplicitTimestamp)
		if cell.Entry.EpochSlot == coordination.EpochSlotContent {
			timestamp = int64(epoch)
		}
		result[index] = physicalCell{
			table: table, row: append([]byte(nil), cell.Entry.Row...),
			family:     append([]byte(nil), cell.Entry.ColumnFamily...),
			qualifier:  append([]byte(nil), cell.Entry.ColumnQualifier...),
			visibility: append([]byte(nil), cell.Visibility...),
			value:      append([]byte(nil), cell.Value...), timestamp: timestamp,
			delete: cell.Delete,
		}
		key := physicalIdentity(result[index])
		if previous, ok := seen[key]; ok {
			if !bytes.Equal(previous, result[index].value) {
				return nil, fmt.Errorf("%w: duplicate physical key has divergent values", transaction.ErrInternal)
			}
			return nil, fmt.Errorf("%w: duplicate physical key", transaction.ErrInvalid)
		}
		seen[key] = result[index].value
	}
	return result, nil
}

func sortPhysical(cells []physicalCell) {
	sort.Slice(cells, func(i, j int) bool {
		left, right := cells[i], cells[j]
		if left.table != right.table {
			return left.table < right.table
		}
		for _, pair := range [][2][]byte{
			{left.row, right.row},
			{left.family, right.family},
			{left.qualifier, right.qualifier},
			{left.visibility, right.visibility},
		} {
			if order := bytes.Compare(pair[0], pair[1]); order != 0 {
				return order < 0
			}
		}
		return left.timestamp > right.timestamp
	})
}

func physicalIdentity(cell physicalCell) string {
	key := &wire.Key{
		Row: cell.row, ColumnFamily: cell.family, ColumnQualifier: cell.qualifier,
		ColumnVisibility: cell.visibility, Timestamp: cell.timestamp,
	}
	var encoded []byte
	for _, component := range [][]byte{
		[]byte(cell.table), key.Row, key.ColumnFamily, key.ColumnQualifier, key.ColumnVisibility,
	} {
		encoded = append(encoded, byte(len(component)>>24), byte(len(component)>>16), byte(len(component)>>8), byte(len(component)))
		encoded = append(encoded, component...)
	}
	for shift := 56; shift >= 0; shift -= 8 {
		encoded = append(encoded, byte(uint64(key.Timestamp)>>shift))
	}
	return string(encoded)
}

func validTableName(table string) bool {
	return table != "" && utf8.ValidString(table) && filepath.Base(table) == table &&
		!strings.ContainsAny(table, `/\`) && table != "." && table != ".."
}

var (
	_ transaction.PhysicalWriter   = (*Physical)(nil)
	_ transaction.PhysicalVerifier = (*Physical)(nil)
)
