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
)

type IndexGenerationV2 struct {
	Family                   Family
	IGEN                     IGEN
	Schema                   []byte
	Buckets                  uint32
	SourceEpoch              Epoch
	BuildThrough             Epoch
	DeltaThrough             Epoch
	PolicyCopyCoverageDigest Digest
	ManifestDigest           Digest
	DeltaDigest              Digest
	State                    IndexGenerationState
}

func (g IndexGenerationV2) computedDigest() Digest {
	e := newPayloadEncoder(MaxRootBytes)
	e.bytes("family", g.Family)
	e.bytes("IGEN", g.IGEN)
	e.bytes("schema", g.Schema)
	e.u32(g.Buckets)
	e.u64(uint64(g.SourceEpoch))
	e.u64(uint64(g.BuildThrough))
	e.u64(uint64(g.DeltaThrough))
	e.digest(g.PolicyCopyCoverageDigest)
	e.digest(g.DeltaDigest)
	return sha256.Sum256(e.data)
}

func (g IndexGenerationV2) ComputeDigest() Digest { return g.computedDigest() }

func NewIndexGenerationV2(value IndexGenerationV2) (IndexGenerationV2, error) {
	value.ManifestDigest = value.computedDigest()
	return value, value.Validate()
}

func (g IndexGenerationV2) Validate() error {
	if err := g.Family.Validate(); err != nil {
		return err
	}
	if err := g.IGEN.Validate(); err != nil {
		return err
	}
	if err := validateOpaque("index schema", g.Schema, MaxOpaqueIDBytes, true); err != nil {
		return err
	}
	if g.Buckets == 0 {
		return invalid("index bucket count must be positive")
	}
	for name, value := range map[string]Epoch{
		"source epoch": g.SourceEpoch, "build frontier": g.BuildThrough,
		"delta frontier": g.DeltaThrough,
	} {
		if err := value.Validate(); err != nil {
			return invalid(name + " must be positive")
		}
	}
	if g.BuildThrough < g.SourceEpoch || g.DeltaThrough < g.BuildThrough {
		return invalid("index frontiers must be monotonic")
	}
	if err := g.PolicyCopyCoverageDigest.Validate("policy-copy coverage digest"); err != nil {
		return err
	}
	if err := g.DeltaDigest.Validate("index delta digest"); err != nil {
		return err
	}
	if err := g.State.Validate(); err != nil {
		return err
	}
	if g.ManifestDigest != g.computedDigest() {
		return invalid("index manifest digest mismatch")
	}
	return nil
}

func ValidateIndexGenerationTransition(previous, next IndexGenerationV2) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.Family, next.Family) ||
		!bytes.Equal(previous.IGEN, next.IGEN) ||
		!bytes.Equal(previous.Schema, next.Schema) ||
		previous.Buckets != next.Buckets ||
		previous.SourceEpoch != next.SourceEpoch {
		return invalid("index-generation identity fields are immutable")
	}
	if previous.State == IndexGenerationPoisoned {
		return invalid("terminal index-generation state cannot transition")
	}
	if previous.State == IndexGenerationSealed {
		if next.State != IndexGenerationPoisoned {
			return invalid("sealed index-generation manifest cannot mutate")
		}
		if next.BuildThrough != previous.BuildThrough ||
			next.DeltaThrough != previous.DeltaThrough ||
			next.PolicyCopyCoverageDigest != previous.PolicyCopyCoverageDigest ||
			next.DeltaDigest != previous.DeltaDigest ||
			next.ManifestDigest != previous.ManifestDigest {
			return invalid("sealed index-generation manifest contents are immutable")
		}
		return nil
	}
	if previous.State != IndexGenerationBuilding {
		return invalid("illegal index-generation state transition")
	}
	if next.State == IndexGenerationPoisoned {
		if next.BuildThrough != previous.BuildThrough ||
			next.DeltaThrough != previous.DeltaThrough ||
			next.PolicyCopyCoverageDigest != previous.PolicyCopyCoverageDigest ||
			next.DeltaDigest != previous.DeltaDigest ||
			next.ManifestDigest != previous.ManifestDigest {
			return invalid("poisoning an index-generation manifest cannot alter its contents")
		}
		return nil
	}
	if next.State != IndexGenerationBuilding && next.State != IndexGenerationSealed {
		return invalid("illegal index-generation state transition")
	}
	if next.BuildThrough < previous.BuildThrough || next.DeltaThrough < previous.DeltaThrough {
		return invalid("index-generation frontiers must not decrease")
	}
	return nil
}

