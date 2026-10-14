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

package coordination

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"
)

var fixtureTime = time.Date(2026, 8, 27, 14, 17, 9, 780000000, time.UTC)

func fixtureGuard() EntityGuardV1 {
	return EntityGuardV1{
		EntityKind: EntityKind("document"), EntityID: EntityID{0, 0xff, 'd'},
		TXN: TXN("txn"), Owner: OwnerID("owner"), LeaseUntil: fixtureTime.Add(time.Hour),
		Fence: 4, AuthorityGeneration: 7, DesiredDigest: testDigest("desired"),
		PreviousDigest: testDigest("previous"), PreviousVersion: 3, State: GuardStateHeld,
	}
}

func fixturePending() PendingMutationV1 {
	return PendingMutationV1{
		EntityKind: EntityKind("document"), EntityID: EntityID{0, 0xff, 'd'},
		TXN: TXN("txn"), ManifestChunk: 2, ManifestEntry: 3, Ordinal: 4,
		LogicalDigest: testDigest("logical"), PhysicalDigest: testDigest("physical"),
	}
}

func fixturePolicyManifest(t *testing.T) PolicyCopyManifestV1 {
	t.Helper()
	value, err := NewPolicyCopyManifestV1(
		LPART{0, 0xff, 'p'}, 4, testDigest("visibility"), BackendID("accumulo"),
		[]byte("objects"), []PolicyCopyEntry{
			{Table: []byte("objects"), RowIdentity: []byte{'z'}, LogicalDigest: testDigest("lz"), PhysicalDigest: testDigest("pz")},
			{Table: []byte("objects"), RowIdentity: []byte{0, 0xff}, LogicalDigest: testDigest("l0"), PhysicalDigest: testDigest("p0")},
		}, CopyStateSealed,
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixturePolicyMap() PolicyCopyMapV3 {
	return PolicyCopyMapV3{
		LPART: LPART{0, 0xff, 'p'}, MapGeneration: 8, CopyGeneration: 4,
		VisibilityDigest: testDigest("visibility"), CopyDigest: testDigest("copy"),
		ActivationKind: ActivationPolicyRoot, ActivationRef: []byte{0, 0xff, 'r'},
		State: CopyStateActive,
	}
}

func fixturePolicyFence() PolicyCopyFenceV1 {
	return PolicyCopyFenceV1{
		LPART: LPART{0, 0xff, 'p'}, CopyGeneration: 4, Owner: OwnerID("owner"),
		LeaseUntil: fixtureTime.Add(time.Hour), Fence: 5, AuthorityGeneration: 7,
		State: GuardStateHeld,
	}
}

func fixtureIndexGeneration() IndexGenerationV2 {
	value := IndexGenerationV2{
		Family: Family{0, 0xff, 'f'}, IGEN: IGEN("igen"), Schema: []byte("schema-v1"),
		Buckets: 32, SourceEpoch: 10, BuildThrough: 12, DeltaThrough: 14,
		PolicyCopyCoverageDigest: testDigest("coverage"), DeltaDigest: testDigest("deltas"),
		State: IndexGenerationSealed,
	}
	value.ManifestDigest = value.ComputeDigest()
	return value
}

func fixtureIndexDelta(t *testing.T) IndexDeltaV1 {
	t.Helper()
	value, err := NewIndexDeltaV1(
		Family{0, 0xff, 'f'}, IGEN("igen"), 13, TXN("txn"),
		fixtureIndexGeneration().ManifestDigest,
		[]IndexDeltaEntry{
			{Kind: []byte("row"), ID: []byte{'z'}, LogicalDigest: testDigest("lz"), PhysicalDigest: testDigest("pz")},
			{Kind: []byte("row"), ID: []byte{0, 0xff}, LogicalDigest: testDigest("l0"), PhysicalDigest: testDigest("p0")},
		}, LifecycleVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fixtureIndexActivation() IndexActivationV2 {
	return IndexActivationV2{
		Family: Family{0, 0xff, 'f'}, IGEN: IGEN("igen"), ActivationEpoch: 16,
		SourceFrontier: 14, ManifestDigest: fixtureIndexGeneration().ManifestDigest,
		DeltaDigest: testDigest("deltas"), TXN: TXN("activate"), Fence: 6,
		AuthorityGeneration: 7, State: LifecycleActive,
	}
}

func fixtureLease() SnapshotLeaseV3 {
	policyPins := []PolicyCopyPin{
		{LPART: LPART{0, 0xff, 'p'}, MapGeneration: 8, CopyGeneration: 7, VisibilityDigest: testDigest("visibility-1")},
		{LPART: LPART("tree"), MapGeneration: 9, CopyGeneration: 6, VisibilityDigest: testDigest("visibility-2")},
	}
	return SnapshotLeaseV3{
		LeaseID: LeaseID{0, 0xff, 'l'}, Frontier: 17, Owner: OwnerID("reader"),
		Fence: 3, AuthorityGeneration: 7, RetentionGeneration: 8, PolicyGeneration: 9,
		PolicyCopyPinDigest: PolicyCopyPinDigest(policyPins), PolicyCopyPins: policyPins,
		IndexPins: []IndexPin{
			{Family: Family{0, 0xff, 'f'}, IGEN: IGEN("i1")},
			{Family: Family("tree"), IGEN: IGEN("t1")},
		},
		CreatedAt: fixtureTime, RenewedAt: fixtureTime.Add(time.Minute),
		ExpiresAt: fixtureTime.Add(time.Hour), State: LeaseStateActive,
	}
}

func fixtureRetirement() RetirementDecisionV1 {
	return RetirementDecisionV1{
		ObjectKind: EntityKind("revision"), ObjectID: EntityID{0, 0xff, 'r'},
		ObjectGeneration: 3, SafeAfterFrontier: 20, SafeAfterTime: fixtureTime.Add(time.Hour),
		HistoryFloor: 18, ProofDigest: testDigest("proof"), AuthorityGeneration: 7,
		State: RetirementCandidate,
	}
}

func fixtureFloor() HistoryFloorV1 {
	value := HistoryFloorV1{
		Floor: 18, RetentionGeneration: 8, AdvancedAt: fixtureTime,
		PredecessorDigest: testDigest("floor-previous"),
	}
	value.Digest = value.ComputeDigest()
	return value
}

func fixtureAuthority() WriterAuthorityV1 {
	value := WriterAuthorityV1{
		Term: AuthorityTerm{0, 0xff, 't'}, Generation: 7, Owner: OwnerID("writer"),
		LeaseUntil: fixtureTime.Add(time.Hour), Fence: 11, State: AuthorityActive,
		PredecessorDigest: testDigest("authority-previous"),
	}
	value.Digest = value.ComputeDigest()
	return value
}

func fixtureObservation() BackendObservationV1 {
	return BackendObservationV1{
		Backend: BackendID{0, 0xff, 'b'}, AuthorityGeneration: 7, AuthorityFence: 11,
		ObservedFrontier: 17, State: BackendPrimary, ObservedDigest: testDigest("observed"),
		ObservedAt: fixtureTime,
	}
}

func TestM3RecordGoldensRoundTripsAndEnvelopeRejection(t *testing.T) {
	tests := []struct {
		name      string
		kind      Kind
		marshal   func() ([]byte, error)
		unmarshal func([]byte) (any, error)
		want      any
	}{
		{"entity_guard_v1.bin", KindEntityGuard, func() ([]byte, error) { return MarshalEntityGuardV1(fixtureGuard()) }, func(b []byte) (any, error) { return UnmarshalEntityGuardV1(b) }, fixtureGuard()},
		{"pending_mutation_v1.bin", KindPendingMutation, func() ([]byte, error) { return MarshalPendingMutationV1(fixturePending()) }, func(b []byte) (any, error) { return UnmarshalPendingMutationV1(b) }, fixturePending()},
		{"policy_copy_manifest_v1.bin", KindPolicyCopyManifest, func() ([]byte, error) { return MarshalPolicyCopyManifestV1(fixturePolicyManifest(t)) }, func(b []byte) (any, error) { return UnmarshalPolicyCopyManifestV1(b) }, fixturePolicyManifest(t)},
		{"policy_copy_map_v3.bin", KindPolicyCopyMap, func() ([]byte, error) { return MarshalPolicyCopyMapV3(fixturePolicyMap()) }, func(b []byte) (any, error) { return UnmarshalPolicyCopyMapV3(b) }, fixturePolicyMap()},
		{"policy_copy_fence_v1.bin", KindPolicyCopyFence, func() ([]byte, error) { return MarshalPolicyCopyFenceV1(fixturePolicyFence()) }, func(b []byte) (any, error) { return UnmarshalPolicyCopyFenceV1(b) }, fixturePolicyFence()},
		{"index_generation_v2.bin", KindIndexGeneration, func() ([]byte, error) { return MarshalIndexGenerationV2(fixtureIndexGeneration()) }, func(b []byte) (any, error) { return UnmarshalIndexGenerationV2(b) }, fixtureIndexGeneration()},
		{"index_delta_v1.bin", KindIndexDelta, func() ([]byte, error) { return MarshalIndexDeltaV1(fixtureIndexDelta(t)) }, func(b []byte) (any, error) { return UnmarshalIndexDeltaV1(b) }, fixtureIndexDelta(t)},
		{"index_activation_v2.bin", KindIndexActivation, func() ([]byte, error) { return MarshalIndexActivationV2(fixtureIndexActivation()) }, func(b []byte) (any, error) { return UnmarshalIndexActivationV2(b) }, fixtureIndexActivation()},
		{"snapshot_lease_v3.bin", KindSnapshotLease, func() ([]byte, error) { return MarshalSnapshotLeaseV3(fixtureLease()) }, func(b []byte) (any, error) { return UnmarshalSnapshotLeaseV3(b) }, fixtureLease()},
		{"retirement_decision_v1.bin", KindRetirementDecision, func() ([]byte, error) { return MarshalRetirementDecisionV1(fixtureRetirement()) }, func(b []byte) (any, error) { return UnmarshalRetirementDecisionV1(b) }, fixtureRetirement()},
		{"history_floor_v1.bin", KindHistoryFloor, func() ([]byte, error) { return MarshalHistoryFloorV1(fixtureFloor()) }, func(b []byte) (any, error) { return UnmarshalHistoryFloorV1(b) }, fixtureFloor()},
		{"writer_authority_v1.bin", KindWriterAuthority, func() ([]byte, error) { return MarshalWriterAuthorityV1(fixtureAuthority()) }, func(b []byte) (any, error) { return UnmarshalWriterAuthorityV1(b) }, fixtureAuthority()},
		{"backend_observation_v1.bin", KindBackendObservation, func() ([]byte, error) { return MarshalBackendObservationV1(fixtureObservation()) }, func(b []byte) (any, error) { return UnmarshalBackendObservationV1(b) }, fixtureObservation()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := test.marshal()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := test.unmarshal(golden(t, test.name, encoded))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.want) {
				t.Fatalf("round trip differs:\ngot  %#v\nwant %#v", decoded, test.want)
			}
			for name, corrupt := range map[string][]byte{
				"truncated": encoded[:len(encoded)-1],
				"trailing":  append(append([]byte(nil), encoded...), 0),
				"checksum":  corruptByte(encoded, envelopeHeaderSize),
				"kind":      corruptHeader(encoded, len(envelopeMagic), uint16(test.kind+1)),
				"version":   corruptHeader(encoded, len(envelopeMagic)+2, 99),
			} {
				if _, err := test.unmarshal(corrupt); err == nil {
					t.Fatalf("%s accepted", name)
				}
			}
		})
	}
}

func corruptByte(value []byte, offset int) []byte {
	result := append([]byte(nil), value...)
	result[offset] ^= 1
	return result
}

func corruptHeader(value []byte, offset int, replacement uint16) []byte {
	result := append([]byte(nil), value...)
	binary.BigEndian.PutUint16(result[offset:], replacement)
	return result
}

func TestM3TransitionAndMonotonicityValidation(t *testing.T) {
	guard := fixtureGuard()
	renewed := guard
	renewed.LeaseUntil = renewed.LeaseUntil.Add(time.Minute)
	if err := ValidateGuardTransition(guard, renewed); err != nil {
		t.Fatal(err)
	}
	renewed.LeaseUntil = guard.LeaseUntil.Add(-time.Nanosecond)
	if ValidateGuardTransition(guard, renewed) == nil {
		t.Fatal("shortened guard lease accepted")
	}

	manifest := fixturePolicyManifest(t)
	building := manifest
	building.State = CopyStateBuilding
	active := manifest
	active.State = CopyStateActive
	if ValidatePolicyCopyTransition(building, active) == nil {
		t.Fatal("BUILDING to ACTIVE transition accepted")
	}
	sealed := manifest
	sealed.State = CopyStateSealed
	if err := ValidatePolicyCopyTransition(sealed, active); err != nil {
		t.Fatal(err)
	}
	nextMap := fixturePolicyMap()
	nextMap.MapGeneration++
	if err := ValidatePolicyCopyMapSuccessor(fixturePolicyMap(), nextMap); err != nil {
		t.Fatal(err)
	}
	nextMap.MapGeneration--
	if ValidatePolicyCopyMapSuccessor(fixturePolicyMap(), nextMap) == nil {
		t.Fatal("duplicate map generation accepted")
	}

	sealedGeneration := fixtureIndexGeneration()
	buildingGeneration := sealedGeneration
	buildingGeneration.State = IndexGenerationBuilding
	buildingGeneration.BuildThrough--
	buildingGeneration.DeltaThrough--
	buildingGeneration.ManifestDigest = buildingGeneration.ComputeDigest()
	if err := ValidateIndexGenerationTransition(buildingGeneration, sealedGeneration); err != nil {
		t.Fatal(err)
	}
	progressedGeneration := buildingGeneration
	progressedGeneration.BuildThrough++
	progressedGeneration.DeltaThrough++
	progressedGeneration.ManifestDigest = progressedGeneration.ComputeDigest()
	if err := ValidateIndexGenerationTransition(buildingGeneration, progressedGeneration); err != nil {
		t.Fatal(err)
	}

	lease := fixtureLease()
	activeAt, err := lease.ActiveAt(fixtureTime.Add(30 * time.Minute))
	if err != nil || !activeAt {
		t.Fatalf("active lease rejected: %v", err)
	}
	activeAt, err = lease.ActiveAt(lease.ExpiresAt)
	if err != nil || activeAt {
		t.Fatalf("expired lease accepted: %v", err)
	}
	renewedLease := lease
	renewedLease.RenewedAt = renewedLease.RenewedAt.Add(time.Minute)
	renewedLease.ExpiresAt = renewedLease.ExpiresAt.Add(time.Minute)
	if err := ValidateSnapshotLeaseTransition(lease, renewedLease); err != nil {
		t.Fatal(err)
	}

	floor := fixtureFloor()
	nextFloor := HistoryFloorV1{
		Floor: floor.Floor + 1, RetentionGeneration: floor.RetentionGeneration + 1,
		AdvancedAt: floor.AdvancedAt.Add(time.Second), PredecessorDigest: floor.Digest,
	}
	nextFloor.Digest = nextFloor.ComputeDigest()
	if err := ValidateHistoryFloorAdvance(floor, nextFloor); err != nil {
		t.Fatal(err)
	}

	authority := fixtureAuthority()
	renewedAuthority := authority
	renewedAuthority.LeaseUntil = authority.LeaseUntil.Add(time.Minute)
	renewedAuthority.Digest = renewedAuthority.ComputeDigest()
	if err := ValidateWriterAuthorityTransition(authority, renewedAuthority); err != nil {
		t.Fatal(err)
	}
	successor := WriterAuthorityV1{
		Term: AuthorityTerm("next"), Generation: authority.Generation + 1,
		Owner: OwnerID("next-owner"), LeaseUntil: authority.LeaseUntil.Add(time.Hour),
		Fence: authority.Fence + 1, State: AuthorityActive, PredecessorDigest: authority.Digest,
	}
	successor.Digest = successor.ComputeDigest()
	if err := ValidateWriterAuthorityAcquisition(&authority, successor); err != nil {
		t.Fatal(err)
	}

	observation := fixtureObservation()
	nextObservation := observation
	nextObservation.ObservedFrontier++
	nextObservation.ObservedAt = nextObservation.ObservedAt.Add(time.Second)
	if err := ValidateBackendObservationSuccessor(observation, nextObservation); err != nil {
		t.Fatal(err)
	}
	nextObservation.ObservedFrontier = observation.ObservedFrontier - 1
	if ValidateBackendObservationSuccessor(observation, nextObservation) == nil {
		t.Fatal("decreasing backend frontier accepted")
	}
	if !observation.RejectsAuthority(observation.AuthorityGeneration-1, observation.AuthorityFence) {
		t.Fatal("stale authority generation was not rejected")
	}

}

func TestRetirementApprovedToAppliedRejectsStaleAuthorityGeneration(t *testing.T) {
	approved := fixtureRetirement()
	approved.State = RetirementApproved
	applied := approved
	applied.State = RetirementApplied
	if err := ValidateRetirementTransition(approved, applied); err != nil {
		t.Fatal(err)
	}
	applied.AuthorityGeneration--
	if ValidateRetirementTransition(approved, applied) == nil {
		t.Fatal("stale APPROVED to APPLIED authority generation accepted")
	}
}

func TestM3CanonicalOrderingDuplicateAndForgedCountRejection(t *testing.T) {
	manifest := fixturePolicyManifest(t)
	manifest.Entries[0], manifest.Entries[1] = manifest.Entries[1], manifest.Entries[0]
	reordered, err := MarshalPolicyCopyManifestV1(manifest)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := MarshalPolicyCopyManifestV1(fixturePolicyManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reordered, canonical) {
		t.Fatal("policy-copy insertion order affected bytes")
	}
	manifest = fixturePolicyManifest(t)
	manifest.Entries[1] = manifest.Entries[0]
	manifest.ManifestDigest = policyCopyEntriesDigest(manifest.Entries)
	if _, err := MarshalPolicyCopyManifestV1(manifest); err == nil {
		t.Fatal("duplicate policy-copy identity accepted")
	}

	delta := fixtureIndexDelta(t)
	delta.Entries[1] = delta.Entries[0]
	delta.DeltaDigest = indexDeltaEntriesDigest(delta.Entries)
	if _, err := MarshalIndexDeltaV1(delta); err == nil {
		t.Fatal("duplicate index-delta identity accepted")
	}

	encoded, err := MarshalIndexDeltaV1(fixtureIndexDelta(t))
	if err != nil {
		t.Fatal(err)
	}
	payload := encoded[envelopeHeaderSize : len(encoded)-checksumSize]
	d := &decoder{data: payload}
	_ = d.bytes("family", MaxOpaqueIDBytes, true)
	_ = d.bytes("IGEN", MaxOpaqueIDBytes, true)
	_ = d.take("epoch", 8)
	_ = d.bytes("txn", MaxOpaqueIDBytes, true)
	_ = d.take("manifest", 32)
	forged := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(forged[envelopeHeaderSize+d.offset:], MaxIndexDeltaEntries+1)
	sum := Sum(forged[:len(forged)-checksumSize])
	copy(forged[len(forged)-checksumSize:], sum[:])
	if _, err := UnmarshalIndexDeltaV1(forged); err == nil {
		t.Fatal("forged index-delta count accepted")
	}
}

func TestM3BoundsEnumsAbsenceAndOverflow(t *testing.T) {
	guard := fixtureGuard()
	guard.State = GuardState(255)
	if _, err := MarshalEntityGuardV1(guard); err == nil {
		t.Fatal("unknown guard state accepted")
	}
	pending := fixturePending()
	pending.EntityID = make([]byte, MaxOpaqueIDBytes+1)
	if _, err := MarshalPendingMutationV1(pending); err == nil {
		t.Fatal("oversized pending identity accepted")
	}
	manifest := fixturePolicyManifest(t)
	manifest.State = CopyState(255)
	if _, err := MarshalPolicyCopyManifestV1(manifest); err == nil {
		t.Fatal("unknown policy-copy state accepted")
	}
	delta := fixtureIndexDelta(t)
	delta.State = LifecycleState(255)
	if _, err := MarshalIndexDeltaV1(delta); err == nil {
		t.Fatal("unknown index state accepted")
	}
	lease := fixtureLease()
	lease.CreatedAt = time.Time{}
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("missing lease created_at accepted")
	}
	lease = fixtureLease()
	lease.PolicyCopyPinDigest = testDigest("tampered")
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("tampered policy-copy pin digest accepted")
	}
	lease = fixtureLease()
	lease.PolicyCopyPins = make([]PolicyCopyPin, MaxPolicyCopyPins+1)
	for index := range lease.PolicyCopyPins {
		lease.PolicyCopyPins[index] = PolicyCopyPin{
			LPART: LPART("part"), MapGeneration: Generation(index + 1), CopyGeneration: 1,
			VisibilityDigest: testDigest("visibility"),
		}
	}
	lease.PolicyCopyPinDigest = PolicyCopyPinDigest(lease.PolicyCopyPins)
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("oversized policy-copy pin list accepted")
	}
	lease = fixtureLease()
	lease.PolicyCopyPins = append(lease.PolicyCopyPins, lease.PolicyCopyPins[0])
	lease.PolicyCopyPinDigest = PolicyCopyPinDigest(lease.PolicyCopyPins)
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("duplicate policy-copy pin accepted")
	}
	lease = fixtureLease()
	lease.IndexPins = make([]IndexPin, MaxIndexPins+1)
	for index := range lease.IndexPins {
		lease.IndexPins[index] = IndexPin{Family: Family("family"), IGEN: IGEN(string(rune(index + 1)))}
	}
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("oversized index pin list accepted")
	}
	lease = fixtureLease()
	lease.IndexPins = append(lease.IndexPins, lease.IndexPins[0])
	if _, err := MarshalSnapshotLeaseV3(lease); err == nil {
		t.Fatal("duplicate index pin accepted")
	}
	authority := fixtureAuthority()
	authority.State = AuthorityState(255)
	authority.Digest = authority.ComputeDigest()
	if _, err := MarshalWriterAuthorityV1(authority); err == nil {
		t.Fatal("unknown authority state accepted")
	}
	if _, err := PolicyGenerationRow(DomainID("domain"), Generation(-1)); err == nil {
		t.Fatal("overflowed generation accepted")
	}
}

