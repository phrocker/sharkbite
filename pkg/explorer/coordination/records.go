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
	"math"
	"sort"
	"time"
)

type ResultIdentity struct {
	Kind []byte
	ID   []byte
}

type TxnRootV3 struct {
	State                     TxnState
	LogicalDigest             Digest
	TokenHash                 Digest
	Epoch                     Epoch
	Owner                     OwnerID
	Fence                     Fence
	ManifestRoot              Digest
	ChunkCount                uint32
	TotalEntries              uint64
	TotalEncodedBytes         uint64
	LPARTs                    []LPART
	ResultIdentities          []ResultIdentity
	StateGeneration           Generation
	WriterAuthorityGeneration Generation
	RetentionGeneration       Generation
}

func (r TxnRootV3) Validate() error {
	if err := r.State.ValidatePersisted(); err != nil {
		return err
	}
	if err := r.LogicalDigest.Validate("logical digest"); err != nil {
		return err
	}
	if err := r.TokenHash.Validate("token hash"); err != nil {
		return err
	}
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if err := r.Fence.Validate(); err != nil {
		return err
	}
	if err := r.StateGeneration.Validate(); err != nil {
		return err
	}
	if err := r.WriterAuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := r.RetentionGeneration.Validate(); err != nil {
		return err
	}
	if r.State >= StateEpochReserved && r.State <= StateCommitted {
		if err := r.Epoch.Validate(); err != nil {
			return err
		}
	} else if r.State.Nonterminal() && r.Epoch != 0 {
		return invalid("epoch must be absent before EPOCH_RESERVED")
	} else if r.Epoch != 0 {
		if err := r.Epoch.Validate(); err != nil {
			return err
		}
	}
	if r.ChunkCount == 0 {
		if r.TotalEntries != 0 || r.TotalEncodedBytes != 0 || r.ManifestRoot != (Digest{}) {
			return invalid("empty manifest must have zero totals and digest")
		}
	} else {
		if r.TotalEntries == 0 || r.TotalEntries > MaxManifestEntries {
			return invalid("manifest total entries is outside its bound")
		}
		if r.TotalEncodedBytes == 0 || r.TotalEncodedBytes > math.MaxInt64 {
			return invalid("manifest total bytes is outside its bound")
		}
		if err := r.ManifestRoot.Validate("manifest root"); err != nil {
			return err
		}
	}
	if len(r.LPARTs) > MaxLPARTs {
		return invalid("transaction has too many LPARTs")
	}
	for i, value := range r.LPARTs {
		if err := value.Validate(); err != nil {
			return err
		}
		if i > 0 && bytes.Compare(r.LPARTs[i-1], value) >= 0 {
			return invalid("LPARTs must be strictly byte-sorted")
		}
	}
	if len(r.ResultIdentities) > MaxResultIdentities {
		return invalid("transaction has too many result identities")
	}
	for i, value := range r.ResultIdentities {
		if err := validateOpaque("result kind", value.Kind, MaxResultIdentityBytes, true); err != nil {
			return err
		}
		if err := validateOpaque("result ID", value.ID, MaxResultIdentityBytes, true); err != nil {
			return err
		}
		if i > 0 {
			previous := r.ResultIdentities[i-1]
			order := bytes.Compare(previous.Kind, value.Kind)
			if order > 0 || order == 0 && bytes.Compare(previous.ID, value.ID) >= 0 {
				return invalid("result identities must be strictly byte-sorted")
			}
		}
	}
	return nil
}

func encodeTxnRoot(e *encoder, r TxnRootV3) {
	e.byte(byte(r.State))
	e.digest(r.LogicalDigest)
	e.digest(r.TokenHash)
	e.optionalEpoch(r.Epoch)
	e.bytes("owner", r.Owner)
	e.u64(uint64(r.Fence))
	e.digest(r.ManifestRoot)
	e.u32(r.ChunkCount)
	e.u64(r.TotalEntries)
	e.u64(r.TotalEncodedBytes)
	e.u32(uint32(len(r.LPARTs)))
	for _, value := range r.LPARTs {
		e.bytes("LPART", value)
	}
	e.u32(uint32(len(r.ResultIdentities)))
	for _, value := range r.ResultIdentities {
		e.bytes("result kind", value.Kind)
		e.bytes("result ID", value.ID)
	}
	e.u64(uint64(r.StateGeneration))
	e.u64(uint64(r.WriterAuthorityGeneration))
	e.u64(uint64(r.RetentionGeneration))
}