func encodeIndexGeneration(e *encoder, g IndexGenerationV2) {
	e.bytes("family", g.Family)
	e.bytes("IGEN", g.IGEN)
	e.bytes("schema", g.Schema)
	e.u32(g.Buckets)
	e.u64(uint64(g.SourceEpoch))
	e.u64(uint64(g.BuildThrough))
	e.u64(uint64(g.DeltaThrough))
	e.digest(g.PolicyCopyCoverageDigest)
	e.digest(g.ManifestDigest)
	e.digest(g.DeltaDigest)
	e.byte(byte(g.State))
}

func MarshalIndexGenerationV2(g IndexGenerationV2) ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindIndexGeneration, VersionIndexGenerationV2, MaxRootBytes,
		func(e *encoder) { encodeIndexGeneration(e, g) })
}

func UnmarshalIndexGenerationV2(data []byte) (IndexGenerationV2, error) {
	payload, err := verifyEnvelope(data, KindIndexGeneration, VersionIndexGenerationV2, MaxRootBytes)
	if err != nil {
		return IndexGenerationV2{}, err
	}
	d := &decoder{data: payload}
	g := IndexGenerationV2{
		Family:                   Family(d.bytes("family", MaxOpaqueIDBytes, true)),
		IGEN:                     IGEN(d.bytes("IGEN", MaxOpaqueIDBytes, true)),
		Schema:                   d.bytes("schema", MaxOpaqueIDBytes, true),
		Buckets:                  d.u32("buckets"),
		SourceEpoch:              Epoch(d.positive("source epoch")),
		BuildThrough:             Epoch(d.positive("build frontier")),
		DeltaThrough:             Epoch(d.positive("delta frontier")),
		PolicyCopyCoverageDigest: d.digest("policy-copy coverage digest"),
		ManifestDigest:           d.digest("manifest digest"),
		DeltaDigest:              d.digest("delta digest"),
		State:                    IndexGenerationState(d.byte("state")),
	}
	if d.err != nil {
		return IndexGenerationV2{}, d.err
	}
	if err := g.Validate(); err != nil {
		return IndexGenerationV2{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeIndexGeneration(e, g) }, MaxRootBytes); err != nil {
		return IndexGenerationV2{}, err
	}
	return g, nil
}

type IndexDeltaEntry struct {
	Kind           []byte
	ID             []byte
	LogicalDigest  Digest
	PhysicalDigest Digest
}

func CompareIndexDeltaEntries(left, right IndexDeltaEntry) int {
	if order := bytes.Compare(left.Kind, right.Kind); order != 0 {
		return order
	}
	return bytes.Compare(left.ID, right.ID)
}

func SortIndexDeltaEntries(values []IndexDeltaEntry) {
	sort.Slice(values, func(i, j int) bool { return CompareIndexDeltaEntries(values[i], values[j]) < 0 })
}

func indexDeltaEntriesDigest(entries []IndexDeltaEntry) Digest {
	e := newPayloadEncoder(MaxChunkBytes)
	e.u32(uint32(len(entries)))
	for _, entry := range entries {
		e.bytes("delta kind", entry.Kind)
		e.bytes("delta ID", entry.ID)
		e.digest(entry.LogicalDigest)
		e.digest(entry.PhysicalDigest)
	}
	return sha256.Sum256(e.data)
}

type IndexDeltaV1 struct {
	Family         Family
	IGEN           IGEN
	Epoch          Epoch
	TXN            TXN
	ManifestDigest Digest
	Entries        []IndexDeltaEntry
	DeltaDigest    Digest
	State          LifecycleState
}

