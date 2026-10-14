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
	"errors"
	"sync"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

// CASFault selects the behaviour a FaultStore imposes on a single
// CompareAndMutate call. It is the mechanism for exercising the hardest part of
// the Store contract: the indeterminate outcome in which a mutation MAY have
// applied and the store is obligated to report ErrConditionalUnknown.
type CASFault uint8

const (
	// CASPassthrough forwards the call to the wrapped store unchanged.
	CASPassthrough CASFault = iota
	// CASUnknownAfterApply applies the mutation to the wrapped store and then
	// reports ErrConditionalUnknown. This models the case where the mutation
	// DID apply but the caller cannot observe that fact from the response.
	CASUnknownAfterApply
	// CASUnknownWithoutApply reports ErrConditionalUnknown without touching the
	// wrapped store. This models the case where the mutation did NOT apply yet
	// the caller still cannot rule out that it did.
	CASUnknownWithoutApply
	// CASUnavailable returns a definite transport error without applying,
	// modelling a partition or unavailable backend. The status is Unknown,
	// which callers must treat as "may or may not have applied".
	CASUnavailable
)

// ErrFaultUnavailable is the transport error injected by CASUnavailable and by
// read/scan faults. It is intentionally NOT ErrConditionalUnknown so that the
// protocol under test classifies it as unavailability rather than an
// indeterminate conditional result.
var ErrFaultUnavailable = errors.New("faultstore: injected unavailability")

// FaultStore wraps any allocator.Store and deterministically injects faults at
// chosen call ordinals. It is implementation-agnostic: it works over the
// exported MemoryStore or over any future real backend, which is the whole
// point of a reusable conformance harness.
//
// Faults are consumed in FIFO order, one per CompareAndMutate call. When the
// queue is empty calls pass through untouched, so faults injected after setup
// begin counting from the first subsequent CompareAndMutate.
type FaultStore struct {
	mu        sync.Mutex
	inner     allocator.Store
	casFaults []CASFault
	readFail  []bool
	scanFail  []bool

	casCalls      int
	appliedUnknwn int // CASUnknownAfterApply emissions where inner accepted
	unappliedUnkn int // CASUnknownWithoutApply emissions
	unavailable   int // CASUnavailable emissions
	readInjected  int
	scanInjected  int
}

// NewFaultStore wraps inner. inner must be non-nil.
func NewFaultStore(inner allocator.Store) *FaultStore {
	return &FaultStore{inner: inner}
}

// InjectCAS enqueues per-call CompareAndMutate faults, consumed in order.
func (s *FaultStore) InjectCAS(faults ...CASFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.casFaults = append(s.casFaults, faults...)
}

// InjectReadFailures enqueues per-call ReadExact transport failures.
func (s *FaultStore) InjectReadFailures(fail ...bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readFail = append(s.readFail, fail...)
}

// InjectScanFailures enqueues per-call ScanRowPrefix transport failures.
func (s *FaultStore) InjectScanFailures(fail ...bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanFail = append(s.scanFail, fail...)
}

// Clear removes all queued faults without resetting statistics.
func (s *FaultStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.casFaults = nil
	s.readFail = nil
	s.scanFail = nil
}

// EmittedFaults reports how many injected faults actually fired, broken down by
// kind. Callers use this to assert that a fault they scheduled was reached.
func (s *FaultStore) EmittedFaults() (appliedUnknown, unappliedUnknown, unavailable, reads, scans int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appliedUnknwn, s.unappliedUnkn, s.unavailable, s.readInjected, s.scanInjected
}

// TotalUnknown reports the number of ErrConditionalUnknown results emitted.
func (s *FaultStore) TotalUnknown() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appliedUnknwn + s.unappliedUnkn
}

func (s *FaultStore) nextCASFault() CASFault {
	if len(s.casFaults) == 0 {
		return CASPassthrough
	}
	fault := s.casFaults[0]
	s.casFaults = s.casFaults[1:]
	return fault
}

func (s *FaultStore) nextReadFail() bool {
	if len(s.readFail) == 0 {
		return false
	}
	fail := s.readFail[0]
	s.readFail = s.readFail[1:]
	return fail
}

func (s *FaultStore) nextScanFail() bool {
	if len(s.scanFail) == 0 {
		return false
	}
	fail := s.scanFail[0]
	s.scanFail = s.scanFail[1:]
	return fail
}

func (s *FaultStore) ReadExact(ctx context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	s.mu.Lock()
	fail := s.nextReadFail()
	if fail {
		s.readInjected++
	}
	s.mu.Unlock()
	if fail {
		return nil, ErrFaultUnavailable
	}
	return s.inner.ReadExact(ctx, coords)
}

func (s *FaultStore) ScanRowPrefix(
	ctx context.Context,
	row, family, qualifierStart, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	s.mu.Lock()
	fail := s.nextScanFail()
	if fail {
		s.scanInjected++
	}
	s.mu.Unlock()
	if fail {
		return nil, ErrFaultUnavailable
	}
	return s.inner.ScanRowPrefix(ctx, row, family, qualifierStart, visibility, limit)
}

func (s *FaultStore) CompareAndMutate(ctx context.Context, mutation allocator.Mutation) (allocator.Status, error) {
	s.mu.Lock()
	s.casCalls++
	fault := s.nextCASFault()
	s.mu.Unlock()

	switch fault {
	case CASUnknownWithoutApply:
		s.mu.Lock()
		s.unappliedUnkn++
		s.mu.Unlock()
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	case CASUnavailable:
		s.mu.Lock()
		s.unavailable++
		s.mu.Unlock()
		return allocator.StatusUnknown, ErrFaultUnavailable
	case CASUnknownAfterApply:
		status, err := s.inner.CompareAndMutate(ctx, mutation)
		if err != nil && !errors.Is(err, allocator.ErrConditionalUnknown) {
			// The inner store genuinely failed; surface that faithfully rather
			// than masking it as an indeterminate-but-applied outcome.
			return status, err
		}
		if status == allocator.StatusAccepted {
			s.mu.Lock()
			s.appliedUnknwn++
			s.mu.Unlock()
			return allocator.StatusUnknown, allocator.ErrConditionalUnknown
		}
		// The mutation was legitimately rejected (a condition failed); a
		// rejection is definite, so report it truthfully.
		return status, err
	default:
		return s.inner.CompareAndMutate(ctx, mutation)
	}
}

var _ allocator.Store = (*FaultStore)(nil)
