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

package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type policyRoot struct {
	Set              PolicyManifestSet
	State            coordination.CopyState
	RecordGeneration coordination.Generation
}

func validateFence(value PolicyFence) error {
	r := value.Request
	if err := r.LPART.Validate(); err != nil {
		return err
	}
	if err := r.CopyGeneration.Validate(); err != nil {
		return err
	}
	if err := r.VisibilityDigest.Validate("visibility digest"); err != nil {
		return err
	}
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if len(r.OperationID) == 0 || len(r.OperationID) > coordination.MaxOpaqueIDBytes {
		return ErrBounds
	}
	if err := validUTC(r.LeaseUntil); err != nil {
		return err
	}
	if err := r.Fence.Validate(); err != nil {
		return err
	}
	if err := r.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := r.RetentionGeneration.Validate(); err != nil {
		return err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return err
	}
	if value.publication != nil {
		encoded, err := coordination.MarshalPolicyCopyMapV3(value.publication.Map)
		if err != nil {
			return err
		}
		if !bytes.Equal(value.publication.Map.LPART, r.LPART) ||
			value.publication.Map.CopyGeneration != r.CopyGeneration ||
			value.publication.Map.VisibilityDigest != r.VisibilityDigest ||
			value.publication.Map.State != coordination.CopyStateActive ||
			coordination.Sum(encoded) != value.publication.MapDigest {
			return ErrConflict
		}
	}
	if value.retirement != nil {
		if value.publication == nil || value.retirement.Through.Validate() != nil ||
			value.retirement.PublicationDigest != value.publication.MapDigest ||
			value.retirement.PredecessorRootDigest == (coordination.Digest{}) ||
			value.retirement.SuccessorRootDigest == (coordination.Digest{}) ||
			value.retirement.PredecessorRootDigest == value.retirement.SuccessorRootDigest {
			return ErrConflict
		}
	}
	return validUTC(value.UpdatedAt)
}

func (c *Client) ReserveCopyGeneration(ctx context.Context, lpart coordination.LPART, reservationID []byte) (coordination.Generation, error) {
	row, err := coordination.PolicyCopyHeadRow(c.domain, lpart)
	if err != nil {
		return 0, err
	}
	return c.reserveGeneration(ctx, row, reservationID)
}

func (c *Client) policyFenceCoordinate(request PolicyFenceRequest) (allocator.Coordinate, error) {
	row, err := coordination.PolicyCopyFenceRowV2(c.domain, request.LPART, request.CopyGeneration, request.VisibilityDigest)
	if err != nil {
		return allocator.Coordinate{}, err
	}
	return c.coordinate(row, familyCopy, qualifierFence), nil
}

func (c *Client) readFence(ctx context.Context, request PolicyFenceRequest) (PolicyFence, []byte, int64, bool, error) {
	coordinate, err := c.policyFenceCoordinate(request)
	if err != nil {
		return PolicyFence{}, nil, 0, false, err
	}
	cell, found, err := c.readOne(ctx, coordinate)
	if err != nil || !found {
		return PolicyFence{}, nil, 0, found, err
	}
	value, err := unmarshalFence(cell.Value)
	if err != nil || cell.Timestamp != int64(value.RecordGeneration) ||
		!bytes.Equal(value.Request.LPART, request.LPART) ||
		value.Request.CopyGeneration != request.CopyGeneration ||
		value.Request.VisibilityDigest != request.VisibilityDigest {
		return PolicyFence{}, nil, 0, false, ErrCorruption
	}
	return value, cell.Value, cell.Timestamp, true, nil
}

