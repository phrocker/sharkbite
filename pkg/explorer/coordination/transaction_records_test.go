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

package coordination

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPartitionCommitCopyAndTxnLeaseCodec(t *testing.T) {
	copyValue := PartitionCommitCopyV1{
		State: StateCommitted, TXN: TXN("txn"), Epoch: 7, LPART: LPART("part"),
		CopyGeneration: 3, VisibilityDigest: Sum([]byte("A")),
		LogicalDigest: Sum([]byte("logical")), PhysicalCopyDigest: Sum([]byte("physical")),
		RequiredIndexFamilies: []Family{Family("graph"), Family("lexical")},
	}
	first, err := MarshalPartitionCommitCopyV1(copyValue)
	if err != nil {
		t.Fatal(err)
	}
	second, err := MarshalPartitionCommitCopyV1(copyValue)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("non-deterministic commit copy: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "partition_commit_copy_v1.bin"))
	if err != nil || !bytes.Equal(first, golden) {
		t.Fatalf("commit-copy golden mismatch: %v", err)
	}
	decoded, err := UnmarshalPartitionCommitCopyV1(first)
	if err != nil || decoded.Epoch != 7 || len(decoded.RequiredIndexFamilies) != 2 {
		t.Fatalf("commit copy round trip = %#v, %v", decoded, err)
	}
	corrupt := append([]byte(nil), first...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := UnmarshalPartitionCommitCopyV1(corrupt); err == nil {
		t.Fatal("corrupt commit copy accepted")
	}
	copyValue.RequiredIndexFamilies = make([]Family, MaxIndexPins+1)
	for i := range copyValue.RequiredIndexFamilies {
		copyValue.RequiredIndexFamilies[i] = Family{byte(i + 1)}
	}
	if _, err := MarshalPartitionCommitCopyV1(copyValue); err == nil {
		t.Fatal("oversized index family set accepted")
	}

	lease := TxnLeaseV1{
		Generation: 2, Owner: OwnerID("worker"), Fence: 4,
		LeaseUntil: time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC),
	}
	data, err := MarshalTxnLeaseV1(lease)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalTxnLeaseV1(data)
	if err != nil || got.Generation != lease.Generation || !got.LeaseUntil.Equal(lease.LeaseUntil) {
		t.Fatalf("lease round trip = %#v, %v", got, err)
	}
	golden, err = os.ReadFile(filepath.Join("testdata", "txn_lease_v1.bin"))
	if err != nil || !bytes.Equal(data, golden) {
		t.Fatalf("lease golden mismatch: %v", err)
	}
}

func TestPartitionCommitCopyCanonicalFamilyOrder(t *testing.T) {
	value := PartitionCommitCopyV1{
		State: StateCommitted, TXN: TXN("txn"), Epoch: 1, LPART: LPART("part"),
		CopyGeneration: 1, VisibilityDigest: Sum([]byte("A")),
		LogicalDigest: Sum([]byte("logical")), PhysicalCopyDigest: Sum([]byte("physical")),
		RequiredIndexFamilies: []Family{Family("z"), Family("a")},
	}
	data, err := MarshalPartitionCommitCopyV1(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalPartitionCommitCopyV1(data)
	if err != nil || string(got.RequiredIndexFamilies[0]) != "a" {
		t.Fatalf("canonical order = %#v, %v", got.RequiredIndexFamilies, err)
	}
}
