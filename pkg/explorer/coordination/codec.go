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
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

type Kind uint16

const (
	KindTxnRoot Kind = iota + 1
	KindManifestChunk
	KindReservation
	KindEpochOutcome
	KindFrontierCheckpoint
	KindEntityGuard
	KindPendingMutation
	KindPolicyCopyManifest
	KindPolicyCopyMap
	KindPolicyCopyFence
	KindIndexGeneration
	KindIndexDelta
	KindIndexActivation
	KindSnapshotLease
	KindRetirementDecision
	KindHistoryFloor
	KindWriterAuthority
	KindBackendObservation
	KindAllocatorHead
	KindPartitionCommitCopy
	KindTxnLease
	KindIdentityScheme
)

const (
	VersionTxnRootV3             uint16 = 3
	VersionManifestChunkV2       uint16 = 2
	VersionReservationV1         uint16 = 1
	VersionEpochOutcomeV1        uint16 = 1
	VersionFrontierCheckpointV1  uint16 = 1
	VersionEntityGuardV1         uint16 = 1
	VersionPendingMutationV1     uint16 = 1
	VersionPolicyCopyManifestV1  uint16 = 1
	VersionPolicyCopyMapV3       uint16 = 3
	VersionPolicyCopyFenceV1     uint16 = 1
	VersionIndexGenerationV2     uint16 = 2
	VersionIndexDeltaV1          uint16 = 1
	VersionIndexActivationV2     uint16 = 2
	VersionSnapshotLeaseV3       uint16 = 3
	VersionRetirementDecisionV1  uint16 = 1
	VersionHistoryFloorV1        uint16 = 1
	VersionWriterAuthorityV1     uint16 = 1
	VersionBackendObservationV1  uint16 = 1
	VersionAllocatorHeadV1       uint16 = 1
	VersionPartitionCommitCopyV1 uint16 = 1
	VersionTxnLeaseV1            uint16 = 1
	VersionIdentitySchemeV1      uint16 = 1

	envelopeMagic      = "SHOALCO\x00"
	envelopeHeaderSize = len(envelopeMagic) + 2 + 2 + 4
	checksumSize       = sha256.Size
)

type encoder struct {
	data     []byte
	expected []byte
	offset   int
	compare  bool
	mismatch bool
	err      error
	maximum  int
}

func newEncoder(maximum int) *encoder {
	return &encoder{data: make([]byte, envelopeHeaderSize), maximum: maximum}
}

func newPayloadEncoder(maximum int) *encoder {
	return &encoder{maximum: maximum}
}

func newComparingEncoder(expected []byte, maximum int) *encoder {
	return &encoder{expected: expected, compare: true, maximum: maximum}
}

func (e *encoder) write(value []byte) {
	if e.err != nil {
		return
	}
	position := len(e.data)
	if e.compare {
		position = e.offset
	}
	if len(value) > e.maximum-position {
		e.err = invalid("record exceeds its encoded byte bound")
		return
	}
	if !e.compare {
		e.data = append(e.data, value...)
		return
	}
	start := e.offset
	e.offset += len(value)
	if e.mismatch || start > len(e.expected) || len(value) > len(e.expected)-start {
		e.mismatch = true
		return
	}
	if !bytes.Equal(e.expected[start:start+len(value)], value) {
		e.mismatch = true
	}
}

func (e *encoder) byte(value byte) { e.write([]byte{value}) }
func (e *encoder) u16(value uint16) {
	var data [2]byte
	binary.BigEndian.PutUint16(data[:], value)
	e.write(data[:])
}
func (e *encoder) u32(value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	e.write(data[:])
}
func (e *encoder) u64(value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	e.write(data[:])
}
func (e *encoder) bytes(name string, value []byte) {
	if len(value) > math.MaxUint32 {
		e.err = invalid(name + " exceeds the codec byte bound")
		return
	}
	e.u32(uint32(len(value)))
	e.write(value)
}
func (e *encoder) digest(value Digest) { e.write(value[:]) }
func (e *encoder) optionalEpoch(value Epoch) {
	if value == 0 {
		e.byte(0)
		return
	}
	e.byte(1)
	e.u64(uint64(value))
}
func (e *encoder) timestamp(value time.Time) {
	if value.IsZero() {
		e.byte(0)
		return
	}
	value = value.UTC()
	e.byte(1)
	e.u16(uint16(value.Year()))
	e.byte(byte(value.Month()))
	e.byte(byte(value.Day()))
	e.byte(byte(value.Hour()))
	e.byte(byte(value.Minute()))
	e.byte(byte(value.Second()))
	e.u32(uint32(value.Nanosecond()))
}

type decoder struct {
	data   []byte
	offset int
	err    error
}

func (d *decoder) remaining() int { return len(d.data) - d.offset }
func (d *decoder) take(name string, size int) []byte {
	if d.err != nil {
		return nil
	}
	if size < 0 || size > d.remaining() {
		d.err = invalid("record is truncated in " + name)
		return nil
	}
	value := d.data[d.offset : d.offset+size]
	d.offset += size
	return value
}
func (d *decoder) byte(name string) byte {
	value := d.take(name, 1)
	if value == nil {
		return 0
	}
	return value[0]
}
func (d *decoder) u16(name string) uint16 {
	value := d.take(name, 2)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint16(value)
}
func (d *decoder) u32(name string) uint32 {
	value := d.take(name, 4)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint32(value)
}
func (d *decoder) u64(name string) uint64 {
	value := d.take(name, 8)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}