func (c *Client) AcquirePolicyFence(ctx context.Context, request PolicyFenceRequest) (PolicyFence, error) {
	candidate := PolicyFence{Request: cloneFenceRequest(request), RecordGeneration: 1, UpdatedAt: c.clock().UTC(), Active: true}
	if err := validateFence(candidate); err != nil {
		return PolicyFence{}, err
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return PolicyFence{}, err
	}
	if err := requireAuthority(request.AuthorityGeneration, request.RetentionGeneration, authority); err != nil {
		return PolicyFence{}, err
	}
	coordinate, err := c.policyFenceCoordinate(request)
	if err != nil {
		return PolicyFence{}, err
	}
	for attempt := 0; ; attempt++ {
		current, before, timestamp, found, readErr := c.readFence(ctx, request)
		if readErr != nil {
			return PolicyFence{}, readErr
		}
		if !found {
			value, encodeErr := marshalFence(candidate)
			if encodeErr != nil {
				return PolicyFence{}, encodeErr
			}
			err = c.absentOrIdentical(ctx, coordinate, value, 1)
			if err == nil {
				return cloneFence(candidate), nil
			}
			if !errors.Is(err, ErrCorruption) || attempt >= c.maxRetries {
				return PolicyFence{}, err
			}
		} else {
			if current.Active && sameFenceIdentity(current.Request, request) {
				return cloneFence(current), nil
			}
			if current.Active && current.Request.LeaseUntil.After(c.clock().UTC()) {
				return PolicyFence{}, ErrBusy
			}
			if current.Active {
				disposition, statusErr := c.operations.Status(ctx, c.domain, append([]byte(nil), current.Request.OperationID...))
				if statusErr != nil {
					return PolicyFence{}, classifyUnavailable(statusErr)
				}
				if disposition == OperationNonterminal {
					return PolicyFence{}, ErrBusy
				}
				if disposition == OperationCommitted {
					return PolicyFence{}, ErrConflict
				}
			}
			if request.Fence <= current.Request.Fence {
				return PolicyFence{}, ErrStaleOwner
			}
			if current.RecordGeneration == coordination.Generation(math.MaxInt64) {
				return PolicyFence{}, ErrOverflow
			}
			candidate.publication = clonePublicationMarker(current.publication)
			candidate.retirement = cloneRetirementMarker(current.retirement)
			candidate.RecordGeneration = current.RecordGeneration + 1
			candidate.UpdatedAt = c.clock().UTC()
			value, encodeErr := marshalFence(candidate)
			if encodeErr != nil {
				return PolicyFence{}, encodeErr
			}
			err = c.transition(ctx, coordinate, before, timestamp, value, int64(candidate.RecordGeneration))
			if err == nil {
				return cloneFence(candidate), nil
			}
			if !errors.Is(err, ErrConflict) || attempt >= c.maxRetries {
				return PolicyFence{}, err
			}
		}
		if err := c.wait(ctx); err != nil {
			return PolicyFence{}, err
		}
	}
}

func (c *Client) RenewPolicyFence(ctx context.Context, request PolicyFenceRequest) (PolicyFence, error) {
	current, before, timestamp, found, err := c.readFence(ctx, request)
	if err != nil {
		return PolicyFence{}, err
	}
	if !found {
		return PolicyFence{}, ErrNotFound
	}
	if !current.Active || !sameFenceOwner(current.Request, request) {
		return PolicyFence{}, ErrStaleOwner
	}
	if !current.Request.LeaseUntil.After(c.clock().UTC()) {
		return PolicyFence{}, ErrExpired
	}
	if request.LeaseUntil.Before(current.Request.LeaseUntil) {
		return PolicyFence{}, ErrConflict
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return PolicyFence{}, err
	}
	if err := requireAuthority(request.AuthorityGeneration, request.RetentionGeneration, authority); err != nil {
		return PolicyFence{}, err
	}
	current.Request.LeaseUntil = request.LeaseUntil
	current.RecordGeneration++
	current.UpdatedAt = c.clock().UTC()
	after, err := marshalFence(current)
	if err != nil {
		return PolicyFence{}, err
	}
	coordinate, _ := c.policyFenceCoordinate(request)
	if err := c.transition(ctx, coordinate, before, timestamp, after, int64(current.RecordGeneration)); err != nil {
		return PolicyFence{}, err
	}
	return cloneFence(current), nil
}

func (c *Client) ReleasePolicyFence(ctx context.Context, request PolicyFenceRequest) error {
	current, before, timestamp, found, err := c.readFence(ctx, request)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	if !sameFenceOwner(current.Request, request) {
		return ErrStaleOwner
	}
	if !current.Active {
		return nil
	}
	current.Active = false
	current.RecordGeneration++
	current.UpdatedAt = c.clock().UTC()
	after, err := marshalFence(current)
	if err != nil {
		return err
	}
	coordinate, _ := c.policyFenceCoordinate(request)
	return c.transition(ctx, coordinate, before, timestamp, after, int64(current.RecordGeneration))
}

