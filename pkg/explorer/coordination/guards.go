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
	"time"
)

type EntityGuardV1 struct {
	EntityKind          EntityKind
	EntityID            EntityID
	TXN                 TXN
	Owner               OwnerID
	LeaseUntil          time.Time
	Fence               Fence
	AuthorityGeneration Generation
	DesiredDigest       Digest
	PreviousDigest      Digest
	PreviousVersion     Epoch
	State               GuardState
}

func (g EntityGuardV1) Validate() error {
	if err := g.EntityKind.Validate(); err != nil {
		return err
	}
	if err := g.EntityID.Validate(); err != nil {
		return err
	}
	if err := g.TXN.Validate(); err != nil {
		return err
	}
	if err := g.Owner.Validate(); err != nil {
		return err
	}
	if err := g.Fence.Validate(); err != nil {
		return err
	}
	if err := g.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := g.DesiredDigest.Validate("desired logical digest"); err != nil {
		return err
	}
	if err := g.State.Validate(); err != nil {
		return err
	}
	if g.PreviousVersion == 0 {
		if g.PreviousDigest != (Digest{}) {
			return invalid("absent previous version must have no previous digest")
		}
	} else {
		if err := g.PreviousVersion.Validate(); err != nil {
			return err
		}
		if err := g.PreviousDigest.Validate("previous committed digest"); err != nil {
			return err
		}
	}
	if g.State == GuardStateHeld {
		return validateTime("guard lease", g.LeaseUntil, false)
	}
	if !g.LeaseUntil.IsZero() {
		return invalid("terminal or released guard must not have a lease")
	}
	return nil
}

func sameGuardIdentity(left, right EntityGuardV1) bool {
	return bytes.Equal(left.EntityKind, right.EntityKind) &&
		bytes.Equal(left.EntityID, right.EntityID) &&
		bytes.Equal(left.TXN, right.TXN) &&
		left.DesiredDigest == right.DesiredDigest &&
		left.PreviousDigest == right.PreviousDigest &&
		left.PreviousVersion == right.PreviousVersion
}

func ValidateGuardAcquisition(previous *EntityGuardV1, next EntityGuardV1) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.State != GuardStateHeld {
		return invalid("acquired guard must be held")
	}
	if previous == nil {
		return nil
	}
	if err := previous.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(previous.EntityKind, next.EntityKind) ||
		!bytes.Equal(previous.EntityID, next.EntityID) {
		return invalid("guard coordinate is immutable")
	}
	if previous.State == GuardStateHeld && bytes.Equal(previous.TXN, next.TXN) {
		return ValidateGuardTransition(*previous, next)
	}
	if next.Fence <= previous.Fence {
		return invalid("guard acquisition requires a greater fence")
	}
	if next.AuthorityGeneration < previous.AuthorityGeneration {
		return invalid("guard acquisition authority generation must not decrease")
	}
	return nil
}

func ValidateGuardTransition(previous, next EntityGuardV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if !sameGuardIdentity(previous, next) {
		return invalid("guard identity fields are immutable")
	}
	if previous.State != GuardStateHeld {
		return invalid("only a held guard may transition")
	}
	if next.Fence < previous.Fence {
		return invalid("guard fence must not decrease")
	}
	if next.AuthorityGeneration < previous.AuthorityGeneration {
		return invalid("guard authority generation must not decrease")
	}
	if next.State == GuardStateHeld {
		if !bytes.Equal(previous.Owner, next.Owner) {
			if next.Fence <= previous.Fence {
				return invalid("guard takeover requires a greater fence")
			}
		} else if next.Fence != previous.Fence {
			return invalid("guard renewal cannot change fence")
		}
		if next.LeaseUntil.Before(previous.LeaseUntil) {
			return invalid("guard renewal cannot shorten its lease")
		}
		return nil
	}
	if !bytes.Equal(previous.Owner, next.Owner) || next.Fence != previous.Fence {
		return invalid("guard release or terminal transition must retain owner and fence")
	}
	switch next.State {
	case GuardStateReleased, GuardStateCommitted, GuardStateAborted,
		GuardStateConflicted, GuardStatePoisoned:
		return nil
	default:
		return invalid("illegal guard transition")
	}
}

func ValidateNewerFence(previous, next Fence) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if next <= previous {
		return invalid("fence must increase")
	}
	return nil
}

func encodeEntityGuard(e *encoder, g EntityGuardV1) {
	e.bytes("entity kind", g.EntityKind)
	e.bytes("entity ID", g.EntityID)
	e.bytes("transaction ID", g.TXN)
	e.bytes("owner", g.Owner)
	e.timestamp(g.LeaseUntil)
	e.u64(uint64(g.Fence))
	e.u64(uint64(g.AuthorityGeneration))
	e.digest(g.DesiredDigest)
	e.digest(g.PreviousDigest)
	e.optionalEpoch(g.PreviousVersion)
	e.byte(byte(g.State))
}

