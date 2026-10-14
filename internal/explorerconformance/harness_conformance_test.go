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

package explorerconformance

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func memoryStoreFactory() StoreFactory {
	return func() allocator.Store { return transaction.NewMemoryStore() }
}

// faultStoreFactory wraps a MemoryStore in a fault-free FaultStore, proving the
// wrapper is a faithful passthrough when no faults are scheduled.
func faultStoreFactory() StoreFactory {
	return func() allocator.Store { return NewFaultStore(transaction.NewMemoryStore()) }
}

func TestMemoryStoreContract(t *testing.T) {
	RunStoreContractSuite(t, memoryStoreFactory())
}

func TestFaultStorePassthroughContract(t *testing.T) {
	RunStoreContractSuite(t, faultStoreFactory())
}

func TestMemoryStoreConcurrency(t *testing.T) {
	RunStoreConcurrencySuite(t, memoryStoreFactory())
}

func TestFaultStorePassthroughConcurrency(t *testing.T) {
	RunStoreConcurrencySuite(t, faultStoreFactory())
}

func TestMemoryStoreIndeterminateCAS(t *testing.T) {
	RunIndeterminateCASSuite(t)
}

func TestMemoryStorePartition(t *testing.T) {
	RunPartitionSuite(t)
}

func TestMemoryStoreClockSkew(t *testing.T) {
	RunClockSkewSuite(t)
}

func TestMemoryStoreRootLeaseDisagreement(t *testing.T) {
	RunRootLeaseDisagreementSuite(t)
}
