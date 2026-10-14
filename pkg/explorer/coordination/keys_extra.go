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
	"math"
)

type EntityHeadKey struct {
	Domain DomainID
	Kind   byte
	ID     EntityID
}

func EntityHeadRow(domain DomainID, kind byte, id EntityID) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if kind == 0 {
		return nil, invalid("entity kind byte is required")
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('H'), B8('H', domain, []byte{kind}, id))
	row = append(row, E(domain)...)
	row = append(row, kind)
	return append(row, E(id)...), nil
}

func EntityGuardRow(domain DomainID, kind byte, id EntityID) ([]byte, error) {
	return EntityHeadRow(domain, kind, id)
}

func PendingMutationRow(domain DomainID, kind byte, id EntityID) ([]byte, error) {
	return EntityHeadRow(domain, kind, id)
}

func ParseEntityHeadRow(row []byte) (EntityHeadKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'H')
	if err != nil {
		return EntityHeadKey{}, err
	}
	if offset >= len(row) || row[offset] == 0 {
		return EntityHeadKey{}, invalid("entity-head kind is missing")
	}
	kind := row[offset]
	id, used, err := DecodeE(row[offset+1:])
	if err != nil {
		return EntityHeadKey{}, err
	}
	result := EntityHeadKey{Domain: domain, Kind: kind, ID: EntityID(id)}
	if err := result.ID.Validate(); err != nil {
		return EntityHeadKey{}, err
	}
	if offset+1+used != len(row) ||
		row[2] != B8('H', result.Domain, []byte{result.Kind}, result.ID) {
		return EntityHeadKey{}, invalid("entity-head row has malformed or trailing components")
	}
	return result, nil
}

func ParseEntityGuardRow(row []byte) (EntityHeadKey, error) {
	return ParseEntityHeadRow(row)
}

func ParsePendingMutationRow(row []byte) (EntityHeadKey, error) {
	return ParseEntityHeadRow(row)
}

type PolicyCopyKey struct {
	Domain           DomainID
	LPART            LPART
	Generation       Generation
	VisibilityDigest Digest
}

func PolicyCopyHeadRow(domain DomainID, lpart LPART) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := lpart.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('H'), B8('Y', domain, lpart))
	row = append(row, E(domain)...)
	return append(row, E(lpart)...), nil
}

func PolicyCopyMapPrefix(domain DomainID, lpart LPART) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := lpart.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('M'), B8('Y', domain, lpart))
	row = append(row, E(domain)...)
	return append(row, E(lpart)...), nil
}

func PolicyCopyMapSeek(domain DomainID, lpart LPART, generation Generation) ([]byte, error) {
	if err := generation.Validate(); err != nil {
		return nil, err
	}
	prefix, err := PolicyCopyMapPrefix(domain, lpart)
	if err != nil {
		return nil, err
	}
	return append(prefix, INV64(uint64(generation))...), nil
}

func PolicyCopyRow(domain DomainID, lpart LPART, generation Generation, visibility Digest) ([]byte, error) {
	if err := validatePolicyKey(domain, lpart, generation, visibility); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('C'), B8('Y', domain, lpart))
	row = append(row, E(domain)...)
	row = append(row, E(lpart)...)
	row = append(row, U64(uint64(generation))...)
	return append(row, visibility[:]...), nil
}

func ParsePolicyCopyRow(row []byte) (PolicyCopyKey, error) {
	return parsePolicyRow(row, 'C', false)
}

func PolicyCopyMapRow(domain DomainID, lpart LPART, generation Generation, visibility Digest) ([]byte, error) {
	if err := validatePolicyKey(domain, lpart, generation, visibility); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('M'), B8('Y', domain, lpart))
	row = append(row, E(domain)...)
	row = append(row, E(lpart)...)
	return append(row, INV64(uint64(generation))...), nil
}

func ParsePolicyCopyMapRow(row []byte) (PolicyCopyKey, error) {
	return parsePolicyRow(row, 'M', true)
}

func validatePolicyKey(domain DomainID, lpart LPART, generation Generation, visibility Digest) error {
	if err := domain.Validate(); err != nil {
		return err
	}
	if err := lpart.Validate(); err != nil {
		return err
	}
	if err := generation.Validate(); err != nil {
		return err
	}
	return visibility.Validate("visibility digest")
}

