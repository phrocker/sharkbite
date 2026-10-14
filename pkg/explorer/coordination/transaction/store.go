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
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

var (
	familyState    = []byte("s")
	qualifierRoot  = []byte("root")
	qualifierLease = []byte("lease")
	familyManifest = []byte("m")
	qualifierChunk = []byte("chunk")
	familyPublish  = []byte("p")
)

type storedTxn struct {
	root  coordination.TxnRootV3
	lease coordination.TxnLeaseV1
}

func coordinate(row, family, qualifier, visibility []byte) allocator.Coordinate {
	return allocator.Coordinate{
		Row: append([]byte(nil), row...), Family: append([]byte(nil), family...),
		Qualifier: append([]byte(nil), qualifier...), Visibility: append([]byte(nil), visibility...),
	}
}

func (c *Coordinator) txnCoordinates(txn coordination.TXN) (allocator.Coordinate, allocator.Coordinate, error) {
	row, err := coordination.TxnRow(c.domain, txn)
	if err != nil {
		return allocator.Coordinate{}, allocator.Coordinate{}, err
	}
	return coordinate(row, familyState, qualifierRoot, c.visibility),
		coordinate(row, familyState, qualifierLease, c.visibility), nil
}

func (c *Coordinator) readTxn(ctx context.Context, txn coordination.TXN) (storedTxn, error) {
	rootCoord, leaseCoord, err := c.txnCoordinates(txn)
	if err != nil {
		return storedTxn{}, errors.Join(ErrInvalid, err)
	}
	cells, err := c.store.ReadExact(ctx, []allocator.Coordinate{rootCoord, leaseCoord})
	if err != nil {
		return storedTxn{}, errors.Join(ErrUnavailable, err)
	}
	var result storedTxn
	var rootFound, leaseFound bool
	for _, cell := range cells {
		switch {
		case sameCoordinate(cell.Coordinate, rootCoord):
			rootFound = true
			result.root, err = coordination.UnmarshalTxnRootV3(cell.Value)
			if err != nil || cell.Timestamp != int64(result.root.StateGeneration) {
				return storedTxn{}, fmt.Errorf("%w: invalid transaction root", ErrInternal)
			}
		case sameCoordinate(cell.Coordinate, leaseCoord):
			leaseFound = true
			result.lease, err = coordination.UnmarshalTxnLeaseV1(cell.Value)
			if err != nil || cell.Timestamp != int64(result.lease.Generation) {
				return storedTxn{}, fmt.Errorf("%w: invalid transaction lease", ErrInternal)
			}
		default:
			return storedTxn{}, fmt.Errorf("%w: unexpected transaction cell", ErrInternal)
		}
	}
	if !rootFound && !leaseFound {
		return storedTxn{}, ErrNotFound
	}
	if !rootFound || !leaseFound || !bytes.Equal(result.root.Owner, result.lease.Owner) ||
		result.root.Fence != result.lease.Fence {
		return storedTxn{}, fmt.Errorf("%w: transaction root and lease disagree", ErrInternal)
	}
	return result, nil
}

func sameCoordinate(a, b allocator.Coordinate) bool {
	return bytes.Equal(a.Row, b.Row) && bytes.Equal(a.Family, b.Family) &&
		bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Visibility, b.Visibility)
}

func exactCondition(cell allocator.Coordinate, value []byte, timestamp int64) allocator.Condition {
	return allocator.Condition{
		Coordinate: cell, Value: value, Timestamp: timestamp, TimestampSet: true,
	}
}
