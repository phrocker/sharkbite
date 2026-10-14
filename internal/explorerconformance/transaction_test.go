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
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

type mappingSink struct{ cells []transaction.TrustedCell }

func (s *mappingSink) WriteTrusted(_ context.Context, cells []transaction.TrustedCell) error {
	s.cells = append([]transaction.TrustedCell(nil), cells...)
	return nil
}

func (s *mappingSink) ReadTrusted(_ context.Context, _ []transaction.TrustedCell) ([]transaction.TrustedCell, error) {
	result := append([]transaction.TrustedCell(nil), s.cells...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, nil
}

func TestMemoryAtomicStoreConformance(t *testing.T) {
	RunAtomicStoreSuite(t, transaction.NewMemoryStore())
}

func TestAccumuloPhysicalMappingConformance(t *testing.T) {
	sink := &mappingSink{}
	adapter, err := transaction.NewAccumuloPhysicalAdapter(sink)
	if err != nil {
		t.Fatal(err)
	}
	RunPhysicalMappingSuite(t, adapter, func() []transaction.TrustedCell { return sink.cells })
}