func parsePolicyRow(row []byte, kind byte, inverse bool) (PolicyCopyKey, error) {
	domain, offset, err := parseTableRowPrefix(row, kind)
	if err != nil {
		return PolicyCopyKey{}, err
	}
	lpart, used, err := DecodeE(row[offset:])
	if err != nil {
		return PolicyCopyKey{}, err
	}
	offset += used
	expected := 8 + sha256Size
	if kind == 'M' {
		expected = 8
	}
	if len(row)-offset != expected {
		return PolicyCopyKey{}, invalid("policy-copy row has malformed components")
	}
	raw := binary.BigEndian.Uint64(row[offset : offset+8])
	if inverse {
		raw = ^raw
	}
	if raw == 0 || raw > math.MaxInt64 {
		return PolicyCopyKey{}, invalid("policy-copy generation is outside the supported range")
	}
	result := PolicyCopyKey{
		Domain: domain, LPART: LPART(lpart), Generation: Generation(raw),
	}
	if kind != 'M' {
		copy(result.VisibilityDigest[:], row[offset+8:])
	}
	if err := result.LPART.Validate(); err != nil {
		return PolicyCopyKey{}, err
	}
	if kind != 'M' {
		if err := result.VisibilityDigest.Validate("visibility digest"); err != nil {
			return PolicyCopyKey{}, err
		}
	}
	if row[2] != B8('Y', result.Domain, result.LPART) {
		return PolicyCopyKey{}, invalid("policy-copy row partition band mismatch")
	}
	return result, nil
}

type PolicyGenerationKey struct {
	Domain     DomainID
	Generation Generation
}

func PolicyGenerationRow(domain DomainID, generation Generation) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := generation.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('G'), B8('Y', domain))
	row = append(row, E(domain)...)
	return append(row, U64(uint64(generation))...), nil
}

func PolicyCopyFenceRow(domain DomainID, generation Generation) ([]byte, error) {
	return PolicyGenerationRow(domain, generation)
}

func PolicyCopyFenceRowV2(
	domain DomainID,
	lpart LPART,
	generation Generation,
	visibility Digest,
) ([]byte, error) {
	return PolicyCopyRow(domain, lpart, generation, visibility)
}

func ParsePolicyGenerationRow(row []byte) (PolicyGenerationKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'G')
	if err != nil {
		return PolicyGenerationKey{}, err
	}
	if len(row)-offset != 8 || row[2] != B8('Y', domain) {
		return PolicyGenerationKey{}, invalid("policy-generation row has malformed components")
	}
	value := binary.BigEndian.Uint64(row[offset:])
	if value == 0 || value > math.MaxInt64 {
		return PolicyGenerationKey{}, invalid("policy generation is outside the supported range")
	}
	return PolicyGenerationKey{Domain: domain, Generation: Generation(value)}, nil
}

func ParsePolicyCopyFenceRow(row []byte) (PolicyGenerationKey, error) {
	return ParsePolicyGenerationRow(row)
}

type IndexGenerationKey struct {
	Domain DomainID
	Family Family
	IGEN   IGEN
}

func IndexGenerationHeadRow(domain DomainID, family Family) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := family.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('H'), B8('G', domain, family))
	row = append(row, E(domain)...)
	return append(row, E(family)...), nil
}

func IndexGenerationRow(domain DomainID, family Family, igen IGEN) ([]byte, error) {
	if err := validateIndexKey(domain, family, igen); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('G'), B8('G', domain, family, igen))
	row = append(row, E(domain)...)
	row = append(row, E(family)...)
	return append(row, E(igen)...), nil
}

func ParseIndexGenerationRow(row []byte) (IndexGenerationKey, error) {
	domain, family, igen, offset, err := parseIndexBase(row, 'G')
	if err != nil {
		return IndexGenerationKey{}, err
	}
	if offset != len(row) || row[2] != B8('G', domain, family, igen) {
		return IndexGenerationKey{}, invalid("index-generation row has malformed components")
	}
	return IndexGenerationKey{Domain: domain, Family: family, IGEN: igen}, nil
}

type IndexDeltaKey struct {
	IndexGenerationKey
	Epoch Epoch
	TXN   TXN
}

func IndexDeltaRow(domain DomainID, family Family, igen IGEN, epoch Epoch, txn TXN) ([]byte, error) {
	if err := validateIndexKey(domain, family, igen); err != nil {
		return nil, err
	}
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	if err := txn.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('D'), B8('G', domain, family, igen))
	row = append(row, E(domain)...)
	row = append(row, E(family)...)
	row = append(row, E(igen)...)
	row = append(row, U64(uint64(epoch))...)
	return append(row, E(txn)...), nil
}

