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
	"reflect"
	"testing"
)

func TestM3CoordinationRowGoldensAndRoundTrips(t *testing.T) {
	domain := DomainID{0, 0xff, 'd'}
	lpart := LPART{'p', 0, 0xff}
	family := Family{'f', 0, 0xff}
	igen := IGEN{0, 0xff, 'i'}
	visibility := testDigest("visibility")
	tests := []struct {
		name  string
		make  func() ([]byte, error)
		parse func([]byte) (any, error)
		want  any
	}{
		{"entity_head_row_v1.bin", func() ([]byte, error) { return EntityHeadRow(domain, 1, EntityID{0, 0xff, 'e'}) }, func(b []byte) (any, error) { return ParseEntityHeadRow(b) }, EntityHeadKey{Domain: domain, Kind: 1, ID: EntityID{0, 0xff, 'e'}}},
		{"policy_copy_row_v1.bin", func() ([]byte, error) { return PolicyCopyRow(domain, lpart, 4, visibility) }, func(b []byte) (any, error) { return ParsePolicyCopyRow(b) }, PolicyCopyKey{Domain: domain, LPART: lpart, Generation: 4, VisibilityDigest: visibility}},
		{"policy_copy_map_row_v1.bin", func() ([]byte, error) { return PolicyCopyMapRow(domain, lpart, 8, visibility) }, func(b []byte) (any, error) { return ParsePolicyCopyMapRow(b) }, PolicyCopyKey{Domain: domain, LPART: lpart, Generation: 8}},
		{"policy_generation_row_v1.bin", func() ([]byte, error) { return PolicyGenerationRow(domain, 8) }, func(b []byte) (any, error) { return ParsePolicyGenerationRow(b) }, PolicyGenerationKey{Domain: domain, Generation: 8}},
		{"index_generation_row_v1.bin", func() ([]byte, error) { return IndexGenerationRow(domain, family, igen) }, func(b []byte) (any, error) { return ParseIndexGenerationRow(b) }, IndexGenerationKey{Domain: domain, Family: family, IGEN: igen}},
		{"index_delta_row_v1.bin", func() ([]byte, error) { return IndexDeltaRow(domain, family, igen, 13, TXN{0, 0xff, 't'}) }, func(b []byte) (any, error) { return ParseIndexDeltaRow(b) }, IndexDeltaKey{IndexGenerationKey: IndexGenerationKey{Domain: domain, Family: family, IGEN: igen}, Epoch: 13, TXN: TXN{0, 0xff, 't'}}},
		{"index_activation_row_v1.bin", func() ([]byte, error) { return IndexActivationRow(domain, family, 16, igen) }, func(b []byte) (any, error) { return ParseIndexActivationRow(b) }, IndexActivationKey{Domain: domain, Family: family, ActivationEpoch: 16}},
		{"snapshot_lease_row_v1.bin", func() ([]byte, error) { return SnapshotLeaseRow(domain, LeaseID{0, 0xff, 'l'}) }, func(b []byte) (any, error) { return ParseSnapshotLeaseRow(b) }, SnapshotLeaseKey{Domain: domain, Lease: LeaseID{0, 0xff, 'l'}}},
		{"retirement_row_v1.bin", func() ([]byte, error) { return RetirementRow(domain, EntityKind{2}, EntityID{0, 0xff, 'r'}) }, func(b []byte) (any, error) { return ParseRetirementRow(b) }, RetirementKey{Domain: domain, Kind: EntityKind{2}, ID: EntityID{0, 0xff, 'r'}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			row = golden(t, test.name, row)
			decoded, err := test.parse(row)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.want) {
				t.Fatalf("row round trip differs:\ngot  %#v\nwant %#v", decoded, test.want)
			}
			if _, err := test.parse(append(append([]byte(nil), row...), 0)); err == nil {
				t.Fatal("trailing row bytes accepted")
			}
			corrupt := append([]byte(nil), row...)
			corrupt[2] ^= 1
			if _, err := test.parse(corrupt); err == nil {
				t.Fatal("partition band mismatch accepted")
			}
		})
	}
}