func (d IndexDeltaV1) Validate() error {
	if err := d.Family.Validate(); err != nil {
		return err
	}
	if err := d.IGEN.Validate(); err != nil {
		return err
	}
	if err := d.Epoch.Validate(); err != nil {
		return err
	}
	if err := d.TXN.Validate(); err != nil {
		return err
	}
	if err := d.ManifestDigest.Validate("index manifest digest"); err != nil {
		return err
	}
	if d.State != LifecycleBuilding && d.State != LifecycleVerified {
		return invalid("index delta state must be BUILDING or VERIFIED")
	}
	if len(d.Entries) == 0 || len(d.Entries) > MaxIndexDeltaEntries {
		return invalid("index delta entry count is outside its bound")
	}
	for index, entry := range d.Entries {
		if err := validateOpaque("delta kind", entry.Kind, MaxObjectKindBytes, true); err != nil {
			return err
		}
		if err := validateOpaque("delta ID", entry.ID, MaxCoordinateBytes, true); err != nil {
			return err
		}
		if err := entry.LogicalDigest.Validate("delta logical digest"); err != nil {
			return err
		}
		if err := entry.PhysicalDigest.Validate("delta physical digest"); err != nil {
			return err
		}
		if index > 0 && CompareIndexDeltaEntries(d.Entries[index-1], entry) >= 0 {
			return invalid("index delta entries must be strictly ordered")
		}
	}
	if d.DeltaDigest != indexDeltaEntriesDigest(d.Entries) {
		return invalid("index delta digest mismatch")
	}
	return nil
}

func NewIndexDeltaV1(family Family, igen IGEN, epoch Epoch, txn TXN, manifest Digest,
	entries []IndexDeltaEntry, state LifecycleState) (IndexDeltaV1, error) {
	entries = append([]IndexDeltaEntry(nil), entries...)
	SortIndexDeltaEntries(entries)
	value := IndexDeltaV1{
		Family: family, IGEN: igen, Epoch: epoch, TXN: txn,
		ManifestDigest: manifest, Entries: entries, State: state,
	}
	value.DeltaDigest = indexDeltaEntriesDigest(entries)
	return value, value.Validate()
}

func encodeIndexDelta(e *encoder, value IndexDeltaV1) {
	e.bytes("family", value.Family)
	e.bytes("IGEN", value.IGEN)
	e.u64(uint64(value.Epoch))
	e.bytes("transaction ID", value.TXN)
	e.digest(value.ManifestDigest)
	e.u32(uint32(len(value.Entries)))
	e.digest(value.DeltaDigest)
	e.byte(byte(value.State))
	for _, entry := range value.Entries {
		e.bytes("delta kind", entry.Kind)
		e.bytes("delta ID", entry.ID)
		e.digest(entry.LogicalDigest)
		e.digest(entry.PhysicalDigest)
	}
}

func MarshalIndexDeltaV1(value IndexDeltaV1) ([]byte, error) {
	value.Entries = append([]IndexDeltaEntry(nil), value.Entries...)
	SortIndexDeltaEntries(value.Entries)
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindIndexDelta, VersionIndexDeltaV1, MaxChunkBytes,
		func(e *encoder) { encodeIndexDelta(e, value) })
}

func UnmarshalIndexDeltaV1(data []byte) (IndexDeltaV1, error) {
	payload, err := verifyEnvelope(data, KindIndexDelta, VersionIndexDeltaV1, MaxChunkBytes)
	if err != nil {
		return IndexDeltaV1{}, err
	}
	d := &decoder{data: payload}
	value := IndexDeltaV1{
		Family:         Family(d.bytes("family", MaxOpaqueIDBytes, true)),
		IGEN:           IGEN(d.bytes("IGEN", MaxOpaqueIDBytes, true)),
		Epoch:          Epoch(d.positive("epoch")),
		TXN:            TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		ManifestDigest: d.digest("manifest digest"),
	}
	count := d.u32("entry count")
	value.DeltaDigest = d.digest("delta digest")
	value.State = LifecycleState(d.byte("state"))
	if count == 0 || count > MaxIndexDeltaEntries || uint64(count) > uint64(d.remaining())/72 {
		return IndexDeltaV1{}, invalid("index delta entry count exceeds its bound or remaining payload")
	}
	value.Entries = make([]IndexDeltaEntry, int(count))
	for index := range value.Entries {
		value.Entries[index] = IndexDeltaEntry{
			Kind:           d.bytes("delta kind", MaxObjectKindBytes, true),
			ID:             d.bytes("delta ID", MaxCoordinateBytes, true),
			LogicalDigest:  d.digest("logical digest"),
			PhysicalDigest: d.digest("physical digest"),
		}
	}
	if d.err != nil {
		return IndexDeltaV1{}, d.err
	}
	if err := value.Validate(); err != nil {
		return IndexDeltaV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeIndexDelta(e, value) }, MaxChunkBytes); err != nil {
		return IndexDeltaV1{}, err
	}
	return value, nil
}