func MarshalEntityGuardV1(g EntityGuardV1) ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindEntityGuard, VersionEntityGuardV1, MaxRootBytes,
		func(e *encoder) { encodeEntityGuard(e, g) })
}

func UnmarshalEntityGuardV1(data []byte) (EntityGuardV1, error) {
	payload, err := verifyEnvelope(data, KindEntityGuard, VersionEntityGuardV1, MaxRootBytes)
	if err != nil {
		return EntityGuardV1{}, err
	}
	d := &decoder{data: payload}
	g := EntityGuardV1{
		EntityKind:          EntityKind(d.bytes("entity kind", MaxObjectKindBytes, true)),
		EntityID:            EntityID(d.bytes("entity ID", MaxOpaqueIDBytes, true)),
		TXN:                 TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		Owner:               OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		LeaseUntil:          d.timestamp("guard lease"),
		Fence:               Fence(d.positive("fence")),
		AuthorityGeneration: Generation(d.positive("authority generation")),
		DesiredDigest:       d.digest("desired logical digest"),
		PreviousDigest:      d.digest("previous committed digest"),
		PreviousVersion:     d.optionalEpoch("previous version"),
		State:               GuardState(d.byte("state")),
	}
	if d.err != nil {
		return EntityGuardV1{}, d.err
	}
	if err := g.Validate(); err != nil {
		return EntityGuardV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeEntityGuard(e, g) }, MaxRootBytes); err != nil {
		return EntityGuardV1{}, err
	}
	return g, nil
}

type PendingMutationV1 struct {
	EntityKind     EntityKind
	EntityID       EntityID
	TXN            TXN
	ManifestChunk  uint32
	ManifestEntry  uint32
	Ordinal        uint32
	LogicalDigest  Digest
	PhysicalDigest Digest
}

func (p PendingMutationV1) Validate() error {
	if err := p.EntityKind.Validate(); err != nil {
		return err
	}
	if err := p.EntityID.Validate(); err != nil {
		return err
	}
	if err := p.TXN.Validate(); err != nil {
		return err
	}
	if p.ManifestEntry >= MaxChunkEntries {
		return invalid("pending mutation manifest entry exceeds its bound")
	}
	if err := p.LogicalDigest.Validate("pending logical digest"); err != nil {
		return err
	}
	return p.PhysicalDigest.Validate("pending physical digest")
}

func (p PendingMutationV1) SameIdentity(other PendingMutationV1) bool {
	return bytes.Equal(p.EntityKind, other.EntityKind) &&
		bytes.Equal(p.EntityID, other.EntityID) &&
		bytes.Equal(p.TXN, other.TXN) &&
		p.ManifestChunk == other.ManifestChunk &&
		p.ManifestEntry == other.ManifestEntry &&
		p.Ordinal == other.Ordinal
}

func encodePendingMutation(e *encoder, p PendingMutationV1) {
	e.bytes("entity kind", p.EntityKind)
	e.bytes("entity ID", p.EntityID)
	e.bytes("transaction ID", p.TXN)
	e.u32(p.ManifestChunk)
	e.u32(p.ManifestEntry)
	e.u32(p.Ordinal)
	e.digest(p.LogicalDigest)
	e.digest(p.PhysicalDigest)
}

func MarshalPendingMutationV1(p PendingMutationV1) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindPendingMutation, VersionPendingMutationV1, MaxRootBytes,
		func(e *encoder) { encodePendingMutation(e, p) })
}

func UnmarshalPendingMutationV1(data []byte) (PendingMutationV1, error) {
	payload, err := verifyEnvelope(data, KindPendingMutation, VersionPendingMutationV1, MaxRootBytes)
	if err != nil {
		return PendingMutationV1{}, err
	}
	d := &decoder{data: payload}
	p := PendingMutationV1{
		EntityKind:     EntityKind(d.bytes("entity kind", MaxObjectKindBytes, true)),
		EntityID:       EntityID(d.bytes("entity ID", MaxOpaqueIDBytes, true)),
		TXN:            TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		ManifestChunk:  d.u32("manifest chunk"),
		ManifestEntry:  d.u32("manifest entry"),
		Ordinal:        d.u32("ordinal"),
		LogicalDigest:  d.digest("logical digest"),
		PhysicalDigest: d.digest("physical digest"),
	}
	if d.err != nil {
		return PendingMutationV1{}, d.err
	}
	if err := p.Validate(); err != nil {
		return PendingMutationV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodePendingMutation(e, p) }, MaxRootBytes); err != nil {
		return PendingMutationV1{}, err
	}
	return p, nil
}