func normalizeManifestSet(value PolicyManifestSet) (PolicyManifestSet, error) {
	value.Chunks = append([]coordination.PolicyCopyManifestV1(nil), value.Chunks...)
	if len(value.Chunks) == 0 || len(value.Chunks) > MaxPolicyChunks {
		return PolicyManifestSet{}, ErrBounds
	}
	var rows uint64
	var encoded [][]byte
	var previous coordination.PolicyCopyEntry
	havePrevious := false
	for _, chunk := range value.Chunks {
		if chunk.State != coordination.CopyStateSealed && chunk.State != coordination.CopyStateActive {
			return PolicyManifestSet{}, ErrConflict
		}
		data, err := coordination.MarshalPolicyCopyManifestV1(chunk)
		if err != nil {
			return PolicyManifestSet{}, err
		}
		for _, entry := range chunk.Entries {
			if havePrevious && coordination.ComparePolicyCopyEntries(previous, entry) >= 0 {
				return PolicyManifestSet{}, ErrConflict
			}
			previous, havePrevious = entry, true
		}
		if rows > math.MaxInt64-chunk.RowCount {
			return PolicyManifestSet{}, ErrOverflow
		}
		rows += chunk.RowCount
		encoded = append(encoded, data)
	}
	if err := value.LogicalDigest.Validate("logical manifest digest"); err != nil {
		return PolicyManifestSet{}, err
	}
	if err := value.PhysicalDigest.Validate("physical copy digest"); err != nil {
		return PolicyManifestSet{}, err
	}
	parts := [][]byte{[]byte("policy-manifest-set-v1"), value.LogicalDigest[:], value.PhysicalDigest[:]}
	parts = append(parts, encoded...)
	computed := digestParts(parts...)
	if value.RowCount != 0 && value.RowCount != rows {
		return PolicyManifestSet{}, ErrConflict
	}
	if value.ManifestDigest != (coordination.Digest{}) && value.ManifestDigest != computed {
		return PolicyManifestSet{}, ErrConflict
	}
	value.RowCount, value.ManifestDigest = rows, computed
	return value, nil
}

func marshalPolicyRoot(value policyRoot) ([]byte, error) {
	set := value.Set
	if len(set.Chunks) == 0 || len(set.Chunks) > MaxPolicyChunks {
		return nil, ErrBounds
	}
	if len(set.Chunks[0].LPART) != 0 {
		var err error
		set, err = normalizeManifestSet(set)
		if err != nil {
			return nil, err
		}
	} else {
		if set.RowCount == 0 {
			return nil, ErrConflict
		}
		if err := set.LogicalDigest.Validate("logical manifest digest"); err != nil {
			return nil, err
		}
		if err := set.PhysicalDigest.Validate("physical copy digest"); err != nil {
			return nil, err
		}
		if err := set.ManifestDigest.Validate("manifest set digest"); err != nil {
			return nil, err
		}
	}
	if err := value.State.Validate(); err != nil {
		return nil, err
	}
	if value.State == coordination.CopyStateBuilding {
		return nil, ErrConflict
	}
	var w writer
	w.u32(uint32(len(set.Chunks)))
	w.u64(set.RowCount)
	w.digest(set.LogicalDigest)
	w.digest(set.PhysicalDigest)
	w.digest(set.ManifestDigest)
	w.u8(byte(value.State))
	w.u64(uint64(value.RecordGeneration))
	return envelope('P', w.Bytes()), nil
}

func unmarshalPolicyRoot(data []byte) (policyRoot, error) {
	payload, err := openEnvelope(data, 'P', coordination.MaxRootBytes)
	if err != nil {
		return policyRoot{}, err
	}
	r := reader{data: payload}
	count := r.u32()
	value := policyRoot{Set: PolicyManifestSet{
		Chunks: make([]coordination.PolicyCopyManifestV1, count), RowCount: r.u64(),
		LogicalDigest: r.digest(), PhysicalDigest: r.digest(), ManifestDigest: r.digest(),
	}, State: coordination.CopyState(r.u8()), RecordGeneration: coordination.Generation(r.u64())}
	if r.err != nil || r.off != len(payload) || count == 0 || count > MaxPolicyChunks ||
		value.State.Validate() != nil || value.RecordGeneration.Validate() != nil {
		return policyRoot{}, ErrCorruption
	}
	return value, nil
}

func chunkQualifier(index int) []byte {
	value := make([]byte, 5)
	value[0] = 'c'
	binary.BigEndian.PutUint32(value[1:], uint32(index))
	return value
}