func TestSnapshotLeaseCanonicalPinSortingAndImmutability(t *testing.T) {
	lease := fixtureLease()
	lease.PolicyCopyPins[0], lease.PolicyCopyPins[1] = lease.PolicyCopyPins[1], lease.PolicyCopyPins[0]
	lease.IndexPins[0], lease.IndexPins[1] = lease.IndexPins[1], lease.IndexPins[0]
	encoded, err := MarshalSnapshotLeaseV3(lease)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalSnapshotLeaseV3(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ComparePolicyCopyPins(decoded.PolicyCopyPins[0], decoded.PolicyCopyPins[1]) >= 0 ||
		CompareIndexPins(decoded.IndexPins[0], decoded.IndexPins[1]) >= 0 {
		t.Fatal("snapshot lease pins were not canonically sorted")
	}
	reencoded, err := MarshalSnapshotLeaseV3(decoded)
	if err != nil || !bytes.Equal(encoded, reencoded) {
		t.Fatalf("canonical fixed point failed: %v", err)
	}
	next := decoded
	next.ExpiresAt = next.ExpiresAt.Add(time.Minute)
	next.RenewedAt = next.RenewedAt.Add(time.Minute)
	next.PolicyCopyPins = append([]PolicyCopyPin(nil), next.PolicyCopyPins...)
	next.PolicyCopyPins[0].CopyGeneration++
	next.PolicyCopyPinDigest = PolicyCopyPinDigest(next.PolicyCopyPins)
	if ValidateSnapshotLeaseTransition(decoded, next) == nil {
		t.Fatal("changed policy-copy pins accepted across lease transition")
	}
	next = decoded
	next.ExpiresAt = next.ExpiresAt.Add(time.Minute)
	next.RenewedAt = next.RenewedAt.Add(time.Minute)
	next.IndexPins = append([]IndexPin(nil), next.IndexPins...)
	next.IndexPins[0].IGEN = IGEN("changed")
	SortIndexPins(next.IndexPins)
	if ValidateSnapshotLeaseTransition(decoded, next) == nil {
		t.Fatal("changed index pins accepted across lease transition")
	}
}

func TestSnapshotLeaseRejectsTamperedPersistedPinCommitment(t *testing.T) {
	encoded, err := MarshalSnapshotLeaseV3(fixtureLease())
	if err != nil {
		t.Fatal(err)
	}
	payload := encoded[envelopeHeaderSize : len(encoded)-checksumSize]
	d := decoder{data: payload}
	_ = d.bytes("lease ID", MaxOpaqueIDBytes, true)
	_ = d.positive("frontier")
	_ = d.bytes("owner", MaxOwnerBytes, true)
	for index := 0; index < 4; index++ {
		_ = d.positive("generation")
	}
	if d.err != nil {
		t.Fatal(d.err)
	}
	encoded[envelopeHeaderSize+d.offset] ^= 0xff
	sum := Sum(encoded[:len(encoded)-checksumSize])
	copy(encoded[len(encoded)-checksumSize:], sum[:])
	if _, err := UnmarshalSnapshotLeaseV3(encoded); err == nil {
		t.Fatal("tampered persisted policy-copy pin digest accepted")
	}
}

func TestIndexGenerationManifestLifecycle(t *testing.T) {
	for _, state := range []IndexGenerationState{
		IndexGenerationState(LifecycleActive),
		IndexGenerationState(LifecycleRetired),
	} {
		value := fixtureIndexGeneration()
		value.State = state
		if _, err := MarshalIndexGenerationV2(value); err == nil {
			t.Fatalf("manifest state %d accepted", state)
		}
		encoded, err := MarshalIndexGenerationV2(fixtureIndexGeneration())
		if err != nil {
			t.Fatal(err)
		}
		encoded[len(encoded)-checksumSize-1] = byte(state)
		sum := Sum(encoded[:len(encoded)-checksumSize])
		copy(encoded[len(encoded)-checksumSize:], sum[:])
		if _, err := UnmarshalIndexGenerationV2(encoded); err == nil {
			t.Fatalf("encoded manifest state %d accepted", state)
		}
	}

	sealed := fixtureIndexGeneration()
	changedFrontier := sealed
	changedFrontier.DeltaThrough++
	changedFrontier.ManifestDigest = changedFrontier.ComputeDigest()
	if ValidateIndexGenerationTransition(sealed, changedFrontier) == nil {
		t.Fatal("post-seal frontier change accepted")
	}

	changedDigest := sealed
	changedDigest.DeltaDigest = testDigest("different delta digest")
	changedDigest.ManifestDigest = changedDigest.ComputeDigest()
	if ValidateIndexGenerationTransition(sealed, changedDigest) == nil {
		t.Fatal("post-seal digest change accepted")
	}

	poisoned := sealed
	poisoned.State = IndexGenerationPoisoned
	if err := ValidateIndexGenerationTransition(sealed, poisoned); err != nil {
		t.Fatal(err)
	}
	if poisoned.ManifestDigest != sealed.ManifestDigest {
		t.Fatal("poison transition changed sealed manifest digest")
	}

	active := fixtureIndexActivation()
	if active.State != LifecycleActive {
		t.Fatal("activation record is not the active-generation representation")
	}
}