func (d *decoder) positive(name string) int64 {
	value := d.u64(name)
	if value == 0 || value > math.MaxInt64 {
		d.err = invalid(name + " must be between 1 and MaxInt64")
		return 0
	}
	return int64(value)
}
func (d *decoder) bytes(name string, maximum int, required bool) []byte {
	length := uint64(d.u32(name + " length"))
	if d.err != nil {
		return nil
	}
	if length > uint64(maximum) {
		d.err = invalid(name + " exceeds its byte bound")
		return nil
	}
	if required && length == 0 {
		d.err = invalid(name + " is required")
		return nil
	}
	if length == 0 {
		return nil
	}
	if length > uint64(d.remaining()) {
		d.err = invalid("record is truncated in " + name)
		return nil
	}
	value := make([]byte, int(length))
	copy(value, d.take(name, int(length)))
	return value
}
func (d *decoder) digest(name string) Digest {
	var value Digest
	copy(value[:], d.take(name, len(value)))
	return value
}
func (d *decoder) optionalEpoch(name string) Epoch {
	switch d.byte(name + " presence") {
	case 0:
		return 0
	case 1:
		return Epoch(d.positive(name))
	default:
		d.err = invalid(name + " has invalid presence")
		return 0
	}
}
func (d *decoder) timestamp(name string) time.Time {
	switch d.byte(name + " presence") {
	case 0:
		return time.Time{}
	case 1:
		year := int(d.u16(name + " year"))
		month := time.Month(d.byte(name + " month"))
		day := int(d.byte(name + " day"))
		hour := int(d.byte(name + " hour"))
		minute := int(d.byte(name + " minute"))
		second := int(d.byte(name + " second"))
		nanos := int(d.u32(name + " nanosecond"))
		if d.err != nil {
			return time.Time{}
		}
		if year < 1 || year > 9999 || month < 1 || month > 12 ||
			day < 1 || day > 31 || hour > 23 || minute > 59 ||
			second > 59 || nanos >= 1_000_000_000 {
			d.err = invalid(name + " has invalid UTC fields")
			return time.Time{}
		}
		value := time.Date(year, month, day, hour, minute, second, nanos, time.UTC)
		if value.Year() != year || value.Month() != month || value.Day() != day {
			d.err = invalid(name + " has invalid calendar fields")
			return time.Time{}
		}
		return value
	default:
		d.err = invalid(name + " has invalid presence")
		return time.Time{}
	}
}

func marshalEnvelope(kind Kind, version uint16, maximum int, encode func(*encoder)) ([]byte, error) {
	e := newEncoder(maximum - checksumSize)
	encode(e)
	if e.err != nil {
		return nil, e.err
	}
	payloadLength := len(e.data) - envelopeHeaderSize
	copy(e.data, envelopeMagic)
	binary.BigEndian.PutUint16(e.data[len(envelopeMagic):], uint16(kind))
	binary.BigEndian.PutUint16(e.data[len(envelopeMagic)+2:], version)
	binary.BigEndian.PutUint32(e.data[envelopeHeaderSize-4:], uint32(payloadLength))
	sum := sha256.Sum256(e.data)
	e.data = append(e.data, sum[:]...)
	return e.data, nil
}

func verifyEnvelope(data []byte, kind Kind, version uint16, maximum int) ([]byte, error) {
	if len(data) < envelopeHeaderSize+checksumSize {
		return nil, invalid("record is truncated")
	}
	if len(data) > maximum {
		return nil, invalid("record exceeds its encoded byte bound")
	}
	if !bytes.Equal(data[:len(envelopeMagic)], []byte(envelopeMagic)) {
		return nil, invalid("record magic is invalid")
	}
	actualKind := Kind(binary.BigEndian.Uint16(data[len(envelopeMagic):]))
	if actualKind != kind {
		return nil, invalid(fmt.Sprintf("record kind %d does not match expected kind %d", actualKind, kind))
	}
	actualVersion := binary.BigEndian.Uint16(data[len(envelopeMagic)+2:])
	if actualVersion != version {
		return nil, invalid(fmt.Sprintf("record version %d is unsupported", actualVersion))
	}
	payloadLength := uint64(binary.BigEndian.Uint32(data[envelopeHeaderSize-4:]))
	expected := uint64(envelopeHeaderSize) + payloadLength + checksumSize
	if uint64(len(data)) < expected {
		return nil, invalid("record is truncated")
	}
	if uint64(len(data)) > expected {
		return nil, invalid("record has trailing bytes")
	}
	payloadEnd := envelopeHeaderSize + int(payloadLength)
	sum := sha256.Sum256(data[:payloadEnd])
	if subtle.ConstantTimeCompare(sum[:], data[payloadEnd:]) != 1 {
		return nil, invalid("record checksum mismatch")
	}
	return data[envelopeHeaderSize:payloadEnd], nil
}

func finishDecode(d *decoder, encode func(*encoder), maximum int) error {
	if d.err != nil {
		return d.err
	}
	if d.remaining() != 0 {
		return invalid("record payload has trailing bytes")
	}
	e := newComparingEncoder(d.data, maximum)
	encode(e)
	if e.err != nil {
		return e.err
	}
	if e.mismatch || e.offset != len(d.data) {
		return invalid("record is not canonically encoded")
	}
	return nil
}
