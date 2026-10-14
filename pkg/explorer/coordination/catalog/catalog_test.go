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
	"crypto/sha256"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

type fault uint8

const (
	faultNone fault = iota
	faultUnknownBefore
	faultUnknownAfter
)

type memoryStore struct {
	mu        sync.Mutex
	versions  map[string][]allocator.Cell
	fault     fault
	faultAt   int
	mutations int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{versions: make(map[string][]allocator.Cell)}
}

func coordinateKey(value allocator.Coordinate) string {
	return string(value.Row) + "\x00" + string(value.Family) + "\x00" +
		string(value.Qualifier) + "\x00" + string(value.Visibility)
}

func cloneCell(value allocator.Cell) allocator.Cell {
	value.Coordinate.Row = append([]byte(nil), value.Coordinate.Row...)
	value.Coordinate.Family = append([]byte(nil), value.Coordinate.Family...)
	value.Coordinate.Qualifier = append([]byte(nil), value.Coordinate.Qualifier...)
	value.Coordinate.Visibility = append([]byte(nil), value.Coordinate.Visibility...)
	value.Value = append([]byte(nil), value.Value...)
	return value
}

func (s *memoryStore) latest(coordinate allocator.Coordinate) (allocator.Cell, bool) {
	values := s.versions[coordinateKey(coordinate)]
	if len(values) == 0 {
		return allocator.Cell{}, false
	}
	return values[len(values)-1], true
}

func (s *memoryStore) ReadExact(_ context.Context, coordinates []allocator.Coordinate) ([]allocator.Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]allocator.Cell, 0, len(coordinates))
	for _, coordinate := range coordinates {
		if value, ok := s.latest(coordinate); ok {
			result = append(result, cloneCell(value))
		}
	}
	return result, nil
}

func (s *memoryStore) ScanPrefix(
	_ context.Context,
	prefix, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	return s.ScanPrefixFrom(context.Background(), prefix, prefix, family, qualifier, visibility, limit)
}

func (s *memoryStore) ScanPrefixFrom(
	_ context.Context,
	prefix, start, family, qualifier, visibility []byte,
	limit int,
) ([]allocator.Cell, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]allocator.Cell, 0, limit)
	for _, values := range s.versions {
		if len(values) == 0 {
			continue
		}
		value := values[len(values)-1]
		if bytes.HasPrefix(value.Coordinate.Row, prefix) &&
			bytes.Compare(value.Coordinate.Row, start) >= 0 &&
			bytes.Equal(value.Coordinate.Family, family) &&
			bytes.Equal(value.Coordinate.Qualifier, qualifier) &&
			bytes.Equal(value.Coordinate.Visibility, visibility) {
			result = append(result, cloneCell(value))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Row, result[j].Coordinate.Row) < 0
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) CompareAndMutate(_ context.Context, mutation allocator.Mutation) (allocator.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutations++
	currentFault := faultNone
	if s.fault != faultNone && (s.faultAt == 0 || s.mutations == s.faultAt) {
		currentFault = s.fault
		s.fault = faultNone
		s.faultAt = 0
	}
	if currentFault == faultUnknownBefore {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}

	for _, condition := range mutation.Conditions {
		current, found := s.latest(condition.Coordinate)
		if condition.Absent {
			if found {
				return allocator.StatusRejected, nil
			}
			continue
		}
		if !found || !bytes.Equal(current.Value, condition.Value) ||
			(condition.TimestampSet && current.Timestamp != condition.Timestamp) {
			return allocator.StatusRejected, nil
		}
	}
	for _, update := range mutation.Updates {
		if update.Delete {
			delete(s.versions, coordinateKey(update.Coordinate))
			continue
		}
		cell := allocator.Cell{
			Coordinate: update.Coordinate, Value: append([]byte(nil), update.Value...), Timestamp: update.Timestamp,
		}
		key := coordinateKey(update.Coordinate)
		s.versions[key] = append(s.versions[key], cloneCell(cell))
	}
	if currentFault == faultUnknownAfter {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	return allocator.StatusAccepted, nil
}

func (s *memoryStore) put(cell allocator.Cell) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := coordinateKey(cell.Coordinate)
	s.versions[key] = append(s.versions[key], cloneCell(cell))
}

