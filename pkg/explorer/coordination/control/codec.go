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

package control

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
)

const (
	kindLease       = 1
	kindRetirement  = 2
	kindAuthority   = 3
	kindObservation = 4
)

type writer struct{ b []byte }

func (w *writer) u64(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	w.b = append(w.b, b[:]...)
}
func (w *writer) bytes(v []byte) { w.u64(uint64(len(v))); w.b = append(w.b, v...) }
func (w *writer) tm(v time.Time) {
	w.u64(uint64(v.Unix()) ^ (uint64(1) << 63))
	w.u64(uint64(v.Nanosecond()))
}

func envelope(kind byte, body []byte) []byte {
	result := []byte{'E', 'C', 'T', controlSchema, kind}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	result = append(result, size[:]...)
	result = append(result, body...)
	sum := sha256.Sum256(result)
	return append(result, sum[:]...)
}

func boundedEnvelope(kind byte, body []byte) ([]byte, error) {
	if len(body) > coordination.MaxRootBytes {
		return nil, ErrBounds
	}
	return envelope(kind, body), nil
}

func openEnvelope(value []byte, kind byte) ([]byte, error) {
	if len(value) < 9+sha256.Size || value[0] != 'E' || value[1] != 'C' || value[2] != 'T' ||
		value[3] != controlSchema || value[4] != kind {
		return nil, ErrCorruption
	}
	size := int(binary.BigEndian.Uint32(value[5:9]))
	if size < 0 || size > coordination.MaxRootBytes || len(value) != 9+size+sha256.Size {
		return nil, ErrCorruption
	}
	sum := sha256.Sum256(value[:9+size])
	if string(sum[:]) != string(value[9+size:]) {
		return nil, ErrCorruption
	}
	return value[9 : 9+size], nil
}

type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) u64() uint64 {
	if r.err != nil || len(r.b)-r.off < 8 {
		r.err = ErrCorruption
		return 0
	}
	v := binary.BigEndian.Uint64(r.b[r.off : r.off+8])
	r.off += 8
	return v
}
func (r *reader) bytes(max int) []byte {
	n := r.u64()
	if r.err != nil || n > uint64(max) || n > uint64(len(r.b)-r.off) {
		r.err = ErrCorruption
		return nil
	}
	v := append([]byte(nil), r.b[r.off:r.off+int(n)]...)
	r.off += int(n)
	return v
}
func (r *reader) tm() time.Time {
	seconds := int64(r.u64() ^ (uint64(1) << 63))
	nanos := r.u64()
	if r.err != nil || nanos >= uint64(time.Second) {
		r.err = ErrCorruption
		return time.Time{}
	}
	return time.Unix(seconds, int64(nanos)).UTC()
}
func (r *reader) done() error {
	if r.err != nil || r.off != len(r.b) {
		return ErrCorruption
	}
	return nil
}

func marshalLease(value Lease) ([]byte, error) {
	inner, err := coordination.MarshalSnapshotLeaseV3(value.Record)
	if err != nil {
		return nil, err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return nil, err
	}
	if value.UpdatedAt.IsZero() || value.UpdatedAt.Location() != time.UTC {
		return nil, ErrBounds
	}
	var w writer
	w.u64(uint64(value.RecordGeneration))
	w.tm(value.UpdatedAt)
	w.bytes(inner)
	return boundedEnvelope(kindLease, w.b)
}

func unmarshalLease(value []byte) (Lease, error) {
	body, err := openEnvelope(value, kindLease)
	if err != nil {
		return Lease{}, err
	}
	r := reader{b: body}
	result := Lease{RecordGeneration: coordination.Generation(r.u64()), UpdatedAt: r.tm()}
	inner := r.bytes(coordination.MaxRootBytes)
	if err := r.done(); err != nil {
		return Lease{}, err
	}
	result.Record, err = coordination.UnmarshalSnapshotLeaseV3(inner)
	if err != nil || result.RecordGeneration <= 0 || result.UpdatedAt.IsZero() {
		return Lease{}, ErrCorruption
	}
	return result, nil
}