func (c *Client) WritePolicyManifest(ctx context.Context, fence PolicyFence, set PolicyManifestSet) (PolicyManifestSet, error) {
	current, _, _, found, err := c.readFence(ctx, fence.Request)
	if err != nil {
		return PolicyManifestSet{}, err
	}
	if !found || !current.Active || !sameFenceOwner(current.Request, fence.Request) {
		return PolicyManifestSet{}, ErrStaleOwner
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return PolicyManifestSet{}, err
	}
	if err := requireAuthority(fence.Request.AuthorityGeneration, fence.Request.RetentionGeneration, authority); err != nil {
		return PolicyManifestSet{}, err
	}
	set, err = normalizeManifestSet(set)
	if err != nil {
		return PolicyManifestSet{}, err
	}
	for index, chunk := range set.Chunks {
		if !bytes.Equal(chunk.LPART, fence.Request.LPART) ||
			chunk.CopyGeneration != fence.Request.CopyGeneration ||
			chunk.VisibilityDigest != fence.Request.VisibilityDigest {
			return PolicyManifestSet{}, ErrConflict
		}
		data, encodeErr := coordination.MarshalPolicyCopyManifestV1(chunk)
		if encodeErr != nil {
			return PolicyManifestSet{}, encodeErr
		}
		row, _ := coordination.PolicyCopyRow(c.domain, chunk.LPART, chunk.CopyGeneration, chunk.VisibilityDigest)
		if err := c.absentOrIdentical(ctx, c.coordinate(row, familyCopy, chunkQualifier(index)), data, int64(chunk.CopyGeneration)); err != nil {
			return PolicyManifestSet{}, err
		}
	}
	root := policyRoot{Set: set, State: coordination.CopyStateSealed, RecordGeneration: 1}
	data, err := marshalPolicyRoot(root)
	if err != nil {
		return PolicyManifestSet{}, err
	}
	row, _ := coordination.PolicyCopyRow(c.domain, fence.Request.LPART, fence.Request.CopyGeneration, fence.Request.VisibilityDigest)
	if err := c.absentOrIdentical(ctx, c.coordinate(row, familyCopy, qualifierRoot), data, 1); err != nil {
		return PolicyManifestSet{}, err
	}
	return cloneManifestSet(set), nil
}

