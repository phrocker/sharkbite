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
 */

// Package catalog implements the durable policy-copy and index-generation
// catalogs used by Explorer coordinators.
package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

const (
	MaxPolicyChunks     = 4096
	MaxCatalogScan      = 10_000
	MaxConditionalRetry = 100
)

var (
	ErrConflict       = errors.New("catalog: conflict")
	ErrBusy           = errors.New("catalog: busy")
	ErrUnavailable    = errors.New("catalog: unavailable")
	ErrUnknown        = errors.New("catalog: conditional result unknown")
	ErrCorruption     = errors.New("catalog: internal corruption")
	ErrExpired        = errors.New("catalog: expired")
	ErrStaleOwner     = errors.New("catalog: stale owner")
	ErrStaleAuthority = errors.New("catalog: stale writer authority")
	ErrStaleRetention = errors.New("catalog: stale retention state")
	ErrNotFound       = errors.New("catalog: not found")
	ErrBounds         = errors.New("catalog: bound exceeded")
	ErrOverflow       = errors.New("catalog: overflow")
	ErrLeaseActive    = errors.New("catalog: snapshot lease still selects generation")
)

type Store interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
	ScanPrefix(context.Context, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
	ScanPrefixFrom(context.Context, []byte, []byte, []byte, []byte, []byte, int) ([]allocator.Cell, error)
	CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
}

type Authority struct {
	Generation          coordination.Generation
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
}

type AuthoritySource interface {
	Current(context.Context, coordination.DomainID) (Authority, error)
}

type OperationDisposition uint8

const (
	OperationNonterminal OperationDisposition = iota + 1
	OperationCommitted
	OperationTerminal
)

type OperationSource interface {
	Status(context.Context, coordination.DomainID, []byte) (OperationDisposition, error)
}

type LeaseSource interface {
	SelectsPolicyCopy(context.Context, coordination.DomainID, coordination.PolicyCopyPin) (bool, error)
	SelectsIndexGeneration(context.Context, coordination.DomainID, coordination.Family, coordination.IGEN) (bool, error)
}

type PolicyCopyProof struct {
	Domain              coordination.DomainID
	LPART               coordination.LPART
	CopyGeneration      coordination.Generation
	VisibilityDigest    coordination.Digest
	ManifestDigest      coordination.Digest
	LogicalDigest       coordination.Digest
	PhysicalDigest      coordination.Digest
	RowCount            uint64
	Owner               coordination.OwnerID
	OperationID         []byte
	Fence               coordination.Fence
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	HistoryFloor        coordination.Epoch
}

type PolicyVerifier interface {
	VerifyCopy(context.Context, PolicyCopyProof) error
	VerifyMapping(context.Context, coordination.DomainID, coordination.PolicyCopyMapV3) error
	AllowPolicyRetirement(context.Context, PolicyCopyProof, coordination.Epoch) error
}

type CommittedOutcome struct {
	Epoch coordination.Epoch
	TXN   coordination.TXN
}

type IndexVerifier interface {
	VerifyDelta(context.Context, coordination.DomainID, coordination.IndexDeltaV1, Authority) error
	CommittedOutcomes(context.Context, coordination.DomainID, coordination.Epoch, coordination.Epoch, int) ([]CommittedOutcome, error)
	VerifyBase(context.Context, coordination.DomainID, coordination.IndexGenerationV2, coordination.Epoch) error
	VerifySealing(context.Context, coordination.DomainID, coordination.IndexGenerationV2, []coordination.IndexDeltaV1) error
	VerifyActivation(context.Context, coordination.DomainID, coordination.IndexActivationV2, coordination.IndexGenerationV2) error
	VerifyLookup(context.Context, coordination.DomainID, coordination.IndexActivationV2, coordination.IndexGenerationV2) error
	AllowIndexRetirement(context.Context, coordination.DomainID, coordination.IndexGenerationV2) error
}

type Config struct {
	Domain            coordination.DomainID
	ControlVisibility []byte
	Store             Store
	Authority         AuthoritySource
	Operations        OperationSource
	Leases            LeaseSource
	PolicyVerifier    PolicyVerifier
	IndexVerifier     IndexVerifier
	Clock             func() time.Time
	MaxRetries        int
	RetryBackoff      time.Duration
	MaxScan           int
}

type PolicyFenceRequest struct {
	LPART               coordination.LPART
	CopyGeneration      coordination.Generation
	VisibilityDigest    coordination.Digest
	Owner               coordination.OwnerID
	OperationID         []byte
	LeaseUntil          time.Time
	Fence               coordination.Fence
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
}

type PolicyFence struct {
	Request          PolicyFenceRequest
	RecordGeneration coordination.Generation
	UpdatedAt        time.Time
	Active           bool
	publication      *policyPublicationMarker
	retirement       *policyRetirementMarker
}

type policyPublicationMarker struct {
	Map       coordination.PolicyCopyMapV3
	MapDigest coordination.Digest
}

type policyRetirementMarker struct {
	Through               coordination.Epoch
	PublicationDigest     coordination.Digest
	PredecessorRootDigest coordination.Digest
	SuccessorRootDigest   coordination.Digest
}

type PolicyManifestSet struct {
	Chunks         []coordination.PolicyCopyManifestV1
	LogicalDigest  coordination.Digest
	PhysicalDigest coordination.Digest
	ManifestDigest coordination.Digest
	RowCount       uint64
}

type PolicyPin struct {
	Map       coordination.PolicyCopyMapV3
	PinDigest coordination.Digest
}

type IndexBuild struct {
	Manifest            coordination.IndexGenerationV2
	Owner               coordination.OwnerID
	OperationID         []byte
	Fence               coordination.Fence
	AuthorityGeneration coordination.Generation
	RetentionGeneration coordination.Generation
	RecordGeneration    coordination.Generation
	UpdatedAt           time.Time
}

type IndexPin struct {
	Activation coordination.IndexActivationV2
	Manifest   coordination.IndexGenerationV2
	PinDigest  coordination.Digest
}