func marshalRetirement(value Retirement) ([]byte, error) {
	inner, err := coordination.MarshalRetirementDecisionV1(value.Decision)
	if err != nil {
		return nil, err
	}
	if err := value.Owner.Validate(); err != nil {
		return nil, err
	}
	if err := value.Fence.Validate(); err != nil {
		return nil, err
	}
	if err := value.RetentionGeneration.Validate(); err != nil {
		return nil, err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return nil, err
	}
	if value.UpdatedAt.IsZero() || value.UpdatedAt.Location() != time.UTC {
		return nil, ErrBounds
	}
	var w writer
	w.bytes(value.Owner)
	w.u64(uint64(value.Fence))
	w.u64(uint64(value.RetentionGeneration))
	w.u64(uint64(value.RecordGeneration))
	w.tm(value.UpdatedAt)
	w.bytes(inner)
	return boundedEnvelope(kindRetirement, w.b)
}

func unmarshalRetirement(value []byte) (Retirement, error) {
	body, err := openEnvelope(value, kindRetirement)
	if err != nil {
		return Retirement{}, err
	}
	r := reader{b: body}
	result := Retirement{
		Owner: coordination.OwnerID(r.bytes(coordination.MaxOwnerBytes)), Fence: coordination.Fence(r.u64()),
		RetentionGeneration: coordination.Generation(r.u64()), RecordGeneration: coordination.Generation(r.u64()),
		UpdatedAt: r.tm(),
	}
	inner := r.bytes(coordination.MaxRootBytes)
	if err := r.done(); err != nil {
		return Retirement{}, err
	}
	result.Decision, err = coordination.UnmarshalRetirementDecisionV1(inner)
	if err != nil || result.Owner.Validate() != nil || result.Fence <= 0 ||
		result.RetentionGeneration <= 0 || result.RecordGeneration <= 0 || result.UpdatedAt.IsZero() {
		return Retirement{}, ErrCorruption
	}
	return result, nil
}

func marshalAuthority(value Authority) ([]byte, error) {
	inner, err := coordination.MarshalWriterAuthorityV1(value.Record)
	if err != nil {
		return nil, err
	}
	if err := value.Mode.Validate(); err != nil {
		return nil, err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return nil, err
	}
	if value.UpdatedAt.IsZero() || value.UpdatedAt.Location() != time.UTC {
		return nil, ErrCorruption
	}
	var w writer
	w.u64(uint64(value.Mode))
	w.u64(uint64(value.RecordGeneration))
	w.tm(value.UpdatedAt)
	w.bytes(inner)
	return boundedEnvelope(kindAuthority, w.b)
}

func unmarshalAuthority(value []byte) (Authority, error) {
	body, err := openEnvelope(value, kindAuthority)
	if err != nil {
		return Authority{}, err
	}
	r := reader{b: body}
	result := Authority{Mode: coordination.WriterMode(r.u64()), RecordGeneration: coordination.Generation(r.u64()), UpdatedAt: r.tm()}
	inner := r.bytes(coordination.MaxRootBytes)
	if err := r.done(); err != nil {
		return Authority{}, err
	}
	result.Record, err = coordination.UnmarshalWriterAuthorityV1(inner)
	if err != nil || result.Mode.Validate() != nil || result.RecordGeneration.Validate() != nil || result.UpdatedAt.IsZero() {
		return Authority{}, ErrCorruption
	}
	return result, nil
}

func marshalObservation(value Observation) ([]byte, error) {
	inner, err := coordination.MarshalBackendObservationV1(value.Record)
	if err != nil {
		return nil, err
	}
	if err := value.Mode.Validate(); err != nil {
		return nil, err
	}
	if err := value.RecordGeneration.Validate(); err != nil {
		return nil, err
	}
	var w writer
	w.u64(uint64(value.Mode))
	w.u64(uint64(value.RecordGeneration))
	w.bytes(inner)
	return boundedEnvelope(kindObservation, w.b)
}

func unmarshalObservation(value []byte) (Observation, error) {
	body, err := openEnvelope(value, kindObservation)
	if err != nil {
		return Observation{}, err
	}
	r := reader{b: body}
	result := Observation{Mode: coordination.WriterMode(r.u64()), RecordGeneration: coordination.Generation(r.u64())}
	inner := r.bytes(coordination.MaxRootBytes)
	if err := r.done(); err != nil {
		return Observation{}, err
	}
	result.Record, err = coordination.UnmarshalBackendObservationV1(inner)
	if err != nil || result.Mode.Validate() != nil || result.RecordGeneration <= 0 {
		return Observation{}, ErrCorruption
	}
	return result, nil
}

func corruption(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCorruption) {
		return err
	}
	return errors.Join(ErrCorruption, err)
}