func (c *Client) PublishPolicyMapping(ctx context.Context, fence PolicyFence, set PolicyManifestSet, mapping coordination.PolicyCopyMapV3) error {
	set, err := normalizeManifestSet(set)
	if err != nil {
		return err
	}
	if !bytes.Equal(mapping.LPART, fence.Request.LPART) ||
		mapping.CopyGeneration != fence.Request.CopyGeneration ||
		mapping.VisibilityDigest != fence.Request.VisibilityDigest ||
		mapping.CopyDigest != set.PhysicalDigest || mapping.State != coordination.CopyStateActive {
		return ErrConflict
	}
	if err := c.verifyStoredPolicySet(ctx, fence.Request, set); err != nil {
		return err
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := requireAuthority(fence.Request.AuthorityGeneration, fence.Request.RetentionGeneration, authority); err != nil {
		return err
	}
	proof := PolicyCopyProof{
		Domain: c.domain, LPART: fence.Request.LPART, CopyGeneration: fence.Request.CopyGeneration,
		VisibilityDigest: fence.Request.VisibilityDigest, ManifestDigest: set.ManifestDigest,
		LogicalDigest: set.LogicalDigest, PhysicalDigest: set.PhysicalDigest, RowCount: set.RowCount,
		Owner: fence.Request.Owner, OperationID: fence.Request.OperationID, Fence: fence.Request.Fence,
		AuthorityGeneration: fence.Request.AuthorityGeneration, RetentionGeneration: fence.Request.RetentionGeneration,
		HistoryFloor: authority.HistoryFloor,
	}
	if err := c.policyVerifier.VerifyCopy(ctx, proof); err != nil {
		return classifyVerifier(err)
	}
	if err := c.policyVerifier.VerifyMapping(ctx, c.domain, mapping); err != nil {
		return classifyVerifier(err)
	}
	data, err := coordination.MarshalPolicyCopyMapV3(mapping)
	if err != nil {
		return err
	}
	row, err := coordination.PolicyCopyMapRow(c.domain, mapping.LPART, mapping.MapGeneration, mapping.VisibilityDigest)
	if err != nil {
		return err
	}
	if err := c.absentOrIdentical(ctx, c.coordinate(row, familyMap, qualifierActive), data, int64(mapping.MapGeneration)); err != nil {
		return err
	}

	currentFence, before, timestamp, found, err := c.readFence(ctx, fence.Request)
	if err != nil {
		return err
	}
	if !found || !currentFence.Active || !sameFenceOwner(currentFence.Request, fence.Request) ||
		!currentFence.Request.LeaseUntil.After(c.clock().UTC()) {
		return ErrStaleOwner
	}
	marker := &policyPublicationMarker{Map: cloneMap(mapping), MapDigest: coordination.Sum(data)}
	if currentFence.publication != nil {
		if !publicationMarkerEqual(currentFence.publication, marker) {
			return ErrConflict
		}
		return nil
	}
	if currentFence.retirement != nil || currentFence.RecordGeneration == coordination.Generation(math.MaxInt64) {
		return ErrConflict
	}
	currentFence.publication = marker
	currentFence.RecordGeneration++
	currentFence.UpdatedAt = c.clock().UTC()
	after, err := marshalFence(currentFence)
	if err != nil {
		return err
	}
	coordinate, _ := c.policyFenceCoordinate(fence.Request)
	return c.transition(ctx, coordinate, before, timestamp, after, int64(currentFence.RecordGeneration))
}

func (c *Client) LookupPolicyCopy(ctx context.Context, lpart coordination.LPART, policyGeneration coordination.Generation) (PolicyPin, error) {
	if err := policyGeneration.Validate(); err != nil {
		return PolicyPin{}, err
	}
	prefix, err := coordination.PolicyCopyMapPrefix(c.domain, lpart)
	if err != nil {
		return PolicyPin{}, err
	}
	start, err := coordination.PolicyCopyMapSeek(c.domain, lpart, policyGeneration)
	if err != nil {
		return PolicyPin{}, err
	}
	cells, err := c.store.ScanPrefixFrom(
		ctx, prefix, start, familyMap, qualifierActive, c.visibility, c.maxScan,
	)
	if err != nil {
		return PolicyPin{}, classifyUnavailable(err)
	}
	if len(cells) == c.maxScan {
		return PolicyPin{}, ErrBounds
	}
	sort.Slice(cells, func(i, j int) bool { return bytes.Compare(cells[i].Coordinate.Row, cells[j].Coordinate.Row) < 0 })
	var selected *coordination.PolicyCopyMapV3
	var observedGeneration coordination.Generation
	for _, cell := range cells {
		key, parseErr := coordination.ParsePolicyCopyMapRow(cell.Coordinate.Row)
		if parseErr != nil || !bytes.Equal(key.Domain, c.domain) || !bytes.Equal(key.LPART, lpart) {
			return PolicyPin{}, ErrCorruption
		}
		if key.Generation > policyGeneration {
			return PolicyPin{}, ErrCorruption
		}
		if observedGeneration == 0 {
			observedGeneration = key.Generation
		} else if key.Generation == observedGeneration {
			return PolicyPin{}, ErrCorruption
		} else if selected != nil {
			break
		}
		value, decodeErr := coordination.UnmarshalPolicyCopyMapV3(cell.Value)
		if decodeErr != nil || value.MapGeneration != key.Generation ||
			!bytes.Equal(value.LPART, lpart) || cell.Timestamp != int64(value.MapGeneration) {
			return PolicyPin{}, ErrCorruption
		}
		if value.State == coordination.CopyStateActive {
			published, retired, markerErr := c.policyMapState(ctx, value, cell.Value)
			if markerErr != nil {
				return PolicyPin{}, markerErr
			}
			if !published || retired {
				continue
			}
			if selected == nil {
				copy := value
				selected = &copy
				continue
			}
			break
		}
	}

	if selected == nil {
		return PolicyPin{}, ErrNotFound
	}
	if err := c.policyVerifier.VerifyMapping(ctx, c.domain, *selected); err != nil {
		return PolicyPin{}, classifyVerifier(err)
	}
	data, _ := coordination.MarshalPolicyCopyMapV3(*selected)
	return PolicyPin{Map: cloneMap(*selected), PinDigest: digestParts([]byte("policy-pin-v1"), data)}, nil
}

func (c *Client) policyMapState(
	ctx context.Context,
	mapping coordination.PolicyCopyMapV3,
	encoded []byte,
) (bool, bool, error) {
	request := PolicyFenceRequest{
		LPART: mapping.LPART, CopyGeneration: mapping.CopyGeneration,
		VisibilityDigest: mapping.VisibilityDigest,
	}
	fence, _, _, found, err := c.readFence(ctx, request)
	if err != nil {
		return false, false, err
	}
	if !found || fence.publication == nil {
		return false, false, nil
	}
	digest := coordination.Sum(encoded)
	if fence.publication.MapDigest != digest || !mapsEqual(fence.publication.Map, mapping) {
		return false, false, nil
	}
	row, err := coordination.PolicyCopyRow(c.domain, mapping.LPART, mapping.CopyGeneration, mapping.VisibilityDigest)
	if err != nil {
		return false, false, err
	}
	rootCell, found, err := c.readOne(ctx, c.coordinate(row, familyCopy, qualifierRoot))
	if err != nil {
		return false, false, err
	}
	if !found {
		return false, false, nil
	}
	root, err := unmarshalPolicyRoot(rootCell.Value)
	if err != nil || root.Set.PhysicalDigest != mapping.CopyDigest {
		return false, false, ErrCorruption
	}
	if fence.retirement == nil {
		if root.State == coordination.CopyStateRetired {
			return false, false, ErrCorruption
		}
		return true, false, nil
	}
	if fence.retirement.PublicationDigest != digest {
		return false, false, ErrCorruption
	}
	rootDigest := coordination.Sum(rootCell.Value)
	if rootDigest != fence.retirement.PredecessorRootDigest &&
		rootDigest != fence.retirement.SuccessorRootDigest {
		return false, false, ErrCorruption
	}
	return true, true, nil
}

func (c *Client) verifyStoredPolicySet(
	ctx context.Context,
	request PolicyFenceRequest,
	set PolicyManifestSet,
) error {
	row, err := coordination.PolicyCopyRow(c.domain, request.LPART, request.CopyGeneration, request.VisibilityDigest)
	if err != nil {
		return err
	}
	coordinates := make([]allocator.Coordinate, 0, len(set.Chunks)+1)
	coordinates = append(coordinates, c.coordinate(row, familyCopy, qualifierRoot))
	for index := range set.Chunks {
		coordinates = append(coordinates, c.coordinate(row, familyCopy, chunkQualifier(index)))
	}
	cells, err := c.store.ReadExact(ctx, coordinates)
	if err != nil {
		return classifyUnavailable(err)
	}
	if len(cells) != len(coordinates) {
		return ErrNotFound
	}
	byCoordinate := make(map[string]allocator.Cell, len(cells))
	for _, cell := range cells {
		byCoordinate[coordinateKeyForCatalog(cell.Coordinate)] = cell
	}
	rootCell, ok := byCoordinate[coordinateKeyForCatalog(coordinates[0])]
	if !ok {
		return ErrNotFound
	}
	root, err := unmarshalPolicyRoot(rootCell.Value)
	if err != nil || root.Set.ManifestDigest != set.ManifestDigest ||
		root.Set.LogicalDigest != set.LogicalDigest || root.Set.PhysicalDigest != set.PhysicalDigest ||
		root.Set.RowCount != set.RowCount || root.State != coordination.CopyStateSealed {
		return ErrCorruption
	}
	for index, chunk := range set.Chunks {
		cell, exists := byCoordinate[coordinateKeyForCatalog(coordinates[index+1])]
		if !exists {
			return ErrNotFound
		}
		expected, encodeErr := coordination.MarshalPolicyCopyManifestV1(chunk)
		if encodeErr != nil || !bytes.Equal(cell.Value, expected) ||
			cell.Timestamp != int64(request.CopyGeneration) {
			return ErrCorruption
		}
	}
	return nil
}

func coordinateKeyForCatalog(value allocator.Coordinate) string {
	return string(value.Row) + "\x00" + string(value.Family) + "\x00" +
		string(value.Qualifier) + "\x00" + string(value.Visibility)
}

func (c *Client) RetirePolicyCopy(ctx context.Context, fence PolicyFence, set PolicyManifestSet, through coordination.Epoch) error {
	if err := through.Validate(); err != nil {
		return err
	}
	set, err := normalizeManifestSet(set)
	if err != nil {
		return err
	}
	currentFence, beforeFence, fenceTimestamp, found, err := c.readFence(ctx, fence.Request)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	row, _ := coordination.PolicyCopyRow(c.domain, fence.Request.LPART, fence.Request.CopyGeneration, fence.Request.VisibilityDigest)
	coordinate := c.coordinate(row, familyCopy, qualifierRoot)
	cell, rootFound, err := c.readOne(ctx, coordinate)
	if err != nil {
		return err
	}
	if !rootFound {
		return ErrNotFound
	}
	root, err := unmarshalPolicyRoot(cell.Value)
	if err != nil {
		return err
	}
	if root.Set.ManifestDigest != set.ManifestDigest {
		return ErrCorruption
	}
	retiredRoot := root
	if retiredRoot.State != coordination.CopyStateRetired {
		if retiredRoot.RecordGeneration == coordination.Generation(math.MaxInt64) {
			return ErrOverflow
		}
		retiredRoot.State = coordination.CopyStateRetired
		retiredRoot.RecordGeneration++
	}
	retiredData, err := marshalPolicyRoot(retiredRoot)
	if err != nil {
		return err
	}
	currentDigest := coordination.Sum(cell.Value)
	retiredDigest := coordination.Sum(retiredData)
	if currentFence.retirement != nil {
		marker := currentFence.retirement
		if marker.Through != through || marker.PublicationDigest != publicationDigest(currentFence.publication) ||
			(currentDigest != marker.PredecessorRootDigest && currentDigest != marker.SuccessorRootDigest) ||
			retiredDigest != marker.SuccessorRootDigest {
			return ErrConflict
		}
		if currentDigest == marker.PredecessorRootDigest {
			if err := c.transition(ctx, coordinate, cell.Value, cell.Timestamp, retiredData, int64(retiredRoot.RecordGeneration)); err != nil {
				return err
			}
		}
		if currentFence.Active && sameFenceOwner(currentFence.Request, fence.Request) {
			return c.ReleasePolicyFence(ctx, currentFence.Request)
		}
		return nil
	}
	if !currentFence.Active || !sameFenceOwner(currentFence.Request, fence.Request) ||
		!currentFence.Request.LeaseUntil.After(c.clock().UTC()) {
		return ErrStaleOwner
	}
	if currentFence.publication == nil {
		return ErrConflict
	}
	selected, err := c.leases.SelectsPolicyCopy(ctx, c.domain, coordination.PolicyCopyPin{
		LPART: fence.Request.LPART, MapGeneration: currentFence.publication.Map.MapGeneration,
		CopyGeneration: fence.Request.CopyGeneration, VisibilityDigest: fence.Request.VisibilityDigest,
	})
	if err != nil {
		return classifyUnavailable(err)
	}
	if selected {
		return ErrLeaseActive
	}
	authority, err := c.currentAuthority(ctx)
	if err != nil {
		return err
	}
	if err := requireAuthority(fence.Request.AuthorityGeneration, fence.Request.RetentionGeneration, authority); err != nil {
		return err
	}
	if through >= authority.HistoryFloor {
		return ErrStaleRetention
	}
	proof := PolicyCopyProof{
		Domain: c.domain, LPART: fence.Request.LPART, CopyGeneration: fence.Request.CopyGeneration,
		VisibilityDigest: fence.Request.VisibilityDigest, ManifestDigest: set.ManifestDigest,
		LogicalDigest: set.LogicalDigest, PhysicalDigest: set.PhysicalDigest, RowCount: set.RowCount,
		Owner: fence.Request.Owner, OperationID: fence.Request.OperationID, Fence: fence.Request.Fence,
		AuthorityGeneration: fence.Request.AuthorityGeneration, RetentionGeneration: fence.Request.RetentionGeneration,
		HistoryFloor: authority.HistoryFloor,
	}
	if err := c.policyVerifier.AllowPolicyRetirement(ctx, proof, through); err != nil {
		return classifyVerifier(err)
	}
	if root.State == coordination.CopyStateRetired {
		return ErrCorruption
	}
	currentFence.retirement = &policyRetirementMarker{
		Through:               through,
		PublicationDigest:     currentFence.publication.MapDigest,
		PredecessorRootDigest: currentDigest,
		SuccessorRootDigest:   retiredDigest,
	}
	if currentFence.RecordGeneration == coordination.Generation(math.MaxInt64) {
		return ErrOverflow
	}
	currentFence.RecordGeneration++
	currentFence.UpdatedAt = c.clock().UTC()
	afterFence, err := marshalFence(currentFence)
	if err != nil {
		return err
	}
	fenceCoordinate, _ := c.policyFenceCoordinate(fence.Request)
	if err := c.transition(ctx, fenceCoordinate, beforeFence, fenceTimestamp, afterFence, int64(currentFence.RecordGeneration)); err != nil {
		return err
	}
	if err := c.transition(ctx, coordinate, cell.Value, cell.Timestamp, retiredData, int64(retiredRoot.RecordGeneration)); err != nil {
		return err
	}
	latest, _, _, found, err := c.readFence(ctx, fence.Request)
	if err != nil {
		return err
	}
	if found && latest.Active && sameFenceOwner(latest.Request, fence.Request) {
		return c.ReleasePolicyFence(ctx, latest.Request)
	}
	return nil
}

func classifyVerifier(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrCorruption) ||
		errors.Is(err, ErrStaleAuthority) || errors.Is(err, ErrStaleRetention) ||
		errors.Is(err, ErrLeaseActive) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: verification failed", ErrUnavailable)
}

