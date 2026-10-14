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

package authorized

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// AccumulatorRecord holds one identity's state within a fixed window. State's
// zero value must represent an unencumbered identity. This is an in-memory
// contract; ledger adapters retain ownership of their persisted format.
type AccumulatorRecord[T any] struct {
	WindowStart time.Time
	State       T
}

// AccumulatorLedger persists windowed state through the policy store. Load
// must return independently owned state; Store must retain its own copy and
// atomically replace a record. Implementations supply durability, just as
// CoOccurrenceLedger does.
type AccumulatorLedger[T any] interface {
	Load(context.Context, string) (AccumulatorRecord[T], bool, error)
	Store(context.Context, string, AccumulatorRecord[T]) error
}

// Accumulator serializes windowed, per-identity read/modify/write operations.
// The caller supplies the meaning of state and its bound. All updates sharing
// a ledger namespace must use the same instance for serialization; independent
// instances or processes require coordination by the caller, as with MosaicBudget.
// An Accumulator must be constructed with NewAccumulator and must not be copied.
type Accumulator[T any] struct {
	mu        sync.Mutex
	ledger    AccumulatorLedger[T]
	window    time.Duration
	namespace string
}

// NewAccumulator fails closed without a ledger, positive window, or identity
// namespace. Use a stable, distinct namespace for each control sharing a ledger.
// Construction enables the accumulator; callers implement optional disabling
// by bypassing construction and Update entirely.
func NewAccumulator[T any](ledger AccumulatorLedger[T], window time.Duration, namespace string) (*Accumulator[T], error) {
	if window <= 0 {
		return nil, dependencyRequired("accumulator window")
	}
	if isNilDependency(ledger) {
		return nil, dependencyRequired("accumulator ledger")
	}
	if namespace == "" {
		return nil, dependencyRequired("accumulator identity namespace")
	}
	return &Accumulator[T]{ledger: ledger, window: window, namespace: namespace}, nil
}

// Update loads the identity's state, starts a fresh window for missing, expired,
// or future-dated records, and persists the result of contribute. The callback
// runs under the accumulator lock and must not reenter Update. A callback error
// prevents Store; any error returns zero state. Callers must not disclose a
// callback's provisional result before Update successfully persists it.
func (a *Accumulator[T]) Update(ctx context.Context, decision auth.Decision, now time.Time, contribute func(T) (T, error)) (T, error) {
	var zero T
	if a == nil || isNilDependency(a.ledger) || a.window <= 0 || a.namespace == "" || contribute == nil {
		return zero, dependencyRequired("initialized accumulator and contribution")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	key := accumulatorIdentityKey(a.namespace, decision)
	record, found, err := a.ledger.Load(ctx, key)
	if err != nil {
		return zero, err
	}
	if !found || now.Before(record.WindowStart) || now.Sub(record.WindowStart) >= a.window {
		record = AccumulatorRecord[T]{WindowStart: now}
	}
	state, err := contribute(record.State)
	if err != nil {
		return zero, err
	}
	record.State = state
	if err := a.ledger.Store(ctx, key, record); err != nil {
		return zero, err
	}
	return state, nil
}

// accumulatorIdentityKey keeps raw subjects and authorization domains out of
// the ledger. Length framing prevents ambiguous component concatenations.
func accumulatorIdentityKey(namespace string, decision auth.Decision) string {
	var buf bytes.Buffer
	writeComponent := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = buf.Write(length[:])
		_, _ = buf.Write(value)
	}
	writeComponent(decision.AuthorizationDomain())
	writeComponent([]byte(decision.Subject()))
	return auth.DigestBytes(
		namespace, buf.Bytes()).String()
}
