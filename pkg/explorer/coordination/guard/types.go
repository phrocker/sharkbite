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

// Package guard implements row-atomic Explorer entity guards.
package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

const MaxEntities = 4096

var (
	ErrConflict       = errors.New("entity guard: conflict")
	ErrBusy           = errors.New("entity guard: busy")
	ErrUnavailable    = errors.New("entity guard: unavailable")
	ErrUnknown        = errors.New("entity guard: conditional result unknown")
	ErrCorruption     = errors.New("entity guard: internal corruption")
	ErrExpired        = errors.New("entity guard: expired")
	ErrStaleAuthority = errors.New("entity guard: stale writer authority")
	ErrStaleRetention = errors.New("entity guard: stale retention state")
	ErrNotFound       = errors.New("entity guard: not found")
	ErrOverflow       = errors.New("entity guard: overflow")
	ErrBounds         = errors.New("entity guard: bound exceeded")

	// ErrForeignHead is the conflict decide returns when the entity's head
	// was written by another store, which its logical policy names (see
	// ownedBy). Unlike every other ErrConflict, it is not a race: the head
	// stays the other store's, so retrying the same intent can never
	// succeed, and a caller that reports outcomes may report it as a
	// definite refusal. It wraps ErrConflict, so errors.Is(err, ErrConflict)
	// holds for it and every handler that keys on the conflict is unchanged.
	ErrForeignHead = fmt.Errorf("%w: the entity head belongs to another store", ErrConflict)
)

type Mode uint8

const (
	ModeAppend Mode = iota + 1
	ModeAbsentOrIdentical
	ModeMutate
	ModeRetire
)

func (m Mode) valid() bool {
	return m >= ModeAppend && m <= ModeRetire
}

type Decision uint8

const (
	DecisionAppend Decision = iota + 1
	DecisionCreate
	DecisionReuse
	DecisionMutate
	DecisionRetire
)

type EntityState uint8

const (
	StateLive EntityState = iota + 1
	StateTombstone
)

type Entity struct {
	Kind byte
	ID   coordination.EntityID
}

func (e Entity) validate() error {
	if e.Kind == 0 {
		return errors.New("entity guard: entity kind is required")
	}
	return e.ID.Validate()
}

type Head struct {
	Generation           coordination.Generation
	UpdatedAt            time.Time
	State                EntityState
	WinnerID             []byte
	Epoch                coordination.Epoch
	TXN                  coordination.TXN
	LogicalDigest        coordination.Digest
	LPART                coordination.LPART
	LogicalPolicyID      []byte
	RetirementGeneration coordination.Generation
}

// EntityHeadV1 is the bounded, versioned committed entity-head record.
type EntityHeadV1 = Head

func (h Head) Validate() error {
	if err := h.Generation.Validate(); err != nil {
		return err
	}
	if err := utc("head update", h.UpdatedAt); err != nil {
		return err
	}
	if h.State != StateLive && h.State != StateTombstone {
		return errors.New("entity guard: head state is unknown")
	}
	if len(h.WinnerID) == 0 || len(h.WinnerID) > coordination.MaxOpaqueIDBytes {
		return errors.New("entity guard: winner identity is outside its bound")
	}
	if err := h.Epoch.Validate(); err != nil {
		return err
	}
	if err := h.TXN.Validate(); err != nil {
		return err
	}
	if err := h.LogicalDigest.Validate("head logical digest"); err != nil {
		return err
	}
	if err := h.LPART.Validate(); err != nil {
		return err
	}
	if len(h.LogicalPolicyID) == 0 || len(h.LogicalPolicyID) > coordination.MaxOpaqueIDBytes {
		return errors.New("entity guard: logical policy identity is outside its bound")
	}
	return h.RetirementGeneration.Validate()
}

type Intent struct {
	Entity               Entity
	TXN                  coordination.TXN
	Owner                coordination.OwnerID
	LeaseUntil           time.Time
	Fence                coordination.Fence
	AuthorityGeneration  coordination.Generation
	AuthorityFence       coordination.Fence
	RetentionGeneration  coordination.Generation
	RetirementGeneration coordination.Generation
	HistoryFloor         coordination.Epoch
	Mode                 Mode
	ExpectedEpoch        coordination.Epoch
	ExpectedDigest       coordination.Digest
	DesiredState         EntityState
	DesiredWinnerID      []byte
	DesiredDigest        coordination.Digest
	LPART                coordination.LPART
	LogicalPolicyID      []byte
	ManifestChunk        uint32
	ManifestEntry        uint32
	Ordinal              uint32
	PhysicalDigest       coordination.Digest
}