func IndexDeltaPrefix(domain DomainID, family Family, igen IGEN) ([]byte, error) {
	if err := validateIndexKey(domain, family, igen); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('D'), B8('G', domain, family, igen))
	row = append(row, E(domain)...)
	row = append(row, E(family)...)
	return append(row, E(igen)...), nil
}

func ParseIndexDeltaRow(row []byte) (IndexDeltaKey, error) {
	domain, family, igen, offset, err := parseIndexBase(row, 'D')
	if err != nil {
		return IndexDeltaKey{}, err
	}
	if len(row)-offset < 10 {
		return IndexDeltaKey{}, invalid("index-delta row is truncated")
	}
	value := binary.BigEndian.Uint64(row[offset : offset+8])
	if value == 0 || value > math.MaxInt64 {
		return IndexDeltaKey{}, invalid("index-delta epoch is outside the supported range")
	}
	txn, used, err := DecodeE(row[offset+8:])
	if err != nil {
		return IndexDeltaKey{}, err
	}
	result := IndexDeltaKey{
		IndexGenerationKey: IndexGenerationKey{Domain: domain, Family: family, IGEN: igen},
		Epoch:              Epoch(value), TXN: TXN(txn),
	}
	if err := result.TXN.Validate(); err != nil {
		return IndexDeltaKey{}, err
	}
	if offset+8+used != len(row) || row[2] != B8('G', domain, family, igen) {
		return IndexDeltaKey{}, invalid("index-delta row has malformed or trailing components")
	}
	return result, nil
}

type IndexActivationKey struct {
	Domain          DomainID
	Family          Family
	ActivationEpoch Epoch
	IGEN            IGEN
}

func IndexActivationRow(domain DomainID, family Family, epoch Epoch, igen IGEN) ([]byte, error) {
	if err := validateIndexKey(domain, family, igen); err != nil {
		return nil, err
	}
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('A'), B8('G', domain, family))
	row = append(row, E(domain)...)
	row = append(row, E(family)...)
	return append(row, INV64(uint64(epoch))...), nil
}

func IndexActivationPrefix(domain DomainID, family Family) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := family.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('A'), B8('G', domain, family))
	row = append(row, E(domain)...)
	return append(row, E(family)...), nil
}

func IndexActivationSeek(domain DomainID, family Family, epoch Epoch) ([]byte, error) {
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	prefix, err := IndexActivationPrefix(domain, family)
	if err != nil {
		return nil, err
	}
	return append(prefix, INV64(uint64(epoch))...), nil
}

func ParseIndexActivationRow(row []byte) (IndexActivationKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'A')
	if err != nil {
		return IndexActivationKey{}, err
	}
	familyBytes, used, err := DecodeE(row[offset:])
	if err != nil {
		return IndexActivationKey{}, err
	}
	offset += used
	if len(row)-offset != 8 {
		return IndexActivationKey{}, invalid("index-activation row is truncated")
	}
	value := ^binary.BigEndian.Uint64(row[offset : offset+8])
	if value == 0 || value > math.MaxInt64 {
		return IndexActivationKey{}, invalid("index activation epoch is outside the supported range")
	}
	result := IndexActivationKey{
		Domain: domain, Family: Family(familyBytes), ActivationEpoch: Epoch(value),
	}
	if err := result.Family.Validate(); err != nil {
		return IndexActivationKey{}, err
	}
	if offset+8 != len(row) || row[2] != B8('G', domain, result.Family) {
		return IndexActivationKey{}, invalid("index-activation row has malformed or trailing components")
	}
	return result, nil
}

func validateIndexKey(domain DomainID, family Family, igen IGEN) error {
	if err := domain.Validate(); err != nil {
		return err
	}
	if err := family.Validate(); err != nil {
		return err
	}
	return igen.Validate()
}

func parseIndexBase(row []byte, kind byte) (DomainID, Family, IGEN, int, error) {
	domain, offset, err := parseTableRowPrefix(row, kind)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	family, used, err := DecodeE(row[offset:])
	if err != nil {
		return nil, nil, nil, 0, err
	}
	offset += used
	igen, used, err := DecodeE(row[offset:])
	if err != nil {
		return nil, nil, nil, 0, err
	}
	offset += used
	f := Family(family)
	i := IGEN(igen)
	if err := f.Validate(); err != nil {
		return nil, nil, nil, 0, err
	}
	if err := i.Validate(); err != nil {
		return nil, nil, nil, 0, err
	}
	return domain, f, i, offset, nil
}

