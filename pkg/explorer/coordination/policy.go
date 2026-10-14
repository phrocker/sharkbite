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
	"crypto/sha256"
	"sort"
	"time"
)

type PolicyCopyEntry struct {
	Table          []byte
	RowIdentity    []byte
	LogicalDigest  Digest
	PhysicalDigest Digest
}

func (p PolicyCopyEntry) Validate() error {
	if err := validateOpaque("policy-copy table", p.Table, MaxCoordinateBytes, true); err != nil {
		return err
	}
	if err := validateOpaque("policy-copy row identity", p.RowIdentity, MaxCoordinateBytes, true); err != nil {
		return err
	}
	if err := p.LogicalDigest.Validate("policy-copy logical digest"); err != nil {
		return err
	}
	return p.PhysicalDigest.Validate("policy-copy physical digest")
}

func ComparePolicyCopyEntries(left, right PolicyCopyEntry) int {
	if order := bytes.Compare(left.Table, right.Table); order != 0 {
		return order
	}
	return bytes.Compare(left.RowIdentity, right.RowIdentity)
}

func SortPolicyCopyEntries(values []PolicyCopyEntry) {
	sort.Slice(values, func(i, j int) bool { return ComparePolicyCopyEntries(values[i], values[j]) < 0 })
}

func policyCopyEntriesDigest(entries []PolicyCopyEntry) Digest {
	e := newPayloadEncoder(MaxChunkBytes)
	e.u32(uint32(len(entries)))
	for _, entry := range entries {
		e.bytes("table", entry.Table)
		e.bytes("row identity", entry.RowIdentity)
		e.digest(entry.LogicalDigest)
		e.digest(entry.PhysicalDigest)
	}
	return sha256.Sum256(e.data)
}

type PolicyCopyManifestV1 struct {
	LPART            LPART
	CopyGeneration   Generation
	VisibilityDigest Digest
	Backend          BackendID
	Table            []byte
	Entries          []PolicyCopyEntry
	RowCount         uint64
	ManifestDigest   Digest
	State            CopyState
}

func (m PolicyCopyManifestV1) Validate() error {
	if err := m.LPART.Validate(); err != nil {
		return err
	}
	if err := m.CopyGeneration.Validate(); err != nil {
		return err
	}
	if err := m.VisibilityDigest.Validate("visibility digest"); err != nil {
		return err
	}
	if err := m.Backend.Validate(); err != nil {
		return err
	}
	if err := validateOpaque("policy-copy table", m.Table, MaxCoordinateBytes, true); err != nil {
		return err
	}
	if err := m.State.Validate(); err != nil {
		return err
	}
	if len(m.Entries) == 0 || len(m.Entries) > MaxPolicyCopyEntries {
		return invalid("policy-copy entry count is outside its bound")
	}
	for index, entry := range m.Entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if !bytes.Equal(entry.Table, m.Table) {
			return invalid("policy-copy entry table does not match manifest table")
		}
		if index > 0 && ComparePolicyCopyEntries(m.Entries[index-1], entry) >= 0 {
			return invalid("policy-copy entries must be strictly ordered")
		}
	}
	if m.RowCount != uint64(len(m.Entries)) {
		return invalid("policy-copy row count does not match entries")
	}
	if m.ManifestDigest != policyCopyEntriesDigest(m.Entries) {
		return invalid("policy-copy manifest digest mismatch")
	}
	return nil
}

func NewPolicyCopyManifestV1(lpart LPART, generation Generation, visibility Digest,
	backend BackendID, table []byte, entries []PolicyCopyEntry, state CopyState) (PolicyCopyManifestV1, error) {
	entries = append([]PolicyCopyEntry(nil), entries...)
	SortPolicyCopyEntries(entries)
	value := PolicyCopyManifestV1{
		LPART: lpart, CopyGeneration: generation, VisibilityDigest: visibility,
		Backend: backend, Table: append([]byte(nil), table...), Entries: entries,
		RowCount: uint64(len(entries)), State: state,
	}
	value.ManifestDigest = policyCopyEntriesDigest(value.Entries)
	return value, value.Validate()
}