func MarshalTxnRootV3(r TxnRootV3) ([]byte, error) {
	r.LPARTs = append([]LPART(nil), r.LPARTs...)
	r.ResultIdentities = append([]ResultIdentity(nil), r.ResultIdentities...)
	SortLPARTs(r.LPARTs)
	SortResultIdentities(r.ResultIdentities)
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindTxnRoot, VersionTxnRootV3, MaxRootBytes, func(e *encoder) {
		encodeTxnRoot(e, r)
	})
}

func UnmarshalTxnRootV3(data []byte) (TxnRootV3, error) {
	payload, err := verifyEnvelope(data, KindTxnRoot, VersionTxnRootV3, MaxRootBytes)
	if err != nil {
		return TxnRootV3{}, err
	}
	d := &decoder{data: payload}
	r := TxnRootV3{
		State:             TxnState(d.byte("state")),
		LogicalDigest:     d.digest("logical digest"),
		TokenHash:         d.digest("token hash"),
		Epoch:             d.optionalEpoch("epoch"),
		Owner:             OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		Fence:             Fence(d.positive("fence")),
		ManifestRoot:      d.digest("manifest root"),
		ChunkCount:        d.u32("chunk count"),
		TotalEntries:      d.u64("total entries"),
		TotalEncodedBytes: d.u64("total encoded bytes"),
	}
	lpartCount := d.u32("LPART count")
	if lpartCount > MaxLPARTs || uint64(lpartCount) > uint64(d.remaining())/4 {
		return TxnRootV3{}, invalid("LPART count exceeds its bound or remaining payload")
	}
	r.LPARTs = make([]LPART, int(lpartCount))
	for i := range r.LPARTs {
		r.LPARTs[i] = LPART(d.bytes("LPART", MaxOpaqueIDBytes, true))
	}
	resultCount := d.u32("result identity count")
	if resultCount > MaxResultIdentities || uint64(resultCount) > uint64(d.remaining())/8 {
		return TxnRootV3{}, invalid("result identity count exceeds its bound or remaining payload")
	}
	r.ResultIdentities = make([]ResultIdentity, int(resultCount))
	for i := range r.ResultIdentities {
		r.ResultIdentities[i] = ResultIdentity{
			Kind: d.bytes("result kind", MaxResultIdentityBytes, true),
			ID:   d.bytes("result ID", MaxResultIdentityBytes, true),
		}
	}
	r.StateGeneration = Generation(d.positive("state generation"))
	r.WriterAuthorityGeneration = Generation(d.positive("writer authority generation"))
	r.RetentionGeneration = Generation(d.positive("retention generation"))
	if d.err != nil {
		return TxnRootV3{}, d.err
	}
	if err := r.Validate(); err != nil {
		return TxnRootV3{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeTxnRoot(e, r) }, MaxRootBytes); err != nil {
		return TxnRootV3{}, err
	}
	return r, nil
}

type ReservationV1 struct {
	ReservationGeneration Generation
	Epoch                 Epoch
	TXN                   TXN
	Owner                 OwnerID
	LeaseUntil            time.Time
	Fence                 Fence
	AuthorityGeneration   Generation
	State                 TxnState
}

func (r ReservationV1) Validate() error {
	if err := r.ReservationGeneration.Validate(); err != nil {
		return err
	}
	if err := r.Epoch.Validate(); err != nil {
		return err
	}
	if err := r.TXN.Validate(); err != nil {
		return err
	}
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if err := validateTime("reservation lease", r.LeaseUntil, false); err != nil {
		return err
	}
	if err := r.Fence.Validate(); err != nil {
		return err
	}
	if err := r.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if !(r.State >= StateEpochReserved && r.State <= StatePrepared || r.State.Terminal()) {
		return invalid("reservation state must be active after epoch reservation or terminal")
	}
	return nil
}

func ValidateReservationSuccessor(previous, next ReservationV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if previous.ReservationGeneration == Generation(math.MaxInt64) {
		return invalid("reservation generation is exhausted")
	}
	if next.ReservationGeneration != previous.ReservationGeneration+1 {
		return invalid("reservation generation must increase exactly once")
	}
	if previous.Epoch != next.Epoch || !bytes.Equal(previous.TXN, next.TXN) ||
		!bytes.Equal(previous.Owner, next.Owner) || previous.Fence != next.Fence ||
		previous.AuthorityGeneration != next.AuthorityGeneration {
		return invalid("reservation identity is immutable")
	}
	if previous.State.Terminal() {
		return invalid("terminal reservation cannot transition")
	}
	if !next.State.Terminal() {
		if err := ValidateTransition(previous.State, next.State); err != nil {
			return err
		}
	}
	return nil
}