type IndexActivationV2 struct {
	Family              Family
	IGEN                IGEN
	ActivationEpoch     Epoch
	SourceFrontier      Epoch
	ManifestDigest      Digest
	DeltaDigest         Digest
	TXN                 TXN
	Fence               Fence
	AuthorityGeneration Generation
	State               LifecycleState
}

func (a IndexActivationV2) Validate() error {
	if err := a.Family.Validate(); err != nil {
		return err
	}
	if err := a.IGEN.Validate(); err != nil {
		return err
	}
	if err := a.ActivationEpoch.Validate(); err != nil {
		return err
	}
	if err := a.SourceFrontier.Validate(); err != nil {
		return err
	}
	if a.ActivationEpoch <= a.SourceFrontier {
		return invalid("index activation epoch must exceed source frontier")
	}
	if err := a.ManifestDigest.Validate("index manifest digest"); err != nil {
		return err
	}
	if err := a.DeltaDigest.Validate("index delta digest"); err != nil {
		return err
	}
	if err := a.TXN.Validate(); err != nil {
		return err
	}
	if err := a.Fence.Validate(); err != nil {
		return err
	}
	if err := a.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if a.State != LifecycleActive && a.State != LifecycleRetired && a.State != LifecyclePoisoned {
		return invalid("index activation state must be ACTIVE, RETIRED, or POISONED")
	}
	return nil
}

func ValidateIndexActivationSuccessor(previous, next IndexActivationV2) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.Family, next.Family) {
		return invalid("index activation family is immutable")
	}
	if next.ActivationEpoch <= previous.ActivationEpoch {
		return invalid("index activation epoch must increase")
	}
	if next.Fence <= previous.Fence {
		return invalid("index activation fence must increase")
	}
	if next.AuthorityGeneration < previous.AuthorityGeneration {
		return invalid("index activation authority generation must not decrease")
	}
	if previous.State == LifecycleActive && bytes.Equal(previous.IGEN, next.IGEN) {
		return invalid("active index generation cannot be reactivated")
	}
	return nil
}

func encodeIndexActivation(e *encoder, a IndexActivationV2) {
	e.bytes("family", a.Family)
	e.bytes("IGEN", a.IGEN)
	e.u64(uint64(a.ActivationEpoch))
	e.u64(uint64(a.SourceFrontier))
	e.digest(a.ManifestDigest)
	e.digest(a.DeltaDigest)
	e.bytes("transaction ID", a.TXN)
	e.u64(uint64(a.Fence))
	e.u64(uint64(a.AuthorityGeneration))
	e.byte(byte(a.State))
}

func MarshalIndexActivationV2(a IndexActivationV2) ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindIndexActivation, VersionIndexActivationV2, MaxRootBytes,
		func(e *encoder) { encodeIndexActivation(e, a) })
}

func UnmarshalIndexActivationV2(data []byte) (IndexActivationV2, error) {
	payload, err := verifyEnvelope(data, KindIndexActivation, VersionIndexActivationV2, MaxRootBytes)
	if err != nil {
		return IndexActivationV2{}, err
	}
	d := &decoder{data: payload}
	a := IndexActivationV2{
		Family:              Family(d.bytes("family", MaxOpaqueIDBytes, true)),
		IGEN:                IGEN(d.bytes("IGEN", MaxOpaqueIDBytes, true)),
		ActivationEpoch:     Epoch(d.positive("activation epoch")),
		SourceFrontier:      Epoch(d.positive("source frontier")),
		ManifestDigest:      d.digest("manifest digest"),
		DeltaDigest:         d.digest("delta digest"),
		TXN:                 TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		Fence:               Fence(d.positive("fence")),
		AuthorityGeneration: Generation(d.positive("authority generation")),
		State:               LifecycleState(d.byte("state")),
	}
	if d.err != nil {
		return IndexActivationV2{}, d.err
	}
	if err := a.Validate(); err != nil {
		return IndexActivationV2{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeIndexActivation(e, a) }, MaxRootBytes); err != nil {
		return IndexActivationV2{}, err
	}
	return a, nil
}