func ValidatePolicyCopyTransition(previous, next PolicyCopyManifestV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.LPART, next.LPART) ||
		previous.CopyGeneration != next.CopyGeneration ||
		previous.VisibilityDigest != next.VisibilityDigest ||
		!bytes.Equal(previous.Backend, next.Backend) ||
		!bytes.Equal(previous.Table, next.Table) ||
		previous.ManifestDigest != next.ManifestDigest ||
		previous.RowCount != next.RowCount {
		return invalid("policy-copy identity and sealed contents are immutable")
	}
	if previous.State == CopyStatePoisoned || previous.State == CopyStateRetired {
		return invalid("terminal policy-copy state cannot transition")
	}
	if next.State == CopyStatePoisoned {
		return nil
	}
	legal := previous.State == CopyStateBuilding && next.State == CopyStateSealed ||
		previous.State == CopyStateSealed && next.State == CopyStateActive ||
		previous.State == CopyStateActive && next.State == CopyStateRetired
	if !legal {
		return invalid("illegal policy-copy state transition")
	}
	return nil
}

func encodePolicyCopyManifest(e *encoder, m PolicyCopyManifestV1) {
	e.bytes("LPART", m.LPART)
	e.u64(uint64(m.CopyGeneration))
	e.digest(m.VisibilityDigest)
	e.bytes("backend", m.Backend)
	e.bytes("table", m.Table)
	e.u32(uint32(len(m.Entries)))
	e.u64(m.RowCount)
	e.digest(m.ManifestDigest)
	e.byte(byte(m.State))
	for _, entry := range m.Entries {
		e.bytes("entry table", entry.Table)
		e.bytes("row identity", entry.RowIdentity)
		e.digest(entry.LogicalDigest)
		e.digest(entry.PhysicalDigest)
	}
}

func MarshalPolicyCopyManifestV1(m PolicyCopyManifestV1) ([]byte, error) {
	m.Entries = append([]PolicyCopyEntry(nil), m.Entries...)
	SortPolicyCopyEntries(m.Entries)
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindPolicyCopyManifest, VersionPolicyCopyManifestV1, MaxChunkBytes,
		func(e *encoder) { encodePolicyCopyManifest(e, m) })
}

func UnmarshalPolicyCopyManifestV1(data []byte) (PolicyCopyManifestV1, error) {
	payload, err := verifyEnvelope(data, KindPolicyCopyManifest, VersionPolicyCopyManifestV1, MaxChunkBytes)
	if err != nil {
		return PolicyCopyManifestV1{}, err
	}
	d := &decoder{data: payload}
	m := PolicyCopyManifestV1{
		LPART:            LPART(d.bytes("LPART", MaxOpaqueIDBytes, true)),
		CopyGeneration:   Generation(d.positive("copy generation")),
		VisibilityDigest: d.digest("visibility digest"),
		Backend:          BackendID(d.bytes("backend", MaxBackendIDBytes, true)),
		Table:            d.bytes("table", MaxCoordinateBytes, true),
	}
	count := d.u32("entry count")
	m.RowCount = d.u64("row count")
	m.ManifestDigest = d.digest("manifest digest")
	m.State = CopyState(d.byte("state"))
	if count == 0 || count > MaxPolicyCopyEntries || uint64(count) > uint64(d.remaining())/72 {
		return PolicyCopyManifestV1{}, invalid("policy-copy entry count exceeds its bound or remaining payload")
	}
	m.Entries = make([]PolicyCopyEntry, int(count))
	for index := range m.Entries {
		m.Entries[index] = PolicyCopyEntry{
			Table:          d.bytes("entry table", MaxCoordinateBytes, true),
			RowIdentity:    d.bytes("row identity", MaxCoordinateBytes, true),
			LogicalDigest:  d.digest("logical digest"),
			PhysicalDigest: d.digest("physical digest"),
		}
	}
	if d.err != nil {
		return PolicyCopyManifestV1{}, d.err
	}
	if err := m.Validate(); err != nil {
		return PolicyCopyManifestV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodePolicyCopyManifest(e, m) }, MaxChunkBytes); err != nil {
		return PolicyCopyManifestV1{}, err
	}
	return m, nil
}