type fixtures struct {
	authority     Authority
	status        OperationDisposition
	policyErr     error
	indexErr      error
	policyPin     bool
	indexPin      bool
	seenPolicyPin coordination.PolicyCopyPin
	outcomes      []CommittedOutcome
}

func (f *fixtures) Current(context.Context, coordination.DomainID) (Authority, error) {
	return f.authority, nil
}
func (f *fixtures) Status(context.Context, coordination.DomainID, []byte) (OperationDisposition, error) {
	return f.status, nil
}
func (f *fixtures) SelectsPolicyCopy(_ context.Context, _ coordination.DomainID, pin coordination.PolicyCopyPin) (bool, error) {
	f.seenPolicyPin = pin
	return f.policyPin, nil
}
func (f *fixtures) SelectsIndexGeneration(context.Context, coordination.DomainID, coordination.Family, coordination.IGEN) (bool, error) {
	return f.indexPin, nil
}
func (f *fixtures) VerifyCopy(context.Context, PolicyCopyProof) error { return f.policyErr }
func (f *fixtures) VerifyMapping(context.Context, coordination.DomainID, coordination.PolicyCopyMapV3) error {
	return f.policyErr
}
func (f *fixtures) AllowPolicyRetirement(context.Context, PolicyCopyProof, coordination.Epoch) error {
	return f.policyErr
}
func (f *fixtures) VerifyDelta(context.Context, coordination.DomainID, coordination.IndexDeltaV1, Authority) error {
	return f.indexErr
}
func (f *fixtures) CommittedOutcomes(context.Context, coordination.DomainID, coordination.Epoch, coordination.Epoch, int) ([]CommittedOutcome, error) {
	return append([]CommittedOutcome(nil), f.outcomes...), f.indexErr
}
func (f *fixtures) VerifyBase(context.Context, coordination.DomainID, coordination.IndexGenerationV2, coordination.Epoch) error {
	return f.indexErr
}
func (f *fixtures) VerifySealing(context.Context, coordination.DomainID, coordination.IndexGenerationV2, []coordination.IndexDeltaV1) error {
	return f.indexErr
}
func (f *fixtures) VerifyActivation(context.Context, coordination.DomainID, coordination.IndexActivationV2, coordination.IndexGenerationV2) error {
	return f.indexErr
}
func (f *fixtures) VerifyLookup(context.Context, coordination.DomainID, coordination.IndexActivationV2, coordination.IndexGenerationV2) error {
	return f.indexErr
}
func (f *fixtures) AllowIndexRetirement(context.Context, coordination.DomainID, coordination.IndexGenerationV2) error {
	return f.indexErr
}