func sameFenceIdentity(a, b PolicyFenceRequest) bool {
	return sameFenceOwner(a, b) && a.LeaseUntil.Equal(b.LeaseUntil)
}

func sameFenceOwner(a, b PolicyFenceRequest) bool {
	return bytes.Equal(a.LPART, b.LPART) && a.CopyGeneration == b.CopyGeneration &&
		a.VisibilityDigest == b.VisibilityDigest && bytes.Equal(a.Owner, b.Owner) &&
		bytes.Equal(a.OperationID, b.OperationID) && a.Fence == b.Fence &&
		a.AuthorityGeneration == b.AuthorityGeneration && a.RetentionGeneration == b.RetentionGeneration
}

func cloneFenceRequest(value PolicyFenceRequest) PolicyFenceRequest {
	value.LPART = append(coordination.LPART(nil), value.LPART...)
	value.Owner = append(coordination.OwnerID(nil), value.Owner...)
	value.OperationID = append([]byte(nil), value.OperationID...)
	return value
}

func cloneFence(value PolicyFence) PolicyFence {
	value.Request = cloneFenceRequest(value.Request)
	value.publication = clonePublicationMarker(value.publication)
	value.retirement = cloneRetirementMarker(value.retirement)
	return value
}

func cloneManifestSet(value PolicyManifestSet) PolicyManifestSet {
	value.Chunks = append([]coordination.PolicyCopyManifestV1(nil), value.Chunks...)
	for i := range value.Chunks {
		value.Chunks[i].LPART = append(coordination.LPART(nil), value.Chunks[i].LPART...)
		value.Chunks[i].Backend = append(coordination.BackendID(nil), value.Chunks[i].Backend...)
		value.Chunks[i].Table = append([]byte(nil), value.Chunks[i].Table...)
		value.Chunks[i].Entries = append([]coordination.PolicyCopyEntry(nil), value.Chunks[i].Entries...)
		for j := range value.Chunks[i].Entries {
			value.Chunks[i].Entries[j].Table = append([]byte(nil), value.Chunks[i].Entries[j].Table...)
			value.Chunks[i].Entries[j].RowIdentity = append([]byte(nil), value.Chunks[i].Entries[j].RowIdentity...)
		}
	}
	return value
}

func cloneMap(value coordination.PolicyCopyMapV3) coordination.PolicyCopyMapV3 {
	value.LPART = append(coordination.LPART(nil), value.LPART...)
	value.ActivationRef = append([]byte(nil), value.ActivationRef...)
	return value
}

func clonePublicationMarker(value *policyPublicationMarker) *policyPublicationMarker {
	if value == nil {
		return nil
	}
	return &policyPublicationMarker{Map: cloneMap(value.Map), MapDigest: value.MapDigest}
}

func cloneRetirementMarker(value *policyRetirementMarker) *policyRetirementMarker {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func publicationMarkerEqual(left, right *policyPublicationMarker) bool {
	return left != nil && right != nil && left.MapDigest == right.MapDigest && mapsEqual(left.Map, right.Map)
}

func mapsEqual(left, right coordination.PolicyCopyMapV3) bool {
	leftData, leftErr := coordination.MarshalPolicyCopyMapV3(left)
	rightData, rightErr := coordination.MarshalPolicyCopyMapV3(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func publicationDigest(marker *policyPublicationMarker) coordination.Digest {
	if marker == nil {
		return coordination.Digest{}
	}
	return marker.MapDigest
}