type SnapshotLeaseKey struct {
	Domain DomainID
	Lease  LeaseID
}

func SnapshotLeaseRow(domain DomainID, lease LeaseID) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := lease.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('L'), B8('L', domain, lease))
	row = append(row, E(domain)...)
	return append(row, E(lease)...), nil
}

func ParseSnapshotLeaseRow(row []byte) (SnapshotLeaseKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'L')
	if err != nil {
		return SnapshotLeaseKey{}, err
	}
	lease, used, err := DecodeE(row[offset:])
	if err != nil {
		return SnapshotLeaseKey{}, err
	}
	result := SnapshotLeaseKey{Domain: domain, Lease: LeaseID(lease)}
	if err := result.Lease.Validate(); err != nil {
		return SnapshotLeaseKey{}, err
	}
	if offset+used != len(row) || row[2] != B8('L', domain, result.Lease) {
		return SnapshotLeaseKey{}, invalid("snapshot-lease row has malformed or trailing components")
	}
	return result, nil
}

type RetirementKey struct {
	Domain DomainID
	Kind   EntityKind
	ID     EntityID
}

func RetirementRow(domain DomainID, kind EntityKind, id EntityID) ([]byte, error) {
	if err := domain.Validate(); err != nil {
		return nil, err
	}
	if err := kind.Validate(); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	row := rowPrefix(RowKind('R'), B8('X', domain, kind, id))
	row = append(row, E(domain)...)
	row = append(row, E(kind)...)
	return append(row, E(id)...), nil
}

func ParseRetirementRow(row []byte) (RetirementKey, error) {
	domain, offset, err := parseTableRowPrefix(row, 'R')
	if err != nil {
		return RetirementKey{}, err
	}
	kind, used, err := DecodeE(row[offset:])
	if err != nil {
		return RetirementKey{}, err
	}
	offset += used
	id, used, err := DecodeE(row[offset:])
	if err != nil {
		return RetirementKey{}, err
	}
	result := RetirementKey{Domain: domain, Kind: EntityKind(kind), ID: EntityID(id)}
	if err := result.Kind.Validate(); err != nil {
		return RetirementKey{}, err
	}
	if err := result.ID.Validate(); err != nil {
		return RetirementKey{}, err
	}
	if offset+used != len(row) ||
		row[2] != B8('X', result.Domain, result.Kind, result.ID) {
		return RetirementKey{}, invalid("retirement row has malformed or trailing components")
	}
	return result, nil
}

func HistoryFloorRow(domain DomainID) ([]byte, error)       { return AllocatorRow(domain) }
func WriterAuthorityRow(domain DomainID) ([]byte, error)    { return AllocatorRow(domain) }
func BackendObservationRow(domain DomainID) ([]byte, error) { return AllocatorRow(domain) }

func ParseHistoryFloorRow(row []byte) (DomainID, error)       { return parseAllocatorAuthorityRow(row) }
func ParseWriterAuthorityRow(row []byte) (DomainID, error)    { return parseAllocatorAuthorityRow(row) }
func ParseBackendObservationRow(row []byte) (DomainID, error) { return parseAllocatorAuthorityRow(row) }

func parseAllocatorAuthorityRow(row []byte) (DomainID, error) {
	parsed, err := ParseCoordinationRow(row)
	if err != nil {
		return nil, err
	}
	if parsed.Kind != RowAllocator {
		return nil, invalid("record is not on the allocator authority row")
	}
	return parsed.Domain, nil
}

func parseTableRowPrefix(row []byte, kind byte) (DomainID, int, error) {
	if len(row) < 5 || row[0] != schemaV1 || row[1] != kind {
		return nil, 0, invalid("coordination table row prefix is malformed")
	}
	domain, used, err := DecodeE(row[3:])
	if err != nil {
		return nil, 0, err
	}
	value := DomainID(domain)
	if err := value.Validate(); err != nil {
		return nil, 0, err
	}
	return value, 3 + used, nil
}

const sha256Size = 32

func EqualRows(left, right []byte) bool { return bytes.Equal(left, right) }