func newTestClient(t *testing.T, store *memoryStore, fixture *fixtures, now *time.Time) *Client {
	t.Helper()
	client, err := New(Config{
		Domain: []byte("domain"), ControlVisibility: []byte("svc"), Store: store,
		Authority: fixture, Operations: fixture, Leases: fixture,
		PolicyVerifier: fixture, IndexVerifier: fixture,
		Clock: func() time.Time { return *now }, MaxRetries: 100, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func digest(value string) coordination.Digest { return coordination.Sum([]byte(value)) }

func fenceRequest(now time.Time, generation coordination.Generation, owner string, fence coordination.Fence) PolicyFenceRequest {
	return PolicyFenceRequest{
		LPART: []byte("lpart"), CopyGeneration: generation, VisibilityDigest: digest("visibility"),
		Owner: []byte(owner), OperationID: []byte("operation-" + owner), LeaseUntil: now.Add(time.Minute),
		Fence: fence, AuthorityGeneration: 3, RetentionGeneration: 4,
	}
}

func manifestSet(t *testing.T, generation coordination.Generation) PolicyManifestSet {
	t.Helper()
	manifest, err := coordination.NewPolicyCopyManifestV1(
		[]byte("lpart"), generation, digest("visibility"), []byte("backend"), []byte("table"),
		[]coordination.PolicyCopyEntry{{
			Table: []byte("table"), RowIdentity: []byte("row"),
			LogicalDigest: digest("logical-row"), PhysicalDigest: digest("physical-row"),
		}}, coordination.CopyStateSealed,
	)
	if err != nil {
		t.Fatal(err)
	}
	return PolicyManifestSet{
		Chunks:        []coordination.PolicyCopyManifestV1{manifest},
		LogicalDigest: digest("logical-set"), PhysicalDigest: digest("physical-set"),
	}
}

func TestPolicyLifecycleAndUnknownReadback(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	fixture := &fixtures{
		authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 20},
		status:    OperationTerminal,
	}
	client := newTestClient(t, store, fixture, &now)
	generation, err := client.ReserveCopyGeneration(context.Background(), []byte("lpart"), []byte("reserve-policy"))
	if err != nil || generation != 1 {
		t.Fatalf("reserve copy generation: %v, %d", err, generation)
	}
	request := fenceRequest(now, generation, "owner", 1)
	store.fault = faultUnknownAfter
	fence, err := client.AcquirePolicyFence(context.Background(), request)
	if err != nil {
		t.Fatalf("unknown-after acquire: %v", err)
	}
	if _, err := client.AcquirePolicyFence(context.Background(), request); err != nil {
		t.Fatalf("idempotent acquire: %v", err)
	}
	renewed := request
	renewed.LeaseUntil = request.LeaseUntil.Add(time.Minute)
	if _, err := client.RenewPolicyFence(context.Background(), renewed); err != nil {
		t.Fatalf("renew: %v", err)
	}
	stale := renewed
	stale.Owner = []byte("stale")
	if _, err := client.RenewPolicyFence(context.Background(), stale); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale renewal error = %v", err)
	}
	set, err := client.WritePolicyManifest(context.Background(), fence, manifestSet(t, generation))
	if err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	mapping := coordination.PolicyCopyMapV3{
		LPART: []byte("lpart"), MapGeneration: 7, CopyGeneration: generation,
		VisibilityDigest: digest("visibility"), CopyDigest: set.PhysicalDigest,
		ActivationKind: coordination.ActivationPolicyRoot, ActivationRef: []byte("root"),
		State: coordination.CopyStateActive,
	}
	fixture.policyErr = ErrConflict
	if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); !errors.Is(err, ErrConflict) {
		t.Fatalf("unverified mapping error = %v", err)
	}
	fixture.policyErr = nil
	if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
		t.Fatalf("publish mapping: %v", err)
	}
	if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
		t.Fatalf("idempotent mapping: %v", err)
	}
	pin, err := client.LookupPolicyCopy(context.Background(), []byte("lpart"), 7)
	if err != nil || pin.Map.CopyGeneration != generation || pin.PinDigest == (coordination.Digest{}) {
		t.Fatalf("lookup pin: %#v, %v", pin, err)
	}
	fixture.policyPin = true
	if err := client.RetirePolicyCopy(context.Background(), fence, set, 19); !errors.Is(err, ErrLeaseActive) {
		t.Fatalf("lease retirement error = %v", err)
	}
	if coordination.ComparePolicyCopyPins(fixture.seenPolicyPin, coordination.PolicyCopyPin{
		LPART: mapping.LPART, MapGeneration: mapping.MapGeneration,
		CopyGeneration: mapping.CopyGeneration, VisibilityDigest: mapping.VisibilityDigest,
	}) != 0 {
		t.Fatalf("retirement queried wrong exact policy pin: %+v", fixture.seenPolicyPin)
	}
	fixture.policyPin = false
	if err := client.RetirePolicyCopy(context.Background(), fence, set, 19); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := client.ReleasePolicyFence(context.Background(), stale); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("stale release error = %v", err)
	}
}