func TestM3RowOrderingAndSharedAllocatorRows(t *testing.T) {
	domain := DomainID("domain")
	visibility := testDigest("visibility")
	oldMap, err := PolicyCopyMapRow(domain, LPART("part"), 7, visibility)
	if err != nil {
		t.Fatal(err)
	}
	newMap, err := PolicyCopyMapRow(domain, LPART("part"), 8, visibility)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(newMap, oldMap) >= 0 {
		t.Fatal("newer policy map does not sort first")
	}
	oldActivation, _ := IndexActivationRow(domain, Family("lexical"), 10, IGEN("g1"))
	newActivation, _ := IndexActivationRow(domain, Family("lexical"), 11, IGEN("g2"))
	if bytes.Compare(newActivation, oldActivation) >= 0 {
		t.Fatal("newer index activation does not sort first")
	}
	sameMap, _ := PolicyCopyMapRow(domain, LPART("part"), 8, testDigest("other"))
	if !bytes.Equal(newMap, sameMap) {
		t.Fatal("policy mapping identity includes visibility digest")
	}
	sameActivation, _ := IndexActivationRow(domain, Family("lexical"), 11, IGEN("other"))
	if !bytes.Equal(newActivation, sameActivation) {
		t.Fatal("index activation identity includes generation")
	}
	firstKind := EntityKind("multi-byte-kind")
	var collidingKind EntityKind
	for value := 0; value < 10_000; value++ {
		candidate := EntityKind([]byte{byte(value >> 8), byte(value)})
		if !bytes.Equal(candidate, firstKind) && B8('X', candidate) == B8('X', firstKind) {
			collidingKind = candidate
			break
		}
	}
	if collidingKind == nil {
		t.Fatal("could not construct legacy retirement-kind hash collision")
	}
	firstRetirement, _ := RetirementRow(domain, firstKind, EntityID("same-id"))
	secondRetirement, _ := RetirementRow(domain, collidingKind, EntityID("same-id"))
	if bytes.Equal(firstRetirement, secondRetirement) {
		t.Fatal("retirement row identity loses the full object kind")
	}
	parsedRetirement, err := ParseRetirementRow(secondRetirement)
	if err != nil || !bytes.Equal(parsedRetirement.Kind, collidingKind) {
		t.Fatalf("retirement kind did not round trip: %#v, %v", parsedRetirement, err)
	}
	mapPrefix, _ := PolicyCopyMapPrefix(domain, LPART("part"))
	if !bytes.HasPrefix(newMap, mapPrefix) {
		t.Fatal("policy map prefix does not cover mapping rows")
	}
	mapSeek, _ := PolicyCopyMapSeek(domain, LPART("part"), 8)
	if !bytes.HasPrefix(newMap, mapSeek) || bytes.Compare(oldMap, mapSeek) < 0 {
		t.Fatal("policy map seek does not begin at the requested inverse generation")
	}
	activationPrefix, _ := IndexActivationPrefix(domain, Family("lexical"))
	if !bytes.HasPrefix(newActivation, activationPrefix) {
		t.Fatal("index activation prefix does not cover activation rows")
	}
	activationSeek, _ := IndexActivationSeek(domain, Family("lexical"), 11)
	if !bytes.HasPrefix(newActivation, activationSeek) || bytes.Compare(oldActivation, activationSeek) < 0 {
		t.Fatal("index activation seek does not begin at the requested inverse epoch")
	}
	delta, _ := IndexDeltaRow(domain, Family("lexical"), IGEN("g1"), 10, TXN("txn"))
	deltaPrefix, _ := IndexDeltaPrefix(domain, Family("lexical"), IGEN("g1"))
	if !bytes.HasPrefix(delta, deltaPrefix) {
		t.Fatal("index delta prefix does not cover delta rows")
	}
	copyHead, _ := PolicyCopyHeadRow(domain, LPART("part"))
	indexHead, _ := IndexGenerationHeadRow(domain, Family("lexical"))
	if bytes.Equal(copyHead, indexHead) {
		t.Fatal("catalog generation heads collide")
	}
	allocator, _ := AllocatorRow(domain)
	for name, makeRow := range map[string]func(DomainID) ([]byte, error){
		"history floor": HistoryFloorRow, "writer authority": WriterAuthorityRow,
		"backend observation": BackendObservationRow,
	} {
		row, err := makeRow(domain)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(row, allocator) {
			t.Fatalf("%s is not stored on the allocator authority row", name)
		}
	}
}