func (i Intent) Validate() error {
	if err := i.Entity.validate(); err != nil {
		return err
	}
	if err := i.TXN.Validate(); err != nil {
		return err
	}
	if err := i.Owner.Validate(); err != nil {
		return err
	}
	if err := utc("guard lease", i.LeaseUntil); err != nil {
		return err
	}
	if err := i.Fence.Validate(); err != nil {
		return err
	}
	if err := i.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := i.AuthorityFence.Validate(); err != nil {
		return err
	}
	if err := i.RetentionGeneration.Validate(); err != nil {
		return err
	}
	if err := i.RetirementGeneration.Validate(); err != nil {
		return err
	}
	if err := i.HistoryFloor.Validate(); err != nil {
		return err
	}
	if !i.Mode.valid() {
		return errors.New("entity guard: acquisition mode is unknown")
	}
	if i.ExpectedEpoch != 0 {
		if err := i.ExpectedEpoch.Validate(); err != nil {
			return err
		}
		if err := i.ExpectedDigest.Validate("expected logical digest"); err != nil {
			return err
		}
	} else if i.ExpectedDigest != (coordination.Digest{}) {
		return errors.New("entity guard: absent expected epoch has a digest")
	}
	if i.DesiredState != StateLive && i.DesiredState != StateTombstone {
		return errors.New("entity guard: desired state is unknown")
	}
	if len(i.DesiredWinnerID) == 0 || len(i.DesiredWinnerID) > coordination.MaxOpaqueIDBytes {
		return errors.New("entity guard: desired winner identity is outside its bound")
	}
	if err := i.DesiredDigest.Validate("desired logical digest"); err != nil {
		return err
	}
	if err := i.LPART.Validate(); err != nil {
		return err
	}
	if len(i.LogicalPolicyID) == 0 || len(i.LogicalPolicyID) > coordination.MaxOpaqueIDBytes {
		return errors.New("entity guard: logical policy identity is outside its bound")
	}
	if i.ManifestEntry >= coordination.MaxChunkEntries {
		return errors.Join(ErrBounds, errors.New("entity guard: manifest entry exceeds its bound"))
	}
	return i.PhysicalDigest.Validate("pending physical digest")
}

type Pending struct {
	Generation coordination.Generation
	UpdatedAt  time.Time
	Active     bool
	Prepared   bool
	Decision   Decision
	Intent     Intent
}

// PendingGuardV1 is the bounded, versioned active or released guard record.
type PendingGuardV1 = Pending

func (p Pending) Validate() error {
	if err := p.Generation.Validate(); err != nil {
		return err
	}
	if err := utc("pending update", p.UpdatedAt); err != nil {
		return err
	}
	if !p.Active {
		if p.Prepared || p.Decision != 0 || !zeroIntent(p.Intent) {
			return errors.New("entity guard: released pending marker carries intent")
		}
		return nil
	}
	if p.Decision < DecisionAppend || p.Decision > DecisionRetire {
		return errors.New("entity guard: pending decision is unknown")
	}
	if err := p.Intent.Validate(); err != nil {
		return err
	}
	coreGuard := coordination.EntityGuardV1{
		EntityKind: coordination.EntityKind{p.Intent.Entity.Kind},
		EntityID:   p.Intent.Entity.ID, TXN: p.Intent.TXN, Owner: p.Intent.Owner,
		LeaseUntil: p.Intent.LeaseUntil, Fence: p.Intent.Fence,
		AuthorityGeneration: p.Intent.AuthorityGeneration,
		DesiredDigest:       p.Intent.DesiredDigest, PreviousDigest: p.Intent.ExpectedDigest,
		PreviousVersion: p.Intent.ExpectedEpoch, State: coordination.GuardStateHeld,
	}
	if err := coreGuard.Validate(); err != nil {
		return err
	}
	corePending := coordination.PendingMutationV1{
		EntityKind: coordination.EntityKind{p.Intent.Entity.Kind},
		EntityID:   p.Intent.Entity.ID, TXN: p.Intent.TXN,
		ManifestChunk: p.Intent.ManifestChunk, ManifestEntry: p.Intent.ManifestEntry,
		Ordinal: p.Intent.Ordinal, LogicalDigest: p.Intent.DesiredDigest,
		PhysicalDigest: p.Intent.PhysicalDigest,
	}
	return corePending.Validate()
}