func TestPolicyFenceTakeoverAndGenerationConcurrency(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	fixture := &fixtures{
		authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 20},
		status:    OperationTerminal,
	}

	client := newTestClient(t, store, fixture, &now)
	const count = 32
	values := make(chan coordination.Generation, count)
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			value, err := client.ReserveCopyGeneration(
				context.Background(), []byte("parallel"), []byte{byte(index + 1)},
			)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			values <- value
		}(i)
	}
	group.Wait()
	close(values)
	seen := make(map[coordination.Generation]bool)
	for value := range values {
		seen[value] = true
	}
	if len(seen) != count {
		t.Fatalf("reserved %d unique generations", len(seen))
	}
	first := fenceRequest(now, 1, "first", 1)
	first.LeaseUntil = now.Add(-time.Second)
	if _, err := client.AcquirePolicyFence(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RenewPolicyFence(context.Background(), first); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired renewal error = %v", err)
	}
	second := fenceRequest(now, 1, "second", 2)
	if _, err := client.AcquirePolicyFence(context.Background(), second); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if _, err := client.AcquirePolicyFence(context.Background(), first); !errors.Is(err, ErrBusy) {
		t.Fatalf("stale takeover error = %v", err)
	}
	unknown := fenceRequest(now, 2, "unknown", 1)
	store.fault = faultUnknownBefore
	if _, err := client.AcquirePolicyFence(context.Background(), unknown); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown-before error = %v", err)
	}
}