type PolicyCopyMapV3 struct {
	LPART            LPART
	MapGeneration    Generation
	CopyGeneration   Generation
	VisibilityDigest Digest
	CopyDigest       Digest
	ActivationKind   ActivationKind
	ActivationRef    []byte
	State            CopyState
}

func (m PolicyCopyMapV3) Validate() error {
	if err := m.LPART.Validate(); err != nil {
		return err
	}
	if err := m.MapGeneration.Validate(); err != nil {
		return err
	}
	if err := m.CopyGeneration.Validate(); err != nil {
		return err
	}
	if err := m.VisibilityDigest.Validate("visibility digest"); err != nil {
		return err
	}
	if err := m.CopyDigest.Validate("policy-copy digest"); err != nil {
		return err
	}
	if err := m.ActivationKind.Validate(); err != nil {
		return err
	}
	if err := validateOpaque("activation reference", m.ActivationRef, MaxOpaqueIDBytes, true); err != nil {
		return err
	}
	if err := m.State.Validate(); err != nil {
		return err
	}
	if m.State == CopyStateBuilding {
		return invalid("policy-copy mapping cannot be BUILDING")
	}
	return nil
}

func ValidatePolicyCopyMapSuccessor(previous, next PolicyCopyMapV3) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.LPART, next.LPART) {
		return invalid("policy-copy map LPART is immutable")
	}
	if next.MapGeneration <= previous.MapGeneration {
		return invalid("policy-copy map generation must increase")
	}
	return nil
}

func ValidateCopyGenerationSuccessor(previous, next Generation) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if next <= previous {
		return invalid("copy generation must increase")
	}
	return nil
}

func ValidateImmutablePolicyCopyMap(previous, next PolicyCopyMapV3) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	left, err := MarshalPolicyCopyMapV3(previous)
	if err != nil {
		return err
	}
	right, err := MarshalPolicyCopyMapV3(next)
	if err != nil {
		return err
	}
	if !bytes.Equal(left, right) {
		return invalid("committed policy-copy mapping is immutable")
	}
	return nil
}

func encodePolicyCopyMap(e *encoder, m PolicyCopyMapV3) {
	e.bytes("LPART", m.LPART)
	e.u64(uint64(m.MapGeneration))
	e.u64(uint64(m.CopyGeneration))
	e.digest(m.VisibilityDigest)
	e.digest(m.CopyDigest)
	e.byte(byte(m.ActivationKind))
	e.bytes("activation reference", m.ActivationRef)
	e.byte(byte(m.State))
}

func MarshalPolicyCopyMapV3(m PolicyCopyMapV3) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindPolicyCopyMap, VersionPolicyCopyMapV3, MaxRootBytes,
		func(e *encoder) { encodePolicyCopyMap(e, m) })
}

func UnmarshalPolicyCopyMapV3(data []byte) (PolicyCopyMapV3, error) {
	payload, err := verifyEnvelope(data, KindPolicyCopyMap, VersionPolicyCopyMapV3, MaxRootBytes)
	if err != nil {
		return PolicyCopyMapV3{}, err
	}
	d := &decoder{data: payload}
	m := PolicyCopyMapV3{
		LPART:            LPART(d.bytes("LPART", MaxOpaqueIDBytes, true)),
		MapGeneration:    Generation(d.positive("map generation")),
		CopyGeneration:   Generation(d.positive("copy generation")),
		VisibilityDigest: d.digest("visibility digest"),
		CopyDigest:       d.digest("copy digest"),
		ActivationKind:   ActivationKind(d.byte("activation kind")),
		ActivationRef:    d.bytes("activation reference", MaxOpaqueIDBytes, true),
		State:            CopyState(d.byte("state")),
	}
	if d.err != nil {
		return PolicyCopyMapV3{}, d.err
	}
	if err := m.Validate(); err != nil {
		return PolicyCopyMapV3{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodePolicyCopyMap(e, m) }, MaxRootBytes); err != nil {
		return PolicyCopyMapV3{}, err
	}
	return m, nil
}

