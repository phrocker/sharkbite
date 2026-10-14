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
	"math"
	"time"
)

// AllocatorHeadV1 is the single authoritative mutable allocator-row head.
// Reservation and checkpoint cells are separate coordinates on the same row.
type AllocatorHeadV1 struct {
	HeadGeneration            Generation
	NextEpoch                 Epoch
	RetiredThrough            Epoch
	Frontier                  Epoch
	VisibleAt                 time.Time
	CheckpointDigest          Digest
	HistoryFloor              Epoch
	RetentionGeneration       Generation
	WriterAuthorityGeneration Generation
	WriterMode                WriterMode
	WriterHolder              OwnerID
	WriterFence               Fence
	ActiveWindowStart         Epoch
	ActiveReservations        uint32
	MaxActiveReservations     uint32
	ImportPlanDigest          Digest
	ImportMaxEpoch            Epoch
}

func (h AllocatorHeadV1) Validate() error {
	if err := h.HeadGeneration.Validate(); err != nil {
		return err
	}
	if err := h.NextEpoch.Validate(); err != nil {
		return err
	}
	if h.RetiredThrough < 0 || h.RetiredThrough >= h.NextEpoch {
		return invalid("retired-through epoch is outside the allocated range")
	}
	if h.Frontier < h.RetiredThrough || h.Frontier >= h.NextEpoch {
		return invalid("frontier is outside the allocated range")
	}
	if h.Frontier == 0 {
		if !h.VisibleAt.IsZero() || h.CheckpointDigest != (Digest{}) {
			return invalid("empty frontier must have no visible time or checkpoint digest")
		}
	} else {
		if err := validateTime("allocator visible_at", h.VisibleAt, false); err != nil {
			return err
		}
		if err := h.CheckpointDigest.Validate("allocator checkpoint digest"); err != nil {
			return err
		}
	}
	if err := h.HistoryFloor.Validate(); err != nil {
		return err
	}
	if h.HistoryFloor > h.Frontier+1 {
		return invalid("history floor is beyond the current frontier")
	}
	if err := h.RetentionGeneration.Validate(); err != nil {
		return err
	}
	if err := h.WriterAuthorityGeneration.Validate(); err != nil {
		return err
	}
	if err := h.WriterMode.Validate(); err != nil {
		return err
	}
	if err := h.WriterHolder.Validate(); err != nil {
		return err
	}
	if err := h.WriterFence.Validate(); err != nil {
		return err
	}
	if h.MaxActiveReservations == 0 || h.MaxActiveReservations > MaxActiveReservations {
		return invalid("maximum active reservations is outside its bound")
	}
	if h.ActiveReservations > h.MaxActiveReservations {
		return invalid("active reservation count exceeds its bound")
	}
	expectedActive := uint64(h.NextEpoch - h.RetiredThrough - 1)
	if uint64(h.ActiveReservations) != expectedActive {
		return invalid("active reservation count does not match the allocated window")
	}
	if h.ActiveReservations == 0 {
		if h.ActiveWindowStart != 0 {
			return invalid("empty active window must have no start")
		}
	} else {
		if err := h.ActiveWindowStart.Validate(); err != nil {
			return err
		}
		if h.ActiveWindowStart != h.RetiredThrough+1 {
			return invalid("active window start does not follow retired-through")
		}
	}
	if h.ImportMaxEpoch == 0 {
		if h.ImportPlanDigest != (Digest{}) {
			return invalid("import plan digest requires an import maximum epoch")
		}
	} else {
		if err := h.ImportMaxEpoch.Validate(); err != nil {
			return err
		}
		if err := h.ImportPlanDigest.Validate("import plan digest"); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAllocatorHeadSuccessor requires exactly one generation increment.
func ValidateAllocatorHeadSuccessor(previous, next AllocatorHeadV1) error {
	if err := previous.Validate(); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if previous.HeadGeneration == Generation(math.MaxInt64) {
		return invalid("allocator head generation is exhausted")
	}
	if next.HeadGeneration != previous.HeadGeneration+1 {
		return invalid("allocator head generation must increase exactly once")
	}
	return nil
}

func encodeAllocatorHead(e *encoder, h AllocatorHeadV1) {
	e.u64(uint64(h.HeadGeneration))
	e.u64(uint64(h.NextEpoch))
	e.u64(uint64(h.RetiredThrough))
	e.u64(uint64(h.Frontier))
	e.timestamp(h.VisibleAt)
	e.digest(h.CheckpointDigest)
	e.u64(uint64(h.HistoryFloor))
	e.u64(uint64(h.RetentionGeneration))
	e.u64(uint64(h.WriterAuthorityGeneration))
	e.byte(byte(h.WriterMode))
	e.bytes("writer holder", h.WriterHolder)
	e.u64(uint64(h.WriterFence))
	e.u64(uint64(h.ActiveWindowStart))
	e.u32(h.ActiveReservations)
	e.u32(h.MaxActiveReservations)
	e.digest(h.ImportPlanDigest)
	e.u64(uint64(h.ImportMaxEpoch))
}

func MarshalAllocatorHeadV1(h AllocatorHeadV1) ([]byte, error) {
	h.WriterHolder = append(OwnerID(nil), h.WriterHolder...)
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return marshalEnvelope(KindAllocatorHead, VersionAllocatorHeadV1, MaxRootBytes,
		func(e *encoder) { encodeAllocatorHead(e, h) })
}

func UnmarshalAllocatorHeadV1(data []byte) (AllocatorHeadV1, error) {
	payload, err := verifyEnvelope(data, KindAllocatorHead, VersionAllocatorHeadV1, MaxRootBytes)
	if err != nil {
		return AllocatorHeadV1{}, err
	}
	d := &decoder{data: payload}
	h := AllocatorHeadV1{
		HeadGeneration:            Generation(d.positive("head generation")),
		NextEpoch:                 Epoch(d.positive("next epoch")),
		RetiredThrough:            Epoch(d.u64("retired through")),
		Frontier:                  Epoch(d.u64("frontier")),
		VisibleAt:                 d.timestamp("visible_at"),
		CheckpointDigest:          d.digest("checkpoint digest"),
		HistoryFloor:              Epoch(d.positive("history floor")),
		RetentionGeneration:       Generation(d.positive("retention generation")),
		WriterAuthorityGeneration: Generation(d.positive("writer authority generation")),
		WriterMode:                WriterMode(d.byte("writer mode")),
		WriterHolder:              OwnerID(d.bytes("writer holder", MaxOwnerBytes, true)),
		WriterFence:               Fence(d.positive("writer fence")),
		ActiveWindowStart:         Epoch(d.u64("active window start")),
		ActiveReservations:        d.u32("active reservations"),
		MaxActiveReservations:     d.u32("maximum active reservations"),
		ImportPlanDigest:          d.digest("import plan digest"),
		ImportMaxEpoch:            Epoch(d.u64("import maximum epoch")),
	}
	if d.err != nil {
		return AllocatorHeadV1{}, d.err
	}
	if err := h.Validate(); err != nil {
		return AllocatorHeadV1{}, err
	}
	if err := finishDecode(d, func(e *encoder) { encodeAllocatorHead(e, h) }, MaxRootBytes); err != nil {
		return AllocatorHeadV1{}, err
	}
	return h, nil
}