func TestPolicyPublicationAndRetirementMarkers(t *testing.T) {
	newPolicy := func(t *testing.T) (*memoryStore, *fixtures, *Client, *time.Time, PolicyFence, PolicyManifestSet, coordination.PolicyCopyMapV3) {
		t.Helper()
		now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
		store := newMemoryStore()
		fixture := &fixtures{
			authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 20},
			status:    OperationTerminal,
		}
		client := newTestClient(t, store, fixture, &now)
		request := fenceRequest(now, 1, "owner", 1)
		fence, err := client.AcquirePolicyFence(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		set, err := client.WritePolicyManifest(context.Background(), fence, manifestSet(t, 1))
		if err != nil {
			t.Fatal(err)
		}
		mapping := coordination.PolicyCopyMapV3{
			LPART: []byte("lpart"), MapGeneration: 7, CopyGeneration: 1,
			VisibilityDigest: digest("visibility"), CopyDigest: set.PhysicalDigest,
			ActivationKind: coordination.ActivationPolicyRoot, ActivationRef: []byte("root"),
			State: coordination.CopyStateActive,
		}
		return store, fixture, client, &now, fence, set, mapping
	}

	t.Run("unmarked and stale maps remain unselectable", func(t *testing.T) {
		store, _, client, now, fence, set, mapping := newPolicy(t)
		encoded, err := coordination.MarshalPolicyCopyMapV3(mapping)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := coordination.PolicyCopyMapRow(client.domain, mapping.LPART, mapping.MapGeneration, mapping.VisibilityDigest)
		store.put(allocator.Cell{Coordinate: client.coordinate(row, familyMap, qualifierActive), Value: encoded, Timestamp: int64(mapping.MapGeneration)})
		if _, err := client.LookupPolicyCopy(context.Background(), mapping.LPART, mapping.MapGeneration); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unmarked map lookup = %v", err)
		}
		store.fault = faultUnknownAfter
		store.faultAt = store.mutations + 2
		if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
			t.Fatalf("unknown-after marker publication: %v", err)
		}
		stale := mapping
		stale.MapGeneration++
		stale.ActivationRef = []byte("stale-after-takeover")
		staleData, _ := coordination.MarshalPolicyCopyMapV3(stale)
		staleRow, _ := coordination.PolicyCopyMapRow(client.domain, stale.LPART, stale.MapGeneration, stale.VisibilityDigest)
		store.put(allocator.Cell{Coordinate: client.coordinate(staleRow, familyMap, qualifierActive), Value: staleData, Timestamp: int64(stale.MapGeneration)})
		*now = fence.Request.LeaseUntil
		nextRequest := fenceRequest(*now, 1, "next", 2)
		next, err := client.AcquirePolicyFence(context.Background(), nextRequest)
		if err != nil {
			t.Fatalf("takeover: %v", err)
		}
		if next.publication == nil || next.publication.Map.MapGeneration != mapping.MapGeneration {
			t.Fatalf("takeover lost publication marker: %#v", next.publication)
		}
		if err := client.PublishPolicyMapping(context.Background(), fence, set, stale); !errors.Is(err, ErrStaleOwner) {
			t.Fatalf("former owner published after takeover: %v", err)
		}
		pin, err := client.LookupPolicyCopy(context.Background(), mapping.LPART, stale.MapGeneration)
		if err != nil || pin.Map.MapGeneration != mapping.MapGeneration {
			t.Fatalf("stale map selected: %#v, %v", pin, err)
		}
	})

	t.Run("retirement marker survives crash and takeover", func(t *testing.T) {
		store, fixture, client, now, fence, set, mapping := newPolicy(t)
		if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
			t.Fatal(err)
		}
		store.fault = faultUnknownBefore
		store.faultAt = store.mutations + 2
		if err := client.RetirePolicyCopy(context.Background(), fence, set, 19); !errors.Is(err, ErrUnknown) {
			t.Fatalf("crash after marker = %v", err)
		}
		if _, err := client.LookupPolicyCopy(context.Background(), mapping.LPART, mapping.MapGeneration); !errors.Is(err, ErrNotFound) {
			t.Fatalf("committed retirement remained selectable: %v", err)
		}
		*now = fence.Request.LeaseUntil
		nextRequest := fenceRequest(*now, 1, "next", 2)
		next, err := client.AcquirePolicyFence(context.Background(), nextRequest)
		if err != nil {
			t.Fatal(err)
		}
		if next.retirement == nil {
			t.Fatal("takeover lost retirement authorization")
		}
		fixture.policyPin = true
		if err := client.RetirePolicyCopy(context.Background(), next, set, 19); err != nil {
			t.Fatalf("authorized retirement resume consulted new pins or failed: %v", err)
		}
		row, _ := coordination.PolicyCopyRow(client.domain, next.Request.LPART, next.Request.CopyGeneration, next.Request.VisibilityDigest)
		cell, found, err := client.readOne(context.Background(), client.coordinate(row, familyCopy, qualifierRoot))
		if err != nil || !found {
			t.Fatalf("retired root read: %v", err)
		}
		root, err := unmarshalPolicyRoot(cell.Value)
		if err != nil || root.State != coordination.CopyStateRetired {
			t.Fatalf("root not retired: %#v, %v", root, err)
		}
	})

	t.Run("former owner cannot authorize retirement after takeover", func(t *testing.T) {
		_, _, client, now, fence, set, mapping := newPolicy(t)
		if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
			t.Fatal(err)
		}
		*now = fence.Request.LeaseUntil
		nextRequest := fenceRequest(*now, 1, "next", 2)
		next, err := client.AcquirePolicyFence(context.Background(), nextRequest)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.RetirePolicyCopy(context.Background(), fence, set, 19); !errors.Is(err, ErrStaleOwner) {
			t.Fatalf("former owner retirement = %v", err)
		}
		current, _, _, found, err := client.readFence(context.Background(), next.Request)
		if err != nil || !found {
			t.Fatalf("read takeover fence: %v", err)
		}
		if current.retirement != nil {
			t.Fatal("takeover unexpectedly contains retirement marker")
		}
		pin, err := client.LookupPolicyCopy(context.Background(), mapping.LPART, mapping.MapGeneration)
		if err != nil || pin.Map.MapGeneration != mapping.MapGeneration {
			t.Fatalf("failed retirement changed lookup: %#v, %v", pin, err)
		}
	})

	t.Run("marker codec is versioned bounded and canonical", func(t *testing.T) {
		_, _, _, now, fence, set, mapping := newPolicy(t)
		mapping.CopyDigest = set.PhysicalDigest
		mapData, err := coordination.MarshalPolicyCopyMapV3(mapping)
		if err != nil {
			t.Fatal(err)
		}
		fence.publication = &policyPublicationMarker{Map: mapping, MapDigest: coordination.Sum(mapData)}
		fence.retirement = &policyRetirementMarker{
			Through: 19, PublicationDigest: fence.publication.MapDigest,
			PredecessorRootDigest: digest("root-before"), SuccessorRootDigest: digest("root-after"),
		}
		fence.UpdatedAt = *now
		encoded, err := marshalFence(fence)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := unmarshalFence(encoded)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := marshalFence(decoded)
		if err != nil || !bytes.Equal(canonical, encoded) {
			t.Fatalf("marker codec not canonical: %v", err)
		}
		legacy := cloneFence(fence)
		legacy.publication = nil
		legacy.retirement = nil
		legacyData, err := marshalFence(legacy)
		if err != nil {
			t.Fatal(err)
		}
		encoded[len(legacyData)-sha256.Size] = 2
		rechecksum(encoded)
		if _, err := unmarshalFence(encoded); err == nil {
			t.Fatal("unknown marker extension version accepted")
		}
	})
}