type PolicyCopyFenceV1 struct {
	LPART               LPART
	CopyGeneration      Generation
	Owner               OwnerID
	LeaseUntil          time.Time
	Fence               Fence
	AuthorityGeneration Generation
	State               GuardState
}

func (f PolicyCopyFenceV1) Validate() error {
	if err := f.LPART.Validate(); err != nil {
		return err
	}
	if err := f.CopyGeneration.Validate(); err != nil {
		return err
	}
	if err := f.Owner.Validate(); err != nil {
		return err
	}
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if err := f.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := f.State.Validate(); err != nil {
		return err
	}
	if f.State == GuardStateHeld {
		return validateTime("policy-copy fence lease", f.LeaseUntil, false)
	}
	if !f.LeaseUntil.IsZero() {
		return invalid("released policy-copy fence must not have a lease")
	}
	return nil
}

func ValidatePolicyCopyFenceTransition(previous, next PolicyCopyFenceV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.LPART, next.LPART) || previous.CopyGeneration != next.CopyGeneration {
		return invalid("policy-copy fence identity is immutable")
	}
	if previous.State != GuardStateHeld {
		return invalid("terminal policy-copy fence cannot transition")
	}
	if next.Fence < previous.Fence || next.AuthorityGeneration < previous.AuthorityGeneration {
		return invalid("policy-copy fence token or authority decreased")
	}
	if next.State == GuardStateHeld {
		if bytes.Equal(previous.Owner, next.Owner) {
			if next.Fence != previous.Fence || next.LeaseUntil.Before(previous.LeaseUntil) {
				return invalid("policy-copy fence renewal changed fence or shortened lease")
			}
		} else if next.Fence <= previous.Fence {
			return invalid("policy-copy fence takeover requires a greater fence")
		}
		return nil
	}
	if !bytes.Equal(previous.Owner, next.Owner) || next.Fence != previous.Fence {
		return invalid("policy-copy fence release changed owner or fence")
	}
	return nil
}

func encodePolicyCopyFence(e *encoder, f PolicyCopyFenceV1) {
	e.bytes("LPART", f.LPART)
	e.u64(uint64(f.CopyGeneration))
	e.bytes("owner", f.Owner)
	e.timestamp(f.LeaseUntil)
	e.u64(uint64(f.Fence))
	e.u64(uint64(f.AuthorityGeneration))
	e.byte(byte(f.State))
}

func MarshalPolicyCopyFenceV1(f PolicyCopyFenceV1) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindPolicyCopyFence, VersionPolicyCopyFenceV1, MaxRootBytes,
		func(e *encoder) { encodePolicyCopyFence(e, f) })
}

func UnmarshalPolicyCopyFenceV1(data []byte) (PolicyCopyFenceV1, error) {
	payload, err := verifyEnvelope(data, KindPolicyCopyFence, VersionPolicyCopyFenceV1, MaxRootBytes)
	if err != nil {
		return PolicyCopyFenceV1{}, err
	}
	d := &decoder{data: payload}
	f := PolicyCopyFenceV1{
		LPART:               LPART(d.bytes("LPART", MaxOpaqueIDBytes, true)),
		CopyGeneration:      Generation(d.positive("copy generation")),
		Owner:               OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		LeaseUntil:          d.timestamp("lease"),
		Fence:               Fence(d.positive("fence")),
		AuthorityGeneration: Generation(d.positive("authority generation")),
		State:               GuardState(d.byte("state")),
	}
	if d.err != nil {
		return PolicyCopyFenceV1{}, d.err
	}
	if err := f.Validate(); err != nil {
		return PolicyCopyFenceV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodePolicyCopyFence(e, f) }, MaxRootBytes); err != nil {
		return PolicyCopyFenceV1{}, err
	}
	return f, nil
}
