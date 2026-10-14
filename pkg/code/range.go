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

package code

import "github.com/phrocker/shoal-oss/pkg/shoal"

// Position identifies an exact source boundary. Byte offsets are zero-based;
// lines and UTF-8 byte columns are one-based.
type Position struct {
	byteOffset uint64
	line       uint32
	column     uint32
}

// NewPosition creates an exact source position.
func NewPosition(byteOffset uint64, line, column uint32) (Position, error) {
	position := Position{byteOffset: byteOffset, line: line, column: column}
	if err := position.Validate(); err != nil {
		return Position{}, err
	}
	return position, nil
}

func (p Position) Validate() error {
	if p.line == 0 || p.column == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source line and column are one-based")
	}
	return nil
}

func (p Position) ByteOffset() uint64 {
	return p.byteOffset
}

func (p Position) Line() uint32 {
	return p.line
}

func (p Position) Column() uint32 {
	return p.column
}

// Range is an exact half-open source interval [Start, End).
type Range struct {
	start Position
	end   Position
}

// NewRange creates and validates a half-open source interval.
func NewRange(start, end Position) (Range, error) {
	sourceRange := Range{start: start, end: end}
	if err := sourceRange.Validate(); err != nil {
		return Range{}, err
	}
	return sourceRange, nil
}

func (r Range) Validate() error {
	order, err := comparePositions(r.start, r.end)
	if err != nil {
		return err
	}
	if order > 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "source range is reversed")
	}
	return nil
}

func (r Range) Start() Position {
	return r.start
}

func (r Range) End() Position {
	return r.end
}

// Contains reports whether inner is fully contained in the half-open range.
func (r Range) Contains(inner Range) bool {
	if r.Validate() != nil || inner.Validate() != nil {
		return false
	}
	startOrder, err := comparePositions(r.start, inner.start)
	if err != nil || startOrder > 0 {
		return false
	}
	endOrder, err := comparePositions(inner.end, r.end)
	return err == nil && endOrder <= 0
}

// IsEmpty reports whether the range has no source bytes.
func (r Range) IsEmpty() bool {
	return r.start.byteOffset == r.end.byteOffset
}

func comparePositions(left, right Position) (int, error) {
	if err := left.Validate(); err != nil {
		return 0, err
	}
	if err := right.Validate(); err != nil {
		return 0, err
	}

	byteOrder := compareUint64(left.byteOffset, right.byteOffset)
	coordinateOrder := compareLineColumn(left, right)
	if byteOrder == 0 && coordinateOrder != 0 ||
		byteOrder != 0 && coordinateOrder == 0 ||
		byteOrder < 0 && coordinateOrder > 0 ||
		byteOrder > 0 && coordinateOrder < 0 {
		return 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "byte and line/column positions disagree")
	}
	return byteOrder, nil
}

func compareLineColumn(left, right Position) int {
	if left.line < right.line {
		return -1
	}
	if left.line > right.line {
		return 1
	}
	if left.column < right.column {
		return -1
	}
	if left.column > right.column {
		return 1
	}
	return 0
}

func compareUint64(left, right uint64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