func makeGeneration(t *testing.T, family coordination.Family, igen coordination.IGEN, through coordination.Epoch, delta coordination.Digest, state coordination.IndexGenerationState) coordination.IndexGenerationV2 {
	t.Helper()
	value, err := coordination.NewIndexGenerationV2(coordination.IndexGenerationV2{
		Family: family, IGEN: igen, Schema: []byte("schema"), Buckets: 8,
		SourceEpoch: 10, BuildThrough: 10, DeltaThrough: through,
		PolicyCopyCoverageDigest: digest("coverage"), DeltaDigest: delta, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func makeDelta(t *testing.T, build coordination.IndexGenerationV2, epoch coordination.Epoch, txn string) coordination.IndexDeltaV1 {
	t.Helper()
	value, err := coordination.NewIndexDeltaV1(
		build.Family, build.IGEN, epoch, []byte(txn), build.ManifestDigest,
		[]coordination.IndexDeltaEntry{{
			Kind: []byte("posting"), ID: []byte(txn),
			LogicalDigest: digest("logical-" + txn), PhysicalDigest: digest("physical-" + txn),
		}}, coordination.LifecycleVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestIndexLifecycleGapSealActivationAndRetirement(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	fixture := &fixtures{
		authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 5},
		status:    OperationTerminal,
	}
	client := newTestClient(t, store, fixture, &now)
	igen, err := client.ReserveIndexGeneration(context.Background(), []byte("lexical"), []byte("reserve-index"))
	if err != nil {
		t.Fatal(err)
	}
	buildManifest := makeGeneration(t, []byte("lexical"), igen, 10, digest("empty"), coordination.IndexGenerationBuilding)
	build := IndexBuild{
		Manifest: buildManifest, Owner: []byte("builder"), OperationID: []byte("build-op"),
		Fence: 9, AuthorityGeneration: 3, RetentionGeneration: 4,
	}
	store.fault = faultUnknownAfter
	build, err = client.CreateIndexGeneration(context.Background(), build)
	if err != nil {
		t.Fatalf("create unknown-after: %v", err)
	}
	if _, err := client.CreateIndexGeneration(context.Background(), build); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	delta11 := makeDelta(t, build.Manifest, 11, "txn-11")
	delta12 := makeDelta(t, build.Manifest, 12, "txn-12")
	fixture.indexErr = ErrConflict
	if err := client.AppendIndexDelta(context.Background(), delta11); !errors.Is(err, ErrConflict) {
		t.Fatalf("unverified delta error = %v", err)
	}
	fixture.indexErr = nil
	if err := client.AppendIndexDelta(context.Background(), delta11); err != nil {
		t.Fatal(err)
	}
	if err := client.AppendIndexDelta(context.Background(), delta12); err != nil {
		t.Fatal(err)
	}
	contradictoryDelta := makeDelta(t, build.Manifest, 11, "txn-11")
	contradictoryDelta.Entries[0].PhysicalDigest = digest("different")
	contradictoryDelta, _ = coordination.NewIndexDeltaV1(
		contradictoryDelta.Family, contradictoryDelta.IGEN, contradictoryDelta.Epoch,
		contradictoryDelta.TXN, contradictoryDelta.ManifestDigest,
		contradictoryDelta.Entries, coordination.LifecycleVerified,
	)
	if err := client.AppendIndexDelta(context.Background(), contradictoryDelta); !errors.Is(err, ErrCorruption) {
		t.Fatalf("delta contradiction error = %v", err)
	}
	encoded11, _ := coordination.MarshalIndexDeltaV1(delta11)
	encoded12, _ := coordination.MarshalIndexDeltaV1(delta12)
	deltaDigest := digestParts([]byte("index-delta-set-v1"), encoded11, encoded12)
	sealed := makeGeneration(t, []byte("lexical"), igen, 12, deltaDigest, coordination.IndexGenerationSealed)
	fixture.outcomes = []CommittedOutcome{{Epoch: 11, TXN: []byte("txn-11")}}
	if _, err := client.SealIndexGeneration(context.Background(), []byte("builder"), []byte("build-op"), 9, sealed); !errors.Is(err, ErrCorruption) {
		t.Fatalf("missing outcome error = %v", err)
	}
	fixture.outcomes = append(fixture.outcomes, CommittedOutcome{Epoch: 12, TXN: []byte("txn-12")})
	sealedBuild, err := client.SealIndexGeneration(context.Background(), []byte("builder"), []byte("build-op"), 9, sealed)
	if err != nil || sealedBuild.Manifest.State != coordination.IndexGenerationSealed {
		t.Fatalf("seal: %#v, %v", sealedBuild, err)
	}
	activation := coordination.IndexActivationV2{
		Family: []byte("lexical"), IGEN: igen, ActivationEpoch: 13, SourceFrontier: 12,
		ManifestDigest: sealed.ManifestDigest, DeltaDigest: sealed.DeltaDigest, TXN: []byte("activation"),
		Fence: 9, AuthorityGeneration: 3, State: coordination.LifecycleActive,
	}
	store.fault = faultUnknownAfter
	if err := client.PublishIndexActivation(context.Background(), activation); err != nil {
		t.Fatalf("activation unknown-after: %v", err)
	}
	contradictoryActivation := activation
	contradictoryActivation.TXN = []byte("different")
	if err := client.PublishIndexActivation(context.Background(), contradictoryActivation); !errors.Is(err, ErrCorruption) {
		t.Fatalf("activation contradiction error = %v", err)
	}
	for epoch := coordination.Epoch(14); epoch < 20; epoch++ {
		newer := activation
		newer.ActivationEpoch = epoch
		newer.TXN = coordination.TXN([]byte{byte(epoch)})
		data, encodeErr := coordination.MarshalIndexActivationV2(newer)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		row, _ := coordination.IndexActivationRow(client.domain, newer.Family, newer.ActivationEpoch, newer.IGEN)
		store.put(allocator.Cell{
			Coordinate: client.coordinate(row, familyActivation, qualifierActive),
			Value:      data, Timestamp: int64(epoch),
		})
	}
	client.maxScan = 2
	pin, err := client.LookupIndexGeneration(context.Background(), []byte("lexical"), 13)
	if err != nil || !bytes.Equal(pin.Manifest.IGEN, igen) || pin.PinDigest == (coordination.Digest{}) {
		t.Fatalf("lookup: %#v, %v", pin, err)
	}
	fixture.indexPin = true
	if err := client.RetireIndexGeneration(context.Background(), []byte("lexical"), igen); !errors.Is(err, ErrLeaseActive) {
		t.Fatalf("lease retirement error = %v", err)
	}
	fixture.indexPin = false
	if err := client.RetireIndexGeneration(context.Background(), []byte("lexical"), igen); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := client.LookupIndexGeneration(context.Background(), []byte("lexical"), 13); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retired lookup error = %v", err)
	}
}

func TestAppendOnlyContradictions(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	fixture := &fixtures{authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 5}}
	client := newTestClient(t, store, fixture, &now)
	generation, _ := client.ReserveCopyGeneration(context.Background(), []byte("lpart"), []byte("reserve-policy"))
	request := fenceRequest(now, generation, "owner", 1)
	fence, _ := client.AcquirePolicyFence(context.Background(), request)
	set, _ := client.WritePolicyManifest(context.Background(), fence, manifestSet(t, generation))
	mapping := coordination.PolicyCopyMapV3{
		LPART: []byte("lpart"), MapGeneration: 1, CopyGeneration: generation,
		VisibilityDigest: digest("visibility"), CopyDigest: set.PhysicalDigest,
		ActivationKind: coordination.ActivationPolicyRoot, ActivationRef: []byte("root"),
		State: coordination.CopyStateActive,
	}
	if err := client.PublishPolicyMapping(context.Background(), fence, set, mapping); err != nil {
		t.Fatal(err)
	}
	for generation := coordination.Generation(2); generation < 8; generation++ {
		newer := mapping
		newer.MapGeneration = generation
		data, encodeErr := coordination.MarshalPolicyCopyMapV3(newer)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		row, _ := coordination.PolicyCopyMapRow(client.domain, newer.LPART, generation, newer.VisibilityDigest)
		store.put(allocator.Cell{
			Coordinate: client.coordinate(row, familyMap, qualifierActive),
			Value:      data, Timestamp: int64(generation),
		})
	}
	client.maxScan = 2
	if pin, err := client.LookupPolicyCopy(context.Background(), []byte("lpart"), 1); err != nil ||
		pin.Map.MapGeneration != 1 {
		t.Fatalf("deep policy lookup = %#v, %v", pin, err)
	}
	contradiction := mapping
	contradiction.ActivationRef = []byte("other")
	if err := client.PublishPolicyMapping(context.Background(), fence, set, contradiction); !errors.Is(err, ErrCorruption) {
		t.Fatalf("mapping contradiction error = %v", err)
	}
}

func TestIndexSealFoldsPreBuildDeltasIntoBase(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	fixture := &fixtures{
		authority: Authority{Generation: 3, RetentionGeneration: 4, HistoryFloor: 5},
		status:    OperationTerminal,
	}
	client := newTestClient(t, store, fixture, &now)
	igen, err := client.ReserveIndexGeneration(context.Background(), []byte("tree"), []byte("reserve-tree"))
	if err != nil {
		t.Fatal(err)
	}
	buildManifest := makeGeneration(t, []byte("tree"), igen, 10, digest("empty"), coordination.IndexGenerationBuilding)
	build, err := client.CreateIndexGeneration(context.Background(), IndexBuild{
		Manifest: buildManifest, Owner: []byte("builder"), OperationID: []byte("tree-build"),
		Fence: 10, AuthorityGeneration: 3, RetentionGeneration: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	deltas := make([]coordination.IndexDeltaV1, 0, 4)
	for epoch := coordination.Epoch(11); epoch <= 14; epoch++ {
		delta := makeDelta(t, build.Manifest, epoch, string([]byte{'t', byte(epoch)}))
		if err := client.AppendIndexDelta(context.Background(), delta); err != nil {
			t.Fatal(err)
		}
		deltas = append(deltas, delta)
	}
	badPreBuild, err := coordination.NewIndexDeltaV1(
		build.Manifest.Family, build.Manifest.IGEN, 11, deltas[0].TXN, digest("wrong-manifest"),
		deltas[0].Entries, coordination.LifecycleVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	badBytes, _ := coordination.MarshalIndexDeltaV1(badPreBuild)
	badRow, _ := coordination.IndexDeltaRow(
		client.domain, badPreBuild.Family, badPreBuild.IGEN, badPreBuild.Epoch, badPreBuild.TXN,
	)
	badCoordinate := client.coordinate(badRow, familyDelta, qualifierDelta)
	store.put(allocator.Cell{Coordinate: badCoordinate, Value: badBytes, Timestamp: 11})
	postBuild13, _ := coordination.MarshalIndexDeltaV1(deltas[2])
	postBuild14, _ := coordination.MarshalIndexDeltaV1(deltas[3])
	deltaDigest := digestParts([]byte("index-delta-set-v1"), postBuild13, postBuild14)
	sealed := makeGeneration(t, []byte("tree"), igen, 14, deltaDigest, coordination.IndexGenerationSealed)
	sealed.BuildThrough = 12
	sealed, err = coordination.NewIndexGenerationV2(sealed)
	if err != nil {
		t.Fatal(err)
	}
	fixture.outcomes = []CommittedOutcome{
		{Epoch: 13, TXN: deltas[2].TXN},
		{Epoch: 14, TXN: deltas[3].TXN},
	}
	if _, err := client.SealIndexGeneration(
		context.Background(), []byte("builder"), []byte("tree-build"), 10, sealed,
	); !errors.Is(err, ErrCorruption) {
		t.Fatalf("malformed pre-build delta error = %v", err)
	}
	goodBytes, _ := coordination.MarshalIndexDeltaV1(deltas[0])
	store.put(allocator.Cell{Coordinate: badCoordinate, Value: goodBytes, Timestamp: 11})
	if _, err := client.SealIndexGeneration(
		context.Background(), []byte("builder"), []byte("tree-build"), 10, sealed,
	); err != nil {
		t.Fatalf("seal with pre-build deltas: %v", err)
	}
}
