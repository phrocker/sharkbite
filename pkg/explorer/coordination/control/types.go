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

// Package control implements snapshot leases, retention decisions, durable
// writer authority, and migration mirrors for Explorer coordination.
package control

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

const (
	MaxScan       = 10_000
	MaxRetries    = 100
	MaxLeaseTTL   = 30 * 24 * time.Hour
	MaxGrace      = 30 * 24 * time.Hour
	maxExactRead  = 8
	controlSchema = 1
)

var (
	ErrConflict       = errors.New("control: conflict")
	ErrUnavailable    = errors.New("control: unavailable")
	ErrUnknown        = errors.New("control: conditional result unknown")
	ErrCorruption     = errors.New("control: internal corruption")
	ErrLeaseActive    = errors.New("control: lease active")
	ErrExpired        = errors.New("control: expired")
	ErrStaleOwner     = errors.New("control: stale owner")
	ErrStaleAuthority = errors.New("control: stale writer authority")
	ErrStaleRetention = errors.New("control: stale retention generation")
	ErrNotFound       = errors.New("control: not found")
	ErrOverflow       = errors.New("control: overflow")
	ErrBounds         = errors.New("control: bound exceeded")
)

// Store is the exact-coordinate, prefix scan, and one-row CAS seam. Scan
// implementations must return only the newest visible version per coordinate.
type Store interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
	ScanPrefix(context.Context, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
	ScanPrefixFrom(context.Context, []byte, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
	CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
}

type PinVerifier interface {
	VerifySnapshotPins(context.Context, coordination.DomainID, coordination.SnapshotLeaseV3) error
}

type RetentionLeaseVerifier interface {
	NoPinsBelow(context.Context, coordination.DomainID, coordination.Epoch, time.Time) error
	SelectsObject(context.Context, coordination.DomainID, coordination.EntityKind, coordination.EntityID, coordination.Generation, time.Time) (bool, error)
}

type HistoryFloorVerifier interface {
	VerifyHistoryFloor(context.Context, coordination.DomainID, coordination.HistoryFloorV1, coordination.Epoch, coordination.Digest) error
}

type RetirementVerifier interface {
	VerifyRetirement(context.Context, coordination.DomainID, coordination.RetirementDecisionV1) error
}

type TrustedDeleter interface {
	DeleteRetired(context.Context, coordination.DomainID, coordination.RetirementDecisionV1) error
}

type MigrationVerifier interface {
	DrainAndVerify(context.Context, coordination.DomainID, coordination.WriterMode, coordination.Generation) error
}

type Route interface {
	Close(context.Context, coordination.DomainID) error
	Open(context.Context, coordination.DomainID, coordination.WriterMode, coordination.Generation, coordination.Fence) error
	Current(context.Context, coordination.DomainID) (coordination.WriterMode, coordination.Generation, coordination.Fence, bool, error)
}

type LeaseIDGenerator interface {
	NewLeaseID(context.Context, coordination.DomainID, coordination.OwnerID) (coordination.LeaseID, error)
}

type TermGenerator interface {
	NewAuthorityTerm(context.Context, coordination.DomainID, coordination.OwnerID) (coordination.AuthorityTerm, error)
}

type Config struct {
	Domain            coordination.DomainID
	ControlVisibility []byte
	Store             Store
	Pins              PinVerifier
	Leases            RetentionLeaseVerifier
	History           HistoryFloorVerifier
	Retirements       RetirementVerifier
	Deleter           TrustedDeleter
	Migration         MigrationVerifier
	Route             Route
	LeaseIDs          LeaseIDGenerator
	Terms             TermGenerator
	EmbeddedBackend   coordination.BackendID
	AccumuloBackend   coordination.BackendID
	Clock             func() time.Time
	MaxRetries        int
	RetryBackoff      time.Duration
	MaxScan           int
	MaxLeaseTTL       time.Duration
	RetirementGrace   time.Duration
}

type Lease struct {
	Record           coordination.SnapshotLeaseV3
	RecordGeneration coordination.Generation
	UpdatedAt        time.Time
}

type CreateLeaseRequest struct {
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	Frontier            coordination.Epoch
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	PolicyGeneration    coordination.Generation
	PolicyCopyPinDigest coordination.Digest
	PolicyCopyPins      []coordination.PolicyCopyPin
	IndexPins           []coordination.IndexPin
	Now                 time.Time
	ExpiresAt           time.Time
}

type RenewLeaseRequest struct {
	LeaseID             coordination.LeaseID
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	RecordGeneration    coordination.Generation
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	Now                 time.Time
	ExpiresAt           time.Time
}

type TakeoverLeaseRequest struct {
	LeaseID             coordination.LeaseID
	PreviousGeneration  coordination.Generation
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	Now                 time.Time
	ExpiresAt           time.Time
}

type ReleaseLeaseRequest struct {
	LeaseID          coordination.LeaseID
	Owner            coordination.OwnerID
	Fence            coordination.Fence
	RecordGeneration coordination.Generation
	Now              time.Time
}

type LeaseCursor struct {
	Band byte
	Row  []byte
}

type Retirement struct {
	Decision            coordination.RetirementDecisionV1
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	RetentionGeneration coordination.Generation
	RecordGeneration    coordination.Generation
	UpdatedAt           time.Time
}

type RetirementRequest struct {
	Decision            coordination.RetirementDecisionV1
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	RetentionGeneration coordination.Generation
	Now                 time.Time
}

type RetirementTransition struct {
	Kind                coordination.EntityKind
	ID                  coordination.EntityID
	Owner               coordination.OwnerID
	Fence               coordination.Fence
	RecordGeneration    coordination.Generation
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
	Now                 time.Time
}

type Authority struct {
	Record           coordination.WriterAuthorityV1
	Mode             coordination.WriterMode
	RecordGeneration coordination.Generation
	UpdatedAt        time.Time
}

type AuthorityRequest struct {
	Owner      coordination.OwnerID
	Mode       coordination.WriterMode
	LeaseUntil time.Time
	Now        time.Time
	Term       coordination.AuthorityTerm
}

type AuthorityTransition struct {
	Owner          coordination.OwnerID
	Term           coordination.AuthorityTerm
	Generation     coordination.Generation
	Fence          coordination.Fence
	Mode           coordination.WriterMode
	LeaseUntil     time.Time
	Now            time.Time
	ExpectedHead   coordination.AllocatorHeadV1
	AllowUnexpired bool
	TerminalState  coordination.AuthorityState
}

type Observation struct {
	Record           coordination.BackendObservationV1
	Mode             coordination.WriterMode
	RecordGeneration coordination.Generation
}

type RoutingDecision struct {
	Authority Authority
	Head      coordination.AllocatorHeadV1
	Embedded  Observation
	Accumulo  Observation
	Enabled   bool
}

// AuthoritySource is directly usable by catalog without importing catalog.
type AuthoritySource struct{ Client *Client }

type CurrentAuthority struct {
	Generation          coordination.Generation
	Fence               coordination.Fence
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
}

func (s AuthoritySource) Current(ctx context.Context, domain coordination.DomainID) (CurrentAuthority, error) {
	if s.Client == nil || string(domain) != string(s.Client.domain) {
		return CurrentAuthority{}, ErrUnavailable
	}

	decision, err := s.Client.RoutingBarrier(ctx, s.Client.now())
	if err != nil || !decision.Enabled {
		if err == nil {
			err = ErrUnavailable
		}
		return CurrentAuthority{}, err
	}
	return CurrentAuthority{
		Generation: decision.Authority.Record.Generation, Fence: decision.Authority.Record.Fence,
		RetentionGeneration: decision.Head.RetentionGeneration, HistoryFloor: decision.Head.HistoryFloor,
	}, nil
}

// AllocatorAuthority returns write authority only when the durable record,
// allocator head, both backend mirrors, and configured route agree.
func (s AuthoritySource) AllocatorAuthority(ctx context.Context, domain coordination.DomainID) (allocator.Authority, error) {
	if s.Client == nil || !s.Client.MatchesDomain(domain) {
		return allocator.Authority{}, ErrUnavailable
	}
	decision, err := s.Client.RoutingBarrier(ctx, s.Client.now())
	if err != nil || !decision.Enabled {
		if err == nil {
			err = ErrUnavailable
		}
		return allocator.Authority{}, err
	}
	return allocator.Authority{
		Generation: decision.Authority.Record.Generation,
		Mode:       decision.Authority.Mode,
		Holder:     append(coordination.OwnerID(nil), decision.Authority.Record.Owner...),
		Fence:      decision.Authority.Record.Fence,
	}, nil
}

type LeaseSource struct{ Client *Client }

func (s LeaseSource) SelectsPolicyCopy(ctx context.Context, domain coordination.DomainID, pin coordination.PolicyCopyPin) (bool, error) {
	if s.Client == nil || string(domain) != string(s.Client.domain) {
		return false, ErrUnavailable
	}
	if err := pin.Validate(); err != nil {
		return false, err
	}
	return s.Client.anyLease(ctx, s.Client.now(), func(l coordination.SnapshotLeaseV3) bool {
		index := sort.Search(len(l.PolicyCopyPins), func(index int) bool {
			return coordination.ComparePolicyCopyPins(l.PolicyCopyPins[index], pin) >= 0
		})
		return index < len(l.PolicyCopyPins) &&
			coordination.ComparePolicyCopyPins(l.PolicyCopyPins[index], pin) == 0
	})
}

func (s LeaseSource) SelectsIndexGeneration(ctx context.Context, domain coordination.DomainID, family coordination.Family, igen coordination.IGEN) (bool, error) {
	if s.Client == nil || string(domain) != string(s.Client.domain) {
		return false, ErrUnavailable
	}
	pin := coordination.IndexPin{Family: family, IGEN: igen}
	return s.Client.anyLease(ctx, s.Client.now(), func(l coordination.SnapshotLeaseV3) bool {
		index := sort.Search(len(l.IndexPins), func(index int) bool {
			return coordination.CompareIndexPins(l.IndexPins[index], pin) >= 0
		})
		return index < len(l.IndexPins) && coordination.CompareIndexPins(l.IndexPins[index], pin) == 0
	})
}