func ValidateReservationTakeover(previous, next ReservationV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if previous.State.Terminal() {
		return invalid("terminal reservation cannot be taken over")
	}
	if previous.ReservationGeneration == Generation(math.MaxInt64) ||
		next.ReservationGeneration != previous.ReservationGeneration+1 {
		return invalid("reservation generation must increase exactly once")
	}
	if previous.Epoch != next.Epoch || !bytes.Equal(previous.TXN, next.TXN) ||
		previous.AuthorityGeneration != next.AuthorityGeneration || previous.State != next.State {
		return invalid("reservation takeover changed immutable identity")
	}
	if next.Fence <= previous.Fence || !next.LeaseUntil.After(previous.LeaseUntil) {
		return invalid("reservation takeover must advance fence and lease")
	}
	return nil
}

func encodeReservation(e *encoder, r ReservationV1) {
	e.u64(uint64(r.ReservationGeneration))
	e.u64(uint64(r.Epoch))
	e.bytes("transaction ID", r.TXN)
	e.bytes("owner", r.Owner)
	e.timestamp(r.LeaseUntil)
	e.u64(uint64(r.Fence))
	e.u64(uint64(r.AuthorityGeneration))
	e.byte(byte(r.State))
}

func MarshalReservationV1(r ReservationV1) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindReservation, VersionReservationV1, MaxRootBytes, func(e *encoder) {
		encodeReservation(e, r)
	})
}

func UnmarshalReservationV1(data []byte) (ReservationV1, error) {
	payload, err := verifyEnvelope(data, KindReservation, VersionReservationV1, MaxRootBytes)
	if err != nil {
		return ReservationV1{}, err
	}
	d := &decoder{data: payload}
	r := ReservationV1{
		ReservationGeneration: Generation(d.positive("reservation generation")),
		Epoch:                 Epoch(d.positive("epoch")),
		TXN:                   TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		Owner:                 OwnerID(d.bytes("owner", MaxOwnerBytes, true)),
		LeaseUntil:            d.timestamp("reservation lease"),
		Fence:                 Fence(d.positive("fence")),
		AuthorityGeneration:   Generation(d.positive("authority generation")),
		State:                 TxnState(d.byte("state")),
	}
	if d.err != nil {
		return ReservationV1{}, d.err
	}
	if err := r.Validate(); err != nil {
		return ReservationV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeReservation(e, r) }, MaxRootBytes); err != nil {
		return ReservationV1{}, err
	}
	return r, nil
}

type EpochOutcomeV1 struct {
	Epoch               Epoch
	TXN                 TXN
	State               TxnState
	OwnerFence          Fence
	AuthorityGeneration Generation
	Digest              Digest
}

func (o EpochOutcomeV1) computedDigest() Digest {
	e := newPayloadEncoder(MaxRootBytes)
	e.u64(uint64(o.Epoch))
	e.bytes("transaction ID", o.TXN)
	e.byte(byte(o.State))
	e.u64(uint64(o.OwnerFence))
	e.u64(uint64(o.AuthorityGeneration))
	return sha256.Sum256(e.data)
}

func (o EpochOutcomeV1) ComputeDigest() Digest { return o.computedDigest() }

func NewEpochOutcomeV1(epoch Epoch, txn TXN, state TxnState, fence Fence, authority Generation) (EpochOutcomeV1, error) {
	value := EpochOutcomeV1{Epoch: epoch, TXN: txn, State: state, OwnerFence: fence, AuthorityGeneration: authority}
	value.Digest = value.computedDigest()
	return value, value.Validate()
}

func (o EpochOutcomeV1) Validate() error {
	if err := o.Epoch.Validate(); err != nil {
		return err
	}
	if err := o.TXN.Validate(); err != nil {
		return err
	}
	if !o.State.Terminal() {
		return invalid("epoch outcome state must be terminal")
	}
	if err := o.OwnerFence.Validate(); err != nil {
		return err
	}
	if err := o.AuthorityGeneration.Validate(); err != nil {
		return err
	}
	if o.Digest != o.computedDigest() {
		return invalid("epoch outcome digest mismatch")
	}
	return nil
}

func encodeOutcome(e *encoder, o EpochOutcomeV1) {
	e.u64(uint64(o.Epoch))
	e.bytes("transaction ID", o.TXN)
	e.byte(byte(o.State))
	e.u64(uint64(o.OwnerFence))
	e.u64(uint64(o.AuthorityGeneration))
	e.digest(o.Digest)
}

func MarshalEpochOutcomeV1(o EpochOutcomeV1) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindEpochOutcome, VersionEpochOutcomeV1, MaxRootBytes, func(e *encoder) {
		encodeOutcome(e, o)
	})
}