type Authority struct {
	Generation          coordination.Generation
	Fence               coordination.Fence
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
}

type AuthoritySource interface {
	Current(context.Context, coordination.DomainID) (Authority, error)
}

type RetirementSource interface {
	Retired(context.Context, coordination.DomainID, Entity) (bool, coordination.Generation, error)
}

type TxnDisposition uint8

const (
	TxnNonterminal TxnDisposition = iota + 1
	TxnCommitted
	TxnTerminal
)

type TxnStatusSource interface {
	Status(context.Context, coordination.DomainID, coordination.TXN) (TxnDisposition, error)
}

type Published struct {
	TXN                 coordination.TXN
	Epoch               coordination.Epoch
	Fence               coordination.Fence
	AuthorityGeneration coordination.Generation
	LogicalDigest       coordination.Digest
	LPART               coordination.LPART
	LogicalPolicyID     []byte
	State               EntityState
	WinnerID            []byte
}

type Reconciler interface {
	ReconcileCommitted(context.Context, coordination.DomainID, Entity, Pending) error
}

type Config struct {
	Domain            coordination.DomainID
	ControlVisibility []byte
	Store             Store
	Authority         AuthoritySource
	Retirement        RetirementSource
	Transactions      TxnStatusSource
	Reconciler        Reconciler
	Clock             func() time.Time
	MaxRetries        int
	RetryBackoff      time.Duration
}

type Store interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
	CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
}

type Acquisition struct {
	Entity   Entity
	Decision Decision
	Pending  Pending
	Head     *Head
}

func utc(name string, value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC || value.Year() < 1 || value.Year() > 9999 {
		return fmt.Errorf("entity guard: %s must be a supported UTC timestamp", name)
	}
	return nil
}

func nextGeneration(values ...coordination.Generation) (coordination.Generation, error) {
	var maximum coordination.Generation
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	if maximum == coordination.Generation(math.MaxInt64) {
		return 0, ErrOverflow
	}
	return maximum + 1, nil
}

func zeroIntent(value Intent) bool {
	return value.Entity.Kind == 0 && len(value.Entity.ID) == 0 && len(value.TXN) == 0 &&
		len(value.Owner) == 0 && value.LeaseUntil.IsZero() && value.Fence == 0 &&
		value.AuthorityGeneration == 0 && value.RetentionGeneration == 0 &&
		value.AuthorityFence == 0 && value.RetirementGeneration == 0 &&
		value.HistoryFloor == 0 && value.Mode == 0 && value.ExpectedEpoch == 0 &&
		value.ExpectedDigest == (coordination.Digest{}) && value.DesiredState == 0 &&
		len(value.DesiredWinnerID) == 0 && value.DesiredDigest == (coordination.Digest{}) &&
		len(value.LPART) == 0 && len(value.LogicalPolicyID) == 0 &&
		value.ManifestChunk == 0 && value.ManifestEntry == 0 && value.Ordinal == 0 &&
		value.PhysicalDigest == (coordination.Digest{})
}

func sameIntent(a, b Intent) bool {
	return a.Entity.Kind == b.Entity.Kind && bytes.Equal(a.Entity.ID, b.Entity.ID) &&
		bytes.Equal(a.TXN, b.TXN) && bytes.Equal(a.Owner, b.Owner) &&
		a.LeaseUntil.Equal(b.LeaseUntil) && a.Fence == b.Fence &&
		a.AuthorityGeneration == b.AuthorityGeneration &&
		a.AuthorityFence == b.AuthorityFence &&
		a.RetentionGeneration == b.RetentionGeneration &&
		a.RetirementGeneration == b.RetirementGeneration && a.HistoryFloor == b.HistoryFloor &&
		a.Mode == b.Mode && a.ExpectedEpoch == b.ExpectedEpoch &&
		a.ExpectedDigest == b.ExpectedDigest && a.DesiredState == b.DesiredState &&
		bytes.Equal(a.DesiredWinnerID, b.DesiredWinnerID) && a.DesiredDigest == b.DesiredDigest &&
		bytes.Equal(a.LPART, b.LPART) && bytes.Equal(a.LogicalPolicyID, b.LogicalPolicyID) &&
		a.ManifestChunk == b.ManifestChunk && a.ManifestEntry == b.ManifestEntry &&
		a.Ordinal == b.Ordinal && a.PhysicalDigest == b.PhysicalDigest
}
