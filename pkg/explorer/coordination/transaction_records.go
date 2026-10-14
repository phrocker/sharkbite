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

package coordination

import (
	"bytes"
	"sort"
	"time"
)

// PartitionCommitCopyV1 is the physical publication fence for one LPART copy.
type PartitionCommitCopyV1 struct {
	State                 TxnState
	TXN                   TXN
	Epoch                 Epoch
	LPART                 LPART
	CopyGeneration        Generation
	VisibilityDigest      Digest
	LogicalDigest         Digest
	PhysicalCopyDigest    Digest
	RequiredIndexFamilies []Family
}

func (c PartitionCommitCopyV1) Validate() error {
	if c.State != StateCommitted {
		return invalid("partition commit copy must be COMMITTED")
	}
	if err := c.TXN.Validate(); err != nil {
		return err
	}
	if err := c.Epoch.Validate(); err != nil {
		return err
	}
	if err := c.LPART.Validate(); err != nil {
		return err
	}
	if err := c.CopyGeneration.Validate(); err != nil {
		return err
	}
	for _, value := range []struct {
		name string
		d    Digest
	}{
		{"visibility digest", c.VisibilityDigest},
		{"logical digest", c.LogicalDigest},
		{"physical copy digest", c.PhysicalCopyDigest},
	} {
		if err := value.d.Validate(value.name); err != nil {
			return err
		}
	}
	if len(c.RequiredIndexFamilies) > MaxIndexPins {
		return invalid("partition commit copy has too many index families")
	}
	for i, family := range c.RequiredIndexFamilies {
		if err := family.Validate(); err != nil {
			return err
		}
		if i > 0 && bytes.Compare(c.RequiredIndexFamilies[i-1], family) >= 0 {
			return invalid("partition commit index families must be strictly byte-sorted")
		}
	}
	return nil
}

func encodePartitionCommitCopy(e *encoder, c PartitionCommitCopyV1) {
	e.byte(byte(c.State))
	e.bytes("transaction ID", c.TXN)
	e.u64(uint64(c.Epoch))
	e.bytes("LPART", c.LPART)
	e.u64(uint64(c.CopyGeneration))
	e.digest(c.VisibilityDigest)
	e.digest(c.LogicalDigest)
	e.digest(c.PhysicalCopyDigest)
	e.u32(uint32(len(c.RequiredIndexFamilies)))
	for _, family := range c.RequiredIndexFamilies {
		e.bytes("index family", family)
	}
}

func MarshalPartitionCommitCopyV1(c PartitionCommitCopyV1) ([]byte, error) {
	c.RequiredIndexFamilies = append([]Family(nil), c.RequiredIndexFamilies...)
	sort.Slice(c.RequiredIndexFamilies, func(i, j int) bool {
		return bytes.Compare(c.RequiredIndexFamilies[i], c.RequiredIndexFamilies[j]) < 0
	})
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindPartitionCommitCopy, VersionPartitionCommitCopyV1, MaxRootBytes, func(e *encoder) {
		encodePartitionCommitCopy(e, c)
	})
}

func UnmarshalPartitionCommitCopyV1(data []byte) (PartitionCommitCopyV1, error) {
	payload, err := verifyEnvelope(data, KindPartitionCommitCopy, VersionPartitionCommitCopyV1, MaxRootBytes)
	if err != nil {
		return PartitionCommitCopyV1{}, err
	}
	d := &decoder{data: payload}
	c := PartitionCommitCopyV1{
		State:              TxnState(d.byte("state")),
		TXN:                TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		Epoch:              Epoch(d.positive("epoch")),
		LPART:              LPART(d.bytes("LPART", MaxOpaqueIDBytes, true)),
		CopyGeneration:     Generation(d.positive("copy generation")),
		VisibilityDigest:   d.digest("visibility digest"),
		LogicalDigest:      d.digest("logical digest"),
		PhysicalCopyDigest: d.digest("physical copy digest"),
	}
	count := d.u32("index family count")
	if count > MaxIndexPins || uint64(count) > uint64(d.remaining())/4 {
		return PartitionCommitCopyV1{}, invalid("partition commit index family count exceeds its bound")
	}
	c.RequiredIndexFamilies = make([]Family, int(count))
	for i := range c.RequiredIndexFamilies {
		c.RequiredIndexFamilies[i] = Family(d.bytes("index family", MaxOpaqueIDBytes, true))
	}
	if d.err != nil {
		return PartitionCommitCopyV1{}, d.err
	}
	if err := c.Validate(); err != nil {
		return PartitionCommitCopyV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodePartitionCommitCopy(e, c) }, MaxRootBytes); err != nil {
		return PartitionCommitCopyV1{}, err
	}
	return c, nil
}

// TxnLeaseV1 is the independently renewable liveness record for a TXN owner.
// Fence changes are coupled to the matching root mutation.
type TxnLeaseV1 struct {
	Generation Generation
	Owner      OwnerID
	Fence      Fence
	LeaseUntil time.Time
}

func (l TxnLeaseV1) Validate() error {
	if err := l.Generation.Validate(); err != nil {
		return err
	}
	if err := l.Owner.Validate(); err != nil {
		return err
	}
	if err := l.Fence.Validate(); err != nil {
		return err
	}
	return validateTime("transaction lease", l.LeaseUntil, false)
}

func encodeTxnLease(e *encoder, l TxnLeaseV1) {
	e.u64(uint64(l.Generation))
	e.bytes("owner", l.Owner)
	e.u64(uint64(l.Fence))
	e.timestamp(l.LeaseUntil)
}

func MarshalTxnLeaseV1(l TxnLeaseV1) ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindTxnLease, VersionTxnLeaseV1, MaxRootBytes, func(e *encoder) {
		encodeTxnLease(e, l)
	})
}

func UnmarshalTxnLeaseV1(data []byte) (TxnLeaseV1, error) {
	payload, err := verifyEnvelope(data, KindTxnLease, VersionTxnLeaseV1, MaxRootBytes)
	if err != nil {
		return TxnLeaseV1{}, err
	}
	d := &decoder{data: payload}
	l := TxnLeaseV1{
		Generation: Generation(d.positive("generation")),
		Owner:      OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		Fence:      Fence(d.positive("fence")),
		LeaseUntil: d.timestamp("lease until"),
	}
	if d.err != nil {
		return TxnLeaseV1{}, d.err
	}
	if err := l.Validate(); err != nil {
		return TxnLeaseV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeTxnLease(e, l) }, MaxRootBytes); err != nil {
		return TxnLeaseV1{}, err
	}
	return l, nil
}