func UnmarshalEpochOutcomeV1(data []byte) (EpochOutcomeV1, error) {
	payload, err := verifyEnvelope(data, KindEpochOutcome, VersionEpochOutcomeV1, MaxRootBytes)
	if err != nil {
		return EpochOutcomeV1{}, err
	}
	d := &decoder{data: payload}
	o := EpochOutcomeV1{
		Epoch:               Epoch(d.positive("epoch")),
		TXN:                 TXN(d.bytes("transaction ID", MaxOpaqueIDBytes, true)),
		State:               TxnState(d.byte("state")),
		OwnerFence:          Fence(d.positive("owner fence")),
		AuthorityGeneration: Generation(d.positive("authority generation")),
		Digest:              d.digest("digest"),
	}
	if d.err != nil {
		return EpochOutcomeV1{}, d.err
	}
	if err := o.Validate(); err != nil {
		return EpochOutcomeV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeOutcome(e, o) }, MaxRootBytes); err != nil {
		return EpochOutcomeV1{}, err
	}
	return o, nil
}

type FrontierCheckpointV1 struct {
	Frontier          Epoch
	VisibleAt         time.Time
	PredecessorDigest Digest
	OutcomesDigest    Digest
	Digest            Digest
}

func (c FrontierCheckpointV1) computedDigest() Digest {
	e := newPayloadEncoder(MaxRootBytes)
	e.u64(uint64(c.Frontier))
	e.timestamp(c.VisibleAt)
	e.digest(c.PredecessorDigest)
	e.digest(c.OutcomesDigest)
	return sha256.Sum256(e.data)
}

func (c FrontierCheckpointV1) ComputeDigest() Digest { return c.computedDigest() }

func NewFrontierCheckpointV1(frontier Epoch, visibleAt time.Time, predecessor, outcomes Digest) (FrontierCheckpointV1, error) {
	value := FrontierCheckpointV1{Frontier: frontier, VisibleAt: visibleAt.UTC(), PredecessorDigest: predecessor, OutcomesDigest: outcomes}
	value.Digest = value.computedDigest()
	return value, value.Validate()
}

func (c FrontierCheckpointV1) Validate() error {
	if err := c.Frontier.Validate(); err != nil {
		return err
	}
	if err := validateTime("checkpoint visible_at", c.VisibleAt, false); err != nil {
		return err
	}
	if c.Digest != c.computedDigest() {
		return invalid("frontier checkpoint digest mismatch")
	}
	if err := c.OutcomesDigest.Validate("checkpoint outcomes digest"); err != nil {
		return err
	}
	return nil
}

func ValidateCheckpointSuccessor(previous, next FrontierCheckpointV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if next.Frontier <= previous.Frontier {
		return invalid("checkpoint frontier must increase")
	}
	if next.VisibleAt.Before(previous.VisibleAt) {
		return invalid("checkpoint visible_at must not decrease")
	}
	if next.PredecessorDigest != previous.Digest {
		return invalid("checkpoint predecessor digest mismatch")
	}
	return nil
}

func encodeCheckpoint(e *encoder, c FrontierCheckpointV1) {
	e.u64(uint64(c.Frontier))
	e.timestamp(c.VisibleAt)
	e.digest(c.PredecessorDigest)
	e.digest(c.OutcomesDigest)
	e.digest(c.Digest)
}

func MarshalFrontierCheckpointV1(c FrontierCheckpointV1) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindFrontierCheckpoint, VersionFrontierCheckpointV1, MaxRootBytes, func(e *encoder) {
		encodeCheckpoint(e, c)
	})
}

func UnmarshalFrontierCheckpointV1(data []byte) (FrontierCheckpointV1, error) {
	payload, err := verifyEnvelope(data, KindFrontierCheckpoint, VersionFrontierCheckpointV1, MaxRootBytes)
	if err != nil {
		return FrontierCheckpointV1{}, err
	}
	d := &decoder{data: payload}
	c := FrontierCheckpointV1{
		Frontier:          Epoch(d.positive("frontier")),
		VisibleAt:         d.timestamp("checkpoint visible_at"),
		PredecessorDigest: d.digest("predecessor digest"),
		OutcomesDigest:    d.digest("outcomes digest"),
		Digest:            d.digest("digest"),
	}
	if d.err != nil {
		return FrontierCheckpointV1{}, d.err
	}
	if err := c.Validate(); err != nil {
		return FrontierCheckpointV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeCheckpoint(e, c) }, MaxRootBytes); err != nil {
		return FrontierCheckpointV1{}, err
	}
	return c, nil
}

func SortLPARTs(values []LPART) {
	sort.Slice(values, func(i, j int) bool { return bytes.Compare(values[i], values[j]) < 0 })
}

func SortResultIdentities(values []ResultIdentity) {
	sort.Slice(values, func(i, j int) bool {
		if order := bytes.Compare(values[i].Kind, values[j].Kind); order != 0 {
			return order < 0
		}
		return bytes.Compare(values[i].ID, values[j].ID) < 0
	})
}
