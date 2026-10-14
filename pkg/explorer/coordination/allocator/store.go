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

// Package allocator implements Explorer's row-atomic epoch allocator lifecycle.
package allocator

import (
	"bytes"
	"context"
	"errors"
)

var ErrConditionalUnknown = errors.New("allocator: conditional result unknown")

type Status uint8

const (
	StatusUnknown Status = iota
	StatusAccepted
	StatusRejected
)

type Coordinate struct {
	Row        []byte
	Family     []byte
	Qualifier  []byte
	Visibility []byte
}

func (c Coordinate) clone() Coordinate {
	return Coordinate{
		Row: append([]byte(nil), c.Row...), Family: append([]byte(nil), c.Family...),
		Qualifier: append([]byte(nil), c.Qualifier...), Visibility: append([]byte(nil), c.Visibility...),
	}
}

func (c Coordinate) equal(other Coordinate) bool {
	return bytes.Equal(c.Row, other.Row) && bytes.Equal(c.Family, other.Family) &&
		bytes.Equal(c.Qualifier, other.Qualifier) && bytes.Equal(c.Visibility, other.Visibility)
}

type Cell struct {
	Coordinate Coordinate
	Value      []byte
	Timestamp  int64
}

type Condition struct {
	Coordinate   Coordinate
	Value        []byte
	Absent       bool
	Timestamp    int64
	TimestampSet bool
}

type Update struct {
	Coordinate Coordinate
	Value      []byte
	Delete     bool
	Timestamp  int64
}

type Mutation struct {
	Row        []byte
	Conditions []Condition
	Updates    []Update
}

// Store is an exact-coordinate, bounded scan, and one-row CAS seam. Implementations
// must return ErrConditionalUnknown only when a mutation may have applied.
type Store interface {
	ReadExact(context.Context, []Coordinate) ([]Cell, error)
	ScanRowPrefix(context.Context, []byte, []byte, []byte, []byte, int) ([]Cell, error)
	CompareAndMutate(context.Context, Mutation) (Status, error)
}
